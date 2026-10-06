// Policy is the soft, per-workspace allow/deny layer that sits on top of
// the hard os.Root containment boundary. Containment decides what is reachable;
// policy decides what, among the reachable, is actually served. Block always
// wins, and a dotfile backstop denies hidden paths that no explicit allow glob
// names. The same Policy also decides whether writes are allowed and which paths
// are listed (hiding gitignored ones from listings, never from reads).
package mcp

import (
	"path"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/mnehpets/workspace-mcp/grrep"
)

// Decision is the outcome of a policy check.
type Decision struct {
	Allowed bool
	// Reason is empty when allowed; otherwise one of: "blocked_glob",
	// "dotfile", "not_allowlisted".
	Reason string
}

var decisionAllowed = Decision{Allowed: true}

// Policy holds one workspace's rules: the allow/block globs, whether writes are
// permitted at all, and whether .gitignore/.ignore files hide paths from
// listings.
type Policy struct {
	allow     []string
	block     []string
	writes    bool
	gitignore bool
}

// PolicyOption adjusts a Policy built by NewPolicy.
type PolicyOption func(*Policy)

// WithWrites permits writes (still subject to the same allow/block rules as
// reads). Without it the policy is read-only.
func WithWrites() PolicyOption { return func(p *Policy) { p.writes = true } }

// WithGitignore hides paths matched by .gitignore/.ignore files from listings
// (ReadDir, WalkDir, Lister). It never affects whether a path can be read.
func WithGitignore() PolicyOption { return func(p *Policy) { p.gitignore = true } }

// CanWrite reports whether the policy permits writes.
func (p *Policy) CanWrite() bool { return p.writes }

// alwaysBlocked is added to every workspace's block list. Git's own directory is
// never served, even when an allow glob such as "**/*" would otherwise match it.
var alwaysBlocked = []string{".git", ".git/**", "**/.git", "**/.git/**"}

// NewPolicy builds a Policy. Globs are doublestar patterns (validated at config load).
func NewPolicy(allow, block []string, opts ...PolicyOption) *Policy {
	p := &Policy{allow: allow, block: append(append([]string{}, alwaysBlocked...), block...)}
	for _, o := range opts {
		o(p)
	}
	return p
}

func matchGlobs(globs []string, rel string) bool {
	for _, g := range globs {
		if ok, _ := doublestar.Match(g, rel); ok {
			return true
		}
	}
	return false
}

func hasDotSegment(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if len(seg) > 1 && seg[0] == '.' {
			return true
		}
	}
	return false
}

// CheckFile decides whether a file's content may be served (file_read, grep
// match, find result). rel must already be a clean workspace-relative path
// (see Clean). A file must clear block, the dotfile backstop, and the
// allow list.
func (p *Policy) CheckFile(rel string) Decision {
	if rel == "." {
		return Decision{false, "not_allowlisted"}
	}
	if matchGlobs(p.block, rel) {
		return Decision{false, "blocked_glob"}
	}
	explicitlyAllowed := matchGlobs(p.allow, rel)
	if hasDotSegment(rel) && !explicitlyAllowed {
		return Decision{false, "dotfile"}
	}
	if len(p.allow) > 0 && !explicitlyAllowed {
		return Decision{false, "not_allowlisted"}
	}
	return decisionAllowed
}

// CheckDir decides whether a directory may be listed or traversed. Directories
// are not subject to the allow list (otherwise nothing could be browsed), but
// they must clear block and the dotfile backstop. The root (".") is always
// listable.
func (p *Policy) CheckDir(rel string) Decision {
	if rel == "." {
		return decisionAllowed
	}
	if matchGlobs(p.block, rel) {
		return Decision{false, "blocked_glob"}
	}
	if hasDotSegment(rel) && !matchGlobs(p.allow, rel) {
		return Decision{false, "dotfile"}
	}
	return decisionAllowed
}

// Check applies the access rules to a clean workspace-relative path: a file is
// subject to block, the dotfile rule and the allow list (CheckFile), a directory
// only to block and the dotfile rule (CheckDir). A nil Policy allows everything.
func (p *Policy) Check(rel string, isDir bool) Decision {
	if p == nil {
		return decisionAllowed
	}
	if isDir {
		return p.CheckDir(rel)
	}
	return p.CheckFile(rel)
}

// Lister decides which paths appear in listings: a path is listed if the access
// rules allow it and neither it nor any parent directory is gitignored. A Lister
// reads the ignore files afresh when created, caches what it learns, and is safe
// for concurrent use, so create one per enumeration.
type Lister struct {
	p    *Policy
	mu   sync.Mutex
	ig   *grrep.IgnoreSet // nil when gitignore handling is off
	seen map[string]bool  // directory -> ignored
}

// Lister returns a new Lister for one enumeration of the tree at dir (the
// workspace's absolute root, where the ignore files are read from).
func (p *Policy) Lister(dir string) *Lister {
	l := &Lister{p: p}
	if p != nil && p.gitignore {
		l.ig = grrep.NewIgnoreSet(dir)
		l.seen = map[string]bool{}
	}
	return l
}

// Listed reports whether the clean workspace-relative path should appear in
// listings and search results. Listed implies the path is accessible.
func (l *Lister) Listed(rel string, isDir bool) bool {
	if !l.p.Check(rel, isDir).Allowed {
		return false
	}
	if l.ig == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Walk down the parent directories, loading each one's ignore file before
	// matching anything beneath it.
	segs := strings.Split(rel, "/")
	dir := ""
	for _, seg := range segs[:len(segs)-1] {
		dir = path.Join(dir, seg)
		ignored, ok := l.seen[dir]
		if !ok {
			ignored = l.ig.Match(dir, true)
			if !ignored {
				l.ig.EnsureNode(dir)
			}
			l.seen[dir] = ignored
		}
		if ignored {
			return false
		}
	}
	return !l.ig.Match(rel, isDir)
}
