package gitaware

import (
	"fmt"
	"sort"

	gogit "github.com/go-git/go-git/v5"
)

// FileStatus is one changed file's two-character porcelain code (staging then
// worktree), e.g. " M", "A ", "??".
type FileStatus struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// Status is the read-only status of a git workspace.
type Status struct {
	Branch   string        `json:"branch"`
	Files    []FileStatus  `json:"files"`
	Upstream *UpstreamInfo `json:"upstream,omitempty"` // nil when no tracking branch is configured
}

// GetStatus returns the current branch and per-file status. dir must be a git
// repository root (ErrNotGitRepo otherwise).
func GetStatus(dir string) (*Status, error) {
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		if err == gogit.ErrRepositoryNotExists {
			return nil, ErrNotGitRepo
		}
		return nil, err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, err
	}
	st, err := wt.Status()
	if err != nil {
		return nil, err
	}

	out := &Status{}
	if head, err := repo.Head(); err == nil {
		out.Branch = head.Name().Short()
		out.Upstream = resolveUpstream(repo, out.Branch)
	}
	for path, s := range st {
		out.Files = append(out.Files, FileStatus{
			Path:   path,
			Status: fmt.Sprintf("%c%c", s.Staging, s.Worktree),
		})
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	return out, nil
}
