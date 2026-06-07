// Package search drives content (grep) and filename (find) search over a
// workspace, filtered by its policy and ignore set, with every leaf opened
// through the workspace's os.Root sandbox.
package search

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/charlievieth/fastwalk"
	"github.com/mnehpets/workspace-mcp/fsroot"
	"github.com/mnehpets/workspace-mcp/grrep"
	"github.com/mnehpets/workspace-mcp/policy"
)

// collectFiles walks the workspace tree starting at startRel (a clean
// workspace-relative slash path, "." for root) and returns the relative slash
// paths of regular files that pass the policy and ignore filters. The traversal
// mirrors grrep's walker: .git is always skipped, dotfiles/dirs are skipped,
// non-regular files (including symlinks) are never followed, and blocked or
// ignored directories are pruned.
func collectFiles(root *fsroot.Root, pol *policy.Policy, ig *grrep.IgnoreSet, startRel string) ([]string, error) {
	base := root.Dir()
	absStart := base
	if startRel != "." {
		absStart = filepath.Join(base, filepath.FromSlash(startRel))
	}

	var files []string
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
				if ig.Match(rel, true) {
					return fs.SkipDir
				}
				// Eager-build this dir's ignore node so child matches are cache hits.
				ig.EnsureNode(rel)
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
		if ig != nil && ig.Match(rel, false) {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if errors.Is(err, fs.SkipAll) {
		return files, nil
	}
	return files, err
}
