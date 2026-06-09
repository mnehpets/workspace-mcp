package test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mnehpets/workspace-mcp/mcp"
)

func TestSecretRefResolveEnv(t *testing.T) {
	env := map[string]string{"TOK": "value-from-env"}
	ref := mcp.SecretRef{Env: "TOK"}
	got, err := ref.Resolve(env)
	if err != nil {
		t.Fatal(err)
	}
	if got != "value-from-env" {
		t.Fatalf("got %q", got)
	}
}

func TestSecretRefMissingIsError(t *testing.T) {
	if _, err := (mcp.SecretRef{Env: "NOPE"}).Resolve(map[string]string{}); err == nil {
		t.Fatal("expected error for missing env var")
	}
	if _, err := (mcp.SecretRef{Env: "EMPTY"}).Resolve(map[string]string{"EMPTY": ""}); err == nil {
		t.Fatal("expected error for empty env var")
	}
}

func TestSecretRefLiteral(t *testing.T) {
	got, err := (mcp.SecretRef{Literal: "inline"}).Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "inline" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveBearerTokensSingle(t *testing.T) {
	c := &mcp.Config{Auth: mcp.AuthConfig{BearerToken: mcp.SecretRef{Env: "A"}}}
	got, err := c.ResolveBearerTokens(map[string]string{"A": "tok-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "tok-a" {
		t.Fatalf("got %v", got)
	}
}

func TestResolveBearerTokensPlural(t *testing.T) {
	c := &mcp.Config{Auth: mcp.AuthConfig{BearerTokens: []mcp.SecretRef{
		{Env: "OLD"}, {Env: "NEW"},
	}}}
	got, err := c.ResolveBearerTokens(map[string]string{"OLD": "old-tok", "NEW": "new-tok"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "old-tok" || got[1] != "new-tok" {
		t.Fatalf("got %v", got)
	}
}

func TestResolveBearerTokensBothSetIsError(t *testing.T) {
	c := &mcp.Config{Auth: mcp.AuthConfig{
		BearerToken:  mcp.SecretRef{Env: "A"},
		BearerTokens: []mcp.SecretRef{{Env: "B"}},
	}}
	if _, err := c.ResolveBearerTokens(map[string]string{"A": "x", "B": "y"}); err == nil {
		t.Fatal("expected error when both bearerToken and bearerTokens are set")
	}
}

// With neither a static token nor OAuth configured, resolution itself is NOT an
// error — an OAuth-only deployment is legitimate, so tokenRefs returns nothing.
// The "some auth must exist" guarantee moved to Validate (see below).
func TestResolveBearerTokensNoneIsEmptyNotError(t *testing.T) {
	c := &mcp.Config{}
	got, err := c.ResolveBearerTokens(map[string]string{})
	if err != nil {
		t.Fatalf("resolving with no tokens configured should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no tokens, got %v", got)
	}
}

// Validate (HTTP mode) is where missing auth is caught: no bearer tokens AND no
// OAuth is rejected, but either alone satisfies it; stdio mode never requires it.
func TestValidateRequiresSomeAuth(t *testing.T) {
	base := func() *mcp.Config {
		return &mcp.Config{
			Server:     mcp.ServerConfig{Host: "127.0.0.1", Port: 3850},
			Workspaces: []mcp.WorkspaceConfig{{Name: "default", Root: t.TempDir(), Read: mcp.ReadConfig{MaxBytes: 1000}}},
		}
	}
	const validTok = "0123456789abcdef0123456789abcdef" // 32 bytes

	// HTTP mode, neither bearer nor OAuth → error.
	if err := base().Validate(nil, true); err == nil {
		t.Fatal("expected error when neither bearer nor oauth is configured")
	}
	// A valid bearer token alone satisfies auth.
	if err := base().Validate([]string{validTok}, true); err != nil {
		t.Fatalf("a valid bearer token should satisfy auth: %v", err)
	}
	// OAuth alone satisfies auth (no static token needed).
	c := base()
	c.Auth.OAuth = mcp.OAuthConfig{ClientID: "cid", ClientSecret: mcp.SecretRef{Env: "X"}}
	if err := c.Validate(nil, true); err != nil {
		t.Fatalf("oauth alone should satisfy auth: %v", err)
	}
	// stdio mode (requireBearer=false) never requires auth.
	if err := base().Validate(nil, false); err != nil {
		t.Fatalf("stdio mode should not require auth: %v", err)
	}
}

// LoadEnv must let the OS environment override dotenv values.
func TestLoadEnvOSOverridesDotenv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("SHARED=from-file\nONLYFILE=file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHARED", "from-os")

	env, err := mcp.LoadEnv(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if env["SHARED"] != "from-os" {
		t.Fatalf("OS should override dotenv; got %q", env["SHARED"])
	}
	if env["ONLYFILE"] != "file" {
		t.Fatalf("dotenv-only var should survive; got %q", env["ONLYFILE"])
	}
}

// A missing dotenv file is not an error (secrets may come from the OS env).
func TestLoadEnvMissingFileOK(t *testing.T) {
	env, err := mcp.LoadEnv(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing env file should not error: %v", err)
	}
	if env == nil {
		t.Fatal("expected non-nil map")
	}
}
