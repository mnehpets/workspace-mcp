package test

import (
	"testing"
)

// shaRead captures the sha256 (and a few neighbours) off a file_read response.
type shaRead struct {
	Content   string `json:"content"`
	SHA256    string `json:"sha256"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
}

// file_read attaches the hex SHA-256 of the file's full on-disk bytes — the same
// hash base_sha256 checks — so a read-then-write loop carries it forward with no
// extra hash pass (§8.3, task 22).
func TestFileReadSHA256(t *testing.T) {
	reg, _, _ := writeRegistry(t)
	f := newMCPFixture(t, reg)
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})

	const content = "hello world\nsecond line\n" // doc.md, seeded by writeRegistry
	want := sha256hex(content)

	var r shaRead
	f.callTool(t, "file_read", map[string]any{"path": "doc.md"}, &r)
	if r.SHA256 != want {
		t.Fatalf("sha256 mismatch: got %q want %q", r.SHA256, want)
	}
}

// The hash covers the full file even for a ranged read (or a maxBytes-truncated
// one) — it is the on-disk state, not the returned slice.
func TestFileReadSHA256StableAcrossRange(t *testing.T) {
	reg, _, _ := writeRegistry(t)
	f := newMCPFixture(t, reg)
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	want := sha256hex("hello world\nsecond line\n")

	var full, ranged, capped shaRead
	f.callTool(t, "file_read", map[string]any{"path": "doc.md"}, &full)
	f.callTool(t, "file_read", map[string]any{"path": "doc.md", "startLine": 2, "endLine": 2}, &ranged)
	f.callTool(t, "file_read", map[string]any{"path": "doc.md", "maxBytes": 5}, &capped)

	if ranged.Content != "second line" {
		t.Fatalf("ranged content wrong: %q", ranged.Content)
	}
	if !capped.Truncated {
		t.Fatalf("a maxBytes=5 read of a 24-byte file should be truncated")
	}
	for _, got := range []string{full.SHA256, ranged.SHA256, capped.SHA256} {
		if got != want {
			t.Fatalf("sha256 should be the full-file hash regardless of range/cap: got %q want %q", got, want)
		}
	}
}

// The captured sha256 round-trips into a write's base_sha256: it matches the
// real on-disk state, so the guarded write succeeds; a stale hash is rejected
// with BASE_SHA_MISMATCH.
func TestFileReadSHA256FeedsBaseSHA(t *testing.T) {
	reg, _, _ := writeRegistry(t)
	f := newMCPFixture(t, reg)
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})

	var r shaRead
	f.callTool(t, "file_read", map[string]any{"path": "doc.md"}, &r)

	// Matches → the guarded replace goes through.
	if tres := f.callTool(t, "file_replace", map[string]any{
		"path": "doc.md", "old_str": "world", "new_str": "there", "base_sha256": r.SHA256,
	}, nil); tres.IsError {
		t.Fatalf("replace with the captured sha256 should succeed, got error: %+v", tres)
	}

	// The file changed; the now-stale hash must be rejected.
	assertToolError(t, f.callTool(t, "file_replace", map[string]any{
		"path": "doc.md", "old_str": "there", "new_str": "world", "base_sha256": r.SHA256,
	}, nil), "BASE_SHA_MISMATCH")
}
