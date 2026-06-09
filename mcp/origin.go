// Origin-header validation for the Streamable-HTTP transport: the MCP 2025-11-25
// revision makes the DNS-rebinding defense explicit — a request bearing an
// Origin the server does not allow must be rejected with 403. This sits ahead of
// bearer auth on the /mcp routes (it is a transport-layer guard, independent of
// AuthN/AuthZ), and matters most on the ngrok-fronted path.
package mcp

import (
	"net/http"

	"github.com/mnehpets/http/endpoint"
)

// OriginCheck is an endpoint.Processor enforcing an Origin allowlist. A request
// carrying an Origin header that is not allowed is refused with 403 before any
// other processing. A request with NO Origin header is allowed: Origin is a
// browser-set header, so its absence is not a rebinding vector — and legitimate
// MCP traffic to this server is Origin-less (the claude.ai cloud connector, curl,
// the MCP Inspector are server-side/non-browser clients, not browsers). The MCP
// spec mandates that *servers* validate Origin but never defines what value a
// client sends, so we bake in no guessed default: an empty allowlist accepts the
// Origin-less legitimate traffic and rejects ANY present Origin (which can only be
// a browser — i.e. the exact DNS-rebinding case the check exists for). Configure
// allowedOrigins explicitly to permit a specific browser origin (e.g. local dev),
// or "*" to disable the check.
type OriginCheck struct {
	allowed  map[string]bool
	allowAny bool // "*" in the config disables the check (local dev / trusted edge)
	log      *Logger
}

// NewOriginCheck builds an Origin processor from an allowlist. An empty `origins`
// allows only Origin-less requests (rejecting any present Origin); a list
// containing "*" allows any origin.
func NewOriginCheck(origins []string, log *Logger) *OriginCheck {
	c := &OriginCheck{allowed: make(map[string]bool, len(origins)), log: log}
	for _, o := range origins {
		if o == "*" {
			c.allowAny = true
		}
		c.allowed[o] = true
	}
	return c
}

// Process implements endpoint.Processor.
func (c *OriginCheck) Process(w http.ResponseWriter, r *http.Request, next func(http.ResponseWriter, *http.Request) (endpoint.Renderer, error)) (endpoint.Renderer, error) {
	origin := r.Header.Get("Origin")
	if origin != "" && !c.allowAny && !c.allowed[origin] {
		if c.log != nil {
			c.log.Slog().Warn("rejected origin", "origin", origin, "remote", r.RemoteAddr)
		}
		return nil, endpoint.Error(http.StatusForbidden, "forbidden origin", nil)
	}
	return next(w, r)
}
