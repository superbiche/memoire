// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"path/filepath"
	"testing"
)

// TestRetryClaimRefreshesAuditProvenance pins BYOK review r2 N1: the run
// idempotency identity deliberately excludes data_boundary, endpoint_id,
// and prompt_version, so an in-place retry claim of a failed run can
// execute under a DIFFERENT boundary/endpoint/prompt than the row records.
// The claim must refresh those audit columns to the attempt that actually
// runs — otherwise `derivation_runs.data_boundary='local'` can sit on a
// run whose provider call went external.
func TestRetryClaimRefreshesAuditProvenance(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	base := DerivationRunInput{
		Project:           "p",
		SessionID:         "s",
		Provider:          "byok",
		Model:             "m",
		DerivationVersion: "v2",
		InputHash:         "hash-1",
		WindowStartEpoch:  100,
		WindowEndEpoch:    200,
		Status:            "running",
	}

	first := base
	first.DataBoundary = "local"
	first.EndpointID = "local-a"
	first.PromptVersion = "prompt-v1"
	res, err := store.EnsureDerivationRun(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if res.Duplicate {
		t.Fatal("first ensure must mint the row")
	}
	if err := store.UpdateDerivationRunStatus(ctx, DerivationRunStatusUpdate{
		ID: res.ID, Status: "failed", CompletedAtEpoch: 150,
	}); err != nil {
		t.Fatal(err)
	}

	// The retry runs under a different declared boundary, endpoint label,
	// and prompt version — same idempotency tuple, so it claims in place.
	retry := base
	retry.DataBoundary = "external"
	retry.EndpointID = "external-b"
	retry.PromptVersion = "prompt-v2"
	res2, err := store.EnsureDerivationRun(ctx, retry)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Duplicate || res2.ID != res.ID {
		t.Fatalf("retry must claim the existing row in place, got dup=%t id=%s (want %s)", res2.Duplicate, res2.ID, res.ID)
	}

	rec, err := store.GetDerivationRun(ctx, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.DataBoundary != "external" || rec.EndpointID != "external-b" || rec.PromptVersion != "prompt-v2" {
		t.Fatalf("retry claim must refresh audit provenance to the attempt that runs, got boundary=%q endpoint=%q prompt=%q",
			rec.DataBoundary, rec.EndpointID, rec.PromptVersion)
	}
}
