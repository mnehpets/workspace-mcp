package test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mnehpets/workspace-mcp/mcp"
)

// Task 17: workspace selection is by route. A connector pointed at /mcp/<name>
// reaches exactly that workspace, an unknown segment 404s, and no tool carries a
// `workspace` argument.

// TestRouteSelectsWorkspace confirms each per-workspace endpoint is bound to its
// own tree: the same tool call returns that workspace's identity, regardless of
// any (now-ignored) "workspace" field in the arguments.
func TestRouteSelectsWorkspace(t *testing.T) {
	reg, _, _ := twoWorkspaceRegistry(t)
	f := newMCPFixture(t, reg)

	for _, name := range []string{"default", "notes"} {
		var wi struct {
			Name string `json:"name"`
		}
		// Pass a misleading "workspace" arg; the route, not the arg, must win.
		f.callToolWS(t, name, "workspace_info", map[string]any{"workspace": "bogus"}, &wi)
		if wi.Name != name {
			t.Errorf("endpoint /mcp/%s returned workspace %q", name, wi.Name)
		}
	}
}

// TestWorkspaceInfoInlinesPreview confirms workspace_info inlines a capped
// preview of the highest-priority well-known file (saving a file_read round-trip),
// and omits it when there is none.
func TestWorkspaceInfoInlinesPreview(t *testing.T) {
	reg, _, _ := twoWorkspaceRegistry(t)
	// Seed default's root with a README so it becomes the top well-known file.
	def, _ := reg.Get("default")
	if err := os.WriteFile(filepath.Join(def.Root.Dir(), "README.md"), []byte("# Title\nhello preview\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Rebuild so orientation is re-detected with the new README.
	reg.Close()
	reg2, err := mcp.Build(&mcp.Config{Workspaces: []mcp.WorkspaceConfig{
		{Name: "default", Root: def.Root.Dir(), Policy: mcp.PolicyConfig{AllowGlobs: []string{"**/*.md"}}, Read: mcp.ReadConfig{MaxBytes: 1000}},
		{Name: "bare", Root: t.TempDir(), Policy: mcp.PolicyConfig{AllowGlobs: []string{"**/*.md"}}, Read: mcp.ReadConfig{MaxBytes: 1000}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg2.Close)
	f := newMCPFixture(t, reg2)

	var wi struct {
		Preview *struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		} `json:"preview"`
	}
	f.callTool(t, "workspace_info", map[string]any{}, &wi)
	if wi.Preview == nil {
		t.Fatal("expected a preview for a workspace with a README")
	}
	if wi.Preview.Path != "README.md" || !strings.Contains(wi.Preview.Content, "hello preview") {
		t.Errorf("preview wrong: %+v", wi.Preview)
	}

	// A workspace with no well-known files → no preview.
	var bare struct {
		Preview any `json:"preview"`
	}
	f.callToolWS(t, "bare", "workspace_info", map[string]any{}, &bare)
	if bare.Preview != nil {
		t.Errorf("expected no preview for a bare workspace, got %+v", bare.Preview)
	}
}

// TestUnknownWorkspaceRoute404 confirms an unconfigured segment is a routing
// miss, not a domain error (the UNKNOWN_WORKSPACE path is gone).
func TestUnknownWorkspaceRoute404(t *testing.T) {
	reg, _, _ := twoWorkspaceRegistry(t)
	f := newMCPFixture(t, reg)
	if code := f.statusFor(t, f.wsURL("ghost")); code != http.StatusNotFound {
		t.Fatalf("unknown workspace route should 404, got %d", code)
	}
}

// TestInstructionsAreWorkspaceSpecific confirms the initialize instructions fold
// in the endpoint's own workspace description.
func TestInstructionsAreWorkspaceSpecific(t *testing.T) {
	reg, _, _ := twoWorkspaceRegistry(t)
	// Override default's description so we can assert it appears verbatim.
	def, _ := reg.Get("default")
	def.Description = "the marker description for default"
	f := newMCPFixture(t, reg)

	rr := f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	var res struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(rr.Result, &res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Instructions, "the marker description for default") {
		t.Errorf("instructions should include this workspace's description, got:\n%s", res.Instructions)
	}

	// workspace_info mirrors the connect-time instructions: its `orientation`
	// string must be byte-for-byte the same payload.
	var wi struct {
		Orientation string `json:"orientation"`
	}
	f.callTool(t, "workspace_info", map[string]any{}, &wi)
	if wi.Orientation != res.Instructions {
		t.Errorf("workspace_info.orientation should equal the initialize instructions:\n got: %q\nwant: %q", wi.Orientation, res.Instructions)
	}
}

// TestWWWAuthenticateOnOAuth401 confirms that, with OAuth configured, a 401
// carries an RFC 9728 WWW-Authenticate header pointing at this endpoint's
// protected-resource metadata; without OAuth there is no such header.
func TestWWWAuthenticateOnOAuth401(t *testing.T) {
	reg, _, _ := twoWorkspaceRegistry(t)
	log := mcp.NewLogger("error", &bytes.Buffer{})

	// With OAuth: header present and resource-scoped to the endpoint path.
	oauth := mcp.NewOAuthServer("client", "supersecretsupersecretsupersecret")
	h := mcp.BuildHandler(reg, log, []string{"tok"}, oauth, nil)
	ts := httptest.NewServer(h)
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/mcp/notes", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req) // no Authorization
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	wa := resp.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(wa, "Bearer ") || !strings.Contains(wa, "resource_metadata=") {
		t.Fatalf("missing/invalid WWW-Authenticate: %q", wa)
	}
	if !strings.Contains(wa, "/.well-known/oauth-protected-resource/mcp/notes") {
		t.Errorf("resource_metadata should target this endpoint's well-known path, got %q", wa)
	}

	// Without OAuth: no resource_metadata hint (static-bearer-only deployment).
	h2 := mcp.BuildHandler(reg, log, []string{"tok"}, nil, nil)
	ts2 := httptest.NewServer(h2)
	defer ts2.Close()
	req2, _ := http.NewRequest("POST", ts2.URL+"/mcp/notes", strings.NewReader(`{}`))
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp2.StatusCode)
	}
	if wa := resp2.Header.Get("WWW-Authenticate"); wa != "" {
		t.Errorf("no WWW-Authenticate expected without OAuth, got %q", wa)
	}
}

// TestProtectedResourceMetadataPerEndpoint confirms the well-known
// protected-resource document resolves under the per-workspace path and names the
// resource accordingly (RFC 9728).
func TestProtectedResourceMetadataPerEndpoint(t *testing.T) {
	reg, _, _ := twoWorkspaceRegistry(t)
	log := mcp.NewLogger("error", &bytes.Buffer{})
	oauth := mcp.NewOAuthServer("client", "supersecretsupersecretsupersecret")
	ts := httptest.NewServer(mcp.BuildHandler(reg, log, []string{"tok"}, oauth, nil))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/.well-known/oauth-protected-resource/mcp/notes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var doc struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(doc.Resource, "/mcp/notes") {
		t.Errorf("resource should be the per-endpoint URL, got %q", doc.Resource)
	}
	if len(doc.AuthorizationServers) != 1 {
		t.Errorf("expected one authorization server, got %v", doc.AuthorizationServers)
	}
}

// TestOriginValidation covers the MCP 2025-11-25 DNS-rebinding defense (task 18).
// The spec defines no client-sent Origin value, and legitimate MCP traffic here is
// Origin-less, so we bake in no default allowlist: an empty allowlist accepts
// Origin-less requests (past the gate, to the 401 since no bearer is sent) and
// 403s ANY present Origin. An explicit allowlist lets a named browser origin
// through; "*" disables the check.
func TestOriginValidation(t *testing.T) {
	reg, _, _ := twoWorkspaceRegistry(t)
	log := mcp.NewLogger("error", &bytes.Buffer{})

	// post sends an unauthenticated POST with the given Origin (empty = none) and
	// returns the status code. 401 means it passed the Origin gate (no bearer
	// sent); 403 means the Origin gate rejected it.
	post := func(ts *httptest.Server, origin string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/mcp/notes", strings.NewReader(`{}`))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Empty allowlist (nil): Origin-less passes, ANY present Origin is rejected.
	def := httptest.NewServer(mcp.BuildHandler(reg, log, []string{"tok"}, nil, nil))
	defer def.Close()
	if got := post(def, ""); got != http.StatusUnauthorized {
		t.Errorf("absent origin: want 401 (past origin gate), got %d", got)
	}
	if got := post(def, "https://evil.example"); got != http.StatusForbidden {
		t.Errorf("present origin (empty allowlist): want 403, got %d", got)
	}
	if got := post(def, "https://claude.ai"); got != http.StatusForbidden {
		t.Errorf("present origin with no allowlist entry: want 403 (no baked-in default), got %d", got)
	}

	// Explicit allowlist: the named origin passes the gate, others are rejected.
	named := httptest.NewServer(mcp.BuildHandler(reg, log, []string{"tok"}, nil, []string{"http://localhost:6274"}))
	defer named.Close()
	if got := post(named, "http://localhost:6274"); got != http.StatusUnauthorized {
		t.Errorf("allowlisted origin: want 401 (past origin gate), got %d", got)
	}
	if got := post(named, "https://evil.example"); got != http.StatusForbidden {
		t.Errorf("non-allowlisted origin: want 403, got %d", got)
	}

	// "*" disables the check: an arbitrary origin reaches the auth layer.
	any := httptest.NewServer(mcp.BuildHandler(reg, log, []string{"tok"}, nil, []string{"*"}))
	defer any.Close()
	if got := post(any, "https://evil.example"); got != http.StatusUnauthorized {
		t.Errorf("wildcard origin: want 401 (past origin gate), got %d", got)
	}
}
