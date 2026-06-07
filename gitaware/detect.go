// Package gitaware adds read-only, git-aware operations to workspaces whose tree
// is a Git repository, using pure-Go go-git (no git binary). It produces
// metadata only (status, tracked files); it never serves file content, so it is
// outside the os.Root content boundary by design.
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
