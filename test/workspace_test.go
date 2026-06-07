package test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mnehpets/workspace-mcp/config"
	"github.com/mnehpets/workspace-mcp/workspace"
)

func twoWorkspaceRegistry(t *testing.T) (*workspace.Registry, string, string) {
	t.Helper()
	d1 := t.TempDir()
	d2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(d1, "in1.md"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d2, "in2.md"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Workspaces: []config.WorkspaceConfig{
			{Name: "default", Root: d1, Policy: config.PolicyConfig{AllowGlobs: []string{"**/*.md"}}, Read: config.ReadConfig{MaxBytes: 1000}},
			{Name: "notes", Root: d2, Policy: config.PolicyConfig{AllowGlobs: []string{"**/*.md"}}, Read: config.ReadConfig{MaxBytes: 1000}},
		},
	}
	reg, err := workspace.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	return reg, d1, d2
}

func TestRegistryDefault(t *testing.T) {
	reg, _, _ := twoWorkspaceRegistry(t)
	ws, err := reg.Get("")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Name != "default" {
		t.Fatalf("empty name should resolve to default, got %q", ws.Name)
	}
}

func TestRegistryUnknown(t *testing.T) {
	reg, _, _ := twoWorkspaceRegistry(t)
	if _, err := reg.Get("nope"); err != workspace.ErrUnknownWorkspace {
		t.Fatalf("expected ErrUnknownWorkspace, got %v", err)
	}
}

// A file that exists only in workspace "notes" must be unreachable through the
// "default" workspace — each has its own os.Root.
func TestCrossWorkspaceIsolation(t *testing.T) {
	reg, _, _ := twoWorkspaceRegistry(t)
	def, _ := reg.Get("default")
	notes, _ := reg.Get("notes")

	// Sanity: each can read its own file.
	if f, err := def.Root.Open("in1.md"); err != nil {
		t.Fatalf("default should read in1.md: %v", err)
	} else {
		f.Close()
	}
	if f, err := notes.Root.Open("in2.md"); err != nil {
		t.Fatalf("notes should read in2.md: %v", err)
	} else {
		f.Close()
	}

	// default must NOT see notes' file.
	if f, err := def.Root.Open("in2.md"); err == nil {
		b, _ := io.ReadAll(f)
		f.Close()
		t.Fatalf("cross-workspace read leaked: %q", b)
	}
}
