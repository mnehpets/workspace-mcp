package gitaware

import (
	"sort"

	gogit "github.com/go-git/go-git/v5"
)

// TrackedFiles returns the workspace-relative paths of all files in the git
// index (tracked files). dir must be a git repository root.
func TrackedFiles(dir string) ([]string, error) {
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		if err == gogit.ErrRepositoryNotExists {
			return nil, ErrNotGitRepo
		}
		return nil, err
	}
	idx, err := repo.Storer.Index()
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(idx.Entries))
	for _, e := range idx.Entries {
		files = append(files, e.Name)
	}
	sort.Strings(files)
	return files, nil
}
