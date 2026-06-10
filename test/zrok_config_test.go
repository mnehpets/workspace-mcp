package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mnehpets/workspace-mcp/mcp"
)

// ResolveZrokEnableToken mirrors ResolveNgrokAuthtoken: it resolves the account
// token from env when zrok is enabled, errors when the ref is unset, and returns
// an empty string (no error) when zrok is disabled.
func TestResolveZrokEnableToken(t *testing.T) {
	env := map[string]string{"ZROK_ENABLE_TOKEN": "tok-from-env"}

	enabled := &mcp.Config{Server: mcp.ServerConfig{Zrok: mcp.ZrokConfig{
		Enabled:     true,
		EnableToken: mcp.SecretRef{Env: "ZROK_ENABLE_TOKEN"},
	}}}
	got, err := enabled.ResolveZrokEnableToken(env)
	if err != nil {
		t.Fatal(err)
	}
	if got != "tok-from-env" {
		t.Fatalf("got %q", got)
	}

	// Enabled but the env var is missing → error.
	if _, err := enabled.ResolveZrokEnableToken(map[string]string{}); err == nil {
		t.Fatal("expected error when ZROK_ENABLE_TOKEN is unset")
	}

	// Disabled → empty string, no error (resolver short-circuits).
	disabled := &mcp.Config{Server: mcp.ServerConfig{Zrok: mcp.ZrokConfig{
		EnableToken: mcp.SecretRef{Env: "ZROK_ENABLE_TOKEN"},
	}}}
	got, err = disabled.ResolveZrokEnableToken(env)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("disabled zrok should resolve to empty, got %q", got)
	}
}

// ngrok and zrok both replace the local TCP listener, so enabling both is a
// config error; enableToken is required whenever zrok is enabled.
func TestZrokValidation(t *testing.T) {
	base := func() *mcp.Config {
		return &mcp.Config{
			Server:     mcp.ServerConfig{Host: "127.0.0.1", Port: 3850},
			Workspaces: []mcp.WorkspaceConfig{{Name: "default", Root: t.TempDir(), Read: mcp.ReadConfig{MaxBytes: 1000}}},
		}
	}
	const validTok = "0123456789abcdef0123456789abcdef" // 32 bytes

	// Both tunnels enabled → rejected.
	c := base()
	c.Server.Ngrok = mcp.NgrokConfig{Enabled: true, Authtoken: mcp.SecretRef{Env: "NGROK_AUTHTOKEN"}}
	c.Server.Zrok = mcp.ZrokConfig{Enabled: true, EnableToken: mcp.SecretRef{Env: "ZROK_ENABLE_TOKEN"}}
	if err := c.Validate([]string{validTok}, true); err == nil {
		t.Fatal("expected error when both ngrok and zrok are enabled")
	} else if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("unexpected error: %v", err)
	}

	// zrok enabled without an enableToken → rejected.
	c = base()
	c.Server.Zrok = mcp.ZrokConfig{Enabled: true}
	if err := c.Validate([]string{validTok}, true); err == nil {
		t.Fatal("expected error when zrok.enableToken is unset")
	} else if !strings.Contains(err.Error(), "enableToken") {
		t.Fatalf("unexpected error: %v", err)
	}

	// zrok enabled with a token → accepted (and the port check is skipped since
	// the tunnel replaces the local listener).
	c = base()
	c.Server.Port = 0
	c.Server.Zrok = mcp.ZrokConfig{Enabled: true, EnableToken: mcp.SecretRef{Env: "ZROK_ENABLE_TOKEN"}}
	if err := c.Validate([]string{validTok}, true); err != nil {
		t.Fatalf("valid zrok config should pass: %v", err)
	}
}

// The full server.zrok block parses (and unknown keys would fail, per
// KnownFields(true)).
func TestZrokConfigParses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
server:
  zrok:
    enabled: true
    enableToken:
      env: ZROK_ENABLE_TOKEN
    apiEndpoint: https://api-v2.zrok.io
    frontend: public
    uniqueName: my-workspace-mcp
workspaces:
  - name: default
    root: ` + dir + `
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := mcp.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	z := c.Server.Zrok
	if !z.Enabled || z.EnableToken.Env != "ZROK_ENABLE_TOKEN" ||
		z.ApiEndpoint != "https://api-v2.zrok.io" || z.Frontend != "public" || z.UniqueName != "my-workspace-mcp" {
		t.Fatalf("zrok config did not round-trip: %+v", z)
	}
}
