// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"memoire/services/api/internal/authctx"
)

func TestIngestSearchDuplicateAndSoftDelete(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	event := EventInput{
		Project:       "memoire-test",
		SessionID:     "session-1",
		SourceAdapter: "test",
		SourceEventID: "event-1",
		EventType:     "note",
		Payload:       json.RawMessage(`{"message":"alpha needle"}`),
		Text:          "alpha needle",
	}

	first, err := store.Ingest(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if first.Duplicate {
		t.Fatal("first insert reported duplicate")
	}

	second, err := store.Ingest(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Duplicate || second.ID != first.ID {
		t.Fatalf("duplicate mismatch: %#v then %#v", first, second)
	}

	hits, err := store.Search(context.Background(), "alpha needle", "memoire-test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected one search hit, got %d", len(hits))
	}

	if err := store.DeleteEvent(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	hits, err = store.Search(context.Background(), "alpha needle", "memoire-test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected soft-deleted event to be hidden, got %d hits", len(hits))
	}
}

func TestSanitizeFTS(t *testing.T) {
	got := SanitizeFTS(`PgBouncer OR "Horizon"; rm -rf`)
	want := `"PgBouncer" OR "OR" OR "Horizon" OR "rm" OR "-rf"`
	if got != want {
		t.Fatalf("sanitize mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestClaudeUserPromptProjection(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	event := EventInput{
		Project:       "memoire-test",
		SessionID:     "session-prompt",
		SourceAdapter: "claude-code",
		SourceEventID: "prompt-1",
		EventType:     "claude_UserPromptSubmit",
		Payload:       json.RawMessage(`{"prompt":"prompt parity needle","raw_only":"zzrawpayloadneedle"}`),
	}

	if _, err := store.Ingest(context.Background(), event); err != nil {
		t.Fatal(err)
	}

	hits, err := store.Search(context.Background(), "prompt parity needle", "memoire-test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected prompt text to be searchable, got %d hits", len(hits))
	}

	hits, err = store.Search(context.Background(), "zzrawpayloadneedle", "memoire-test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected non-prompt raw payload fields to stay out of search, got %d hits", len(hits))
	}
}

func TestOpenAppliesSchemaMigrationsOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memoire.db")
	store, err := Open(dbPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{4, 5, 6} {
		var version int
		if err := store.db.QueryRowContext(context.Background(), `SELECT version FROM schema_version WHERE version = ?`, want).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != want {
			t.Fatalf("expected schema version %d, got %d", want, version)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dbPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
}

func TestDerivationModelMigrationPreservesRunCount(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memoire.db")
	conn, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"0001_initial.sql", "0004_derivation.sql", "0005_observation_review_log.sql"} {
		b, err := os.ReadFile(filepath.Join("../../../../schema", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(string(b)); err != nil {
			t.Fatalf("apply old migration %s: %v", name, err)
		}
	}
	if _, err := conn.Exec(`INSERT INTO derivation_runs(
		id, project, session_id, provider, data_boundary, derivation_version, prompt_version,
		input_hash, window_start_epoch, window_end_epoch, status, created_at_epoch, completed_at_epoch, meta_json
	) VALUES
		('drv_old_rules', 'memoire-test', 'session-old', 'rules', 'local', 'observation-derive-v1', NULL, 'sha256:old-rules', 100, 200, 'completed', 300, 200, '{}'),
		('drv_old_manual', 'memoire-test', 'session-old', 'manual', 'local', 'observation-derive-v1', NULL, 'sha256:old-manual', 101, 201, 'completed', 301, 201, '{}')`); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM derivation_runs`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(dbPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var after int
	if err := store.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM derivation_runs`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("derivation run count changed across model migration: before=%d after=%d", before, after)
	}
	var version int
	if err := store.db.QueryRowContext(context.Background(), `SELECT version FROM schema_version WHERE version = 6`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 6 {
		t.Fatalf("expected schema version 6, got %d", version)
	}
}

func TestEnsureDerivationRunIdempotency(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	in := DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "session-1",
		Provider:          "openai",
		Model:             "gpt-4o-mini",
		DataBoundary:      "external",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:input-a",
		WindowStartEpoch:  100,
		WindowEndEpoch:    200,
		Status:            "completed",
		CreatedAtEpoch:    300,
	}

	first, err := store.EnsureDerivationRun(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Duplicate {
		t.Fatal("first derivation run should not be duplicate")
	}
	if first.ID == "" {
		t.Fatal("expected derivation run id")
	}

	second, err := store.EnsureDerivationRun(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Duplicate {
		t.Fatal("expected duplicate derivation run")
	}
	if second.ID != first.ID {
		t.Fatalf("expected duplicate to resolve to same id, got %q and %q", first.ID, second.ID)
	}
	if second.CreatedAtEpoch != first.CreatedAtEpoch {
		t.Fatalf("expected duplicate to preserve created epoch, got %d and %d", first.CreatedAtEpoch, second.CreatedAtEpoch)
	}
}

func TestEnsureDerivationRunIdempotencyIncludesModelNotEndpoint(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	base := DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "session-model-id",
		Provider:          "local-qwen",
		Model:             "qwen-14b",
		EndpointID:        "hl3",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:model-id",
		WindowStartEpoch:  100,
		WindowEndEpoch:    200,
		Status:            "completed",
		CreatedAtEpoch:    300,
	}
	first, err := store.EnsureDerivationRun(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	sameModelDifferentEndpoint := base
	sameModelDifferentEndpoint.EndpointID = "hl5"
	second, err := store.EnsureDerivationRun(context.Background(), sameModelDifferentEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Duplicate || second.ID != first.ID {
		t.Fatalf("expected endpoint-only change to resolve as duplicate, first=%#v second=%#v", first, second)
	}

	differentModel := base
	differentModel.Model = "qwen-7b"
	third, err := store.EnsureDerivationRun(context.Background(), differentModel)
	if err != nil {
		t.Fatal(err)
	}
	if third.Duplicate {
		t.Fatal("different model should create an independent derivation run")
	}

	var count int
	if err := store.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM derivation_runs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected two derivation runs after model split, got %d", count)
	}
}

// Stale-running recovery (turn-ingest Phase 1, slice 3): a run abandoned at
// `running` past the age ceiling retries in place like a failed run; a live
// (young) running run still dedups; a just-recovered run is immediately
// protected from a second takeover by its fresh attempt stamp.
func TestEnsureDerivationRunStaleRunningRetries(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	nowEpoch := time.Now().Unix()
	stale := DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "session-stale",
		Provider:          "local-qwen",
		Model:             "qwen-14b",
		EndpointID:        "hl5",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v2",
		InputHash:         "sha256:stale-a",
		WindowStartEpoch:  100,
		WindowEndEpoch:    200,
		Status:            "running",
		// Older than the 2h default ceiling: the process that created it is gone.
		CreatedAtEpoch: nowEpoch - 3*60*60,
	}
	first, err := store.EnsureDerivationRun(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}

	retry, err := store.EnsureDerivationRun(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Duplicate {
		t.Fatal("stale running run must be retryable, not a duplicate")
	}
	if retry.ID != first.ID {
		t.Fatalf("stale retry must reuse the run id, got %q want %q", retry.ID, first.ID)
	}
	if retry.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", retry.RetryCount)
	}

	// The takeover stamped last_attempt_start_epoch, so an immediate re-ensure
	// sees a live attempt and dedups — the recovered run is not stolen twice.
	again, err := store.EnsureDerivationRun(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Duplicate || again.ID != first.ID {
		t.Fatalf("freshly recovered run must dedup, got %#v", again)
	}

	// A young running run (fresh attempt) is in flight, not stale: dedup.
	live := stale
	live.SessionID = "session-live"
	live.InputHash = "sha256:live-a"
	live.CreatedAtEpoch = nowEpoch
	if _, err := store.EnsureDerivationRun(ctx, live); err != nil {
		t.Fatal(err)
	}
	dedup, err := store.EnsureDerivationRun(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	if !dedup.Duplicate {
		t.Fatal("young running run must dedup, not retry")
	}

	// A caller-provided ceiling (2× execute-timeout) wins over the default:
	// with a 60s ceiling, a 10-minute-old running run is already stale.
	short := stale
	short.SessionID = "session-short"
	short.InputHash = "sha256:short-a"
	short.CreatedAtEpoch = nowEpoch - 10*60
	if _, err := store.EnsureDerivationRun(ctx, short); err != nil {
		t.Fatal(err)
	}
	short.StaleRunningRetrySeconds = 60
	shortRetry, err := store.EnsureDerivationRun(ctx, short)
	if err != nil {
		t.Fatal(err)
	}
	if shortRetry.Duplicate || shortRetry.RetryCount != 1 {
		t.Fatalf("caller ceiling must make a 10-minute running run retryable, got %#v", shortRetry)
	}

	// An old failed run retries once, and the retry's attempt stamp — written
	// atomically WITH the status flip — immediately protects it from a
	// stale-running steal despite the ancient created_at_epoch (orthogonal
	// review 2026-07-11, MAJOR).
	failed := stale
	failed.SessionID = "session-failed"
	failed.InputHash = "sha256:failed-a"
	failed.Status = "failed"
	failed.CreatedAtEpoch = nowEpoch - 3*60*60
	if _, err := store.EnsureDerivationRun(ctx, failed); err != nil {
		t.Fatal(err)
	}
	failedRetry, err := store.EnsureDerivationRun(ctx, failed)
	if err != nil {
		t.Fatal(err)
	}
	if failedRetry.Duplicate || failedRetry.RetryCount != 1 {
		t.Fatalf("old failed run must retry in place, got %#v", failedRetry)
	}
	protected, err := store.EnsureDerivationRun(ctx, failed)
	if err != nil {
		t.Fatal(err)
	}
	if !protected.Duplicate {
		t.Fatal("fresh failed-retry must dedup, not be stale-stolen, despite old created_at")
	}

	// Completed runs are untouched by the recovery path: still duplicates.
	done := stale
	done.SessionID = "session-done"
	done.InputHash = "sha256:done-a"
	done.Status = "completed"
	if _, err := store.EnsureDerivationRun(ctx, done); err != nil {
		t.Fatal(err)
	}
	doneDedup, err := store.EnsureDerivationRun(ctx, done)
	if err != nil {
		t.Fatal(err)
	}
	if !doneDedup.Duplicate {
		t.Fatal("completed run must remain a duplicate regardless of age")
	}
}

// -force-rerun (turn-ingest Phase 1, slice 4): Force mints a NEW run for a
// window that would otherwise dedup, records forced provenance, and leaves
// default idempotency untouched.
func TestEnsureDerivationRunForceMintsNewRun(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	in := DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "session-force",
		Provider:          "local-qwen",
		Model:             "qwen-14b",
		EndpointID:        "hl5",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v2",
		InputHash:         "sha256:force-a",
		WindowStartEpoch:  100,
		WindowEndEpoch:    200,
		Status:            "completed",
	}
	original, err := store.EnsureDerivationRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	forced := in
	forced.Status = "running"
	forced.Force = true
	minted, err := store.EnsureDerivationRun(ctx, forced)
	if err != nil {
		t.Fatal(err)
	}
	if minted.Duplicate {
		t.Fatal("forced run must not dedup")
	}
	if !minted.Forced {
		t.Fatal("forced run must report Forced")
	}
	if minted.ID == original.ID {
		t.Fatal("forced run must mint a new run id")
	}
	if minted.SupersededRunID != original.ID {
		t.Fatalf("superseded run id = %q, want %q", minted.SupersededRunID, original.ID)
	}

	rec, err := store.GetDerivationRun(ctx, minted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.InputHash, "sha256:force-a#forced-") {
		t.Fatalf("forced input_hash must carry the visible salt, got %q", rec.InputHash)
	}
	var meta map[string]any
	if err := json.Unmarshal(rec.Metadata, &meta); err != nil {
		t.Fatalf("forced run meta_json: %v", err)
	}
	if meta["forced"] != true || meta["canonical_input_hash"] != "sha256:force-a" || meta["superseded_run_id"] != original.ID {
		t.Fatalf("forced provenance missing from insert-time meta: %v", meta)
	}

	// A second force supersedes the latest run for the window (the first
	// forced rerun), not the original.
	second, err := store.EnsureDerivationRun(ctx, forced)
	if err != nil {
		t.Fatal(err)
	}
	if second.Duplicate || second.ID == minted.ID || second.SupersededRunID != minted.ID {
		t.Fatalf("second force must supersede the prior forced run: %#v", second)
	}

	// Default idempotency is untouched: an unforced ensure still dedups
	// against the ORIGINAL canonical run.
	dedup, err := store.EnsureDerivationRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !dedup.Duplicate || dedup.ID != original.ID {
		t.Fatalf("unforced ensure must still dedup to the original, got %#v", dedup)
	}

	// A force on a never-derived window is a plain new run with no superseded id.
	fresh := forced
	fresh.SessionID = "session-force-fresh"
	fresh.InputHash = "sha256:force-fresh"
	freshRes, err := store.EnsureDerivationRun(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !freshRes.Forced || freshRes.SupersededRunID != "" {
		t.Fatalf("forced run on fresh window: %#v", freshRes)
	}
}

func TestDerivationRunProviderDataBoundaryValidation(t *testing.T) {
	valid := DerivationRunInput{
		Project:           "memoire-test",
		Provider:          "openai",
		Model:             "gpt-4o-mini",
		DataBoundary:      "external",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:input-a",
		WindowStartEpoch:  100,
		WindowEndEpoch:    200,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("expected valid openai/external run: %v", err)
	}

	invalidExternal := valid
	invalidExternal.Provider = "rules"
	invalidExternal.DataBoundary = "external"
	if err := invalidExternal.Validate(); err == nil {
		t.Fatal("expected rules/external to be rejected")
	}

	invalidLocal := valid
	invalidLocal.Provider = "openai"
	invalidLocal.DataBoundary = "local"
	if err := invalidLocal.Validate(); err == nil {
		t.Fatal("expected openai/local to be rejected")
	}

	localQwen := valid
	localQwen.Provider = "local-qwen"
	localQwen.Model = "qwen-14b"
	localQwen.EndpointID = "hl3"
	localQwen.DataBoundary = "local"
	if err := localQwen.Validate(); err != nil {
		t.Fatalf("expected valid local-qwen/local run: %v", err)
	}

	missingEndpoint := localQwen
	missingEndpoint.EndpointID = ""
	if err := missingEndpoint.Validate(); err == nil {
		t.Fatal("expected local-qwen without endpoint_id to be rejected")
	}

	missingModel := valid
	missingModel.Model = ""
	if err := missingModel.Validate(); err == nil {
		t.Fatal("expected model-backed provider without model to be rejected")
	}
}

func TestListAndGetDerivationRunsForOperatorInspection(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	eventID := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "session-run-inspect",
		SourceAdapter:   "test",
		SourceEventID:   "event-run-inspect",
		EventType:       "note",
		OccurredAtEpoch: 123,
		Payload:         json.RawMessage(`{"message":"source"}`),
	})
	runID := ensureTestRun(t, store, DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "session-run-inspect",
		Provider:          "rules",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:run-inspect",
		WindowStartEpoch:  120,
		WindowEndEpoch:    130,
		Status:            "completed",
		CreatedAtEpoch:    140,
		CompletedAtEpoch:  150,
		Metadata:          json.RawMessage(`{"source_window":{"project":"memoire-test"}}`),
	})
	if _, err := store.InsertDerivedObservation(context.Background(), DerivedObservationInput{
		Project:         "memoire-test",
		SessionID:       "session-run-inspect",
		Kind:            "open_question",
		Text:            "Should inspection show proposed counts?",
		ReviewState:     "proposed",
		DerivationRunID: runID,
		ContentHash:     "sha256:run-inspect-proposed",
		SourceEventIDs:  []int64{eventID},
	}); err != nil {
		t.Fatal(err)
	}

	runs, err := store.ListDerivationRuns(context.Background(), DerivationRunFilter{
		Project:   "memoire-test",
		SessionID: "session-run-inspect",
		Provider:  "rules",
		Status:    "completed",
		Limit:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected one run, got %d", len(runs))
	}
	if runs[0].ID != runID || runs[0].ObservationCount != 1 || runs[0].ProposedObservationCount != 1 {
		t.Fatalf("unexpected derivation run summary: %#v", runs[0])
	}
	if len(runs[0].Metadata) == 0 {
		t.Fatalf("expected run metadata in summary: %#v", runs[0])
	}

	rec, err := store.GetDerivationRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID != runID || rec.CompletedAtEpoch != 150 || rec.ObservationCount != 1 || rec.ProposedObservationCount != 1 {
		t.Fatalf("unexpected derivation run detail: %#v", rec)
	}
}

func TestListDerivationRunsFiltersByModelAndEndpoint(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	hl3Run := ensureTestRun(t, store, DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "session-filter-model",
		Provider:          "local-qwen",
		Model:             "qwen-14b",
		EndpointID:        "hl3",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:qwen-14b",
		WindowStartEpoch:  100,
		WindowEndEpoch:    200,
		Status:            "completed",
	})
	_ = ensureTestRun(t, store, DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "session-filter-model",
		Provider:          "local-qwen",
		Model:             "qwen-7b",
		EndpointID:        "hl3",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:qwen-7b",
		WindowStartEpoch:  100,
		WindowEndEpoch:    200,
		Status:            "completed",
	})

	runs, err := store.ListDerivationRuns(context.Background(), DerivationRunFilter{
		Project:    "memoire-test",
		Provider:   "local-qwen",
		Model:      "qwen-14b",
		EndpointID: "hl3",
		Status:     "completed",
		Limit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected one filtered run, got %d", len(runs))
	}
	if runs[0].ID != hl3Run || runs[0].Model != "qwen-14b" || runs[0].EndpointID != "hl3" {
		t.Fatalf("unexpected filtered run: %#v", runs[0])
	}
}

func TestInsertDerivedObservationStoresReviewStateRunAndContentHash(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	eventID := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "session-obs",
		SourceAdapter:   "test",
		SourceEventID:   "event-obs-1",
		EventType:       "note",
		OccurredAtEpoch: 111,
		Payload:         json.RawMessage(`{"message":"source"}`),
	})
	runID := ensureTestRun(t, store, DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "session-obs",
		Provider:          "rules",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:obs-run",
		WindowStartEpoch:  100,
		WindowEndEpoch:    120,
		Status:            "completed",
	})

	confidence := 0.9
	result, err := store.InsertDerivedObservation(context.Background(), DerivedObservationInput{
		Project:         "memoire-test",
		SessionID:       "session-obs",
		Kind:            "decision",
		Text:            "Use additive derivation metadata",
		PrincipalID:     "tester",
		Confidence:      &confidence,
		ReviewState:     "accepted",
		DerivationRunID: runID,
		ContentHash:     "sha256:content-a",
		SourceEventIDs:  []int64{eventID},
		Metadata:        json.RawMessage(`{"source":"test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ReviewState != "accepted" {
		t.Fatalf("expected accepted review state, got %q", result.ReviewState)
	}

	var reviewState, derivationRunID, contentHash string
	var occurredAt int64
	err = store.db.QueryRowContext(context.Background(), `SELECT review_state, derivation_run_id, content_hash, occurred_at_epoch
		FROM observations WHERE id = ?`, result.ID).Scan(&reviewState, &derivationRunID, &contentHash, &occurredAt)
	if err != nil {
		t.Fatal(err)
	}
	if reviewState != "accepted" || derivationRunID != runID || contentHash != "sha256:content-a" {
		t.Fatalf("unexpected observation fields: state=%q run=%q hash=%q", reviewState, derivationRunID, contentHash)
	}
	if occurredAt != 111 {
		t.Fatalf("expected occurred_at_epoch to default from source events, got %d", occurredAt)
	}

	var sourceLinks int
	err = store.db.QueryRowContext(context.Background(), `SELECT count(*) FROM observation_source_events WHERE observation_id = ?`, result.ID).Scan(&sourceLinks)
	if err != nil {
		t.Fatal(err)
	}
	if sourceLinks != 1 {
		t.Fatalf("expected one source link, got %d", sourceLinks)
	}
}

func TestInsertDerivedObservationDowngradesOverlappingAcceptedDuplicate(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	firstEventID := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "session-dup",
		SourceAdapter:   "test",
		SourceEventID:   "event-dup-1",
		EventType:       "note",
		OccurredAtEpoch: 200,
		Payload:         json.RawMessage(`{"message":"source one"}`),
	})
	secondEventID := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "session-dup",
		SourceAdapter:   "test",
		SourceEventID:   "event-dup-2",
		EventType:       "note",
		OccurredAtEpoch: 201,
		Payload:         json.RawMessage(`{"message":"source two"}`),
	})
	runID := ensureTestRun(t, store, DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "session-dup",
		Provider:          "rules",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:dup-run",
		WindowStartEpoch:  190,
		WindowEndEpoch:    210,
		Status:            "completed",
	})

	first, err := store.InsertDerivedObservation(context.Background(), DerivedObservationInput{
		Project:         "memoire-test",
		SessionID:       "session-dup",
		Kind:            "task",
		Text:            "Follow up on duplicate suppression",
		ReviewState:     "accepted",
		DerivationRunID: runID,
		ContentHash:     "sha256:dup-hash",
		SourceEventIDs:  []int64{firstEventID, secondEventID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.DowngradedFromAccepted {
		t.Fatal("first observation should remain accepted")
	}

	second, err := store.InsertDerivedObservation(context.Background(), DerivedObservationInput{
		Project:         "memoire-test",
		SessionID:       "session-dup",
		Kind:            "task",
		Text:            "Follow up on duplicate suppression",
		ReviewState:     "accepted",
		DerivationRunID: runID,
		ContentHash:     "sha256:dup-hash",
		SourceEventIDs:  []int64{secondEventID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.DowngradedFromAccepted {
		t.Fatal("expected overlapping accepted duplicate to be downgraded")
	}
	if second.ReviewState != "proposed" {
		t.Fatalf("expected downgraded review state to be proposed, got %q", second.ReviewState)
	}

	var acceptedCount int
	err = store.db.QueryRowContext(context.Background(), `SELECT count(*) FROM observations
		WHERE project = 'memoire-test' AND kind = 'task' AND content_hash = 'sha256:dup-hash' AND review_state = 'accepted'`).Scan(&acceptedCount)
	if err != nil {
		t.Fatal(err)
	}
	if acceptedCount != 1 {
		t.Fatalf("expected one accepted observation after overlap check, got %d", acceptedCount)
	}
}

func TestDefaultReviewStateForModelDerivedObservations(t *testing.T) {
	if got := DefaultReviewState(true); got != "proposed" {
		t.Fatalf("expected model-derived default to be proposed, got %q", got)
	}
	if got := DefaultReviewState(false); got != "accepted" {
		t.Fatalf("expected non-model-derived default to be accepted, got %q", got)
	}
}

func TestInsertDerivedObservationRequiresExistingRun(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	eventID := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "session-missing-run",
		SourceAdapter:   "test",
		SourceEventID:   "event-missing-run",
		EventType:       "note",
		OccurredAtEpoch: 321,
		Payload:         json.RawMessage(`{"message":"source"}`),
	})

	_, err = store.InsertDerivedObservation(context.Background(), DerivedObservationInput{
		Project:         "memoire-test",
		SessionID:       "session-missing-run",
		Kind:            "fact",
		Text:            "Store should reject unknown derivation runs",
		DerivationRunID: "drv_missing",
		ContentHash:     "sha256:missing-run",
		SourceEventIDs:  []int64{eventID},
	})
	if err == nil {
		t.Fatal("expected missing derivation run error")
	}
	if err.Error() != `derivation run "drv_missing" not found` {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInsertDerivedObservationRejectsUnknownKind(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	eventID := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "session-unknown-kind",
		SourceAdapter:   "test",
		SourceEventID:   "event-unknown-kind",
		EventType:       "note",
		OccurredAtEpoch: 444,
		Payload:         json.RawMessage(`{"message":"source"}`),
	})
	runID := ensureTestRun(t, store, DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "session-unknown-kind",
		Provider:          "local-qwen",
		Model:             "qwen2.5-7b",
		EndpointID:        "hl3",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:unknown-kind-run",
		WindowStartEpoch:  400,
		WindowEndEpoch:    500,
		Status:            "completed",
	})

	// Defense in depth: "unknown" is the audit channel, never a storable kind.
	// Any direct caller of InsertDerivedObservation must be rejected by the
	// validator (the schema intentionally has no kind CHECK constraint).
	_, err = store.InsertDerivedObservation(context.Background(), DerivedObservationInput{
		Project:         "memoire-test",
		SessionID:       "session-unknown-kind",
		Kind:            "unknown",
		Text:            "should never be stored",
		DerivationRunID: runID,
		ContentHash:     "sha256:unknown-kind",
		SourceEventIDs:  []int64{eventID},
	})
	if err == nil {
		t.Fatal("expected rejection for kind=\"unknown\"")
	}
	if !strings.Contains(err.Error(), "unknown") || !strings.Contains(err.Error(), "audit channel") {
		t.Fatalf("unexpected error: %v", err)
	}

	// A storable kind on the same run still works (the guardrail is kind-specific).
	if _, err := store.InsertDerivedObservation(context.Background(), DerivedObservationInput{
		Project:         "memoire-test",
		SessionID:       "session-unknown-kind",
		Kind:            "gotcha",
		Text:            "expected A, got costly B",
		DerivationRunID: runID,
		ContentHash:     "sha256:gotcha-ok",
		SourceEventIDs:  []int64{eventID},
	}); err != nil {
		t.Fatalf("gotcha must be storable (no kind CHECK constraint): %v", err)
	}
}

func TestObservationReadVisibilityUsesReviewAwareDefaults(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	eventID := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "session-review-read",
		SourceAdapter:   "test",
		SourceEventID:   "event-review-read",
		EventType:       "note",
		OccurredAtEpoch: 400,
		Payload:         json.RawMessage(`{"message":"raw visibility needle"}`),
		Text:            "raw visibility needle",
	})
	rawID := rawObservationIDForEvent(t, store, eventID)
	runID := ensureTestRun(t, store, DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "session-review-read",
		Provider:          "manual",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:review-read",
		WindowStartEpoch:  390,
		WindowEndEpoch:    410,
		Status:            "completed",
	})

	acceptedID := insertTestDerivedObservation(t, store, runID, eventID, "session-review-read", "decision", "accepted decision needle", "accepted", "sha256:accepted-decision")
	acceptedQuestionID := insertTestDerivedObservation(t, store, runID, eventID, "session-review-read", "open_question", "accepted open question needle", "accepted", "sha256:accepted-open-question")
	proposedTaskID := insertTestDerivedObservation(t, store, runID, eventID, "session-review-read", "task", "proposed task needle", "proposed", "sha256:proposed-task")
	proposedQuestionID := insertTestDerivedObservation(t, store, runID, eventID, "session-review-read", "open_question", "proposed open question needle", "proposed", "sha256:proposed-open-question")
	rejectedID := insertTestDerivedObservation(t, store, runID, eventID, "session-review-read", "fact", "rejected fact needle", "proposed", "sha256:rejected-fact")
	if _, err := store.TransitionObservationReview(context.Background(), rejectedID, "rejected", "not durable"); err != nil {
		t.Fatal(err)
	}
	supersededID := insertTestDerivedObservation(t, store, runID, eventID, "session-review-read", "constraint", "superseded constraint needle", "accepted", "sha256:superseded-constraint")
	if _, err := store.TransitionObservationReview(context.Background(), supersededID, "superseded", "replaced"); err != nil {
		t.Fatal(err)
	}
	softDeletedID := insertTestDerivedObservation(t, store, runID, eventID, "session-review-read", "task", "soft deleted task needle", "accepted", "sha256:soft-deleted-task")
	if _, err := store.TransitionObservationReview(context.Background(), softDeletedID, "soft_deleted", "hidden"); err != nil {
		t.Fatal(err)
	}

	defaultList, _, err := store.ListObservations(context.Background(), ObservationFilter{Project: "memoire-test", SessionID: "session-review-read", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	assertObservationIDs(t, defaultList, []int64{rawID, acceptedID, acceptedQuestionID}, []int64{proposedTaskID, proposedQuestionID, rejectedID, supersededID, softDeletedID})

	questionList, _, err := store.ListObservations(context.Background(), ObservationFilter{
		Project:   "memoire-test",
		SessionID: "session-review-read",
		Kind:      "open_question",
		Limit:     20,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertObservationIDs(t, questionList, []int64{acceptedQuestionID, proposedQuestionID}, []int64{rawID, acceptedID, proposedTaskID})

	rejectedList, _, err := store.ListObservations(context.Background(), ObservationFilter{
		Project:     "memoire-test",
		SessionID:   "session-review-read",
		ReviewState: "rejected",
		Limit:       20,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertObservationIDs(t, rejectedList, []int64{rejectedID}, []int64{acceptedID, proposedTaskID})

	softDeletedList, _, err := store.ListObservations(context.Background(), ObservationFilter{
		Project:     "memoire-test",
		SessionID:   "session-review-read",
		ReviewState: "soft_deleted",
		Limit:       20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(softDeletedList) != 1 || softDeletedList[0].ID != softDeletedID || softDeletedList[0].DeletedAtEpoch == 0 {
		t.Fatalf("expected explicit soft_deleted filter to expose hidden row, got %#v", softDeletedList)
	}

	if _, err := store.GetObservation(context.Background(), proposedQuestionID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected hidden proposed open question to be absent from default get path, got %v", err)
	}

	if hits, err := store.Search(context.Background(), "accepted decision needle", "memoire-test", 10); err != nil {
		t.Fatal(err)
	} else {
		assertSearchHitIDs(t, hits, []int64{acceptedID}, []int64{proposedTaskID, proposedQuestionID, rejectedID, supersededID, softDeletedID})
	}

	if hits, err := store.Search(context.Background(), "proposed open question needle", "memoire-test", 10); err != nil {
		t.Fatal(err)
	} else {
		assertSearchHitIDs(t, hits, []int64{acceptedQuestionID}, []int64{proposedQuestionID})
	}

	if hits, err := store.SearchWithFilter(context.Background(), "proposed open question needle", ObservationFilter{
		Project:   "memoire-test",
		SessionID: "session-review-read",
		Kind:      "open_question",
		Limit:     10,
	}); err != nil {
		t.Fatal(err)
	} else {
		assertSearchHitIDs(t, hits, []int64{acceptedQuestionID, proposedQuestionID}, []int64{proposedTaskID, rejectedID, supersededID, softDeletedID})
	}

	timeline, err := store.TimelineWithFilter(context.Background(), ObservationFilter{
		Project:   "memoire-test",
		SessionID: "session-review-read",
		Limit:     20,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSearchHitIDs(t, timeline, []int64{rawID, acceptedID, acceptedQuestionID}, []int64{proposedTaskID, proposedQuestionID, rejectedID, supersededID, softDeletedID})

	questionTimeline, err := store.TimelineWithFilter(context.Background(), ObservationFilter{
		Project:   "memoire-test",
		SessionID: "session-review-read",
		Kind:      "open_question",
		Limit:     20,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSearchHitIDs(t, questionTimeline, []int64{acceptedQuestionID, proposedQuestionID}, []int64{acceptedID, proposedTaskID})
}

func TestTransitionObservationReviewMatrixAndLog(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	eventID := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "session-review-transition",
		SourceAdapter:   "test",
		SourceEventID:   "event-review-transition",
		EventType:       "note",
		OccurredAtEpoch: 500,
		Payload:         json.RawMessage(`{"message":"review transition source"}`),
	})
	rawID := rawObservationIDForEvent(t, store, eventID)
	runID := ensureTestRun(t, store, DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "session-review-transition",
		Provider:          "manual",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:review-transition",
		WindowStartEpoch:  490,
		WindowEndEpoch:    510,
		Status:            "completed",
	})
	reviewCtx := authctx.With(context.Background(), authctx.Context{
		PrincipalID:  "reviewer",
		CredentialID: "cred-review",
		RequestID:    "req-review",
		Scopes:       []string{"admin"},
	})

	proposedAcceptedID := insertTestDerivedObservation(t, store, runID, eventID, "session-review-transition", "decision", "transition accept needle", "proposed", "sha256:transition-accept")
	acceptedRec, err := store.TransitionObservationReview(reviewCtx, proposedAcceptedID, "accepted", "looks durable")
	if err != nil {
		t.Fatal(err)
	}
	if acceptedRec.ReviewState != "accepted" {
		t.Fatalf("expected accepted transition result, got %#v", acceptedRec)
	}
	assertReviewLogEntry(t, store, proposedAcceptedID, "proposed", "accepted", "cred-review", "looks durable")

	proposedRejectedID := insertTestDerivedObservation(t, store, runID, eventID, "session-review-transition", "task", "transition reject needle", "proposed", "sha256:transition-reject")
	rejectedRec, err := store.TransitionObservationReview(reviewCtx, proposedRejectedID, "rejected", "not durable")
	if err != nil {
		t.Fatal(err)
	}
	if rejectedRec.ReviewState != "rejected" {
		t.Fatalf("expected rejected transition result, got %#v", rejectedRec)
	}

	acceptedSupersededID := insertTestDerivedObservation(t, store, runID, eventID, "session-review-transition", "fact", "transition supersede needle", "accepted", "sha256:transition-supersede")
	supersededRec, err := store.TransitionObservationReview(reviewCtx, acceptedSupersededID, "superseded", "stale")
	if err != nil {
		t.Fatal(err)
	}
	if supersededRec.ReviewState != "superseded" {
		t.Fatalf("expected superseded transition result, got %#v", supersededRec)
	}

	acceptedSoftDeletedID := insertTestDerivedObservation(t, store, runID, eventID, "session-review-transition", "constraint", "transition soft delete needle", "accepted", "sha256:transition-soft-delete")
	softDeletedRec, err := store.TransitionObservationReview(reviewCtx, acceptedSoftDeletedID, "soft_deleted", "hide")
	if err != nil {
		t.Fatal(err)
	}
	if softDeletedRec.ReviewState != "soft_deleted" || softDeletedRec.DeletedAtEpoch == 0 {
		t.Fatalf("expected soft-deleted transition result, got %#v", softDeletedRec)
	}
	if _, err := store.GetObservation(context.Background(), acceptedSoftDeletedID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected soft-deleted observation to be hidden from default read, got %v", err)
	}

	if count, err := store.AuditCount(context.Background(), "observation.review"); err != nil || count != 4 {
		t.Fatalf("observation.review audit count = %d, err=%v", count, err)
	}

	cases := []struct {
		name    string
		id      int64
		toState string
		wantErr string
	}{
		{name: "raw locked", id: rawID, toState: "superseded", wantErr: "raw observations are locked at accepted"},
		{name: "rejected cannot accept", id: proposedRejectedID, toState: "accepted", wantErr: "unsupported review transition rejected -> accepted"},
		{name: "superseded cannot accept", id: acceptedSupersededID, toState: "accepted", wantErr: "unsupported review transition superseded -> accepted"},
		{name: "soft_deleted cannot accept", id: acceptedSoftDeletedID, toState: "accepted", wantErr: "unsupported review transition soft_deleted -> accepted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.TransitionObservationReview(reviewCtx, tc.id, tc.toState, "nope")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestListObservationReviewLogFiltersAndLimits(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	eventID := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "session-review-log-list",
		SourceAdapter:   "test",
		SourceEventID:   "event-review-log-list",
		EventType:       "note",
		OccurredAtEpoch: 700,
		Payload:         json.RawMessage(`{"message":"review log list source"}`),
	})
	runID := ensureTestRun(t, store, DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "session-review-log-list",
		Provider:          "manual",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:review-log-list",
		WindowStartEpoch:  690,
		WindowEndEpoch:    710,
		Status:            "completed",
	})
	reviewCtx := authctx.With(context.Background(), authctx.Context{
		PrincipalID:  "reviewer",
		CredentialID: "cred-review",
		RequestID:    "req-review-log-list",
		Scopes:       []string{"admin"},
	})

	acceptedID := insertTestDerivedObservation(t, store, runID, eventID, "session-review-log-list", "decision", "review log accept needle", "proposed", "sha256:review-log-accept")
	if _, err := store.TransitionObservationReview(reviewCtx, acceptedID, "accepted", "durable"); err != nil {
		t.Fatal(err)
	}

	rejectedID := insertTestDerivedObservation(t, store, runID, eventID, "session-review-log-list", "task", "review log reject needle", "proposed", "sha256:review-log-reject")
	if _, err := store.TransitionObservationReview(reviewCtx, rejectedID, "rejected", "not durable"); err != nil {
		t.Fatal(err)
	}

	supersededID := insertTestDerivedObservation(t, store, runID, eventID, "session-review-log-list", "fact", "review log supersede needle", "accepted", "sha256:review-log-supersede")
	if _, err := store.TransitionObservationReview(reviewCtx, supersededID, "superseded", "stale"); err != nil {
		t.Fatal(err)
	}

	limited, err := store.ListObservationReviewLog(context.Background(), ObservationReviewLogFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 {
		t.Fatalf("expected 2 review log rows, got %d", len(limited))
	}
	if limited[0].ObservationID != supersededID || limited[0].ToState != "superseded" || limited[0].Actor != "cred-review" || limited[0].ReasonText != "stale" {
		t.Fatalf("unexpected first review log row: %#v", limited[0])
	}
	if limited[1].ObservationID != rejectedID || limited[1].ToState != "rejected" || limited[1].ReasonText != "not durable" {
		t.Fatalf("unexpected second review log row: %#v", limited[1])
	}

	filtered, err := store.ListObservationReviewLog(context.Background(), ObservationReviewLogFilter{
		ObservationID: acceptedID,
		Limit:         10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 {
		t.Fatalf("expected 1 filtered review log row, got %d", len(filtered))
	}
	if filtered[0].ObservationID != acceptedID || filtered[0].FromState != "proposed" || filtered[0].ToState != "accepted" || filtered[0].Actor != "cred-review" || filtered[0].ReasonText != "durable" || filtered[0].OccurredAtEpoch == 0 {
		t.Fatalf("unexpected filtered review log row: %#v", filtered[0])
	}
}

func TestProposedListSurfacesProvenanceAndTransitionIsIdempotencySafe(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	eventA := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "session-provenance",
		SourceAdapter:   "test",
		SourceEventID:   "event-provenance-a",
		EventType:       "note",
		OccurredAtEpoch: 800,
		Payload:         json.RawMessage(`{"message":"provenance source a"}`),
	})
	eventB := ingestTestEvent(t, store, EventInput{
		Project:         "memoire-test",
		SessionID:       "session-provenance",
		SourceAdapter:   "test",
		SourceEventID:   "event-provenance-b",
		EventType:       "note",
		OccurredAtEpoch: 805,
		Payload:         json.RawMessage(`{"message":"provenance source b"}`),
	})
	runID := ensureTestRun(t, store, DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "session-provenance",
		Provider:          "local-qwen",
		Model:             "qwen-14b",
		EndpointID:        "hl3",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:provenance",
		WindowStartEpoch:  790,
		WindowEndEpoch:    810,
		Status:            "completed",
	})
	confidence := 0.42
	proposed, err := store.InsertDerivedObservation(context.Background(), DerivedObservationInput{
		Project:         "memoire-test",
		SessionID:       "session-provenance",
		Kind:            "task",
		Text:            "provenance proposed needle",
		ReviewState:     "proposed",
		DerivationRunID: runID,
		ContentHash:     "sha256:provenance-task",
		Confidence:      &confidence,
		SourceEventIDs:  []int64{eventA, eventB},
	})
	if err != nil {
		t.Fatal(err)
	}

	// (1) The proposed-queue listing surfaces full triage provenance:
	// derivation_run_id, source_event_ids, kind, confidence.
	list, _, err := store.ListObservations(context.Background(), ObservationFilter{
		Project:     "memoire-test",
		SessionID:   "session-provenance",
		ReviewState: "proposed",
		Limit:       20,
	})
	if err != nil {
		t.Fatal(err)
	}
	var rec ObservationRecord
	for _, r := range list {
		if r.ID == proposed.ID {
			rec = r
		}
	}
	if rec.ID == 0 {
		t.Fatalf("expected proposed observation %d in list, got %#v", proposed.ID, list)
	}
	if rec.DerivationRunID != runID {
		t.Fatalf("expected derivation_run_id %q, got %q", runID, rec.DerivationRunID)
	}
	if rec.Kind != "task" || rec.Confidence != confidence {
		t.Fatalf("expected kind=task confidence=%v, got kind=%q confidence=%v", confidence, rec.Kind, rec.Confidence)
	}
	if len(rec.SourceEventIDs) != 2 || rec.SourceEventIDs[0] != eventA || rec.SourceEventIDs[1] != eventB {
		t.Fatalf("expected source_event_ids [%d %d], got %#v", eventA, eventB, rec.SourceEventIDs)
	}

	// (2) Accept is idempotency-safe: the first proposed->accepted wins; a
	// second attempt from the now-accepted state is rejected as illegal,
	// not silently re-applied (the SQL guard means a concurrent transition
	// affects 0 rows).
	accepted, err := store.TransitionObservationReview(context.Background(), proposed.ID, "accepted", "durable")
	if err != nil {
		t.Fatal(err)
	}
	if accepted.ReviewState != "accepted" {
		t.Fatalf("expected accepted, got %q", accepted.ReviewState)
	}
	if len(accepted.SourceEventIDs) != 2 {
		t.Fatalf("expected transition result to carry provenance, got %#v", accepted.SourceEventIDs)
	}

	// Once accepted, GetObservation (default read path) surfaces the same provenance.
	got, err := store.GetObservation(context.Background(), proposed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.SourceEventIDs) != 2 || got.SourceEventIDs[0] != eventA || got.SourceEventIDs[1] != eventB {
		t.Fatalf("expected GetObservation to surface 2 ordered source_event_ids, got %#v", got.SourceEventIDs)
	}
	if _, err := store.TransitionObservationReview(context.Background(), proposed.ID, "accepted", "again"); err == nil ||
		!strings.Contains(err.Error(), "unsupported review transition accepted -> accepted") {
		t.Fatalf("expected illegal re-accept to be rejected, got %v", err)
	}
	// Rejecting an already-accepted (not proposed) observation is illegal.
	if _, err := store.TransitionObservationReview(context.Background(), proposed.ID, "rejected", "too late"); err == nil ||
		!strings.Contains(err.Error(), "unsupported review transition accepted -> rejected") {
		t.Fatalf("expected reject-after-accept to be rejected, got %v", err)
	}
	// Exactly one review-log + audit row for the single successful transition.
	if count, err := store.AuditCount(context.Background(), "observation.review"); err != nil || count != 1 {
		t.Fatalf("expected 1 observation.review audit row, got %d err=%v", count, err)
	}
}

func ingestTestEvent(t *testing.T, store *Store, in EventInput) int64 {
	t.Helper()
	res, err := store.Ingest(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return res.ID
}

func ensureTestRun(t *testing.T, store *Store, in DerivationRunInput) string {
	t.Helper()
	res, err := store.EnsureDerivationRun(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return res.ID
}

func rawObservationIDForEvent(t *testing.T, store *Store, eventID int64) int64 {
	t.Helper()
	var observationID int64
	if err := store.db.QueryRowContext(context.Background(), `SELECT id FROM observations WHERE event_id = ? AND kind = 'raw'`, eventID).Scan(&observationID); err != nil {
		t.Fatal(err)
	}
	return observationID
}

func insertTestDerivedObservation(t *testing.T, store *Store, runID string, eventID int64, sessionID, kind, text, reviewState, contentHash string) int64 {
	t.Helper()
	res, err := store.InsertDerivedObservation(context.Background(), DerivedObservationInput{
		Project:         "memoire-test",
		SessionID:       sessionID,
		Kind:            kind,
		Text:            text,
		ReviewState:     reviewState,
		DerivationRunID: runID,
		ContentHash:     contentHash,
		SourceEventIDs:  []int64{eventID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.ID
}

func assertObservationIDs(t *testing.T, recs []ObservationRecord, wantPresent, wantAbsent []int64) {
	t.Helper()
	seen := make(map[int64]ObservationRecord, len(recs))
	for _, rec := range recs {
		seen[rec.ID] = rec
	}
	for _, id := range wantPresent {
		if _, ok := seen[id]; !ok {
			t.Fatalf("expected observation %d in %#v", id, recs)
		}
	}
	for _, id := range wantAbsent {
		if _, ok := seen[id]; ok {
			t.Fatalf("did not expect observation %d in %#v", id, recs)
		}
	}
}

func assertSearchHitIDs(t *testing.T, hits []SearchHit, wantPresent, wantAbsent []int64) {
	t.Helper()
	seen := make(map[int64]SearchHit, len(hits))
	for _, hit := range hits {
		seen[hit.ID] = hit
	}
	for _, id := range wantPresent {
		if _, ok := seen[id]; !ok {
			t.Fatalf("expected hit %d in %#v", id, hits)
		}
	}
	for _, id := range wantAbsent {
		if _, ok := seen[id]; ok {
			t.Fatalf("did not expect hit %d in %#v", id, hits)
		}
	}
}

func assertReviewLogEntry(t *testing.T, store *Store, observationID int64, fromState, toState, actor, reasonText string) {
	t.Helper()
	var gotFromState, gotToState, gotActor, gotReasonText string
	var occurredAt int64
	if err := store.db.QueryRowContext(context.Background(), `SELECT from_state, to_state, actor, COALESCE(reason_text, ''), occurred_at_epoch
		FROM observation_review_log
		WHERE observation_id = ?
		ORDER BY id DESC
		LIMIT 1`, observationID).Scan(&gotFromState, &gotToState, &gotActor, &gotReasonText, &occurredAt); err != nil {
		t.Fatal(err)
	}
	if gotFromState != fromState || gotToState != toState || gotActor != actor || gotReasonText != reasonText || occurredAt == 0 {
		t.Fatalf("unexpected review log row: from=%q to=%q actor=%q reason=%q occurred_at=%d", gotFromState, gotToState, gotActor, gotReasonText, occurredAt)
	}
}
