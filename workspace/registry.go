// Package workspace builds and serves the registry of configured workspaces.
// Each workspace is an independent os.Root sandbox with its own policy, ignore
// set, and git-ness — permissions never cross between workspaces.
package workspace

import (
	"errors"
	"fmt"

	"github.com/mnehpets/workspace-mcp/config"
	"github.com/mnehpets/workspace-mcp/fsroot"
	"github.com/mnehpets/workspace-mcp/gitaware"
	"github.com/mnehpets/workspace-mcp/grrep"
	"github.com/mnehpets/workspace-mcp/policy"
)

// ErrUnknownWorkspace is returned when a requested workspace name is not configured.
var ErrUnknownWorkspace = errors.New("unknown workspace")

// Workspace is one configured directory tree and its per-workspace settings.
type Workspace struct {
	Name      string
	Root      *fsroot.Root
	Policy    *policy.Policy
	Ignore    *grrep.IgnoreSet // nil when respectGitignore is disabled
	IsGitRepo bool
	Read      config.ReadConfig
	Grep      config.GrepConfig
}

// Registry maps workspace names to their resolved Workspace.
type Registry struct {
	byName map[string]*Workspace
	order  []*Workspace
}

// Build constructs the registry from config, opening one os.Root per workspace
// and detecting git-ness. The caller owns Close.
func Build(cfg *config.Config) (*Registry, error) {
	reg := &Registry{byName: make(map[string]*Workspace, len(cfg.Workspaces))}
	for i := range cfg.Workspaces {
		wc := cfg.Workspaces[i]
		root, err := fsroot.Open(wc.Root)
		if err != nil {
			reg.Close()
			return nil, fmt.Errorf("workspace %q: open root: %w", wc.Name, err)
		}
		var ig *grrep.IgnoreSet
		if wc.RespectGitignore {
			ig = grrep.NewIgnoreSet(wc.Root)
		}
		ws := &Workspace{
			Name:      wc.Name,
			Root:      root,
			Policy:    policy.New(wc.Policy.AllowGlobs, wc.Policy.BlockGlobs),
			Ignore:    ig,
			IsGitRepo: gitaware.Detect(wc.Root),
			Read:      wc.Read,
			Grep:      wc.Grep,
		}
		reg.byName[wc.Name] = ws
		reg.order = append(reg.order, ws)
	}
	return reg, nil
}

// Get resolves a workspace by name; an empty name defaults to "default".
func (r *Registry) Get(name string) (*Workspace, error) {
	if name == "" {
		name = "default"
	}
	ws, ok := r.byName[name]
	if !ok {
		return nil, ErrUnknownWorkspace
	}
	return ws, nil
}

// List returns the workspaces in configuration order.
func (r *Registry) List() []*Workspace { return r.order }

// Close releases every workspace's os.Root.
func (r *Registry) Close() {
	for _, ws := range r.order {
		if ws.Root != nil {
			ws.Root.Close()
		}
	}
}
