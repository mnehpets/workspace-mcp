package test

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInitializeHandshake(t *testing.T) {
	reg, _, _ := twoWorkspaceRegistry(t)
	f := newMCPFixture(t, reg)
	rr := f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if rr.Error != nil {
		t.Fatalf("initialize error: %+v", rr.Error)
	}
	var res struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(rr.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.ProtocolVersion != "2025-06-18" {
		t.Errorf("protocol echo: got %q", res.ProtocolVersion)
	}
	if _, ok := res.Capabilities["tools"]; !ok {
		t.Errorf("missing tools capability: %+v", res.Capabilities)
	}
	if res.ServerInfo.Name == "" {
		t.Errorf("missing serverInfo.name")
	}
}

func TestInitializeSendsInstructions(t *testing.T) {
	reg, _, _ := twoWorkspaceRegistry(t)
	f := newMCPFixture(t, reg)
	rr := f.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if rr.Error != nil {
		t.Fatalf("initialize error: %+v", rr.Error)
	}
	var res struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(rr.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.Instructions == "" {
		t.Fatal("expected non-empty instructions in initialize result")
	}
	// Orient-first guidance and the read-only constraint must be present.
	for _, want := range []string{"tree_search", "read-only"} {
		if !strings.Contains(res.Instructions, want) {
			t.Errorf("instructions missing %q", want)
		}
	}
}

func TestInitializeUnknownProtocolNegotiatesDown(t *testing.T) {
	reg, _, _ := twoWorkspaceRegistry(t)
	f := newMCPFixture(t, reg)
	rr := f.call(t, "initialize", map[string]any{"protocolVersion": "1999-01-01"})
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	json.Unmarshal(rr.Result, &res)
	if res.ProtocolVersion != "2025-11-25" {
		t.Fatalf("expected fallback to newest supported, got %q", res.ProtocolVersion)
	}
}

func TestToolsListInvariant(t *testing.T) {
	reg, _, _ := twoWorkspaceRegistry(t)
	f := newMCPFixture(t, reg)
	rr := f.call(t, "tools/list", map[string]any{})
	if rr.Error != nil {
		t.Fatalf("tools/list error: %+v", rr.Error)
	}
	var res struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
			Annotations map[string]any `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(rr.Result, &res); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"workspace_info": true, "file_read": true,
		"tree_search": true, "git_status": true, "git_diff": true,
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no input schema", tool.Name)
		}
		// Every tool here is read-only and closed-world; annotations must say so.
		if tool.Annotations["readOnlyHint"] != true {
			t.Errorf("tool %q missing readOnlyHint=true: %+v", tool.Name, tool.Annotations)
		}
		if tool.Annotations["openWorldHint"] != false {
			t.Errorf("tool %q missing openWorldHint=false: %+v", tool.Name, tool.Annotations)
		}
		// Workspace selection is by route now (§17): no tool takes a `workspace` arg.
		if props, ok := tool.InputSchema["properties"].(map[string]any); ok {
			if _, has := props["workspace"]; has {
				t.Errorf("tool %q still exposes a workspace param: %+v", tool.Name, props)
			}
		}
		// No mutating/shell surface may ever appear.
		for _, banned := range []string{"write", "create", "delete", "move", "rename", "exec", "shell", "patch", "command"} {
			if strings.Contains(tool.Name, banned) {
				t.Errorf("forbidden tool exposed: %q", tool.Name)
			}
		}
	}
	if len(got) != len(want) {
		t.Fatalf("tool set mismatch: got %v want %v", got, want)
	}
	for n := range want {
		if !got[n] {
			t.Errorf("missing tool %q", n)
		}
	}
}

func TestUnknownToolRejected(t *testing.T) {
	reg, _, _ := twoWorkspaceRegistry(t)
	f := newMCPFixture(t, reg)
	rr := f.call(t, "tools/call", map[string]any{"name": "definitely_not_a_tool", "arguments": map[string]any{}})
	if rr.Error == nil {
		t.Fatal("expected JSON-RPC error for unknown tool name")
	}
}
