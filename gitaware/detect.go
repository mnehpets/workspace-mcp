// Package gitaware adds read-only, git-aware operations to workspaces whose tree
// is a Git repository, using pure-Go go-git (no git binary). Most operations
// produce metadata only (status, tracked files). The exception is Diff, which
// emits file content as a unified diff: its blob bytes come from go-git's object
// store, and its worktree bytes are read through an injected WorktreeReader that
// the mcp layer backs with the workspace os.Root — gitaware never opens worktree
// files itself, keeping content reads inside the os.Root boundary.
package gitaware

import (
	"errors"

	gogit "github.com/go-git/go-git/v5"
)

// ErrNotGitRepo is returned by operations invoked on a non-git tree.
var ErrNotGitRepo = errors.New("not a git repository")

// Detect reports whether dir is the root of a Git repository.
func Detect(dir string) bool {
	_, err := gogit.PlainOpen(dir)
	return err == nil
}
