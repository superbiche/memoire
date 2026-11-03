// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"
)

// pgMigrateLockKey is memoire's schema-applier advisory lock key. EVERY
// applier that runs schema-pg/ DDL against a Postgres store takes THIS key
// before touching the database — openPostgres (the store's own migrate) and
// pgshadow.Apply are the two that exist; a third must join them here rather
// than mint its own. The literal must NEVER change: two binaries built from
// different commits are exactly the concurrency this serializes, and a
// changed key silently un-serializes them.
//
// Why this exists (2026-08-27 incident): the nightly hl5 derive CronJob and
// the hourly derive CronJob both fire at :03; both open the PG store, and
// migrate re-executes 0001_baseline.sql on every open (version 1 has no
// applied-check). The two concurrent baseline DDL executions deadlocked —
// `apply 0001_baseline.sql: ERROR: deadlock detected (SQLSTATE 40P01)` — and
// the hl5 sweep died. inbox/2026-08-04-agentic-qm1-migrate-race-and-twin-
// enforcement.md item 1 predicted the class, including the transient
// `CREATE TABLE IF NOT EXISTS` failures on pg_type unique violations that
// concurrent DDL also produces. Serialization is the fix for all of it.
//
// Value: ASCII "memoire" followed by 0x01 (the applier-lock namespace), so
// the constant is recognizable in hex rather than an arbitrary integer.
// pg_locks shows it split into classid/objid decimals, not as this literal.
const pgMigrateLockKey int64 = 0x6D656D6F69726501

// pgMigrateLockTimeout bounds the wait for the applier lock. Package var,
// not const, so the test can shrink it; production never rewrites it.
//
// 300s rationale: a normal apply is milliseconds (every migration past
// version 1 is skipped by its applied-check, and the version-1 baseline is
// idempotent DDL). The longest LEGITIMATE holder is a D233-style rebuild
// migration against the multi-GB hub store. Past five minutes the peer is
// not slow, it is stuck — and a stuck peer must produce a FAILED tick, never
// a silently hanging one (L-family fail-loud doctrine). lock_timeout turns
// the wait into SQLSTATE 55P03, which surfaces here as a loud error naming
// the lock and the likely cause; the next tick retries. A dead peer is not
// starvable: Postgres releases session advisory locks when the session ends.
var pgMigrateLockTimeout = 300 * time.Second

// AcquirePGMigrateLock takes the schema-applier advisory lock on a dedicated
// session drawn from conn, and returns the release func. Advisory locks are
// SESSION-scoped, so acquire, hold and release all ride the one pinned
// connection — a pool-level Exec would put the lock on an arbitrary session
// and release on another.
//
// The returned release is idempotent-safe to call exactly once on every path
// (defer it, or call it explicitly before the error return); it unlocks and
// then DISCARDS the pinned session.
//
// Failure modes, all handled here:
//   - acquisition timeout (55P03): loud error, the open/apply fails, next
//     tick retries — never a hang;
//   - context cancellation during acquire: same path, session discarded,
//     error propagates to the caller;
//   - unlock failure at release: tolerated, because discarding the session
//     ends it and Postgres drops its advisory locks;
//   - error mid-migration: the caller's release still runs, so the lock is
//     never orphaned by a failed apply.
func AcquirePGMigrateLock(ctx context.Context, conn *sql.DB) (func(), error) {
	c, err := conn.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("schema-applier advisory lock: pin session: %w", err)
	}
	return AcquirePGMigrateLockOn(ctx, c)
}

// AcquirePGMigrateLockOn takes the schema-applier advisory lock on a
// session the CALLER pinned, instead of pinning its own like
// AcquirePGMigrateLock. It exists for appliers that must hold the lock
// AND run the protected DDL on the same session: reindex-bm25 derives
// its plan from the catalog read that happens under the lock (a pre-lock
// read would let a queued second invocation execute a stale plan against
// a catalog a concurrent run already finished — a20 = A, F2), so the DDL
// session IS the lock session. Same key, same 300s wait window, same
// release semantics: unlock, then discard the session (see
// AcquirePGMigrateLock for why a pinned session is never pooled back).
func AcquirePGMigrateLockOn(ctx context.Context, c *sql.Conn) (func(), error) {
	// discard, never c.Close() alone: database/sql RETURNS a Conn to the pool
	// on Close, it does not end the session. Pooling it back would (a) leave
	// lock_timeout set on a session that then serves ordinary store
	// queries, and (b) make the "session end releases the lock" backstop a
	// lie whenever pg_advisory_unlock failed. Marking the driver conn bad
	// makes the pool close it for real. One reconnect per applier run.
	discard := func() {
		_ = c.Raw(func(any) error { return driver.ErrBadConn })
		_ = c.Close()
	}
	// lock_timeout takes no placeholders; integer means milliseconds.
	if _, err := c.ExecContext(ctx, fmt.Sprintf("SET lock_timeout = %d", pgMigrateLockTimeout.Milliseconds())); err != nil {
		discard()
		return nil, fmt.Errorf("schema-applier advisory lock: set lock_timeout: %w", err)
	}
	if _, err := c.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, pgMigrateLockKey); err != nil {
		discard()
		return nil, fmt.Errorf("schema-applier advisory lock %d not acquired within %s: another schema applier (a concurrent store open or pgshadow apply) is holding it — a wedged peer, since a normal apply takes milliseconds; inspect pg_locks for this key: %w",
			pgMigrateLockKey, pgMigrateLockTimeout, err)
	}
	return func() {
		// Background, not ctx: a cancelled ctx must not skip the explicit
		// unlock. Failure is non-fatal — discard ends the session anyway.
		_, _ = c.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, pgMigrateLockKey)
		discard()
	}, nil
}
