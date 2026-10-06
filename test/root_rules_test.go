package test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mnehpets/workspace-mcp/mcp"
)

// rulesTree builds a workspace that exercises the access/visibility split:
// a normal file, a gitignored file, a dotfile, a blocked file, and .git.
func rulesTree(t *testing.T, allow, block []string) *mcp.Root {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range map[string]string{
		"a.md":           "a",
		"build/out.md":   "ignored but allowed",
		".github/ci.md":  "dotdir",
		".env":           "SECRET",
		"secret.md":      "blocked",
		".git/config":    "gitdir",
		".gitignore":     "build/\n",
		"sub/.gitignore": "skip.md\n",
		"sub/skip.md":    "ignored in subdir",
		"sub/keep.md":    "kept",
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := mcp.Open(dir, mcp.WithPolicy(mcp.NewPolicy(allow, block, mcp.WithWrites(), mcp.WithGitignore())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func denied(err error, reason string) bool {
	var d *mcp.DeniedError
	return errors.As(err, &d) && d.Reason == reason
}

// Gitignore hides a file from listings but never from a read by name.
func TestRootGitignoreHidesButDoesNotDeny(t *testing.T) {
	r := rulesTree(t, []string{"**/*.md"}, []string{"secret.md"})
	l := r.Lister()
	if l.Listed("build/out.md", false) {
		t.Fatal("gitignored file should not be listed")
	}
	if l.Listed("sub/skip.md", false) {
		t.Fatal("file ignored by a nested .gitignore should not be listed")
	}
	if !l.Listed("sub/keep.md", false) || !l.Listed("a.md", false) {
		t.Fatal("ordinary files should be listed")
	}
	f, err := r.Open("build/out.md")
	if err != nil {
		t.Fatalf("a gitignored file must still be readable by name: %v", err)
	}
	f.Close()
}

// Policy denials are enforced by Root itself, and denied wins over not-found.
func TestRootEnforcesPolicy(t *testing.T) {
	r := rulesTree(t, []string{"**/*.md"}, []string{"secret.md"})
	if _, err := r.Open("secret.md"); !denied(err, "blocked_glob") {
		t.Fatalf("blocked file: %v", err)
	}
	if _, err := r.Open(".env"); !denied(err, "dotfile") {
		t.Fatalf("dotfile: %v", err)
	}
	if _, err := r.Stat("missing.txt"); !denied(err, "not_allowlisted") {
		t.Fatalf("a missing non-allowlisted path must read as denied, got %v", err)
	}
	if _, err := r.Stat("missing.md"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing allowed path is not-found, got %v", err)
	}
	if _, err := r.CreateNew("secret2.txt"); !denied(err, "not_allowlisted") {
		t.Fatalf("write outside the allow list: %v", err)
	}
	if _, err := r.WriteExisting(".env"); !denied(err, "dotfile") {
		t.Fatalf("overwrite of a dotfile: %v", err)
	}
}

// .git is blocked by default, even when an allow glob matches everything.
func TestRootGitDirAlwaysBlocked(t *testing.T) {
	r := rulesTree(t, []string{"**/*"}, nil)
	if _, err := r.Open(".git/config"); !denied(err, "blocked_glob") {
		t.Fatalf(".git/config: %v", err)
	}
	if r.Lister().Listed(".git", true) {
		t.Fatal(".git must not be listed")
	}
}

// An allow glob that names a dotfile makes it readable and listed; without one
// it is neither.
func TestRootDotfileOptIn(t *testing.T) {
	r := rulesTree(t, nil, nil)
	if r.Lister().Listed(".github/ci.md", false) {
		t.Fatal("dot directory should be hidden when no allow glob matches it")
	}
	r2 := rulesTree(t, []string{".github/**"}, nil)
	if !r2.Lister().Listed(".github/ci.md", false) {
		t.Fatal("explicit allow should list the dot directory's files")
	}
	f, err := r2.Open(".github/ci.md")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// ReadDir and WalkDir leave out unlisted entries.
func TestRootReadDirAndWalkDirFilter(t *testing.T) {
	r := rulesTree(t, []string{"**/*.md"}, []string{"secret.md"})
	entries, err := r.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	if !got["a.md"] || !got["sub"] {
		t.Fatalf("expected a.md and sub in listing, got %v", got)
	}
	for _, n := range []string{"build", ".env", ".git", ".github", "secret.md"} {
		if got[n] {
			t.Fatalf("%s should not be listed", n)
		}
	}
	var walked []string
	if err := r.WalkDir(".", func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			walked = append(walked, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"a.md": true, "sub/keep.md": true}
	if len(walked) != len(want) {
		t.Fatalf("walk = %v", walked)
	}
	for _, p := range walked {
		if !want[p] {
			t.Fatalf("unexpected walked path %s", p)
		}
	}
}

// A Root whose policy lacks WithWrites refuses every write, whatever the policy.
func TestRootReadOnlyByDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := mcp.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if _, err := r.CreateNew("b.md"); !errors.Is(err, mcp.ErrReadOnly) {
		t.Fatalf("CreateNew: %v", err)
	}
	if _, err := r.WriteExisting("a.md"); !errors.Is(err, mcp.ErrReadOnly) {
		t.Fatalf("WriteExisting: %v", err)
	}
}
