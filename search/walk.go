// Package search drives content (grep) and filename (find) search over a
// workspace, filtered by its policy and ignore set, with every leaf opened
// through the workspace's os.Root sandbox.
package search

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"

	"github.com/charlievieth/fastwalk"
	"github.com/mnehpets/workspace-mcp/fsroot"
	"github.com/mnehpets/workspace-mcp/grrep"
	"github.com/mnehpets/workspace-mcp/policy"
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
// that pass the policy and ignore filters, each with its size. The traversal
// mirrors grrep's walker: .git is always skipped, dotfiles/dirs are skipped,
// non-regular files (including symlinks) are never followed, and blocked or
// ignored directories are pruned.
//
// fastwalk invokes the callback concurrently across goroutines, so every touch
// of shared state — the results slice and the (non-thread-safe) ignore tree — is
// serialized under mu. The callback body is cheap; fastwalk's own stat/readdir
// work stays parallel.
func collectFiles(root *fsroot.Root, pol *policy.Policy, ig *grrep.IgnoreSet, startRel string) ([]fileMeta, error) {
	base := root.Dir()
	absStart := base
	if startRel != "." {
		absStart = filepath.Join(base, filepath.FromSlash(startRel))
	}

	var (
		mu    sync.Mutex
		files []fileMeta
	)
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
		name := d.Name()

		if d.IsDir() {
			if p == absStart {
				return nil
			}
			if name == ".git" {
				return fs.SkipDir
			}
			if strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			if !pol.CheckDir(rel).Allowed {
				return fs.SkipDir
			}
			if ig != nil {
				mu.Lock()
				ignored := ig.Match(rel, true)
				if !ignored {
					// Eager-build this dir's ignore node so child matches are cache hits.
					ig.EnsureNode(rel)
				}
				mu.Unlock()
				if ignored {
					return fs.SkipDir
				}
			}
			return nil
		}

		if strings.HasPrefix(name, ".") {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if !pol.CheckFile(rel).Allowed {
			return nil
		}
		if ig != nil {
			mu.Lock()
			ignored := ig.Match(rel, false)
			mu.Unlock()
			if ignored {
				return nil
			}
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
