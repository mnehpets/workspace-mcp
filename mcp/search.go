package mcp

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/mnehpets/workspace-mcp/grrep"
)

// InvalidPatternError signals an uncompilable regex (a `where` predicate with
// fixedString:false). The MCP layer maps it to INVALID_PATTERN.
type InvalidPatternError struct{ Err error }

func (e *InvalidPatternError) Error() string { return "invalid pattern: " + e.Err.Error() }
func (e *InvalidPatternError) Unwrap() error { return e.Err }

// defaultReadCap bounds how much of each file is read for content matching and
// frontmatter extraction when the caller passes no cap. Matches/metadata beyond
// this point in a single file are not seen — acceptable for the doc/notes trees
// this server orients (file_read is bounded the same way).
const defaultReadCap = 1 << 20 // 1 MiB

// binaryProbe is how many leading bytes are scanned for a NUL when classifying a
// file as binary (mirrors grrep's peek size and file_read's probe).
const binaryProbe = 8000

// metadataProbe bounds how many leading bytes a metadata-only enumeration reads
// to find the frontmatter fence. Frontmatter is conventionally a few hundred
// bytes; 64 KiB sits far above any real fence yet is a tiny fraction of a large
// file, so browsing a big tree with includeMetadata stays cheap. A fence that
// does not close within this window yields no metadata (the file is still
// listed).
const metadataProbe = 64 << 10 // 64 KiB

// Predicate is one content match over a file's body, AND-combined with the
// others: a file qualifies only if every predicate matches at least one line.
type Predicate struct {
	Text            string
	FixedString     bool
	CaseInsensitive bool
	WordBoundary    bool
}

// SearchRequest is a consolidated path+content query. PathGlob is a doublestar
// glob selecting candidate files ("" = the whole tree); Where holds the body
// predicates (empty = pure enumeration, no content read or grep needed).
type SearchRequest struct {
	PathGlob        string
	Where           []Predicate
	IncludeMatches  bool
	IncludeMetadata bool
}

// Line is one matched line within a file (no path — it is implied by the file).
type Line struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// FileResult is one matched file. Size is the file's byte size (always set, so a
// path-only enumeration still reports it). Matches holds body hits;
// MetadataMatches holds hits inside a leading `---`…`---` frontmatter fence;
// Metadata is the raw, unparsed frontmatter text (only when requested and a
// fence is present).
type FileResult struct {
	Path            string `json:"path"`
	Size            int64  `json:"size"`
	Matches         []Line `json:"matches,omitempty"`
	MetadataMatches []Line `json:"metadataMatches,omitempty"`
	Metadata        string `json:"metadata,omitempty"`
	// SHA256 is the hex SHA-256 of the file's full bytes, set only when the caller
	// passes IncludeMetadata — the same hash file_replace/file_overwrite check via
	// base_sha256, so a discovery pass can capture it for a follow-up edit (§8.7.4).
	SHA256 string `json:"sha256,omitempty"`
}

// SearchResult is the flat file list. Notice carries an optional steering hint
// (set by the caller when Truncated).
type SearchResult struct {
	Files     []FileResult `json:"files"`
	Truncated bool         `json:"truncated"`
	Notice    string       `json:"notice,omitempty"`
}

// Search locates files by path glob and body content in one pass. It compiles
// every predicate up front (a bad regex yields *InvalidPatternError and no
// walk), collects candidate files through the policy/ignore filter, narrows them
// by the glob, then scans them with a worker pool — opening each leaf through
// the workspace os.Root. A file qualifies only if all predicates match; matches
// inside a frontmatter fence are split out from body matches. The result list is
// path-sorted and capped at maxMatches (counting matched lines, or one per file
// when there are no matches), with Truncated set when the cap drops a file.
func Search(root *Root, pol *Policy, ig *grrep.IgnoreSet, req SearchRequest, workers, maxMatches int, readCap int64) (*SearchResult, error) {
	matchers := make([]*grrep.Matcher, len(req.Where))
	for i, p := range req.Where {
		m, err := grrep.CompileMatcher(p.Text, grrep.MatchOpts{
			FixedString:     p.FixedString,
			CaseInsensitive: p.CaseInsensitive,
			WordBoundary:    p.WordBoundary,
		})
		if err != nil {
			return nil, &InvalidPatternError{Err: err}
		}
		matchers[i] = m
	}

	// Narrow the walk to the glob's literal directory prefix (e.g. docs/**/*.md
	// walks only docs/), then match the full glob against each candidate's
	// workspace-relative path. The prefix is cleaned through fsroot so an
	// absolute or traversing glob base is rejected like any other path.
	walkStart := "."
	glob := strings.TrimSpace(req.PathGlob)
	if glob != "" {
		base, _ := doublestar.SplitPattern(glob)
		clean, err := Clean(base)
		if err != nil {
			return nil, err
		}
		walkStart = clean
	}

	if maxMatches <= 0 {
		maxMatches = 1000
	}
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if readCap <= 0 {
		readCap = defaultReadCap
	}

	files, err := collectFiles(root, pol, ig, walkStart)
	if err != nil {
		return nil, err
	}
	if glob != "" {
		kept := files[:0]
		for _, f := range files {
			if ok, _ := doublestar.Match(glob, f.Path); ok {
				kept = append(kept, f)
			}
		}
		files = kept
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	outRes := make([]FileResult, len(files))
	outInc := make([]bool, len(files))
	idxCh := make(chan int, 256)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idxCh {
				outRes[i], outInc[i] = scanOne(root, files[i], matchers, req, readCap)
			}
		}()
	}
	for i := range files {
		idxCh <- i
	}
	close(idxCh)
	wg.Wait()

	res := &SearchResult{Files: []FileResult{}}
	emitted := 0
	for i := range files {
		if !outInc[i] {
			continue
		}
		if emitted >= maxMatches {
			res.Truncated = true
			break
		}
		fr := outRes[i]
		res.Files = append(res.Files, fr)
		cost := len(fr.Matches) + len(fr.MetadataMatches)
		if cost == 0 {
			cost = 1
		}
		emitted += cost
	}
	return res, nil
}

// scanOne evaluates one candidate file. It returns the file's result and whether
// it qualifies. Three cases by cost:
//   - pure enumeration (no predicates, no metadata): never opens the file;
//   - metadata-only (no predicates, includeMetadata): opens the file but reads
//     only the leading frontmatter fence (bounded by metadataProbe), never the
//     body, so browsing a big tree stays cheap;
//   - content search (predicates present): reads up to readCap, skips binary,
//     requires every predicate to match, splits matches into body vs
//     frontmatter-fence by line, and lifts metadata from the same buffer.
func scanOne(root *Root, fm fileMeta, matchers []*grrep.Matcher, req SearchRequest, readCap int64) (FileResult, bool) {
	rel := fm.Path
	fr := FileResult{Path: rel, Size: fm.Size}

	if len(matchers) == 0 {
		if !req.IncludeMetadata {
			return fr, true // enumeration: the walk already proved the path exists
		}
		// Metadata-only: read just the frontmatter region, not the whole file.
		f, err := root.Open(rel)
		if err != nil {
			return fr, true // unreadable: still listed, no metadata
		}
		probe := int64(metadataProbe)
		if readCap > 0 && readCap < probe {
			probe = readCap
		}
		if meta, ok := readFrontmatter(f, probe); ok {
			fr.Metadata = meta
		}
		f.Close()
		fr.SHA256 = hashFileFull(root, rel, readCap)
		return fr, true
	}

	f, err := root.Open(rel)
	if err != nil {
		// Unreadable: a content predicate can't be satisfied.
		return fr, false
	}
	buf := make([]byte, readCap)
	n, _ := io.ReadFull(f, buf)
	f.Close()
	data := buf[:n]

	head := data
	if len(head) > binaryProbe {
		head = head[:binaryProbe]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return fr, false // binary: excluded from content search
	}

	hasFence, fenceEnd, metaText := detectFence(data)

	// Every predicate must match somewhere in the body.
	byLine := map[int]string{}
	for _, m := range matchers {
		ms := grrep.ScanBytes(rel, data, m)
		if len(ms) == 0 {
			return fr, false
		}
		for _, mm := range ms {
			byLine[mm.Line] = mm.Text
		}
	}

	if req.IncludeMatches && len(byLine) > 0 {
		lines := make([]int, 0, len(byLine))
		for l := range byLine {
			lines = append(lines, l)
		}
		sort.Ints(lines)
		for _, l := range lines {
			ln := Line{Line: l, Text: byLine[l]}
			if hasFence && l > 1 && l < fenceEnd { // strictly inside the fence
				fr.MetadataMatches = append(fr.MetadataMatches, ln)
			} else {
				fr.Matches = append(fr.Matches, ln)
			}
		}
	}
	if req.IncludeMetadata && hasFence {
		fr.Metadata = metaText
	}
	if req.IncludeMetadata {
		fr.SHA256 = hashFileFull(root, rel, readCap)
	}
	return fr, true
}

// hashFileFull streams the hex SHA-256 of a file's full bytes through the
// workspace os.Root, matching what base_sha256 checks (readForHash). It returns
// "" if the file is unreadable, errors mid-read, or exceeds cap (a file past the
// editable limit has no comparable base hash — file_replace would reject it with
// FILE_TOO_LARGE anyway). cap mirrors the workspace read limit.
func hashFileFull(root *Root, rel string, limit int64) string {
	f, err := root.Open(rel)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, limit+1))
	if err != nil || n > limit {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// detectFence locates a leading YAML-style frontmatter fence: line 1 is exactly
// `---` and a later line is exactly `---`. It returns whether a fence exists, the
// 1-based line number of the closing fence, and the raw text between the fences
// (unparsed). This is textual boundary-finding only — never a YAML parse.
func detectFence(data []byte) (ok bool, closeLine int, meta string) {
	if len(data) == 0 {
		return false, 0, ""
	}
	lines := bytes.Split(data, []byte("\n"))
	if isFence(lines[0]) {
		for i := 1; i < len(lines); i++ {
			if isFence(lines[i]) {
				body := make([]string, 0, i-1)
				for _, ln := range lines[1:i] {
					body = append(body, strings.TrimRight(string(ln), "\r"))
				}
				return true, i + 1, strings.Join(body, "\n")
			}
		}
	}
	return false, 0, ""
}

// isFence reports whether a line is a `---` frontmatter delimiter (ignoring a
// trailing CR or spaces).
func isFence(line []byte) bool {
	return isFenceText(string(line))
}

// isFenceText is isFence for a string line, tolerating a trailing newline so it
// can be fed lines straight from bufio.ReadString.
func isFenceText(s string) bool {
	return strings.TrimRight(s, "\r\n ") == "---"
}

// readFrontmatter reads only a leading `---`…`---` frontmatter fence from r,
// stopping as soon as the closing fence is found (or probe bytes are consumed)
// instead of reading the whole file — so a metadata-only enumeration pays for the
// fence, not the body. It returns the raw text between the fences (matching
// detectFence) and true, or ("", false) if the first line is not `---` or no
// closing fence appears within the probe window.
func readFrontmatter(r io.Reader, probe int64) (string, bool) {
	br := bufio.NewReader(io.LimitReader(r, probe))
	first, err := br.ReadString('\n')
	if !isFenceText(first) {
		return "", false
	}
	if err != nil {
		return "", false // the first line ran to EOF/limit; no closing fence can follow
	}
	var body []string
	for {
		line, err := br.ReadString('\n')
		if isFenceText(line) {
			return strings.Join(body, "\n"), true
		}
		if err != nil {
			return "", false // EOF or probe limit before a closing fence
		}
		body = append(body, strings.TrimRight(line, "\r\n"))
	}
}
