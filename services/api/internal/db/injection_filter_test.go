// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

// The session-start injection path reads recent semantic observations while
// the store is dominated by raw capture projections: it needs to exclude
// kind=raw and opt into review_state=proposed (model-derived observations
// land as proposed until reviewed). Both knobs are opt-in and additive;
// default read behavior stays accepted-only, raw included.
func TestObservationFilterExcludeKindAndIncludeProposed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	eventID := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "session-inject",
		SourceAdapter:   "test",
		SourceEventID:   "event-inject-1",
		EventType:       "note",
		OccurredAtEpoch: 100,
		Payload:         json.RawMessage(`{"message":"source"}`),
	})

	// Event ingestion already projects a raw observation; reuse it.
	var rawID int64
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT id FROM observations WHERE event_id = ? AND kind = 'raw'`, eventID).Scan(&rawID); err != nil {
		t.Fatal(err)
	}

	runID := ensureTestRun(t, store, DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "session-inject",
		Provider:          "rules",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:inject-run",
		WindowStartEpoch:  100,
		WindowEndEpoch:    120,
		Status:            "completed",
	})

	acceptedFact, err := store.InsertDerivedObservation(context.Background(), DerivedObservationInput{
		Project:         "memoire-test",
		SessionID:       "session-inject",
		Kind:            "fact",
		Text:            "accepted derived fact",
		ReviewState:     "accepted",
		DerivationRunID: runID,
		ContentHash:     "sha256:inject-fact",
		SourceEventIDs:  []int64{eventID},
	})
	if err != nil {
		t.Fatal(err)
	}

	proposedDecision, err := store.InsertDerivedObservation(context.Background(), DerivedObservationInput{
		Project:         "memoire-test",
		SessionID:       "session-inject",
		Kind:            "decision",
		Text:            "proposed derived decision",
		ReviewState:     "proposed",
		DerivationRunID: runID,
		ContentHash:     "sha256:inject-decision",
		SourceEventIDs:  []int64{eventID},
	})
	if err != nil {
		t.Fatal(err)
	}

	base := ObservationFilter{Project: "memoire-test", SessionID: "session-inject", Limit: 20}

	defaultTimeline, err := store.TimelineWithFilter(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	assertSearchHitIDs(t, defaultTimeline, []int64{rawID, acceptedFact.ID}, []int64{proposedDecision.ID})

	noRaw := base
	noRaw.ExcludeKind = "raw"
	noRawTimeline, err := store.TimelineWithFilter(context.Background(), noRaw)
	if err != nil {
		t.Fatal(err)
	}
	assertSearchHitIDs(t, noRawTimeline, []int64{acceptedFact.ID}, []int64{rawID, proposedDecision.ID})

	injection := base
	injection.ExcludeKind = "raw"
	injection.IncludeProposed = true
	injectionTimeline, err := store.TimelineWithFilter(context.Background(), injection)
	if err != nil {
		t.Fatal(err)
	}
	assertSearchHitIDs(t, injectionTimeline, []int64{acceptedFact.ID, proposedDecision.ID}, []int64{rawID})

	// By-id: the ratified default hides proposed; the opt-in filter exposes it
	// so ids surfaced with include_proposed never 404 on the detail fetch.
	if _, err := store.GetObservation(context.Background(), proposedDecision.ID); err == nil {
		t.Fatal("expected default by-id get to hide proposed observation")
	}
	got, err := store.GetObservationWithFilter(context.Background(), proposedDecision.ID, ObservationFilter{IncludeProposed: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != proposedDecision.ID || got.ReviewState != "proposed" {
		t.Fatalf("unexpected opt-in by-id record: %#v", got)
	}
}
