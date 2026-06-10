package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/mnehpets/workspace-mcp/mcp"
)

// diffResp decodes either a git_diff envelope or an in-band toolError (Code /
// Reason) — callTool decodes content[0].text into here regardless of IsError.
type diffResp struct {
	Files []struct {
		Path      string `json:"path"`
		Change    string `json:"change"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
		Binary    bool   `json:"binary"`
		TooLarge  bool   `json:"tooLarge"`
		Symlink   bool   `json:"symlink"`
	} `json:"files"`
	Diff      string `json:"diff"`
	Truncated bool   `json:"truncated"`
	Notice    string `json:"notice"`
	Code      string `json:"code"`
	Reason    string `json:"reason"`
}

func (r diffResp) file(path string) (int, bool) {
	for i, f := range r.Files {
		if f.Path == path {
			return i, true
		}
	}
	return 0, false
}

// gitDiffEnv is a git-repo workspace plus handles to drive its worktree.
type gitDiffEnv struct {
	f    *mcpFixture
	dir  string
	repo *gogit.Repository
	wt   *gogit.Worktree
}

func (e *gitDiffEnv) write(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(e.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *gitDiffEnv) writeBytes(t *testing.T, rel string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, filepath.FromSlash(rel)), content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *gitDiffEnv) rm(t *testing.T, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(e.dir, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

func (e *gitDiffEnv) add(t *testing.T, rel string) {
	t.Helper()
	if _, err := e.wt.Add(rel); err != nil {
		t.Fatal(err)
	}
}

func (e *gitDiffEnv) commit(t *testing.T, msg string) {
	t.Helper()
	_, err := e.wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@e", When: time.Unix(0, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *gitDiffEnv) diff(t *testing.T, args map[string]any) (diffResp, bool) {
	t.Helper()
	var r diffResp
	res := e.f.callTool(t, "git_diff", args, &r)
	return r, res.IsError
}

// newGitDiffEnv inits a repo at a temp dir, applies an optional initial commit
// via seed, and wires a "default" workspace over it with the given read cap and
// policy globs (allow defaults to "**/*").
func newGitDiffEnv(t *testing.T, maxBytes int64, allow, block []string, seed func(e *gitDiffEnv)) *gitDiffEnv {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	e := &gitDiffEnv{dir: dir, repo: repo, wt: wt}
	if seed != nil {
		seed(e)
	}
	if len(allow) == 0 {
		allow = []string{"**/*"}
	}
	cfg := &mcp.Config{
		Workspaces: []mcp.WorkspaceConfig{{
			Name: "default", Root: dir,
			Policy: mcp.PolicyConfig{AllowGlobs: allow, BlockGlobs: block},
			Read:   mcp.ReadConfig{MaxBytes: maxBytes},
			Grep:   mcp.GrepConfig{Enabled: true, MaxMatches: 500},
		}},
	}
	reg, err := mcp.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	e.f = newMCPFixture(t, reg)
	return e
}

const bigRead = 1 << 20

// committed builds a repo with an initial commit of a.txt and dir/b.txt.
func committed(e *gitDiffEnv) {
	// seed runs against the worktree via os ops then add/commit.
	p := func(rel, content string) {
		full := filepath.Join(e.dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o600)
		e.wt.Add(rel)
	}
	p("a.txt", "line1\nline2\nline3\n")
	p("dir/b.txt", "hello\n")
	e.wt.Commit("init", &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@e", When: time.Unix(0, 0)},
	})
}

// Row 1: clean repo, no args → empty success.
func TestGitDiffClean(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, committed)
	r, isErr := e.diff(t, map[string]any{})
	if isErr {
		t.Fatalf("unexpected error: %+v", r)
	}
	if len(r.Files) != 0 || r.Diff != "" || r.Truncated {
		t.Fatalf("expected empty diff, got %+v", r)
	}
	if r.Notice != "no changes" {
		t.Errorf("notice: got %q want \"no changes\"", r.Notice)
	}
}

// Row 2: modified tracked file, unstaged.
func TestGitDiffModifiedUnstaged(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, committed)
	e.write(t, "a.txt", "line1\nCHANGED\nline3\n")
	r, isErr := e.diff(t, map[string]any{})
	if isErr {
		t.Fatalf("unexpected error: %+v", r)
	}
	i, ok := r.file("a.txt")
	if !ok {
		t.Fatalf("a.txt absent: %+v", r)
	}
	if r.Files[i].Change != "modified" {
		t.Errorf("change: got %q", r.Files[i].Change)
	}
	if r.Files[i].Additions != 1 || r.Files[i].Deletions != 1 {
		t.Errorf("counts: got +%d-%d", r.Files[i].Additions, r.Files[i].Deletions)
	}
	if !strings.Contains(r.Diff, "diff --git a/a.txt b/a.txt") ||
		!strings.Contains(r.Diff, "+CHANGED") || !strings.Contains(r.Diff, "-line2") {
		t.Errorf("diff text missing edit:\n%s", r.Diff)
	}
}

// Row 3: new untracked file → all-additions new-file diff.
func TestGitDiffUntracked(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, committed)
	e.write(t, "new.txt", "fresh\ncontent\n")
	r, _ := e.diff(t, map[string]any{})
	i, ok := r.file("new.txt")
	if !ok {
		t.Fatalf("new.txt absent: %+v", r)
	}
	if r.Files[i].Change != "untracked" || r.Files[i].Additions != 2 {
		t.Errorf("got %+v", r.Files[i])
	}
	if !strings.Contains(r.Diff, "new file mode") || !strings.Contains(r.Diff, "--- /dev/null") {
		t.Errorf("not a new-file diff:\n%s", r.Diff)
	}
}

// Row 4: deleted tracked file (unstaged) → all-deletions.
func TestGitDiffDeletedUnstaged(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, committed)
	e.rm(t, "a.txt")
	r, _ := e.diff(t, map[string]any{})
	i, ok := r.file("a.txt")
	if !ok {
		t.Fatalf("a.txt absent: %+v", r)
	}
	if r.Files[i].Change != "deleted" || r.Files[i].Deletions != 3 {
		t.Errorf("got %+v", r.Files[i])
	}
	if !strings.Contains(r.Diff, "deleted file mode") || !strings.Contains(r.Diff, "+++ /dev/null") {
		t.Errorf("not a delete diff:\n%s", r.Diff)
	}
}

// Row 5: staged add/modify/delete with staged:true.
func TestGitDiffStaged(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, committed)
	e.write(t, "a.txt", "line1\nline2\nMOD\n") // modify+stage
	e.add(t, "a.txt")
	e.write(t, "added.txt", "brand new\n") // add+stage
	e.add(t, "added.txt")
	e.rm(t, "dir/b.txt") // delete+stage
	e.add(t, "dir/b.txt")

	r, isErr := e.diff(t, map[string]any{"staged": true})
	if isErr {
		t.Fatalf("unexpected error: %+v", r)
	}
	want := map[string]string{"a.txt": "modified", "added.txt": "added", "dir/b.txt": "deleted"}
	for path, change := range want {
		i, ok := r.file(path)
		if !ok {
			t.Fatalf("%s absent: %+v", path, r)
		}
		if r.Files[i].Change != change {
			t.Errorf("%s: change got %q want %q", path, r.Files[i].Change, change)
		}
	}
}

// Row 6: same file part-staged → staged and unstaged calls return different content.
func TestGitDiffStagedVsUnstaged(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, committed)
	e.write(t, "a.txt", "line1\nSTAGED\nline3\n")
	e.add(t, "a.txt")
	e.write(t, "a.txt", "line1\nWORKTREE\nline3\n")

	staged, _ := e.diff(t, map[string]any{"staged": true})
	unstaged, _ := e.diff(t, map[string]any{})
	if !strings.Contains(staged.Diff, "+STAGED") {
		t.Errorf("staged diff missing STAGED:\n%s", staged.Diff)
	}
	if !strings.Contains(unstaged.Diff, "+WORKTREE") {
		t.Errorf("unstaged diff missing WORKTREE:\n%s", unstaged.Diff)
	}
	if staged.Diff == unstaged.Diff {
		t.Error("staged and unstaged diffs should differ")
	}
}

// Row 7: path = changed file → only that file.
func TestGitDiffScopedFile(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, committed)
	e.write(t, "a.txt", "line1\nX\nline3\n")
	e.write(t, "dir/b.txt", "changed\n")
	r, _ := e.diff(t, map[string]any{"path": "a.txt"})
	if len(r.Files) != 1 || r.Files[0].Path != "a.txt" {
		t.Fatalf("expected only a.txt, got %+v", r.Files)
	}
}

// Row 8: path = directory prefix; "dir" must not match "dirx".
func TestGitDiffScopedDir(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, committed)
	e.write(t, "dir/b.txt", "changed\n")
	e.write(t, "dirx/c.txt", "sibling\n") // untracked under a sibling prefix
	e.write(t, "a.txt", "line1\nX\nline3\n")
	r, _ := e.diff(t, map[string]any{"path": "dir"})
	if len(r.Files) != 1 || r.Files[0].Path != "dir/b.txt" {
		t.Fatalf("dir scope should match only dir/b.txt, got %+v", r.Files)
	}
}

// Row 9: path = unchanged-but-existing file → empty success.
func TestGitDiffScopedUnchanged(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, committed)
	e.write(t, "a.txt", "line1\nX\nline3\n") // change a different file
	r, isErr := e.diff(t, map[string]any{"path": "dir/b.txt"})
	if isErr {
		t.Fatalf("unexpected error: %+v", r)
	}
	if len(r.Files) != 0 || r.Notice != "no changes" {
		t.Fatalf("expected no-changes success, got %+v", r)
	}
}

// Row 10: path = nonexistent everywhere → NOT_FOUND.
func TestGitDiffScopedNotFound(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, committed)
	r, isErr := e.diff(t, map[string]any{"path": "nope/missing.txt"})
	if !isErr || r.Code != "NOT_FOUND" {
		t.Fatalf("expected NOT_FOUND, got isErr=%v %+v", isErr, r)
	}
}

// Row 11: path = policy-blocked → POLICY_DENIED even if dirty.
func TestGitDiffScopedBlocked(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, []string{"**/secret.txt"}, committed)
	e.write(t, "secret.txt", "leak\n") // dirty + blocked
	r, isErr := e.diff(t, map[string]any{"path": "secret.txt"})
	if !isErr || r.Code != "POLICY_DENIED" || r.Reason != "blocked_glob" {
		t.Fatalf("expected POLICY_DENIED/blocked_glob, got isErr=%v %+v", isErr, r)
	}
}

// Row 12: unscoped diff hides a dirty blocked file (security).
func TestGitDiffUnscopedHidesBlocked(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, []string{"**/secret.txt"}, committed)
	e.write(t, "secret.txt", "leak\n")
	e.write(t, "a.txt", "line1\nX\nline3\n")
	r, _ := e.diff(t, map[string]any{})
	if _, ok := r.file("secret.txt"); ok {
		t.Errorf("blocked secret.txt leaked into files: %+v", r.Files)
	}
	if strings.Contains(r.Diff, "secret.txt") || strings.Contains(r.Diff, "leak") {
		t.Errorf("blocked content leaked into diff:\n%s", r.Diff)
	}
}

// Row 13: path = ../x or /abs → POLICY_DENIED via mapPathError.
func TestGitDiffUnsafePaths(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, committed)
	for _, tc := range []struct{ path, reason string }{
		{"../escape.txt", "traversal"},
		{"/etc/passwd", "absolute_path"},
	} {
		r, isErr := e.diff(t, map[string]any{"path": tc.path})
		if !isErr || r.Code != "POLICY_DENIED" || r.Reason != tc.reason {
			t.Errorf("%s: want POLICY_DENIED/%s, got isErr=%v %+v", tc.path, tc.reason, isErr, r)
		}
	}
}

// Row 14: non-git workspace → NOT_A_GIT_REPO.
func TestGitDiffNonGit(t *testing.T) {
	reg, _, _ := twoWorkspaceRegistry(t) // plain dirs, not repos
	f := newMCPFixture(t, reg)
	var r diffResp
	res := f.callTool(t, "git_diff", map[string]any{}, &r)
	if !res.IsError || r.Code != "NOT_A_GIT_REPO" {
		t.Fatalf("expected NOT_A_GIT_REPO, got isErr=%v %+v", res.IsError, r)
	}
}

// Row 15: binary file modified → listed binary, marker line, no content.
func TestGitDiffBinary(t *testing.T) {
	bin := []byte{'P', 'N', 'G', 0x00, 0x01, 0x02, 0x00, 'x'}
	e := newGitDiffEnv(t, bigRead, nil, nil, committed)
	// commit the binary so a later change is a modify.
	e.writeBytes(t, "img.bin", bin)
	e.add(t, "img.bin")
	e.commit(t, "add bin")
	e.writeBytes(t, "img.bin", append(bin, 0x00, 'y'))

	r, _ := e.diff(t, map[string]any{})
	i, ok := r.file("img.bin")
	if !ok {
		t.Fatalf("img.bin absent: %+v", r)
	}
	if !r.Files[i].Binary {
		t.Errorf("expected binary:true, got %+v", r.Files[i])
	}
	if !strings.Contains(r.Diff, "Binary files a/img.bin and b/img.bin differ") {
		t.Errorf("missing binary marker:\n%s", r.Diff)
	}
}

// Row 16: file > read.maxBytes modified → tooLarge, marker, no content.
func TestGitDiffTooLarge(t *testing.T) {
	e := newGitDiffEnv(t, 16, nil, nil, committed) // tiny read cap
	e.write(t, "a.txt", strings.Repeat("x", 100)+"\n")
	r, _ := e.diff(t, map[string]any{})
	i, ok := r.file("a.txt")
	if !ok {
		t.Fatalf("a.txt absent: %+v", r)
	}
	if !r.Files[i].TooLarge {
		t.Errorf("expected tooLarge:true, got %+v", r.Files[i])
	}
	if !strings.Contains(r.Diff, "# diff of a.txt skipped: file exceeds read limit") {
		t.Errorf("missing tooLarge marker:\n%s", r.Diff)
	}
}

// Row 17: symlink in worktree → skipped + flagged, target never read.
func TestGitDiffSymlink(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, committed)
	if err := os.Symlink("a.txt", filepath.Join(e.dir, "link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	r, _ := e.diff(t, map[string]any{})
	i, ok := r.file("link")
	if !ok {
		t.Fatalf("link absent: %+v", r)
	}
	if !r.Files[i].Symlink {
		t.Errorf("expected symlink:true, got %+v", r.Files[i])
	}
	if !strings.Contains(r.Diff, "# diff of link skipped: symlink") {
		t.Errorf("missing symlink marker:\n%s", r.Diff)
	}
}

// Row 18: changes exceeding the total cap → truncated, steering notice, files complete.
func TestGitDiffTruncated(t *testing.T) {
	// Small total cap; several modified files each under the per-file cap but
	// together over the total.
	e := newGitDiffEnv(t, 120, nil, nil, func(e *gitDiffEnv) {
		for _, n := range []string{"f1.txt", "f2.txt", "f3.txt", "f4.txt"} {
			full := filepath.Join(e.dir, n)
			os.WriteFile(full, []byte("orig\n"), 0o600)
			e.wt.Add(n)
		}
		e.wt.Commit("init", &gogit.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@e", When: time.Unix(0, 0)}})
	})
	for _, n := range []string{"f1.txt", "f2.txt", "f3.txt", "f4.txt"} {
		e.write(t, n, "changed\n")
	}
	r, _ := e.diff(t, map[string]any{})
	if !r.Truncated {
		t.Fatalf("expected truncated, got %+v files, diff len %d", len(r.Files), len(r.Diff))
	}
	if len(r.Files) != 4 {
		t.Errorf("files list must be complete (4), got %d", len(r.Files))
	}
	if !strings.Contains(r.Notice, "truncated") {
		t.Errorf("missing steering notice: %q", r.Notice)
	}
}

// Row 19: unborn-HEAD repo, staged:true → all-additions vs empty tree, no panic.
func TestGitDiffUnbornHeadStaged(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, nil) // no initial commit
	e.write(t, "first.txt", "hello\nworld\n")
	e.add(t, "first.txt")
	r, isErr := e.diff(t, map[string]any{"staged": true})
	if isErr {
		t.Fatalf("unexpected error: %+v", r)
	}
	i, ok := r.file("first.txt")
	if !ok {
		t.Fatalf("first.txt absent: %+v", r)
	}
	if r.Files[i].Change != "added" || r.Files[i].Additions != 2 {
		t.Errorf("got %+v", r.Files[i])
	}
}

// Row 20: CRLF-only change is shown (no normalization).
func TestGitDiffCRLF(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, func(e *gitDiffEnv) {
		os.WriteFile(filepath.Join(e.dir, "crlf.txt"), []byte("x\ny\n"), 0o600)
		e.wt.Add("crlf.txt")
		e.wt.Commit("init", &gogit.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@e", When: time.Unix(0, 0)}})
	})
	e.write(t, "crlf.txt", "x\r\ny\n")
	r, _ := e.diff(t, map[string]any{})
	if !strings.Contains(r.Diff, "\r") {
		t.Errorf("CRLF change not reflected in diff:\n%q", r.Diff)
	}
}

// Row 21: unified-format fidelity — no-newline-at-EOF handling.
func TestGitDiffNoNewlineAtEOF(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, func(e *gitDiffEnv) {
		os.WriteFile(filepath.Join(e.dir, "n.txt"), []byte("a\nb"), 0o600) // no trailing newline
		e.wt.Add("n.txt")
		e.wt.Commit("init", &gogit.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@e", When: time.Unix(0, 0)}})
	})
	e.write(t, "n.txt", "a\nc") // still no trailing newline
	r, _ := e.diff(t, map[string]any{})
	if !strings.Contains(r.Diff, "\\ No newline at end of file") {
		t.Errorf("missing no-newline marker:\n%s", r.Diff)
	}
	if !strings.Contains(r.Diff, "@@") {
		t.Errorf("missing hunk header:\n%s", r.Diff)
	}
}

// Empty-file edges: a zero-byte side must never be mislabeled binary nor emit a
// stray "Binary files … differ" header (regression for filePatch.IsBinary).
func TestGitDiffEmptyFiles(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, committed)
	e.write(t, "empty.txt", "") // new zero-byte untracked file
	e.write(t, "a.txt", "")     // truncate a tracked file to empty
	r, isErr := e.diff(t, map[string]any{})
	if isErr {
		t.Fatalf("unexpected error: %+v", r)
	}
	for _, f := range r.Files {
		if f.Binary {
			t.Errorf("%s wrongly flagged binary: %+v", f.Path, f)
		}
	}
	if strings.Contains(r.Diff, "Binary files") {
		t.Errorf("empty-file diff emitted a stray binary header:\n%s", r.Diff)
	}
	// The new empty file is still reported as untracked with a new-file header.
	if i, ok := r.file("empty.txt"); !ok || r.Files[i].Change != "untracked" {
		t.Errorf("empty.txt: want untracked, got %+v", r.Files)
	}
	if !strings.Contains(r.Diff, "diff --git a/empty.txt b/empty.txt") {
		t.Errorf("missing new-file header for empty.txt:\n%s", r.Diff)
	}
}

// Row 22: tool list advertises git_diff with readOnlyHint on a git workspace.
func TestGitDiffToolListed(t *testing.T) {
	e := newGitDiffEnv(t, bigRead, nil, nil, committed)
	rr := e.f.call(t, "tools/list", map[string]any{})
	if rr.Error != nil {
		t.Fatalf("tools/list error: %+v", rr.Error)
	}
	var res struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
			Annotations map[string]any `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(rr.Result, &res); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range res.Tools {
		if tool.Name != "git_diff" {
			continue
		}
		found = true
		if tool.Annotations["readOnlyHint"] != true {
			t.Errorf("git_diff missing readOnlyHint=true: %+v", tool.Annotations)
		}
		if tool.Annotations["openWorldHint"] != false {
			t.Errorf("git_diff missing openWorldHint=false: %+v", tool.Annotations)
		}
		if tool.InputSchema == nil {
			t.Errorf("git_diff missing input schema")
		}
	}
	if !found {
		t.Fatalf("git_diff not advertised: %s", rr.Result)
	}
}
