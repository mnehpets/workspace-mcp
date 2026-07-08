package mcp

import (
	"fmt"
	"testing"
	"time"

	"github.com/mnehpets/http/endpoint"
)

// A fresh server for internal tests, with a controllable clock so auth-code
// expiry is deterministic.
func testOAuth(now *time.Time) *OAuthServer {
	return NewOAuthServer("cid", "client-secret-long-enough-xxxxxx").
		WithClock(func() time.Time { return *now })
}

// sweepExpiredLocked reclaims memory from auth codes that aged out without being
// redeemed — otherwise the map would only ever shrink on redemption.
func TestSweepExpiredCodes(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base
	s := testOAuth(&now)

	s.codes["fresh"] = authCode{expiry: base.Add(2 * oauthCodeTTL)}
	s.codes["stale"] = authCode{expiry: base.Add(oauthCodeTTL / 2)}

	now = base.Add(oauthCodeTTL) // past "stale", before "fresh"
	s.mu.Lock()
	s.sweepExpiredLocked()
	s.mu.Unlock()

	if _, ok := s.codes["stale"]; ok {
		t.Error("expired code should have been swept")
	}
	if _, ok := s.codes["fresh"]; !ok {
		t.Error("unexpired code must survive the sweep")
	}
}

// The cap is a memory backstop: once the map is full, a further issue is refused
// (503) rather than growing without bound — and a refused issue neither inserts
// nor consumes the single-use console nonce.
func TestIssueCodeCapRejects(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base
	s := testOAuth(&now)

	for i := 0; i < oauthMaxCodes; i++ {
		s.codes[fmt.Sprintf("c%d", i)] = authCode{expiry: base.Add(oauthCodeTTL)}
	}
	s.nonce = "approve-me"
	s.nonceAt = base

	r, err := s.issueCode(authorizeParams{
		ClientID:    "cid",
		RedirectURI: "https://claude.ai/cb",
		Nonce:       "approve-me",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	jr, ok := r.(*endpoint.JSONRenderer)
	if !ok || jr.Status != 503 {
		t.Fatalf("want 503 JSON error at cap, got %#v", r)
	}
	if len(s.codes) != oauthMaxCodes {
		t.Errorf("cap breached: %d codes stored", len(s.codes))
	}
	if s.nonce != "approve-me" {
		t.Error("a capped (rejected) issue must not consume the console nonce")
	}
}

// ensureChallengeLocked prints only when it generates: repeat calls within the
// TTL neither reprint nor rotate the code.
func TestEnsureChallengeStableWithinTTL(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base
	s := testOAuth(&now)

	s.mu.Lock()
	s.ensureChallengeLocked()
	first := s.nonce
	now = base.Add(oauthNonceTTL / 2)
	s.ensureChallengeLocked()
	second := s.nonce
	s.mu.Unlock()
	if first == "" || first != second {
		t.Fatalf("nonce rotated within TTL: %q -> %q", first, second)
	}

	now = base.Add(oauthNonceTTL + time.Second)
	s.mu.Lock()
	s.ensureChallengeLocked()
	third := s.nonce
	s.mu.Unlock()
	if third == first {
		t.Fatal("nonce should rotate after the TTL elapses")
	}
}
