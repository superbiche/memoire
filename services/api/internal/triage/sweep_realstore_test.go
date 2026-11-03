// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package triage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"memoire/services/api/internal/contenthash"
	"memoire/services/api/internal/db"
)

// The sweep driven end-to-end against the REAL store (memoire:D49):
// fakeness confined to the provider, which is the external boundary. The
// driver tests elsewhere in this package use an in-memory store to isolate
// chunking and failure posture; this one proves the scope read, the
// idempotency index, and the suggestion write survive real SQL.
func TestSweepAgainstTheRealStore(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	ev, err := store.Ingest(ctx, db.EventInput{
		SessionID: "real-sweep", Project: "memoire-test", SourceAdapter: "test",
		SourceEventID: "real-sweep-1", EventType: "PostToolUse",
		Payload: []byte(`{"k":"v"}`), Text: "seed", OccurredAtEpoch: time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.EnsureDerivationRun(ctx, db.DerivationRunInput{
		Project: "memoire-test", SessionID: "real-sweep", Provider: "rules",
		DataBoundary: "local", DerivationVersion: "v1", InputHash: "h-real-sweep",
		WindowStartEpoch: 1, WindowEndEpoch: 2, Status: "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, text := range []string{"a synthesized outcome", "awaiting the other lane"} {
		hash, err := contenthash.Canonical("memoire-test", "fact", text)
		if err != nil {
			t.Fatal(err)
		}
		res, err := store.InsertDerivedObservation(ctx, db.DerivedObservationInput{
			Project: "memoire-test", SessionID: "real-sweep", Kind: "fact", Text: text,
			ReviewState: "proposed", DerivationRunID: run.ID, ContentHash: hash,
			SourceEventIDs: []int64{ev.ID}, Provenance: "inferred",
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.ID)
	}

	judge := &stubJudge{responses: [][]byte{verdictsJSON(t,
		Verdict{ObservationID: ids[0], Verdict: db.TriageVerdictAccept},
		Verdict{ObservationID: ids[1], Verdict: db.TriageVerdictNoiseLapse, Genre: "orchestration-chatter"},
	)}}
	opts := SweepOptions{Stage: 1, EndpointID: "hl5", Model: "judge-model", SessionID: "real-sweep"}

	out, err := Sweep(ctx, store, judge, opts)
	if err != nil {
		t.Fatal(err)
	}
	if out.RowsJudged != 2 || out.Suggestions != 2 {
		t.Fatalf("unexpected outcome %+v", out)
	}

	rows, err := store.ListTriageScope(ctx, db.TriageScopeFilter{SessionID: "real-sweep", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if len(r.Suggestions) != 1 {
			t.Fatalf("#%d carries %d suggestions, want 1", r.Observation.ID, len(r.Suggestions))
		}
		// Nothing the sweep did may have moved the row.
		if r.Observation.ReviewState != "proposed" {
			t.Fatalf("#%d is %q — the sweep moved a row it may not touch", r.Observation.ID, r.Observation.ReviewState)
		}
	}

	// A second pass writes nothing new AND says so: the rows are already
	// judged at this prompt version, so the scope read skips them and the
	// judge is never called again. A pass that re-counted them would report
	// a full night's work while writing nothing.
	before := judge.calls
	again, err := Sweep(ctx, store, judge, opts)
	if err != nil {
		t.Fatal(err)
	}
	if again.Suggestions != 0 || again.RowsJudged != 0 {
		t.Fatalf("the second pass claimed work: %+v", again)
	}
	if judge.calls != before {
		t.Fatalf("the second pass burned %d judge calls on already-judged rows", judge.calls-before)
	}
	rows, err = store.ListTriageScope(ctx, db.TriageScopeFilter{SessionID: "real-sweep", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if len(r.Suggestions) != 1 {
			t.Fatalf("re-suggestion duplicated a row: #%d has %d", r.Observation.ID, len(r.Suggestions))
		}
	}
}
