// Package mcp implements the Model Context Protocol surface (initialize,
// tools/list, tools/call) on top of the jsonrpc registry, gating every tool
// call through per-workspace policy and the os.Root sandbox.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/mnehpets/http/jsonrpc"
	"github.com/mnehpets/workspace-mcp/audit"
	"github.com/mnehpets/workspace-mcp/fsroot"
	"github.com/mnehpets/workspace-mcp/gitaware"
	"github.com/mnehpets/workspace-mcp/search"
	"github.com/mnehpets/workspace-mcp/workspace"
)

const serverName = "workspace-mcp"

// serverVersion is reported in initialize.
const serverVersion = "0.1.0"

// supportedProtocols lists MCP protocol versions we understand, newest first.
var supportedProtocols = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Server holds shared state for the MCP handlers.
type Server struct {
	reg *workspace.Registry
	log *audit.Logger
}

// NewServer builds a Server.
func NewServer(reg *workspace.Registry, log *audit.Logger) *Server {
	return &Server{reg: reg, log: log}
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
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Initialize negotiates a protocol version and advertises the tools capability.
func (s *Server) Initialize(_ context.Context, p InitializeParams) (InitializeResult, error) {
	return InitializeResult{
		ProtocolVersion: negotiateProtocol(p.ProtocolVersion),
		Capabilities:    map[string]any{"tools": map[string]any{}},
		ServerInfo:      serverInfo{Name: serverName, Version: serverVersion},
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
type toolFunc func(s *Server, args json.RawMessage) (any, audit.ToolEvent, error)

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
	"workspace_list": (*Server).workspaceList,
	"tree_list":      (*Server).treeList,
	"file_read":      (*Server).fileRead,
	"tree_find":      (*Server).treeFind,
	"tree_grep":      (*Server).treeGrep,
	"git_status":     (*Server).gitStatus,
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
	case errors.Is(err, fsroot.ErrAbsolutePath):
		return &toolError{Code: "POLICY_DENIED", Message: "absolute path not allowed", Reason: "absolute_path"}
	case errors.Is(err, fsroot.ErrTraversal):
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

// mapWorkspaceError maps a registry lookup failure.
func mapWorkspaceError(err error) *toolError {
	if errors.Is(err, workspace.ErrUnknownWorkspace) {
		return &toolError{Code: "UNKNOWN_WORKSPACE", Message: "unknown workspace"}
	}
	return asToolError(err)
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

// invalidPattern wraps a search.InvalidPatternError.
func invalidPattern(err error) *toolError {
	var ip *search.InvalidPatternError
	if errors.As(err, &ip) {
		return &toolError{Code: "INVALID_PATTERN", Message: ip.Err.Error()}
	}
	return asToolError(err)
}
