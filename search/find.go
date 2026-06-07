package search

import (
	"path"
	"sort"
	"strings"

	"github.com/mnehpets/workspace-mcp/fsroot"
	"github.com/mnehpets/workspace-mcp/grrep"
	"github.com/mnehpets/workspace-mcp/policy"
)

// FindResult lists matching workspace-relative file paths, best matches first.
// Truncated is set when more files matched than the limit returned; Notice
// carries an optional steering hint (set by the caller) for that case.
type FindResult struct {
	Files     []string `json:"files"`
	Truncated bool     `json:"truncated,omitempty"`
	Notice    string   `json:"notice,omitempty"`
}

// Find returns files whose name/path fuzzily matches query, filtered by policy
// and ignore rules. An empty query returns all eligible files (path-sorted).
func Find(root *fsroot.Root, pol *policy.Policy, ig *grrep.IgnoreSet, query string, limit int) (*FindResult, error) {
	files, err := collectFiles(root, pol, ig, ".")
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}

	type scored struct {
		path  string
		score int
	}
	var hits []scored
	for _, rel := range files {
		ps, pok := fuzzyScore(query, rel)
		bs, bok := fuzzyScore(query, path.Base(rel))
		if !pok && !bok {
			continue
		}
		score := ps
		if bok && bs+200 > score {
			score = bs + 200 // basename matches outrank deep-path matches
		}
		hits = append(hits, scored{rel, score})
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].path < hits[j].path
	})

	out := make([]string, 0, limit)
	for i := 0; i < len(hits) && i < limit; i++ {
		out = append(out, hits[i].path)
	}
	return &FindResult{Files: out, Truncated: len(hits) > limit}, nil
}

// fuzzyScore returns a relevance score and whether query matches target.
// A substring match scores highest (earlier = better); otherwise an in-order
// subsequence match scores low; no subsequence means no match.
func fuzzyScore(query, target string) (int, bool) {
	if query == "" {
		return 0, true
	}
	q := strings.ToLower(query)
	t := strings.ToLower(target)
	if idx := strings.Index(t, q); idx >= 0 {
		return 1000 - idx, true
	}
	ti := 0
	for qi := 0; qi < len(q); qi++ {
		c := q[qi]
		found := false
		for ti < len(t) {
			if t[ti] == c {
				found = true
				ti++
				break
			}
			ti++
		}
		if !found {
			return 0, false
		}
	}
	return 100, true
}
