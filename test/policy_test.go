package test

import (
	"testing"

	"github.com/mnehpets/workspace-mcp/policy"
)

func newTestPolicy() *policy.Policy {
	return policy.New(
		[]string{"**/*.md", "**/*.go", "docs/**", "README*"},
		[]string{".git/**", "**/.env", "**/.env.*", "**/*secret*", "**/*.pem", "**/*.key", "**/id_rsa*", "**/.ssh/**", "**/node_modules/**"},
	)
}

func TestPolicyAllowsListedFiles(t *testing.T) {
	p := newTestPolicy()
	for _, rel := range []string{"README.md", "docs/guide.md", "pkg/main.go", "docs/sub/x.txt"} {
		if d := p.CheckFile(rel); !d.Allowed {
			t.Errorf("expected %q allowed, got reason %q", rel, d.Reason)
		}
	}
}

func TestPolicyBlocksSensitive(t *testing.T) {
	p := newTestPolicy()
	cases := map[string]string{
		".env":                    "blocked_glob", // block wins over dotfile backstop
		".aws/config":             "dotfile",      // hidden, not covered by any block glob
		"config/.env":             "blocked_glob",
		"deploy/.env.production":  "blocked_glob",
		"keys/server.pem":         "blocked_glob",
		"keys/server.key":         "blocked_glob",
		"secrets/api_secret.txt":  "blocked_glob",
		"home/.ssh/id_rsa":        "blocked_glob",
		"node_modules/x/index.js": "blocked_glob",
		".git/config":             "blocked_glob",
	}
	for rel, wantReason := range cases {
		d := p.CheckFile(rel)
		if d.Allowed {
			t.Errorf("expected %q denied", rel)
			continue
		}
		if d.Reason != wantReason {
			t.Errorf("%q: want reason %q, got %q", rel, wantReason, d.Reason)
		}
	}
}

func TestPolicyNotAllowlisted(t *testing.T) {
	p := newTestPolicy()
	d := p.CheckFile("build/output.bin")
	if d.Allowed || d.Reason != "not_allowlisted" {
		t.Fatalf("expected not_allowlisted, got %+v", d)
	}
}

func TestPolicyDirListable(t *testing.T) {
	p := newTestPolicy()
	if d := p.CheckDir("."); !d.Allowed {
		t.Fatal("root must be listable")
	}
	if d := p.CheckDir("pkg"); !d.Allowed {
		t.Fatalf("ordinary dir must be listable, got %q", d.Reason)
	}
	if d := p.CheckDir("node_modules"); d.Allowed {
		t.Fatal("blocked dir must not be listable")
	}
	if d := p.CheckDir(".git"); d.Allowed {
		t.Fatal("dotfile dir must not be listable")
	}
}

// docs/../.env style inputs never reach policy as a literal; fsroot.Clean
// resolves them. Policy still denies the resolved sensitive path.
func TestPolicyResolvedDotEnvDenied(t *testing.T) {
	p := newTestPolicy()
	if d := p.CheckFile("app/.env"); d.Allowed {
		t.Fatal(".env must be denied")
	}
}
