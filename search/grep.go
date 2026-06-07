package search

import (
	"runtime"
	"sync"

	"github.com/mnehpets/workspace-mcp/fsroot"
	"github.com/mnehpets/workspace-mcp/grrep"
	"github.com/mnehpets/workspace-mcp/policy"
)

// InvalidPatternError signals an uncompilable regex (fixedString: false). The
// MCP layer maps it to INVALID_PATTERN.
type InvalidPatternError struct{ Err error }

func (e *InvalidPatternError) Error() string { return "invalid pattern: " + e.Err.Error() }
func (e *InvalidPatternError) Unwrap() error { return e.Err }

// GrepRequest is a content-search request. Path is a workspace-relative subtree
// to search ("" or "." for the whole workspace).
type GrepRequest struct {
	Pattern         string
	Path            string
	FixedString     bool
	CaseInsensitive bool
	WordBoundary    bool
}

// GrepResult is the search outcome. Notice carries an optional steering hint
// (set by the caller when Truncated) telling the model how to get the rest.
type GrepResult struct {
	Matches   []grrep.Match `json:"matches"`
	Truncated bool          `json:"truncated"`
	Notice    string        `json:"notice,omitempty"`
}

// Grep searches file contents under req.Path. It compiles the matcher (a bad
// regex yields *InvalidPatternError and no walk), collects candidate files
// through the policy/ignore filter, then scans them with a worker pool, opening
// every leaf through the workspace os.Root. Results are capped at maxMatches.
func Grep(root *fsroot.Root, pol *policy.Policy, ig *grrep.IgnoreSet, req GrepRequest, workers, maxMatches int) (*GrepResult, error) {
	m, err := grrep.CompileMatcher(req.Pattern, grrep.MatchOpts{
		FixedString:     req.FixedString,
		CaseInsensitive: req.CaseInsensitive,
		WordBoundary:    req.WordBoundary,
	})
	if err != nil {
		return nil, &InvalidPatternError{Err: err}
	}

	startRel, err := fsroot.Clean(req.Path)
	if err != nil {
		return nil, err
	}
	if maxMatches <= 0 {
		maxMatches = 1000
	}
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}

	files, err := collectFiles(root, pol, ig, startRel)
	if err != nil {
		return nil, err
	}

	paths := make(chan string, 256)
	var (
		mu        sync.Mutex
		matches   []grrep.Match
		truncated bool
		stop      bool
	)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rel := range paths {
				mu.Lock()
				done := stop
				mu.Unlock()
				if done {
					continue // drain channel without scanning
				}
				f, err := root.Open(rel)
				if err != nil {
					continue
				}
				fileMatches := grrep.ScanFile(rel, f, m)
				f.Close()
				if len(fileMatches) == 0 {
					continue
				}
				mu.Lock()
				for _, fm := range fileMatches {
					if len(matches) >= maxMatches {
						truncated = true
						stop = true
						break
					}
					matches = append(matches, fm)
				}
				mu.Unlock()
			}
		}()
	}

	for _, rel := range files {
		mu.Lock()
		done := stop
		mu.Unlock()
		if done {
			break
		}
		paths <- rel
	}
	close(paths)
	wg.Wait()

	return &GrepResult{Matches: matches, Truncated: truncated}, nil
}
