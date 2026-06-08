package test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mnehpets/workspace-mcp/fsroot"
	"github.com/mnehpets/workspace-mcp/grrep"
	"github.com/mnehpets/workspace-mcp/policy"
	"github.com/mnehpets/workspace-mcp/search"
)

// searchTree seeds a small workspace exercising every filter: an allowed go and
// markdown file, a frontmatter-bearing note, a policy-blocked key, a
// gitignored vendored file, and a binary blob.
func searchTree(t *testing.T) (*fsroot.Root, *policy.Policy, *grrep.IgnoreSet) {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", "package main\nfunc Hello() {}\n// TODO: fix\n")
	write("docs/guide.md", "Use the ASC workflow here.\nAnother line.\n")
	write("docs/notes.md", "nothing interesting\n")
	// A note with YAML frontmatter: "california" appears both as a declared tag
	// (inside the fence) and incidentally in the body.
	write("docs/west.md", "---\ntitle: West Coast\ntags: [california, drought]\n---\n\nReservoir levels across California fell.\nUnrelated body line.\n")
	write("secret.key", "PRIVATE-MATERIAL TODO\n")      // blocked by policy
	write("vendor/lib.go", "func TODO_vendored() {}\n") // ignored via .gitignore
	write(".gitignore", "vendor/\n")
	write("bin.dat", "abc\x00TODO\x00def\n") // binary, must be skipped

	r, err := fsroot.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	pol := policy.New([]string{"**/*.go", "**/*.md"}, []string{"**/*.key"})
	ig := grrep.NewIgnoreSet(dir)
	return r, pol, ig
}

func search1(text string, fixed bool) search.SearchRequest {
	return search.SearchRequest{
		Where:          []search.Predicate{{Text: text, FixedString: fixed}},
		IncludeMatches: true,
	}
}

func run(t *testing.T, r *fsroot.Root, pol *policy.Policy, ig *grrep.IgnoreSet, req search.SearchRequest, max int) *search.SearchResult {
	t.Helper()
	res, err := search.Search(r, pol, ig, req, 0, max, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A body predicate matches like the old grep: one literal hit in one file.
func TestSearchLiteralBody(t *testing.T) {
	r, pol, ig := searchTree(t)
	res := run(t, r, pol, ig, search1("TODO", true), 500)
	if len(res.Files) != 1 || res.Files[0].Path != "a.go" {
		t.Fatalf("expected only a.go, got %+v", res.Files)
	}
	if len(res.Files[0].Matches) != 1 || res.Files[0].Matches[0].Line != 3 {
		t.Fatalf("unexpected matches: %+v", res.Files[0].Matches)
	}
}

func TestSearchBlockedIgnoredBinarySkipped(t *testing.T) {
	r, pol, ig := searchTree(t)
	res := run(t, r, pol, ig, search1("TODO", true), 500)
	for _, f := range res.Files {
		if f.Path == "secret.key" || f.Path == "vendor/lib.go" || f.Path == "bin.dat" {
			t.Fatalf("blocked/ignored/binary path leaked: %s", f.Path)
		}
	}
}

func TestSearchRegexBody(t *testing.T) {
	r, pol, ig := searchTree(t)
	res := run(t, r, pol, ig, search1("func [A-Z]", false), 500)
	if len(res.Files) != 1 || res.Files[0].Matches[0].Text != "func Hello() {}" {
		t.Fatalf("regex match unexpected: %+v", res.Files)
	}
}

func TestSearchCaseInsensitive(t *testing.T) {
	r, pol, ig := searchTree(t)
	req := search.SearchRequest{
		Where:          []search.Predicate{{Text: "asc workflow", FixedString: true, CaseInsensitive: true}},
		IncludeMatches: true,
	}
	res := run(t, r, pol, ig, req, 500)
	if len(res.Files) != 1 || res.Files[0].Path != "docs/guide.md" {
		t.Fatalf("expected 1 case-insensitive match in guide.md, got %+v", res.Files)
	}
}

func TestSearchInvalidPattern(t *testing.T) {
	r, pol, ig := searchTree(t)
	_, err := search.Search(r, pol, ig, search1("func (", false), 0, 500, 1<<20)
	if _, ok := err.(*search.InvalidPatternError); !ok {
		t.Fatalf("expected InvalidPatternError, got %v", err)
	}
}

// Multiple predicates are AND-combined: only a file containing BOTH qualifies.
func TestSearchMultiPredicateAND(t *testing.T) {
	r, pol, ig := searchTree(t)
	both := search.SearchRequest{
		Where: []search.Predicate{
			{Text: "Reservoir", FixedString: true},
			{Text: "title", FixedString: true},
		},
		IncludeMatches: true,
	}
	res := run(t, r, pol, ig, both, 500)
	if len(res.Files) != 1 || res.Files[0].Path != "docs/west.md" {
		t.Fatalf("AND should select only west.md, got %+v", res.Files)
	}
	// And a predicate no file satisfies alongside one that some do → no results.
	none := search.SearchRequest{
		Where: []search.Predicate{
			{Text: "Reservoir", FixedString: true},
			{Text: "no-such-token", FixedString: true},
		},
		IncludeMatches: true,
	}
	res = run(t, r, pol, ig, none, 500)
	if len(res.Files) != 0 {
		t.Fatalf("AND with an unsatisfiable predicate should yield nothing, got %+v", res.Files)
	}
}

// Frontmatter-fence hits are split into metadataMatches; body hits stay in
// matches. "california" appears in both regions of west.md.
func TestSearchFenceSplit(t *testing.T) {
	r, pol, ig := searchTree(t)
	req := search.SearchRequest{
		PathGlob:       "docs/west.md",
		Where:          []search.Predicate{{Text: "california", FixedString: true, CaseInsensitive: true}},
		IncludeMatches: true,
	}
	res := run(t, r, pol, ig, req, 500)
	if len(res.Files) != 1 {
		t.Fatalf("expected west.md, got %+v", res.Files)
	}
	f := res.Files[0]
	if len(f.MetadataMatches) != 1 || f.MetadataMatches[0].Line != 3 {
		t.Fatalf("expected one metadata match on line 3 (tags), got %+v", f.MetadataMatches)
	}
	if len(f.Matches) != 1 || f.Matches[0].Line != 6 {
		t.Fatalf("expected one body match on line 6, got %+v", f.Matches)
	}
}

// includeMetadata returns the raw, unparsed frontmatter text; a fence-less file
// gets none.
func TestSearchIncludeMetadata(t *testing.T) {
	r, pol, ig := searchTree(t)
	req := search.SearchRequest{
		PathGlob:        "docs/*.md",
		IncludeMetadata: true,
	}
	res := run(t, r, pol, ig, req, 500)
	byPath := map[string]search.FileResult{}
	for _, f := range res.Files {
		byPath[f.Path] = f
	}
	west, ok := byPath["docs/west.md"]
	if !ok {
		t.Fatalf("west.md missing from %+v", res.Files)
	}
	want := "title: West Coast\ntags: [california, drought]"
	if west.Metadata != want {
		t.Fatalf("raw frontmatter mismatch:\n got %q\nwant %q", west.Metadata, want)
	}
	// A fence-less file is a no-op for metadata.
	if g, ok := byPath["docs/guide.md"]; !ok || g.Metadata != "" {
		t.Fatalf("fence-less guide.md should have empty metadata, got %q (present=%v)", g.Metadata, ok)
	}
}

// Metadata-only enumeration reads just the frontmatter region, so a tiny fence in
// front of a body far larger than the read cap is still lifted in full — and a
// fence that does not close within the (read-cap-bounded) probe yields nothing.
func TestSearchMetadataLargeBody(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("filler line of body text\n", 100000) // ~2.4 MiB body
	if err := os.WriteFile(filepath.Join(dir, "big.md"),
		[]byte("---\ntitle: Big Doc\n---\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := fsroot.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	pol := policy.New([]string{"**/*.md"}, nil)
	ig := grrep.NewIgnoreSet(dir)
	req := search.SearchRequest{IncludeMetadata: true}

	// Generous read cap: the small fence is lifted despite the multi-MiB body.
	res, err := search.Search(r, pol, ig, req, 0, 500, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Metadata != "title: Big Doc" {
		t.Fatalf("expected frontmatter from big body, got %+v", res.Files)
	}

	// Read cap smaller than the fence itself: it never closes within the probe,
	// so no metadata — but the file is still listed.
	res, err = search.Search(r, pol, ig, req, 0, 500, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Metadata != "" {
		t.Fatalf("expected no metadata under a tiny probe, got %+v", res.Files)
	}
}

// A where-less path glob enumerates matching files (no content read, just paths)
// and does not require grep to be enabled.
func TestSearchEnumeration(t *testing.T) {
	r, pol, ig := searchTree(t)
	res := run(t, r, pol, ig, search.SearchRequest{PathGlob: "docs/**/*.md"}, 500)
	got := map[string]bool{}
	for _, f := range res.Files {
		got[f.Path] = true
		if len(f.Matches) != 0 || len(f.MetadataMatches) != 0 {
			t.Errorf("enumeration should carry no matches, got %+v", f)
		}
		if f.Size <= 0 {
			t.Errorf("enumeration should report a positive size for %s, got %d", f.Path, f.Size)
		}
	}
	for _, want := range []string{"docs/guide.md", "docs/notes.md", "docs/west.md"} {
		if !got[want] {
			t.Errorf("enumeration missing %s (got %v)", want, got)
		}
	}
	if got["a.go"] {
		t.Errorf("glob docs/**/*.md should not match a.go")
	}
}

// Omitting path (and "**/*") enumerates the whole tree recursively; a single "*"
// is root-level only — the distinction the tool docs steer on.
func TestSearchEnumerationDepth(t *testing.T) {
	r, pol, ig := searchTree(t)

	whole := func(glob string) map[string]bool {
		res := run(t, r, pol, ig, search.SearchRequest{PathGlob: glob}, 500)
		got := map[string]bool{}
		for _, f := range res.Files {
			got[f.Path] = true
		}
		return got
	}

	// No path → entire tree: root file and nested file both present.
	all := whole("")
	if !all["a.go"] || !all["docs/guide.md"] {
		t.Fatalf("empty path should enumerate the whole tree, got %v", all)
	}
	// "**/*" is equivalent to omitting path.
	star2 := whole("**/*")
	if !star2["a.go"] || !star2["docs/guide.md"] {
		t.Fatalf("**/* should enumerate the whole tree, got %v", star2)
	}
	// "*" is single-segment: root files only, no nested files.
	root := whole("*")
	if !root["a.go"] {
		t.Fatalf("* should list root-level a.go, got %v", root)
	}
	if root["docs/guide.md"] {
		t.Fatalf("* must not cross into docs/, got %v", root)
	}
}

// includeMatches=false returns paths only even when predicates match.
func TestSearchIncludeMatchesToggle(t *testing.T) {
	r, pol, ig := searchTree(t)
	req := search.SearchRequest{
		Where:          []search.Predicate{{Text: "TODO", FixedString: true}},
		IncludeMatches: false,
	}
	res := run(t, r, pol, ig, req, 500)
	if len(res.Files) != 1 || res.Files[0].Path != "a.go" {
		t.Fatalf("expected a.go, got %+v", res.Files)
	}
	if len(res.Files[0].Matches) != 0 {
		t.Fatalf("includeMatches=false should drop matches, got %+v", res.Files[0].Matches)
	}
}

// A many-file, many-directory tree exercises fastwalk's concurrent callback. The
// walk must serialize its shared state (results slice + the non-thread-safe
// ignore tree); under -race the unsynchronized version flags a data race here,
// while the small fixtures above are too sparse to interleave.
func TestSearchConcurrentWalkRace(t *testing.T) {
	dir := t.TempDir()
	for d := 0; d < 40; d++ {
		sub := filepath.Join(dir, "d"+strconv.Itoa(d))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		// A per-directory .gitignore forces EnsureNode (a tree write) to race
		// against sibling Match calls (tree reads) without the walk's lock.
		if err := os.WriteFile(filepath.Join(sub, ".gitignore"), []byte("ignored.txt\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for f := 0; f < 25; f++ {
			p := filepath.Join(sub, "f"+strconv.Itoa(f)+".md")
			if err := os.WriteFile(p, []byte("needle here\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	r, err := fsroot.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	pol := policy.New([]string{"**/*.md"}, nil)
	ig := grrep.NewIgnoreSet(dir)

	res := run(t, r, pol, ig, search.SearchRequest{}, 10000) // enumerate everything
	if len(res.Files) != 40*25 {
		t.Fatalf("expected %d files, got %d", 40*25, len(res.Files))
	}
}

// The cap limits emitted results and sets truncated.
func TestSearchTruncation(t *testing.T) {
	r, pol, ig := searchTree(t)
	// "line" appears in several markdown files; cap at 1 file.
	res := run(t, r, pol, ig, search1("line", true), 1)
	if !res.Truncated {
		t.Fatal("expected truncated=true at the cap")
	}
	if len(res.Files) != 1 {
		t.Fatalf("expected exactly 1 file at the cap, got %d", len(res.Files))
	}
}
