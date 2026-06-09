// oauth implements a minimal OAuth 2.0 authorization code server for
// authenticating claude.ai as an MCP connector.
//
// Access and refresh tokens are self-contained AEAD-encrypted blobs
// (ChaCha20-Poly1305) with keys derived via HKDF from the client secret using
// distinct info strings — so the token types are cryptographically
// non-interchangeable. Auth codes are kept in-memory because they are
// single-use: we must delete each code on first redemption to prevent replays.
package mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/mnehpets/http/endpoint"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	oauthCodeTTL    = 2 * time.Minute
	oauthTokenTTL   = time.Hour
	oauthRefreshTTL = 30 * 24 * time.Hour
)

// expiryBytes is the size of a token's payload: the expiry as a big-endian
// uint64 Unix timestamp (8 bytes).
const expiryBytes = 8

// tokenCodec issues and validates self-contained AEAD tokens.
// The payload is the expiry as a big-endian Unix timestamp.
type tokenCodec struct {
	key [chacha20poly1305.KeySize]byte
}

func newTokenCodec(clientSecret, info string) tokenCodec {
	r := hkdf.New(sha256.New, []byte(clientSecret), nil, []byte(info))
	var key [chacha20poly1305.KeySize]byte
	if _, err := io.ReadFull(r, key[:]); err != nil {
		panic(err) // only fails if KeySize is wrong
	}
	return tokenCodec{key: key}
}

func (c tokenCodec) issue(expiry time.Time) (string, error) {
	aead, err := chacha20poly1305.NewX(c.key[:])
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload := binary.BigEndian.AppendUint64(nil, uint64(expiry.Unix()))
	ct := aead.Seal(nonce, nonce, payload, nil)
	return base64.RawURLEncoding.EncodeToString(ct), nil
}

func (c tokenCodec) validate(token string) bool {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return false
	}
	aead, err := chacha20poly1305.NewX(c.key[:])
	if err != nil {
		return false
	}
	if len(b) < aead.NonceSize()+aead.Overhead() {
		return false
	}
	nonce, ct := b[:aead.NonceSize()], b[aead.NonceSize():]
	payload, err := aead.Open(nil, nonce, ct, nil)
	if err != nil || len(payload) != expiryBytes {
		return false
	}
	expiry := int64(binary.BigEndian.Uint64(payload))
	return time.Now().Unix() < expiry
}

type authCode struct {
	redirectURI   string
	expiry        time.Time
	codeChallenge string // PKCE S256: BASE64URL(SHA256(verifier)); empty = no PKCE
}

// OAuthServer implements the OAuth 2.0 authorization code flow for a single
// registered client. It is safe for concurrent use.
type OAuthServer struct {
	clientID         string
	clientSecretHash [32]byte   // SHA-256 of client secret for constant-time comparison
	codec            tokenCodec // access tokens
	refreshCodec     tokenCodec // refresh tokens — distinct HKDF info, non-interchangeable
	mu               sync.Mutex
	codes            map[string]authCode
}

// NewOAuthServer creates an OAuthServer. clientSecret derives the AEAD keys for
// both token types and is hashed for validation at the token endpoint.
func NewOAuthServer(clientID, clientSecret string) *OAuthServer {
	return &OAuthServer{
		clientID:         clientID,
		clientSecretHash: sha256.Sum256([]byte(clientSecret)),
		codec:            newTokenCodec(clientSecret, "workspace-mcp oauth token key"),
		refreshCodec:     newTokenCodec(clientSecret, "workspace-mcp oauth refresh token key"),
		codes:            make(map[string]authCode),
	}
}

// CheckToken reports whether token is a valid, unexpired access token.
func (s *OAuthServer) CheckToken(token string) bool {
	return s.codec.validate(token)
}

// WellKnownAuthServer is an endpoint.EndpointFunc[struct{}] for
// GET /.well-known/oauth-authorization-server (RFC 8414).
func (s *OAuthServer) WellKnownAuthServer(_ http.ResponseWriter, r *http.Request, _ struct{}) (endpoint.Renderer, error) {
	base := "https://" + r.Host
	return &endpoint.JSONRenderer{Value: map[string]any{
		"issuer":                           base,
		"authorization_endpoint":           base + "/oauth/authorize",
		"token_endpoint":                   base + "/oauth/token",
		"response_types_supported":         []string{"code"},
		"grant_types_supported":            []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported": []string{"S256"},
	}}, nil
}

// WellKnownProtectedResource is an endpoint.EndpointFunc[struct{}] for
// GET /.well-known/oauth-protected-resource (RFC 9728).
func (s *OAuthServer) WellKnownProtectedResource(_ http.ResponseWriter, r *http.Request, _ struct{}) (endpoint.Renderer, error) {
	base := "https://" + r.Host
	return &endpoint.JSONRenderer{Value: map[string]any{
		"resource":              base + "/mcp",
		"authorization_servers": []string{base},
	}}, nil
}

// authorizeParams decodes both GET (query) and POST (form) for the authorize endpoint.
type authorizeParams struct {
	ResponseType        string `query:"response_type" form:"response_type"`
	ClientID            string `query:"client_id" form:"client_id"`
	RedirectURI         string `query:"redirect_uri" form:"redirect_uri"`
	State               string `query:"state" form:"state"`
	Scope               string `query:"scope" form:"scope"`
	CodeChallenge       string `query:"code_challenge" form:"code_challenge"`
	CodeChallengeMethod string `query:"code_challenge_method" form:"code_challenge_method"`
}

// Authorize is an endpoint.EndpointFunc[authorizeParams] for /oauth/authorize:
// GET shows the approve page; POST issues the auth code.
func (s *OAuthServer) Authorize(w http.ResponseWriter, r *http.Request, p authorizeParams) (endpoint.Renderer, error) {
	switch r.Method {
	case http.MethodGet:
		return s.showApprovePage(p)
	case http.MethodPost:
		return s.issueCode(p)
	default:
		return nil, endpoint.Error(http.StatusMethodNotAllowed, "method not allowed", nil)
	}
}

func (s *OAuthServer) showApprovePage(p authorizeParams) (endpoint.Renderer, error) {
	if p.ClientID != s.clientID {
		return nil, endpoint.Error(http.StatusBadRequest, "unknown client_id", nil)
	}
	if p.RedirectURI == "" {
		return nil, endpoint.Error(http.StatusBadRequest, "redirect_uri required", nil)
	}
	if p.ResponseType != "code" {
		return oauthRedirectError(p.RedirectURI, "unsupported_response_type", p.State)
	}
	if p.CodeChallengeMethod != "" && p.CodeChallengeMethod != "S256" {
		return oauthRedirectError(p.RedirectURI, "invalid_request", p.State)
	}
	return &endpoint.HTMLTemplateRenderer{
		Template: approveTemplate,
		Values: approveData{
			ClientID:      p.ClientID,
			RedirectURI:   p.RedirectURI,
			State:         p.State,
			ResponseType:  p.ResponseType,
			Scope:         p.Scope,
			CodeChallenge: p.CodeChallenge,
		},
	}, nil
}

func (s *OAuthServer) issueCode(p authorizeParams) (endpoint.Renderer, error) {
	if p.ClientID != s.clientID {
		return nil, endpoint.Error(http.StatusBadRequest, "unknown client_id", nil)
	}
	if p.RedirectURI == "" {
		return nil, endpoint.Error(http.StatusBadRequest, "redirect_uri required", nil)
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, endpoint.Error(http.StatusInternalServerError, "server error", err)
	}
	code := base64.RawURLEncoding.EncodeToString(raw[:])

	s.mu.Lock()
	s.codes[code] = authCode{
		redirectURI:   p.RedirectURI,
		expiry:        time.Now().Add(oauthCodeTTL),
		codeChallenge: p.CodeChallenge,
	}
	s.mu.Unlock()

	target, err := url.Parse(p.RedirectURI)
	if err != nil {
		return nil, endpoint.Error(http.StatusBadRequest, "invalid redirect_uri", err)
	}
	q := target.Query()
	q.Set("code", code)
	if p.State != "" {
		q.Set("state", p.State)
	}
	target.RawQuery = q.Encode()
	return &endpoint.RedirectRenderer{URL: target.String(), Status: http.StatusFound}, nil
}

// tokenParams decodes the POST /oauth/token form body.
type tokenParams struct {
	GrantType    string `form:"grant_type"`
	Code         string `form:"code"`
	RedirectURI  string `form:"redirect_uri"`
	ClientID     string `form:"client_id"`
	ClientSecret string `form:"client_secret"`
	CodeVerifier string `form:"code_verifier"` // PKCE
	RefreshToken string `form:"refresh_token"`
}

// Token is an endpoint.EndpointFunc[tokenParams] for POST /oauth/token.
func (s *OAuthServer) Token(_ http.ResponseWriter, _ *http.Request, p tokenParams) (endpoint.Renderer, error) {
	switch p.GrantType {
	case "authorization_code":
		return s.authCodeGrant(p)
	case "refresh_token":
		return s.refreshGrant(p)
	default:
		return oauthErrorRenderer(http.StatusBadRequest, "unsupported_grant_type", "supported: authorization_code, refresh_token")
	}
}

// validateClient checks client_id and client_secret via constant-time comparison.
func (s *OAuthServer) validateClient(p tokenParams) (endpoint.Renderer, bool) {
	idOK := p.ClientID == s.clientID
	got := sha256.Sum256([]byte(p.ClientSecret))
	secretOK := subtle.ConstantTimeCompare(got[:], s.clientSecretHash[:]) == 1
	if !idOK || !secretOK {
		r, _ := oauthErrorRenderer(http.StatusUnauthorized, "invalid_client", "invalid client credentials")
		return r, false
	}
	return nil, true
}

func (s *OAuthServer) authCodeGrant(p tokenParams) (endpoint.Renderer, error) {
	if errR, ok := s.validateClient(p); !ok {
		return errR, nil
	}
	s.mu.Lock()
	ac, ok := s.codes[p.Code]
	if ok {
		delete(s.codes, p.Code) // single-use
	}
	s.mu.Unlock()

	if !ok || time.Now().After(ac.expiry) {
		return oauthErrorRenderer(http.StatusBadRequest, "invalid_grant", "code not found or expired")
	}
	if ac.redirectURI != p.RedirectURI {
		return oauthErrorRenderer(http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
	}
	if ac.codeChallenge != "" {
		h := sha256.Sum256([]byte(p.CodeVerifier))
		if base64.RawURLEncoding.EncodeToString(h[:]) != ac.codeChallenge {
			return oauthErrorRenderer(http.StatusBadRequest, "invalid_grant", "code_verifier mismatch")
		}
	}
	return s.issueTokenPair()
}

func (s *OAuthServer) refreshGrant(p tokenParams) (endpoint.Renderer, error) {
	if errR, ok := s.validateClient(p); !ok {
		return errR, nil
	}
	if !s.refreshCodec.validate(p.RefreshToken) {
		return oauthErrorRenderer(http.StatusBadRequest, "invalid_grant", "refresh token invalid or expired")
	}
	return s.issueTokenPair()
}

func (s *OAuthServer) issueTokenPair() (endpoint.Renderer, error) {
	access, err := s.codec.issue(time.Now().Add(oauthTokenTTL))
	if err != nil {
		return nil, endpoint.Error(http.StatusInternalServerError, "server error", err)
	}
	refresh, err := s.refreshCodec.issue(time.Now().Add(oauthRefreshTTL))
	if err != nil {
		return nil, endpoint.Error(http.StatusInternalServerError, "server error", err)
	}
	body := map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(oauthTokenTTL.Seconds()),
		"refresh_token": refresh,
	}
	return endpoint.RendererFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Cache-Control", "no-store")
		return (&endpoint.JSONRenderer{Value: body}).Render(w, r)
	}), nil
}

func oauthErrorRenderer(status int, code, desc string) (endpoint.Renderer, error) {
	return &endpoint.JSONRenderer{
		Status: status,
		Value:  map[string]string{"error": code, "error_description": desc},
	}, nil
}

func oauthRedirectError(redirectURI, errCode, state string) (endpoint.Renderer, error) {
	target, err := url.Parse(redirectURI)
	if err != nil {
		return nil, endpoint.Error(http.StatusBadRequest, "invalid redirect_uri", nil)
	}
	q := target.Query()
	q.Set("error", errCode)
	if state != "" {
		q.Set("state", state)
	}
	target.RawQuery = q.Encode()
	return &endpoint.RedirectRenderer{URL: target.String(), Status: http.StatusFound}, nil
}

type approveData struct {
	ClientID      string
	RedirectURI   string
	State         string
	ResponseType  string
	Scope         string
	CodeChallenge string
}

var approveTemplate = template.Must(template.New("approve").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Authorize {{.ClientID}}</title>
<style>
*{box-sizing:border-box}
body{font-family:system-ui,sans-serif;max-width:420px;margin:80px auto;padding:0 24px;color:#111}
h1{font-size:1.15rem;margin-bottom:.4rem}
p{color:#555;font-size:.9rem;margin:.4rem 0 1.5rem}
button{background:#18181b;color:#fff;border:none;padding:10px 22px;border-radius:7px;cursor:pointer;font-size:.95rem;font-weight:500}
button:hover{background:#27272a}
</style>
</head>
<body>
<h1>Authorize <strong>{{.ClientID}}</strong></h1>
<p>This grants read-only access to your workspace-mcp server.</p>
<form method="POST" action="/oauth/authorize">
<input type="hidden" name="client_id" value="{{.ClientID}}">
<input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
<input type="hidden" name="state" value="{{.State}}">
<input type="hidden" name="response_type" value="{{.ResponseType}}">
<input type="hidden" name="scope" value="{{.Scope}}">
<input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
<button type="submit">Authorize</button>
</form>
</body>
</html>`))
