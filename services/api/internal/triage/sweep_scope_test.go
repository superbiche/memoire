// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package triage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"memoire/services/api/internal/contenthash"
	"memoire/services/api/internal/db"
)

// seedScope creates n proposed rows spread across the named sessions.
func seedScope(t *testing.T, sessions []string, perSession int) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	for _, sess := range sessions {
		ev, err := store.Ingest(ctx, db.EventInput{
			SessionID: sess, Project: "memoire-test", SourceAdapter: "test",
			SourceEventID: "seed-" + sess, EventType: "PostToolUse",
			Payload: []byte(`{"k":"v"}`), Text: "seed", OccurredAtEpoch: time.Now().Unix(),
		})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.EnsureDerivationRun(ctx, db.DerivationRunInput{
			Project: "memoire-test", SessionID: sess, Provider: "rules",
			DataBoundary: "local", DerivationVersion: "v1", InputHash: "h-" + sess,
			WindowStartEpoch: 1, WindowEndEpoch: 2, Status: "completed",
		})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < perSession; i++ {
			text := fmt.Sprintf("%s row %d", sess, i)
			hash, err := contenthash.Canonical("memoire-test", "fact", text)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.InsertDerivedObservation(ctx, db.DerivedObservationInput{
				Project: "memoire-test", SessionID: sess, Kind: "fact", Text: text,
				ReviewState: "proposed", DerivationRunID: run.ID, ContentHash: hash,
				SourceEventIDs: []int64{ev.ID}, Provenance: "inferred",
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return store
}

// echoJudge answers every chunk by accepting exactly the rows it was shown,
// and records which sessions each prompt carried.
type echoJudge struct {
	sessionsPerPrompt []map[string]bool
}

func (j *echoJudge) Chat(_ context.Context, _, _, prompt string) ([]byte, error) {
	seen := map[string]bool{}
	var vs []Verdict
	for _, line := range strings.Split(prompt, "\n") {
		if !strings.HasPrefix(line, "### id=") {
			continue
		}
		var id int64
		var kind, project, state string
		fmt.Sscanf(line, "### id=%d kind=%s project=%s state=%s", &id, &kind, &project, &state)
		if id == 0 {
			continue
		}
		vs = append(vs, Verdict{ObservationID: id, Verdict: db.TriageVerdictAccept})
	}
	// Session membership is read from the rendered rows via their text.
	for _, line := range strings.Split(prompt, "\n") {
		for _, s := range []string{"alpha", "beta"} {
			if strings.HasPrefix(line, s+" row ") {
				seen[s] = true
			}
		}
	}
	j.sessionsPerPrompt = append(j.sessionsPerPrompt, seen)
	return verdictsJSONRaw(vs), nil
}

func verdictsJSONRaw(vs []Verdict) []byte {
	var b strings.Builder
	b.WriteString(`{"schema_version":"1","verdicts":[`)
	for i, v := range vs {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"observation_id":%d,"verdict":%q}`, v.ObservationID, v.Verdict)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func countSuggestions(t *testing.T, store *db.Store) int {
	t.Helper()
	rows, err := store.ListTriageScope(context.Background(), db.TriageScopeFilter{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		n += len(r.Suggestions)
	}
	return n
}

// F1: the sweep must walk every session and page until the selected scope is
// exhausted, never mixing sessions in one prompt, and must report inserted
// vs skipped honestly.
func TestSweepExhaustsTheScopeAcrossSessions(t *testing.T) {
	store := seedScope(t, []string{"alpha", "beta"}, 101) // 202 in-scope rows
	judge := &echoJudge{}
	opts := SweepOptions{Stage: 1, EndpointID: "hl5", Model: "judge-model", Limit: 200}

	out, err := Sweep(context.Background(), store, judge, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := countSuggestions(t, store); got != 202 {
		t.Errorf("one pass suggested %d of 202 rows — the scope must be exhausted, not one capped prefix", got)
	}
	if out.Suggestions != 202 {
		t.Errorf("outcome reported %d suggestions, want 202", out.Suggestions)
	}
	for i, seen := range judge.sessionsPerPrompt {
		if len(seen) > 1 {
			t.Errorf("prompt %d mixed sessions %v — the judging unit is one session, chunked", i, seen)
		}
	}

	// A second pass writes nothing new and must SAY so rather than counting
	// ignored duplicate inserts as fresh work.
	again, err := Sweep(context.Background(), store, judge, opts)
	if err != nil {
		t.Fatal(err)
	}
	if again.Suggestions != 0 {
		t.Errorf("second pass reported %d new suggestions; duplicates must not count as inserts", again.Suggestions)
	}
	if got := countSuggestions(t, store); got != 202 {
		t.Errorf("second pass changed the stored count to %d", got)
	}
}

type deadJudge struct{ calls int }

func (d *deadJudge) Chat(context.Context, string, string, string) ([]byte, error) {
	d.calls++
	return nil, errors.New("endpoint unavailable")
}

type silentJudge struct{ calls int }

func (s *silentJudge) Chat(context.Context, string, string, string) ([]byte, error) {
	s.calls++
	return []byte(`{"schema_version":"1","verdicts":[]}`), nil
}

// The budget bounds rows JUDGED, not pages fetched: a budget of 1 costs one
// row, not one page.
func TestBudgetBoundsRowsNotPages(t *testing.T) {
	store := seedScope(t, []string{"alpha"}, 40)
	judge := &echoJudge{}
	out, err := Sweep(context.Background(), store, judge, SweepOptions{
		Stage: 1, EndpointID: "hl5", Model: "m", Limit: 40, Budget: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.RowsAttempted != 1 || out.Suggestions != 1 {
		t.Fatalf("budget=1 must cost exactly one row, got attempted=%d suggestions=%d", out.RowsAttempted, out.Suggestions)
	}
	if !out.BudgetReached {
		t.Error("a pass stopped by its budget must say so — 'nothing left' and 'more waiting' must not look alike")
	}
}

// A FAILED chunk consumes budget too. Without that a persistently failing
// judge spins through the whole backlog for free, which is the opposite of
// a cost bound.
func TestFailedChunksConsumeBudget(t *testing.T) {
	store := seedScope(t, []string{"beta"}, 40)
	dead := &deadJudge{}
	out, err := Sweep(context.Background(), store, dead, SweepOptions{
		Stage: 1, EndpointID: "hl5", Model: "m", Limit: 10, MaxRowsPerChunk: 5, Budget: 5,
	})
	if err == nil {
		t.Fatal("a pass whose every chunk failed must not report success")
	}
	if out.RowsAttempted != 5 {
		t.Fatalf("failed chunks must consume budget, attempted=%d want 5", out.RowsAttempted)
	}
	if dead.calls != 1 {
		t.Fatalf("the budget must stop the dead judge after one chunk, got %d calls", dead.calls)
	}
}

// "Evaluated, nothing to suggest" is recorded, so a re-sweep skips the row
// and paging can exhaust the scope.
func TestCoverageRowsLetTheSweepExhaust(t *testing.T) {
	store := seedScope(t, []string{"gamma"}, 3)
	silent := &silentJudge{}
	opts := SweepOptions{Stage: 1, EndpointID: "hl5", Model: "m", Limit: 10}

	out, err := Sweep(context.Background(), store, silent, opts)
	if err != nil {
		t.Fatal(err)
	}
	// Every judged row gets a coverage row — and coverage rows are counted
	// as COVERAGE, never as suggestions. Reporting three coverage rows as
	// three suggestions would say the judge nominated three rows when it
	// nominated none.
	if out.RowsJudged != 3 || out.Covered != 3 || out.Suggestions != 0 {
		t.Fatalf("want 3 judged, 3 covered, 0 suggested, got %+v", out)
	}
	again, err := Sweep(context.Background(), store, silent, opts)
	if err != nil {
		t.Fatal(err)
	}
	if again.RowsJudged != 0 {
		t.Fatalf("the second pass must skip judged rows, judged=%d", again.RowsJudged)
	}
	if silent.calls != 1 {
		t.Fatalf("the judge must not be re-burned on judged rows, got %d calls", silent.calls)
	}
}
