// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"testing"

	"memoire/services/api/internal/db"
)

// Derivation-time supersede classification (Fibery #344).
//
// Every case below starts from a pair the near-dup policy ALREADY HITS: the
// supersede verdict only re-classifies pairs the gate rejects today, so a
// fixture that does not hit would prove nothing. supersedeCase asserts the hit
// first and fails loudly when a fixture drifts out of the gate.

func supersedeCase(t *testing.T, oldText, newText, state string, intra bool) (trigram, containment, growth float64, ok bool) {
	t.Helper()
	score, hit := gateHit(t, newText, oldText)
	if !hit {
		t.Fatalf("fixture must HIT the near-dup policy to be a supersede candidate (score=%.3f)\nold: %s\nnew: %s", score, oldText, newText)
	}
	ref := nearDupRef{profile: buildNearDupProfile(oldText), observationID: 7, reviewState: state, intraResponse: intra}
	containment, growth, ok = supersedeVerdict(buildNearDupProfile(newText), ref)
	return score, containment, growth, ok
}

// The mid-arc pair this slice exists for: a cut taken while the session was
// still running said what it knew; the next cut says the same and one sentence
// more. Measured: trigram 0.595, containment 1.000, growth 0.405, +76 runes.
const (
	supersedeOldText = "The rhythm-cut detector walks the same population the prompt builder admits, so cuts land on event seams."
	supersedeNewText = supersedeOldText +
		" The min-content guard vetoes any cut that would strand a sub-floor fragment."
)

func TestSupersedeExtensionOfEarlierProposal(t *testing.T) {
	trigram, containment, growth, ok := supersedeCase(t, supersedeOldText, supersedeNewText, "proposed", false)
	if !ok {
		t.Fatalf("an extension of an earlier proposal must supersede: trigram=%.3f containment=%.3f growth=%.3f", trigram, containment, growth)
	}
	if containment < DefaultSupersedeContainment || growth < DefaultSupersedeGrowthRatio {
		t.Fatalf("verdict passed under its own thresholds: containment=%.3f growth=%.3f", containment, growth)
	}
	// The three scores are the calibration record; they must be measurements,
	// never placeholders.
	if trigram <= 0 || containment <= 0 || growth <= 0 {
		t.Fatalf("all three scores must be measured: trigram=%.3f containment=%.3f growth=%.3f", trigram, containment, growth)
	}
}

func TestSupersedeRefusedForNonProposedTarget(t *testing.T) {
	// A human-ruled row is superseded only by a human gesture (memoire:D146
	// rewrite). Every other state rejects exactly as today.
	for _, state := range []string{"accepted", "disputed", "rejected", "lapsed", "superseded", "soft_deleted", ""} {
		if _, _, _, ok := supersedeCase(t, supersedeOldText, supersedeNewText, state, false); ok {
			t.Fatalf("target in state %q must not be superseded by derivation", state)
		}
	}
}

func TestSupersedeRefusedForSameRunTarget(t *testing.T) {
	// Review round-1 F1: "not from this response" is not "from an earlier
	// run". A stale-running takeover reuses the run id, so the retry's session
	// read sees the dead attempt's rows — and superseding one would file two
	// rows under one run id. Built from the ref set the way the orchestrator
	// builds it, so the run-id plumbing is under test and not just the switch.
	summary := db.SessionObservationSummary{
		ID: 7, Kind: "fact", Text: supersedeOldText, ReviewState: "proposed",
		DerivationRunID: "drv_same",
	}
	cand := buildNearDupProfile(supersedeNewText)

	sameRun := nearDupRefsFromSession([]db.SessionObservationSummary{summary}, "drv_same")
	if _, _, ok := supersedeVerdict(cand, sameRun[0]); ok {
		t.Fatal("a target written by the run now executing must not be superseded")
	}

	otherRun := nearDupRefsFromSession([]db.SessionObservationSummary{summary}, "drv_other")
	if _, _, ok := supersedeVerdict(cand, otherRun[0]); !ok {
		t.Fatal("a target from a DIFFERENT run must still supersede")
	}

	// No run id at all — a rewrite successor — must stay eligible under the
	// ordinary conditions, never crash, and never silently count as same-run
	// (including when the current run id is itself empty, which is how a
	// caller with no run would look).
	noRun := summary
	noRun.DerivationRunID = ""
	for _, current := range []string{"drv_same", ""} {
		refs := nearDupRefsFromSession([]db.SessionObservationSummary{noRun}, current)
		if refs[0].sameRun {
			t.Fatalf("an empty run id must never match (current=%q)", current)
		}
		if _, _, ok := supersedeVerdict(cand, refs[0]); !ok {
			t.Fatalf("a run-less target must stay eligible (current=%q)", current)
		}
	}
}

func TestSupersedeRefusedForIntraResponseMatch(t *testing.T) {
	if _, _, _, ok := supersedeCase(t, supersedeOldText, supersedeNewText, "proposed", true); ok {
		t.Fatal("a match against an earlier candidate of the same response must not supersede")
	}
}

func TestSupersedeRefusedForEqualLengthParaphrase(t *testing.T) {
	// The D211 founding pair: same statement, same length, no added content.
	oldText := `UI bug: "Load older" button disappears when a list is fully accepted/confirmed; expected to remain visible.`
	newText := `UI issue: 'Load older' button disappears after accepting/confirming a whole list; expected to remain visible.`
	_, containment, growth, ok := supersedeCase(t, oldText, newText, "proposed", false)
	if ok {
		t.Fatalf("an equal-length paraphrase must stay rejected: containment=%.3f growth=%.3f", containment, growth)
	}
}

func TestSupersedeRefusedInHiBand(t *testing.T) {
	// trigram >= 0.90 means near-identical texts, which makes the growth ratio
	// unreachable by construction. Asserted as a band property, not merely as
	// one verdict.
	oldText := `Background command "Run delta 3 re-check (effort re-issued per D289)" completed successfully with exit code 0.`
	newText := `Background command "Run delta 4 re-check (effort re-issued per D289)" completed successfully with exit code 0.`
	trigram, _ := gateHit(t, newText, oldText)
	if trigram < DefaultNearDupHiThreshold {
		t.Fatalf("fixture must sit in the hi band to test it: trigram=%.3f", trigram)
	}
	_, _, growth, ok := supersedeCase(t, oldText, newText, "proposed", false)
	if ok {
		t.Fatalf("a hi-band near-identical restatement must stay rejected (growth=%.3f)", growth)
	}
	if growth >= DefaultSupersedeGrowthRatio {
		t.Fatalf("hi-band growth %.3f should be unreachable; the band claim is wrong", growth)
	}
}

func TestSupersedeGrowthCharBoundary(t *testing.T) {
	// The absolute floor is the boundary a long reword would otherwise slip
	// past: 39 added runes reject, 40 supersede, everything else held equal
	// (the addition is appended, so containment stays 1 and the growth ratio
	// clears comfortably in both halves — only the floor decides).
	base := "The sweep derives the open tail of a running session on every tick, mid-arc, and that is the whole point of the marker."
	// Distinct words, not a repeated character: a run of one rune adds a
	// single trigram and would fail the RATIO instead, testing nothing.
	just39 := base + " extra distinct words follow here to gr"
	just40 := base + " extra distinct words follow here to gro"
	delta := func(s string) int {
		return buildNearDupProfile(s).runeLen - buildNearDupProfile(base).runeLen
	}
	if d := delta(just39); d != DefaultSupersedeGrowthChars-1 {
		t.Fatalf("fixture drift: the under-floor case grows %d normalized runes, want %d", d, DefaultSupersedeGrowthChars-1)
	}
	if d := delta(just40); d != DefaultSupersedeGrowthChars {
		t.Fatalf("fixture drift: the at-floor case grows %d normalized runes, want %d", d, DefaultSupersedeGrowthChars)
	}
	_, _, growth, ok := supersedeCase(t, base, just39, "proposed", false)
	if ok {
		t.Fatal("39 added runes must not clear the absolute growth floor")
	}
	if growth < DefaultSupersedeGrowthRatio {
		t.Fatalf("the under-floor case must fail on the FLOOR, not the ratio (growth=%.3f)", growth)
	}
	if _, _, _, ok := supersedeCase(t, base, just40, "proposed", false); !ok {
		t.Fatal("40 added runes must clear the absolute growth floor")
	}
}

func TestSupersedeContainmentBoundary(t *testing.T) {
	// Containment is directional and independent of growth: a candidate that
	// DROPS part of what the target said is a replacement the machine may not
	// make, however much it adds elsewhere.
	kept := "The derive sweep cuts a session on its rhythm first and halves only what rhythm left too big, per the ruling."
	oldText := kept + " Caps and thresholds are calibration values."
	newText := kept + " The open tail is now marked provisional so a later measurement can tell scaffolding from durable content."
	_, containment, growth, ok := supersedeCase(t, oldText, newText, "proposed", false)
	if containment >= DefaultSupersedeContainment {
		t.Fatalf("fixture must drop enough of the target to sit under the line: containment=%.3f", containment)
	}
	if ok {
		t.Fatalf("containment %.3f below the threshold must reject", containment)
	}
	// Containment must be the ONLY failing condition, or the case proves
	// nothing about containment (measured: 0.766 / growth 0.476 / +62 runes).
	if growth < DefaultSupersedeGrowthRatio {
		t.Fatalf("fixture also fails the growth ratio (%.3f): containment is not isolated", growth)
	}
	if d := buildNearDupProfile(newText).runeLen - buildNearDupProfile(oldText).runeLen; d < DefaultSupersedeGrowthChars {
		t.Fatalf("fixture also fails the growth floor (+%d runes): containment is not isolated", d)
	}
}

func TestSupersedeBandIsBoundedByTheGateItself(t *testing.T) {
	// The load-bearing consequence of condition 1 (supersede only
	// re-classifies pairs the gate already HITS), stated as arithmetic and
	// pinned so nobody has to re-derive it from a surprise in production.
	//
	// For a pure extension (containment 1, i = |tri(old)|):
	//     trigram J = |tri(old)| / |tri(new)|   and   growth = 1 - J.
	// So growth >= 0.15 requires J <= 0.85, and the lo tier requires
	// J >= 0.45, which caps growth at 0.55. A candidate that grows MORE than
	// that — roughly more than doubling the target's length — falls out of the
	// gate entirely and lands as an ordinary new row: today's behavior, no
	// content lost, but no filiation recorded either.
	base := "The rhythm-cut detector walks the same population the prompt builder admits, so cuts land on event seams."
	tooBig := base +
		" The min-content guard then vetoes any cut that would strand a sub-floor fragment, which stays with its neighbour." +
		" A boundary never splits events sharing one whole-second epoch."
	score, hit := gateHit(t, tooBig, base)
	if hit {
		t.Fatalf("fixture is meant to sit BELOW the gate: trigram=%.3f", score)
	}
	if _, growth := supersedeScores(buildNearDupProfile(tooBig), buildNearDupProfile(base)); growth <= 1-DefaultNearDupLoThreshold {
		t.Fatalf("an out-of-band extension must exceed growth %.2f; got %.3f", 1-DefaultNearDupLoThreshold, growth)
	}
	// And the in-band extension of the same target IS a supersede, so the
	// boundary is the gate's, not the verdict's.
	if _, _, _, ok := supersedeCase(t, supersedeOldText, supersedeNewText, "proposed", false); !ok {
		t.Fatal("the in-band extension must still supersede")
	}
}

func TestSupersedeScoresAreDirectional(t *testing.T) {
	// Pure-extension arithmetic: every trigram of the target is present in the
	// candidate (containment 1) while the candidate carries novel ones.
	small := buildNearDupProfile("alpha beta")
	big := buildNearDupProfile("alpha beta gamma delta")
	if c, g := supersedeScores(big, small); c != 1 || g <= 0 {
		t.Fatalf("extension must contain fully and grow: containment=%.3f growth=%.3f", c, g)
	}
	// Reversed, the shorter text neither contains nor grows on the longer one.
	if c, g := supersedeScores(small, big); c >= 1 || g != 0 {
		t.Fatalf("the reverse direction must not look like an extension: containment=%.3f growth=%.3f", c, g)
	}
}
