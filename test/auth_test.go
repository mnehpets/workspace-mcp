package test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mnehpets/http/endpoint"
	"github.com/mnehpets/workspace-mcp/audit"
	"github.com/mnehpets/workspace-mcp/auth"
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

func protectedHandler(log *audit.Logger) http.Handler {
	return endpoint.Handler(okEndpoint, auth.NewBearer([]string{testToken}, log))
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
	h := endpoint.Handler(okEndpoint, auth.NewBearer([]string{oldToken, newToken}, nil))

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
	h := endpoint.Handler(okEndpoint, auth.NewBearer(nil, nil))
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 with no configured tokens, got %d", rec.Code)
	}
}

// The token must never appear in the audit log, on success or failure.
func TestBearerTokenNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	log := audit.New("info", &buf)
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
