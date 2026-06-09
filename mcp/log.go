// Log provides a structured logger for the server. It is designed so
// that secrets (the bearer token) and file contents are never written to the
// log: there is simply no code path that passes either into it.
package mcp

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mnehpets/http/endpoint"
)

// Logger wraps slog with audit-specific helpers.
type Logger struct {
	slog *slog.Logger
}

// NewLogger builds a Logger writing to w at the given level ("debug"|"info"|"warn"|
// "error"; anything else defaults to info).
func NewLogger(level string, w io.Writer) *Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})
	return &Logger{slog: slog.New(h)}
}

// Slog returns the underlying slog.Logger for general (non-audit) messages.
func (a *Logger) Slog() *slog.Logger { return a.slog }

// ToolEvent is one audited tool invocation. It deliberately carries no file
// contents and no token — only metadata about the decision.
type ToolEvent struct {
	Method    string
	Tool      string
	Workspace string
	Paths     []string
	Allowed   bool
	Reason    string
	Bytes     int
	Matches   int
	Err       string
}

// ToolCall records a tool invocation outcome.
func (a *Logger) ToolCall(e ToolEvent) {
	a.slog.Info("tool_call",
		"method", e.Method,
		"tool", e.Tool,
		"workspace", e.Workspace,
		"paths", e.Paths,
		"allowed", e.Allowed,
		"reason", e.Reason,
		"bytes", e.Bytes,
		"matches", e.Matches,
		"err", e.Err,
	)
}

// Auth records an authentication outcome. The token is never an argument here.
func (a *Logger) Auth(allowed bool, remote string) {
	a.slog.Info("auth", "allowed", allowed, "remote", remote)
}

// Process implements endpoint.Processor, logging each request's method, path,
// status code, and elapsed duration.
func (l *Logger) Process(w http.ResponseWriter, r *http.Request,
	next func(http.ResponseWriter, *http.Request) (endpoint.Renderer, error),
) (endpoint.Renderer, error) {
	start := time.Now()
	method, path := r.Method, r.URL.Path
	rend, err := next(w, r)
	if err != nil {
		l.slog.Info("request", "method", method, "path", path,
			"status", http.StatusInternalServerError,
			"duration", time.Since(start).Round(time.Millisecond).String())
		return rend, err
	}
	return endpoint.RendererFunc(func(w http.ResponseWriter, r *http.Request) error {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		rerr := rend.Render(rec, r)
		l.slog.Info("request", "method", method, "path", path,
			"status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond).String())
		return rerr
	}), nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
