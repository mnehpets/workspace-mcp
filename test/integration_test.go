package test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/mnehpets/workspace-mcp/config"
	"github.com/mnehpets/workspace-mcp/workspace"
)

// integrationRegistry builds a git "default" workspace and a non-git "notes"
// workspace with sample files, a blocked .env, and an outward symlink.
func integrationRegistry(t *testing.T) *workspace.Registry {
	t.Helper()
	gitDir := t.TempDir()
	notesDir := t.TempDir()

	write := func(base, rel, content string) {
		p := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// --- git workspace ---
	repo, err := gogit.PlainInit(gitDir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	write(gitDir, "README.md", "# Project\nintro\n")
	write(gitDir, "docs/guide.md", "Use the ASC workflow.\n")
	write(gitDir, "src/main.go", "package main\nfunc main() {}\n")
	if _, err := wt.Add("."); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("init", &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@e", When: time.Unix(0, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	// Post-commit changes for git_status.
	write(gitDir, "README.md", "# Project\nintro\nmore\n")
	// Sensitive + escape files (created after commit; .env is gitignored-style blocked).
	write(gitDir, ".env", "SECRET=should-never-be-served\n")
	// An allowed-looking name that is actually an outward symlink.
	_ = os.Symlink("/etc/passwd", filepath.Join(gitDir, "escape.md"))

	// --- notes workspace (not git) ---
	write(notesDir, "todo.md", "buy milk\n")

	cfg := &config.Config{
		Workspaces: []config.WorkspaceConfig{
			{
				Name: "default", Root: gitDir, RespectGitignore: true,
				Policy: config.PolicyConfig{
					AllowGlobs: []string{"**/*.md", "**/*.go", "README*"},
					BlockGlobs: []string{".git/**", "**/.env", "**/.env.*"},
				},
				Read: config.ReadConfig{MaxBytes: 100000},
				Grep: config.GrepConfig{Enabled: true, MaxMatches: 500},
			},
			{
				Name: "notes", Root: notesDir, RespectGitignore: false,
				Policy: config.PolicyConfig{AllowGlobs: []string{"**/*.md"}},
				Read:   config.ReadConfig{MaxBytes: 100000},
				Grep:   config.GrepConfig{Enabled: false},
			},
		},
	}
	reg, err := workspace.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	return reg
}

func assertToolError(t *testing.T, tres toolResult, wantCode string) {
	t.Helper()
	if !tres.IsError {
		t.Fatalf("expected isError result, got success: %+v", tres)
	}
	var te struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
	}
	if len(tres.Content) == 0 {
		t.Fatal("error result has no content")
	}
	if err := json.Unmarshal([]byte(tres.Content[0].Text), &te); err != nil {
		t.Fatalf("decode tool error: %v", err)
	}
	if te.Code != wantCode {
		t.Fatalf("want error code %q, got %q (reason %q)", wantCode, te.Code, te.Reason)
	}
}

func TestIntegrationReadFlow(t *testing.T) {
	reg := integrationRegistry(t)
	f := newMCPFixture(t, reg)

	// Handshake.
	if rr := f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"}); rr.Error != nil {
		t.Fatalf("initialize: %+v", rr.Error)
	}
	if rr := f.call(t, "tools/list", map[string]any{}); rr.Error != nil {
		t.Fatalf("tools/list: %+v", rr.Error)
	}

	// workspace_list.
	var wl struct {
		Workspaces []struct {
			Name      string `json:"name"`
			IsGitRepo bool   `json:"isGitRepo"`
		} `json:"workspaces"`
	}
	f.callTool(t, "workspace_list", map[string]any{}, &wl)
	if len(wl.Workspaces) != 2 {
		t.Fatalf("expected 2 workspaces, got %+v", wl.Workspaces)
	}
	gitFound := false
	for _, w := range wl.Workspaces {
		if w.Name == "default" && w.IsGitRepo {
			gitFound = true
		}
		if w.Name == "notes" && w.IsGitRepo {
			t.Error("notes should not be a git repo")
		}
	}
	if !gitFound {
		t.Error("default should be a git repo")
	}

	// tree_list root: README.md visible; .env and .git hidden.
	var tl struct {
		Entries []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"entries"`
	}
	f.callTool(t, "tree_list", map[string]any{"workspace": "default"}, &tl)
	names := map[string]string{}
	for _, e := range tl.Entries {
		names[e.Path] = e.Type
	}
	if names["README.md"] != "file" {
		t.Errorf("README.md should be listed as file, entries=%+v", tl.Entries)
	}
	if _, ok := names[".env"]; ok {
		t.Error(".env must not be listed")
	}
	if _, ok := names[".git"]; ok {
		t.Error(".git must not be listed")
	}

	// file_read allowed.
	var fr struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
		Binary    bool   `json:"binary"`
		Notice    string `json:"notice"`
	}
	f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "README.md"}, &fr)
	if !strings.Contains(fr.Content, "# Project") {
		t.Errorf("README content wrong: %q", fr.Content)
	}

	// file_read truncated by maxBytes carries a steering notice (not just a flag).
	var frt struct {
		Truncated bool   `json:"truncated"`
		Notice    string `json:"notice"`
	}
	f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "README.md", "maxBytes": 1}, &frt)
	if !frt.Truncated {
		t.Error("expected truncated=true with maxBytes=1")
	}
	if frt.Notice == "" {
		t.Error("expected a steering notice on a truncated file_read")
	}

	// file_read blocked .env -> POLICY_DENIED.
	assertToolError(t, f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": ".env"}, nil), "POLICY_DENIED")

	// file_read symlink escape (allowed name, outward target) -> POLICY_DENIED.
	assertToolError(t, f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "escape.md"}, nil), "POLICY_DENIED")

	// file_read traversal -> POLICY_DENIED.
	assertToolError(t, f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "../../etc/passwd"}, nil), "POLICY_DENIED")

	// tree_grep.
	var gr struct {
		Matches []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Text string `json:"text"`
		} `json:"matches"`
		Truncated bool `json:"truncated"`
	}
	f.callTool(t, "tree_grep", map[string]any{"workspace": "default", "pattern": "ASC workflow"}, &gr)
	if len(gr.Matches) != 1 || gr.Matches[0].Path != "docs/guide.md" {
		t.Errorf("grep result unexpected: %+v", gr.Matches)
	}

	// tree_grep disabled on notes -> GREP_DISABLED.
	assertToolError(t, f.callTool(t, "tree_grep", map[string]any{"workspace": "notes", "pattern": "milk"}, nil), "GREP_DISABLED")

	// tree_find.
	var fd struct {
		Files []string `json:"files"`
	}
	f.callTool(t, "tree_find", map[string]any{"workspace": "default", "query": "guide"}, &fd)
	if len(fd.Files) == 0 || fd.Files[0] != "docs/guide.md" {
		t.Errorf("find unexpected: %+v", fd.Files)
	}

	// git_status on git workspace.
	var gs struct {
		Branch string `json:"branch"`
		Files  []struct {
			Path   string `json:"path"`
			Status string `json:"status"`
		} `json:"files"`
	}
	f.callTool(t, "git_status", map[string]any{"workspace": "default"}, &gs)
	if gs.Branch == "" {
		t.Error("expected a branch")
	}
	foundMod := false
	for _, fst := range gs.Files {
		if fst.Path == "README.md" && strings.Contains(fst.Status, "M") {
			foundMod = true
		}
	}
	if !foundMod {
		t.Errorf("expected README.md modified in status: %+v", gs.Files)
	}

	// git_status on non-git workspace -> NOT_A_GIT_REPO.
	assertToolError(t, f.callTool(t, "git_status", map[string]any{"workspace": "notes"}, nil), "NOT_A_GIT_REPO")

	// Unknown workspace -> UNKNOWN_WORKSPACE.
	assertToolError(t, f.callTool(t, "file_read", map[string]any{"workspace": "ghost", "path": "x.md"}, nil), "UNKNOWN_WORKSPACE")

	// Audit log recorded tool calls.
	if !strings.Contains(f.logs.String(), "tool_call") {
		t.Error("expected tool_call entries in audit log")
	}
}

func TestUnauthenticatedRejectedOverHTTP(t *testing.T) {
	reg := integrationRegistry(t)
	f := newMCPFixture(t, reg)

	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	req, _ := http.NewRequest("POST", f.url, body)
	req.Header.Set("Content-Type", "application/json")
	// No Authorization header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", resp.StatusCode)
	}
}
