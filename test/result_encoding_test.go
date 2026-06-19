package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mnehpets/workspace-mcp/mcp"
)

// A line that exercises every character a tool result tends to mangle: HTML
// metacharacters ('<' '>' '&') and a backslash (e.g. a Windows path / regex).
const escapeSampleLine = `if a < b && c > d { p = "C:\tmp\x" }`

// escapeRegistry builds a single read-only workspace holding one file whose body
// is escapeSampleLine, so the encoding of a real file_read result can be checked
// end-to-end through the HTTP server.
func escapeRegistry(t *testing.T) *mcp.Registry {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.md"), []byte(escapeSampleLine), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &mcp.Config{
		Workspaces: []mcp.WorkspaceConfig{{
			Name: "default", Root: dir,
			Policy: mcp.PolicyConfig{AllowGlobs: []string{"**/*.md"}},
			Read:   mcp.ReadConfig{MaxBytes: 4096},
			Grep:   mcp.GrepConfig{Enabled: true, MaxMatches: 500},
		}},
	}
	reg, err := mcp.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	return reg
}

// TestResultTextBlockNoHTMLEscape pins the text-content half of the escaping fix
// (PLAN.md §27): tool-result JSON is serialized with HTML escaping OFF, so '<'
// '>' '&' reach a text-only client as literals instead of < etc. — the
// gratuitous escaping that broke copying file content into a file_replace old_str.
func TestResultTextBlockNoHTMLEscape(t *testing.T) {
	f := newMCPFixture(t, escapeRegistry(t))
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})

	tres := f.callTool(t, "file_read", map[string]any{"path": "sample.md"}, nil)
	if len(tres.Content) == 0 {
		t.Fatal("no content in file_read result")
	}
	text := tres.Content[0].Text

	// The escaped forms (<, >, &) must be absent — check the
	// ASCII tail to avoid writing the backslash sequence in a literal.
	if strings.Contains(text, "u003c") || strings.Contains(text, "u003e") || strings.Contains(text, "u0026") {
		t.Errorf("text block should not HTML-escape '<' '>' '&':\n%s", text)
	}
	if !strings.Contains(text, "a < b && c > d") {
		t.Errorf("text block should carry literal '<' '>' '&':\n%s", text)
	}
}

// TestResultStructuredContent pins the structuredContent half of the fix: the
// result is also returned as a structuredContent object, which a client decodes
// straight off the wire with NO JSON-escaping artifacts — so the file content,
// backslashes and all, arrives verbatim. This is the path that fixes the `\`
// confusion that the text block (standard JSON `\\`) cannot.
func TestResultStructuredContent(t *testing.T) {
	f := newMCPFixture(t, escapeRegistry(t))
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})

	rr := f.call(t, "tools/call", map[string]any{
		"name":      "file_read",
		"arguments": map[string]any{"path": "sample.md"},
	})
	if rr.Error != nil {
		t.Fatalf("tools/call file_read: %+v", rr.Error)
	}

	var res struct {
		StructuredContent struct {
			Content string `json:"content"`
		} `json:"structuredContent"`
	}
	if err := json.Unmarshal(rr.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.StructuredContent.Content == "" {
		t.Fatal("structuredContent missing or empty; expected the file_read payload")
	}
	if res.StructuredContent.Content != escapeSampleLine {
		t.Errorf("structuredContent must deliver content verbatim (incl. '\\'):\n got: %q\nwant: %q",
			res.StructuredContent.Content, escapeSampleLine)
	}
}
