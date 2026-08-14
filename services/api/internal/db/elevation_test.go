// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"memoire/services/api/internal/authctx"
)

// Elevation contract (docs/ELEVATION-DESIGN.md, amended by memoire:D146
// R-5): elevate sets can_instruct=1 on an accepted observation whose
// provenance is ALREADY user_confirmed/imported (the confirm verdict's
// output) — it never promotes provenance itself. Demote flips can_instruct
// back to 0 with provenance untouched; both write review-log entries.
// Anything not in the required state is ErrElevationConflict (HTTP 409),
// never a trigger abort.
func TestElevateDemoteLifecycle(t *testing.T) {
	store, eventID, runID := governanceSeed(t, "elevation-lifecycle")
	defer store.Close()

	ctx := authctx.With(context.Background(), authctx.Context{
		PrincipalID:  "operator",
		CredentialID: "cred-elevation",
		RequestID:    "req-elevation",
		Scopes:       []string{"admin"},
	})

	// Slice E (memoire:D146, R-5): elevation is the SECOND human gesture —
	// it requires provenance user_confirmed/imported (the confirm verdict's
	// output) and never promotes provenance itself. Accepted/inferred rows
	// get a clean 409 pointing at confirm.
	id := insertTestDerivedObservation(t, store, runID, eventID, "elevation-lifecycle", "constraint", "elevation lifecycle target", "proposed", "sha256:elevation-lifecycle")
	if _, err := store.TransitionObservationReview(ctx, id, "accepted", "plain accept"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ElevateObservation(ctx, id, "too early"); !errors.Is(err, ErrElevationConflict) {
		t.Fatalf("elevate from accepted/inferred must conflict (confirm first), got %v", err)
	}
	// The failed elevate must not have touched the row.
	var provenance string
	if err := store.db.QueryRowContext(context.Background(), `SELECT provenance FROM observations WHERE id = ?`, id).Scan(&provenance); err != nil {
		t.Fatal(err)
	}
	if provenance != "inferred" {
		t.Fatalf("failed elevate must leave provenance untouched, got %q", provenance)
	}

	// Second gesture unlocked by confirm... but the row is already accepted;
	// confirm is not valid from accepted. Use a fresh row: confirm, then
	// elevate — two POSTs, two review-log rows.
	id = insertTestDerivedObservation(t, store, runID, eventID, "elevation-lifecycle", "constraint", "elevation lifecycle target 2", "proposed", "sha256:elevation-lifecycle-2")
	if _, err := store.ConfirmObservation(ctx, id, "verified"); err != nil {
		t.Fatal(err)
	}
	elevated, err := store.ElevateObservation(ctx, id, "rides every session")
	if err != nil {
		t.Fatal(err)
	}
	assertIntFieldValue(t, elevated, "CanInstruct", 1)
	assertStringFieldValue(t, elevated, "Provenance", "user_confirmed")
	if elevated.ReviewState != "accepted" {
		t.Fatalf("elevate must not change review_state, got %#v", elevated)
	}
	assertReviewActionLogEntry(t, store, id, "accepted", "elevate", "cred-elevation", "rides every session")

	// Already elevated → conflict, not a silent no-op.
	if _, err := store.ElevateObservation(ctx, id, "again"); !errors.Is(err, ErrElevationConflict) {
		t.Fatalf("expected ErrElevationConflict for double elevate, got %v", err)
	}

	demoted, err := store.DemoteObservation(ctx, id, "no longer load-bearing")
	if err != nil {
		t.Fatal(err)
	}
	assertIntFieldValue(t, demoted, "CanInstruct", 0)
	// Demote leaves provenance untouched: the observation stays
	// user-confirmed, it just stops riding sessions.
	assertStringFieldValue(t, demoted, "Provenance", "user_confirmed")
	if demoted.ReviewState != "accepted" {
		t.Fatalf("demote must not change review_state, got %#v", demoted)
	}
	assertReviewActionLogEntry(t, store, id, "accepted", "demote", "cred-elevation", "no longer load-bearing")

	// Not elevated → conflict.
	if _, err := store.DemoteObservation(ctx, id, "again"); !errors.Is(err, ErrElevationConflict) {
		t.Fatalf("expected ErrElevationConflict for demote of non-elevated, got %v", err)
	}
}

func TestElevateRequiresAcceptedState(t *testing.T) {
	store, eventID, runID := governanceSeed(t, "elevation-gate")
	defer store.Close()

	ctx := authctx.With(context.Background(), authctx.Context{
		PrincipalID:  "operator",
		CredentialID: "cred-elevation-gate",
		RequestID:    "req-elevation-gate",
		Scopes:       []string{"admin"},
	})

	proposedID := insertTestDerivedObservation(t, store, runID, eventID, "elevation-gate", "fact", "proposed cannot elevate", "proposed", "sha256:elevation-gate-proposed")
	if _, err := store.ElevateObservation(ctx, proposedID, "too early"); !errors.Is(err, ErrElevationConflict) ||
		!strings.Contains(err.Error(), `requires review_state=accepted, got "proposed"`) {
		t.Fatalf("expected accepted-only conflict for proposed, got %v", err)
	}

	rejectedID := insertTestDerivedObservation(t, store, runID, eventID, "elevation-gate", "fact", "rejected cannot elevate", "proposed", "sha256:elevation-gate-rejected")
	if _, err := store.TransitionObservationReview(ctx, rejectedID, "rejected", "noise"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ElevateObservation(ctx, rejectedID, "never"); !errors.Is(err, ErrElevationConflict) {
		t.Fatalf("expected accepted-only conflict for rejected, got %v", err)
	}

	// Raw capture projections sit at accepted by construction but are locked
	// out of elevation, mirroring the review-action raw lock.
	rawID := rawObservationIDForEvent(t, store, eventID)
	if _, err := store.ElevateObservation(ctx, rawID, "raw"); !errors.Is(err, ErrElevationConflict) ||
		!strings.Contains(err.Error(), "raw observations cannot be elevated") {
		t.Fatalf("expected raw lock conflict, got %v", err)
	}

	// Elevation never happened: flags and provenance are untouched.
	var canInstruct int
	var provenance string
	if err := store.db.QueryRowContext(context.Background(), `SELECT can_instruct, COALESCE(provenance, '') FROM observations WHERE id = ?`, proposedID).Scan(&canInstruct, &provenance); err != nil {
		t.Fatal(err)
	}
	if canInstruct != 0 || provenance != "inferred" {
		t.Fatalf("failed elevate must not mutate the row: can_instruct=%d provenance=%q", canInstruct, provenance)
	}
}

// The operating-notes injection read: can_instruct=true is an additive
// opt-in filter (like include_proposed) narrowing every read surface to
// elevated observations only.
func TestObservationFilterCanInstruct(t *testing.T) {
	store, eventID, runID := governanceSeed(t, "elevation-filter")
	defer store.Close()

	ctx := authctx.With(context.Background(), authctx.Context{
		PrincipalID:  "operator",
		CredentialID: "cred-elevation-filter",
		RequestID:    "req-elevation-filter",
		Scopes:       []string{"admin"},
	})

	plainID := insertTestDerivedObservation(t, store, runID, eventID, "elevation-filter", "fact", "plain accepted elevation needle", "accepted", "sha256:elevation-filter-plain")
	elevatedID := insertTestDerivedObservation(t, store, runID, eventID, "elevation-filter", "constraint", "elevated operating elevation needle", "proposed", "sha256:elevation-filter-elevated")
	if _, err := store.ConfirmObservation(ctx, elevatedID, "verified"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ElevateObservation(ctx, elevatedID, "operating note"); err != nil {
		t.Fatal(err)
	}

	base := ObservationFilter{Project: "memoire-test", SessionID: "elevation-filter", Limit: 20}

	defaultTimeline, err := store.TimelineWithFilter(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	assertSearchHitIDs(t, defaultTimeline, []int64{plainID, elevatedID}, nil)
	elevatedHit := findSearchHit(t, defaultTimeline, elevatedID)
	assertIntFieldValue(t, elevatedHit, "CanInstruct", 1)
	plainHit := findSearchHit(t, defaultTimeline, plainID)
	assertIntFieldValue(t, plainHit, "CanInstruct", 0)

	notes := base
	notes.ExcludeKind = "raw"
	notes.CanInstruct = true
	notesTimeline, err := store.TimelineWithFilter(context.Background(), notes)
	if err != nil {
		t.Fatal(err)
	}
	assertSearchHitIDs(t, notesTimeline, []int64{elevatedID}, []int64{plainID})

	// Hits carry occurred_at under BOTH names and carry provenance, so a
	// consumer can date and elevation-tag a result line without a per-id
	// fetch round. The confirmed+elevated row is provenance user_confirmed;
	// the plain derived row stays inferred.
	assertHitGovernance(t, store, elevatedHit)
	assertHitGovernance(t, store, plainHit)
	if elevatedHit.Provenance != "user_confirmed" {
		t.Fatalf("confirmed observation hit: want provenance user_confirmed, got %q", elevatedHit.Provenance)
	}
	if plainHit.Provenance != "inferred" {
		t.Fatalf("derived observation hit: want provenance inferred, got %q", plainHit.Provenance)
	}

	filteredSearch, err := store.SearchWithFilter(context.Background(), "elevation needle", notes)
	if err != nil {
		t.Fatal(err)
	}
	assertSearchHitIDs(t, filteredSearch, []int64{elevatedID}, []int64{plainID})
	searchHit := findSearchHit(t, filteredSearch, elevatedID)
	assertHitGovernance(t, store, searchHit)
	if searchHit.Provenance != "user_confirmed" || searchHit.CanInstruct != 1 {
		t.Fatalf("search hit must carry the governance columns, got provenance=%q can_instruct=%d",
			searchHit.Provenance, searchHit.CanInstruct)
	}

	listed, _, err := store.ListObservations(context.Background(), notes)
	if err != nil {
		t.Fatal(err)
	}
	assertObservationIDs(t, listed, []int64{elevatedID}, []int64{plainID})

	// Noise composition: an elevated noise candidate stays excluded from the
	// notes read whether or not review_state=accepted is passed explicitly —
	// the two spellings of "accepted elevated notes" must agree.
	noisyElevatedID := insertTestDerivedObservation(t, store, runID, eventID, "elevation-filter", "fact", "noisy elevated elevation needle", "proposed", "sha256:elevation-filter-noisy")
	if _, err := store.ConfirmObservation(ctx, noisyElevatedID, "verified"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), `UPDATE observations SET noise_candidate = 1 WHERE id = ?`, noisyElevatedID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ElevateObservation(ctx, noisyElevatedID, "noisy note"); err != nil {
		t.Fatal(err)
	}
	for name, filter := range map[string]ObservationFilter{
		"implicit accepted": notes,
		"explicit accepted": {Project: "memoire-test", SessionID: "elevation-filter", ExcludeKind: "raw", CanInstruct: true, ReviewState: "accepted", Limit: 20},
	} {
		hits, err := store.TimelineWithFilter(context.Background(), filter)
		if err != nil {
			t.Fatal(err)
		}
		assertSearchHitIDs(t, hits, []int64{elevatedID}, []int64{noisyElevatedID, plainID})
		if len(hits) != 1 {
			t.Fatalf("%s notes read must agree on noise exclusion, got %#v", name, hits)
		}
	}
	withNoise := notes
	withNoise.IncludeNoise = true
	noisyHits, err := store.TimelineWithFilter(context.Background(), withNoise)
	if err != nil {
		t.Fatal(err)
	}
	assertSearchHitIDs(t, noisyHits, []int64{elevatedID, noisyElevatedID}, []int64{plainID})
	if _, err := store.DemoteObservation(ctx, noisyElevatedID, "cleanup"); err != nil {
		t.Fatal(err)
	}

	// Demote empties the notes read again.
	if _, err := store.DemoteObservation(ctx, elevatedID, "done"); err != nil {
		t.Fatal(err)
	}
	afterDemote, err := store.TimelineWithFilter(context.Background(), notes)
	if err != nil {
		t.Fatal(err)
	}
	assertSearchHitIDs(t, afterDemote, nil, []int64{elevatedID, plainID})
}

// assertHitGovernance checks a hit against the row it projects: both epoch
// fields carry the observation's occurred_at (captured_at_epoch is the
// legacy alias of the same column) and provenance is the stored value.
func assertHitGovernance(t *testing.T, store *Store, hit SearchHit) {
	t.Helper()
	var occurredAt int64
	var provenance string
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT occurred_at_epoch, COALESCE(provenance, '') FROM observations WHERE id = ?`,
		hit.ID).Scan(&occurredAt, &provenance); err != nil {
		t.Fatal(err)
	}
	if hit.OccurredAtEpoch != occurredAt {
		t.Fatalf("hit %d: occurred_at_epoch = %d, want %d", hit.ID, hit.OccurredAtEpoch, occurredAt)
	}
	if hit.CapturedAtEpoch != occurredAt {
		t.Fatalf("hit %d: captured_at_epoch alias = %d, want the same occurred_at %d",
			hit.ID, hit.CapturedAtEpoch, occurredAt)
	}
	if hit.Provenance != provenance {
		t.Fatalf("hit %d: provenance = %q, want %q", hit.ID, hit.Provenance, provenance)
	}
}
