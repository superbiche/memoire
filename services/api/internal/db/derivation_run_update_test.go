// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func newRunUpdateStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestUpdateDerivationRunStatus(t *testing.T) {
	store := newRunUpdateStore(t)
	ctx := context.Background()

	run, err := store.EnsureDerivationRun(ctx, DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "sess",
		Provider:          "local-qwen",
		Model:             "qwen2.5-7b",
		EndpointID:        "hl3",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:abc",
		WindowStartEpoch:  1000,
		WindowEndEpoch:    2000,
		Status:            "running",
	})
	if err != nil {
		t.Fatalf("ensure run: %v", err)
	}

	meta := json.RawMessage(`{"status":"completed","written_observation_count":2}`)
	if err := store.UpdateDerivationRunStatus(ctx, DerivationRunStatusUpdate{
		ID:               run.ID,
		Status:           "completed",
		CompletedAtEpoch: 2500,
		Metadata:         meta,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	rec, err := store.GetDerivationRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec.Status != "completed" {
		t.Fatalf("status not updated: %q", rec.Status)
	}
	if rec.CompletedAtEpoch != 2500 {
		t.Fatalf("completed_at not updated: %d", rec.CompletedAtEpoch)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Metadata, &got); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if got["written_observation_count"].(float64) != 2 {
		t.Fatalf("metadata not updated: %v", got)
	}
}

func TestUpdateDerivationRunStatusValidation(t *testing.T) {
	store := newRunUpdateStore(t)
	ctx := context.Background()

	if err := store.UpdateDerivationRunStatus(ctx, DerivationRunStatusUpdate{ID: "", Status: "completed"}); err == nil {
		t.Fatalf("expected error for empty id")
	}
	if err := store.UpdateDerivationRunStatus(ctx, DerivationRunStatusUpdate{ID: "x", Status: "bogus"}); err == nil {
		t.Fatalf("expected error for invalid status")
	}
	if err := store.UpdateDerivationRunStatus(ctx, DerivationRunStatusUpdate{ID: "missing", Status: "failed"}); err == nil {
		t.Fatalf("expected not-found error for missing run")
	}
}
