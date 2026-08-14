// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Ping must answer through the READ pool only: SQLite's sole write
// connection is legitimately held for the duration of bulk transactions,
// and a readiness probe that queues behind it would shed capture traffic
// from a pod that is merely busy (review 2026-08-03 F2, reproduced with a
// held write transaction before the fix).
func TestPingUnaffectedByHeldWriteHandle(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "ping.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Occupy the single write connection with a valid, in-flight
	// transaction — normal bulk-write operation, not a failure.
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := store.Ping(ctx); err != nil {
		t.Fatalf("Ping failed behind a healthy write transaction: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Ping took %v behind a healthy write transaction — it is queuing on the write handle", elapsed)
	}
}
