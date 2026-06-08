package test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/mnehpets/workspace-mcp/config"
	"github.com/mnehpets/workspace-mcp/workspace"
)

// pngBytes is a minimal PNG: the 8-byte signature plus an IHDR chunk that
// contains NUL bytes (so the server's NUL probe classifies it as binary) and is
// recognizable by content sniffing.
var pngBytes = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00,
}

// binaryRegistry seeds a "default" workspace with a .png, an extensionless copy
// (for content sniffing), and a policy-blocked secret.png.
func binaryRegistry(t *testing.T) *workspace.Registry {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"pic.png", "blob", "secret.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), pngBytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Workspaces: []config.WorkspaceConfig{{
		Name: "default", Root: dir,
		Policy: config.PolicyConfig{AllowGlobs: []string{"**/*"}, BlockGlobs: []string{"**/secret.*"}},
		Read:   config.ReadConfig{MaxBytes: 100000},
	}}}
	reg, err := workspace.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	return reg
}

type binRead struct {
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
	Encoding  string `json:"encoding"`
	MimeType  string `json:"mimeType"`
}

func TestFileReadBinaryDelivery(t *testing.T) {
	f := newMCPFixture(t, binaryRegistry(t))
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	var r binRead
	f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "pic.png", "allowBinary": true}, &r)
	if !r.Binary || r.Encoding != "base64" {
		t.Fatalf("expected base64 binary delivery, got %+v", r)
	}
	if r.MimeType != "image/png" {
		t.Errorf("expected image/png mimeType, got %q", r.MimeType)
	}
	got, err := base64.StdEncoding.DecodeString(r.Content)
	if err != nil {
		t.Fatalf("content is not valid base64: %v", err)
	}
	if string(got) != string(pngBytes) {
		t.Errorf("decoded bytes != original (%d vs %d)", len(got), len(pngBytes))
	}
	if r.Truncated {
		t.Error("a whole-file binary read under the cap is not truncated")
	}
}

// An extensionless binary file gets its MIME type from content sniffing.
func TestFileReadBinarySniffsExtensionless(t *testing.T) {
	f := newMCPFixture(t, binaryRegistry(t))
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	var r binRead
	f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "blob", "allowBinary": true}, &r)
	if r.MimeType != "image/png" {
		t.Errorf("expected sniffed image/png, got %q", r.MimeType)
	}
}

func TestFileReadBinaryTruncated(t *testing.T) {
	f := newMCPFixture(t, binaryRegistry(t))
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	// Cap past the first NUL so the file is still recognized as binary, but below
	// its length so the read truncates. (A cap of a few bytes would fall before any
	// NUL and the file would read as text — detection only sees what is read.)
	var r binRead
	f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "pic.png", "allowBinary": true, "maxBytes": 12}, &r)
	if !r.Binary {
		t.Fatal("expected the read to still be classified binary")
	}
	if !r.Truncated {
		t.Error("an over-cap binary read must set truncated")
	}
	got, err := base64.StdEncoding.DecodeString(r.Content)
	if err != nil {
		t.Fatalf("content is not valid base64: %v", err)
	}
	if len(got) != 12 || string(got) != string(pngBytes[:12]) {
		t.Errorf("expected first 12 bytes, got %d (%v)", len(got), got)
	}
}

// Without allowBinary the default behavior is unchanged: flagged, no content.
func TestFileReadBinaryDefaultRefuses(t *testing.T) {
	f := newMCPFixture(t, binaryRegistry(t))
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	var r binRead
	f.callTool(t, "file_read", map[string]any{"workspace": "default", "path": "pic.png"}, &r)
	if !r.Binary {
		t.Error("binary should still be flagged by default")
	}
	if r.Content != "" || r.Encoding != "" {
		t.Errorf("default must not return binary content, got %+v", r)
	}
}

// Policy still gates the path even with allowBinary set.
func TestFileReadBinaryPolicyGated(t *testing.T) {
	f := newMCPFixture(t, binaryRegistry(t))
	f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	assertToolError(t, f.callTool(t, "file_read",
		map[string]any{"workspace": "default", "path": "secret.png", "allowBinary": true}, nil), "POLICY_DENIED")
}
