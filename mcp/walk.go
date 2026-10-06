package mcp

import (
	"errors"
	"io/fs"
	"path/filepath"
	"sync"

	"github.com/charlievieth/fastwalk"
)

// fileMeta is one regular file discovered by the walk: its workspace-relative
// slash path and size in bytes (captured from the dir entry so enumeration needs
// no extra stat).
type fileMeta struct {
	Path string
	Size int64
}

// collectFiles walks the workspace tree starting at startRel (a clean
// workspace-relative slash path, "." for root) and returns the regular files
// that the Root lists, each with its size. Which paths are listed is decided
// entirely by Root.Lister (access rules, dotfiles, .git, gitignore); this walker
// has no filtering rules of its own. Non-regular files (including symlinks) are
// never followed, and unlisted directories are pruned.
//
// fastwalk invokes the callback concurrently across goroutines; the Lister is
// safe for concurrent use and the results slice is guarded by mu. fastwalk's own
// stat/readdir work stays parallel.
func collectFiles(root *Root, startRel string) ([]fileMeta, error) {
	base := root.Dir()
	absStart := base
	if startRel != "." {
		absStart = filepath.Join(base, filepath.FromSlash(startRel))
	}

	var (
		mu    sync.Mutex
		files []fileMeta
	)
	lister := root.Lister()
	cfg := &fastwalk.Config{}
	err := fastwalk.Walk(cfg, absStart, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(base, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if p == absStart {
				return nil
			}
			if !lister.Listed(rel, true) {
				return fs.SkipDir
			}
			return nil
		}

		if !d.Type().IsRegular() {
			return nil
		}
		if !lister.Listed(rel, false) {
			return nil
		}
		var size int64
		if info, ierr := d.Info(); ierr == nil {
			size = info.Size()
		}
		mu.Lock()
		files = append(files, fileMeta{Path: rel, Size: size})
		mu.Unlock()
		return nil
	})
	if errors.Is(err, fs.SkipAll) {
		return files, nil
	}
	return files, err
}
