// Root wraps os.Root to give every workspace a hard, symlink-safe,
// TOCTOU-safe filesystem boundary. All model-supplied paths cross the boundary
// here: they are workspace-relative, slash-separated, and resolved through the
// underlying *os.Root, which guarantees the result stays within the root even in
// the presence of symlinks.
//
// Root is also the single place the workspace's access rules (Policy) and
// visibility rules (.gitignore) are applied, so callers cannot forget them:
// Open, Stat, Lstat and the write methods refuse paths the policy denies, and
// ReadDir, WalkDir and Lister leave out paths that are not listed (denied by
// policy, or ignored). See docs/design.md §2.
package mcp

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Sentinel errors for unsafe model-supplied paths. They are rejected before the
// path ever reaches os.Root, as defense-in-depth on top of the OS boundary.
var (
	ErrAbsolutePath = errors.New("absolute path not allowed")
	ErrTraversal    = errors.New("path traversal (\"..\") not allowed")
	ErrReadOnly     = errors.New("workspace is read-only")
)

// DeniedError is returned when a path is refused by the workspace's access
// rules. Reason is one of "blocked_glob", "dotfile" or "not_allowlisted".
type DeniedError struct{ Reason string }

func (e *DeniedError) Error() string { return "denied by policy: " + e.Reason }

// Root is a sandboxed view of one workspace directory tree.
type Root struct {
	root   *os.Root
	dir    string
	policy *Policy // nil: every path is accessible
}

// RootOption configures the rules a Root enforces.
type RootOption func(*Root)

// WithPolicy makes the Root enforce p's access rules.
func WithPolicy(p *Policy) RootOption { return func(r *Root) { r.policy = p } }

// Open opens dir as an os.Root sandbox. Without options it enforces only
// containment and is read-only.
func Open(dir string, opts ...RootOption) (*Root, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	rt := &Root{root: r, dir: dir}
	for _, o := range opts {
		o(rt)
	}
	return rt, nil
}

// Check applies the workspace policy's access rules to a clean workspace-relative
// path; see Policy.Check.
func (r *Root) Check(rel string, isDir bool) Decision { return r.policy.Check(rel, isDir) }

// Lister returns a new Lister for one enumeration; see Policy.Lister.
func (r *Root) Lister() *Lister { return r.policy.Lister(r.dir) }

// Writable reports whether the workspace's policy permits writes. Without a
// policy a Root is read-only.
func (r *Root) Writable() bool { return r.policy != nil && r.policy.CanWrite() }

func (r *Root) require(rel string, isDir bool) error {
	if d := r.Check(rel, isDir); !d.Allowed {
		return &DeniedError{Reason: d.Reason}
	}
	return nil
}

// Close releases the underlying root.
func (r *Root) Close() error { return r.root.Close() }

// Dir returns the absolute root directory (for diagnostics, not exposed to the model).
func (r *Root) Dir() string { return r.dir }

// OS exposes the underlying *os.Root so trusted readers (e.g. the grep walker)
// can open leaves through the same boundary.
func (r *Root) OS() *os.Root { return r.root }

// Clean validates and normalizes a model-supplied workspace-relative path. It
// rejects absolute paths and any ".." segment, and returns a clean slash path
// with "." denoting the root. The returned path satisfies fs.ValidPath.
func Clean(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." {
		return ".", nil
	}
	if path.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return "", ErrAbsolutePath
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", ErrTraversal
		}
	}
	cleaned := path.Clean(rel)
	// path.Clean can still yield a leading ".." if the input dodged the split
	// check via odd encodings; reject defensively.
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", ErrTraversal
	}
	if !fs.ValidPath(cleaned) {
		return "", ErrTraversal
	}
	return cleaned, nil
}

// Open opens a file for reading through the sandbox.
func (r *Root) Open(rel string) (*os.File, error) {
	clean, err := Clean(rel)
	if err != nil {
		return nil, err
	}
	if err := r.require(clean, false); err != nil {
		return nil, err
	}
	return r.root.Open(filepath.FromSlash(clean))
}

// Stat stats a path through the sandbox (following symlinks, but never escaping).
func (r *Root) Stat(rel string) (os.FileInfo, error) {
	return r.statChecked(rel, r.root.Stat)
}

// statChecked stats rel and applies the access rules. The rules that hold for
// files and directories alike (block, dotfile) are checked before touching the
// filesystem, and a missing path is checked as a file, so a denied path is
// reported as denied whether or not it exists.
func (r *Root) statChecked(rel string, stat func(string) (os.FileInfo, error)) (os.FileInfo, error) {
	clean, err := Clean(rel)
	if err != nil {
		return nil, err
	}
	if err := r.require(clean, true); err != nil {
		return nil, err
	}
	info, err := stat(filepath.FromSlash(clean))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if derr := r.require(clean, false); derr != nil {
				return nil, derr
			}
		}
		return nil, err
	}
	if !info.IsDir() {
		if err := r.require(clean, false); err != nil {
			return nil, err
		}
	}
	return info, nil
}

// Lstat stats a path through the sandbox without following a terminal symlink,
// so a symlink is reported as one (ModeSymlink) rather than resolved. Used by
// git_diff to skip symlinks instead of diffing their targets.
func (r *Root) Lstat(rel string) (os.FileInfo, error) {
	return r.statChecked(rel, r.root.Lstat)
}

// ReadDir lists directory entries through the sandbox, leaving out entries that
// are not listed (denied by policy or gitignored).
func (r *Root) ReadDir(rel string) ([]fs.DirEntry, error) {
	clean, err := Clean(rel)
	if err != nil {
		return nil, err
	}
	if err := r.require(clean, true); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(r.root.FS(), clean)
	if err != nil {
		return nil, err
	}
	l := r.Lister()
	kept := entries[:0]
	for _, e := range entries {
		if l.Listed(path.Join(clean, e.Name()), e.IsDir()) {
			kept = append(kept, e)
		}
	}
	return kept, nil
}

// CreateNew creates a new file for writing through the sandbox, failing if the
// path already exists (O_CREATE|O_EXCL|O_WRONLY). Missing parent directories are
// created (MkdirAll, inside the root). The caller owns Close. This is the only
// path that creates a file, so a collision is reported (os.ErrExist) rather than
// silently clobbered.
func (r *Root) CreateNew(rel string) (*os.File, error) {
	if !r.Writable() {
		return nil, ErrReadOnly
	}
	clean, err := Clean(rel)
	if err != nil {
		return nil, err
	}
	if err := r.require(clean, false); err != nil {
		return nil, err
	}
	if dir := path.Dir(clean); dir != "." {
		if err := r.root.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
			return nil, err
		}
	}
	return r.root.OpenFile(filepath.FromSlash(clean), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
}

// WriteExisting opens an existing file for a truncating write through the
// sandbox (O_TRUNC|O_WRONLY, deliberately no O_CREATE). A missing path fails with
// os.ErrNotExist rather than creating a stray file — so a typo'd overwrite is a
// NOT_FOUND, not a silent new file. The caller owns Close.
func (r *Root) WriteExisting(rel string) (*os.File, error) {
	if !r.Writable() {
		return nil, ErrReadOnly
	}
	clean, err := Clean(rel)
	if err != nil {
		return nil, err
	}
	if err := r.require(clean, false); err != nil {
		return nil, err
	}
	return r.root.OpenFile(filepath.FromSlash(clean), os.O_TRUNC|os.O_WRONLY, 0o644)
}

// WalkDir walks the tree rooted at rel through the sandbox, skipping entries
// that are not listed (and everything under an unlisted directory).
func (r *Root) WalkDir(rel string, fn fs.WalkDirFunc) error {
	clean, err := Clean(rel)
	if err != nil {
		return err
	}
	if err := r.require(clean, true); err != nil {
		return err
	}
	l := r.Lister()
	return fs.WalkDir(r.root.FS(), clean, func(p string, d fs.DirEntry, err error) error {
		if err == nil && p != clean && !l.Listed(p, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		return fn(p, d, err)
	})
}
