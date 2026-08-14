// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
)

// ListObservations is capped at 200 rows: sort direction decides WHICH rows
// the caller gets back, so client-side sorting of a truncated page cannot
// substitute for server-side order. This pins the default/asc/desc/garbage
// behavior of ObservationFilter.Order, including the id tiebreak for rows
// sharing an occurred_at_epoch.
func TestObservationFilterOrder(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// A and B share occurred_at_epoch=100; A is ingested first so its
	// observation id is lower. C has a later occurred_at_epoch.
	aID := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "session-order",
		SourceAdapter:   "test",
		SourceEventID:   "event-order-a",
		EventType:       "note",
		OccurredAtEpoch: 100,
		Payload:         json.RawMessage(`{"message":"a"}`),
	})
	bID := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "session-order",
		SourceAdapter:   "test",
		SourceEventID:   "event-order-b",
		EventType:       "note",
		OccurredAtEpoch: 100,
		Payload:         json.RawMessage(`{"message":"b"}`),
	})
	cID := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "session-order",
		SourceAdapter:   "test",
		SourceEventID:   "event-order-c",
		EventType:       "note",
		OccurredAtEpoch: 200,
		Payload:         json.RawMessage(`{"message":"c"}`),
	})

	obsA := rawObservationIDForEvent(t, store, aID)
	obsB := rawObservationIDForEvent(t, store, bID)
	obsC := rawObservationIDForEvent(t, store, cID)

	base := ObservationFilter{Project: "memoire-test", SessionID: "session-order", Limit: 10}

	// Default, explicit "desc", and a garbage value all fall back to the
	// same descending order: newest occurred_at_epoch first, id DESC as
	// the tiebreak for rows sharing an epoch.
	for _, order := range []string{"", "desc", "banana"} {
		filter := base
		filter.Order = order
		recs, _, err := store.ListObservations(context.Background(), filter)
		if err != nil {
			t.Fatalf("order=%q: %v", order, err)
		}
		assertObservationOrder(t, order, recs, []int64{obsC, obsB, obsA})
	}

	ascFilter := base
	ascFilter.Order = "asc"
	ascRecs, _, err := store.ListObservations(context.Background(), ascFilter)
	if err != nil {
		t.Fatal(err)
	}
	assertObservationOrder(t, "asc", ascRecs, []int64{obsA, obsB, obsC})
}

// TestObservationCursorPagination pins keyset (cursor) pagination on top of
// the order windows exercised by TestObservationFilterOrder: 7 observations
// where 3 share one occurred_at_epoch (proving the id tiebreak survives a
// continuation, including mid-group), walked page by page in both
// directions, with malformed-cursor and filter-composition checks.
func TestObservationCursorPagination(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// obs1-obs3 share occurred_at_epoch=100 (ingested in order, so ids
	// ascend obs1 < obs2 < obs3). obs4-obs7 each get a distinct, later
	// epoch. Desc order is therefore obs7,obs6,obs5,obs4,obs3,obs2,obs1;
	// asc order is the reverse.
	epochs := []int64{100, 100, 100, 200, 300, 400, 500}
	var obsIDs []int64
	for i, epoch := range epochs {
		eventID := ingestTestEvent(t, store, EventInput{
			Project:         "memoire-test",
			SessionID:       "session-cursor",
			SourceAdapter:   "test",
			SourceEventID:   "event-cursor-" + strconv.Itoa(i+1),
			EventType:       "note",
			OccurredAtEpoch: epoch,
			Payload:         json.RawMessage(`{"message":"cursor"}`),
		})
		obsIDs = append(obsIDs, rawObservationIDForEvent(t, store, eventID))
	}
	obs1, obs2, obs3, obs4, obs5, obs6, obs7 := obsIDs[0], obsIDs[1], obsIDs[2], obsIDs[3], obsIDs[4], obsIDs[5], obsIDs[6]

	base := ObservationFilter{Project: "memoire-test", SessionID: "session-cursor", Limit: 3}

	descWant := []int64{obs7, obs6, obs5, obs4, obs3, obs2, obs1}
	descGot := walkObservationCursor(t, store, base)
	assertObservationOrder(t, "desc-cursor-walk", toRecords(descGot), descWant)

	ascFilter := base
	ascFilter.Order = "asc"
	ascWant := []int64{obs1, obs2, obs3, obs4, obs5, obs6, obs7}
	ascGot := walkObservationCursor(t, store, ascFilter)
	assertObservationOrder(t, "asc-cursor-walk", toRecords(ascGot), ascWant)

	// Malformed cursors: wrong segment count, non-integer parts.
	for _, bad := range []string{"banana", "1:2:3", "x:1", "1:x"} {
		badFilter := base
		badFilter.Cursor = bad
		if _, _, err := store.ListObservations(context.Background(), badFilter); !errors.Is(err, ErrBadCursor) {
			t.Fatalf("cursor=%q: expected ErrBadCursor, got %v", bad, err)
		}
	}

	// Filter change (kind) combined with cursor: WHERE composition must
	// still apply the cursor bound alongside the kind filter, not just one
	// or the other. Seed 3 "fact" observations at epochs interleaved with
	// the raw group above (150, 250, 350) so a broken composition (cursor
	// bound applied without the kind filter, or vice versa) would leak raw
	// rows into the fact-only walk or skip/duplicate fact rows.
	runID := ensureTestRun(t, store, DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "session-cursor",
		Provider:          "rules",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:cursor-run",
		WindowStartEpoch:  100,
		WindowEndEpoch:    500,
		Status:            "completed",
	})
	sourceEventID := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "session-cursor",
		SourceAdapter:   "test",
		SourceEventID:   "event-cursor-fact-source",
		EventType:       "note",
		OccurredAtEpoch: 1,
		Payload:         json.RawMessage(`{"message":"fact source"}`),
	})
	var factIDs []int64
	for i, epoch := range []int64{150, 250, 350} {
		res, err := store.InsertDerivedObservation(context.Background(), DerivedObservationInput{
			Project:         "memoire-test",
			SessionID:       "session-cursor",
			Kind:            "fact",
			Text:            "cursor fact",
			OccurredAtEpoch: epoch,
			ReviewState:     "accepted",
			DerivationRunID: runID,
			ContentHash:     "sha256:cursor-fact-" + strconv.Itoa(i+1),
			SourceEventIDs:  []int64{sourceEventID},
		})
		if err != nil {
			t.Fatal(err)
		}
		factIDs = append(factIDs, res.ID)
	}
	factWant := []int64{factIDs[2], factIDs[1], factIDs[0]} // desc by epoch: 350, 250, 150

	kindFilter := ObservationFilter{Project: "memoire-test", SessionID: "session-cursor", Kind: "fact", Limit: 1}
	kindGot := walkObservationCursor(t, store, kindFilter)
	assertObservationOrder(t, "kind+cursor-walk", toRecords(kindGot), factWant)
}

// walkObservationCursor drains filter page by page via next_cursor until it
// comes back empty, returning the concatenated ids in the order they were
// returned. It fails the test on any duplicate id across pages (overlap) or
// if the walk does not terminate within a generous page budget.
func walkObservationCursor(t *testing.T, store *Store, filter ObservationFilter) []int64 {
	t.Helper()
	var got []int64
	seen := map[int64]bool{}
	cursor := ""
	for pageNum := 0; ; pageNum++ {
		if pageNum > 20 {
			t.Fatalf("cursor walk did not terminate after %d pages: %v", pageNum, got)
		}
		pageFilter := filter
		pageFilter.Cursor = cursor
		recs, next, err := store.ListObservations(context.Background(), pageFilter)
		if err != nil {
			t.Fatalf("page %d: %v", pageNum, err)
		}
		for _, rec := range recs {
			if seen[rec.ID] {
				t.Fatalf("page %d: duplicate observation %d across pages: %v", pageNum, rec.ID, got)
			}
			seen[rec.ID] = true
			got = append(got, rec.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return got
}

func toRecords(ids []int64) []ObservationRecord {
	recs := make([]ObservationRecord, len(ids))
	for i, id := range ids {
		recs[i] = ObservationRecord{ID: id}
	}
	return recs
}

func assertObservationOrder(t *testing.T, order string, recs []ObservationRecord, wantIDs []int64) {
	t.Helper()
	if len(recs) != len(wantIDs) {
		t.Fatalf("order=%q: expected %d observations, got %d: %#v", order, len(wantIDs), len(recs), recs)
	}
	for i, want := range wantIDs {
		if recs[i].ID != want {
			var gotIDs []int64
			for _, r := range recs {
				gotIDs = append(gotIDs, r.ID)
			}
			t.Fatalf("order=%q: expected id order %v, got %v", order, wantIDs, gotIDs)
		}
	}
}
