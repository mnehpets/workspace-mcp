package test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
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

// --- upstream tracking helpers ---

// makeCommit creates a commit on the worktree's current branch and returns the hash.
func makeCommit(t *testing.T, dir string, repo *gogit.Repository, msg string) plumbing.Hash {
	t.Helper()
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, msg+".txt")
	if err := os.WriteFile(f, []byte(msg), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add(msg + ".txt"); err != nil {
		t.Fatal(err)
	}
	h, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@e", When: time.Unix(0, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// currentBranch returns the short name of HEAD's target branch.
func currentBranch(t *testing.T, repo *gogit.Repository) string {
	t.Helper()
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	return head.Name().Short()
}

// setTracking writes a fake remote-tracking ref and wires the branch config,
// adding a stub remote entry so go-git's config validation accepts the branch.
func setTracking(t *testing.T, repo *gogit.Repository, branchName, remote string, trackingHash plumbing.Hash) {
	t.Helper()
	trackingRefName := plumbing.NewRemoteReferenceName(remote, branchName)
	if err := repo.Storer.SetReference(plumbing.NewHashReference(trackingRefName, trackingHash)); err != nil {
		t.Fatal(err)
	}
	cfg, err := repo.Config()
	if err != nil {
		t.Fatal(err)
	}
	// go-git validates that a branch's remote exists in cfg.Remotes.
	if cfg.Remotes == nil {
		cfg.Remotes = make(map[string]*gitconfig.RemoteConfig)
	}
	if _, ok := cfg.Remotes[remote]; !ok {
		cfg.Remotes[remote] = &gitconfig.RemoteConfig{
			Name: remote,
			URLs: []string{"https://example.com/fake.git"},
		}
	}
	if cfg.Branches == nil {
		cfg.Branches = make(map[string]*gitconfig.Branch)
	}
	cfg.Branches[branchName] = &gitconfig.Branch{
		Name:   branchName,
		Remote: remote,
		Merge:  plumbing.NewBranchReferenceName(branchName),
	}
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

// initRepo creates a bare-minimum git repo and returns the opened *Repository.
func initRepo(t *testing.T, dir string) *gogit.Repository {
	t.Helper()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

// --- upstream tests ---

func TestUpstreamInSync(t *testing.T) {
	dir := t.TempDir()
	repo := initRepo(t, dir)
	h := makeCommit(t, dir, repo, "base")
	branch := currentBranch(t, repo)
	setTracking(t, repo, branch, "origin", h)

	st, err := gitaware.GetStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	u := st.Upstream
	if u == nil {
		t.Fatal("upstream should not be nil")
	}
	if !u.InSync || u.Ahead != 0 || u.Behind != 0 {
		t.Errorf("expected in-sync, got ahead=%d behind=%d inSync=%v", u.Ahead, u.Behind, u.InSync)
	}
}

func TestUpstreamAhead(t *testing.T) {
	dir := t.TempDir()
	repo := initRepo(t, dir)
	base := makeCommit(t, dir, repo, "base")
	branch := currentBranch(t, repo)
	setTracking(t, repo, branch, "origin", base)
	makeCommit(t, dir, repo, "local1")
	makeCommit(t, dir, repo, "local2")

	st, err := gitaware.GetStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	u := st.Upstream
	if u == nil {
		t.Fatal("upstream should not be nil")
	}
	if u.Ahead != 2 || u.Behind != 0 || u.InSync {
		t.Errorf("expected ahead=2 behind=0, got ahead=%d behind=%d inSync=%v", u.Ahead, u.Behind, u.InSync)
	}
}

func TestUpstreamBehind(t *testing.T) {
	dir := t.TempDir()
	repo := initRepo(t, dir)
	makeCommit(t, dir, repo, "base")
	branch := currentBranch(t, repo)

	// Create two "remote" commits by making them on a temp branch.
	wt, _ := repo.Worktree()
	if err := wt.Checkout(&gogit.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("remote-branch"), Create: true}); err != nil {
		t.Fatal(err)
	}
	makeCommit(t, dir, repo, "remote1")
	remoteTip := makeCommit(t, dir, repo, "remote2")
	// Return to the default branch (which is still at base).
	if err := wt.Checkout(&gogit.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(branch)}); err != nil {
		t.Fatal(err)
	}
	setTracking(t, repo, branch, "origin", remoteTip)

	st, err := gitaware.GetStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	u := st.Upstream
	if u == nil {
		t.Fatal("upstream should not be nil")
	}
	if u.Behind != 2 || u.Ahead != 0 || u.InSync {
		t.Errorf("expected ahead=0 behind=2, got ahead=%d behind=%d inSync=%v", u.Ahead, u.Behind, u.InSync)
	}
}

func TestUpstreamDiverged(t *testing.T) {
	dir := t.TempDir()
	repo := initRepo(t, dir)
	makeCommit(t, dir, repo, "base")
	branch := currentBranch(t, repo)

	// Create one remote commit on a side branch.
	wt, _ := repo.Worktree()
	if err := wt.Checkout(&gogit.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("remote-branch"), Create: true}); err != nil {
		t.Fatal(err)
	}
	remoteTip := makeCommit(t, dir, repo, "remote1")
	if err := wt.Checkout(&gogit.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(branch)}); err != nil {
		t.Fatal(err)
	}
	// One local commit.
	makeCommit(t, dir, repo, "local1")
	setTracking(t, repo, branch, "origin", remoteTip)

	st, err := gitaware.GetStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	u := st.Upstream
	if u == nil {
		t.Fatal("upstream should not be nil")
	}
	if u.Ahead != 1 || u.Behind != 1 || u.InSync {
		t.Errorf("expected ahead=1 behind=1, got ahead=%d behind=%d inSync=%v", u.Ahead, u.Behind, u.InSync)
	}
}

func TestUpstreamNoUpstream(t *testing.T) {
	dir := t.TempDir()
	repo := initRepo(t, dir)
	makeCommit(t, dir, repo, "base")
	// No setTracking call — branch has no upstream configured.

	st, err := gitaware.GetStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Upstream != nil {
		t.Errorf("expected nil upstream, got %+v", st.Upstream)
	}
}

func TestUpstreamUnborn(t *testing.T) {
	dir := t.TempDir()
	gogit.PlainInit(dir, false)
	// No commits → repo.Head() errors → Branch="" → upstream nil.

	st, err := gitaware.GetStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Branch != "" {
		t.Errorf("expected empty branch for unborn HEAD, got %q", st.Branch)
	}
	if st.Upstream != nil {
		t.Errorf("expected nil upstream for unborn HEAD, got %+v", st.Upstream)
	}
}

// TestUpstreamAheadMergeTopology is a regression test for the ancestor-exclusion
// bug: when M is a merge commit with parents [tracking, ancestor-of-tracking],
// the ancestor is reachable from the tracking ref, so ahead should be 1 (just M),
// not 2 (M + the ancestor counted again via the merge edge).
func TestUpstreamAheadMergeTopology(t *testing.T) {
	dir := t.TempDir()
	repo := initRepo(t, dir)
	c1 := makeCommit(t, dir, repo, "c1")
	branch := currentBranch(t, repo)
	c2 := makeCommit(t, dir, repo, "c2")
	setTracking(t, repo, branch, "origin", c2)

	// Create a merge commit M with parents [c2, c1].
	// c1 is reachable from c2, so it must not be counted as an extra "ahead" commit.
	wt, _ := repo.Worktree()
	if _, err := wt.Commit("merge", &gogit.CommitOptions{
		Author:            &object.Signature{Name: "t", Email: "t@e", When: time.Unix(0, 0)},
		Parents:           []plumbing.Hash{c2, c1},
		AllowEmptyCommits: true,
	}); err != nil {
		t.Fatal(err)
	}

	st, err := gitaware.GetStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	u := st.Upstream
	if u == nil {
		t.Fatal("upstream should not be nil")
	}
	if u.Ahead != 1 || u.Behind != 0 || u.InSync {
		t.Errorf("expected ahead=1 behind=0, got ahead=%d behind=%d inSync=%v", u.Ahead, u.Behind, u.InSync)
	}
}
