package test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mnehpets/workspace-mcp/fsroot"
	"github.com/mnehpets/workspace-mcp/grrep"
	"github.com/mnehpets/workspace-mcp/policy"
	"github.com/mnehpets/workspace-mcp/search"
)

func searchTree(t *testing.T) (*fsroot.Root, *policy.Policy, *grrep.IgnoreSet) {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", "package main\nfunc Hello() {}\n// TODO: fix\n")
	write("docs/guide.md", "Use the ASC workflow here.\nAnother line.\n")
	write("docs/notes.md", "nothing interesting\n")
	write("secret.key", "PRIVATE-MATERIAL TODO\n")      // blocked by policy
	write("vendor/lib.go", "func TODO_vendored() {}\n") // ignored via .gitignore
	write(".gitignore", "vendor/\n")
	write("bin.dat", "abc\x00TODO\x00def\n") // binary, must be skipped

	r, err := fsroot.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	pol := policy.New([]string{"**/*.go", "**/*.md"}, []string{"**/*.key"})
	ig := grrep.NewIgnoreSet(dir)
	return r, pol, ig
}

func TestGrepLiteral(t *testing.T) {
	r, pol, ig := searchTree(t)
	res, err := search.Grep(r, pol, ig, search.GrepRequest{Pattern: "TODO", FixedString: true}, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 {
		t.Fatalf("expected 1 match (only a.go), got %d: %+v", len(res.Matches), res.Matches)
	}
	if res.Matches[0].Path != "a.go" || res.Matches[0].Line != 3 {
		t.Fatalf("unexpected match: %+v", res.Matches[0])
	}
}

func TestGrepBlockedAndIgnoredAndBinarySkipped(t *testing.T) {
	r, pol, ig := searchTree(t)
	res, err := search.Grep(r, pol, ig, search.GrepRequest{Pattern: "TODO", FixedString: true}, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res.Matches {
		if m.Path == "secret.key" || m.Path == "vendor/lib.go" || m.Path == "bin.dat" {
			t.Fatalf("blocked/ignored/binary path leaked: %s", m.Path)
		}
	}
}

func TestGrepRegex(t *testing.T) {
	r, pol, ig := searchTree(t)
	res, err := search.Grep(r, pol, ig, search.GrepRequest{Pattern: "func [A-Z]", FixedString: false}, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 || res.Matches[0].Text != "func Hello() {}" {
		t.Fatalf("regex match unexpected: %+v", res.Matches)
	}
}

func TestGrepCaseInsensitive(t *testing.T) {
	r, pol, ig := searchTree(t)
	res, err := search.Grep(r, pol, ig, search.GrepRequest{Pattern: "asc workflow", FixedString: true, CaseInsensitive: true}, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 {
		t.Fatalf("expected 1 case-insensitive match, got %+v", res.Matches)
	}
}

func TestGrepInvalidPattern(t *testing.T) {
	r, pol, ig := searchTree(t)
	_, err := search.Grep(r, pol, ig, search.GrepRequest{Pattern: "func (", FixedString: false}, 0, 500)
	if _, ok := err.(*search.InvalidPatternError); !ok {
		t.Fatalf("expected InvalidPatternError, got %v", err)
	}
}

func TestGrepTruncation(t *testing.T) {
	r, pol, ig := searchTree(t)
	res, err := search.Grep(r, pol, ig, search.GrepRequest{Pattern: "e", FixedString: true}, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("expected truncated=true with maxMatches=1")
	}
	if len(res.Matches) != 1 {
		t.Fatalf("expected exactly 1 match at cap, got %d", len(res.Matches))
	}
}

func TestFind(t *testing.T) {
	r, pol, ig := searchTree(t)
	res, err := search.Find(r, pol, ig, "guide", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) == 0 || res.Files[0] != "docs/guide.md" {
		t.Fatalf("expected docs/guide.md first, got %+v", res.Files)
	}
	for _, f := range res.Files {
		if f == "secret.key" || f == "vendor/lib.go" {
			t.Fatalf("blocked/ignored file in find results: %s", f)
		}
	}
}
