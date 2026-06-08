package test

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/mnehpets/workspace-mcp/mcp"
)

// buildTree creates a sandbox tree with a known file and returns the root dir
// plus an opened *mcp.Root.
func buildTree(t *testing.T) (string, *mcp.Root) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "inside.txt"), []byte("in-tree content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "nested.txt"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := mcp.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return dir, r
}

func TestEscapeAbsolutePathRejected(t *testing.T) {
	_, r := buildTree(t)
	if _, err := r.Open("/etc/passwd"); err == nil {
		t.Fatal("absolute path must be rejected")
	}
}

func TestEscapeDotDotRejected(t *testing.T) {
	_, r := buildTree(t)
	for _, p := range []string{"../outside", "sub/../../escape", "a/b/../../../etc/passwd"} {
		if _, err := r.Open(p); err == nil {
			t.Fatalf("traversal %q must be rejected", p)
		}
	}
}

func TestEscapeSymlinkToOutsideCannotRead(t *testing.T) {
	dir, r := buildTree(t)
	// Create a secret outside the tree and a symlink inside pointing at it.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "escape-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	f, err := r.Open("escape-link")
	if err == nil {
		b, _ := io.ReadAll(f)
		f.Close()
		t.Fatalf("symlink escape leaked content: %q", b)
	}
}

func TestEscapeSymlinkOutsideDirCannotEscape(t *testing.T) {
	dir, r := buildTree(t)
	// A symlink to an outside *directory*; reading a file "through" it must fail.
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "x.txt"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "dlink")
	if err := os.Symlink(outDir, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := r.Open("dlink/x.txt"); err == nil {
		t.Fatal("reading through an outward dir symlink must fail")
	}
}

func TestInTreeSymlinkWorks(t *testing.T) {
	dir, r := buildTree(t)
	link := filepath.Join(dir, "good-link")
	// Relative, in-tree target: os.Root rejects absolute symlink targets even
	// when they happen to point inside, so a valid in-tree link must be relative.
	if err := os.Symlink("inside.txt", link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	f, err := r.Open("good-link")
	if err != nil {
		t.Fatalf("in-tree symlink should resolve: %v", err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "in-tree content" {
		t.Fatalf("got %q", b)
	}
}

// A symlink swapped in concurrently must never let an open escape. os.Root
// re-resolves on every call, so even if a benign name is replaced by an
// outward symlink between calls, the next open is still contained.
func TestTOCTOUSymlinkSwapCannotEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	dir, r := buildTree(t)
	outside := filepath.Join(t.TempDir(), "swap-secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "swap")
	if err := os.WriteFile(target, []byte("benign"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Swap the regular file for an outward symlink.
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if f, err := r.Open("swap"); err == nil {
		b, _ := io.ReadAll(f)
		f.Close()
		t.Fatalf("post-swap open escaped: %q", b)
	}
}

func TestReadDirContained(t *testing.T) {
	_, r := buildTree(t)
	entries, err := r.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, e := range entries {
		found[e.Name()] = true
	}
	if !found["inside.txt"] || !found["sub"] {
		t.Fatalf("expected inside.txt and sub in listing, got %v", found)
	}
}
