// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

// Triage lane v1 contract tests (R-A through R-D). The real store, per
// memoire:D49 — the anti-join, the idempotency index, and the scope
// predicate are only meaningful against real SQL.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// agentAccept forges the ONE row no API path can produce: an accept
// transition rendered by an `agent:*` actor. The feed actor-gate (R-D)
// exists precisely so that a future machine verdict is excluded before it
// can be minted, so the falsifier has to write the review-log row directly.
func agentAccept(t *testing.T, store *Store, observationID int64, actor string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `UPDATE observations SET review_state = 'accepted' WHERE id = ?`, observationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO observation_review_log(
		observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch
	) VALUES (?, 'proposed', 'accepted', ?, NULL, ?)`, observationID, actor, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
}

func listIDs(t *testing.T, store *Store, filter ObservationFilter) map[int64]bool {
	t.Helper()
	recs, _, err := store.ListObservations(context.Background(), filter)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]bool{}
	for _, r := range recs {
		out[r.ID] = true
	}
	return out
}

// TestFeedActorGate is R-D's required test: a hypothetical non-human
// (`agent:*`) accept is feed-ineligible, while a rules-born row (no
// review-log accept entry at all) and a human accept remain eligible.
func TestFeedActorGate(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "feed-gate")
	ctx := reviewCtx("feed-gate")

	// Rules-born: accepted at birth, never through a verdict, so it carries
	// NO review-log accept entry. This is the 372k-row shape on the hub.
	rulesBorn := proposed(t, store, runID, eventID, "feed-gate", "fact", "rules-born accepted row")
	if _, err := store.db.ExecContext(context.Background(),
		`UPDATE observations SET review_state = 'accepted' WHERE id = ?`, rulesBorn); err != nil {
		t.Fatal(err)
	}

	humanAccepted := proposed(t, store, runID, eventID, "feed-gate", "fact", "a human accepted this one")
	if _, err := store.TransitionObservationReview(ctx, humanAccepted, "accepted", ""); err != nil {
		t.Fatal(err)
	}

	machineAccepted := proposed(t, store, runID, eventID, "feed-gate", "fact", "a machine accepted this one")
	agentAccept(t, store, machineAccepted, TriageActor("qwen3-30b"))

	// The EXPLICIT accepted band (GET /v1/observations?review_state=accepted;
	// first consumer Liaison's memoire_feed, retired 2026-11-01 — liaison
	// L0005) is the band that must gate.
	feed := listIDs(t, store, ObservationFilter{ReviewState: "accepted", Limit: 100})
	if !feed[rulesBorn] {
		t.Error("rules-born accepted row must stay feed-eligible — zero regression on the existing feed")
	}
	if !feed[humanAccepted] {
		t.Error("a human accept must stay feed-eligible")
	}
	if feed[machineAccepted] {
		t.Error("an agent:* accept must be feed-INELIGIBLE")
	}

	// The default (implicit) band is the injection surface and gates the
	// same way.
	implicit := listIDs(t, store, ObservationFilter{Limit: 100})
	if implicit[machineAccepted] {
		t.Error("an agent:* accept must be excluded from the default accepted band too")
	}
	if !implicit[rulesBorn] || !implicit[humanAccepted] {
		t.Error("the default band must keep rules-born and human-accepted rows")
	}

	// Fail-closed means the operator can still SEE the excluded row: the
	// gate hides it from the feed, never from review.
	visible := listIDs(t, store, ObservationFilter{ReviewState: "accepted", IncludeAgentAccepted: true, Limit: 100})
	if !visible[machineAccepted] {
		t.Error("include_agent_accepted must surface the gated row to the operator")
	}

	// The proposed queue must not acquire the anti-join: its plan is pinned
	// to the 0008 partial indexes via INDEXED BY, which fails the prepare
	// loudly if the predicate shape drifts.
	if _, _, err := store.ListObservations(context.Background(), ObservationFilter{ReviewState: "proposed", Limit: 10}); err != nil {
		t.Fatalf("proposed-queue read must keep its pinned index plan: %v", err)
	}
}

// TestTriageScopeIsInPlace pins R-A: proposed rows plus TTL-lapsed rows,
// with the human-lapsed row EXCLUDED — its verdict is respected and never
// re-opened by the sweep.
func TestTriageScopeIsInPlace(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "triage-scope")
	ctx := reviewCtx("triage-scope")

	stillProposed := proposed(t, store, runID, eventID, "triage-scope", "fact", "untouched proposed row")

	humanLapsed := proposed(t, store, runID, eventID, "triage-scope", "fact", "the operator lapsed this one")
	if _, err := store.TransitionObservationReview(ctx, humanLapsed, "lapsed", "not worth it"); err != nil {
		t.Fatal(err)
	}

	ttlLapsed := proposed(t, store, runID, eventID, "triage-scope", "fact", "the timer lapsed this one")
	if _, err := store.db.ExecContext(context.Background(),
		`UPDATE observations SET review_state = 'lapsed' WHERE id = ?`, ttlLapsed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO observation_review_log(
		observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch
	) VALUES (?, 'proposed', 'lapsed', ?, 'ttl', ?)`, ttlLapsed, LapseTTLActor, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	accepted := proposed(t, store, runID, eventID, "triage-scope", "fact", "already accepted, out of scope")
	if _, err := store.TransitionObservationReview(ctx, accepted, "accepted", ""); err != nil {
		t.Fatal(err)
	}

	rows, err := store.ListTriageScope(context.Background(), TriageScopeFilter{SessionID: "triage-scope", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, r := range rows {
		got[r.Observation.ID] = true
	}
	if !got[stillProposed] {
		t.Error("proposed rows are in scope")
	}
	if !got[ttlLapsed] {
		t.Error("TTL-lapsed rows are in scope — the pool is a filter, not a state")
	}
	if got[humanLapsed] {
		t.Error("a human-lapsed row must stay OUT of scope: no mass de-lapse, the human verdict stands")
	}
	if got[accepted] {
		t.Error("accepted rows are not reviewable and must be out of scope")
	}

	counts, err := store.TriageScopeCountsFor(context.Background(), "", "triage-scope")
	if err != nil {
		t.Fatal(err)
	}
	if counts.Proposed != 1 || counts.TTLLapsed != 1 || counts.Total != 2 {
		t.Errorf("triage surface must compute its OWN counters, got %+v", counts)
	}
}

// TestSuggestionsAreMetadataNeverState pins the hard constraint: writing a
// suggestion moves no review state, no provenance, no can_instruct.
func TestSuggestionsAreMetadataNeverState(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "triage-meta")
	id := proposed(t, store, runID, eventID, "triage-meta", "fact", "a row a judge will opine on")
	ctx := context.Background()

	before, err := store.GetObservationWithFilter(ctx, id, ObservationFilter{IncludeProposed: true})
	if err != nil {
		t.Fatal(err)
	}
	in := TriageSuggestionInput{
		ObservationID: id, Stage: 1, PromptVersion: TriagePromptStage1,
		Model: "qwen3-30b", SuggestedVerdict: TriageVerdictAccept,
	}
	if _, err := store.InsertTriageSuggestion(ctx, in); err != nil {
		t.Fatal(err)
	}
	after, err := store.GetObservationWithFilter(ctx, id, ObservationFilter{IncludeProposed: true})
	if err != nil {
		t.Fatal(err)
	}
	if after.ReviewState != before.ReviewState || after.Provenance != before.Provenance || after.CanInstruct != before.CanInstruct {
		t.Fatalf("a suggestion changed governed state: before=%+v after=%+v", before, after)
	}
	logRows, err := store.ListObservationReviewLog(ctx, ObservationReviewLogFilter{ObservationID: id, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(logRows) != 0 {
		t.Fatalf("a suggestion must write no review-log row, got %d", len(logRows))
	}

	// Idempotent per (observation, prompt version): the second pass is a
	// silent no-op, so re-suggestion is cheap and safe.
	if _, err := store.InsertTriageSuggestion(ctx, in); err != nil {
		t.Fatal(err)
	}
	sugg, err := store.listSuggestionsFor(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(sugg) != 1 {
		t.Fatalf("re-suggestion at the same prompt version must be idempotent, got %d rows", len(sugg))
	}
	if sugg[0].Actor != "agent:triage-qwen3-30b" {
		t.Errorf("suggestion actor must be agent:triage-<model>, got %q", sugg[0].Actor)
	}
}

func TestRejectRequiresClosedTaxonomyReason(t *testing.T) {
	cases := []struct {
		name string
		in   TriageSuggestionInput
		want string
	}{
		{"no reason", TriageSuggestionInput{ObservationID: 1, Stage: 1, PromptVersion: TriagePromptStage1, Model: "m", SuggestedVerdict: TriageVerdictReject}, "closed taxonomy"},
		{"open reason", TriageSuggestionInput{ObservationID: 1, Stage: 1, PromptVersion: TriagePromptStage1, Model: "m", SuggestedVerdict: TriageVerdictReject, ReasonGenre: "meh"}, "closed taxonomy"},
		{"other without text", TriageSuggestionInput{ObservationID: 1, Stage: 1, PromptVersion: TriagePromptStage1, Model: "m", SuggestedVerdict: TriageVerdictReject, ReasonGenre: TriageRejectOtherExplicit}, "non-empty reason_text"},
		{"duplicate without id", TriageSuggestionInput{ObservationID: 1, Stage: 1, PromptVersion: TriagePromptStage1, Model: "m", SuggestedVerdict: TriageVerdictReject, ReasonGenre: TriageRejectDuplicateOf}, "duplicated observation id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
	ok := TriageSuggestionInput{ObservationID: 1, Stage: 1, PromptVersion: TriagePromptStage1, Model: "m",
		SuggestedVerdict: TriageVerdictReject, ReasonGenre: TriageRejectFabrication}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a fabrication reject is well-formed: %v", err)
	}
}

func TestActorClassIsAClosedFold(t *testing.T) {
	cases := map[string]string{
		"cred-abc":               "human",
		"local":                  "human",
		LapseTTLActor:            "system",
		"agent:triage-qwen3-30b": "agent",
		"agent:anything":         "agent",
	}
	for actor, want := range cases {
		if got := ActorClass(actor); got != want {
			t.Errorf("ActorClass(%q) = %q, want %q", actor, got, want)
		}
	}
}

// TestReviewTransitionActorSplit pins R-D's metrics split: review-log
// derived counts separate human from system from agent BEFORE any
// suggestion-driven verdict could blur them.
func TestReviewTransitionActorSplit(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "actor-split")
	ctx := reviewCtx("actor-split")

	human := proposed(t, store, runID, eventID, "actor-split", "fact", "human accepts this")
	if _, err := store.TransitionObservationReview(ctx, human, "accepted", ""); err != nil {
		t.Fatal(err)
	}
	machine := proposed(t, store, runID, eventID, "actor-split", "fact", "machine accepts this")
	agentAccept(t, store, machine, TriageActor("qwen3-30b"))
	ttl := proposed(t, store, runID, eventID, "actor-split", "fact", "timer lapses this")
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO observation_review_log(
		observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch
	) VALUES (?, 'proposed', 'lapsed', ?, 'ttl', ?)`, ttl, LapseTTLActor, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	counts, err := store.CountReviewTransitionsByActorClass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[[2]string]int64{}
	for _, c := range counts {
		got[[2]string{c.ToState, c.ActorClass}] = c.Count
	}
	if got[[2]string{"accepted", "human"}] != 1 {
		t.Errorf("want one human accept, got %v", got)
	}
	if got[[2]string{"accepted", "agent"}] != 1 {
		t.Errorf("want one agent accept, got %v", got)
	}
	if got[[2]string{"lapsed", "system"}] != 1 {
		t.Errorf("want one system lapse, got %v", got)
	}
}

// TestOverrideRateIsComputedFromSource pins that applied-vs-flipped is read
// off the review log rather than stored beside the suggestion.
func TestOverrideRateIsComputedFromSource(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "override")
	ctx := reviewCtx("override")
	bg := context.Background()

	applied := proposed(t, store, runID, eventID, "override", "fact", "suggestion the operator honored")
	flipped := proposed(t, store, runID, eventID, "override", "fact", "suggestion the operator flipped")
	pending := proposed(t, store, runID, eventID, "override", "fact", "suggestion nobody ruled on")

	for _, id := range []int64{applied, flipped, pending} {
		if _, err := store.InsertTriageSuggestion(bg, TriageSuggestionInput{
			ObservationID: id, Stage: 1, PromptVersion: TriagePromptStage1,
			Model: "qwen3-30b", SuggestedVerdict: TriageVerdictLapse, ReasonGenre: "status-narration",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.TransitionObservationReview(ctx, applied, "lapsed", "agreed"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionObservationReview(ctx, flipped, "accepted", "actually useful"); err != nil {
		t.Fatal(err)
	}

	rates, err := store.TriageOverrideRates(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 1 {
		t.Fatalf("want one (prompt_version, verdict) cell, got %d: %+v", len(rates), rates)
	}
	r := rates[0]
	if r.PromptVersion != TriagePromptStage1 || r.Verdict != TriageVerdictLapse {
		t.Fatalf("cell must be split by prompt version AND verdict, got %+v", r)
	}
	if r.Suggested != 3 || r.Applied != 1 || r.Flipped != 1 || r.Pending != 1 {
		t.Fatalf("want suggested=3 applied=1 flipped=1 pending=1, got %+v", r)
	}
}

// TestGraduationEvidenceCarriesTheFlipSet pins R-B's falsifier input: a
// genre with any flip is reported WITH its flip set, which is what makes a
// graduation attempt refusable.
func TestGraduationEvidenceCarriesTheFlipSet(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "graduation")
	ctx := reviewCtx("graduation")
	bg := context.Background()

	var kept int64
	for i := 0; i < 5; i++ {
		id := proposed(t, store, runID, eventID, "graduation", "fact", "chatter row "+string(rune('a'+i)))
		if _, err := store.InsertTriageSuggestion(bg, TriageSuggestionInput{
			ObservationID: id, Stage: 1, PromptVersion: TriagePromptStage1,
			Model: "qwen3-30b", SuggestedVerdict: TriageVerdictNoiseLapse, ReasonGenre: "orchestration-chatter",
		}); err != nil {
			t.Fatal(err)
		}
		if i == 4 {
			kept = id
			if _, err := store.TransitionObservationReview(ctx, id, "accepted", "this one was a real outcome"); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if _, err := store.TransitionObservationReview(ctx, id, "lapsed", "noise"); err != nil {
			t.Fatal(err)
		}
	}

	ev, err := store.TriageGenreEvidence(bg, "orchestration-chatter")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Occurrences != 5 || ev.Judged != 5 {
		t.Fatalf("want 5 occurrences all judged, got %+v", ev)
	}
	if len(ev.FlipSet) != 1 || ev.FlipSet[0] != kept {
		t.Fatalf("the kept row must appear in the flip set, got %+v", ev.FlipSet)
	}

	// Below the evidence threshold, so it is not even suggested.
	cands, err := store.TriageGraduationCandidates(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("a genre under the evidence threshold must not be suggested, got %+v", cands)
	}
}

// A re-armed review TTL must NOT be able to feed the graduation falsifier.
// The un-pause condition of memoire:D0253 puts the timer back in play, and
// a timer lapsing a judge-flagged row is the timer agreeing with the judge,
// not the operator. Counting it would let the paused mechanism manufacture
// a zero-flip genre across the evidence threshold with no human attention.
func TestTimerCannotFeedTheGraduationFalsifier(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "ttl-evidence")
	bg := context.Background()

	id := proposed(t, store, runID, eventID, "ttl-evidence", "fact", "a row the judge flagged as noise")
	if _, err := store.InsertTriageSuggestion(bg, TriageSuggestionInput{
		ObservationID: id, Stage: 1, PromptVersion: TriagePromptStage1,
		Model: "qwen3-30b", SuggestedVerdict: TriageVerdictNoiseLapse, ReasonGenre: "orchestration-chatter",
	}); err != nil {
		t.Fatal(err)
	}
	// The TTL lapses it AFTER the suggestion — the exact re-armed-timer shape.
	if _, err := store.db.ExecContext(bg,
		`UPDATE observations SET review_state = 'lapsed' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(bg, `INSERT INTO observation_review_log(
		observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch
	) VALUES (?, 'proposed', 'lapsed', ?, 'ttl', ?)`, id, LapseTTLActor, time.Now().Unix()+10); err != nil {
		t.Fatal(err)
	}

	ev, err := store.TriageGenreEvidence(bg, "orchestration-chatter")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Judged != 0 {
		t.Fatalf("a TTL lapse must not count as a human ruling: judged=%d", ev.Judged)
	}
	rates, err := store.TriageOverrideRates(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 1 || rates[0].Applied != 0 || rates[0].Pending != 1 {
		t.Fatalf("a TTL lapse must count as pending, never applied: %+v", rates)
	}
}

// The operator's manual noise genre is surfaced, and only from HUMAN
// lapses: a TTL lapse carrying the same reason text would be the timer
// naming a genre.
func TestOperatorGenresComeOnlyFromHumanLapses(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "operator-genres")
	ctx := reviewCtx("operator-genres")
	bg := context.Background()

	human := proposed(t, store, runID, eventID, "operator-genres", "fact", "the operator names this genre")
	if _, err := store.TransitionObservationReview(ctx, human, "lapsed",
		TriageNoiseReasonPrefix+"session-mechanics"); err != nil {
		t.Fatal(err)
	}
	timer := proposed(t, store, runID, eventID, "operator-genres", "fact", "the timer would name this one")
	if _, err := store.db.ExecContext(bg, `INSERT INTO observation_review_log(
		observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch
	) VALUES (?, 'proposed', 'lapsed', ?, ?, ?)`,
		timer, LapseTTLActor, TriageNoiseReasonPrefix+"timer-invented", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	got, err := store.TriageOperatorGenres(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Genre != "session-mechanics" || got[0].Count != 1 {
		t.Fatalf("want exactly the human-named genre, got %+v", got)
	}
	if got[0].JudgeKnows {
		t.Error("no judge has claimed this slug — judge_knows must be false, which is the blind-spot signal")
	}
}
