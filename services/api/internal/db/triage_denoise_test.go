// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"testing"
)

// flagOnly is a row derivation flagged as noise with nothing else said about
// it: no suggestion, and therefore nothing a suggestion-keyed exclusion can
// ever mark handled. It is the shape every predicate in this file has to get
// right.
func flagOnly(t *testing.T, store *Store, runID string, eventID int64, session, text string) int64 {
	t.Helper()
	id := proposed(t, store, runID, eventID, session, "fact", text)
	if _, err := store.db.ExecContext(context.Background(),
		`UPDATE observations SET noise_candidate = 1 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func denoiseIDs(t *testing.T, store *Store, session string) map[int64]bool {
	t.Helper()
	rows, err := store.ListTriageScope(context.Background(), TriageScopeFilter{
		SessionID: session, Pool: TriagePoolDeNoise, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]bool{}
	for _, r := range rows {
		out[r.Observation.ID] = true
	}
	return out
}

func mainIDs(t *testing.T, store *Store, session string) map[int64]bool {
	t.Helper()
	rows, err := store.ListTriageScope(context.Background(), TriageScopeFilter{
		SessionID: session, Pool: TriagePoolMain, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]bool{}
	for _, r := range rows {
		out[r.Observation.ID] = true
	}
	return out
}

func noiseLapseSuggestion(t *testing.T, store *Store, id int64, genre string, confidence *float64) {
	t.Helper()
	if _, err := store.InsertTriageSuggestion(context.Background(), TriageSuggestionInput{
		ObservationID: id, Stage: 1, PromptVersion: TriagePromptStage1, Model: "judge",
		SuggestedVerdict: TriageVerdictNoiseLapse, ReasonGenre: genre, Confidence: confidence,
	}); err != nil {
		t.Fatal(err)
	}
}

// Pool membership: either noise signal alone admits a row, the two pools
// PARTITION the scope, and the counters say the same thing the reads do.
func TestDeNoisePoolMembershipAndMainPoolExclusion(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "denoise-pool")
	bg := context.Background()

	judged := proposed(t, store, runID, eventID, "denoise-pool", "fact", "the judge calls this noise")
	noiseLapseSuggestion(t, store, judged, "status-narration", nil)
	flagged := flagOnly(t, store, runID, eventID, "denoise-pool", "derivation flagged this one")
	lapsedNoise := ttlLapsed(t, store, runID, eventID, "denoise-pool", "timer-lapsed and judged noise")
	noiseLapseSuggestion(t, store, lapsedNoise, "orchestration-chatter", nil)
	ordinary := proposed(t, store, runID, eventID, "denoise-pool", "fact", "nothing says this is noise")

	pool := denoiseIDs(t, store, "denoise-pool")
	for _, id := range []int64{judged, flagged, lapsedNoise} {
		if !pool[id] {
			t.Errorf("row %d carries an unhandled noise signal and must be in the de-noise pool", id)
		}
	}
	if pool[ordinary] {
		t.Error("a row with no noise signal must not be in the de-noise pool")
	}

	// The pools PARTITION: no row is presented as two different jobs.
	main := mainIDs(t, store, "denoise-pool")
	if !main[ordinary] {
		t.Error("the ordinary row belongs to the main pool")
	}
	for id := range pool {
		if main[id] {
			t.Errorf("row %d is in both pools — the main pools must exclude the de-noise members", id)
		}
	}

	counts, err := store.TriageScopeCountsFor(bg, "", "denoise-pool")
	if err != nil {
		t.Fatal(err)
	}
	if counts.DeNoise != int64(len(pool)) {
		t.Errorf("de_noise count = %d, want %d", counts.DeNoise, len(pool))
	}
	if counts.Total != int64(len(main)) {
		t.Errorf("main-pool total = %d, want %d — the counts must exclude identified noise", counts.Total, len(main))
	}
	if counts.Proposed != 1 || counts.TTLLapsed != 0 {
		t.Errorf("main-pool proposed/ttl_lapsed = %d/%d, want 1/0", counts.Proposed, counts.TTLLapsed)
	}
}

// The SWEEP's scope must keep seeing noise rows. A main-pool exclusion
// leaking into the filter's empty default would starve the de-noise pool of
// exactly the judgments that resolve its doubt block.
func TestSweepScopeStillSeesNoiseRows(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "denoise-sweep")
	flagged := flagOnly(t, store, runID, eventID, "denoise-sweep", "flagged, and the judge has not opined")

	rows, err := store.ListTriageScope(context.Background(), TriageScopeFilter{
		SessionID: "denoise-sweep", Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Observation.ID == flagged {
			found = true
		}
	}
	if !found {
		t.Fatal("the whole-scope read (the sweep's) must still return noise-flagged rows")
	}
}

// A KEPT flag-only row is the trap the pool predicate exists for: it has no
// suggestion to mark handled, so an exclusion keyed on suggestions would
// re-admit it on every load, forever. It must leave the de-noise pool and
// arrive back in the ordinary queue with its accept-or-lapse question open.
func TestNotNoiseRescuesAFlagOnlyRowPermanently(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "denoise-keep")
	ctx := reviewCtx("denoise-keep")

	id := flagOnly(t, store, runID, eventID, "denoise-keep", "flagged, but the operator keeps it")
	if !denoiseIDs(t, store, "denoise-keep")[id] {
		t.Fatal("precondition: the flag-only row starts in the de-noise pool")
	}

	before, err := store.GetObservationWithFilter(context.Background(), id,
		ObservationFilter{IncludeProposed: true, IncludeNoise: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyObservationReviewAction(ctx, id, ReviewActionNotNoise, ""); err != nil {
		t.Fatalf("not-noise must be a legal review action: %v", err)
	}
	after, err := store.GetObservationWithFilter(context.Background(), id,
		ObservationFilter{IncludeProposed: true, IncludeNoise: true})
	if err != nil {
		t.Fatal(err)
	}
	// It changes NO flag: noise_candidate is derivation's recorded opinion,
	// and the keep is the operator's answer to it, not an erasure of it.
	if after.ReviewState != before.ReviewState || after.NoiseCandidate != before.NoiseCandidate ||
		after.CanInstruct != before.CanInstruct || after.CanEvidence != before.CanEvidence {
		t.Fatalf("not-noise must change nothing on the row: before=%+v after=%+v", before, after)
	}

	if denoiseIDs(t, store, "denoise-keep")[id] {
		t.Error("a kept row must leave the de-noise pool permanently")
	}
	if !mainIDs(t, store, "denoise-keep")[id] {
		t.Error("a kept row must return to the ordinary queue — its accept-or-lapse question is still open")
	}
}

// The mirror direction: a flag-only row the operator CONFIRMS as noise must
// drain from BOTH pools. Without an observation-level encounter exclusion it
// simply moves from the de-noise pool to the main lapsed pool and re-pins the
// page there instead — the same re-pin, relocated.
func TestMarkNoiseDrainsAFlagOnlyRowFromBothPools(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "denoise-confirm")
	ctx := reviewCtx("denoise-confirm")

	id := ttlLapsed(t, store, runID, eventID, "denoise-confirm", "timer-lapsed, flagged, confirmed noise")
	if _, err := store.db.ExecContext(context.Background(),
		`UPDATE observations SET noise_candidate = 1 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if !denoiseIDs(t, store, "denoise-confirm")[id] {
		t.Fatal("precondition: the row starts in the de-noise pool")
	}
	if _, err := store.ApplyObservationReviewAction(ctx, id, ReviewActionMarkNoise,
		TriageNoiseReasonPrefix+"session-mechanics"); err != nil {
		t.Fatal(err)
	}
	if denoiseIDs(t, store, "denoise-confirm")[id] {
		t.Error("a confirmed-noise row must leave the de-noise pool")
	}
	if mainIDs(t, store, "denoise-confirm")[id] {
		t.Error("a confirmed-noise row must not reappear in the main pool — that is the same re-pin, relocated")
	}
}

// An ACCEPT — the escape hatch, including on a lapsed row — takes the row out
// of the pool BY STATE, with nothing written about noise at all.
func TestAcceptEscapeHatchLeavesThePoolByState(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "denoise-accept")
	ctx := reviewCtx("denoise-accept")

	id := ttlLapsed(t, store, runID, eventID, "denoise-accept", "timer-lapsed noise that is actually good")
	noiseLapseSuggestion(t, store, id, "status-narration", nil)
	if !denoiseIDs(t, store, "denoise-accept")[id] {
		t.Fatal("precondition: the row starts in the de-noise pool")
	}
	// lapsed -> accepted is the on-encounter revival path (memoire:D146 R-6);
	// the escape hatch rides it rather than inventing a transition.
	rec, err := store.TransitionObservationReview(ctx, id, "accepted", "actually worth keeping")
	if err != nil {
		t.Fatalf("the escape hatch must ride the legal lapsed -> accepted transition: %v", err)
	}
	if rec.ReviewState != "accepted" {
		t.Fatalf("want accepted, got %q", rec.ReviewState)
	}
	if denoiseIDs(t, store, "denoise-accept")[id] {
		t.Error("an accepted row leaves the de-noise pool by STATE")
	}
	if mainIDs(t, store, "denoise-accept")[id] {
		t.Error("an accepted row leaves the reviewable scope entirely")
	}
}

// A DISAGREEMENT row — the judge said accept, derivation flagged noise — is
// in the pool and condemned by default like everything else in it. Deliberate:
// everything in de-noise is condemned unless kept.
func TestDisagreementRowIsInThePool(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "denoise-doubt")
	id := flagOnly(t, store, runID, eventID, "denoise-doubt", "flagged noise, judged accept")
	if _, err := store.InsertTriageSuggestion(context.Background(), TriageSuggestionInput{
		ObservationID: id, Stage: 1, PromptVersion: TriagePromptStage1, Model: "judge",
		SuggestedVerdict: TriageVerdictAccept,
	}); err != nil {
		t.Fatal(err)
	}
	if !denoiseIDs(t, store, "denoise-doubt")[id] {
		t.Error("a judge-vs-flag disagreement belongs in the de-noise pool, in its doubt block")
	}
}

// METRIC HYGIENE. A keep counts against the judge only where the judge
// actually claimed noise — and where it does, it counts as a FLIP whatever
// the row's state says.
func TestNotNoiseCountsOnlyAgainstANoiseClaim(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "denoise-metric")
	ctx := reviewCtx("denoise-metric")
	bg := context.Background()

	// (a) A keep on a flag-only row overrides NO verdict. There is no
	// suggestion, so nothing may appear in the override table at all.
	bare := flagOnly(t, store, runID, eventID, "denoise-metric", "flagged only, then kept")
	if _, err := store.ApplyObservationReviewAction(ctx, bare, ReviewActionNotNoise, ""); err != nil {
		t.Fatal(err)
	}
	rates, err := store.TriageOverrideRates(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 0 {
		t.Fatalf("a keep on a flag-only row overrides no judge verdict, got %+v", rates)
	}

	// (b) A keep on a TTL-LAPSED noise-lapse suggestion is a FLIP. The
	// action logs to_state == from_state == `lapsed`, which is exactly what
	// the suggestion wanted, so a state-only read would score the rescue as
	// agreement.
	lapsedKept := ttlLapsed(t, store, runID, eventID, "denoise-metric", "timer-lapsed, judged noise, kept")
	noiseLapseSuggestion(t, store, lapsedKept, "status-narration", nil)
	if _, err := store.ApplyObservationReviewAction(ctx, lapsedKept, ReviewActionNotNoise, ""); err != nil {
		t.Fatal(err)
	}
	rates, err = store.TriageOverrideRates(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 1 || rates[0].Flipped != 1 || rates[0].Applied != 0 {
		t.Fatalf("a keep on a noise-lapse suggestion is a flip, got %+v", rates)
	}

	// (c) And the graduation falsifier must SEE it: one row the operator
	// kept is one row a graduated genre would have destroyed unseen.
	ev, err := store.TriageGenreEvidence(bg, "status-narration")
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.FlipSet) != 1 || ev.FlipSet[0] != lapsedKept {
		t.Fatalf("the kept row must be in the genre's flip set, got %+v", ev)
	}
}

// A keep does NOT resolve a non-noise suggestion: the row went back to the
// ordinary queue with that question still open, so the suggestion stays
// honestly pending rather than being scored by an encounter that said
// nothing about it.
func TestNotNoiseLeavesANonNoiseSuggestionPending(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "denoise-pending")
	ctx := reviewCtx("denoise-pending")
	bg := context.Background()

	id := flagOnly(t, store, runID, eventID, "denoise-pending", "flagged noise, judged accept, kept")
	if _, err := store.InsertTriageSuggestion(bg, TriageSuggestionInput{
		ObservationID: id, Stage: 1, PromptVersion: TriagePromptStage1, Model: "judge",
		SuggestedVerdict: TriageVerdictAccept,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyObservationReviewAction(ctx, id, ReviewActionNotNoise, ""); err != nil {
		t.Fatal(err)
	}
	rates, err := store.TriageOverrideRates(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 1 || rates[0].Pending != 1 {
		t.Fatalf("the accept suggestion is untouched by a keep and stays pending, got %+v", rates)
	}
}

// Confidence is additive and OPTIONAL: it round-trips when stated, stays
// absent when not, and a value outside [0,1] never reaches storage.
func TestSuggestionConfidenceRoundTrip(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "denoise-conf")
	bg := context.Background()

	withConf := proposed(t, store, runID, eventID, "denoise-conf", "fact", "judged with a stated confidence")
	c := 0.42
	noiseLapseSuggestion(t, store, withConf, "status-narration", &c)
	silent := proposed(t, store, runID, eventID, "denoise-conf", "fact", "judged without one")
	noiseLapseSuggestion(t, store, silent, "status-narration", nil)

	got, err := store.listSuggestionsFor(bg, withConf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Confidence == nil || *got[0].Confidence != c {
		t.Fatalf("a stated confidence must round-trip, got %+v", got)
	}
	got, err = store.listSuggestionsFor(bg, silent)
	if err != nil {
		t.Fatal(err)
	}
	// Absent must stay absent: a v1-era suggestion made no confidence claim,
	// and reading that absence as 0 would pin the whole backlog into doubt.
	if len(got) != 1 || got[0].Confidence != nil {
		t.Fatalf("an unstated confidence must stay NULL, got %+v", got)
	}

	bad := 1.5
	if err := (TriageSuggestionInput{
		ObservationID: withConf, Stage: 1, PromptVersion: TriagePromptStage1, Model: "judge",
		SuggestedVerdict: TriageVerdictAccept, Confidence: &bad,
	}).Validate(); err == nil {
		t.Error("a confidence outside [0,1] must be refused at the boundary")
	}
}

// Graduation evidence counts ROWS, not suggestion rows. A prompt-version bump
// re-opens every judged row to re-suggestion, so counting claims would walk a
// genre across the threshold on a prompt change alone and push duplicate ids
// into the flip set.
func TestGenreEvidenceCountsRowsAcrossPromptVersions(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "denoise-versions")
	bg := context.Background()

	id := proposed(t, store, runID, eventID, "denoise-versions", "fact", "judged noise twice, at two prompt versions")
	for _, version := range []string{"triage-stage1-v1", TriagePromptStage1} {
		if _, err := store.InsertTriageSuggestion(bg, TriageSuggestionInput{
			ObservationID: id, Stage: 1, PromptVersion: version, Model: "judge",
			SuggestedVerdict: TriageVerdictNoiseLapse, ReasonGenre: "status-narration",
		}); err != nil {
			t.Fatal(err)
		}
	}
	ev, err := store.TriageGenreEvidence(bg, "status-narration")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Occurrences != 1 {
		t.Fatalf("one row claimed at two prompt versions is ONE occurrence of the genre, got %d", ev.Occurrences)
	}
}
