// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

// Derivation-time supersede at the STORE boundary (Fibery #344): the three
// gate outcomes, the CAS precondition, the filiation read shape, and the
// already-captured/gate-ref exclusion.

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"memoire/services/api/internal/contenthash"
)

// supersedeOfferMeta is the metadata shape the orchestrator hands the store
// for a winning offer.
func supersedeOfferMeta(t *testing.T, target int64) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"confidence_source": "model_self_report",
		"supersede": map[string]any{
			"of": target, "trigram": 0.59, "containment": 1.0, "growth": 0.4,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func insertSuperseding(t *testing.T, store *Store, ctx context.Context, runID string, eventID int64, session, kind, text string, matched []int64, offers []ObservationSupersedeOffer) DerivedObservationGateResult {
	t.Helper()
	hash, err := contenthash.Canonical("memoire-test", kind, text)
	if err != nil {
		t.Fatal(err)
	}
	conf := 0.9
	out, err := store.InsertDerivedObservationSuperseding(ctx, DerivedObservationInput{
		Project: "memoire-test", SessionID: session, Kind: kind, Text: text,
		Confidence: &conf, ReviewState: "proposed", DerivationRunID: runID,
		ContentHash: hash, SourceEventIDs: []int64{eventID}, ModelDerived: true,
		Metadata: json.RawMessage(`{"confidence_source":"model_self_report"}`),
	}, matched, offers)
	if err != nil {
		t.Fatalf("superseding insert: %v", err)
	}
	return out
}

func reviewLogOf(t *testing.T, store *Store, id int64) []ObservationReviewLogRecord {
	t.Helper()
	rows, err := store.ListObservationReviewLog(context.Background(), ObservationReviewLogFilter{ObservationID: id})
	if err != nil {
		t.Fatalf("review log: %v", err)
	}
	return rows
}

func auditActions(t *testing.T, store *Store, resourceID int64) []string {
	t.Helper()
	rows, err := store.rdb.QueryContext(context.Background(),
		`SELECT action FROM audit_log WHERE resource_type = 'observation' AND resource_id = ? ORDER BY id ASC`,
		strconv.FormatInt(resourceID, 10))
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func TestDeriveSupersedeWritesFiliationAndTransitionsTarget(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "sup-happy")
	ctx := reviewCtx("sup-happy")
	target := proposed(t, store, runID, eventID, "sup-happy", "fact", "the cut lands on an event seam")

	out := insertSuperseding(t, store, ctx, runID, eventID, "sup-happy", "fact",
		"the cut lands on an event seam and the guard vetoes stranded fragments",
		[]int64{target}, []ObservationSupersedeOffer{{TargetID: target, Metadata: supersedeOfferMeta(t, target)}})

	if out.LiveMatchID != 0 {
		t.Fatalf("an accepted offer must not reject: liveMatch=%d", out.LiveMatchID)
	}
	if out.SupersededID != target {
		t.Fatalf("supersededID = %d, want the target %d", out.SupersededID, target)
	}
	if out.Result.ID == 0 {
		t.Fatal("the successor row must be written")
	}

	// The new row: proposed, carrying filiation, with ITS OWN source-event
	// links (a later cut has events of its own — unlike a rewrite).
	rec, err := store.GetObservationWithFilter(context.Background(), out.Result.ID, ObservationFilter{IncludeProposed: true})
	if err != nil {
		t.Fatalf("get successor: %v", err)
	}
	if rec.ReviewState != "proposed" {
		t.Fatalf("successor state = %q, want proposed", rec.ReviewState)
	}
	if rec.SupersedesID == nil || *rec.SupersedesID != target {
		t.Fatalf("successor supersedes_id = %v, want %d", rec.SupersedesID, target)
	}
	if len(rec.SourceEventIDs) != 1 || rec.SourceEventIDs[0] != eventID {
		t.Fatalf("successor must carry this run's own source events, got %v", rec.SourceEventIDs)
	}
	var meta struct {
		Supersede struct {
			Of          int64   `json:"of"`
			Trigram     float64 `json:"trigram"`
			Containment float64 `json:"containment"`
			Growth      float64 `json:"growth"`
		} `json:"supersede"`
	}
	var metaRaw string
	if err := store.rdb.QueryRowContext(context.Background(),
		`SELECT meta_json FROM observations WHERE id = ?`, out.Result.ID).Scan(&metaRaw); err != nil {
		t.Fatalf("meta read: %v", err)
	}
	if err := json.Unmarshal([]byte(metaRaw), &meta); err != nil {
		t.Fatalf("meta decode: %v", err)
	}
	if meta.Supersede.Of != target || meta.Supersede.Trigram == 0 || meta.Supersede.Containment == 0 || meta.Supersede.Growth == 0 {
		t.Fatalf("the winning offer's supersede block must be persisted whole: %+v", meta.Supersede)
	}

	// The target: superseded, with an attributable review-log row and an
	// audit entry naming the successor.
	var state string
	if err := store.rdb.QueryRowContext(context.Background(),
		`SELECT review_state FROM observations WHERE id = ?`, target).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "superseded" {
		t.Fatalf("target state = %q, want superseded", state)
	}
	logRows := reviewLogOf(t, store, target)
	if len(logRows) != 1 {
		t.Fatalf("target must carry exactly one review-log row, got %d", len(logRows))
	}
	if logRows[0].FromState != "proposed" || logRows[0].ToState != "superseded" {
		t.Fatalf("review-log row = %s -> %s", logRows[0].FromState, logRows[0].ToState)
	}
	if logRows[0].Actor != DeriveSupersedeActor {
		t.Fatalf("actor = %q, want %q — a system supersede must never impersonate a human", logRows[0].Actor, DeriveSupersedeActor)
	}
	if got := auditActions(t, store, target); len(got) != 1 || got[0] != "observation.supersede" {
		t.Fatalf("audit actions on the target = %v, want [observation.supersede]", got)
	}
	var details string
	if err := store.rdb.QueryRowContext(context.Background(),
		`SELECT details_json FROM audit_log WHERE action = 'observation.supersede' AND resource_id = ?`, strconv.FormatInt(target, 10)).Scan(&details); err != nil {
		t.Fatal(err)
	}
	var d struct {
		SupersededBy int64 `json:"superseded_by"`
	}
	if err := json.Unmarshal([]byte(details), &d); err != nil {
		t.Fatal(err)
	}
	if d.SupersededBy != out.Result.ID {
		t.Fatalf("audit details superseded_by = %d, want %d", d.SupersededBy, out.Result.ID)
	}

	// The public state machine is NOT widened: the same transition through
	// the review verb is still illegal (memoire:D146).
	second := proposed(t, store, runID, eventID, "sup-happy", "gotcha", "another proposed row")
	if _, err := store.TransitionObservationReview(ctx, second, "superseded", "by hand"); err == nil {
		t.Fatal("proposed -> superseded must stay illegal through the review verb")
	}
}

func TestDeriveSupersedeCASLoserRejectsWithoutWriting(t *testing.T) {
	// The race the CAS exists for: the target moved between the gate's read
	// and the UPDATE. Driven deterministically on BOTH dialects by offering a
	// target whose state has already moved — the barrier harness in
	// enginepair_test can only interleave two writers on Postgres, where the
	// SQLite write pool is capped at one connection.
	store, eventID, runID := sliceESeed(t, "sup-cas")
	ctx := reviewCtx("sup-cas")
	target := proposed(t, store, runID, eventID, "sup-cas", "fact", "the cut lands on an event seam")
	if _, err := store.TransitionObservationReview(ctx, target, "accepted", "operator was faster"); err != nil {
		t.Fatalf("accept: %v", err)
	}

	before := len(reviewLogOf(t, store, target))
	out := insertSuperseding(t, store, ctx, runID, eventID, "sup-cas", "fact",
		"the cut lands on an event seam and the guard vetoes stranded fragments",
		[]int64{target}, []ObservationSupersedeOffer{{TargetID: target, Metadata: supersedeOfferMeta(t, target)}})

	if out.LiveMatchID != target {
		t.Fatalf("a CAS loser must reject against the moved target: liveMatch=%d want %d", out.LiveMatchID, target)
	}
	if out.SupersededID != 0 || out.Result.ID != 0 {
		t.Fatalf("a CAS loser must write nothing: superseded=%d inserted=%d", out.SupersededID, out.Result.ID)
	}
	var state string
	if err := store.rdb.QueryRowContext(context.Background(),
		`SELECT review_state FROM observations WHERE id = ?`, target).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "accepted" {
		t.Fatalf("the target must be untouched, got %q", state)
	}
	if n := len(reviewLogOf(t, store, target)); n != before {
		t.Fatalf("a CAS loser must write no review-log row: %d -> %d", before, n)
	}
	for _, a := range auditActions(t, store, target) {
		if a == "observation.supersede" {
			t.Fatal("a CAS loser must write no supersede audit entry")
		}
	}
	var children int
	if err := store.rdb.QueryRowContext(context.Background(),
		`SELECT count(*) FROM observations WHERE supersedes_id = ?`, target).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if children != 0 {
		t.Fatalf("a CAS loser must leave no successor, got %d", children)
	}
}

func TestDeriveSupersedeDeadTargetFallsThroughToPlainInsert(t *testing.T) {
	// An offered target that is no longer LIVE: the rejection it would have
	// justified is void (D212), so the candidate is WRITTEN — ungated, with no
	// filiation to a dead row.
	//
	// Scope, stated so the comment does not overclaim: this exercises the
	// ordinary no-live-match path, because firstLiveObservation returns 0 and
	// the CAS never runs. The CAS's zero-row-then-nothing-live sub-branch
	// (target soft-deleted BETWEEN the in-tx read and the UPDATE) needs two
	// interleaved writers and is unreachable deterministically on SQLite,
	// whose write pool is one connection; it is covered by construction — the
	// same firstLiveObservation call — not by this test.
	store, eventID, runID := sliceESeed(t, "sup-dead")
	ctx := reviewCtx("sup-dead")
	target := proposed(t, store, runID, eventID, "sup-dead", "fact", "the cut lands on an event seam")
	if _, err := store.TransitionObservationReview(ctx, target, "accepted", "prep"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionObservationReview(ctx, target, "soft_deleted", "gone mid-generation"); err != nil {
		t.Fatal(err)
	}

	out := insertSuperseding(t, store, ctx, runID, eventID, "sup-dead", "fact",
		"the cut lands on an event seam and the guard vetoes stranded fragments",
		[]int64{target}, []ObservationSupersedeOffer{{TargetID: target, Metadata: supersedeOfferMeta(t, target)}})

	if out.LiveMatchID != 0 || out.SupersededID != 0 {
		t.Fatalf("a dead target must neither reject nor be superseded: %+v", out)
	}
	if out.Result.ID == 0 {
		t.Fatal("the candidate must be written")
	}
	rec, err := store.GetObservationWithFilter(context.Background(), out.Result.ID, ObservationFilter{IncludeProposed: true})
	if err != nil {
		t.Fatal(err)
	}
	if rec.SupersedesID != nil {
		t.Fatalf("a plain insert must carry no filiation, got %v", rec.SupersedesID)
	}
}

func TestDeriveSupersedeNoOffersIsTodaysGate(t *testing.T) {
	// The kill switch's guarantee at the store boundary: with no offers the
	// superseding entry point behaves exactly like the gate it extends.
	store, eventID, runID := sliceESeed(t, "sup-off")
	ctx := context.Background()
	target := proposed(t, store, runID, eventID, "sup-off", "fact", "the cut lands on an event seam")

	out := insertSuperseding(t, store, ctx, runID, eventID, "sup-off", "fact",
		"the cut lands on an event seam and the guard vetoes stranded fragments",
		[]int64{target}, nil)
	if out.LiveMatchID != target || out.SupersededID != 0 || out.Result.ID != 0 {
		t.Fatalf("without an offer a live match must reject and write nothing: %+v", out)
	}
	// And the legacy entry point is unchanged.
	hash, _ := contenthash.Canonical("memoire-test", "fact", "legacy path text about the same seam")
	conf := 0.9
	_, live, err := store.InsertDerivedObservationUnlessMatchedLive(ctx, DerivedObservationInput{
		Project: "memoire-test", SessionID: "sup-off", Kind: "fact",
		Text: "legacy path text about the same seam", Confidence: &conf,
		ReviewState: "proposed", DerivationRunID: runID, ContentHash: hash,
		SourceEventIDs: []int64{eventID}, ModelDerived: true,
	}, []int64{target})
	if err != nil || live != target {
		t.Fatalf("the legacy gate entry point must be unchanged: live=%d err=%v", live, err)
	}
}

func TestSessionObservationsForDerivationExcludeSuperseded(t *testing.T) {
	// D5: a superseded row leaves both the prompt's already-captured section
	// and the gate's comparison set, because its content lives on in the
	// successor that is in the same list.
	store, eventID, runID := sliceESeed(t, "sup-list")
	ctx := reviewCtx("sup-list")
	target := proposed(t, store, runID, eventID, "sup-list", "fact", "the cut lands on an event seam")
	keep := proposed(t, store, runID, eventID, "sup-list", "decision", "chose rhythm cuts over halving")

	list, err := store.ListSessionObservationsForDerivation(ctx, "memoire-test", "sup-list", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("both proposed rows must be listed before the supersede, got %d", len(list))
	}

	out := insertSuperseding(t, store, ctx, runID, eventID, "sup-list", "fact",
		"the cut lands on an event seam and the guard vetoes stranded fragments",
		[]int64{target}, []ObservationSupersedeOffer{{TargetID: target, Metadata: supersedeOfferMeta(t, target)}})
	if out.SupersededID != target {
		t.Fatalf("fixture must supersede: %+v", out)
	}

	list, err = store.ListSessionObservationsForDerivation(ctx, "memoire-test", "sup-list", 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawTarget, sawKeep, sawSuccessor bool
	for _, o := range list {
		switch o.ID {
		case target:
			sawTarget = true
		case keep:
			sawKeep = true
		case out.Result.ID:
			sawSuccessor = true
		}
		if o.ReviewState == "superseded" {
			t.Fatalf("a superseded row must not be listed: %d", o.ID)
		}
	}
	if sawTarget {
		t.Fatal("the superseded target must leave the derivation list")
	}
	if !sawKeep || !sawSuccessor {
		t.Fatalf("the untouched row and the successor must both stay listed: keep=%v successor=%v", sawKeep, sawSuccessor)
	}
	// Reviewed-away content still shows: the exclusion is superseded-only,
	// not a quiet narrowing of the operator-ruled "all states" contract.
	if _, err := store.TransitionObservationReview(ctx, keep, "rejected", "not this"); err != nil {
		t.Fatal(err)
	}
	list, err = store.ListSessionObservationsForDerivation(ctx, "memoire-test", "sup-list", 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range list {
		if o.ID == keep && o.ReviewState == "rejected" {
			found = true
		}
	}
	if !found {
		t.Fatal("rejected rows must still be listed (operator-ruled 2026-08-02)")
	}
}

func TestObservationRecordCarriesRewriteFiliation(t *testing.T) {
	// The read shape is filiation-generic: an operator rewrite's successor
	// reads its pointer through the same field (memoire:D146 + #344).
	store, eventID, runID := sliceESeed(t, "sup-rw")
	ctx := reviewCtx("sup-rw")
	orig := proposed(t, store, runID, eventID, "sup-rw", "fact", "the API port is 8757 maybe")
	rec, err := store.RewriteObservation(ctx, orig, "The API listens on 127.0.0.1:37778.", "port was wrong")
	if err != nil {
		t.Fatal(err)
	}
	if rec.SupersedesID == nil || *rec.SupersedesID != orig {
		t.Fatalf("rewrite successor supersedes_id = %v, want %d", rec.SupersedesID, orig)
	}
	// And an ordinary row reads nil rather than zero.
	plain, err := store.GetObservationWithFilter(context.Background(),
		proposed(t, store, runID, eventID, "sup-rw", "gotcha", "a row with no ancestry"),
		ObservationFilter{IncludeProposed: true})
	if err != nil {
		t.Fatal(err)
	}
	if plain.SupersedesID != nil {
		t.Fatalf("a row with no ancestry must read nil, got %v", plain.SupersedesID)
	}
}
