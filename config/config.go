// Package config loads and validates the server's YAML configuration.
//
// The config is intentionally structured (a list of workspaces, each with its
// own policy/read/grep settings), which is why it is YAML rather than a flat
// KEY=value file. Secret values are never stored here — see secrets.go.
package config

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

// ServerConfig holds listener settings. The server binds localhost only; a
// tunnel (ngrok) fronts it.
type ServerConfig struct {
	Host      string `yaml:"host"`
	Port      int    `yaml:"port"`
	PublicURL string `yaml:"publicURL"`
}

// AuthConfig holds authentication settings. Bearer tokens are secret references
// (see SecretRef), never literals in committed config. Configure either a single
// `bearerToken` or a list of `bearerTokens` (not both); the server accepts a
// request bearing any one of them, which allows overlap-window rotation.
type AuthConfig struct {
	BearerToken  SecretRef   `yaml:"bearerToken"`
	BearerTokens []SecretRef `yaml:"bearerTokens"`
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

// Load reads and parses the YAML config at path. Unknown keys are an error
// (KnownFields(true)) so typos fail fast rather than silently doing nothing.
// It does not resolve secrets or validate semantics — call ResolveBearerTokens
// and Validate after.
func Load(path string) (*Config, error) {
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
		if c.Server.Port < 1 || c.Server.Port > 65535 {
			return fmt.Errorf("server.port %d out of range (1-65535)", c.Server.Port)
		}
		if len(bearerTokens) == 0 {
			return fmt.Errorf("at least one bearer token is required")
		}
		for i, t := range bearerTokens {
			if len(t) < 32 {
				return fmt.Errorf("resolved bearer token %d must be at least 32 bytes, got %d", i, len(t))
			}
		}
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
