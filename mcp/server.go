// Package mcp implements the Model Context Protocol surface (initialize,
// tools/list, tools/call) on top of the jsonrpc registry, gating every tool
// call through per-workspace policy and the os.Root sandbox.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"text/template"

	"github.com/mnehpets/http/jsonrpc"
	"github.com/mnehpets/workspace-mcp/gitaware"
)

const serverName = "workspace-mcp"

// serverVersion is reported in initialize.
const serverVersion = "0.1.0"

// supportedProtocols lists MCP protocol versions we understand, newest first.
var supportedProtocols = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// Server holds shared state for the MCP handlers. Each Server is bound to a
// single workspace: workspace selection happens at the HTTP routing layer (one
// endpoint per workspace, §17), so the tools take no `workspace` argument and the
// instructions are workspace-specific.
type Server struct {
	ws  *Workspace
	log *Logger
}

// NewServer builds a Server bound to one workspace.
func NewServer(ws *Workspace, log *Logger) *Server {
	return &Server{ws: ws, log: log}
}

// Register wires the MCP methods onto a jsonrpc endpoint.
func (s *Server) Register(e *jsonrpc.JSONRPCEndpoint) {
	e.Register("", s)
}

// --- initialize ---

// InitializeParams is the client handshake.
type InitializeParams struct {
	_               struct{}        `jsonrpc:"initialize"`
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities"`
	ClientInfo      json.RawMessage `json:"clientInfo"`
}

// InitializeResult is the server handshake response.
type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      serverInfo     `json:"serverInfo"`
	Instructions    string         `json:"instructions,omitempty"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// instructionsData is the render context for instructionsTemplate.
type instructionsData struct {
	Description    string // this workspace's description (may be empty)
	WellKnownFiles string // orientation files, comma-joined (empty when none)
	IsGitRepo      bool
	Writable       bool // the opt-in write surface is enabled for this workspace
}

// instructionsTemplate renders the MCP `instructions` string for one workspace.
// The URL already picked the tree (§17), so the text is workspace-specific: it
// folds in the workspace's description and detected orientation files, then the
// orient-first flow and the hard constraints. Trim markers ({{- / -}}) keep the
// conditional blocks from leaving stray blank lines.
var instructionsTemplate = template.Must(template.New("instructions").Parse(
	`This server is a window into a single local directory tree (a "workspace") on the user's machine — notes, docs, papers, code, or data. It hands you raw bytes plus cheap orientation; YOU do the analysis. {{if .Writable}}It can also create and edit files in this tree (see below), but it is not a coding agent and cannot run commands.{{else}}It is read-only: it is not a coding agent and cannot run commands or write anything.{{end}}
{{if .Description}}
This workspace: {{.Description}}
{{end}}
How to use it:
{{if .WellKnownFiles -}}
1. Getting oriented (if the question needs it): this tree has {{.WellKnownFiles}} at its root worth reading with file_read, and tree_search surveys what else exists.
{{- else -}}
1. Getting oriented (if the question needs it): tree_search surveys what files exist — try "includeMetadata": true to read their titles/tags in one pass.
{{- end}}
2. Locate and browse: tree_search finds files by path and/or content. Omit "path" (or pass "**/*") to see the ENTIRE tree at once; use a glob like "docs/**/*.md" to narrow by name/location ("**" crosses directories, a single "*" does not — "*" is root-level only). Add "where" (content predicates) to find where a term appears. Omit "where" to just list matching files with their sizes (this is also how you explore directory structure) — and pass "includeMetadata": true on that listing to get each file's frontmatter (title/tags/summary) so you can pick the right files in one pass instead of guessing from filenames. Narrow "path" to avoid truncation.
3. Read: file_read returns a file's contents; large files truncate at a byte cap (raise "maxBytes" up to the workspace limit).
{{if .IsGitRepo -}}
4. git_status gives the current branch + per-file change codes (this workspace is a git repository).
{{end -}}
{{if .Writable}}
Editing (only when the user asks you to change files): file_create writes a new file (fails if it exists), file_overwrite replaces an existing file wholesale, and file_replace swaps an exact substring (defaults to a unique single match). Edits write raw bytes with no normalization; pass a file's current "sha256" (returned by file_read, or by a prior write) as "base_sha256" to refuse the write if the file changed underneath you, and "dry_run": true to preview. Writes go only where reads are allowed — blocked paths stay unwritable. The human reviews the diff in git and commits; this server never commits, pushes, deletes, moves, or renames.
{{end -}}
Constraints: {{if .Writable}}create/overwrite/replace are the only mutations — no shell, no git mutation, no delete/move/rename.{{else}}read-only (no writes, shell, or git mutation exist).{{end}} Access is default-deny — some paths are intentionally invisible, so a NOT_FOUND or POLICY_DENIED is an expected answer, not a transient error to retry. All paths are workspace-relative; absolute paths and ".." are rejected.`))

// workspaceInstructions renders the `instructions` string for one workspace. It
// is best-effort: the spec lets servers send it, but general-purpose hosts may
// ignore it — so the tool descriptions and the workspace_info ("start here") tool
// must stand on their own too.
func workspaceInstructions(ws *Workspace) string {
	var b strings.Builder
	if err := instructionsTemplate.Execute(&b, instructionsData{
		Description:    ws.Description,
		WellKnownFiles: strings.Join(ws.WellKnownFiles, ", "),
		IsGitRepo:      ws.IsGitRepo,
		Writable:       ws.Write.Enabled,
	}); err != nil {
		// The template is a compile-time constant with trivial fields; an execution
		// error is impossible in practice. Degrade to empty rather than panic.
		return ""
	}
	return b.String()
}

// Initialize negotiates a protocol version and advertises the tools capability.
func (s *Server) Initialize(_ context.Context, p InitializeParams) (InitializeResult, error) {
	return InitializeResult{
		ProtocolVersion: negotiateProtocol(p.ProtocolVersion),
		Capabilities:    map[string]any{"tools": map[string]any{}},
		ServerInfo:      serverInfo{Name: serverName, Version: serverVersion},
		Instructions:    workspaceInstructions(s.ws),
	}, nil
}

// negotiateProtocol echoes the client's version when supported, else falls back
// to our newest supported version (the client then decides whether to proceed).
func negotiateProtocol(requested string) string {
	for _, v := range supportedProtocols {
		if v == requested {
			return v
		}
	}
	return supportedProtocols[0]
}

// --- notifications/initialized ---

// InitializedParams carries the post-handshake notification (no response).
type InitializedParams struct {
	_ struct{} `jsonrpc:"notifications/initialized"`
}

// Initialized accepts the client's initialized notification.
func (s *Server) Initialized(_ context.Context, _ InitializedParams) (struct{}, error) {
	return struct{}{}, nil
}

// --- ping ---

// PingParams is an MCP ping.
type PingParams struct {
	_ struct{} `jsonrpc:"ping"`
}

// Ping responds to a ping with an empty object.
func (s *Server) Ping(_ context.Context, _ PingParams) (map[string]any, error) {
	return map[string]any{}, nil
}

// --- tools/list ---

// ToolsListParams is the tools/list request (no fields).
type ToolsListParams struct {
	_ struct{} `jsonrpc:"tools/list"`
}

// ToolsListResult is the tools/list response.
type ToolsListResult struct {
	Tools []Tool `json:"tools"`
}

// ToolsList returns the tool catalog (with the workspace enum populated from config).
func (s *Server) ToolsList(_ context.Context, _ ToolsListParams) (ToolsListResult, error) {
	return ToolsListResult{Tools: s.toolDefs()}, nil
}

// --- tools/call ---

// ToolsCallParams is the tools/call request.
type ToolsCallParams struct {
	_         struct{}        `jsonrpc:"tools/call"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// TextContent is an MCP text content block.
type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolResult is the tools/call response.
type ToolResult struct {
	Content []TextContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// toolFunc is a tool handler. It returns the result value, audit metadata, and
// an error (a *toolError for in-band domain failures).
type toolFunc func(s *Server, args json.RawMessage) (any, ToolEvent, error)

// ToolsCall dispatches to the named tool, enforcing the allowlist and mapping
// domain errors to isError results while protocol errors stay JSON-RPC errors.
func (s *Server) ToolsCall(_ context.Context, p ToolsCallParams) (ToolResult, error) {
	h, ok := toolHandlers[p.Name]
	if !ok {
		// Unknown tool name is a protocol-level rejection.
		return ToolResult{}, jsonrpc.NewError(jsonrpc.CodeInvalidParams, "unknown tool: "+p.Name)
	}
	result, ev, err := h(s, p.Arguments)
	ev.Method = "tools/call"
	ev.Tool = p.Name
	ev.Workspace = s.ws.Name
	if err != nil {
		te := asToolError(err)
		ev.Allowed = false
		if te.Reason != "" {
			ev.Reason = te.Reason
		} else {
			ev.Reason = te.Code
		}
		ev.Err = te.Code
		s.log.ToolCall(ev)
		return errorResult(te), nil
	}
	ev.Allowed = true
	s.log.ToolCall(ev)
	return okResult(result), nil
}

// toolHandlers maps tool name to handler (method expressions).
var toolHandlers = map[string]toolFunc{
	"workspace_info": (*Server).workspaceInfo,
	"file_read":      (*Server).fileRead,
	"tree_search":    (*Server).treeSearch,
	"git_status":     (*Server).gitStatus,
	// Write surface (§8.7). Always registered so a forced call on a write-disabled
	// workspace returns READ_ONLY (via writeGate) rather than "unknown tool"; they
	// only appear in tools/list when write.enabled (see toolDefs).
	"file_create":    (*Server).fileCreate,
	"file_overwrite": (*Server).fileOverwrite,
	"file_replace":   (*Server).fileReplace,
}

// --- error model ---

// toolError is an in-band tool failure, surfaced to the model as an isError
// result with a machine-readable code (see PLAN §13).
type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Reason  string `json:"reason,omitempty"`
}

func (e *toolError) Error() string { return e.Code + ": " + e.Message }

func newToolError(code, msg string) *toolError { return &toolError{Code: code, Message: msg} }

// asToolError normalizes any error into a *toolError.
func asToolError(err error) *toolError {
	var te *toolError
	if errors.As(err, &te) {
		return te
	}
	return &toolError{Code: "INTERNAL", Message: "internal error"}
}

// mapPathError converts fsroot/os path errors into a POLICY_DENIED or NOT_FOUND
// tool error.
func mapPathError(err error) *toolError {
	switch {
	case errors.Is(err, ErrAbsolutePath):
		return &toolError{Code: "POLICY_DENIED", Message: "absolute path not allowed", Reason: "absolute_path"}
	case errors.Is(err, ErrTraversal):
		return &toolError{Code: "POLICY_DENIED", Message: "path traversal not allowed", Reason: "traversal"}
	case errors.Is(err, os.ErrNotExist):
		return &toolError{Code: "NOT_FOUND", Message: "not found"}
	case errors.Is(err, gitaware.ErrNotGitRepo):
		return &toolError{Code: "NOT_A_GIT_REPO", Message: "not a git repository"}
	default:
		// Anything else from os.Root (incl. "path escapes from parent") is a
		// containment denial; do not leak the underlying message.
		return &toolError{Code: "POLICY_DENIED", Message: "path denied", Reason: "outside_root"}
	}
}

// mapPolicyDenied builds a POLICY_DENIED error for a policy decision reason.
func mapPolicyDenied(reason string) *toolError {
	return &toolError{Code: "POLICY_DENIED", Message: "denied by policy", Reason: reason}
}

func okResult(v any) ToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return errorResult(&toolError{Code: "INTERNAL", Message: "encode error"})
	}
	return ToolResult{Content: []TextContent{{Type: "text", Text: string(b)}}}
}

func errorResult(te *toolError) ToolResult {
	b, _ := json.Marshal(te)
	return ToolResult{Content: []TextContent{{Type: "text", Text: string(b)}}, IsError: true}
}

// mapSearchError maps a tree_search failure: an uncompilable regex predicate to
// INVALID_PATTERN, anything else (e.g. an absolute/traversing path glob) through
// the shared path-error mapping.
func mapSearchError(err error) *toolError {
	var ip *InvalidPatternError
	if errors.As(err, &ip) {
		return &toolError{Code: "INVALID_PATTERN", Message: ip.Err.Error()}
	}
	return mapPathError(err)
}
