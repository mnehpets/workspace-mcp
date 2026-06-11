package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"os"
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

// writeAnnotations is the annotation set for the opt-in write tools: not
// read-only, closed-world, and (for the in-place ops) flagged destructive since
// they replace existing bytes. file_create is non-destructive — it only ever adds
// a new file.
func writeAnnotations(title string, destructive bool) map[string]any {
	return map[string]any{
		"title":           title,
		"readOnlyHint":    false,
		"destructiveHint": destructive,
		"openWorldHint":   false,
	}
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

// toolDefs returns the tool catalog. The workspace is fixed by the endpoint URL
// (§17), so no tool takes a `workspace` argument. A tool may still fail per-call
// for this workspace (disabled grep, non-git tree).
func (s *Server) toolDefs() []Tool {
	tools := []Tool{
		{
			Name: "workspace_info",
			Description: "Orientation for THIS workspace (the local directory tree this connector is pointed at): its name, whether it is a git repository, a short description of what it is for, which orientation files (e.g. README.md) exist at its root, an `orientation` string telling you how to use the tools, and a `preview` inlining the start of the top orientation file. " +
				"The `orientation` string duplicates the instructions the server sends at connect time, so skip calling this just for those if you already received them — but the `preview` is NOT in those instructions, so call it to read the README/overview in one shot instead of a separate file_read. No parameters.",
			InputSchema: schema(map[string]any{}),
			Annotations: readOnlyAnnotations("Workspace info"),
		},
		{
			Name: "file_read",
			Description: "Read the contents of one file in this workspace. " +
				"Use it after locating a file with tree_search. " +
				"Pass `startLine`/`endLine` to read only a span of a large file (the result reports `totalLines` so you can page through). " +
				"Large reads are truncated at a byte cap (`truncated` is set; raise `maxBytes` up to the workspace limit). " +
				"Binary files are flagged and not returned as text by default; set `allowBinary` to receive their raw bytes base64-encoded (with a `mimeType`) so you can parse them yourself. " +
				"The result includes the file's `sha256` (over the full file, even for a ranged or truncated read) — pass it as a later edit's `base_sha256` to guard against the file changing between read and write.",
			InputSchema: schema(map[string]any{
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
			Description: "Find files in this workspace by path, by content, or both — also how you browse what exists. " +
				"Give `path` (a glob like \"docs/**/*.md\") to select candidate files by name/location, and/or `where` (a list of content predicates, AND-combined) to keep only files whose body contains all of them. " +
				"With `path` alone and no `where` it just enumerates the matching files — use it to discover structure instead of a separate directory listing. To see the whole tree at once, omit `path` (or pass \"**/*\"); use \"*\" only for the root level and \"docs/**\" for a subtree (a single `*` does not cross directories). With `where` it searches their contents like grep. " +
				"Returns a flat list of files, each with its `size` in bytes and the matched lines (`matches`); set `includeMatches=false` for paths only. " +
				"Matches inside a leading `---`…`---` frontmatter block are reported separately as `metadataMatches`, and `includeMetadata=true` returns each file's raw frontmatter text — pass it while browsing to read titles/tags/summaries up front and pick the right files in a single call rather than judging by filename. " +
				"Results are capped (see `truncated`) — narrow the `path` glob or add a more specific `where` predicate to cut noise.",
			InputSchema: schema(map[string]any{
				"path": map[string]any{"type": "string", "description": "Glob selecting candidate files — both the search boundary and a name filter. Omit it (or use \"**/*\") to walk the ENTIRE tree recursively; that is usually what you want for \"show me everything\". `**` crosses directory boundaries, but a single `*` does NOT — so \"*\" lists only the root level, \"docs/*\" only the immediate children of docs/, while \"docs/**\" or \"docs/**/*.md\" reaches all descendants. Prefer `**` unless you deliberately want one level."},
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
			Description: "Show read-only git status — current branch, per-file change codes, and upstream tracking info — when this workspace is a git repository (otherwise returns NOT_A_GIT_REPO). " +
				"The `upstream` field reports how many commits the local branch is ahead/behind its remote-tracking ref; it is null when no tracking branch is configured or the remote was never fetched. " +
				"Counts are as of the last fetch — no network call is made. " +
				"Orientation only: it neither reads file contents nor modifies anything. No parameters.",
			InputSchema: schema(map[string]any{}),
			Annotations: readOnlyAnnotations("Git status"),
		},
		{
			Name: "git_diff",
			Description: "Show the working-tree diff — what actually changed, as a standard unified diff — when this workspace is a git repository (otherwise NOT_A_GIT_REPO). " +
				"Read-only. The natural companion to git_status: status lists changed paths, git_diff shows their content changes. " +
				"By default diffs the worktree against the index (like `git diff`), with untracked files included as all-new; `staged: true` diffs the index against HEAD (like `git diff --cached`). " +
				"Pass `path` (a file, or a directory prefix to scope to a subtree) to diff only that — do this when the whole-tree diff comes back truncated. " +
				"Returns a `files` summary (per-file change kind and +/- line counts; always complete even when the diff text is truncated) and `diff`, the unified diff text. " +
				"Binary files, symlinks, and files over the read limit are listed but their content is skipped. Renames are not detected (they appear as a delete plus an add).",
			InputSchema: schema(map[string]any{
				"path":   map[string]any{"type": "string", "description": "Optional workspace-relative file or directory prefix to scope the diff. Omit for the whole worktree. Policy-gated like any read: a blocked path returns POLICY_DENIED."},
				"staged": map[string]any{"type": "boolean", "description": "Diff the index against HEAD (`git diff --cached`) instead of the worktree against the index (default false)."},
			}),
			Annotations: readOnlyAnnotations("Git diff"),
		},
	}
	if s.ws.Write.Enabled {
		tools = append(tools, s.writeToolDefs()...)
	}
	return tools
}

// writeToolDefs returns the opt-in write tools (§8.7). They are only included in
// tools/list when the workspace sets write.enabled; a write-disabled workspace
// never advertises them (and a forced call returns READ_ONLY via writeGate).
func (s *Server) writeToolDefs() []Tool {
	return []Tool{
		{
			Name: "file_create",
			Description: "Create a NEW file in this workspace with the given contents. " +
				"Fails with PATH_EXISTS if the path already exists — use file_overwrite to replace an existing file. " +
				"Missing parent directories are created automatically. Writes raw bytes with no normalization (no trailing-newline or line-ending rewrite), so include exactly the bytes you want. " +
				"Returns the resulting `sha256` (pass it as a later edit's `base_sha256`).",
			InputSchema: schema(map[string]any{
				"path":     map[string]any{"type": "string", "description": "Workspace-relative path of the file to create."},
				"contents": map[string]any{"type": "string", "description": "Full contents of the new file, written verbatim."},
			}, "path", "contents"),
			Annotations: writeAnnotations("Create file", false),
		},
		{
			Name: "file_overwrite",
			Description: "Replace the ENTIRE contents of an existing file in this workspace. " +
				"Use it when a file changes so substantially that quoting an `old_str` would be pointless; for a localized edit prefer file_replace. " +
				"Fails with NOT_FOUND if the path does not exist (use file_create), so a typo can't silently create a stray file. " +
				"Pass `base_sha256` (the file's current hash, e.g. the `sha256` returned by file_read) to reject the write if the file changed since you read it (BASE_SHA_MISMATCH). " +
				"Pass `dry_run: true` to preview the resulting hash without writing. Writes raw bytes with no normalization.",
			InputSchema: schema(map[string]any{
				"path":        map[string]any{"type": "string", "description": "Workspace-relative path of the existing file to overwrite."},
				"contents":    map[string]any{"type": "string", "description": "Full new contents, written verbatim."},
				"base_sha256": map[string]any{"type": "string", "description": "Optional optimistic-concurrency guard: the file's expected current hex SHA-256. The write is rejected with BASE_SHA_MISMATCH (returning the actual hash) if it differs."},
				"dry_run":     map[string]any{"type": "boolean", "description": "If true, validate and return the would-be result hash without writing (default false)."},
			}, "path", "contents"),
			Annotations: writeAnnotations("Overwrite file", true),
		},
		{
			Name: "file_replace",
			Description: "Replace occurrences of an exact substring (`old_str`) with `new_str` in an existing file. " +
				"Matches raw bytes exactly — no whitespace or line-ending normalization — so quote `old_str` precisely, including indentation. " +
				"By default exactly ONE occurrence must match; the call is rejected with MATCH_COUNT_MISMATCH (echoing the actual count) otherwise, which guarantees you edited the span you meant. " +
				"Set `expected_replacements` to change all N matches deliberately. Empty `old_str` is rejected. " +
				"Optional `base_sha256` (reject on drift) and `dry_run` (preview the match count and resulting hash without writing) behave as in file_overwrite. " +
				"Files larger than the workspace read limit are rejected with FILE_TOO_LARGE.",
			InputSchema: schema(map[string]any{
				"path":                  map[string]any{"type": "string", "description": "Workspace-relative path of the existing file to edit."},
				"old_str":               map[string]any{"type": "string", "description": "Exact substring to find (matched against raw bytes, verbatim). Must be non-empty."},
				"new_str":               map[string]any{"type": "string", "description": "Replacement substring (may be empty to delete the matched text)."},
				"expected_replacements": map[string]any{"type": "integer", "description": "Number of occurrences that must match (default 1). The edit is rejected unless the actual count equals this exactly."},
				"base_sha256":           map[string]any{"type": "string", "description": "Optional optimistic-concurrency guard: the file's expected current hex SHA-256 (BASE_SHA_MISMATCH on drift)."},
				"dry_run":               map[string]any{"type": "boolean", "description": "If true, return the match count and resulting hash without writing (default false)."},
			}, "path", "old_str", "new_str"),
			Annotations: writeAnnotations("Replace in file", true),
		},
	}
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

// --- workspace_info ---

// workspaceInfo returns this workspace's orientation. It scans the tree root
// fresh on every call (task 25) so that a README or other well-known file added
// through the write tools shows up without a server restart. The `orientation`
// text is rebuilt from the fresh data, so it may differ from the startup-time
// `instructions` in `initialize` if the tree changed. When a well-known file
// exists it inlines a capped preview so the "read the README" move needs no
// follow-up file_read round-trip.
func (s *Server) workspaceInfo(_ json.RawMessage) (any, ToolEvent, error) {
	ev := ToolEvent{}
	w := s.ws

	// Scan fresh: reflect any files created/deleted since startup.
	freshFiles := detectOrientation(w.Root, w.Policy)

	// Description: config-supplied is authoritative and never refreshed;
	// README-derived is re-derived from the fresh file list.
	desc := w.Description
	if !w.HasConfigDescription {
		desc = deriveDescription(w.Root, freshFiles)
	}

	orientation := renderInstructions(instructionsData{
		Description:    desc,
		WellKnownFiles: strings.Join(freshFiles, ", "),
		IsGitRepo:      w.IsGitRepo,
		Writable:       w.Write.Enabled,
	})

	out := map[string]any{
		"name":           w.Name,
		"isGitRepo":      w.IsGitRepo,
		"description":    desc,
		"wellKnownFiles": freshFiles,
		"orientation":    orientation,
		"version":        s.version,
	}
	if p := readOrientationPreview(w, freshFiles); p != nil {
		out["preview"] = p
		ev.Paths = []string{p.Path}
		ev.Bytes = len(p.Content)
	}
	return out, ev, nil
}

// previewMaxLines / previewMaxBytes bound the inlined orientation-file preview:
// enough to orient, not enough to dump a large file into every workspace_info.
const (
	previewMaxLines = 200
	previewMaxBytes = 16 << 10
)

type orientationPreview struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Truncated  bool   `json:"truncated"`            // more file remains beyond the preview
	TotalLines int    `json:"totalLines,omitempty"` // only when the whole file fit the byte window
}

// readOrientationPreview reads the first previewMaxLines (capped by bytes) of
// the highest-priority file in files, through the workspace's os.Root. Files
// must already be policy-gated (detectOrientation ensures this). Returns nil
// when files is empty, the read fails, or the content looks binary.
func readOrientationPreview(w *Workspace, files []string) *orientationPreview {
	if len(files) == 0 {
		return nil
	}
	name := files[0]
	limit := int64(previewMaxBytes)
	if w.Read.MaxBytes > 0 && w.Read.MaxBytes < limit {
		limit = w.Read.MaxBytes
	}
	f, err := w.Root.Open(name)
	if err != nil {
		return nil
	}
	defer f.Close()

	data := make([]byte, limit)
	n, _ := io.ReadFull(f, data)
	data = data[:n]
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return nil // binary; not a useful preview
	}
	// Does more content remain past the byte window?
	moreBytes := false
	var extra [1]byte
	if k, _ := f.Read(extra[:]); k > 0 {
		moreBytes = true
	}

	lines := splitLines(string(data))
	total := len(lines)
	truncated := moreBytes
	if total > previewMaxLines {
		lines = lines[:previewMaxLines]
		truncated = true
	}
	p := &orientationPreview{
		Path:      name,
		Content:   strings.Join(lines, "\n"),
		Truncated: truncated,
	}
	if !moreBytes { // line count is only trustworthy when the whole file fit
		p.TotalLines = total
	}
	return p
}

// --- file_read ---

type fileReadArgs struct {
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
	// SHA256 is the hex SHA-256 of the file's full on-disk bytes (the same hash
	// base_sha256 checks, §8.3), independent of any line range or maxBytes
	// truncation — so a read-then-write loop carries it straight into a
	// file_replace/file_overwrite base_sha256 with no extra round-trip. Empty for
	// files past the workspace read limit (uneditable, no comparable base hash).
	SHA256 string `json:"sha256,omitempty"`
	Notice string `json:"notice,omitempty"`
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
	ws := s.ws
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
	// Hash the file's full on-disk bytes (the base_sha256 guard, §8.3), not the
	// returned slice: when nothing was truncated, data already is the whole file;
	// otherwise re-read it through os.Root bounded by the workspace limit.
	if !readTruncated {
		res.SHA256 = hashHex(data)
	} else {
		res.SHA256 = hashFileFull(ws.Root, clean, ws.Read.MaxBytes)
	}
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
	ws := s.ws
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

func (s *Server) gitStatus(_ json.RawMessage) (any, ToolEvent, error) {
	ev := ToolEvent{}
	ws := s.ws
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

// --- git_diff ---

type gitDiffArgs struct {
	Path   string `json:"path"`
	Staged bool   `json:"staged"`
}

// gitDiffFileSummary is one file's metadata in the envelope. It is always
// complete (cheap), even when the `diff` text is truncated, so the model can
// re-scope to a single path.
type gitDiffFileSummary struct {
	Path      string `json:"path"`
	Change    string `json:"change"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary,omitempty"`
	TooLarge  bool   `json:"tooLarge,omitempty"`
	Symlink   bool   `json:"symlink,omitempty"`
}

type gitDiffResult struct {
	Files     []gitDiffFileSummary `json:"files"`
	Diff      string               `json:"diff"`
	Truncated bool                 `json:"truncated"`
	Notice    string               `json:"notice,omitempty"`
}

const diffTruncatedNotice = "diff truncated at the read limit — re-run scoped to a single path (the files list is complete)"

// rootWorktreeReader backs gitaware's WorktreeReader with the workspace os.Root,
// so every worktree byte read for a diff crosses the same symlink-/TOCTOU-safe
// boundary as file_read. It caps each read at max+1 bytes; gitaware treats a
// result longer than the limit as TooLarge, so no unbounded file is buffered.
type rootWorktreeReader struct {
	root *Root
	max  int64
}

func (r rootWorktreeReader) ReadFile(rel string) ([]byte, os.FileMode, bool, error) {
	info, err := r.root.Lstat(rel)
	if err != nil {
		return nil, 0, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, info.Mode(), true, nil
	}
	f, err := r.root.Open(rel)
	if err != nil {
		return nil, 0, false, err
	}
	defer f.Close()
	limit := r.max + 1
	if limit <= 1 {
		limit = 1 << 62 // no workspace cap configured; read fully
	}
	data := make([]byte, limit)
	n, err := io.ReadFull(f, data)
	switch err {
	case nil, io.EOF, io.ErrUnexpectedEOF:
	default:
		return nil, 0, false, err
	}
	return data[:n], info.Mode(), false, nil
}

func (s *Server) gitDiff(args json.RawMessage) (any, ToolEvent, error) {
	var a gitDiffArgs
	ev := ToolEvent{}
	if err := unmarshalArgs(args, &a); err != nil {
		return nil, ev, err
	}
	ws := s.ws
	if !ws.IsGitRepo {
		return nil, ev, newToolError("NOT_A_GIT_REPO", "workspace is not a git repository")
	}

	scope := "" // "" = whole worktree
	scoped := false
	if strings.TrimSpace(a.Path) != "" {
		clean, err := Clean(a.Path)
		if err != nil {
			return nil, ev, mapPathError(err)
		}
		// Denied wins over existence, so a blocked path never leaks its presence.
		if d := ws.Policy.CheckFile(clean); !d.Allowed {
			return nil, ev, mapPolicyDenied(d.Reason)
		}
		scope = clean
		scoped = true
		ev.Paths = []string{clean}
	}

	// Per-file gate: policy (a denied file is silently excluded from an unscoped
	// diff so a dirty .env can never leak) AND, when scoped, the path scope.
	filter := func(rel string) bool {
		if !ws.Policy.CheckFile(rel).Allowed {
			return false
		}
		return scopeMatches(scope, rel)
	}

	reader := rootWorktreeReader{root: ws.Root, max: ws.Read.MaxBytes}
	diffs, err := gitaware.Diff(ws.Root.Dir(), reader, gitaware.DiffOptions{
		Staged:       a.Staged,
		PathFilter:   filter,
		MaxFileBytes: ws.Read.MaxBytes,
	})
	if err != nil {
		return nil, ev, mapPathError(err)
	}

	if scoped && len(diffs) == 0 {
		// Decide NOT_FOUND vs unchanged: the path is present if the worktree has
		// it (tracked or not), or it lives in the index/HEAD (a deleted file).
		exists := false
		if _, statErr := ws.Root.Stat(scope); statErr == nil {
			exists = true
		}
		if !exists {
			if known, _ := gitaware.PathInRepo(ws.Root.Dir(), scope); known {
				exists = true
			}
		}
		if !exists {
			return nil, ev, newToolError("NOT_FOUND", "path not found in worktree, index, or HEAD")
		}
	}

	res := gitDiffResult{Files: make([]gitDiffFileSummary, 0, len(diffs))}
	var b strings.Builder
	totalCap := ws.Read.MaxBytes
	for _, fd := range diffs {
		res.Files = append(res.Files, gitDiffFileSummary{
			Path:      fd.Path,
			Change:    fd.Change,
			Additions: fd.Additions,
			Deletions: fd.Deletions,
			Binary:    fd.Binary,
			TooLarge:  fd.TooLarge,
			Symlink:   fd.Symlink,
		})
		if res.Truncated {
			continue // files list stays complete; diff text stopped at the cap
		}
		piece := diffPiece(fd)
		if totalCap > 0 && int64(b.Len()+len(piece)) > totalCap && b.Len() > 0 {
			res.Truncated = true
			continue
		}
		b.WriteString(piece)
	}
	res.Diff = b.String()

	switch {
	case res.Truncated:
		res.Notice = diffTruncatedNotice
	case len(res.Files) == 0:
		res.Notice = "no changes"
	}
	ev.Matches = len(res.Files)
	return res, ev, nil
}

// diffPiece returns the text a single file contributes to the unified diff:
// its patch, or a one-line marker for content that is intentionally skipped.
func diffPiece(fd gitaware.FileDiff) string {
	switch {
	case fd.Symlink:
		return "# diff of " + fd.Path + " skipped: symlink\n"
	case fd.TooLarge:
		return "# diff of " + fd.Path + " skipped: file exceeds read limit\n"
	case fd.Binary:
		return "Binary files a/" + fd.Path + " and b/" + fd.Path + " differ\n"
	default:
		return fd.Patch
	}
}

// scopeMatches reports whether rel falls under a scope path. An empty scope
// matches everything; otherwise it matches the exact file or a segment-aligned
// directory prefix ("mcp" matches "mcp/tools.go", not "mcpx/y.go").
func scopeMatches(scope, rel string) bool {
	if scope == "" || scope == "." {
		return true
	}
	return rel == scope || strings.HasPrefix(rel, scope+"/")
}
