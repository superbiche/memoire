// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"memoire/services/api/internal/authctx"
)

func TestGovernanceMigrationBackfillDefaultsTriggersAndReplay(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upgrade.db")
	buildPreGovernanceDB(t, dbPath)

	store, err := Open(dbPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	assertTableColumns(t, store.db, "observations", []string{
		"provenance",
		"can_instruct",
		"can_evidence",
		"needs_confirmation",
		"noise_candidate",
	})
	assertTableColumns(t, store.db, "observation_review_log", []string{"action"})

	var rawProvenance, derivedProvenance string
	if err := store.db.QueryRowContext(context.Background(), `SELECT provenance FROM observations WHERE kind = 'raw'`).Scan(&rawProvenance); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(context.Background(), `SELECT provenance FROM observations WHERE kind = 'decision'`).Scan(&derivedProvenance); err != nil {
		t.Fatal(err)
	}
	if rawProvenance != "observed" || derivedProvenance != "inferred" {
		t.Fatalf("unexpected provenance backfill: raw=%q derived=%q", rawProvenance, derivedProvenance)
	}

	var canInstruct, canEvidence, needsConfirmation, noiseCandidate int
	if err := store.db.QueryRowContext(context.Background(), `SELECT can_instruct, can_evidence, needs_confirmation, noise_candidate FROM observations WHERE kind = 'decision'`).Scan(&canInstruct, &canEvidence, &needsConfirmation, &noiseCandidate); err != nil {
		t.Fatal(err)
	}
	if canInstruct != 0 || canEvidence != 1 || needsConfirmation != 0 || noiseCandidate != 0 {
		t.Fatalf("unexpected governance defaults: can_instruct=%d can_evidence=%d needs_confirmation=%d noise_candidate=%d", canInstruct, canEvidence, needsConfirmation, noiseCandidate)
	}

	assertDirectSQLInvariant(t, store)
}

func TestGovernanceFreshReplayIsDeterministic(t *testing.T) {
	storeA, err := Open(filepath.Join(t.TempDir(), "a.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()
	storeB, err := Open(filepath.Join(t.TempDir(), "b.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()

	assertTableColumns(t, storeA.db, "observations", []string{"provenance", "can_instruct", "can_evidence", "needs_confirmation", "noise_candidate"})
	assertTableColumns(t, storeA.db, "observation_review_log", []string{"action"})

	versions := schemaVersions(t, storeA.db)
	if !strings.Contains(","+versions+",", ",7,") {
		t.Fatalf("expected governance migration version 7 to be applied, got schema_version sequence %q", versions)
	}
	triggerNames := strings.Join(schemaObjectNames(t, storeA.db, "trigger"), ",")
	for _, want := range []string{
		"governance_observations_can_instruct_insert",
		"governance_observations_can_instruct_update",
	} {
		if !strings.Contains(triggerNames, want) {
			t.Fatalf("expected trigger %s in %q", want, triggerNames)
		}
	}

	if gotA, gotB := schemaFingerprint(t, storeA.db), schemaFingerprint(t, storeB.db); gotA != gotB {
		t.Fatalf("fresh DB replay is not deterministic\nA:\n%s\nB:\n%s", gotA, gotB)
	}
}

func TestGovernanceConfirmSetsProvenanceAndAppInvariant(t *testing.T) {
	store, eventID, runID := governanceSeed(t, "governance-confirm")
	defer store.Close()

	reviewCtx := authctx.With(context.Background(), authctx.Context{
		PrincipalID:  "reviewer",
		CredentialID: "cred-governance-confirm",
		RequestID:    "req-governance-confirm",
		Scopes:       []string{"admin"},
	})
	// Slice E (memoire:D146, R-5): plain accept means "seen, not contested" —
	// provenance stays inferred. The explicit confirm verdict promotes.
	plainID := insertTestDerivedObservation(t, store, runID, eventID, "governance-confirm", "fact", "plain accept keeps provenance", "proposed", "sha256:governance-plain-accept")
	accepted, err := store.TransitionObservationReview(reviewCtx, plainID, "accepted", "seen, fine")
	if err != nil {
		t.Fatal(err)
	}
	if accepted.ReviewState != "accepted" {
		t.Fatalf("expected accepted review state, got %#v", accepted)
	}
	assertStringFieldValue(t, accepted, "Provenance", "inferred")

	proposedID := insertTestDerivedObservation(t, store, runID, eventID, "governance-confirm", "decision", "confirm should set provenance", "proposed", "sha256:governance-confirm")
	confirmed, err := store.ConfirmObservation(reviewCtx, proposedID, "human confirmed")
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.ReviewState != "accepted" {
		t.Fatalf("expected accepted review state, got %#v", confirmed)
	}
	assertStringFieldValue(t, confirmed, "Provenance", "user_confirmed")
	assertIntFieldValue(t, confirmed, "CanInstruct", 0)

	var provenance string
	if err := store.db.QueryRowContext(context.Background(), `SELECT provenance FROM observations WHERE id = ?`, proposedID).Scan(&provenance); err != nil {
		t.Fatal(err)
	}
	if provenance != "user_confirmed" {
		t.Fatalf("confirm verdict should set provenance=user_confirmed, got %q", provenance)
	}

	in := DerivedObservationInput{
		Project:         "memoire-test",
		SessionID:       "governance-confirm",
		Kind:            "fact",
		Text:            "invalid can_instruct provenance",
		ReviewState:     "accepted",
		DerivationRunID: runID,
		ContentHash:     "sha256:invalid-can-instruct-provenance",
		SourceEventIDs:  []int64{eventID},
	}
	setStringField(t, &in, "Provenance", "inferred")
	setFlagField(t, &in, "CanInstruct", 1)
	_, err = store.InsertDerivedObservation(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "app invariant: can_instruct requires provenance user_confirmed or imported") {
		t.Fatalf("expected app-level can_instruct/provenance invariant error, got %v", err)
	}
}

func TestGovernanceDisputedTerminalTransitionsDoNotMutateFlags(t *testing.T) {
	store, eventID, runID := governanceSeed(t, "governance-dispute-flags")
	defer store.Close()

	reviewCtx := authctx.With(context.Background(), authctx.Context{
		PrincipalID:  "reviewer",
		CredentialID: "cred-governance-dispute-flags",
		RequestID:    "req-governance-dispute-flags",
		Scopes:       []string{"admin"},
	})

	for _, tc := range []struct {
		toState string
	}{
		{toState: "rejected"},
		{toState: "superseded"},
	} {
		t.Run("disputed_to_"+tc.toState+"_leaves_flags_untouched", func(t *testing.T) {
			id := insertTestDerivedObservation(t, store, runID, eventID, "governance-dispute-flags", "fact", "disputed terminal flags "+tc.toState, "accepted", "sha256:disputed-terminal-flags-"+tc.toState)
			setObservationFlags(t, store, id, 1, 0, 1)
			if _, err := store.TransitionObservationReview(reviewCtx, id, "disputed", "challenge"); err != nil {
				t.Fatal(err)
			}
			resolved, err := store.TransitionObservationReview(reviewCtx, id, tc.toState, "resolve")
			if err != nil {
				t.Fatal(err)
			}
			assertIntFieldValue(t, resolved, "CanInstruct", 1)
			assertIntFieldValue(t, resolved, "CanEvidence", 0)
			assertIntFieldValue(t, resolved, "NeedsConfirmation", 1)
		})
	}
}

func TestGovernanceInsertDerivedObservationRejectsInvalidProvenance(t *testing.T) {
	store, eventID, runID := governanceSeed(t, "governance-provenance-reject")
	defer store.Close()

	for _, tc := range []struct {
		name       string
		provenance string
		wantErr    string
	}{
		{name: "outside five value set", provenance: "bogus", wantErr: "provenance"},
		{name: "user confirmed forbidden for derivation", provenance: "user_confirmed", wantErr: "provenance"},
		{name: "imported forbidden for derivation", provenance: "imported", wantErr: "provenance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.InsertDerivedObservation(context.Background(), DerivedObservationInput{
				Project:         "memoire-test",
				SessionID:       "governance-provenance-reject",
				Kind:            "fact",
				Text:            "invalid provenance " + tc.provenance,
				ReviewState:     "accepted",
				DerivationRunID: runID,
				ContentHash:     "sha256:invalid-provenance-" + tc.provenance,
				SourceEventIDs:  []int64{eventID},
				Provenance:      tc.provenance,
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected provenance rejection containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestGovernanceDisputedTransitionsAndFlagActions(t *testing.T) {
	store, eventID, runID := governanceSeed(t, "governance-dispute")
	defer store.Close()

	reviewCtx := authctx.With(context.Background(), authctx.Context{
		PrincipalID:  "reviewer",
		CredentialID: "cred-governance-dispute",
		RequestID:    "req-governance-dispute",
		Scopes:       []string{"admin"},
	})

	for _, tc := range []struct {
		name    string
		toState string
	}{
		{name: "disputed to accepted", toState: "accepted"},
		{name: "disputed to superseded", toState: "superseded"},
		{name: "disputed to rejected", toState: "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := insertTestDerivedObservation(t, store, runID, eventID, "governance-dispute", "fact", "disputed path "+tc.toState, "accepted", "sha256:disputed-"+tc.toState)
			disputed, err := store.TransitionObservationReview(reviewCtx, id, "disputed", "challenge")
			if err != nil {
				t.Fatal(err)
			}
			if disputed.ReviewState != "disputed" {
				t.Fatalf("expected disputed, got %#v", disputed)
			}
			resolved, err := store.TransitionObservationReview(reviewCtx, id, tc.toState, "resolve")
			if err != nil {
				t.Fatal(err)
			}
			if resolved.ReviewState != tc.toState {
				t.Fatalf("expected %s, got %#v", tc.toState, resolved)
			}
		})
	}

	proposedID := insertTestDerivedObservation(t, store, runID, eventID, "governance-dispute", "task", "proposed cannot dispute", "proposed", "sha256:proposed-dispute")
	if _, err := store.TransitionObservationReview(reviewCtx, proposedID, "disputed", "too early"); err == nil ||
		!strings.Contains(err.Error(), "unsupported review transition proposed -> disputed") {
		t.Fatalf("expected proposed->disputed rejection, got %v", err)
	}

	disputedProposedID := insertTestDerivedObservation(t, store, runID, eventID, "governance-dispute", "fact", "disputed cannot return to proposed", "accepted", "sha256:disputed-proposed")
	if _, err := store.TransitionObservationReview(reviewCtx, disputedProposedID, "disputed", "challenge"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionObservationReview(reviewCtx, disputedProposedID, "proposed", "reopen"); err == nil ||
		!strings.Contains(err.Error(), "unsupported review transition disputed -> proposed") {
		t.Fatalf("expected disputed->proposed rejection, got %v", err)
	}

	disputedSoftDeleteID := insertTestDerivedObservation(t, store, runID, eventID, "governance-dispute", "fact", "disputed cannot soft delete", "accepted", "sha256:disputed-soft-delete")
	if _, err := store.TransitionObservationReview(reviewCtx, disputedSoftDeleteID, "disputed", "challenge"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionObservationReview(reviewCtx, disputedSoftDeleteID, "soft_deleted", "hide"); err == nil ||
		!strings.Contains(err.Error(), "unsupported review transition disputed -> soft_deleted") {
		t.Fatalf("expected disputed->soft_deleted rejection, got %v", err)
	}

	rejectedDisputedID := insertTestDerivedObservation(t, store, runID, eventID, "governance-dispute", "task", "rejected cannot dispute", "proposed", "sha256:rejected-disputed")
	if _, err := store.TransitionObservationReview(reviewCtx, rejectedDisputedID, "rejected", "reject"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionObservationReview(reviewCtx, rejectedDisputedID, "disputed", "challenge rejected"); err == nil ||
		!strings.Contains(err.Error(), "unsupported review transition rejected -> disputed") {
		t.Fatalf("expected rejected->disputed rejection, got %v", err)
	}

	supersededDisputedID := insertTestDerivedObservation(t, store, runID, eventID, "governance-dispute", "fact", "superseded cannot dispute", "accepted", "sha256:superseded-disputed")
	if _, err := store.TransitionObservationReview(reviewCtx, supersededDisputedID, "superseded", "stale"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionObservationReview(reviewCtx, supersededDisputedID, "disputed", "challenge superseded"); err == nil ||
		!strings.Contains(err.Error(), "unsupported review transition superseded -> disputed") {
		t.Fatalf("expected superseded->disputed rejection, got %v", err)
	}

	rawID := rawObservationIDForEvent(t, store, eventID)
	if _, err := store.TransitionObservationReview(reviewCtx, rawID, "disputed", "raw challenge"); err == nil ||
		!strings.Contains(err.Error(), "raw observations are locked at accepted") {
		t.Fatalf("expected raw lock to hold for disputed, got %v", err)
	}

	actionID := insertTestDerivedObservation(t, store, runID, eventID, "governance-dispute", "constraint", "flag action target", "accepted", "sha256:flag-action-target")
	actionStore, ok := any(store).(interface {
		ApplyObservationReviewAction(context.Context, int64, string, string) (ObservationRecord, error)
	})
	if !ok {
		t.Fatalf("Store must implement ApplyObservationReviewAction(context.Context, int64, string, string) (ObservationRecord, error)")
	}
	for _, tc := range []struct {
		action           string
		wantCanInstruct  int
		wantCanEvidence  int
		wantConfirmation int
	}{
		{action: "evidence-only", wantCanInstruct: 0, wantCanEvidence: 1, wantConfirmation: 0},
		{action: "restrict-scope", wantCanInstruct: 0, wantCanEvidence: 1, wantConfirmation: 1},
		{action: "mark-stale", wantCanInstruct: 0, wantCanEvidence: 0, wantConfirmation: 1},
	} {
		rec, err := actionStore.ApplyObservationReviewAction(reviewCtx, actionID, tc.action, "flag action "+tc.action)
		if err != nil {
			t.Fatal(err)
		}
		if rec.ReviewState != "accepted" {
			t.Fatalf("flag action %s changed review_state: %#v", tc.action, rec)
		}
		assertReviewActionLogEntry(t, store, actionID, "accepted", tc.action, "cred-governance-dispute", "flag action "+tc.action)
		logRows, err := store.ListObservationReviewLog(context.Background(), ObservationReviewLogFilter{
			ObservationID: actionID,
			Limit:         1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(logRows) != 1 {
			t.Fatalf("expected latest review action log row, got %#v", logRows)
		}
		assertStringFieldValue(t, logRows[0], "Action", tc.action)
		var canInstruct, canEvidence, needsConfirmation int
		if err := store.db.QueryRowContext(context.Background(), `SELECT can_instruct, can_evidence, needs_confirmation FROM observations WHERE id = ?`, actionID).Scan(&canInstruct, &canEvidence, &needsConfirmation); err != nil {
			t.Fatal(err)
		}
		if canInstruct != tc.wantCanInstruct || canEvidence != tc.wantCanEvidence || needsConfirmation != tc.wantConfirmation {
			t.Fatalf("unexpected flags after %s: can_instruct=%d can_evidence=%d needs_confirmation=%d", tc.action, canInstruct, canEvidence, needsConfirmation)
		}
	}

	if _, err := actionStore.ApplyObservationReviewAction(reviewCtx, actionID, "edit", "out of scope"); err == nil ||
		!strings.Contains(err.Error(), "review action must be one of evidence-only, restrict-scope, mark-stale") {
		t.Fatalf("expected out-of-scope action rejection, got %v", err)
	}
}

func TestGovernanceFlagActionsApplyFullContractFromNonDefaultFlags(t *testing.T) {
	store, eventID, runID := governanceSeed(t, "governance-flag-actions")
	defer store.Close()

	reviewCtx := authctx.With(context.Background(), authctx.Context{
		PrincipalID:  "reviewer",
		CredentialID: "cred-governance-flag-actions",
		RequestID:    "req-governance-flag-actions",
		Scopes:       []string{"admin"},
	})
	actionStore, ok := any(store).(interface {
		ApplyObservationReviewAction(context.Context, int64, string, string) (ObservationRecord, error)
	})
	if !ok {
		t.Fatalf("Store must implement ApplyObservationReviewAction(context.Context, int64, string, string) (ObservationRecord, error)")
	}

	for _, tc := range []struct {
		action           string
		wantCanInstruct  int64
		wantCanEvidence  int64
		wantConfirmation int64
	}{
		{action: "evidence-only", wantCanInstruct: 0, wantCanEvidence: 1, wantConfirmation: 0},
		{action: "restrict-scope", wantCanInstruct: 0, wantCanEvidence: 1, wantConfirmation: 1},
		{action: "mark-stale", wantCanInstruct: 0, wantCanEvidence: 0, wantConfirmation: 1},
	} {
		t.Run(tc.action, func(t *testing.T) {
			id := insertTestDerivedObservation(t, store, runID, eventID, "governance-flag-actions", "constraint", "flag action full state "+tc.action, "accepted", "sha256:flag-action-full-state-"+tc.action)
			setObservationFlags(t, store, id, 1, 0, 1)
			if _, err := actionStore.ApplyObservationReviewAction(reviewCtx, id, tc.action, "flag action "+tc.action); err != nil {
				t.Fatal(err)
			}
			updated, err := store.GetObservation(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			assertIntFieldValue(t, updated, "CanInstruct", tc.wantCanInstruct)
			assertIntFieldValue(t, updated, "CanEvidence", tc.wantCanEvidence)
			assertIntFieldValue(t, updated, "NeedsConfirmation", tc.wantConfirmation)
		})
	}
}

func TestGovernanceNoiseFilterAndCaptureUngated(t *testing.T) {
	store, eventID, runID := governanceSeed(t, "governance-noise")
	defer store.Close()

	cleanID := insertTestDerivedObservation(t, store, runID, eventID, "governance-noise", "fact", "clean governance read needle", "accepted", "sha256:clean-governance-read")
	noisyID := insertTestDerivedObservation(t, store, runID, eventID, "governance-noise", "fact", "noisy governance read needle", "accepted", "sha256:noisy-governance-read")
	proposedNoisyID := insertTestDerivedObservation(t, store, runID, eventID, "governance-noise", "task", "proposed noisy review needle", "proposed", "sha256:proposed-noisy-review")
	if _, err := store.db.ExecContext(context.Background(), `UPDATE observations SET noise_candidate = 1 WHERE id = ?`, noisyID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), `UPDATE observations SET noise_candidate = 1 WHERE id = ?`, proposedNoisyID); err != nil {
		t.Fatal(err)
	}

	defaultList, _, err := store.ListObservations(context.Background(), ObservationFilter{
		Project:   "memoire-test",
		SessionID: "governance-noise",
		Limit:     20,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertObservationIDs(t, defaultList, []int64{cleanID}, []int64{noisyID})
	cleanRecord := findObservationRecord(t, defaultList, cleanID)
	assertStringFieldValue(t, cleanRecord, "Provenance", "inferred")
	assertIntFieldValue(t, cleanRecord, "CanInstruct", 0)
	assertIntFieldValue(t, cleanRecord, "CanEvidence", 1)
	assertIntFieldValue(t, cleanRecord, "NeedsConfirmation", 0)
	assertIntFieldValue(t, cleanRecord, "NoiseCandidate", 0)

	cleanByID, err := store.GetObservation(context.Background(), cleanID)
	if err != nil {
		t.Fatal(err)
	}
	assertStringFieldValue(t, cleanByID, "Provenance", "inferred")

	includeNoise := ObservationFilter{Project: "memoire-test", SessionID: "governance-noise", Limit: 20}
	setBoolField(t, &includeNoise, "IncludeNoise", true)
	withNoise, _, err := store.ListObservations(context.Background(), includeNoise)
	if err != nil {
		t.Fatal(err)
	}
	assertObservationIDs(t, withNoise, []int64{cleanID, noisyID}, nil)

	// Default noise exclusion applies on explicit review_state reads too:
	// the review queue's include_noise toggle keys distinct capped server
	// windows on it.
	proposedReviewQueue, _, err := store.ListObservations(context.Background(), ObservationFilter{
		Project:     "memoire-test",
		SessionID:   "governance-noise",
		ReviewState: "proposed",
		Limit:       20,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertObservationIDs(t, proposedReviewQueue, nil, []int64{proposedNoisyID})

	noisyProposedQueue := ObservationFilter{
		Project:     "memoire-test",
		SessionID:   "governance-noise",
		ReviewState: "proposed",
		Limit:       20,
	}
	setBoolField(t, &noisyProposedQueue, "IncludeNoise", true)
	proposedWithNoise, _, err := store.ListObservations(context.Background(), noisyProposedQueue)
	if err != nil {
		t.Fatal(err)
	}
	assertObservationIDs(t, proposedWithNoise, []int64{proposedNoisyID}, nil)

	defaultSearch, err := store.SearchWithFilter(context.Background(), "governance read needle", ObservationFilter{
		Project:   "memoire-test",
		SessionID: "governance-noise",
		Limit:     20,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSearchHitIDs(t, defaultSearch, []int64{cleanID}, []int64{noisyID})

	searchFilter := ObservationFilter{Project: "memoire-test", SessionID: "governance-noise", Limit: 20}
	setBoolField(t, &searchFilter, "IncludeNoise", true)
	withNoiseSearch, err := store.SearchWithFilter(context.Background(), "governance read needle", searchFilter)
	if err != nil {
		t.Fatal(err)
	}
	assertSearchHitIDs(t, withNoiseSearch, []int64{cleanID, noisyID}, nil)
	noisyHit := findSearchHit(t, withNoiseSearch, noisyID)
	assertIntFieldValue(t, noisyHit, "NoiseCandidate", 1)

	defaultTimeline, err := store.TimelineWithFilter(context.Background(), ObservationFilter{
		Project:   "memoire-test",
		SessionID: "governance-noise",
		Limit:     20,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSearchHitIDs(t, defaultTimeline, []int64{cleanID}, []int64{noisyID})

	noisyEventID := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "governance-noise",
		SourceAdapter:   "governance-test",
		SourceEventID:   "noisy-capture-ungated",
		EventType:       "note",
		OccurredAtEpoch: 1800000002,
		Payload:         json.RawMessage(`{"message":"scratch capture must not be gated","noise_candidate":true}`),
		Text:            "scratch capture must not be gated",
	})
	var provenance string
	var noiseCandidate int
	if err := store.db.QueryRowContext(context.Background(), `SELECT provenance, noise_candidate FROM observations WHERE event_id = ? AND kind = 'raw'`, noisyEventID).Scan(&provenance, &noiseCandidate); err != nil {
		t.Fatal(err)
	}
	if provenance != "observed" || noiseCandidate != 0 {
		t.Fatalf("raw capture should remain observed and non-noise-gated, got provenance=%q noise_candidate=%d", provenance, noiseCandidate)
	}
}

func buildPreGovernanceDB(t *testing.T, dbPath string) {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	for _, name := range []string{"0001_initial.sql", "0004_derivation.sql", "0005_observation_review_log.sql", "0006_derivation_model.sql"} {
		b, err := os.ReadFile(filepath.Join("../../../../schema", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(string(b)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	if _, err := conn.Exec(`INSERT INTO events(
		id, idempotency_key, session_id, project, source_adapter, source_event_id, event_type,
		payload_json, text, occurred_at_epoch, received_at_epoch, principal_id, meta_json
	) VALUES (1, 'governance-upgrade-event-key', 'governance-upgrade', 'memoire-test', 'test', 'governance-upgrade-event', 'note',
		'{"message":"governance upgrade"}', 'governance upgrade', 1800000000, 1800000001, 'local', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO observations(
		event_id, project, session_id, kind, text, source_adapter, event_type,
		occurred_at_epoch, created_at_epoch, principal_id, meta_json, review_state
	) VALUES
		(1, 'memoire-test', 'governance-upgrade', 'raw', 'raw governance upgrade', 'test', 'note', 1800000000, 1800000001, 'local', '{}', 'accepted'),
		(NULL, 'memoire-test', 'governance-upgrade', 'decision', 'derived governance upgrade', 'test', 'note', 1800000000, 1800000001, 'local', '{}', 'proposed')`); err != nil {
		t.Fatal(err)
	}
}

func governanceSeed(t *testing.T, sessionID string) (*Store, int64, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	eventID := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       sessionID,
		SourceAdapter:   "governance-test",
		SourceEventID:   sessionID + "-event",
		EventType:       "note",
		OccurredAtEpoch: 1800000000,
		Payload:         json.RawMessage(fmt.Sprintf(`{"message":%q}`, sessionID+" source")),
		Text:            sessionID + " source",
	})
	runID := ensureTestRun(t, store, DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         sessionID,
		Provider:          "manual",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:" + sessionID,
		WindowStartEpoch:  1799999990,
		WindowEndEpoch:    1800000010,
		Status:            "completed",
	})
	return store, eventID, runID
}

func assertTableColumns(t *testing.T, conn dbc, table string, want []string) {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), `PRAGMA table_info(`+table+`)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		got[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("expected column %s.%s; columns present: %#v", table, name, got)
		}
	}
}

func assertDirectSQLInvariant(t *testing.T, store *Store) {
	t.Helper()
	_, err := store.db.ExecContext(context.Background(), `INSERT INTO observations(
		project, kind, text, occurred_at_epoch, created_at_epoch, provenance, can_instruct
	) VALUES ('memoire-test', 'fact', 'direct invalid insert', 1, 1, 'inferred', 1)`)
	if err == nil || !strings.Contains(err.Error(), "can_instruct requires provenance user_confirmed or imported") {
		t.Fatalf("expected direct SQL insert invariant failure, got %v", err)
	}

	_, err = store.db.ExecContext(context.Background(), `INSERT INTO observations(
		project, kind, text, occurred_at_epoch, created_at_epoch, can_instruct
	) VALUES ('memoire-test', 'fact', 'direct null provenance invalid insert', 1, 1, 1)`)
	if err == nil || !strings.Contains(err.Error(), "can_instruct requires provenance user_confirmed or imported") {
		t.Fatalf("expected direct SQL NULL-provenance insert invariant failure, got %v", err)
	}

	_, err = store.db.ExecContext(context.Background(), `INSERT INTO observations(
		project, kind, text, occurred_at_epoch, created_at_epoch, provenance, can_instruct
	) VALUES ('memoire-test', 'fact', 'direct valid insert for update', 2, 2, 'inferred', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	res, err := store.db.ExecContext(context.Background(), `UPDATE observations SET can_instruct = 1 WHERE text = 'direct valid insert for update'`)
	if err == nil {
		if rows, rowsErr := res.RowsAffected(); rowsErr == nil {
			t.Fatalf("expected direct SQL update invariant failure, got nil error and %d rows", rows)
		}
		t.Fatalf("expected direct SQL update invariant failure, got nil error")
	}
	if !strings.Contains(err.Error(), "can_instruct requires provenance user_confirmed or imported") {
		t.Fatalf("expected can_instruct invariant error, got %v", err)
	}
}

func schemaVersions(t *testing.T, conn dbc) string {
	t.Helper()
	var out sql.NullString
	if err := conn.QueryRowContext(context.Background(), `SELECT group_concat(version, ',') FROM (SELECT version FROM schema_version ORDER BY version)`).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out.String
}

func schemaObjectNames(t *testing.T, conn dbc, typ string) []string {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), `SELECT name FROM sqlite_master WHERE type = ? ORDER BY name`, typ)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func schemaFingerprint(t *testing.T, conn dbc) string {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name, tbl_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var typ, name, tbl, sqlText string
		if err := rows.Scan(&typ, &name, &tbl, &sqlText); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, typ+"|"+name+"|"+tbl+"|"+sqlText)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(parts, "\n")
}

func assertReviewActionLogEntry(t *testing.T, store *Store, observationID int64, state, action, actor, reasonText string) {
	t.Helper()
	var gotFromState, gotToState, gotAction, gotActor, gotReasonText string
	var occurredAt int64
	if err := store.db.QueryRowContext(context.Background(), `SELECT from_state, to_state, COALESCE(action, ''), actor, COALESCE(reason_text, ''), occurred_at_epoch
		FROM observation_review_log
		WHERE observation_id = ?
		ORDER BY id DESC
		LIMIT 1`, observationID).Scan(&gotFromState, &gotToState, &gotAction, &gotActor, &gotReasonText, &occurredAt); err != nil {
		t.Fatal(err)
	}
	if gotFromState != state || gotToState != state || gotAction != action || gotActor != actor || gotReasonText != reasonText || occurredAt == 0 {
		t.Fatalf("unexpected action log row: from=%q to=%q action=%q actor=%q reason=%q occurred_at=%d", gotFromState, gotToState, gotAction, gotActor, gotReasonText, occurredAt)
	}
}

func setObservationFlags(t *testing.T, store *Store, observationID int64, canInstruct, canEvidence, needsConfirmation int) {
	t.Helper()
	_, err := store.db.ExecContext(context.Background(), `UPDATE observations
		SET provenance = 'user_confirmed',
		    can_instruct = ?,
		    can_evidence = ?,
		    needs_confirmation = ?
		WHERE id = ?`,
		canInstruct, canEvidence, needsConfirmation, observationID)
	if err != nil {
		t.Fatal(err)
	}
}

func findObservationRecord(t *testing.T, recs []ObservationRecord, id int64) ObservationRecord {
	t.Helper()
	for _, rec := range recs {
		if rec.ID == id {
			return rec
		}
	}
	t.Fatalf("observation %d not found in %#v", id, recs)
	return ObservationRecord{}
}

func findSearchHit(t *testing.T, hits []SearchHit, id int64) SearchHit {
	t.Helper()
	for _, hit := range hits {
		if hit.ID == id {
			return hit
		}
	}
	t.Fatalf("search hit %d not found in %#v", id, hits)
	return SearchHit{}
}

func assertStringFieldValue(t *testing.T, target any, name, want string) {
	t.Helper()
	field := reflect.ValueOf(target).FieldByName(name)
	if !field.IsValid() {
		t.Fatalf("expected field %s on %T", name, target)
	}
	if field.Kind() != reflect.String {
		t.Fatalf("expected %s to be string, got %s", name, field.Kind())
	}
	if got := field.String(); got != want {
		t.Fatalf("expected %s=%q, got %q", name, want, got)
	}
}

func assertIntFieldValue(t *testing.T, target any, name string, want int64) {
	t.Helper()
	field := reflect.ValueOf(target).FieldByName(name)
	if !field.IsValid() {
		t.Fatalf("expected field %s on %T", name, target)
	}
	var got int64
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		got = field.Int()
	case reflect.Bool:
		if field.Bool() {
			got = 1
		}
	default:
		t.Fatalf("expected %s to be int-like or bool, got %s", name, field.Kind())
	}
	if got != want {
		t.Fatalf("expected %s=%d, got %d", name, want, got)
	}
}

func setStringField(t *testing.T, target any, name, value string) {
	t.Helper()
	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		t.Fatalf("setStringField target must be non-nil pointer")
	}
	field := v.Elem().FieldByName(name)
	if !field.IsValid() {
		t.Fatalf("expected field %s on %s", name, v.Elem().Type())
	}
	if field.Kind() != reflect.String {
		t.Fatalf("expected field %s to be string, got %s", name, field.Kind())
	}
	field.SetString(value)
}

func setFlagField(t *testing.T, target any, name string, value int) {
	t.Helper()
	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		t.Fatalf("setFlagField target must be non-nil pointer")
	}
	field := v.Elem().FieldByName(name)
	if !field.IsValid() {
		t.Fatalf("expected field %s on %s", name, v.Elem().Type())
	}
	switch field.Kind() {
	case reflect.Bool:
		field.SetBool(value != 0)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		field.SetInt(int64(value))
	default:
		t.Fatalf("expected field %s to be bool or int-like, got %s", name, field.Kind())
	}
}

func setBoolField(t *testing.T, target any, name string, value bool) {
	t.Helper()
	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		t.Fatalf("setBoolField target must be non-nil pointer")
	}
	field := v.Elem().FieldByName(name)
	if !field.IsValid() {
		t.Fatalf("expected field %s on %s", name, v.Elem().Type())
	}
	if field.Kind() != reflect.Bool {
		t.Fatalf("expected field %s to be bool, got %s", name, field.Kind())
	}
	field.SetBool(value)
}

var _ = errors.Is
