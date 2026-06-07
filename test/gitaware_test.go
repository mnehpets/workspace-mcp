package test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/mnehpets/workspace-mcp/gitaware"
)

func TestDetectNonGit(t *testing.T) {
	if gitaware.Detect(t.TempDir()) {
		t.Fatal("plain dir should not be detected as a git repo")
	}
}

func TestStatusNonGitErr(t *testing.T) {
	if _, err := gitaware.GetStatus(t.TempDir()); err != gitaware.ErrNotGitRepo {
		t.Fatalf("expected ErrNotGitRepo, got %v", err)
	}
}

func TestStatusAndTracked(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("committed.txt", "v1\n")
	if _, err := wt.Add("committed.txt"); err != nil {
		t.Fatal(err)
	}
	_, err = wt.Commit("initial", &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@e", When: time.Unix(0, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !gitaware.Detect(dir) {
		t.Fatal("should detect git repo")
	}

	write("committed.txt", "v1\nv2\n") // unstaged modification
	write("staged.txt", "new\n")
	if _, err := wt.Add("staged.txt"); err != nil { // staged add
		t.Fatal(err)
	}
	write("untracked.txt", "u\n") // untracked

	st, err := gitaware.GetStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Branch == "" {
		t.Fatal("expected a branch name after a commit")
	}
	codes := map[string]string{}
	for _, f := range st.Files {
		codes[f.Path] = f.Status
	}
	if codes["untracked.txt"] != "??" {
		t.Errorf("untracked.txt: want ??, got %q", codes["untracked.txt"])
	}
	if codes["staged.txt"] == "" || codes["staged.txt"][0] != 'A' {
		t.Errorf("staged.txt: want staged A, got %q", codes["staged.txt"])
	}
	if codes["committed.txt"] == "" || codes["committed.txt"][1] != 'M' {
		t.Errorf("committed.txt: want worktree M, got %q", codes["committed.txt"])
	}

	tracked, err := gitaware.TrackedFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	// committed.txt and staged.txt are in the index; untracked.txt is not.
	hasTracked := map[string]bool{}
	for _, f := range tracked {
		hasTracked[f] = true
	}
	if !hasTracked["committed.txt"] || !hasTracked["staged.txt"] {
		t.Errorf("expected committed.txt and staged.txt tracked, got %v", tracked)
	}
	if hasTracked["untracked.txt"] {
		t.Errorf("untracked.txt should not be tracked")
	}
}
