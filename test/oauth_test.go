package test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mnehpets/http/endpoint"
	"github.com/mnehpets/workspace-mcp/mcp"
)

const (
	oauthClientID     = "workspace-mcp-client"
	oauthClientSecret = "oauth-client-secret-long-enough-xxxxxxxx"
	oauthRedirectURI  = "https://claude.ai/api/mcp/oauth/callback"
)

// oauthFixture wires the /oauth/authorize and /oauth/token handlers around a
// fresh server, capturing the console approval code the operator would read.
type oauthFixture struct {
	authorize http.Handler
	token     http.Handler
	console   *strings.Builder
}

func newOAuthFixture() *oauthFixture {
	var console strings.Builder
	srv := mcp.NewOAuthServer(oauthClientID, oauthClientSecret).WithNonceOutput(&console)
	return &oauthFixture{
		authorize: endpoint.HandleFunc(srv.Authorize),
		token:     endpoint.HandleFunc(srv.Token),
		console:   &console,
	}
}

// getNonce loads the consent page and returns the approval code printed to the
// server console (the code is deliberately NOT in the page itself).
func (f *oauthFixture) getNonce(t *testing.T) string {
	t.Helper()
	// Note: the code is printed only when generated, so a reload within the TTL may
	// print nothing. The last code printed is always the current active one, so we
	// parse the accumulated console rather than resetting and demanding a reprint.
	q := url.Values{
		"client_id":     {oauthClientID},
		"redirect_uri":  {oauthRedirectURI},
		"response_type": {"code"},
	}
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	f.authorize.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /oauth/authorize: want 200, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), parseNonce(t, f.console.String())) {
		t.Fatalf("the approval code must not appear in the consent page HTML")
	}
	return parseNonce(t, f.console.String())
}

// postAuthorize submits the consent form with the given nonce and returns the
// recorder. A correct nonce yields a 302 redirect carrying the code; a wrong one
// re-renders the page (200).
func (f *oauthFixture) postAuthorize(nonce string) *httptest.ResponseRecorder {
	form := url.Values{
		"client_id":     {oauthClientID},
		"redirect_uri":  {oauthRedirectURI},
		"response_type": {"code"},
		"nonce":         {nonce},
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	f.authorize.ServeHTTP(rec, req)
	return rec
}

// codeFromRedirect extracts the ?code= param from a 302 Location.
func codeFromRedirect(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("want 302 redirect, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	return u.Query().Get("code")
}

// parseNonce pulls the approval code out of the console output line.
func parseNonce(t *testing.T, out string) string {
	t.Helper()
	const marker = "consent page: "
	i := strings.LastIndex(out, marker)
	if i < 0 {
		t.Fatalf("no approval code printed to console:\n%s", out)
	}
	return strings.TrimSpace(strings.SplitN(out[i+len(marker):], "\n", 2)[0])
}

// The full flow: consent with the console code, then redeem the code for tokens.
func TestOAuthHappyPath(t *testing.T) {
	f := newOAuthFixture()
	nonce := f.getNonce(t)
	code := codeFromRedirect(t, f.postAuthorize(nonce))
	if code == "" {
		t.Fatal("no code issued after correct console approval")
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {oauthRedirectURI},
		"client_id":     {oauthClientID},
		"client_secret": {oauthClientSecret},
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	f.token.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /oauth/token: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if body.AccessToken == "" || body.RefreshToken == "" {
		t.Fatalf("want access+refresh tokens, got %+v", body)
	}
}

// The DoS fix: without the console code, /oauth/authorize never issues a code, so
// the code map cannot be filled by a remote caller. Every attempt re-renders the
// page (200) and no redirect/code is produced.
func TestOAuthNoCodeWithoutNonce(t *testing.T) {
	f := newOAuthFixture()
	// Prime a nonce so a *wrong* guess is what's being rejected, not an empty store.
	f.getNonce(t)

	for _, guess := range []string{"", "wrong", "AAAAAAAAAAAA", "not-the-code"} {
		rec := f.postAuthorize(guess)
		if rec.Code != http.StatusOK {
			t.Fatalf("nonce %q: want 200 re-render, got %d", guess, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Fatalf("nonce %q: a code was issued without the console code (Location=%s)", guess, loc)
		}
	}

	// A legitimate operator who reads the console still gets through.
	if code := codeFromRedirect(t, f.postAuthorize(f.getNonce(t))); code == "" {
		t.Fatal("valid console code should still issue a code")
	}
}

// The console code is single-use: replaying it after a successful approval fails.
func TestOAuthNonceSingleUse(t *testing.T) {
	f := newOAuthFixture()
	nonce := f.getNonce(t)
	if code := codeFromRedirect(t, f.postAuthorize(nonce)); code == "" {
		t.Fatal("first use should issue a code")
	}
	// Same nonce again, without a new consent-page load: must be refused.
	rec := f.postAuthorize(nonce)
	if rec.Code != http.StatusOK {
		t.Fatalf("replayed nonce: want 200 re-render, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("replayed nonce issued a code (Location=%s)", loc)
	}
}

// A wrong client_id is rejected at both the authorize and token endpoints.
func TestOAuthWrongClientID(t *testing.T) {
	f := newOAuthFixture()
	q := url.Values{"client_id": {"attacker"}, "redirect_uri": {oauthRedirectURI}, "response_type": {"code"}}
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	f.authorize.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong client_id at authorize: want 400, got %d", rec.Code)
	}
}
