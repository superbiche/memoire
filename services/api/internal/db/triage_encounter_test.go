// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func ttlLapsed(t *testing.T, store *Store, runID string, eventID int64, session, text string) int64 {
	t.Helper()
	id := proposed(t, store, runID, eventID, session, "fact", text)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx,
		`UPDATE observations SET review_state = 'lapsed' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO observation_review_log(
		observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch
	) VALUES (?, 'proposed', 'lapsed', ?, 'ttl', ?)`, id, LapseTTLActor, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	return id
}

// The operator's encounter with a TTL-lapsed row — the headline workload of
// this lane — must be durably recorded WITHOUT widening the state machine,
// must count as the human ruling, and must stop the row re-pinning the queue.
func TestHumanEncounterOnATTLLapsedRow(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "encounter")
	ctx := reviewCtx("encounter")
	bg := context.Background()

	id := ttlLapsed(t, store, runID, eventID, "encounter", "a timer-lapsed row the operator now reads")
	if _, err := store.InsertTriageSuggestion(bg, TriageSuggestionInput{
		ObservationID: id, Stage: 1, PromptVersion: TriagePromptStage1,
		Model: "qwen3-30b", SuggestedVerdict: TriageVerdictNoiseLapse, ReasonGenre: "status-narration",
	}); err != nil {
		t.Fatal(err)
	}

	// The transition route cannot carry this encounter, and must not learn to.
	if _, err := store.TransitionObservationReview(ctx, id, "lapsed", "agreed"); err == nil {
		t.Fatal("lapsed -> lapsed must remain illegal — the state machine is not to be widened")
	}

	// It rides the D125 append-only action path instead.
	if _, err := store.ApplyObservationReviewAction(ctx, id, ReviewActionMarkNoise,
		TriageNoiseReasonPrefix+"session-mechanics"); err != nil {
		t.Fatalf("marking noise must be recordable as a flag-only action: %v", err)
	}
	rec, err := store.GetObservationWithFilter(bg, id, ObservationFilter{IncludeProposed: true, IncludeNoise: true})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ReviewState != "lapsed" {
		t.Errorf("the encounter must not move review_state, got %q", rec.ReviewState)
	}
	if rec.NoiseCandidate != 1 {
		t.Error("marking noise must set the noise_candidate flag")
	}

	// It counts as the HUMAN ruling for the override rate...
	rates, err := store.TriageOverrideRates(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 1 || rates[0].Applied != 1 || rates[0].Pending != 0 {
		t.Fatalf("a human encounter must resolve the suggestion, got %+v", rates)
	}
	// ...and for graduation evidence.
	ev, err := store.TriageGenreEvidence(bg, "status-narration")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Judged != 1 || len(ev.FlipSet) != 0 {
		t.Fatalf("the affirmed lapse is evidence for the genre, got %+v", ev)
	}
	// The operator's own slug survives.
	og, err := store.TriageOperatorGenres(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(og) != 1 || og[0].Genre != "session-mechanics" {
		t.Fatalf("the manual genre must persist, got %+v", og)
	}

	// And the handled row stops re-pinning the queue.
	rows, err := store.ListTriageScope(bg, TriageScopeFilter{SessionID: "encounter", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Observation.ID == id {
			t.Error("a row carrying a post-suggestion human ruling must leave the queue")
		}
	}
}

// Affirming a lapse changes no flag at all — the entry IS the record — and
// still resolves the suggestion.
func TestAffirmLapseIsAPureRecord(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "affirm")
	ctx := reviewCtx("affirm")
	bg := context.Background()

	id := ttlLapsed(t, store, runID, eventID, "affirm", "timer-lapsed, and the operator agrees")
	if _, err := store.InsertTriageSuggestion(bg, TriageSuggestionInput{
		ObservationID: id, Stage: 1, PromptVersion: TriagePromptStage1,
		Model: "m", SuggestedVerdict: TriageVerdictLapse, ReasonGenre: "thin-restatement",
	}); err != nil {
		t.Fatal(err)
	}
	before, err := store.GetObservationWithFilter(bg, id, ObservationFilter{IncludeProposed: true, IncludeNoise: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyObservationReviewAction(ctx, id, ReviewActionAffirmLapse, "read it, it stays"); err != nil {
		t.Fatal(err)
	}
	after, err := store.GetObservationWithFilter(bg, id, ObservationFilter{IncludeProposed: true, IncludeNoise: true})
	if err != nil {
		t.Fatal(err)
	}
	if after.ReviewState != before.ReviewState || after.NoiseCandidate != before.NoiseCandidate ||
		after.CanInstruct != before.CanInstruct || after.CanEvidence != before.CanEvidence {
		t.Fatalf("affirming a lapse must change nothing: before=%+v after=%+v", before, after)
	}
	rates, err := store.TriageOverrideRates(bg)
	if err != nil {
		t.Fatal(err)
	}
	if rates[0].Applied != 1 {
		t.Fatalf("the affirmation must resolve the suggestion, got %+v", rates)
	}
}

// The OLDER governance flag edits must not be read as rulings on a suggested
// verdict: they say nothing about it, and scoring them would turn an
// unrelated flag edit into agreement or a flip.
func TestGovernanceFlagEditsAreNotRulings(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "flag-edit")
	ctx := reviewCtx("flag-edit")
	bg := context.Background()

	id := proposed(t, store, runID, eventID, "flag-edit", "fact", "a row someone marks stale")
	if _, err := store.InsertTriageSuggestion(bg, TriageSuggestionInput{
		ObservationID: id, Stage: 1, PromptVersion: TriagePromptStage1,
		Model: "m", SuggestedVerdict: TriageVerdictAccept,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyObservationReviewAction(ctx, id, ReviewActionMarkStale, "aging"); err != nil {
		t.Fatal(err)
	}
	rates, err := store.TriageOverrideRates(bg)
	if err != nil {
		t.Fatal(err)
	}
	if rates[0].Pending != 1 || rates[0].Flipped != 0 || rates[0].Applied != 0 {
		t.Fatalf("a mark-stale flag edit is not a ruling on the suggestion, got %+v", rates)
	}
	rows, err := store.ListTriageScope(bg, TriageScopeFilter{SessionID: "flag-edit", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the row must stay in the queue — nobody has ruled on it, got %d rows", len(rows))
	}
}

// A hostile genre never reaches storage as a structured slug.
func TestGenreSlugGrammarIsEnforced(t *testing.T) {
	for _, hostile := range []string{
		"$(printf TRIAGE_PWNED)", "../../etc/pwned", "has spaces", "UPPER",
		"-leading", "with_underscore", "quote's", "semi;colon", "back\u0060tick",
		"", strings.Repeat("a", 65),
	} {
		if hostile == "" {
			continue // empty means "no genre", which is legal
		}
		in := TriageSuggestionInput{
			ObservationID: 1, Stage: 1, PromptVersion: TriagePromptStage1, Model: "m",
			SuggestedVerdict: TriageVerdictNoiseLapse, ReasonGenre: hostile,
		}
		if err := in.Validate(); err == nil {
			t.Errorf("genre %q must be refused at the boundary", hostile)
		}
	}
	// A trailing hyphen conforms to the ruled grammar and is harmless for
	// both a shell word and a path segment; it is kept legal rather than
	// tightened past what was ruled.
	for _, ok := range []string{"status-narration", "a", "orchestration-chatter", "genre-2", "trailing-"} {
		in := TriageSuggestionInput{
			ObservationID: 1, Stage: 1, PromptVersion: TriagePromptStage1, Model: "m",
			SuggestedVerdict: TriageVerdictNoiseLapse, ReasonGenre: ok,
		}
		if err := in.Validate(); err != nil {
			t.Errorf("genre %q is well-formed: %v", ok, err)
		}
	}
}

// Stage 2 runs over the HUMAN-validated set only: a rules-born accepted row
// was accepted at birth by a rule and never passed a human eye, so it is not
// material for a vouch or a standing order.
func TestStage2ScopeIsHumanValidatedOnly(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "stage2-scope")
	ctx := reviewCtx("stage2-scope")
	bg := context.Background()

	rulesBorn := proposed(t, store, runID, eventID, "stage2-scope", "fact", "rules-born accepted row")
	if _, err := store.db.ExecContext(bg, `UPDATE observations SET review_state='accepted' WHERE id=?`, rulesBorn); err != nil {
		t.Fatal(err)
	}
	human := proposed(t, store, runID, eventID, "stage2-scope", "fact", "a human accepted this")
	if _, err := store.TransitionObservationReview(ctx, human, "accepted", ""); err != nil {
		t.Fatal(err)
	}
	machine := proposed(t, store, runID, eventID, "stage2-scope", "fact", "a machine accepted this")
	agentAccept(t, store, machine, TriageActor("qwen3-30b"))

	rows, err := store.ListHumanValidatedObservations(bg, "", "stage2-scope", 50)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, r := range rows {
		got[r.ID] = true
	}
	if !got[human] {
		t.Error("a human-accepted row is the stage-2 material")
	}
	if got[rulesBorn] {
		t.Error("a rules-born accepted row was never validated by anyone and must be OUT of the stage-2 scope")
	}
	if got[machine] {
		t.Error("a machine accept is not a human validation")
	}
}

// The override rate carries the stage-2 arm, read from the STORED column,
// and no verdict class is permanently pending.
func TestOverrideRatesCarryTheArmAndResolve(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "arms")
	ctx := reviewCtx("arms")
	bg := context.Background()

	// Two accepted rows, one per arm (assignment is id parity).
	var armA, armB int64
	for armA == 0 || armB == 0 {
		id := proposed(t, store, runID, eventID, "arms", "fact", fmt.Sprintf("validated row %d", armA+armB))
		if _, err := store.TransitionObservationReview(ctx, id, "accepted", ""); err != nil {
			t.Fatal(err)
		}
		if TriageStage2Arm(id) == TriageArmA && armA == 0 {
			armA = id
		} else if TriageStage2Arm(id) == TriageArmB && armB == 0 {
			armB = id
		}
	}
	for _, id := range []int64{armA, armB} {
		if _, err := store.InsertTriageSuggestion(bg, TriageSuggestionInput{
			ObservationID: id, Stage: 2, PromptVersion: TriagePromptStage2,
			Model: "m", SuggestedVerdict: TriageVerdictElevate, Arm: TriageStage2Arm(id),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Arm A's suggestion is taken; arm B's is declined.
	if _, err := store.ConfirmObservation(ctx, armA, "vouched"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ElevateObservation(ctx, armA, "standing order"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyObservationReviewAction(ctx, armB, ReviewActionDeclineElevation, "not a standing order"); err != nil {
		t.Fatal(err)
	}

	rates, err := store.TriageOverrideRates(bg)
	if err != nil {
		t.Fatal(err)
	}
	byArm := map[string]TriageOverrideRate{}
	for _, r := range rates {
		if r.Stage == 2 {
			byArm[r.Arm] = r
		}
	}
	if len(byArm) != 2 {
		t.Fatalf("the metric must split by arm, got %+v", rates)
	}
	if byArm[TriageArmA].Applied != 1 || byArm[TriageArmA].Pending != 0 {
		t.Errorf("arm A: an applied elevation must read as applied, got %+v", byArm[TriageArmA])
	}
	if byArm[TriageArmB].Flipped != 1 || byArm[TriageArmB].Pending != 0 {
		t.Errorf("arm B: a declined elevation must resolve as flipped, never sit pending, got %+v", byArm[TriageArmB])
	}
}

// The handled-row exclusion is STAGE-AWARE. The primary operator flow — a
// stage-1 suggestion the operator then accepts — is exactly what should
// arrive at stage 2; a stage-blind predicate read that accept as "already
// handled" and made stage 2 unreachable on the normal path.
func TestHandledRowExclusionIsStageAware(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "stage-aware")
	ctx := reviewCtx("stage-aware")
	bg := context.Background()

	id := proposed(t, store, runID, eventID, "stage-aware", "fact", "judge suggested accept, operator accepted")
	if _, err := store.InsertTriageSuggestion(bg, TriageSuggestionInput{
		ObservationID: id, Stage: 1, PromptVersion: TriagePromptStage1,
		Model: "m", SuggestedVerdict: TriageVerdictAccept,
	}); err != nil {
		t.Fatal(err)
	}
	// The accept lands at or after the suggestion, which is what made the
	// stage-blind exclusion fire.
	if _, err := store.db.ExecContext(bg, `UPDATE observation_triage_suggestions SET created_at_epoch = created_at_epoch - 10 WHERE observation_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionObservationReview(ctx, id, "accepted", ""); err != nil {
		t.Fatal(err)
	}

	// Stage 1 is handled — the accept answered the stage-1 suggestion.
	stage1, err := store.ListTriageScope(bg, TriageScopeFilter{Stage: 1, SessionID: "stage-aware", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range stage1 {
		if r.Observation.ID == id {
			t.Error("the stage-1 suggestion was answered; the row must leave the stage-1 queue")
		}
	}
	// Stage 2 has asked nothing yet, so the row is squarely in its scope.
	stage2, err := store.ListHumanValidatedObservations(bg, "", "stage-aware", 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range stage2 {
		if r.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("a stage-1 suggestion answered by a human accept is the PRIMARY stage-2 input and must reach it")
	}

	// Once stage 2 has suggested and the operator has ruled, it leaves too.
	if _, err := store.InsertTriageSuggestion(bg, TriageSuggestionInput{
		ObservationID: id, Stage: 2, PromptVersion: TriagePromptStage2,
		Model: "m", SuggestedVerdict: TriageVerdictElevate, Arm: TriageStage2Arm(id),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(bg, `UPDATE observation_triage_suggestions SET created_at_epoch = created_at_epoch - 10 WHERE observation_id = ? AND stage = 2`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyObservationReviewAction(ctx, id, ReviewActionDeclineElevation, "not a standing order"); err != nil {
		t.Fatal(err)
	}
	stage2, err = store.ListHumanValidatedObservations(bg, "", "stage-aware", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range stage2 {
		if r.ID == id {
			t.Error("a ruled stage-2 suggestion must take the row out of the stage-2 scope")
		}
	}
}

// Stage-2 scope excludes rules-born and machine accepts through the SAME
// shared predicate the paging uses.
func TestStage2ScopeExcludesUnvalidatedAccepts(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "s2-unvalidated")
	ctx := reviewCtx("s2-unvalidated")
	bg := context.Background()

	rulesBorn := proposed(t, store, runID, eventID, "s2-unvalidated", "fact", "rules-born accepted row")
	if _, err := store.db.ExecContext(bg, `UPDATE observations SET review_state='accepted' WHERE id=?`, rulesBorn); err != nil {
		t.Fatal(err)
	}
	machine := proposed(t, store, runID, eventID, "s2-unvalidated", "fact", "a machine accepted this")
	agentAccept(t, store, machine, TriageActor("qwen3-30b"))
	human := proposed(t, store, runID, eventID, "s2-unvalidated", "fact", "a human accepted this")
	if _, err := store.TransitionObservationReview(ctx, human, "accepted", ""); err != nil {
		t.Fatal(err)
	}

	rows, err := store.ListHumanValidatedObservations(bg, "", "s2-unvalidated", 50)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, r := range rows {
		got[r.ID] = true
	}
	if !got[human] || got[rulesBorn] || got[machine] {
		t.Fatalf("stage-2 scope must be human-validated only, got %v", got)
	}
}

// A duplicate action append — the lost-response retry the action route
// cannot rule out — must not inflate any metric or flip a queue exclusion.
// This pins ALL FIVE readers that touch the review log: the override rate
// and graduation evidence take one row per suggestion through a LIMIT-1
// scalar subquery, the queue exclusion is EXISTS-shaped, the operator-genre
// count is DISTINCT over observations, and the actor-split census counts
// TRANSITIONS only, excluding flag-verb action rows entirely.
func TestReadersAreDuplicateTolerant(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "dup-tolerant")
	ctx := reviewCtx("dup-tolerant")
	bg := context.Background()

	id := ttlLapsed(t, store, runID, eventID, "dup-tolerant", "a row marked noise twice")
	if _, err := store.InsertTriageSuggestion(bg, TriageSuggestionInput{
		ObservationID: id, Stage: 1, PromptVersion: TriagePromptStage1,
		Model: "m", SuggestedVerdict: TriageVerdictNoiseLapse, ReasonGenre: "status-narration",
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.ApplyObservationReviewAction(ctx, id, ReviewActionMarkNoise,
			TriageNoiseReasonPrefix+"session-mechanics"); err != nil {
			t.Fatal(err)
		}
	}

	rates, err := store.TriageOverrideRates(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 1 || rates[0].Suggested != 1 || rates[0].Applied != 1 {
		t.Fatalf("three duplicate appends must yield one applied suggestion, got %+v", rates)
	}
	ev, err := store.TriageGenreEvidence(bg, "status-narration")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Occurrences != 1 || ev.Judged != 1 {
		t.Fatalf("graduation evidence must count suggestions, not log rows, got %+v", ev)
	}
	og, err := store.TriageOperatorGenres(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(og) != 1 || og[0].Count != 1 {
		t.Fatalf("operator genres must count DISTINCT observations, not log rows, got %+v", og)
	}
	rows, err := store.ListTriageScope(bg, TriageScopeFilter{SessionID: "dup-tolerant", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatal("the queue exclusion is EXISTS-shaped: duplicates cannot flip it back")
	}

	// The fifth reader, published as memoire_review_transitions on the
	// UNAUTHENTICATED surface. A flag-verb row is not a transition — it
	// writes from_state == to_state — so counting one would publish a state
	// change that never happened, and three duplicate appends would publish
	// it three times.
	census, err := store.CountReviewTransitionsByActorClass(bg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range census {
		if c.ActorClass == "human" {
			t.Errorf("three mark-noise actions are not %d human transitions to %q", c.Count, c.ToState)
		}
	}
	// The TTL lapse behind the row is a real transition and is still counted.
	found := false
	for _, c := range census {
		if c.ToState == "lapsed" && c.ActorClass == "system" && c.Count == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("the real system transition must still be counted, got %+v", census)
	}
}

// Coverage rows are bookkeeping, not suggestions: never a verdict class in
// the override rate, never counted as "suggested" on the surface.
func TestCoverageRowsAreNotSuggestions(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "coverage")
	bg := context.Background()

	id := proposed(t, store, runID, eventID, "coverage", "fact", "the judge nominated nothing here")
	inserted, err := store.InsertTriageSuggestion(bg, TriageSuggestionInput{
		ObservationID: id, Stage: 1, PromptVersion: TriagePromptStage1,
		Model: "m", SuggestedVerdict: TriageVerdictNone,
	})
	if err != nil || !inserted {
		t.Fatalf("a coverage row must be storable: inserted=%v err=%v", inserted, err)
	}
	rates, err := store.TriageOverrideRates(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 0 {
		t.Fatalf("a coverage row nominates nothing, so it can never be applied or flipped: %+v", rates)
	}
	counts, err := store.TriageScopeCountsFor(bg, "", "coverage")
	if err != nil {
		t.Fatal(err)
	}
	if counts.Suggested != 0 {
		t.Errorf("a coverage row must not read as a suggestion on the surface, got %+v", counts)
	}
	if counts.Proposed != 1 {
		t.Errorf("the row itself still needs a human ruling, got %+v", counts)
	}
	// And the row stays in the queue: the judge had no opinion, the
	// operator still has to rule.
	rows, err := store.ListTriageScope(bg, TriageScopeFilter{SessionID: "coverage", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("a coverage row must not remove the row from the queue, got %d", len(rows))
	}
}
