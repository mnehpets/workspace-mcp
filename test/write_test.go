package test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mnehpets/workspace-mcp/mcp"
)

// writeRegistry builds a writable "default" workspace and a read-only "ro"
// workspace. Both allow markdown and block *.key; default has write.enabled.
// Returns the registry and the two root dirs so tests can read bytes back off
// disk and confirm what was actually written.
func writeRegistry(t *testing.T) (*mcp.Registry, string, string) {
	t.Helper()
	rwDir := t.TempDir()
	roDir := t.TempDir()
	seed := func(base, rel, content string) {
		p := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seed(rwDir, "doc.md", "hello world\nsecond line\n")
	seed(rwDir, "dup.md", "x\nx\nx\n")
	seed(roDir, "note.md", "read only\n")

	cfg := &mcp.Config{
		Workspaces: []mcp.WorkspaceConfig{
			{
				Name: "default", Root: rwDir,
				Policy: mcp.PolicyConfig{AllowGlobs: []string{"**/*.md"}, BlockGlobs: []string{"**/*.key"}},
				Read:   mcp.ReadConfig{MaxBytes: 64},
				Grep:   mcp.GrepConfig{Enabled: true, MaxMatches: 500},
				Write:  mcp.WriteConfig{Enabled: true},
			},
			{
				Name: "ro", Root: roDir,
				Policy: mcp.PolicyConfig{AllowGlobs: []string{"**/*.md"}},
				Read:   mcp.ReadConfig{MaxBytes: 64},
				Write:  mcp.WriteConfig{Enabled: false},
			},
		},
	}
	reg, err := mcp.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	return reg, rwDir, roDir
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func readDisk(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func TestFileCreate(t *testing.T) {
	reg, rwDir, _ := writeRegistry(t)
	f := newMCPFixture(t, reg)

	// Success: auto-creates the missing parent dir and writes exact bytes.
	body := "fresh\ncontent\n"
	var res struct {
		Path         string `json:"path"`
		BytesWritten int    `json:"bytesWritten"`
		SHA256       string `json:"sha256"`
	}
	f.callTool(t, "file_create", map[string]any{"path": "new/dir/a.md", "contents": body}, &res)
	if res.BytesWritten != len(body) || res.SHA256 != sha256hex(body) {
		t.Fatalf("unexpected create result: %+v (want sha %s)", res, sha256hex(body))
	}
	if got := readDisk(t, rwDir, "new/dir/a.md"); got != body {
		t.Fatalf("on-disk contents %q != %q", got, body)
	}

	// Collision: an existing path is never clobbered.
	assertToolError(t, f.callTool(t, "file_create", map[string]any{"path": "doc.md", "contents": "x"}, nil), "PATH_EXISTS")

	// Blocked path stays unwritable (same policy as a read).
	assertToolError(t, f.callTool(t, "file_create", map[string]any{"path": "secret.key", "contents": "x"}, nil), "POLICY_DENIED")

	// Traversal/absolute rejected before touching the FS.
	assertToolError(t, f.callTool(t, "file_create", map[string]any{"path": "../escape.md", "contents": "x"}, nil), "POLICY_DENIED")
}

func TestFileOverwrite(t *testing.T) {
	reg, rwDir, _ := writeRegistry(t)
	f := newMCPFixture(t, reg)

	// Absent path -> NOT_FOUND (no silent create).
	assertToolError(t, f.callTool(t, "file_overwrite", map[string]any{"path": "nope.md", "contents": "x"}, nil), "NOT_FOUND")

	// base_sha256 mismatch is rejected; matching hash succeeds.
	cur := readDisk(t, rwDir, "doc.md")
	assertToolError(t, f.callTool(t, "file_overwrite", map[string]any{
		"path": "doc.md", "contents": "x", "base_sha256": sha256hex("WRONG"),
	}, nil), "BASE_SHA_MISMATCH")

	repl := "rewritten\n"
	var res struct {
		SHA256 string `json:"sha256"`
	}
	f.callTool(t, "file_overwrite", map[string]any{
		"path": "doc.md", "contents": repl, "base_sha256": sha256hex(cur),
	}, &res)
	if res.SHA256 != sha256hex(repl) {
		t.Fatalf("overwrite sha %s != %s", res.SHA256, sha256hex(repl))
	}
	if got := readDisk(t, rwDir, "doc.md"); got != repl {
		t.Fatalf("on-disk %q != %q", got, repl)
	}

	// dry_run previews without writing.
	var dr struct {
		SHA256 string `json:"sha256"`
		DryRun bool   `json:"dryRun"`
	}
	f.callTool(t, "file_overwrite", map[string]any{"path": "doc.md", "contents": "ignored", "dry_run": true}, &dr)
	if !dr.DryRun || dr.SHA256 != sha256hex("ignored") {
		t.Fatalf("unexpected dry_run result: %+v", dr)
	}
	if got := readDisk(t, rwDir, "doc.md"); got != repl {
		t.Fatalf("dry_run must not write: on-disk now %q", got)
	}
}

func TestFileReplace(t *testing.T) {
	reg, rwDir, _ := writeRegistry(t)
	f := newMCPFixture(t, reg)

	// Empty old_str rejected.
	assertToolError(t, f.callTool(t, "file_replace", map[string]any{"path": "doc.md", "old_str": "", "new_str": "x"}, nil), "INVALID_ARGS")

	// Unique single replacement.
	var res struct {
		Replacements int    `json:"replacements"`
		SHA256       string `json:"sha256"`
	}
	f.callTool(t, "file_replace", map[string]any{"path": "doc.md", "old_str": "world", "new_str": "there"}, &res)
	if res.Replacements != 1 {
		t.Fatalf("want 1 replacement, got %d", res.Replacements)
	}
	want := "hello there\nsecond line\n"
	if got := readDisk(t, rwDir, "doc.md"); got != want {
		t.Fatalf("on-disk %q != %q", got, want)
	}

	// Default expects exactly one match: 3 occurrences -> MATCH_COUNT_MISMATCH.
	assertToolError(t, f.callTool(t, "file_replace", map[string]any{"path": "dup.md", "old_str": "x", "new_str": "y"}, nil), "MATCH_COUNT_MISMATCH")

	// Zero occurrences -> MATCH_COUNT_MISMATCH too.
	assertToolError(t, f.callTool(t, "file_replace", map[string]any{"path": "dup.md", "old_str": "absent", "new_str": "y"}, nil), "MATCH_COUNT_MISMATCH")

	// expected_replacements lets the caller change all N deliberately.
	f.callTool(t, "file_replace", map[string]any{"path": "dup.md", "old_str": "x", "new_str": "y", "expected_replacements": 3}, &res)
	if res.Replacements != 3 {
		t.Fatalf("want 3 replacements, got %d", res.Replacements)
	}
	if got := readDisk(t, rwDir, "dup.md"); got != "y\ny\ny\n" {
		t.Fatalf("on-disk %q", got)
	}

	// dry_run previews the count + hash without writing.
	var dr struct {
		Replacements int  `json:"replacements"`
		DryRun       bool `json:"dryRun"`
	}
	f.callTool(t, "file_replace", map[string]any{"path": "doc.md", "old_str": "there", "new_str": "z", "dry_run": true}, &dr)
	if !dr.DryRun || dr.Replacements != 1 {
		t.Fatalf("unexpected dry_run: %+v", dr)
	}
	if got := readDisk(t, rwDir, "doc.md"); got != want {
		t.Fatalf("dry_run must not write: %q", got)
	}

	// Empty new_str is the delete-text path: the matched span is excised, the
	// rest of the file untouched. doc.md is currently "hello there\nsecond line\n".
	f.callTool(t, "file_replace", map[string]any{"path": "doc.md", "old_str": "\nsecond line", "new_str": ""}, &res)
	if res.Replacements != 1 {
		t.Fatalf("want 1 replacement, got %d", res.Replacements)
	}
	if got := readDisk(t, rwDir, "doc.md"); got != "hello there\n" {
		t.Fatalf("delete-text path wrong: on-disk %q", got)
	}
}

// A file past the workspace read limit can't be edited in place (the whole file
// must be read to match safely) -> FILE_TOO_LARGE, never a partial replace.
func TestFileReplaceTooLarge(t *testing.T) {
	reg, rwDir, _ := writeRegistry(t)
	f := newMCPFixture(t, reg)
	big := strings.Repeat("A", 200) + "needle" // > 64-byte read cap
	if err := os.WriteFile(filepath.Join(rwDir, "big.md"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	assertToolError(t, f.callTool(t, "file_replace", map[string]any{"path": "big.md", "old_str": "needle", "new_str": "x"}, nil), "FILE_TOO_LARGE")
}

// Raw bytes in, raw bytes out: a trailing newline and CRLF survive create and
// replace untouched (no normalization that could move an edit to the wrong place).
func TestWriteNoNormalization(t *testing.T) {
	reg, rwDir, _ := writeRegistry(t)
	f := newMCPFixture(t, reg)
	body := "a\r\nb\r\nno-trailing-newline"
	f.callTool(t, "file_create", map[string]any{"path": "crlf.md", "contents": body}, nil)
	if got := readDisk(t, rwDir, "crlf.md"); got != body {
		t.Fatalf("CRLF/no-newline not preserved: %q != %q", got, body)
	}
	// Replace an exact CRLF-bearing span, raw.
	f.callTool(t, "file_replace", map[string]any{"path": "crlf.md", "old_str": "a\r\nb", "new_str": "A\r\nB"}, nil)
	if got := readDisk(t, rwDir, "crlf.md"); got != "A\r\nB\r\nno-trailing-newline" {
		t.Fatalf("raw replace wrong: %q", got)
	}
}

// A write-disabled workspace exposes no write tools and rejects any forced call.
func TestWriteDisabledWorkspace(t *testing.T) {
	reg, _, roDir := writeRegistry(t)
	f := newMCPFixture(t, reg)

	assertToolError(t, f.callToolWS(t, "ro", "file_create", map[string]any{"path": "x.md", "contents": "x"}, nil), "READ_ONLY")
	assertToolError(t, f.callToolWS(t, "ro", "file_overwrite", map[string]any{"path": "note.md", "contents": "x"}, nil), "READ_ONLY")
	assertToolError(t, f.callToolWS(t, "ro", "file_replace", map[string]any{"path": "note.md", "old_str": "read", "new_str": "x"}, nil), "READ_ONLY")
	// Nothing was written.
	if got := readDisk(t, roDir, "note.md"); got != "read only\n" {
		t.Fatalf("read-only workspace was mutated: %q", got)
	}
}

// tools/list advertises the write tools only when write.enabled.
func TestWriteToolsListVisibility(t *testing.T) {
	reg, _, _ := writeRegistry(t)
	f := newMCPFixture(t, reg)

	writeTools := func(ws string) map[string]map[string]any {
		rr := f.callURL(t, f.wsURL(ws), f.token, "tools/list", map[string]any{})
		if rr.Error != nil {
			t.Fatalf("tools/list %s: %+v", ws, rr.Error)
		}
		var res struct {
			Tools []struct {
				Name        string         `json:"name"`
				Annotations map[string]any `json:"annotations"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(rr.Result, &res); err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]any{}
		for _, tl := range res.Tools {
			if strings.HasPrefix(tl.Name, "file_create") || strings.HasPrefix(tl.Name, "file_overwrite") || strings.HasPrefix(tl.Name, "file_replace") {
				out[tl.Name] = tl.Annotations
			}
		}
		return out
	}

	rw := writeTools("default")
	for _, n := range []string{"file_create", "file_overwrite", "file_replace"} {
		if _, ok := rw[n]; !ok {
			t.Errorf("writable workspace missing %q in tools/list", n)
		}
		if rw[n] != nil && rw[n]["readOnlyHint"] != false {
			t.Errorf("%q must have readOnlyHint=false: %+v", n, rw[n])
		}
	}
	// create is non-destructive; in-place ops are destructive.
	if rw["file_create"]["destructiveHint"] != false {
		t.Errorf("file_create should not be destructive: %+v", rw["file_create"])
	}
	if rw["file_overwrite"]["destructiveHint"] != true || rw["file_replace"]["destructiveHint"] != true {
		t.Errorf("in-place ops must be destructive: %+v / %+v", rw["file_overwrite"], rw["file_replace"])
	}

	if ro := writeTools("ro"); len(ro) != 0 {
		t.Errorf("read-only workspace must not advertise write tools: %+v", ro)
	}
}

// The writable workspace's instructions flip to mention editing; the read-only
// one keeps the read-only constraint.
func TestWriteInstructionsFlip(t *testing.T) {
	reg, _, _ := writeRegistry(t)
	f := newMCPFixture(t, reg)

	instr := func(ws string) string {
		rr := f.callURL(t, f.wsURL(ws), f.token, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
		var res struct {
			Instructions string `json:"instructions"`
		}
		if err := json.Unmarshal(rr.Result, &res); err != nil {
			t.Fatal(err)
		}
		return res.Instructions
	}

	rw := instr("default")
	if !strings.Contains(rw, "file_create") || !strings.Contains(rw, "file_replace") {
		t.Errorf("writable instructions should mention the edit tools: %q", rw)
	}
	ro := instr("ro")
	if !strings.Contains(ro, "read-only") {
		t.Errorf("read-only instructions should state read-only: %q", ro)
	}
	if strings.Contains(ro, "file_create") {
		t.Errorf("read-only instructions must not advertise editing: %q", ro)
	}
}
