package test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mnehpets/workspace-mcp/config"
	"github.com/mnehpets/workspace-mcp/workspace"
)

// rangeRegistry seeds a "default" workspace with a 10-line text file and a
// NUL-containing binary file, both allowed.
func rangeRegistry(t *testing.T) *workspace.Registry {
	t.Helper()
	dir := t.TempDir()
	// ten.md: "line1\nline2\n...\nline10\n"
	var b []byte
	for i := 1; i <= 10; i++ {
		b = append(b, "line"+itoa(i)+"\n"...)
	}
	if err := os.WriteFile(filepath.Join(dir, "ten.md"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin.md"), []byte("ab\x00cd\nmore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspaces: []config.WorkspaceConfig{{
		Name: "default", Root: dir,
		Policy: config.PolicyConfig{AllowGlobs: []string{"**/*.md"}},
		Read:   config.ReadConfig{MaxBytes: 100000},
	}}}
	reg, err := workspace.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	return reg
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var s []byte
	for n > 0 {
		s = append([]byte{byte('0' + n%10)}, s...)
		n /= 10
	}
	return string(s)
}

type rangeRead struct {
	Content    string `json:"content"`
	Truncated  bool   `json:"truncated"`
	Binary     bool   `json:"binary"`
	StartLine  int    `json:"startLine"`
	EndLine    int    `json:"endLine"`
	TotalLines int    `json:"totalLines"`
}

func TestFileReadFullUnchangedWhenNoRange(t *testing.T) {
	f := newMCPFixture(t, rangeRegistry(t))
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	var r rangeRead
	f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "ten.md"}, &r)
	if r.Content != "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10\n" {
		t.Errorf("full read content wrong: %q", r.Content)
	}
	// Line fields are omitted (zero) when no range is requested.
	if r.StartLine != 0 || r.EndLine != 0 || r.TotalLines != 0 {
		t.Errorf("expected no line fields without a range, got %+v", r)
	}
}

func TestFileReadExactSpan(t *testing.T) {
	f := newMCPFixture(t, rangeRegistry(t))
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	var r rangeRead
	f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "ten.md", "startLine": 2, "endLine": 4}, &r)
	if r.Content != "line2\nline3\nline4" {
		t.Errorf("span content wrong: %q", r.Content)
	}
	if r.StartLine != 2 || r.EndLine != 4 || r.TotalLines != 10 {
		t.Errorf("span fields wrong: %+v", r)
	}
}

func TestFileReadOpenEndedStart(t *testing.T) {
	f := newMCPFixture(t, rangeRegistry(t))
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	var r rangeRead
	f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "ten.md", "startLine": 9}, &r)
	if r.Content != "line9\nline10" || r.StartLine != 9 || r.EndLine != 10 || r.TotalLines != 10 {
		t.Errorf("open-ended start wrong: %+v", r)
	}
}

func TestFileReadOpenEndedEnd(t *testing.T) {
	f := newMCPFixture(t, rangeRegistry(t))
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	var r rangeRead
	f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "ten.md", "endLine": 2}, &r)
	if r.Content != "line1\nline2" || r.StartLine != 1 || r.EndLine != 2 || r.TotalLines != 10 {
		t.Errorf("open-ended end wrong: %+v", r)
	}
}

func TestFileReadPastEOFClamps(t *testing.T) {
	f := newMCPFixture(t, rangeRegistry(t))
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	var r rangeRead
	f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "ten.md", "startLine": 100}, &r)
	if r.Content != "" {
		t.Errorf("past-EOF span should be empty, got %q", r.Content)
	}
	if r.TotalLines != 10 {
		t.Errorf("past-EOF should still report true totalLines, got %d", r.TotalLines)
	}
	if r.Truncated {
		t.Error("a clamped past-EOF span is not truncated")
	}
}

func TestFileReadSpanClippedByMaxBytes(t *testing.T) {
	f := newMCPFixture(t, rangeRegistry(t))
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	var r rangeRead
	f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "ten.md", "startLine": 1, "endLine": 10, "maxBytes": 5}, &r)
	if len(r.Content) != 5 {
		t.Errorf("span should be clipped to 5 bytes, got %d (%q)", len(r.Content), r.Content)
	}
	if !r.Truncated {
		t.Error("a maxBytes-clipped span must set truncated")
	}
}

func TestFileReadBinaryFlaggedWithRange(t *testing.T) {
	f := newMCPFixture(t, rangeRegistry(t))
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	var r rangeRead
	f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "bin.md", "startLine": 1, "endLine": 2}, &r)
	if !r.Binary {
		t.Error("binary file must be flagged regardless of range")
	}
	if r.Content != "" {
		t.Errorf("binary content must not be returned as text, got %q", r.Content)
	}
}

func TestFileReadInvalidRange(t *testing.T) {
	f := newMCPFixture(t, rangeRegistry(t))
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	// startLine < 1
	assertToolError(t, f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "ten.md", "startLine": 0}, nil), "INVALID_RANGE")
	// startLine > endLine
	assertToolError(t, f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "ten.md", "startLine": 5, "endLine": 2}, nil), "INVALID_RANGE")
}
