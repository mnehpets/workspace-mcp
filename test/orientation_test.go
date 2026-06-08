package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mnehpets/workspace-mcp/mcp"
)

// buildWS builds a single-"default"-workspace registry rooted at a temp dir
// seeded with the given relative→content files, under the given allow globs.
func buildWS(t *testing.T, files map[string]string, allow []string, desc string) *mcp.Workspace {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &mcp.Config{Workspaces: []mcp.WorkspaceConfig{{
		Name: "default", Root: dir, Description: desc,
		Policy: mcp.PolicyConfig{AllowGlobs: allow},
		Read:   mcp.ReadConfig{MaxBytes: 100000},
	}}}
	reg, err := mcp.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	ws, _ := reg.Get("default")
	return ws
}

func TestDescriptionConfigWinsOverReadme(t *testing.T) {
	ws := buildWS(t,
		map[string]string{"README.md": "# Title\nfrom the readme\n"},
		[]string{"**/*.md"}, "from config")
	if ws.Description != "from config" {
		t.Errorf("config description should win, got %q", ws.Description)
	}
}

func TestDescriptionReadmeFallback(t *testing.T) {
	ws := buildWS(t,
		map[string]string{"README.md": "# Project Title\nThis is   the intro paragraph.\n\n## Next\nignored\n"},
		[]string{"**/*.md"}, "")
	if ws.Description != "This is the intro paragraph." {
		t.Errorf("README first section wrong: %q", ws.Description)
	}
}

func TestDescriptionCap(t *testing.T) {
	long := strings.Repeat("word ", 200) // ~1000 chars, no headings after the first
	ws := buildWS(t,
		map[string]string{"README.md": "# T\n" + long + "\n"},
		[]string{"**/*.md"}, "")
	r := []rune(ws.Description)
	if len(r) == 0 || r[len(r)-1] != '…' {
		t.Errorf("expected capped description ending in ellipsis, got len=%d", len(r))
	}
	if len(r) > 281 {
		t.Errorf("description exceeds cap: %d runes", len(r))
	}
}

func TestDescriptionPolicyBlockedReadme(t *testing.T) {
	// README exists but the allow list does not admit it → no description, and it
	// must not appear in wellKnownFiles either.
	ws := buildWS(t,
		map[string]string{"README.md": "# Title\nintro\n", "docs/x.md": "x\n"},
		[]string{"docs/**"}, "")
	if ws.Description != "" {
		t.Errorf("policy-blocked README should yield no description, got %q", ws.Description)
	}
	for _, f := range ws.WellKnownFiles {
		if f == "README.md" {
			t.Errorf("policy-blocked README must not appear in wellKnownFiles: %v", ws.WellKnownFiles)
		}
	}
}

func TestWellKnownFilesSubset(t *testing.T) {
	ws := buildWS(t,
		map[string]string{
			"README.md": "# r\n", "CLAUDE.md": "c\n", "other.md": "o\n",
		},
		[]string{"**/*.md"}, "")
	got := strings.Join(ws.WellKnownFiles, ",")
	if got != "README.md,CLAUDE.md" {
		t.Errorf("wellKnownFiles should be exactly the present subset in order, got %q", got)
	}
}

// --- Task 17: convention recognizer ---

// Case-insensitive, extension-agnostic recognition across the curated stems,
// returned in priority order (then alphabetical).
func TestOrientationConventions(t *testing.T) {
	ws := buildWS(t, map[string]string{
		"ReadMe.MD":    "x\n", // case-insensitive
		"index.md":     "x\n",
		"_index.md":    "x\n",
		"OVERVIEW":     "x\n", // no extension
		"about.rst":    "x\n", // non-markdown extension
		"unrelated.md": "x\n", // not an orientation stem
	}, []string{"**/*"}, "skip-desc")
	got := strings.Join(ws.WellKnownFiles, ",")
	want := "ReadMe.MD,index.md,_index.md,OVERVIEW,about.rst"
	if got != want {
		t.Errorf("convention detection/order wrong:\n got %q\nwant %q", got, want)
	}
}

// Only the tree root is scanned — a README nested in a subdirectory is ignored.
func TestOrientationRootOnly(t *testing.T) {
	ws := buildWS(t, map[string]string{"docs/README.md": "x\n"}, []string{"**/*"}, "skip")
	if len(ws.WellKnownFiles) != 0 {
		t.Errorf("non-root orientation files must be ignored, got %v", ws.WellKnownFiles)
	}
}

// The reported list is capped so a noisy root can't flood workspace_list, and the
// cap keeps the highest-priority stems.
func TestOrientationCap(t *testing.T) {
	ws := buildWS(t, map[string]string{
		"readme.md": "x\n", "index.md": "x\n", "_index.md": "x\n",
		"contents.md": "x\n", "toc.md": "x\n", "overview.md": "x\n", // 6 → capped to 5
	}, []string{"**/*"}, "skip")
	if len(ws.WellKnownFiles) != 5 {
		t.Fatalf("expected cap of 5, got %d (%v)", len(ws.WellKnownFiles), ws.WellKnownFiles)
	}
	for _, f := range ws.WellKnownFiles {
		if f == "overview.md" {
			t.Errorf("lowest-priority file should have been dropped by the cap: %v", ws.WellKnownFiles)
		}
	}
}

// The description falls back to the highest-priority detected file, not README
// specifically — but README still outranks index when both exist.
func TestOrientationDescriptionPriority(t *testing.T) {
	ws := buildWS(t, map[string]string{
		"README.md": "# R\nreadme intro\n",
		"index.md":  "# I\nindex intro\n",
	}, []string{"**/*"}, "")
	if ws.Description != "readme intro" {
		t.Errorf("description should come from the highest-priority file (README), got %q", ws.Description)
	}
	// And when README is absent, index drives it.
	ws2 := buildWS(t, map[string]string{"index.md": "# I\nindex intro\n"}, []string{"**/*"}, "")
	if ws2.Description != "index intro" {
		t.Errorf("with no README, description should come from index, got %q", ws2.Description)
	}
}
