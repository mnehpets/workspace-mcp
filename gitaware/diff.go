package gitaware

import (
	"bytes"
	"io"
	"os"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/utils/diff"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// WorktreeReader reads worktree file content for diffing. The caller (mcp
// layer) backs it with the workspace's os.Root so reads are symlink-safe and
// sandboxed; gitaware never opens worktree files itself.
type WorktreeReader interface {
	// ReadFile returns the file's bytes, its os.FileMode, and whether it is a
	// symlink (symlinks are reported, not followed; target never read). The
	// caller may cap the bytes returned at the workspace read limit (plus a
	// little, so the diff layer can detect an over-limit file by length).
	ReadFile(rel string) (data []byte, mode os.FileMode, isSymlink bool, err error)
}

// DiffOptions configures a Diff call.
type DiffOptions struct {
	// Staged selects index-vs-HEAD (git diff --cached) instead of the default
	// worktree-vs-index (git diff).
	Staged bool
	// PathFilter returns false for files that must be excluded (policy-denied or
	// out of the requested path scope). Never nil — caller passes
	// func(string) bool { return true } for "everything".
	PathFilter func(rel string) bool
	// MaxFileBytes: skip (TooLarge) any file whose either side exceeds this.
	MaxFileBytes int64
}

// FileDiff is one changed file's diff result. Patch is empty when the file's
// content is skipped (Binary, TooLarge, or Symlink).
type FileDiff struct {
	Path      string // clean slash path
	Change    string // added | modified | deleted | untracked
	Binary    bool
	TooLarge  bool
	Symlink   bool
	Additions int
	Deletions int
	Patch     string // this file's unified diff text ("" when Binary/TooLarge/Symlink)
}

// Diff computes per-file diffs for the repository at dir. It returns
// ErrNotGitRepo when dir is not a git repository. Files are returned sorted by
// path (matching GetStatus). The mcp layer owns the total-size cap and envelope
// assembly; gitaware owns the git mechanics.
func Diff(dir string, wr WorktreeReader, opts DiffOptions) ([]FileDiff, error) {
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
	idx, err := repo.Storer.Index()
	if err != nil {
		return nil, err
	}

	// HEAD tree, if any. An unborn HEAD (fresh repo, no commits) leaves it nil,
	// so staged diffs compare the index against an empty tree.
	var headTree *object.Tree
	if ref, err := repo.Head(); err == nil {
		if commit, err := repo.CommitObject(ref.Hash()); err == nil {
			if t, err := commit.Tree(); err == nil {
				headTree = t
			}
		}
	}

	paths := make([]string, 0, len(st))
	for p := range st {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var out []FileDiff
	for _, p := range paths {
		if opts.PathFilter != nil && !opts.PathFilter(p) {
			continue
		}
		s := st[p]
		var fs *fileSides
		if opts.Staged {
			fs, err = stagedSides(repo, idx, headTree, p, s)
		} else {
			fs, err = unstagedSides(repo, idx, wr, p, s)
		}
		if err != nil {
			return nil, err
		}
		if fs == nil {
			continue
		}
		fd, err := assemble(*fs, opts.MaxFileBytes)
		if err != nil {
			return nil, err
		}
		out = append(out, fd)
	}
	return out, nil
}

// PathInRepo reports whether clean (an exact path or a directory prefix) is
// present in the index or the HEAD tree of the repository at dir. The mcp layer
// uses it, together with a worktree stat, to tell a NOT_FOUND apart from an
// unchanged path when a scoped diff comes back empty. ErrNotGitRepo when dir is
// not a repository.
func PathInRepo(dir, clean string) (bool, error) {
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		if err == gogit.ErrRepositoryNotExists {
			return false, ErrNotGitRepo
		}
		return false, err
	}
	prefix := clean + "/"
	if idx, err := repo.Storer.Index(); err == nil {
		for _, e := range idx.Entries {
			if e.Name == clean || strings.HasPrefix(e.Name, prefix) {
				return true, nil
			}
		}
	}
	if ref, err := repo.Head(); err == nil {
		if commit, err := repo.CommitObject(ref.Hash()); err == nil {
			if tree, err := commit.Tree(); err == nil {
				if _, err := tree.File(clean); err == nil {
					return true, nil
				}
				found := false
				if err := tree.Files().ForEach(func(f *object.File) error {
					if strings.HasPrefix(f.Name, prefix) {
						found = true
						return storer.ErrStop
					}
					return nil
				}); err != nil {
					return false, err
				}
				if found {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// fileSides holds the two ends of one file's diff plus enough metadata to
// header it. Content is loaded lazily (loadOld/loadNew) so the size guard can
// fire before any blob is read; the worktree side is loaded eagerly by the
// reader but still wrapped behind loadNew.
type fileSides struct {
	path                   string
	change                 string
	oldMode, newMode       filemode.FileMode
	oldHash, newHash       plumbing.Hash
	oldSize, newSize       int64
	oldPresent, newPresent bool
	symlink                bool
	loadOld, loadNew       func() ([]byte, error)
}

// unstagedSides builds the worktree-vs-index sides for one status entry, or nil
// when the entry has no worktree change (it is only staged).
func unstagedSides(repo *gogit.Repository, idx *index.Index, wr WorktreeReader, p string, s *gogit.FileStatus) (*fileSides, error) {
	if s.Worktree == gogit.Unmodified {
		return nil, nil
	}
	fs := &fileSides{path: p}
	switch s.Worktree {
	case gogit.Untracked:
		fs.change = "untracked"
	case gogit.Deleted:
		fs.change = "deleted"
	default:
		fs.change = "modified"
	}

	// Old side: the index blob (absent for an untracked, brand-new file).
	if fs.change != "untracked" {
		if e, err := idx.Entry(p); err == nil {
			if err := setBlobSide(repo, fs, false, e.Hash, e.Mode); err != nil {
				return nil, err
			}
		}
	}
	// New side: the worktree bytes (absent for a deleted file).
	if fs.change != "deleted" {
		data, osMode, isSym, err := wr.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if isSym {
			fs.symlink = true
			return fs, nil
		}
		fs.newPresent = true
		fs.newMode = osToGitMode(osMode)
		fs.newHash = plumbing.ZeroHash
		fs.newSize = int64(len(data))
		d := data
		fs.loadNew = func() ([]byte, error) { return d, nil }
	}
	return fs, nil
}

// stagedSides builds the index-vs-HEAD sides for one status entry, or nil when
// the entry has no staged change.
func stagedSides(repo *gogit.Repository, idx *index.Index, headTree *object.Tree, p string, s *gogit.FileStatus) (*fileSides, error) {
	if s.Staging == gogit.Unmodified || s.Staging == gogit.Untracked {
		return nil, nil
	}
	fs := &fileSides{path: p}
	// Old side: the HEAD-tree blob (absent ⇒ added; also absent under unborn HEAD).
	if headTree != nil {
		if f, err := headTree.File(p); err == nil {
			if err := setBlobSide(repo, fs, false, f.Hash, f.Mode); err != nil {
				return nil, err
			}
		}
	}
	// New side: the index blob (absent ⇒ deleted).
	if s.Staging != gogit.Deleted {
		if e, err := idx.Entry(p); err == nil {
			if err := setBlobSide(repo, fs, true, e.Hash, e.Mode); err != nil {
				return nil, err
			}
		}
	}
	switch {
	case !fs.oldPresent && fs.newPresent:
		fs.change = "added"
	case fs.oldPresent && !fs.newPresent:
		fs.change = "deleted"
	default:
		fs.change = "modified"
	}
	return fs, nil
}

// setBlobSide wires one side of fs to a git blob, recording its size up front
// (cheap, from the object header) so the caller can size-guard before loading.
func setBlobSide(repo *gogit.Repository, fs *fileSides, isNew bool, h plumbing.Hash, mode filemode.FileMode) error {
	blob, err := object.GetBlob(repo.Storer, h)
	if err != nil {
		return err
	}
	loader := func() ([]byte, error) {
		r, err := blob.Reader()
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return io.ReadAll(r)
	}
	if isNew {
		fs.newPresent, fs.newMode, fs.newHash, fs.newSize, fs.loadNew = true, mode, h, blob.Size, loader
	} else {
		fs.oldPresent, fs.oldMode, fs.oldHash, fs.oldSize, fs.loadOld = true, mode, h, blob.Size, loader
	}
	return nil
}

// assemble turns the two sides into a FileDiff, short-circuiting symlinks,
// over-limit files, and binary content before any text diff is computed.
func assemble(fs fileSides, maxBytes int64) (FileDiff, error) {
	fd := FileDiff{Path: fs.path, Change: fs.change}
	if fs.symlink {
		fd.Symlink = true
		return fd, nil
	}
	if maxBytes > 0 && (fs.oldSize > maxBytes || fs.newSize > maxBytes) {
		fd.TooLarge = true
		return fd, nil
	}
	var oldData, newData []byte
	var err error
	if fs.oldPresent {
		if oldData, err = fs.loadOld(); err != nil {
			return fd, err
		}
	}
	if fs.newPresent {
		if newData, err = fs.loadNew(); err != nil {
			return fd, err
		}
	}
	if isBinaryContent(oldData) || isBinaryContent(newData) {
		fd.Binary = true
		return fd, nil
	}
	fd.Patch, fd.Additions, fd.Deletions = encodeUnified(fs, oldData, newData)
	return fd, nil
}

// encodeUnified line-diffs old→new and renders one file's standard unified diff
// via go-git's UnifiedEncoder, returning the patch text and +/- line counts.
func encodeUnified(fs fileSides, oldData, newData []byte) (string, int, int) {
	diffs := diff.Do(string(oldData), string(newData))
	var chunks []fdiff.Chunk
	var add, del int
	for _, d := range diffs {
		var op fdiff.Operation
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			op = fdiff.Equal
		case diffmatchpatch.DiffDelete:
			op = fdiff.Delete
			del += countLines(d.Text)
		case diffmatchpatch.DiffInsert:
			op = fdiff.Add
			add += countLines(d.Text)
		}
		chunks = append(chunks, &textChunk{content: d.Text, op: op})
	}
	fp := &filePatch{chunks: chunks}
	if fs.oldPresent {
		fp.from = &diffFile{path: fs.path, mode: fs.oldMode, hash: fs.oldHash}
	}
	if fs.newPresent {
		fp.to = &diffFile{path: fs.path, mode: fs.newMode, hash: fs.newHash}
	}
	var buf bytes.Buffer
	enc := fdiff.NewUnifiedEncoder(&buf, fdiff.DefaultContextLines)
	if err := enc.Encode(&unifiedPatch{filePatches: []fdiff.FilePatch{fp}}); err != nil {
		return "", add, del
	}
	return buf.String(), add, del
}

// countLines counts the lines in a diff segment: full lines end in '\n', and a
// trailing partial line (no final newline) counts as one more.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// isBinaryContent matches the repo-wide heuristic (file_read, tree_search): a
// NUL byte in the first 8000 bytes means binary.
func isBinaryContent(data []byte) bool {
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	return bytes.IndexByte(head, 0) >= 0
}

// osToGitMode maps a worktree os.FileMode to a git filemode for the diff header,
// preserving the executable bit; non-regular modes fall back to Regular (the
// header value is cosmetic — models do not consume it).
func osToGitMode(m os.FileMode) filemode.FileMode {
	if gm, err := filemode.NewFromOSFileMode(m); err == nil {
		return gm
	}
	return filemode.Regular
}

// --- fdiff interface implementations (mirroring plumbing/object/patch.go) ---

type diffFile struct {
	path string
	mode filemode.FileMode
	hash plumbing.Hash
}

func (f *diffFile) Hash() plumbing.Hash     { return f.hash }
func (f *diffFile) Mode() filemode.FileMode { return f.mode }
func (f *diffFile) Path() string            { return f.path }

type textChunk struct {
	content string
	op      fdiff.Operation
}

func (t *textChunk) Content() string       { return t.content }
func (t *textChunk) Type() fdiff.Operation { return t.op }

type filePatch struct {
	chunks   []fdiff.Chunk
	from, to *diffFile
}

// IsBinary always reports false: assemble screens out binary content before
// ever calling encodeUnified, so a filePatch only exists for text. Deferring to
// the encoder's len(chunks)==0 default would mislabel a legitimately empty side
// (e.g. a new zero-byte file) as binary and emit a stray "Binary files … differ".
func (fp *filePatch) IsBinary() bool { return false }

func (fp *filePatch) Files() (from, to fdiff.File) {
	if fp.from != nil {
		from = fp.from
	}
	if fp.to != nil {
		to = fp.to
	}
	return
}

func (fp *filePatch) Chunks() []fdiff.Chunk { return fp.chunks }

type unifiedPatch struct {
	filePatches []fdiff.FilePatch
}

func (p *unifiedPatch) FilePatches() []fdiff.FilePatch { return p.filePatches }
func (p *unifiedPatch) Message() string                { return "" }
