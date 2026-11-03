// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// The 2026-08-27 deadlock oracle: two schema appliers must never execute DDL
// against the same Postgres database at the same time. These legs hold the
// applier lock from outside and assert the store's own migrate QUEUES behind
// it — a store that walks straight through is the deadlocking store. Both
// legs mutate process-global state (the lock is per-database, leg 2 shrinks
// the package timeout), so neither may run parallel.

// holdPGMigrateLock takes pgMigrateLockKey on a session-pinned holder pool
// and returns the releaser. SetMaxOpenConns(1) is load-bearing: advisory
// locks are session-scoped, so a pooled lock/unlock pair could land on two
// different sessions and release nothing. The releaser unlocks explicitly
// AND closes the pool (session end releases too) — release must be
// deterministic, not eventually-consistent.
func holdPGMigrateLock(t *testing.T, dsn string) func() {
	t.Helper()
	holder, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open lock holder: %v", err)
	}
	holder.SetMaxOpenConns(1)
	if _, err := holder.Exec(`SELECT pg_advisory_lock($1)`, pgMigrateLockKey); err != nil {
		_ = holder.Close()
		t.Fatalf("hold advisory lock: %v", err)
	}
	var once bool
	release := func() {
		if once {
			return
		}
		once = true
		_, _ = holder.Exec(`SELECT pg_advisory_unlock($1)`, pgMigrateLockKey)
		_ = holder.Close()
	}
	t.Cleanup(release)
	return release
}

// Serialization leg: Open blocks while a peer holds the applier lock, and
// completes once the peer releases.
func TestPGMigrateLockSerializesOpen(t *testing.T) {
	dsn, _ := conformancePGURL(t)
	schemaDir := schemaDirForTest(t)
	release := holdPGMigrateLock(t, dsn)

	done := make(chan error, 1)
	go func() {
		store, err := Open(dsn, schemaDir)
		if store != nil {
			_ = store.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("Open completed while the applier advisory lock was held (migrate is not serialized; err=%v)", err)
	case <-time.After(500 * time.Millisecond):
	}

	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Open failed after the applier lock was released: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Open did not complete within 60s of the applier lock being released")
	}
}

// Timeout leg: a wedged peer must produce a LOUD failed open, not a hang.
// The error names the advisory lock so an operator reading a CronJob log
// knows what to look for in pg_locks.
func TestPGMigrateLockTimesOutLoudly(t *testing.T) {
	dsn, _ := conformancePGURL(t)
	schemaDir := schemaDirForTest(t)

	prev := pgMigrateLockTimeout
	pgMigrateLockTimeout = 750 * time.Millisecond
	defer func() { pgMigrateLockTimeout = prev }()

	holdPGMigrateLock(t, dsn)

	start := time.Now()
	store, err := Open(dsn, schemaDir)
	if err == nil {
		_ = store.Close()
		t.Fatal("Open succeeded while the applier advisory lock was held")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("Open took %s to fail; the bounded wait did not bound", elapsed)
	}
	if !strings.Contains(err.Error(), "advisory lock") {
		t.Errorf("timeout error does not name the advisory lock: %v", err)
	}
}
