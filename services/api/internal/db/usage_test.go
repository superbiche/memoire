// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func newUsageStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// rawObservationID returns the 'raw' observation Ingest creates for an event.
func rawObservationID(t *testing.T, store *Store, eventID int64) int64 {
	t.Helper()
	var id int64
	if err := store.rdb.QueryRow(`SELECT id FROM observations WHERE event_id = ?`, eventID).Scan(&id); err != nil {
		t.Fatalf("raw observation for event %d: %v", eventID, err)
	}
	return id
}

func ingestUsage(t *testing.T, store *Store, sessionID, sourceEventID string, occurred int64, served, verdicts string) IngestResult {
	t.Helper()
	res, err := store.Ingest(context.Background(), EventInput{
		Project:         "memoire-test",
		SessionID:       sessionID,
		SourceAdapter:   "claude-code",
		SourceEventID:   sourceEventID,
		EventType:       EventTypeMemoryUsage,
		OccurredAtEpoch: occurred,
		Payload:         json.RawMessage(`{"hook_version":"2","served_ids":` + served + `,"verdicts":` + verdicts + `}`),
		Text:            "Memoire attribution at Stop",
	})
	if err != nil {
		t.Fatalf("ingest usage: %v", err)
	}
	return res
}

func TestMemoryUsageProjection(t *testing.T) {
	store := newUsageStore(t)
	ctx := context.Background()

	seed, err := store.Ingest(ctx, EventInput{Project: "memoire-test", SessionID: "s0", SourceAdapter: "manual",
		EventType: "note", OccurredAtEpoch: 1000, Payload: json.RawMessage(`{}`), Text: "the fact"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	obs := rawObservationID(t, store, seed.ID)
	seed2, err := store.Ingest(ctx, EventInput{Project: "memoire-test", SessionID: "s0", SourceAdapter: "manual",
		EventType: "note", OccurredAtEpoch: 1001, Payload: json.RawMessage(`{}`), Text: "another fact"})
	if err != nil {
		t.Fatalf("seed2: %v", err)
	}
	obs2 := rawObservationID(t, store, seed2.ID)

	// One turn: the first observation used, an unknown id, an unknown verdict, a duplicate id.
	verdicts := `[{"id":` + itoa(obs) + `,"verdict":"used","reason":"carried the webhook ordering"},
		{"id":999999,"verdict":"used","reason":"names no row"},
		{"id":` + itoa(obs2) + `,"verdict":"seen","reason":"not in the grammar"},
		{"id":` + itoa(obs) + `,"verdict":"unused","reason":"duplicate line, first wins"}]`
	servedBoth := "[" + itoa(obs) + "," + itoa(obs2) + ",999999]"
	first := ingestUsage(t, store, "s1", "claude-memory-usage-1", 2000, servedBoth, verdicts)
	if first.Duplicate {
		t.Fatalf("first usage event flagged duplicate")
	}
	// Replay of the same queue item: idempotent, no second projection.
	replay := ingestUsage(t, store, "s1", "claude-memory-usage-1", 2000, servedBoth, verdicts)
	if !replay.Duplicate {
		t.Fatalf("replayed usage event not flagged duplicate")
	}
	// A second session marks it unused, and fetch-failed on the other one.
	ingestUsage(t, store, "s2", "claude-memory-usage-2", 3000, servedBoth,
		`[{"id":`+itoa(obs)+`,"verdict":"unused","reason":"seen, not relied on"},{"id":`+itoa(obs2)+`,"verdict":"fetch-failed","reason":"body fetch failed"}]`)

	lines, err := store.ListObservationUsage(ctx, obs, 0)
	if err != nil {
		t.Fatalf("list usage: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("want 2 usage lines, got %d: %+v", len(lines), lines)
	}
	if lines[0].SessionID != "s2" || lines[0].Verdict != "unused" || lines[0].OccurredAtEpoch != 3000 {
		t.Fatalf("newest line wrong: %+v", lines[0])
	}
	if lines[1].SessionID != "s1" || lines[1].Verdict != "used" || lines[1].Reason != "carried the webhook ordering" || lines[1].EventID != first.ID {
		t.Fatalf("oldest line wrong: %+v", lines[1])
	}
	unknown, err := store.ListObservationUsage(ctx, 999999, 0)
	if err != nil || len(unknown) != 0 {
		t.Fatalf("unknown id must project nothing: %v %+v", err, unknown)
	}

	recs := []ObservationRecord{{ID: obs}, {ID: obs2}, {ID: 424242}}
	if err := store.AttachUsage(ctx, recs); err != nil {
		t.Fatalf("attach: %v", err)
	}
	u := recs[0].Usage
	if u == nil || u.Used != 1 || u.Unused != 1 || u.Sessions != 2 || u.LastAtEpoch != 3000 || u.LastUsedAtEpoch != 2000 {
		t.Fatalf("summary wrong: %+v", u)
	}
	u2 := recs[1].Usage
	if u2 == nil || u2.FetchFailed != 1 || u2.Used != 0 || u2.Sessions != 1 {
		t.Fatalf("summary 2 wrong: %+v", u2)
	}
	if recs[2].Usage != nil {
		t.Fatalf("no signal must stay nil, got %+v", recs[2].Usage)
	}

	// An id the model names that the turn was NOT served must never become a
	// usage signal, even when it names an existing observation (review r1 F3).
	seed3, err := store.Ingest(ctx, EventInput{Project: "another-project", SessionID: "s0", SourceAdapter: "manual",
		EventType: "note", OccurredAtEpoch: 1002, Payload: json.RawMessage(`{}`), Text: "an unrelated fact"})
	if err != nil {
		t.Fatalf("seed3: %v", err)
	}
	obs3 := rawObservationID(t, store, seed3.ID)
	invented := ingestUsage(t, store, "s4", "claude-memory-usage-4", 5000, "["+itoa(obs)+"]",
		`[{"id":`+itoa(obs3)+`,"verdict":"used","reason":"invented by the model, never served"}]`)
	var inventedRows int
	if err := store.rdb.QueryRow(`SELECT COUNT(*) FROM observation_usage WHERE event_id = ?`, invented.ID).Scan(&inventedRows); err != nil || inventedRows != 0 {
		t.Fatalf("unserved id projected %d rows (err %v)", inventedRows, err)
	}
	if l, err := store.ListObservationUsage(ctx, obs3, 0); err != nil || len(l) != 0 {
		t.Fatalf("unserved existing observation got usage lines: %+v err %v", l, err)
	}
	// No served_ids at all: nothing is projected.
	noServed, err := store.Ingest(ctx, EventInput{Project: "memoire-test", SessionID: "s5", SourceAdapter: "claude-code",
		SourceEventID: "claude-memory-usage-5", EventType: EventTypeMemoryUsage, OccurredAtEpoch: 6000,
		Payload: json.RawMessage(`{"verdicts":[{"id":` + itoa(obs) + `,"verdict":"used","reason":"no served set"}]}`), Text: "no served"})
	if err != nil {
		t.Fatalf("ingest without served_ids: %v", err)
	}
	if err := store.rdb.QueryRow(`SELECT COUNT(*) FROM observation_usage WHERE event_id = ?`, noServed.ID).Scan(&inventedRows); err != nil || inventedRows != 0 {
		t.Fatalf("event without served_ids projected %d rows (err %v)", inventedRows, err)
	}

	// A malformed payload keeps the event and projects nothing.
	bad, err := store.Ingest(ctx, EventInput{Project: "memoire-test", SessionID: "s3", SourceAdapter: "claude-code",
		SourceEventID: "claude-memory-usage-3", EventType: EventTypeMemoryUsage, OccurredAtEpoch: 4000,
		Payload: json.RawMessage(`{"verdicts":"not a list"}`), Text: "bad"})
	if err != nil || bad.ID == 0 {
		t.Fatalf("malformed usage payload must still ingest: %v", err)
	}
	var n int
	if err := store.rdb.QueryRow(`SELECT COUNT(*) FROM observation_usage WHERE event_id = ?`, bad.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("malformed payload projected %d rows (err %v)", n, err)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
