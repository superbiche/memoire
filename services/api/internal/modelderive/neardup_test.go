// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"strings"
	"testing"

	"memoire/services/api/internal/db"
)

// The fixture pairs below are real observation texts from the 2026-08-02
// calibration corpus (tmp/neardup-calibration-2026-08-02.md) — the policy is
// asserted against the exact cases that shaped it.

func gateHit(t *testing.T, a, b string) (float64, bool) {
	t.Helper()
	return nearDupHit(buildNearDupProfile(a), buildNearDupProfile(b))
}

func TestNearDupParaphraseRejected(t *testing.T) {
	// The D211 founding pair: observations #347900/#348265 — same statement
	// about the same source event, two runs' paraphrases.
	score, hit := gateHit(t,
		`UI bug: "Load older" button disappears when a list is fully accepted/confirmed; expected to remain visible.`,
		`UI issue: 'Load older' button disappears after accepting/confirming a whole list; expected to remain visible.`)
	if !hit {
		t.Fatalf("founding paraphrase pair must gate: score=%.3f", score)
	}
}

func TestNearDupDigitGuardProtectsDistinctOutcomes(t *testing.T) {
	// Distinct facts whose whole difference is a digit token must survive:
	// the moderate tier requires equal digit sets, and these sit below the
	// unconditional tier.
	pairs := [][2]string{
		{
			"Background Codex xhigh security review of PR #67 completed successfully (exit code 0).",
			"Background Codex xhigh review of PR #69 completed successfully (exit code 0).",
		},
		{
			"v1 vs base judge verdict: Base model preferred 92% of the time (FT 2.97 vs Base 4.64).",
			"v2 vs base judge verdict: Base model preferred 95% of the time (v2 5/95).",
		},
	}
	for _, p := range pairs {
		if score, hit := gateHit(t, p[0], p[1]); hit {
			t.Fatalf("digit-differing distinct outcomes must not gate (score=%.3f):\nA: %s\nB: %s", score, p[0], p[1])
		}
	}
}

func TestNearDupHighTierCatchesTemplateBookkeeping(t *testing.T) {
	// Round-counter narration differing only in the iteration number crosses
	// the unconditional tier despite the digit mismatch — losing it is the
	// point (calibration: all >=0.90 residuals were this genre).
	score, hit := gateHit(t,
		`Background command "Run delta 3 re-check (effort re-issued per D289)" completed successfully with exit code 0.`,
		`Background command "Run delta 4 re-check (effort re-issued per D289)" completed successfully with exit code 0.`)
	if !hit || score < DefaultNearDupHiThreshold {
		t.Fatalf("template bookkeeping must cross the unconditional tier: score=%.3f hit=%v", score, hit)
	}
}

func TestNearDupNegationBlindnessDocumented(t *testing.T) {
	// KNOWN ACCEPTED BLINDNESS (operator-ruled 2026-08-02, calibration doc):
	// a digit-free update pair whose meaning flips on one word gates as a
	// dup. This test documents the accepted behavior — if it ever starts
	// failing, the policy changed and the calibration doc must be revisited.
	score, hit := gateHit(t,
		"The queue drainer is broken on the PostgreSQL engine",
		"The queue drainer is fixed on the PostgreSQL engine")
	if !hit {
		t.Fatalf("documented negation blindness no longer reproduces (score=%.3f) — policy changed?", score)
	}
}

func TestNearDupDistinctContentSurvives(t *testing.T) {
	score, hit := gateHit(t,
		"Minting must run with ~/.memoire/db.env sourced in-process; credential values must never appear in argv.",
		"The hourly sweep re-derived whole sessions because the event fetch had no coverage exclusion.")
	if hit {
		t.Fatalf("unrelated observations must not gate: score=%.3f", score)
	}
}

func TestFindNearDupPrefersStrongestMatch(t *testing.T) {
	refs := []nearDupRef{
		{profile: buildNearDupProfile("totally unrelated content about DNS records"), observationID: 1},
		{profile: buildNearDupProfile("the deploy gate requires a clean worktree before images-push"), observationID: 2},
	}
	match, hit := findNearDup("the deploy gate requires a clean worktree before running images-push", refs)
	if !hit || match.ref.observationID != 2 {
		t.Fatalf("expected strongest match against observation 2, got hit=%v match=%+v", hit, match)
	}
	if match.score < DefaultNearDupLoThreshold {
		t.Fatalf("winning score below lo threshold: %.3f", match.score)
	}
}

func TestRenderAlreadyCapturedEntryCap(t *testing.T) {
	list := make([]db.SessionObservationSummary, 0, DefaultAlreadyCapturedMaxEntries+5)
	for i := 0; i < DefaultAlreadyCapturedMaxEntries+5; i++ {
		list = append(list, db.SessionObservationSummary{ID: int64(i + 1), Kind: "fact", Text: "short text", ReviewState: "proposed"})
	}
	section, entries, truncated := renderAlreadyCaptured(list, DefaultAlreadyCapturedMaxBytes)
	if entries != DefaultAlreadyCapturedMaxEntries || !truncated {
		t.Fatalf("entry cap: entries=%d truncated=%v", entries, truncated)
	}
	if len(section) > DefaultAlreadyCapturedMaxBytes {
		t.Fatalf("section %d bytes exceeds its own budget", len(section))
	}
}

func TestRenderAlreadyCapturedByteBudgetAndClip(t *testing.T) {
	long := strings.Repeat("x", alreadyCapturedEntryBytes+50)
	list := []db.SessionObservationSummary{
		{ID: 1, Kind: "fact", Text: long, ReviewState: "accepted"},
		{ID: 2, Kind: "fact", Text: long, ReviewState: "accepted"},
	}
	// Budget fits the intro plus roughly one clipped entry line.
	budget := len(alreadyCapturedIntro) + alreadyCapturedEntryBytes + 40
	section, entries, truncated := renderAlreadyCaptured(list, budget)
	if !truncated || entries != 1 {
		t.Fatalf("byte budget must stop admission after one clipped entry: entries=%d truncated=%v", entries, truncated)
	}
	if len(section) > budget {
		t.Fatalf("section %d bytes exceeds budget %d", len(section), budget)
	}
	// The input slice's texts must be untouched (rendering clips copies).
	if len(list[0].Text) != alreadyCapturedEntryBytes+50 {
		t.Fatal("render mutated the caller's slice")
	}
}

func TestRenderAlreadyCapturedYieldsAway(t *testing.T) {
	list := []db.SessionObservationSummary{{ID: 1, Kind: "fact", Text: "abc", ReviewState: "proposed"}}
	// No room for even the intro: the section yields to nothing, loudly.
	section, entries, truncated := renderAlreadyCaptured(list, 40)
	if section != "" || entries != 0 || !truncated {
		t.Fatalf("undersized budget must omit the section and report truncation: %q entries=%d truncated=%v", section, entries, truncated)
	}
	if got := alreadyCapturedBudget(0, 50_000); got != DefaultAlreadyCapturedMaxBytes {
		t.Fatalf("no prompt cap must mean the default budget, got %d", got)
	}
	if got := alreadyCapturedBudget(70_000, 60_000); got != 10_000 {
		t.Fatalf("headroom must shrink the budget: got %d", got)
	}
}

func TestClipUTF8NeverSplitsRunes(t *testing.T) {
	s := strings.Repeat("é", 200) // 2 bytes per rune
	clipped := clipUTF8(s, 301)
	if len(clipped) != 300 {
		t.Fatalf("expected 300 bytes (rune boundary under 301), got %d", len(clipped))
	}
	for _, r := range clipped {
		if r == '�' {
			t.Fatal("clip produced an invalid rune")
		}
	}
}
