package test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestLargeToolCallBody guards task 22: a tools/call whose JSON body carries a
// legitimate large write payload must reach the handler and succeed, not die at
// the request-decode layer's field-size limit. A ~20 KB file_create exceeds the
// old 16 KB per-field cap in github.com/mnehpets/http (v0.6.0); the v0.6.1 bump
// raises the jsonrpc body limit to 12 MB, so this passes. file_create does not
// cap contents by read.maxBytes, so the only thing that can reject the payload
// is the transport — which is exactly what this isolates.
func TestLargeToolCallBody(t *testing.T) {
	reg, rwDir, _ := writeRegistry(t)
	f := newMCPFixture(t, reg)

	const size = 20 * 1024
	body := strings.Repeat("abcdefghij", size/10) // 20 KB of content

	reqBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "file_create",
			"arguments": map[string]any{"path": "big.md", "contents": body},
		},
	})
	if len(reqBody) <= 16*1024 {
		t.Fatalf("request body %d bytes does not exceed the old 16 KB field cap; widen the payload", len(reqBody))
	}

	req, _ := http.NewRequest("POST", f.url, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("oversize body rejected at transport: HTTP %d, body %q", resp.StatusCode, raw)
	}
	var rr rpcResponse
	if err := json.Unmarshal(raw, &rr); err != nil {
		t.Fatalf("decode response %q: %v", raw, err)
	}
	if rr.Error != nil {
		t.Fatalf("JSON-RPC error for large body: %+v", rr.Error)
	}
	var tres toolResult
	if err := json.Unmarshal(rr.Result, &tres); err != nil {
		t.Fatalf("decode tool result %q: %v", rr.Result, err)
	}
	if tres.IsError {
		t.Fatalf("file_create returned an error result: %q", tres.Content[0].Text)
	}
	if got := readDisk(t, rwDir, "big.md"); got != body {
		t.Fatalf("written content mismatch: got %d bytes, want %d", len(got), len(body))
	}
}
