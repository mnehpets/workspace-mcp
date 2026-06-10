// Bearer-token authentication for the mnehpets/http endpoint chain.
// Authentication is server-wide (AuthN); per-workspace policy is AuthZ.
package mcp

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/mnehpets/http/endpoint"
)

// Bearer is an endpoint.Processor that requires a valid bearer token. It accepts
// any one of a set of expected tokens — so an old and a new token can both be
// valid during an overlap window, enabling zero-lockstep rotation. Each presented
// token is compared (as a SHA-256 digest) against every expected one in constant
// time, so neither token content/length nor which token matched leaks via timing.
// An optional extra validator (e.g. an OAuth token checker) is consulted when no
// static token matches. On failure it returns 401 with no hint as to what failed.
// Tokens are never logged.
type Bearer struct {
	expected         [][32]byte
	extra            func(string) bool
	log              *Logger
	resourceMetadata bool // emit a WWW-Authenticate resource_metadata hint on 401
	trustXFF         bool // log the client from X-Forwarded-For (set only behind a trusted tunnel that injects it, e.g. zrok)
}

// NewBearer builds a Bearer processor accepting any of the given tokens. With an
// empty slice every request is rejected unless an extra validator is set.
// trustXFF should be true only when the server sits behind a tunnel/proxy that
// terminates the client connection and injects X-Forwarded-For (zrok), where
// RemoteAddr is otherwise an opaque transport string; it must be false for the
// ngrok and direct-TCP paths, where RemoteAddr is the real, unspoofable peer.
func NewBearer(tokens []string, log *Logger, trustXFF bool) *Bearer {
	exp := make([][32]byte, len(tokens))
	for i, t := range tokens {
		exp[i] = sha256.Sum256([]byte(t))
	}
	return &Bearer{expected: exp, log: log, trustXFF: trustXFF}
}

// WithExtra adds a dynamic token validator (e.g. OAuth access token check) that
// is consulted when no static token matches.
func (b *Bearer) WithExtra(fn func(string) bool) *Bearer {
	b.extra = fn
	return b
}

// WithResourceMetadata makes a 401 carry a WWW-Authenticate header pointing at
// this endpoint's protected-resource metadata (RFC 9728 / MCP 2025-11-25), the
// standard OAuth-discovery trigger. Enable it only when an OAuth authorization
// server is configured (otherwise the advertised metadata URL would 404).
func (b *Bearer) WithResourceMetadata() *Bearer {
	b.resourceMetadata = true
	return b
}

// Process implements endpoint.Processor.
func (b *Bearer) Process(w http.ResponseWriter, r *http.Request, next func(http.ResponseWriter, *http.Request) (endpoint.Renderer, error)) (endpoint.Renderer, error) {
	token := extractBearer(r.Header.Get("Authorization"))
	got := sha256.Sum256([]byte(token))
	// Compare against all expected tokens without short-circuiting, so the time
	// taken does not reveal which (if any) matched.
	match := 0
	for i := range b.expected {
		match |= subtle.ConstantTimeCompare(got[:], b.expected[i][:])
	}
	if match == 0 && b.extra != nil && token != "" {
		if b.extra(token) {
			match = 1
		}
	}
	ok := token != "" && match == 1
	if b.log != nil {
		b.log.Auth(ok, b.clientAddr(r))
	}
	if !ok {
		if b.resourceMetadata {
			// RFC 9728: the protected-resource metadata URL for a resource at path
			// /mcp/<name> is /.well-known/oauth-protected-resource/mcp/<name>. The
			// request path already carries that suffix, so reuse it directly.
			md := "https://" + r.Host + "/.well-known/oauth-protected-resource" + r.URL.Path
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+md+`"`)
		}
		return nil, endpoint.Error(http.StatusUnauthorized, "unauthorized", nil)
	}
	return next(w, r)
}

// clientAddr returns the best client address for audit logging. Normally that
// is the real TCP peer (r.RemoteAddr), which the client cannot forge. But when
// the server runs behind a tunnel that terminates the client connection
// elsewhere and forwards over an overlay — zrok/ziti, where RemoteAddr is an
// opaque "ziti-edge-router connId=..." string with no client IP in it — the only
// place the originating client survives is the frontend-injected
// X-Forwarded-For header. trustXFF is set (at wiring time, by the zrok serve
// path) only for that case, so a client on the ngrok/direct path cannot spoof
// its logged identity via a forged header. We take the first XFF entry (the
// originating client); a missing header falls back to RemoteAddr rather than
// inventing a value, which also surfaces a wrong header-name assumption instead
// of hiding it.
func (b *Bearer) clientAddr(r *http.Request) string {
	if b.trustXFF {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	return r.RemoteAddr
}

func extractBearer(header string) string {
	const prefix = "Bearer "
	if len(header) >= len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}
