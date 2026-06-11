package gitaware

import (
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// upstreamWalkCap bounds commit walks. Counts at the cap are lower bounds;
// Capped signals the caller to say "1000+" rather than an exact number.
const upstreamWalkCap = 1000

// UpstreamInfo describes the local branch's relationship to its upstream
// tracking ref. All counts are computed against the local remote-tracking ref
// (no network fetch is performed — counts reflect the state as of last fetch).
type UpstreamInfo struct {
	Ref    string `json:"ref"`              // full tracking ref, e.g. "refs/remotes/origin/main"
	Ahead  int    `json:"ahead"`            // local commits not in upstream
	Behind int    `json:"behind"`           // upstream commits not in local
	InSync bool   `json:"inSync"`           // true when ahead==0 && behind==0
	Capped bool   `json:"capped,omitempty"` // true when any walk hit upstreamWalkCap
}

// resolveUpstream returns upstream tracking info for branchName. Returns nil when
// the branch has no upstream configured or the tracking ref doesn't exist locally.
//
// The tracking ref is constructed as refs/remotes/<remote>/<merge-short>, which
// assumes the standard fetch refspec (refs/heads/* → refs/remotes/<remote>/*).
// Non-standard refspecs may resolve to the wrong ref; that is accepted as a
// known limitation.
func resolveUpstream(repo *gogit.Repository, branchName string) *UpstreamInfo {
	if branchName == "" {
		return nil // detached HEAD or unborn
	}

	cfg, err := repo.Config()
	if err != nil {
		return nil
	}
	branch, ok := cfg.Branches[branchName]
	if !ok || branch.Remote == "" || branch.Merge == "" {
		return nil // no upstream configured
	}

	// Construct refs/remotes/<remote>/<branch> from the Merge refspec.
	trackingRef := plumbing.NewRemoteReferenceName(branch.Remote, branch.Merge.Short())

	remoteRef, err := repo.Reference(trackingRef, true)
	if err != nil {
		return nil // configured but tracking ref not fetched yet
	}

	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		return nil
	}

	info := &UpstreamInfo{Ref: string(trackingRef)}

	// Short-circuit: identical tips → in sync.
	if localRef.Hash() == remoteRef.Hash() {
		info.InSync = true
		return info
	}

	localCommit, err := repo.CommitObject(localRef.Hash())
	if err != nil {
		return nil // treat unresolvable tip like unconfigured
	}
	remoteCommit, err := repo.CommitObject(remoteRef.Hash())
	if err != nil {
		return nil
	}

	// Find merge base(s). Empty result (unrelated histories) → walk both sides
	// fully, capped, without any exclusion.
	bases, err := localCommit.MergeBase(remoteCommit)

	// Build seenExternal by walking all ancestors of each base. This is
	// necessary (not just passing base hashes as ignore) because go-git's
	// ignore list skips the exact hash but the parent-push loop uses
	// seenExternal to block pushing already-seen parents — so without
	// pre-populating it, ancestors of the base that are directly reachable
	// from a tip via a merge edge (not passing through the base) would be
	// counted incorrectly as ahead/behind.
	var seenExternal map[plumbing.Hash]bool
	var baseCapped bool
	if err == nil && len(bases) > 0 {
		seenExternal, baseCapped = collectBaseAncestors(bases)
	}

	ahead, aheadCapped := countCommitsFrom(localCommit, seenExternal)
	behind, behindCapped := countCommitsFrom(remoteCommit, seenExternal)
	info.Ahead = ahead
	info.Behind = behind
	info.InSync = ahead == 0 && behind == 0
	info.Capped = baseCapped || aheadCapped || behindCapped
	return info
}

// collectBaseAncestors walks from each merge base and returns the set of all
// reachable commit hashes. The caller passes this as seenExternal to
// countCommitsFrom so the ahead/behind walks stop at the base boundary
// (including all commits reachable from the base, not just the base itself).
func collectBaseAncestors(bases []*object.Commit) (map[plumbing.Hash]bool, bool) {
	seen := make(map[plumbing.Hash]bool)
	for _, base := range bases {
		iter := object.NewCommitPreorderIter(base, nil, nil)
		for {
			c, err := iter.Next()
			if err != nil {
				break
			}
			seen[c.Hash] = true
			if len(seen) >= upstreamWalkCap {
				iter.Close()
				return seen, true
			}
		}
		iter.Close()
	}
	return seen, false
}

// countCommitsFrom counts commits reachable from from that are not in
// seenExternal (the base ancestry set). Caps at upstreamWalkCap.
func countCommitsFrom(from *object.Commit, seenExternal map[plumbing.Hash]bool) (int, bool) {
	// Pre-check: if from is itself in seenExternal the iterator would skip it
	// and then try to read from an empty stack, causing a panic.
	if seenExternal[from.Hash] {
		return 0, false
	}
	iter := object.NewCommitPreorderIter(from, seenExternal, nil)
	defer iter.Close()
	count := 0
	for {
		if _, err := iter.Next(); err != nil {
			break
		}
		count++
		if count >= upstreamWalkCap {
			return count, true
		}
	}
	return count, false
}
