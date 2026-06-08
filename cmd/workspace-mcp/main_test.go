package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mnehpets/http/endpoint"
	"github.com/mnehpets/http/jsonrpc"

	"github.com/mnehpets/workspace-mcp/mcp"
)

func TestStdioLoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &mcp.Config{Workspaces: []mcp.WorkspaceConfig{{
		Name: "default", Root: dir,
		Policy: mcp.PolicyConfig{AllowGlobs: []string{"**/*.md"}},
		Read:   mcp.ReadConfig{MaxBytes: 1000},
	}}}
	reg, err := mcp.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	server := mcp.NewServer(reg, mcp.NewLogger("error", &bytes.Buffer{}))
	rpc := jsonrpc.NewEndpoint()
	server.Register(rpc)
	handler := endpoint.Handler(rpc.Endpoint)

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`, // notification: no output
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"file_read","arguments":{"path":"README.md"}}}`,
		"",
	}, "\n")

	var out bytes.Buffer
	if err := serveStdioRW(handler, strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// Exactly two responses: the notification must produce no line.
	if len(lines) != 2 {
		t.Fatalf("expected 2 response lines (notification silent), got %d:\n%s", len(lines), out.String())
	}

	var initResp struct {
		Result struct {
			ServerInfo struct{ Name string } `json:"serverInfo"`
		} `json:"result"`
		ID int `json:"id"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil {
		t.Fatal(err)
	}
	if initResp.ID != 1 || initResp.Result.ServerInfo.Name != "workspace-mcp" {
		t.Fatalf("unexpected initialize response: %s", lines[0])
	}

	var callResp struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
		ID int `json:"id"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &callResp); err != nil {
		t.Fatal(err)
	}
	if callResp.ID != 2 || !strings.Contains(callResp.Result.Content[0].Text, "# Hi") {
		t.Fatalf("unexpected tool response: %s", lines[1])
	}
}
