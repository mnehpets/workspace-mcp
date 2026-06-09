// Root wraps os.Root to give every workspace a hard, symlink-safe,
// TOCTOU-safe filesystem boundary. All model-supplied paths cross the boundary
// here: they are workspace-relative, slash-separated, and resolved through the
// underlying *os.Root, which guarantees the result stays within the root even in
// the presence of symlinks.
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
)

// Root is a sandboxed view of one workspace directory tree.
type Root struct {
	root *os.Root
	dir  string
}

// Open opens dir as an os.Root sandbox.
func Open(dir string) (*Root, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Root{root: r, dir: dir}, nil
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
	return r.root.Open(filepath.FromSlash(clean))
}

// Stat stats a path through the sandbox (following symlinks, but never escaping).
func (r *Root) Stat(rel string) (os.FileInfo, error) {
	clean, err := Clean(rel)
	if err != nil {
		return nil, err
	}
	return r.root.Stat(filepath.FromSlash(clean))
}

// ReadDir lists directory entries through the sandbox.
func (r *Root) ReadDir(rel string) ([]fs.DirEntry, error) {
	clean, err := Clean(rel)
	if err != nil {
		return nil, err
	}
	return fs.ReadDir(r.root.FS(), clean)
}

// CreateNew creates a new file for writing through the sandbox, failing if the
// path already exists (O_CREATE|O_EXCL|O_WRONLY). Missing parent directories are
// created (MkdirAll, inside the root). The caller owns Close. This is the only
// path that creates a file, so a collision is reported (os.ErrExist) rather than
// silently clobbered.
func (r *Root) CreateNew(rel string) (*os.File, error) {
	clean, err := Clean(rel)
	if err != nil {
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
	clean, err := Clean(rel)
	if err != nil {
		return nil, err
	}
	return r.root.OpenFile(filepath.FromSlash(clean), os.O_TRUNC|os.O_WRONLY, 0o644)
}

// WalkDir walks the tree rooted at rel through the sandbox.
func (r *Root) WalkDir(rel string, fn fs.WalkDirFunc) error {
	clean, err := Clean(rel)
	if err != nil {
		return err
	}
	return fs.WalkDir(r.root.FS(), clean, fn)
}
