package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/mnehpets/workspace-mcp/gitaware"
)

// Tool is one entry in the tools/list catalog.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

// readOnlyAnnotations is the MCP tool-annotation set every tool here shares:
// each only reads, and only from the local sandbox (no external/open world).
// title is a human-readable display name for clients that surface one.
func readOnlyAnnotations(title string) map[string]any {
	return map[string]any{
		"title":         title,
		"readOnlyHint":  true,
		"openWorldHint": false,
	}
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
			Name: "workspace_list",
			Description: "Start here. Lists the available workspaces (named local directory trees) you can operate on: each one's name, whether it is a git repository, a short description of what it is for, and which orientation files (e.g. README.md) exist at its root. " +
				"Call this first to discover valid `workspace` values and pick one by intent, then orient yourself in it (read the listed orientation files) before using the other tools. No parameters.",
			InputSchema: schema(map[string]any{}),
			Annotations: readOnlyAnnotations("List workspaces"),
		},
		{
			Name: "file_read",
			Description: "Read the contents of one file in a workspace. " +
				"Use it after locating a file with tree_search. " +
				"Pass `startLine`/`endLine` to read only a span of a large file (the result reports `totalLines` so you can page through). " +
				"Large reads are truncated at a byte cap (`truncated` is set; raise `maxBytes` up to the workspace limit). " +
				"Binary files are flagged and not returned as text by default; set `allowBinary` to receive their raw bytes base64-encoded (with a `mimeType`) so you can parse them yourself.",
			InputSchema: schema(map[string]any{
				"workspace":   wsProp(),
				"path":        map[string]any{"type": "string", "description": "Workspace-relative path to the file to read."},
				"maxBytes":    map[string]any{"type": "integer", "description": "Optional cap on bytes returned (still bounded by the workspace's read limit). If more remains, the result is truncated."},
				"startLine":   map[string]any{"type": "integer", "description": "Optional 1-based first line to return (inclusive). Omit to start at the beginning."},
				"endLine":     map[string]any{"type": "integer", "description": "Optional 1-based last line to return (inclusive). Omit to read to the end. Use with startLine to page a large file."},
				"allowBinary": map[string]any{"type": "boolean", "description": "If true, return a binary file's raw bytes as base64 (with encoding=\"base64\" and a detected mimeType) instead of just flagging it (default false)."},
			}, "path"),
			Annotations: readOnlyAnnotations("Read file"),
		},
		{
			Name: "tree_search",
			Description: "Find files in a workspace by path, by content, or both — also how you browse what exists. " +
				"Give `path` (a glob like \"docs/**/*.md\") to select candidate files by name/location, and/or `where` (a list of content predicates, AND-combined) to keep only files whose body contains all of them. " +
				"With `path` alone and no `where` it just enumerates the matching files — use it to discover structure instead of a separate directory listing. To see the whole tree at once, omit `path` (or pass \"**/*\"); use \"*\" only for the root level and \"docs/**\" for a subtree (a single `*` does not cross directories). With `where` it searches their contents like grep. " +
				"Returns a flat list of files, each with its `size` in bytes and the matched lines (`matches`); set `includeMatches=false` for paths only. " +
				"Matches inside a leading `---`…`---` frontmatter block are reported separately as `metadataMatches`, and `includeMetadata=true` returns each file's raw frontmatter text — pass it while browsing to read titles/tags/summaries up front and pick the right files in a single call rather than judging by filename. " +
				"Results are capped (see `truncated`) — narrow the `path` glob or add a more specific `where` predicate to cut noise.",
			InputSchema: schema(map[string]any{
				"workspace": wsProp(),
				"path":      map[string]any{"type": "string", "description": "Glob selecting candidate files — both the search boundary and a name filter. Omit it (or use \"**/*\") to walk the ENTIRE tree recursively; that is usually what you want for \"show me everything\". `**` crosses directory boundaries, but a single `*` does NOT — so \"*\" lists only the root level, \"docs/*\" only the immediate children of docs/, while \"docs/**\" or \"docs/**/*.md\" reaches all descendants. Prefer `**` unless you deliberately want one level."},
				"where": map[string]any{
					"type":        "array",
					"description": "Content predicates over each file's body, AND-combined: a file is returned only if every predicate matches at least one line. Omit to search by path alone (file enumeration). Requires the workspace's grep to be enabled.",
					"items": schema(map[string]any{
						"text":            map[string]any{"type": "string", "description": "Text to search for. A literal substring unless fixedString=false, in which case a Go regular expression."},
						"fixedString":     map[string]any{"type": "boolean", "description": "Literal substring search (default true). Set false to treat `text` as a regular expression."},
						"caseInsensitive": map[string]any{"type": "boolean", "description": "Case-insensitive match (default false)."},
						"wordBoundary":    map[string]any{"type": "boolean", "description": "Match only at word boundaries (default false)."},
					}, "text"),
				},
				"includeMatches":  map[string]any{"type": "boolean", "description": "Attach the matched lines (line number + text) to each file (default true). Set false to return just the paths."},
				"includeMetadata": map[string]any{"type": "boolean", "description": "Attach each file's raw, unparsed frontmatter block (the text between leading `---` fences) as `metadata` (default false). No effect on files without a frontmatter fence. Set this when enumerating/browsing (no `where`) to triage by each file's own description — titles, tags, summaries — in one pass, instead of guessing relevance from filenames and then opening files one by one."},
			}),
			Annotations: readOnlyAnnotations("Search files by path and content"),
		},
		{
			Name: "git_status",
			Description: "Show read-only git status — current branch and per-file change codes — for a workspace that is a git repository (otherwise returns NOT_A_GIT_REPO). " +
				"Orientation only: it neither reads file contents nor modifies anything.",
			InputSchema: schema(map[string]any{
				"workspace": wsProp(),
			}),
			Annotations: readOnlyAnnotations("Git status"),
		},
	}
}

// resolveWS looks up a workspace, returning a *toolError on failure.
func (s *Server) resolveWS(name string) (*Workspace, *toolError) {
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

func (s *Server) workspaceList(_ json.RawMessage) (any, ToolEvent, error) {
	ev := ToolEvent{}
	type wsInfo struct {
		Name           string   `json:"name"`
		IsGitRepo      bool     `json:"isGitRepo"`
		Description    string   `json:"description,omitempty"`
		WellKnownFiles []string `json:"wellKnownFiles,omitempty"`
	}
	list := []wsInfo{}
	for _, w := range s.reg.List() {
		list = append(list, wsInfo{
			Name:           w.Name,
			IsGitRepo:      w.IsGitRepo,
			Description:    w.Description,
			WellKnownFiles: w.WellKnownFiles,
		})
	}
	return map[string]any{"workspaces": list}, ev, nil
}

// --- file_read ---

type fileReadArgs struct {
	Workspace   string `json:"workspace"`
	Path        string `json:"path"`
	MaxBytes    int64  `json:"maxBytes"`
	StartLine   *int   `json:"startLine"`   // 1-based inclusive; nil = open-ended toward the start
	EndLine     *int   `json:"endLine"`     // 1-based inclusive; nil = open-ended toward the end
	AllowBinary bool   `json:"allowBinary"` // deliver binary files as base64 instead of refusing
}

type fileReadResult struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Truncated  bool   `json:"truncated"`
	Binary     bool   `json:"binary"`
	Encoding   string `json:"encoding,omitempty"`   // "base64" when content is raw binary
	MimeType   string `json:"mimeType,omitempty"`   // detected MIME type for binary delivery
	StartLine  int    `json:"startLine,omitempty"`  // resolved span start (only when a range was requested)
	EndLine    int    `json:"endLine,omitempty"`    // resolved span end
	TotalLines int    `json:"totalLines,omitempty"` // total lines in the (scanned) file
	Notice     string `json:"notice,omitempty"`
}

// Steering notices attached when a result is capped, so the model knows how to
// get the rest instead of treating the partial result as complete (per the
// truncation-messaging guidance in docs/design.md).
const (
	fileTruncatedNotice   = "Output truncated at the byte cap. Raise `maxBytes` (up to the workspace limit) or read a narrower section of the file."
	searchTruncatedNotice = "Results truncated at the match cap. Narrow `path` to a tighter glob, or add a more specific `where` predicate (and `wordBoundary`)."
)

func (s *Server) fileRead(args json.RawMessage) (any, ToolEvent, error) {
	var a fileReadArgs
	ev := ToolEvent{}
	if err := unmarshalArgs(args, &a); err != nil {
		return nil, ev, err
	}
	ev.Workspace = a.Workspace
	ws, te := s.resolveWS(a.Workspace)
	if te != nil {
		return nil, ev, te
	}
	clean, err := Clean(a.Path)
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

	rangeRequested := a.StartLine != nil || a.EndLine != nil
	if a.StartLine != nil && *a.StartLine < 1 {
		return nil, ev, newToolError("INVALID_RANGE", "startLine must be >= 1")
	}
	if a.EndLine != nil && *a.EndLine < 1 {
		return nil, ev, newToolError("INVALID_RANGE", "endLine must be >= 1")
	}
	if a.StartLine != nil && a.EndLine != nil && *a.StartLine > *a.EndLine {
		return nil, ev, newToolError("INVALID_RANGE", "startLine must be <= endLine")
	}

	// Bytes to scan. A ranged read scans up to the workspace cap (maxBytes then
	// caps the returned *span*); a whole-file read scans the smaller of the two,
	// exactly as before.
	scanLimit := ws.Read.MaxBytes
	if !rangeRequested && a.MaxBytes > 0 && a.MaxBytes < scanLimit {
		scanLimit = a.MaxBytes
	}

	f, err := ws.Root.Open(clean)
	if err != nil {
		return nil, ev, mapPathError(err)
	}
	defer f.Close()

	data := make([]byte, scanLimit)
	n, err := io.ReadFull(f, data)
	data = data[:n]
	readTruncated := false
	switch err {
	case nil:
		// Filled the cap exactly; check whether more bytes remain.
		var extra [1]byte
		if k, _ := f.Read(extra[:]); k > 0 {
			readTruncated = true
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

	res := fileReadResult{Path: clean, Binary: binary}
	if binary {
		if !a.AllowBinary {
			// Default: flag, don't return content as text — ranges don't apply.
			res.Truncated = readTruncated
			if readTruncated {
				res.Notice = fileTruncatedNotice
			}
			return res, ev, nil
		}
		// Raw-binary delivery: hand the bytes over base64-encoded for the platform
		// to parse. Same os.Root/policy/byte-cap limits as a text read — the server
		// extracts nothing. Ranges are text-only, so deliver the whole capped blob.
		blob := data
		truncated := readTruncated
		if a.MaxBytes > 0 && int64(len(blob)) > a.MaxBytes {
			blob = blob[:a.MaxBytes]
			truncated = true
		}
		res.Content = base64.StdEncoding.EncodeToString(blob)
		res.Encoding = "base64"
		res.MimeType = detectMime(clean, blob)
		res.Truncated = truncated
		if truncated {
			res.Notice = fileTruncatedNotice
		}
		ev.Bytes = len(blob)
		return res, ev, nil
	}

	if !rangeRequested {
		res.Content = string(data)
		res.Truncated = readTruncated
		if readTruncated {
			res.Notice = fileTruncatedNotice
		}
		ev.Bytes = len(data)
		return res, ev, nil
	}

	// Ranged read: extract the requested line span, then cap it at maxBytes.
	lines := splitLines(string(data))
	total := len(lines)
	start := 1
	if a.StartLine != nil {
		start = *a.StartLine
	}
	end := total
	if a.EndLine != nil {
		end = *a.EndLine
	}
	if end > total { // ordered out-of-bounds ranges clamp to the file
		end = total
	}
	var span string
	if start <= total && start <= end {
		span = strings.Join(lines[start-1:end], "\n")
	}
	spanTruncated := false
	if a.MaxBytes > 0 && int64(len(span)) > a.MaxBytes {
		span = span[:a.MaxBytes]
		spanTruncated = true
	}
	res.Content = span
	res.StartLine = start
	res.EndLine = end
	res.TotalLines = total
	res.Truncated = readTruncated || spanTruncated
	if res.Truncated {
		res.Notice = fileTruncatedNotice
	}
	ev.Bytes = len(span)
	return res, ev, nil
}

// detectMime returns a MIME type for binary delivery: the file extension's
// registered type if known, else content sniffing (always yields something,
// defaulting to application/octet-stream).
func detectMime(name string, data []byte) string {
	if ext := path.Ext(name); ext != "" {
		if t := mime.TypeByExtension(ext); t != "" {
			return t
		}
	}
	return http.DetectContentType(data)
}

// splitLines splits text into lines, dropping the spurious trailing empty
// element a final newline produces so totalLines reflects real line count.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// --- tree_search ---

type treeSearchArgs struct {
	Workspace       string           `json:"workspace"`
	Path            string           `json:"path"`  // glob; "" = whole tree
	Where           []wherePredicate `json:"where"` // body predicates, AND-combined
	IncludeMatches  *bool            `json:"includeMatches"`
	IncludeMetadata bool             `json:"includeMetadata"`
}

type wherePredicate struct {
	Text            string `json:"text"`
	FixedString     *bool  `json:"fixedString"`
	CaseInsensitive bool   `json:"caseInsensitive"`
	WordBoundary    bool   `json:"wordBoundary"`
}

func (s *Server) treeSearch(args json.RawMessage) (any, ToolEvent, error) {
	var a treeSearchArgs
	ev := ToolEvent{}
	if err := unmarshalArgs(args, &a); err != nil {
		return nil, ev, err
	}
	ev.Workspace = a.Workspace
	ws, te := s.resolveWS(a.Workspace)
	if te != nil {
		return nil, ev, te
	}
	// Content predicates require grep; a pure path-glob query (file enumeration)
	// does not — it only walks.
	if len(a.Where) > 0 && !ws.Grep.Enabled {
		return nil, ev, newToolError("GREP_DISABLED", "grep is disabled for this workspace")
	}
	preds := make([]Predicate, 0, len(a.Where))
	for _, w := range a.Where {
		if strings.TrimSpace(w.Text) == "" {
			return nil, ev, newToolError("INVALID_ARGS", "each `where` predicate needs non-empty text")
		}
		fixed := true
		if w.FixedString != nil {
			fixed = *w.FixedString
		}
		preds = append(preds, Predicate{
			Text:            w.Text,
			FixedString:     fixed,
			CaseInsensitive: w.CaseInsensitive,
			WordBoundary:    w.WordBoundary,
		})
	}
	includeMatches := true
	if a.IncludeMatches != nil {
		includeMatches = *a.IncludeMatches
	}
	if a.Path != "" {
		ev.Paths = []string{a.Path}
	}
	res, err := Search(ws.Root, ws.Policy, ws.Ignore, SearchRequest{
		PathGlob:        a.Path,
		Where:           preds,
		IncludeMatches:  includeMatches,
		IncludeMetadata: a.IncludeMetadata,
	}, ws.Grep.Workers, ws.Grep.MaxMatches, ws.Read.MaxBytes)
	if err != nil {
		return nil, ev, mapSearchError(err)
	}
	if res.Truncated {
		res.Notice = searchTruncatedNotice
	}
	ev.Matches = len(res.Files)
	return res, ev, nil
}

// --- git_status ---

type gitStatusArgs struct {
	Workspace string `json:"workspace"`
}

func (s *Server) gitStatus(args json.RawMessage) (any, ToolEvent, error) {
	var a gitStatusArgs
	ev := ToolEvent{}
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
