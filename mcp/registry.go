package mcp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/mnehpets/workspace-mcp/gitaware"
	"github.com/mnehpets/workspace-mcp/grrep"
)

// ErrUnknownWorkspace is returned when a requested workspace name is not configured.
var ErrUnknownWorkspace = errors.New("unknown workspace")

// Workspace is one configured directory tree and its per-workspace settings.
type Workspace struct {
	Name           string
	Root           *Root
	Policy         *Policy
	Ignore         *grrep.IgnoreSet // nil when respectGitignore is disabled
	IsGitRepo      bool
	Read           ReadConfig
	Grep           GrepConfig
	Write          WriteConfig
	Description    string   // what the tree is for; config-supplied or README-derived. May be empty.
	WellKnownFiles []string // orientation files present at the root (subset of wellKnownCandidates).
}

// Registry maps workspace names to their resolved Workspace.
type Registry struct {
	byName map[string]*Workspace
	order  []*Workspace
}

// Build constructs the registry from config, opening one os.Root per workspace
// and detecting git-ness. The caller owns Close.
func Build(cfg *Config) (*Registry, error) {
	reg := &Registry{byName: make(map[string]*Workspace, len(cfg.Workspaces))}
	for i := range cfg.Workspaces {
		wc := cfg.Workspaces[i]
		root, err := Open(wc.Root)
		if err != nil {
			reg.Close()
			return nil, fmt.Errorf("workspace %q: open root: %w", wc.Name, err)
		}
		var ig *grrep.IgnoreSet
		if wc.RespectGitignore {
			ig = grrep.NewIgnoreSet(wc.Root)
		}
		pol := NewPolicy(wc.Policy.AllowGlobs, wc.Policy.BlockGlobs)
		ws := &Workspace{
			Name:      wc.Name,
			Root:      root,
			Policy:    pol,
			Ignore:    ig,
			IsGitRepo: gitaware.Detect(wc.Root),
			Read:      wc.Read,
			Grep:      wc.Grep,
			Write:     wc.Write,
		}
		// Orientation metadata, computed once at startup. Both ride the workspace's
		// os.Root + policy: a blocked/missing file simply contributes nothing.
		ws.WellKnownFiles = detectOrientation(root, pol)
		if wc.Description != "" {
			ws.Description = wc.Description // config is authoritative
		} else {
			ws.Description = deriveDescription(root, ws.WellKnownFiles)
		}
		reg.byName[wc.Name] = ws
		reg.order = append(reg.order, ws)
	}
	return reg, nil
}

// orientationStems is the closed, server-owned set of root orientation-file
// stems, in priority order. Recognition is case-insensitive and
// extension-agnostic on the filename stem, so `README.md`, `README.rst`,
// `index.md`, and `_index` all match. Priority drives both the wellKnownFiles
// ordering and which file the description falls back to.
//
// This is deliberately NOT user-extensible (no config knob — a hand-maintained
// per-workspace list would only rot on rename). To teach the server a new
// convention, add a stem here with a test; revisit only if that proves
// insufficient in practice.
var orientationStems = []string{
	"readme", "index", "_index", "contents", "toc", "overview", "about", "agents", "claude",
}

// maxWellKnownFiles caps the reported list so a noisy root can't flood workspace_info.
const maxWellKnownFiles = 5

// detectOrientation scans the tree root once and returns the orientation files
// present, priority-ordered (then alphabetical), capped at maxWellKnownFiles.
// A candidate must be a regular file (symlinks/dirs skipped) whose lowercased,
// extension-stripped name is a known stem and that clears policy. Presence only
// — it never reads content.
func detectOrientation(root *Root, pol *Policy) []string {
	rank := make(map[string]int, len(orientationStems))
	for i, s := range orientationStems {
		rank[s] = i
	}
	entries, err := root.ReadDir(".")
	if err != nil {
		return nil
	}
	type hit struct {
		name string
		rank int
	}
	var hits []hit
	for _, e := range entries {
		if !e.Type().IsRegular() { // skip dirs and symlinks
			continue
		}
		name := e.Name()
		stem := strings.ToLower(strings.TrimSuffix(name, path.Ext(name)))
		r, ok := rank[stem]
		if !ok {
			continue
		}
		if !pol.CheckFile(name).Allowed {
			continue
		}
		hits = append(hits, hit{name, r})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].rank != hits[j].rank {
			return hits[i].rank < hits[j].rank
		}
		return hits[i].name < hits[j].name
	})
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.name)
		if len(out) >= maxWellKnownFiles {
			break
		}
	}
	return out
}

// deriveDescription returns the first section of the highest-priority detected
// orientation file (text under its first heading), whitespace-collapsed and
// capped. Files are tried in priority order; the first that yields usable,
// non-binary prose wins. Reads ride the workspace's os.Root (symlink-safe);
// files were already policy-gated by detectOrientation.
func deriveDescription(root *Root, files []string) string {
	for _, name := range files {
		f, err := root.Open(name)
		if err != nil {
			continue
		}
		buf := make([]byte, 8192)
		n, _ := io.ReadFull(f, buf)
		f.Close()
		data := buf[:n]
		if bytes.IndexByte(data, 0) >= 0 {
			continue // binary; not a description source
		}
		if d := firstSection(string(data)); d != "" {
			return d
		}
	}
	return ""
}

const descriptionCap = 280

// firstSection extracts the prose under the first markdown heading (or the
// leading paragraph if there is no heading), collapses whitespace, and trims to
// descriptionCap runes. Returns "" when there is no usable prose.
func firstSection(md string) string {
	lines := strings.Split(md, "\n")
	headingIdx := -1
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "#") {
			headingIdx = i
			break
		}
	}
	var body []string
	if headingIdx >= 0 {
		// Everything after the first heading up to the next heading.
		for _, ln := range lines[headingIdx+1:] {
			if strings.HasPrefix(strings.TrimSpace(ln), "#") {
				break
			}
			body = append(body, ln)
		}
	} else {
		body = lines
	}
	text := strings.Join(strings.Fields(strings.Join(body, " ")), " ")
	if r := []rune(text); len(r) > descriptionCap {
		text = strings.TrimSpace(string(r[:descriptionCap])) + "…"
	}
	return text
}

// Get resolves a workspace by name; an empty name defaults to "default".
func (r *Registry) Get(name string) (*Workspace, error) {
	if name == "" {
		name = "default"
	}
	ws, ok := r.byName[name]
	if !ok {
		return nil, ErrUnknownWorkspace
	}
	return ws, nil
}

// List returns the workspaces in configuration order.
func (r *Registry) List() []*Workspace { return r.order }

// Close releases every workspace's os.Root.
func (r *Registry) Close() {
	for _, ws := range r.order {
		if ws.Root != nil {
			ws.Root.Close()
		}
	}
}
