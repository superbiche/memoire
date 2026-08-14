// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

// Slice E (memoire:D146) contract tests: accept/provenance split (R-5),
// rewrite-as-supersede (R-5), lapsed state + TTL (R-6), session listing (E3).

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"memoire/services/api/internal/authctx"
	"memoire/services/api/internal/contenthash"
)

func sliceESeed(t *testing.T, session string) (*Store, int64, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	res, err := store.Ingest(ctx, EventInput{
		SessionID:       session,
		Project:         "memoire-test",
		SourceAdapter:   "test",
		SourceEventID:   "slice-e-" + session,
		EventType:       "PostToolUse",
		Payload:         []byte(`{"k":"v"}`),
		Text:            "seed event",
		OccurredAtEpoch: time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.EnsureDerivationRun(ctx, DerivationRunInput{
		Project: "memoire-test", SessionID: session, Provider: "rules",
		DataBoundary: "local", DerivationVersion: "v1", InputHash: "h-" + session,
		WindowStartEpoch: 1, WindowEndEpoch: 2, Status: "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, res.ID, run.ID
}

func reviewCtx(name string) context.Context {
	return authctx.With(context.Background(), authctx.Context{
		PrincipalID: "reviewer", CredentialID: "cred-" + name, RequestID: "req-" + name,
		Scopes: []string{"admin"},
	})
}

func proposed(t *testing.T, store *Store, runID string, eventID int64, session, kind, text string) int64 {
	t.Helper()
	hash, err := contenthash.Canonical("memoire-test", kind, text)
	if err != nil {
		t.Fatal(err)
	}
	in := DerivedObservationInput{
		Project: "memoire-test", SessionID: session, Kind: kind, Text: text,
		ReviewState: "proposed", DerivationRunID: runID, ContentHash: hash,
		SourceEventIDs: []int64{eventID}, Provenance: "inferred",
	}
	res, err := store.InsertDerivedObservation(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return res.ID
}

func TestLapsedLifecycle(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "lapse-life")
	ctx := reviewCtx("lapse-life")

	id := proposed(t, store, runID, eventID, "lapse-life", "fact", "will lapse then revive")
	lapsed, err := store.TransitionObservationReview(ctx, id, "lapsed", "not now")
	if err != nil {
		t.Fatal(err)
	}
	if lapsed.ReviewState != "lapsed" {
		t.Fatalf("want lapsed, got %s", lapsed.ReviewState)
	}

	// Lapsed rows are hidden from default reads but reachable by explicit filter.
	def, _, err := store.ListObservations(context.Background(), ObservationFilter{Project: "memoire-test", SessionID: "lapse-life", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range def {
		if o.ID == id {
			t.Fatal("lapsed row leaked into default read")
		}
	}
	byState, _, err := store.ListObservations(context.Background(), ObservationFilter{Project: "memoire-test", ReviewState: "lapsed", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(byState) != 1 || byState[0].ID != id {
		t.Fatalf("explicit lapsed filter must find the row, got %v", byState)
	}

	// Reversible on-encounter: lapsed -> accepted; provenance untouched.
	revived, err := store.TransitionObservationReview(ctx, id, "accepted", "still true")
	if err != nil {
		t.Fatal(err)
	}
	if revived.ReviewState != "accepted" || revived.Provenance != "inferred" {
		t.Fatalf("revival must accept without promoting provenance, got %s/%s", revived.ReviewState, revived.Provenance)
	}

	// Terminal detours stay closed.
	id2 := proposed(t, store, runID, eventID, "lapse-life", "decision", "lapsed cannot supersede")
	if _, err := store.TransitionObservationReview(ctx, id2, "lapsed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionObservationReview(ctx, id2, "superseded", ""); err == nil {
		t.Fatal("lapsed -> superseded must be rejected (rewrite is the only path)")
	}
}

func TestConfirmFromLapsedPromotesProvenance(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "lapse-confirm")
	ctx := reviewCtx("lapse-confirm")
	id := proposed(t, store, runID, eventID, "lapse-confirm", "fact", "revive with confirmation")
	if _, err := store.TransitionObservationReview(ctx, id, "lapsed", ""); err != nil {
		t.Fatal(err)
	}
	confirmed, err := store.ConfirmObservation(ctx, id, "verified on encounter")
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.ReviewState != "accepted" || confirmed.Provenance != "user_confirmed" {
		t.Fatalf("confirm from lapsed must accept+promote, got %s/%s", confirmed.ReviewState, confirmed.Provenance)
	}
}

func TestRewriteSupersedesOriginal(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "rewrite")
	ctx := reviewCtx("rewrite")
	origID := proposed(t, store, runID, eventID, "rewrite", "fact", "the API port is 8757 maybe")

	rec, err := store.RewriteObservation(ctx, origID, "The API listens on 127.0.0.1:37778.", "port was wrong")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ReviewState != "accepted" || rec.Provenance != "user_confirmed" {
		t.Fatalf("rewrite must be born accepted/user_confirmed, got %s/%s", rec.ReviewState, rec.Provenance)
	}
	if rec.ID == origID {
		t.Fatal("rewrite must create a NEW observation")
	}
	wantHash, err := contenthash.Canonical("memoire-test", "fact", "The API listens on 127.0.0.1:37778.")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ContentHash != wantHash {
		t.Fatalf("rewrite content_hash must track the new text: got %s want %s", rec.ContentHash, wantHash)
	}
	if len(rec.SourceEventIDs) != 1 || rec.SourceEventIDs[0] != eventID {
		t.Fatalf("rewrite must copy source-event links, got %v", rec.SourceEventIDs)
	}

	var supersedes int64
	var origState string
	if err := store.db.QueryRow(`SELECT COALESCE(supersedes_id,0) FROM observations WHERE id = ?`, rec.ID).Scan(&supersedes); err != nil {
		t.Fatal(err)
	}
	if supersedes != origID {
		t.Fatalf("filiation: new row must carry supersedes_id=%d, got %d", origID, supersedes)
	}
	if err := store.db.QueryRow(`SELECT review_state FROM observations WHERE id = ?`, origID).Scan(&origState); err != nil {
		t.Fatal(err)
	}
	if origState != "superseded" {
		t.Fatalf("original must be superseded, got %s", origState)
	}

	// From lapsed: same mechanics.
	lapsedID := proposed(t, store, runID, eventID, "rewrite", "decision", "lapsed then rewritten")
	if _, err := store.TransitionObservationReview(ctx, lapsedID, "lapsed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RewriteObservation(ctx, lapsedID, "the settled decision text", ""); err != nil {
		t.Fatal(err)
	}

	// From accepted: rejected (rewrite is a review-queue verdict).
	acceptedID := proposed(t, store, runID, eventID, "rewrite", "gotcha", "already accepted row")
	if _, err := store.TransitionObservationReview(ctx, acceptedID, "accepted", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RewriteObservation(ctx, acceptedID, "nope", ""); err == nil {
		t.Fatal("rewrite from accepted must be rejected")
	}
	// Empty text: rejected.
	if _, err := store.RewriteObservation(ctx, lapsedID, "   ", ""); err == nil {
		t.Fatal("empty rewrite text must be rejected")
	}
}

func TestLapseSessionRemaining(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "lapse-bulk")
	ctx := reviewCtx("lapse-bulk")
	a := proposed(t, store, runID, eventID, "lapse-bulk", "fact", "untreated one")
	b := proposed(t, store, runID, eventID, "lapse-bulk", "decision", "untreated two")
	c := proposed(t, store, runID, eventID, "lapse-bulk", "gotcha", "will be accepted first")
	if _, err := store.TransitionObservationReview(ctx, c, "accepted", ""); err != nil {
		t.Fatal(err)
	}

	n, err := store.LapseSessionRemaining(ctx, "lapse-bulk", "skimmed, rest lapses")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("must lapse exactly the 2 untreated rows, got %d", n)
	}
	for _, id := range []int64{a, b} {
		var state string
		if err := store.db.QueryRow(`SELECT review_state FROM observations WHERE id = ?`, id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "lapsed" {
			t.Fatalf("row %d must be lapsed, got %s", id, state)
		}
	}
	var cState string
	if err := store.db.QueryRow(`SELECT review_state FROM observations WHERE id = ?`, c).Scan(&cState); err != nil {
		t.Fatal(err)
	}
	if cState != "accepted" {
		t.Fatalf("accepted row must be untouched, got %s", cState)
	}
	// Idempotent.
	n2, err := store.LapseSessionRemaining(ctx, "lapse-bulk", "")
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("second bulk lapse must be 0, got %d", n2)
	}
}

func TestLapseExpiredProposedTTL(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "lapse-ttl")
	ctx := reviewCtx("lapse-ttl")
	oldUntouched := proposed(t, store, runID, eventID, "lapse-ttl", "fact", "old and never opened")
	oldTouched := proposed(t, store, runID, eventID, "lapse-ttl", "decision", "old but disputed once")
	fresh := proposed(t, store, runID, eventID, "lapse-ttl", "gotcha", "fresh")

	// Age the two "old" rows past the TTL.
	past := time.Now().Unix() - 30*86400
	for _, id := range []int64{oldUntouched, oldTouched} {
		if _, err := store.db.Exec(`UPDATE observations SET created_at_epoch = ? WHERE id = ?`, past, id); err != nil {
			t.Fatal(err)
		}
	}
	// Touch oldTouched: any review-log row means "opened", TTL must skip it.
	// (accept then dispute leaves it in a non-proposed state anyway; use a
	// pure log touch instead: accept and revert is not possible — dispute
	// requires accepted. Log the touch via a reject->nothing? Simplest true
	// "touched but still proposed" shape: a review-log row written by a
	// failed bulk gesture does not exist, so emulate an operator note by
	// inserting the log row directly.)
	if _, err := store.db.Exec(`INSERT INTO observation_review_log(observation_id, from_state, to_state, actor, occurred_at_epoch)
		VALUES (?, 'proposed', 'proposed', 'reviewer', ?)`, oldTouched, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	n, err := store.LapseExpiredProposed(ctx, 21)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("TTL must lapse exactly the old untouched row, got %d", n)
	}
	assertState := func(id int64, want string) {
		t.Helper()
		var state string
		if err := store.db.QueryRow(`SELECT review_state FROM observations WHERE id = ?`, id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != want {
			t.Fatalf("row %d: want %s, got %s", id, want, state)
		}
	}
	assertState(oldUntouched, "lapsed")
	assertState(oldTouched, "proposed")
	assertState(fresh, "proposed")

	// Attributable system actor on the log row.
	var actor string
	if err := store.db.QueryRow(`SELECT actor FROM observation_review_log WHERE observation_id = ? AND to_state = 'lapsed'`, oldUntouched).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if actor != LapseTTLActor {
		t.Fatalf("TTL lapse actor must be %s, got %s", LapseTTLActor, actor)
	}
}

func TestListSessionsCatchup(t *testing.T) {
	store, eventID, runID := sliceESeed(t, "catchup-a")
	ctx := reviewCtx("catchup")
	proposed(t, store, runID, eventID, "catchup-a", "fact", "workload one")
	id2 := proposed(t, store, runID, eventID, "catchup-a", "decision", "workload two")
	if _, err := store.TransitionObservationReview(ctx, id2, "lapsed", ""); err != nil {
		t.Fatal(err)
	}

	all, err := store.ListSessions(context.Background(), "memoire-test", false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].SessionID != "catchup-a" {
		t.Fatalf("want the seeded session, got %v", all)
	}
	if all[0].ProposedCount != 1 || all[0].LapsedCount != 1 || all[0].ObservationCount != 2 {
		t.Fatalf("counts wrong: %+v", all[0])
	}

	// proposedOnly filters out sessions with no outstanding work.
	if _, err := store.TransitionObservationReview(ctx, mustFirstProposedID(t, store, "catchup-a"), "accepted", ""); err != nil {
		t.Fatal(err)
	}
	withWork, err := store.ListSessions(context.Background(), "memoire-test", true, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(withWork) != 0 {
		t.Fatalf("proposedOnly must exclude drained sessions, got %v", withWork)
	}
}

func mustFirstProposedID(t *testing.T, store *Store, session string) int64 {
	t.Helper()
	var id int64
	if err := store.db.QueryRow(`SELECT id FROM observations WHERE session_id = ? AND review_state = 'proposed' LIMIT 1`, session).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
