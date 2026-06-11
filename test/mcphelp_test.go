package test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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

// mcpFixture is a running test server plus the bearer token to reach it. With
// workspace-per-URL (§17) the server mounts one MCP endpoint per workspace at
// /mcp/<name>; `url` targets the "default" workspace, and wsURL/callToolWS reach
// a named one.
type mcpFixture struct {
	base  string // scheme://host
	url   string // base + /mcp/default
	token string
	logs  *bytes.Buffer
}

func newMCPFixture(t *testing.T, reg *mcp.Registry) *mcpFixture {
	t.Helper()
	const token = "0123456789abcdef0123456789abcdef"
	logs := &bytes.Buffer{}
	log := mcp.NewLogger("info", logs)
	handler := mcp.BuildHandler(reg, log, []string{token}, nil, nil, false, "(test)")
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &mcpFixture{base: ts.URL, url: ts.URL + "/mcp/default", token: token, logs: logs}
}

// wsURL is the MCP endpoint for a named workspace.
func (f *mcpFixture) wsURL(name string) string { return f.base + "/mcp/" + name }

// statusFor POSTs an authenticated tools/list to url and returns the HTTP status
// code (used to assert routing-level outcomes like a 404 for an unknown
// workspace segment).
func (f *mcpFixture) statusFor(t *testing.T, url string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}})
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// call sends a JSON-RPC request to the default workspace with a valid token.
func (f *mcpFixture) call(t *testing.T, method string, params any) rpcResponse {
	t.Helper()
	return f.callURL(t, f.url, f.token, method, params)
}

func (f *mcpFixture) callWithToken(t *testing.T, token, method string, params any) rpcResponse {
	t.Helper()
	return f.callURL(t, f.url, token, method, params)
}

func (f *mcpFixture) callURL(t *testing.T, url, token, method string, params any) rpcResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
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

// callTool invokes tools/call against the default workspace.
func (f *mcpFixture) callTool(t *testing.T, name string, args map[string]any, out any) toolResult {
	t.Helper()
	return f.callToolWS(t, "default", name, args, out)
}

// callToolWS invokes tools/call against a named workspace's endpoint and returns
// the parsed ToolResult plus the decoded domain payload (content[0].text as JSON
// into out, if non-nil).
func (f *mcpFixture) callToolWS(t *testing.T, ws, name string, args map[string]any, out any) toolResult {
	t.Helper()
	rr := f.callURL(t, f.wsURL(ws), f.token, "tools/call", map[string]any{"name": name, "arguments": args})
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
