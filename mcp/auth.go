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
// On failure it returns a 401 with no hint as to whether the token was missing or
// wrong. Tokens are never logged.
type Bearer struct {
	expected [][32]byte
	log      *Logger
}

// NewBearer builds a Bearer processor accepting any of the given tokens. With an
// empty slice every request is rejected (no token can match).
func NewBearer(tokens []string, log *Logger) *Bearer {
	exp := make([][32]byte, len(tokens))
	for i, t := range tokens {
		exp[i] = sha256.Sum256([]byte(t))
	}
	return &Bearer{expected: exp, log: log}
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
	ok := token != "" && match == 1
	if b.log != nil {
		b.log.Auth(ok, r.RemoteAddr)
	}
	if !ok {
		return nil, endpoint.Error(http.StatusUnauthorized, "unauthorized", nil)
	}
	return next(w, r)
}

func extractBearer(header string) string {
	const prefix = "Bearer "
	if len(header) >= len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}
