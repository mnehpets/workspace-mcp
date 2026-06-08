// Policy is the soft, per-workspace allow/deny layer that sits on top of
// the hard os.Root containment boundary. Containment decides what is reachable;
// policy decides what, among the reachable, is actually served. Block always
// wins, and a dotfile backstop denies hidden paths that no explicit allow glob
// names.
package mcp

import (
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Decision is the outcome of a policy check.
type Decision struct {
	Allowed bool
	// Reason is empty when allowed; otherwise one of: "blocked_glob",
	// "dotfile", "not_allowlisted".
	Reason string
}

var decisionAllowed = Decision{Allowed: true}

// Policy holds one workspace's allow/block globs.
type Policy struct {
	allow []string
	block []string
}

// NewPolicy builds a Policy. Globs are doublestar patterns (validated at config load).
func NewPolicy(allow, block []string) *Policy {
	return &Policy{allow: allow, block: block}
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
