// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

// READ-ONLY CAGE (2026-07-02, the human operator's ruling: "failed never dedups").
//
// These are the contract tests for failed-run retry semantics on the -execute
// derivation path. They pin BEHAVIOR at the orchestrator boundary; the
// implementation may reshape internals freely within the allowed-files list.
//
// The ruled contract (amended 2026-08-01, coverage-exclusion slice):
//   - status=completed: the run's recorded input manifest covers its events,
//     so a re-run of the window skips at the fetch with
//     Skipped=fully_covered — no run-table touch, provider never called.
//     The input-hash Duplicate short-circuit still fires when the manifest
//     is absent (pre-0015 legacy rows; proven in rules_test.go R001 tail).
//   - status=partial satisfies idempotency: a re-run of the same (provider,
//     model, derivation_version, project, session, window, input_hash)
//     tuple returns Duplicate=true and never calls the provider.
//   - status=failed NEVER satisfies idempotency: the next Run of the same tuple
//     re-executes automatically (no new flag), reuses the SAME run row (same
//     run id), and records the attempt count as a top-level `retry_count`
//     number in the run's meta_json (1 on the first retry, incrementing).
//   - status=running still dedups (stuck-running recovery is explicitly out of
//     scope for this slice).
//
// Editing this file is a failed run. Done additionally requires:
//   go test -tags sqlite_fts5 ./services/api/...   -> ok (all packages)

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"memoire/services/api/internal/db"
)

// retryContractSeed seeds one prompt event and returns a request bound to its
// session so runs are deterministic per test.
func retryContractSeed(t *testing.T, store *db.Store, sessionID string) Request {
	t.Helper()
	seedPrompt(t, store, sessionID, "retry-src-"+sessionID, 1_690_000_100, "use Go for the API service")
	return orchRequest(sessionID)
}

func retryContractRun(t *testing.T, store *db.Store, id string) db.DerivationRunRecord {
	t.Helper()
	rec, err := store.GetDerivationRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetDerivationRun(%q): %v", id, err)
	}
	return rec
}

// assertRunRowUntouched pins the dedup contract at the row level (Codex review
// 2026-07-02): a duplicate path that mutates status, creation time, or metadata
// before returning Duplicate=true is a contract violation even if the provider
// was never called.
func assertRunRowUntouched(t *testing.T, before, after db.DerivationRunRecord) {
	t.Helper()
	if after.Status != before.Status {
		t.Fatalf("dedup mutated status: %q -> %q", before.Status, after.Status)
	}
	if after.CreatedAtEpoch != before.CreatedAtEpoch {
		t.Fatalf("dedup mutated created_at_epoch: %d -> %d", before.CreatedAtEpoch, after.CreatedAtEpoch)
	}
	if after.CompletedAtEpoch != before.CompletedAtEpoch {
		t.Fatalf("dedup mutated completed_at_epoch: %d -> %d", before.CompletedAtEpoch, after.CompletedAtEpoch)
	}
	if string(after.Metadata) != string(before.Metadata) {
		t.Fatalf("dedup mutated meta_json:\nbefore: %s\nafter:  %s", string(before.Metadata), string(after.Metadata))
	}
}

func retryCountOf(t *testing.T, rec db.DerivationRunRecord) float64 {
	t.Helper()
	var meta map[string]any
	if len(rec.Metadata) == 0 {
		t.Fatalf("run %s has empty meta_json; expected retry_count", rec.ID)
	}
	if err := json.Unmarshal(rec.Metadata, &meta); err != nil {
		t.Fatalf("meta_json unmarshal: %v", err)
	}
	count, ok := meta["retry_count"].(float64)
	if !ok {
		t.Fatalf("meta_json has no numeric retry_count: %s", string(rec.Metadata))
	}
	return count
}

func TestRetryContractCompletedRunDedups(t *testing.T) {
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()
	req := retryContractSeed(t, store, "retry-completed")

	provider := localFake(func(ctx context.Context, r Request) (ProviderResponse, error) {
		return validResponse(r), nil
	})

	first, err := o.Run(ctx, provider, req)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if first.Duplicate {
		t.Fatalf("first run must not be a duplicate")
	}
	before := retryContractRun(t, store, first.RunID)
	if before.Status != "completed" {
		t.Fatalf("first run status = %q, want completed", before.Status)
	}

	// A completed run records its input manifest, so the rerun's fetch
	// finds nothing uncovered and skips before touching the run table —
	// the ExcludeCovered successor of the input-hash duplicate
	// short-circuit (which still guards partial/failed and manifest-less
	// legacy runs).
	second, err := o.Run(ctx, provider, req)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.Skipped != SkipReasonFullyCovered {
		t.Fatalf("completed run rerun must skip fully_covered, got %#v", second)
	}
	if provider.calls != 1 {
		t.Fatalf("provider must not be re-called for a completed run, calls=%d", provider.calls)
	}
	assertRunRowUntouched(t, before, retryContractRun(t, store, first.RunID))
}

func TestRetryContractPartialRunDedups(t *testing.T) {
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()
	req := retryContractSeed(t, store, "retry-partial")

	provider := localFake(func(ctx context.Context, r Request) (ProviderResponse, error) {
		return validResponse(r), nil
	})
	first, err := o.Run(ctx, provider, req)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Force the terminal state to partial: partial wrote observations, so the
	// ruled contract keeps it idempotent (re-running could double-write).
	if err := store.UpdateDerivationRunStatus(ctx, db.DerivationRunStatusUpdate{
		ID: first.RunID, Status: "partial", CompletedAtEpoch: 1_700_000_010,
	}); err != nil {
		t.Fatalf("force partial: %v", err)
	}
	before := retryContractRun(t, store, first.RunID)

	second, err := o.Run(ctx, provider, req)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !second.Duplicate || second.RunID != first.RunID {
		t.Fatalf("partial run must dedup: duplicate=%t run_id=%s", second.Duplicate, second.RunID)
	}
	if provider.calls != 1 {
		t.Fatalf("provider must not be re-called for a partial run, calls=%d", provider.calls)
	}
	assertRunRowUntouched(t, before, retryContractRun(t, store, first.RunID))
}

func TestRetryContractRunningRunStillDedups(t *testing.T) {
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()
	req := retryContractSeed(t, store, "retry-running")

	provider := localFake(func(ctx context.Context, r Request) (ProviderResponse, error) {
		return validResponse(r), nil
	})
	first, err := o.Run(ctx, provider, req)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := store.UpdateDerivationRunStatus(ctx, db.DerivationRunStatusUpdate{
		ID: first.RunID, Status: "running",
	}); err != nil {
		t.Fatalf("force running: %v", err)
	}
	before := retryContractRun(t, store, first.RunID)

	second, err := o.Run(ctx, provider, req)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !second.Duplicate || second.RunID != first.RunID {
		t.Fatalf("running run must still dedup (stuck-running recovery is out of scope): duplicate=%t", second.Duplicate)
	}
	if provider.calls != 1 {
		t.Fatalf("provider must not be re-called for a running run, calls=%d", provider.calls)
	}
	assertRunRowUntouched(t, before, retryContractRun(t, store, first.RunID))
}

func TestRetryContractFailedRunRetriesInPlace(t *testing.T) {
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()
	req := retryContractSeed(t, store, "retry-failed")

	shouldFail := true
	provider := localFake(func(ctx context.Context, r Request) (ProviderResponse, error) {
		if shouldFail {
			return ProviderResponse{}, errors.New("synthetic provider outage")
		}
		return validResponse(r), nil
	})

	first, err := o.Run(ctx, provider, req)
	if err != nil {
		t.Fatalf("first run should record failure as data, not error: %v", err)
	}
	failedRec := retryContractRun(t, store, first.RunID)
	if failedRec.Status != "failed" {
		t.Fatalf("first run status = %q, want failed", failedRec.Status)
	}
	if provider.calls != 1 {
		t.Fatalf("first run should call provider once, calls=%d", provider.calls)
	}

	// The ruled contract: a failed run never dedups — the same tuple
	// re-executes automatically, reusing the SAME run row.
	shouldFail = false
	second, err := o.Run(ctx, provider, req)
	if err != nil {
		t.Fatalf("retry run: %v", err)
	}
	if second.Duplicate {
		t.Fatalf("failed run must NOT dedup: got duplicate=true")
	}
	if second.RunID != first.RunID {
		t.Fatalf("retry must reuse the same run row: got %s, want %s", second.RunID, first.RunID)
	}
	if provider.calls != 2 {
		t.Fatalf("retry must re-call the provider, calls=%d", provider.calls)
	}

	rec := retryContractRun(t, store, first.RunID)
	if rec.Status != "completed" {
		t.Fatalf("retried run status = %q, want completed", rec.Status)
	}
	// Row continuity (Codex review 2026-07-02): the retry must reuse the
	// physical row, not delete-and-reinsert under the same id.
	if rec.CreatedAtEpoch != failedRec.CreatedAtEpoch {
		t.Fatalf("retry changed created_at_epoch: %d -> %d (row not reused in place)", failedRec.CreatedAtEpoch, rec.CreatedAtEpoch)
	}
	if got := retryCountOf(t, rec); got != 1 {
		t.Fatalf("retry_count = %v, want 1 after first retry", got)
	}
	if rec.ObservationCount == 0 {
		t.Fatalf("successful retry must write observations")
	}
}

func TestRetryContractRepeatedFailuresIncrementRetryCount(t *testing.T) {
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()
	req := retryContractSeed(t, store, "retry-repeat")

	attempts := 0
	provider := localFake(func(ctx context.Context, r Request) (ProviderResponse, error) {
		attempts++
		if attempts <= 2 {
			return ProviderResponse{}, errors.New("synthetic provider outage")
		}
		return validResponse(r), nil
	})

	first, err := o.Run(ctx, provider, req)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if _, err := o.Run(ctx, provider, req); err != nil {
		t.Fatalf("run 2 (retry, fails again): %v", err)
	}
	rec := retryContractRun(t, store, first.RunID)
	if rec.Status != "failed" {
		t.Fatalf("after second failure status = %q, want failed", rec.Status)
	}
	if got := retryCountOf(t, rec); got != 1 {
		t.Fatalf("retry_count after first retry = %v, want 1", got)
	}

	third, err := o.Run(ctx, provider, req)
	if err != nil {
		t.Fatalf("run 3 (retry, succeeds): %v", err)
	}
	if third.Duplicate || third.RunID != first.RunID {
		t.Fatalf("third run must be an in-place retry: duplicate=%t run_id=%s", third.Duplicate, third.RunID)
	}
	rec = retryContractRun(t, store, first.RunID)
	if rec.Status != "completed" {
		t.Fatalf("final status = %q, want completed", rec.Status)
	}
	if got := retryCountOf(t, rec); got != 2 {
		t.Fatalf("retry_count after second retry = %v, want 2", got)
	}
	if provider.calls != 3 {
		t.Fatalf("provider calls = %d, want 3", provider.calls)
	}
}
