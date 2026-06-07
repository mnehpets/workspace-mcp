package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"strings"

	"github.com/mnehpets/workspace-mcp/audit"
	"github.com/mnehpets/workspace-mcp/fsroot"
	"github.com/mnehpets/workspace-mcp/gitaware"
	"github.com/mnehpets/workspace-mcp/search"
	"github.com/mnehpets/workspace-mcp/workspace"
)

// Tool is one entry in the tools/list catalog.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// workspaceProp builds the shared `workspace` schema property. It advertises the
// configured workspace names and the default, so the model can omit it for the
// common case (→ "default") or pick a valid one without a separate lookup.
func workspaceProp(names []string) map[string]any {
	desc := "Workspace to operate on. Optional — omit to use the \"default\" workspace."
	if len(names) > 0 {
		desc += " Available: " + strings.Join(names, ", ") + "."
	}
	p := map[string]any{
		"type":        "string",
		"description": desc,
	}
	if len(names) > 0 {
		p["enum"] = names
	}
	return p
}

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// toolDefs returns the tool catalog. Tool identities/shapes are fixed, but the
// `workspace` enum/description is populated from the configured workspaces so the
// model sees the available names and the default. A tool may still fail per-call
// for a given workspace (disabled grep, non-git tree).
func (s *Server) toolDefs() []Tool {
	names := make([]string, 0)
	for _, w := range s.reg.List() {
		names = append(names, w.Name)
	}
	wsProp := func() map[string]any { return workspaceProp(names) }
	return []Tool{
		{
			Name:        "workspace_list",
			Description: "List the configured workspaces (directory trees) available to operate on.",
			InputSchema: schema(map[string]any{}),
		},
		{
			Name:        "tree_list",
			Description: "List directory entries under a path in a workspace.",
			InputSchema: schema(map[string]any{
				"workspace": wsProp(),
				"path":      map[string]any{"type": "string", "description": "Workspace-relative directory (default: root)."},
				"recursive": map[string]any{"type": "boolean", "description": "Recurse into subdirectories."},
			}),
		},
		{
			Name:        "file_read",
			Description: "Read the contents of one allowed file in a workspace.",
			InputSchema: schema(map[string]any{
				"workspace": wsProp(),
				"path":      map[string]any{"type": "string", "description": "Workspace-relative file path."},
				"maxBytes":  map[string]any{"type": "integer", "description": "Optional read cap (bounded by the workspace limit)."},
			}, "path"),
		},
		{
			Name:        "tree_find",
			Description: "Fuzzy-search filenames in a workspace.",
			InputSchema: schema(map[string]any{
				"workspace": wsProp(),
				"query":     map[string]any{"type": "string", "description": "Fuzzy filename query."},
				"limit":     map[string]any{"type": "integer", "description": "Max results (default 100)."},
			}, "query"),
		},
		{
			Name:        "tree_grep",
			Description: "Search file contents in a workspace (fixed-string by default; regex optional).",
			InputSchema: schema(map[string]any{
				"workspace":       wsProp(),
				"pattern":         map[string]any{"type": "string", "description": "Search pattern."},
				"path":            map[string]any{"type": "string", "description": "Workspace-relative subtree to search (default: root)."},
				"fixedString":     map[string]any{"type": "boolean", "description": "Literal substring search (default true). Set false for regex."},
				"caseInsensitive": map[string]any{"type": "boolean", "description": "Case-insensitive match."},
				"wordBoundary":    map[string]any{"type": "boolean", "description": "Match only at word boundaries."},
			}, "pattern"),
		},
		{
			Name:        "git_status",
			Description: "Read-only git status (branch + per-file codes) for a git-repo workspace.",
			InputSchema: schema(map[string]any{
				"workspace": wsProp(),
			}),
		},
	}
}

// resolveWS looks up a workspace, returning a *toolError on failure.
func (s *Server) resolveWS(name string) (*workspace.Workspace, *toolError) {
	ws, err := s.reg.Get(name)
	if err != nil {
		return nil, mapWorkspaceError(err)
	}
	return ws, nil
}

func unmarshalArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return newToolError("INVALID_ARGS", "invalid arguments: "+err.Error())
	}
	return nil
}

// --- workspace_list ---

func (s *Server) workspaceList(_ json.RawMessage) (any, audit.ToolEvent, error) {
	ev := audit.ToolEvent{}
	type wsInfo struct {
		Name      string `json:"name"`
		IsGitRepo bool   `json:"isGitRepo"`
	}
	list := []wsInfo{}
	for _, w := range s.reg.List() {
		list = append(list, wsInfo{Name: w.Name, IsGitRepo: w.IsGitRepo})
	}
	return map[string]any{"workspaces": list}, ev, nil
}

// --- tree_list ---

type treeListArgs struct {
	Workspace string `json:"workspace"`
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"`
}

type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type treeListResult struct {
	Entries []treeEntry `json:"entries"`
}

func (s *Server) treeList(args json.RawMessage) (any, audit.ToolEvent, error) {
	var a treeListArgs
	ev := audit.ToolEvent{}
	if err := unmarshalArgs(args, &a); err != nil {
		return nil, ev, err
	}
	ev.Workspace = a.Workspace
	ws, te := s.resolveWS(a.Workspace)
	if te != nil {
		return nil, ev, te
	}
	clean, err := fsroot.Clean(a.Path)
	if err != nil {
		return nil, ev, mapPathError(err)
	}
	ev.Paths = []string{clean}
	if d := ws.Policy.CheckDir(clean); !d.Allowed {
		return nil, ev, mapPolicyDenied(d.Reason)
	}
	entries, err := listDir(ws, clean, a.Recursive)
	if err != nil {
		return nil, ev, mapPathError(err)
	}
	return treeListResult{Entries: entries}, ev, nil
}

func listDir(ws *workspace.Workspace, clean string, recursive bool) ([]treeEntry, error) {
	out := []treeEntry{}
	if recursive {
		err := ws.Root.WalkDir(clean, func(rel string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if rel == clean {
				return nil
			}
			if d.IsDir() {
				if !ws.Policy.CheckDir(rel).Allowed {
					return fs.SkipDir
				}
				if ws.Ignore != nil {
					if ws.Ignore.Match(rel, true) {
						return fs.SkipDir
					}
					ws.Ignore.EnsureNode(rel)
				}
				out = append(out, treeEntry{Path: rel, Type: "dir"})
				return nil
			}
			if !ws.Policy.CheckFile(rel).Allowed {
				return nil
			}
			if ws.Ignore != nil && ws.Ignore.Match(rel, false) {
				return nil
			}
			out = append(out, treeEntry{Path: rel, Type: "file", Size: entrySize(d)})
			return nil
		})
		return out, err
	}

	entries, err := ws.Root.ReadDir(clean)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		rel := e.Name()
		if clean != "." {
			rel = clean + "/" + e.Name()
		}
		if e.IsDir() {
			if !ws.Policy.CheckDir(rel).Allowed {
				continue
			}
			if ws.Ignore != nil && ws.Ignore.Match(rel, true) {
				continue
			}
			out = append(out, treeEntry{Path: rel, Type: "dir"})
			continue
		}
		if !ws.Policy.CheckFile(rel).Allowed {
			continue
		}
		if ws.Ignore != nil && ws.Ignore.Match(rel, false) {
			continue
		}
		out = append(out, treeEntry{Path: rel, Type: "file", Size: entrySize(e)})
	}
	return out, nil
}

func entrySize(d fs.DirEntry) int64 {
	if info, err := d.Info(); err == nil {
		return info.Size()
	}
	return 0
}

// --- file_read ---

type fileReadArgs struct {
	Workspace string `json:"workspace"`
	Path      string `json:"path"`
	MaxBytes  int64  `json:"maxBytes"`
}

type fileReadResult struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
}

func (s *Server) fileRead(args json.RawMessage) (any, audit.ToolEvent, error) {
	var a fileReadArgs
	ev := audit.ToolEvent{}
	if err := unmarshalArgs(args, &a); err != nil {
		return nil, ev, err
	}
	ev.Workspace = a.Workspace
	ws, te := s.resolveWS(a.Workspace)
	if te != nil {
		return nil, ev, te
	}
	clean, err := fsroot.Clean(a.Path)
	if err != nil {
		return nil, ev, mapPathError(err)
	}
	ev.Paths = []string{clean}
	if d := ws.Policy.CheckFile(clean); !d.Allowed {
		return nil, ev, mapPolicyDenied(d.Reason)
	}

	info, err := ws.Root.Stat(clean)
	if err != nil {
		return nil, ev, mapPathError(err)
	}
	if info.IsDir() {
		return nil, ev, newToolError("NOT_FOUND", "path is a directory")
	}

	limit := ws.Read.MaxBytes
	if a.MaxBytes > 0 && a.MaxBytes < limit {
		limit = a.MaxBytes
	}

	f, err := ws.Root.Open(clean)
	if err != nil {
		return nil, ev, mapPathError(err)
	}
	defer f.Close()

	data := make([]byte, limit)
	n, err := io.ReadFull(f, data)
	data = data[:n]
	truncated := false
	switch err {
	case nil:
		// Filled the cap exactly; check whether more bytes remain.
		var extra [1]byte
		if k, _ := f.Read(extra[:]); k > 0 {
			truncated = true
		}
	case io.EOF, io.ErrUnexpectedEOF:
		// Whole file read.
	default:
		return nil, ev, mapPathError(err)
	}

	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	binary := bytes.IndexByte(head, 0) >= 0

	res := fileReadResult{Path: clean, Truncated: truncated, Binary: binary}
	if !binary {
		res.Content = string(data)
		ev.Bytes = len(data)
	}
	return res, ev, nil
}

// --- tree_find ---

type treeFindArgs struct {
	Workspace string `json:"workspace"`
	Query     string `json:"query"`
	Limit     int    `json:"limit"`
}

func (s *Server) treeFind(args json.RawMessage) (any, audit.ToolEvent, error) {
	var a treeFindArgs
	ev := audit.ToolEvent{}
	if err := unmarshalArgs(args, &a); err != nil {
		return nil, ev, err
	}
	ev.Workspace = a.Workspace
	ws, te := s.resolveWS(a.Workspace)
	if te != nil {
		return nil, ev, te
	}
	res, err := search.Find(ws.Root, ws.Policy, ws.Ignore, a.Query, a.Limit)
	if err != nil {
		return nil, ev, mapPathError(err)
	}
	ev.Matches = len(res.Files)
	return res, ev, nil
}

// --- tree_grep ---

type treeGrepArgs struct {
	Workspace       string `json:"workspace"`
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	FixedString     *bool  `json:"fixedString"`
	CaseInsensitive bool   `json:"caseInsensitive"`
	WordBoundary    bool   `json:"wordBoundary"`
}

func (s *Server) treeGrep(args json.RawMessage) (any, audit.ToolEvent, error) {
	var a treeGrepArgs
	ev := audit.ToolEvent{}
	if err := unmarshalArgs(args, &a); err != nil {
		return nil, ev, err
	}
	ev.Workspace = a.Workspace
	ws, te := s.resolveWS(a.Workspace)
	if te != nil {
		return nil, ev, te
	}
	if !ws.Grep.Enabled {
		return nil, ev, newToolError("GREP_DISABLED", "grep is disabled for this workspace")
	}
	clean, err := fsroot.Clean(a.Path)
	if err != nil {
		return nil, ev, mapPathError(err)
	}
	ev.Paths = []string{clean}
	if d := ws.Policy.CheckDir(clean); !d.Allowed {
		return nil, ev, mapPolicyDenied(d.Reason)
	}
	fixed := true
	if a.FixedString != nil {
		fixed = *a.FixedString
	}
	res, err := search.Grep(ws.Root, ws.Policy, ws.Ignore, search.GrepRequest{
		Pattern:         a.Pattern,
		Path:            clean,
		FixedString:     fixed,
		CaseInsensitive: a.CaseInsensitive,
		WordBoundary:    a.WordBoundary,
	}, ws.Grep.Workers, ws.Grep.MaxMatches)
	if err != nil {
		return nil, ev, invalidPattern(err)
	}
	ev.Matches = len(res.Matches)
	return res, ev, nil
}

// --- git_status ---

type gitStatusArgs struct {
	Workspace string `json:"workspace"`
}

func (s *Server) gitStatus(args json.RawMessage) (any, audit.ToolEvent, error) {
	var a gitStatusArgs
	ev := audit.ToolEvent{}
	if err := unmarshalArgs(args, &a); err != nil {
		return nil, ev, err
	}
	ev.Workspace = a.Workspace
	ws, te := s.resolveWS(a.Workspace)
	if te != nil {
		return nil, ev, te
	}
	if !ws.IsGitRepo {
		return nil, ev, newToolError("NOT_A_GIT_REPO", "workspace is not a git repository")
	}
	st, err := gitaware.GetStatus(ws.Root.Dir())
	if err != nil {
		return nil, ev, mapPathError(err)
	}
	ev.Matches = len(st.Files)
	return st, ev, nil
}
