package test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mnehpets/http/endpoint"
	"github.com/mnehpets/http/jsonrpc"

	"github.com/mnehpets/workspace-mcp/mcp"
)

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
	ID      any             `json:"id"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// mcpFixture is a running test server plus the bearer token to reach it.
type mcpFixture struct {
	url   string
	token string
	logs  *bytes.Buffer
}

func newMCPFixture(t *testing.T, reg *mcp.Registry) *mcpFixture {
	t.Helper()
	const token = "0123456789abcdef0123456789abcdef"
	logs := &bytes.Buffer{}
	log := mcp.NewLogger("info", logs)
	server := mcp.NewServer(reg, log)
	rpc := jsonrpc.NewEndpoint()
	server.Register(rpc)
	bearer := mcp.NewBearer([]string{token}, log)

	mux := http.NewServeMux()
	mux.Handle("POST /mcp", endpoint.Handler(rpc.Endpoint, bearer))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &mcpFixture{url: ts.URL + "/mcp", token: token, logs: logs}
}

// call sends a JSON-RPC request with the fixture's valid token.
func (f *mcpFixture) call(t *testing.T, method string, params any) rpcResponse {
	t.Helper()
	return f.callWithToken(t, f.token, method, params)
}

func (f *mcpFixture) callWithToken(t *testing.T, token, method string, params any) rpcResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	req, _ := http.NewRequest("POST", f.url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("unexpected 401 for %s", method)
	}
	var rr rpcResponse
	if err := json.Unmarshal(raw, &rr); err != nil {
		t.Fatalf("decode %s response %q: %v", method, raw, err)
	}
	return rr
}

// callTool invokes tools/call and returns the parsed ToolResult plus the
// decoded domain payload (content[0].text as JSON into out, if non-nil).
func (f *mcpFixture) callTool(t *testing.T, name string, args map[string]any, out any) toolResult {
	t.Helper()
	rr := f.call(t, "tools/call", map[string]any{"name": name, "arguments": args})
	if rr.Error != nil {
		t.Fatalf("tools/call %s returned JSON-RPC error: %+v", name, rr.Error)
	}
	var tres toolResult
	if err := json.Unmarshal(rr.Result, &tres); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	if out != nil && len(tres.Content) > 0 {
		if err := json.Unmarshal([]byte(tres.Content[0].Text), out); err != nil {
			t.Fatalf("decode domain payload %q: %v", tres.Content[0].Text, err)
		}
	}
	return tres
}
