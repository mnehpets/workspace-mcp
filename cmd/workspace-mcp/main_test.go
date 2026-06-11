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

	ws, err := selectStdioWorkspace(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(ws, mcp.NewLogger("error", &bytes.Buffer{}), "(test)")
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

// Stdio has no URL to carry the workspace (§17), so selection is explicit: a
// single workspace is picked implicitly, multiple require -workspace by name.
func TestSelectStdioWorkspace(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	mk := func(specs ...mcp.WorkspaceConfig) *mcp.Registry {
		reg, err := mcp.Build(&mcp.Config{Workspaces: specs})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(reg.Close)
		return reg
	}
	one := mk(mcp.WorkspaceConfig{Name: "solo", Root: dir1, Policy: mcp.PolicyConfig{AllowGlobs: []string{"**/*"}}, Read: mcp.ReadConfig{MaxBytes: 1000}})
	two := mk(
		mcp.WorkspaceConfig{Name: "a", Root: dir1, Policy: mcp.PolicyConfig{AllowGlobs: []string{"**/*"}}, Read: mcp.ReadConfig{MaxBytes: 1000}},
		mcp.WorkspaceConfig{Name: "b", Root: dir2, Policy: mcp.PolicyConfig{AllowGlobs: []string{"**/*"}}, Read: mcp.ReadConfig{MaxBytes: 1000}},
	)

	// One workspace, no flag → that one.
	if ws, err := selectStdioWorkspace(one, ""); err != nil || ws.Name != "solo" {
		t.Fatalf("single workspace should be picked implicitly: ws=%v err=%v", ws, err)
	}
	// Multiple, no flag → error.
	if _, err := selectStdioWorkspace(two, ""); err == nil {
		t.Fatal("multiple workspaces with no -workspace should error")
	}
	// Multiple, named flag → that one.
	if ws, err := selectStdioWorkspace(two, "b"); err != nil || ws.Name != "b" {
		t.Fatalf("-workspace b should select b: ws=%v err=%v", ws, err)
	}
	// Unknown name → error.
	if _, err := selectStdioWorkspace(two, "nope"); err == nil {
		t.Fatal("-workspace with unknown name should error")
	}
}

// TestBuildVersion confirms buildVersion returns a non-empty string regardless of
// whether the binary was built with ldflags or from source.
func TestBuildVersion(t *testing.T) {
	v := buildVersion()
	if v == "" {
		t.Fatal("buildVersion must return a non-empty string")
	}
}
