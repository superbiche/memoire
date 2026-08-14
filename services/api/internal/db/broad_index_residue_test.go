// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"path/filepath"
	"testing"
)

// memoire:D142 — a DB where the round-1 broad 0008 shape was hand-applied
// (same index names, no proposed-only partial predicate, version-8 row
// pre-stamped) never self-heals: IF NOT EXISTS matches by name. The residue
// check is the loud inverse of INDEXED BY's absence failure.
func TestBroadReviewQueueIndexResidue(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	residue, err := store.BroadReviewQueueIndexResidue()
	if err != nil {
		t.Fatal(err)
	}
	if len(residue) != 0 {
		t.Fatalf("fresh schema must have no broad residue, got %v", residue)
	}

	// Recreate the exact hand-applied round-1 broad shape (commit bdce654).
	for _, stmt := range []string{
		`DROP INDEX idx_obs_review_queue`,
		`DROP INDEX idx_obs_review_queue_all`,
		`CREATE INDEX idx_obs_review_queue
		   ON observations(review_state, noise_candidate, occurred_at_epoch, id)
		   WHERE deleted_at_epoch IS NULL`,
		`CREATE INDEX idx_obs_review_queue_all
		   ON observations(review_state, occurred_at_epoch, id)
		   WHERE deleted_at_epoch IS NULL`,
	} {
		if _, err := store.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	residue, err = store.BroadReviewQueueIndexResidue()
	if err != nil {
		t.Fatal(err)
	}
	if len(residue) != 2 {
		t.Fatalf("broad shape must be reported for both indexes, got %v", residue)
	}
}
