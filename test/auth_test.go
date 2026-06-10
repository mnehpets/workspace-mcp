package test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mnehpets/http/endpoint"
	"github.com/mnehpets/workspace-mcp/mcp"
)

const testToken = "0123456789abcdef0123456789abcdef" // 32 bytes

// okEndpoint is a trivial protected endpoint returning 200.
func okEndpoint(w http.ResponseWriter, r *http.Request, _ struct{}) (endpoint.Renderer, error) {
	return endpoint.RendererFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("ok"))
		return err
	}), nil
}

func protectedHandler(log *mcp.Logger) http.Handler {
	return endpoint.Handler(okEndpoint, mcp.NewBearer([]string{testToken}, log, false))
}

func TestBearerMissingToken(t *testing.T) {
	h := protectedHandler(nil)
	req := httptest.NewRequest("POST", "/mcp", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestBearerWrongToken(t *testing.T) {
	h := protectedHandler(nil)
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer not-the-right-token-aaaaaaaaaaaaa")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestBearerValidToken(t *testing.T) {
	h := protectedHandler(nil)
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
}

// During rotation the server accepts any configured token: both an old and a new
// token are valid, while an unrelated token is still rejected.
func TestBearerMultipleTokensRotation(t *testing.T) {
	const oldToken = "0000000000000000oldoldoldoldoldo" // 32 bytes
	const newToken = "1111111111111111newnewnewnewnewn" // 32 bytes
	h := endpoint.Handler(okEndpoint, mcp.NewBearer([]string{oldToken, newToken}, nil, false))

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"old still valid", oldToken, http.StatusOK},
		{"new valid", newToken, http.StatusOK},
		{"unrelated rejected", "2222222222222222badbadbadbadbadb", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/mcp", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("want %d, got %d", tc.want, rec.Code)
			}
		})
	}
}

// An empty token set rejects everything (no token can match).
func TestBearerNoTokensRejectsAll(t *testing.T) {
	h := endpoint.Handler(okEndpoint, mcp.NewBearer(nil, nil, false))
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 with no configured tokens, got %d", rec.Code)
	}
}

// The token must never appear in the audit log, on success or failure.
func TestBearerTokenNeverLogged(t *testing.T) {	var buf bytes.Buffer
	log := mcp.NewLogger("info", &buf)
	h := protectedHandler(log)

	for _, tok := range []string{testToken, "wrong-but-secret-token-zzzzzzzzzz"} {
		req := httptest.NewRequest("POST", "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if strings.Contains(buf.String(), testToken) || strings.Contains(buf.String(), "secret-token") {
		t.Fatalf("token leaked into audit log:\n%s", buf.String())
	}
}

// The audit log's "remote" field comes from X-Forwarded-For only when the server
// is configured to trust it (the zrok case, where RemoteAddr is an opaque overlay
// string). On the untrusted path (ngrok/direct) a client-supplied X-Forwarded-For
// must be ignored in favor of the real RemoteAddr, so a client cannot spoof its
// logged identity.
func TestBearerForwardedForLogging(t *testing.T) {
	logged := func(trustXFF bool, xff string) string {
		var buf bytes.Buffer
		log := mcp.NewLogger("info", &buf)
		h := endpoint.Handler(okEndpoint, mcp.NewBearer([]string{testToken}, log, trustXFF))
		req := httptest.NewRequest("POST", "/mcp", nil)
		req.RemoteAddr = "203.0.113.9:5555" // the real (or overlay) peer
		req.Header.Set("Authorization", "Bearer "+testToken)
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		return buf.String()
	}

	// Trusted: the first XFF entry (the originating client) is logged, not the
	// overlay RemoteAddr or any intermediate proxy hop.
	if out := logged(true, "198.51.100.7:4444, 10.0.0.1:80"); !strings.Contains(out, "198.51.100.7:4444") {
		t.Errorf("trustXFF: want client from X-Forwarded-For in log, got:\n%s", out)
	}
	// Trusted but no header present: fall back to RemoteAddr rather than blank.
	if out := logged(true, ""); !strings.Contains(out, "203.0.113.9:5555") {
		t.Errorf("trustXFF, no header: want RemoteAddr fallback, got:\n%s", out)
	}
	// Untrusted: a client-supplied X-Forwarded-For is ignored; RemoteAddr wins.
	out := logged(false, "1.2.3.4:9999")
	if !strings.Contains(out, "203.0.113.9:5555") {
		t.Errorf("untrusted: want RemoteAddr in log, got:\n%s", out)
	}
	if strings.Contains(out, "1.2.3.4") {
		t.Errorf("untrusted: spoofable X-Forwarded-For must not be logged, got:\n%s", out)
	}
}
