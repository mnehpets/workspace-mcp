package mcp

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// SecretRef is a secret-valued config field. It is either a reference to an
// environment variable ({ env: NAME }) or, discouraged, an inline literal
// string. Secrets must not be committed in config.yaml, so the {env:NAME} form
// is the supported path; an inline literal is accepted but flagged.
type SecretRef struct {
	Env     string // name of an env var to read the value from
	Literal string // inline value (discouraged; kept out of committed config)
}

// UnmarshalYAML accepts either a scalar string (literal) or a mapping with an
// `env` key.
func (r *SecretRef) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		return node.Decode(&r.Literal)
	case yaml.MappingNode:
		var m struct {
			Env string `yaml:"env"`
		}
		if err := node.Decode(&m); err != nil {
			return err
		}
		if m.Env == "" {
			return fmt.Errorf("secret reference mapping must have a non-empty `env` key")
		}
		r.Env = m.Env
		return nil
	default:
		return fmt.Errorf("secret reference must be a string or an { env: NAME } mapping")
	}
}

// IsLiteral reports whether the secret was given inline (discouraged for tokens).
func (r SecretRef) IsLiteral() bool { return r.Env == "" && r.Literal != "" }

// set reports whether the reference was populated (env name or inline literal).
func (r SecretRef) set() bool { return r.Env != "" || r.Literal != "" }

// LoadEnv builds the secret-resolution environment: it reads the dotenv file at
// path (a missing file is not an error — secrets may come entirely from the
// process environment), then overlays os.Environ() so the OS environment
// overrides dotenv values.
func LoadEnv(path string) (map[string]string, error) {
	env := map[string]string{}
	if path != "" {
		fileVals, err := godotenv.Read(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("read env file %q: %w", path, err)
			}
		} else {
			for k, v := range fileVals {
				env[k] = v
			}
		}
	}
	// OS environment overrides dotenv.
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	return env, nil
}

// Resolve returns the concrete secret value for r against the merged env map.
// A missing or empty referenced variable is an error.
func (r SecretRef) Resolve(env map[string]string) (string, error) {
	if r.Env != "" {
		v, ok := env[r.Env]
		if !ok || v == "" {
			return "", fmt.Errorf("referenced env var %q is not set or empty", r.Env)
		}
		return v, nil
	}
	if r.Literal != "" {
		return r.Literal, nil
	}
	return "", fmt.Errorf("empty secret reference")
}

// tokenRefs returns the configured bearer-token references. Exactly one of the
// singular `bearerToken` or the plural `bearerTokens` must be set.
func (a AuthConfig) tokenRefs() ([]SecretRef, error) {
	single := a.BearerToken.set()
	multi := len(a.BearerTokens) > 0
	switch {
	case single && multi:
		return nil, fmt.Errorf("set either auth.bearerToken or auth.bearerTokens, not both")
	case multi:
		return a.BearerTokens, nil
	case single:
		return []SecretRef{a.BearerToken}, nil
	default:
		return nil, fmt.Errorf("no bearer token configured (set auth.bearerToken or auth.bearerTokens)")
	}
}

// ResolveBearerTokens resolves every configured bearer token against env. The
// server accepts a request bearing any one of the returned tokens, which enables
// overlap-window rotation (add the new token, switch clients over, drop the old).
func (c *Config) ResolveBearerTokens(env map[string]string) ([]string, error) {
	refs, err := c.Auth.tokenRefs()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(refs))
	for i, ref := range refs {
		v, err := ref.Resolve(env)
		if err != nil {
			return nil, fmt.Errorf("bearer token %d: %w", i, err)
		}
		out = append(out, v)
	}
	return out, nil
}
