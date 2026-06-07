package test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mnehpets/workspace-mcp/config"
)

func TestSecretRefResolveEnv(t *testing.T) {
	env := map[string]string{"TOK": "value-from-env"}
	ref := config.SecretRef{Env: "TOK"}
	got, err := ref.Resolve(env)
	if err != nil {
		t.Fatal(err)
	}
	if got != "value-from-env" {
		t.Fatalf("got %q", got)
	}
}

func TestSecretRefMissingIsError(t *testing.T) {
	if _, err := (config.SecretRef{Env: "NOPE"}).Resolve(map[string]string{}); err == nil {
		t.Fatal("expected error for missing env var")
	}
	if _, err := (config.SecretRef{Env: "EMPTY"}).Resolve(map[string]string{"EMPTY": ""}); err == nil {
		t.Fatal("expected error for empty env var")
	}
}

func TestSecretRefLiteral(t *testing.T) {
	got, err := (config.SecretRef{Literal: "inline"}).Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "inline" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveBearerTokensSingle(t *testing.T) {
	c := &config.Config{Auth: config.AuthConfig{BearerToken: config.SecretRef{Env: "A"}}}
	got, err := c.ResolveBearerTokens(map[string]string{"A": "tok-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "tok-a" {
		t.Fatalf("got %v", got)
	}
}

func TestResolveBearerTokensPlural(t *testing.T) {
	c := &config.Config{Auth: config.AuthConfig{BearerTokens: []config.SecretRef{
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
	c := &config.Config{Auth: config.AuthConfig{
		BearerToken:  config.SecretRef{Env: "A"},
		BearerTokens: []config.SecretRef{{Env: "B"}},
	}}
	if _, err := c.ResolveBearerTokens(map[string]string{"A": "x", "B": "y"}); err == nil {
		t.Fatal("expected error when both bearerToken and bearerTokens are set")
	}
}

func TestResolveBearerTokensNoneSetIsError(t *testing.T) {
	c := &config.Config{}
	if _, err := c.ResolveBearerTokens(map[string]string{}); err == nil {
		t.Fatal("expected error when no bearer token is configured")
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

	env, err := config.LoadEnv(envFile)
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
	env, err := config.LoadEnv(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing env file should not error: %v", err)
	}
	if env == nil {
		t.Fatal("expected non-nil map")
	}
}
