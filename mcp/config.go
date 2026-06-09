// config loads and validates the server's YAML configuration.
//
// The config is intentionally structured (a list of workspaces, each with its
// own policy/read/grep settings), which is why it is YAML rather than a flat
// KEY=value file. Secret values are never stored here — see secrets.go.
package mcp

import (
	"bytes"
	"fmt"
	"os"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration.
type Config struct {
	Server     ServerConfig      `yaml:"server"`
	Auth       AuthConfig        `yaml:"auth"`
	Workspaces []WorkspaceConfig `yaml:"workspaces"`
	Log        LogConfig         `yaml:"log"`
}

// ServerConfig holds listener settings. Use either the plain TCP listener
// (host/port) or the built-in ngrok tunnel (ngrok.enabled: true) — not both.
type ServerConfig struct {
	Host  string      `yaml:"host"`
	Port  int         `yaml:"port"`
	Ngrok NgrokConfig `yaml:"ngrok"`
	// AllowedOrigins is the Origin-header allowlist for the Streamable-HTTP
	// transport (MCP 2025-11-25 DNS-rebinding defense): a request whose Origin is
	// present but not listed is rejected with 403. Requests with no Origin header
	// (non-browser clients — the normal case for MCP traffic) are always allowed.
	// Empty/omitted means an empty allowlist: Origin-less requests pass and ANY
	// present Origin is rejected. Add a specific origin to permit a browser client
	// (e.g. local dev), or a single "*" to disable the check.
	AllowedOrigins []string `yaml:"allowedOrigins"`
}

// NgrokConfig enables the built-in ngrok tunnel. When enabled, the server
// dials ngrok directly and skips the local TCP listener — no external ngrok
// process or ngrok.yml is needed. The public URL is logged at startup.
type NgrokConfig struct {
	Enabled   bool      `yaml:"enabled"`
	Authtoken SecretRef `yaml:"authtoken"`
	Domain    string    `yaml:"domain"` // optional reserved domain (e.g. my-host.ngrok.app)
}

// AuthConfig holds authentication settings. Bearer tokens are secret references
// (see SecretRef), never literals in committed config. Configure either a single
// `bearerToken` or a list of `bearerTokens` (not both); the server accepts a
// request bearing any one of them, which allows overlap-window rotation.
// Optionally configure `oauth` to enable the OAuth 2.0 authorization code flow
// (e.g. for claude.ai connectors that only support OAuth).
type AuthConfig struct {
	BearerToken  SecretRef   `yaml:"bearerToken"`
	BearerTokens []SecretRef `yaml:"bearerTokens"`
	OAuth        OAuthConfig `yaml:"oauth"`
}

// OAuthConfig enables the OAuth 2.0 authorization code flow. clientID is
// public and stored directly in config.yaml; clientSecret is a secret
// reference. Access tokens are issued dynamically per session.
type OAuthConfig struct {
	ClientID     string    `yaml:"clientID"`
	ClientSecret SecretRef `yaml:"clientSecret"`
}

// Enabled reports whether OAuth is configured.
func (o OAuthConfig) Enabled() bool {
	return o.ClientID != "" && o.ClientSecret.set()
}

// WorkspaceConfig describes one named directory tree and its per-workspace
// permissions. Permissions are per-workspace, never global.
type WorkspaceConfig struct {
	Name             string       `yaml:"name"`
	Root             string       `yaml:"root"`
	RespectGitignore bool         `yaml:"respectGitignore"`
	Description      string       `yaml:"description"` // optional; what the tree is for. Falls back to the README's first section.
	Policy           PolicyConfig `yaml:"policy"`
	Read             ReadConfig   `yaml:"read"`
	Grep             GrepConfig   `yaml:"grep"`
}

// PolicyConfig is the per-workspace allow/deny glob policy. Block always wins.
type PolicyConfig struct {
	AllowGlobs []string `yaml:"allowGlobs"`
	BlockGlobs []string `yaml:"blockGlobs"`
}

// ReadConfig bounds file reads.
type ReadConfig struct {
	MaxBytes int64 `yaml:"maxBytes"`
}

// GrepConfig configures the content search tool.
type GrepConfig struct {
	Enabled    bool `yaml:"enabled"`
	Workers    int  `yaml:"workers"` // 0 = GOMAXPROCS
	MaxMatches int  `yaml:"maxMatches"`
}

// LogConfig configures logging.
type LogConfig struct {
	Level string `yaml:"level"`
}

// ResolveNgrokAuthtoken resolves the ngrok authtoken from env. Returns an empty
// string if ngrok is not enabled.
func (c *Config) ResolveNgrokAuthtoken(env map[string]string) (string, error) {
	if !c.Server.Ngrok.Enabled {
		return "", nil
	}
	return c.Server.Ngrok.Authtoken.Resolve(env)
}

// ResolveOAuthCredentials returns the OAuth client credentials. clientID comes
// directly from config; clientSecret is resolved from the env map.
func (c *Config) ResolveOAuthCredentials(env map[string]string) (clientID, clientSecret string, err error) {
	clientID = c.Auth.OAuth.ClientID
	clientSecret, err = c.Auth.OAuth.ClientSecret.Resolve(env)
	if err != nil {
		return "", "", fmt.Errorf("oauth clientSecret: %w", err)
	}
	return clientID, clientSecret, nil
}

// Load reads and parses the YAML config at path. Unknown keys are an error
// (KnownFields(true)) so typos fail fast rather than silently doing nothing.
// It does not resolve secrets or validate semantics — call ResolveBearerTokens
// and Validate after.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	return &c, nil
}

// Validate checks semantic constraints. bearerTokens are the already-resolved
// secret values (see secrets.go). When requireBearer is true (HTTP mode) at least
// one must be configured and each must be at least 32 bytes; in stdio mode tokens
// are unused and not required.
func (c *Config) Validate(bearerTokens []string, requireBearer bool) error {
	if requireBearer {
		if !c.Server.Ngrok.Enabled {
			if c.Server.Port < 1 || c.Server.Port > 65535 {
				return fmt.Errorf("server.port %d out of range (1-65535)", c.Server.Port)
			}
		}
		if len(bearerTokens) == 0 && !c.Auth.OAuth.Enabled() {
			return fmt.Errorf("at least one bearer token or auth.oauth must be configured")
		}
		for i, t := range bearerTokens {
			if len(t) < 32 {
				return fmt.Errorf("resolved bearer token %d must be at least 32 bytes, got %d", i, len(t))
			}
		}
	}
	if c.Server.Ngrok.Enabled && !c.Server.Ngrok.Authtoken.set() {
		return fmt.Errorf("ngrok.authtoken is required when ngrok.enabled is true")
	}
	if len(c.Workspaces) == 0 {
		return fmt.Errorf("at least one workspace is required")
	}

	seen := make(map[string]bool, len(c.Workspaces))
	hasDefault := false
	for i := range c.Workspaces {
		w := &c.Workspaces[i]
		if w.Name == "" {
			return fmt.Errorf("workspaces[%d]: name is required", i)
		}
		if seen[w.Name] {
			return fmt.Errorf("duplicate workspace name %q", w.Name)
		}
		seen[w.Name] = true
		if w.Name == "default" {
			hasDefault = true
		}
		if w.Root == "" {
			return fmt.Errorf("workspace %q: root is required", w.Name)
		}
		info, err := os.Stat(w.Root)
		if err != nil {
			return fmt.Errorf("workspace %q: root %q: %w", w.Name, w.Root, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("workspace %q: root %q is not a directory", w.Name, w.Root)
		}
		for _, g := range w.Policy.AllowGlobs {
			if !doublestar.ValidatePattern(g) {
				return fmt.Errorf("workspace %q: invalid allow glob %q", w.Name, g)
			}
		}
		for _, g := range w.Policy.BlockGlobs {
			if !doublestar.ValidatePattern(g) {
				return fmt.Errorf("workspace %q: invalid block glob %q", w.Name, g)
			}
		}
		if w.Read.MaxBytes <= 0 {
			return fmt.Errorf("workspace %q: read.maxBytes must be positive", w.Name)
		}
	}
	if !hasDefault {
		return fmt.Errorf("a workspace named \"default\" must exist (it is the fallback for the workspace param)")
	}
	return nil
}
