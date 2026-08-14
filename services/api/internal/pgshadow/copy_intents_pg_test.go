// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package pgshadow

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/mattn/go-sqlite3"

	memdb "memoire/services/api/internal/db"
)

// Non-empty COPY fixture for the R-10.1 tables (memoire:D165 round-3
// LOW-1): a derive intent row, a run with a snapshot watermark, and a
// manifest row must survive the SQLite->PG bulk copy value-exact. A later
// column-list regression in copyTables can no longer stay green against
// empty tables. Runs only with MEMOIRE_TEST_PG_URL (admin DSN with
// CREATEDB — the local pg-shadow container); creates and drops a
// throwaway database.
func TestCopyCarriesDeriveIntentTables(t *testing.T) {
	admin := os.Getenv("MEMOIRE_TEST_PG_URL")
	if admin == "" {
		t.Skip("MEMOIRE_TEST_PG_URL not set; postgres copy fixture skipped")
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("MEMOIRE_TEST_PG_URL: %v", err)
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	dbName := "memoire_copyint_" + hex.EncodeToString(suffix[:])
	adminConn, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminConn.Exec("CREATE DATABASE " + dbName); err != nil {
		_ = adminConn.Close()
		t.Fatalf("create fixture database (role needs CREATEDB): %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminConn.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
		_ = adminConn.Close()
	})
	u.Path = "/" + dbName
	pg, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })

	ctx := context.Background()
	if err := Apply(ctx, pg, "../../../../schema-pg"); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// SQLite source through the real migration chain, then seed the three
	// surfaces directly.
	litePath := filepath.Join(t.TempDir(), "memoire.db")
	store, err := memdb.Open(litePath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	lite, err := sql.Open("sqlite3", litePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lite.Close() })
	for _, stmt := range []string{
		`INSERT INTO events(id, idempotency_key, source_event_id, session_id, project, source_adapter, event_type, payload_json, occurred_at_epoch, received_at_epoch)
		 VALUES (7, 'ck1', 'ck1', 'cs1', 'copyproj', 'claude-code', 'note', '{}', 100, 100)`,
		`INSERT INTO derivation_runs(id, project, session_id, provider, data_boundary, derivation_version, input_hash, window_start_epoch, window_end_epoch, status, created_at_epoch, snapshot_max_event_id)
		 VALUES ('crun1', 'copyproj', 'cs1', 'manual', 'local', 'v1', 'ch1', 90, 110, 'completed', 100, 7)`,
		`INSERT INTO run_input_events(run_id, event_id) VALUES ('crun1', 7)`,
		`INSERT INTO derive_intents(project, session_id, first_notified_epoch, last_notified_epoch, notify_count, completed_at_epoch, completed_run_id, latency_seconds)
		 VALUES ('copyproj', 'cs1', 50, 60, 2, 120, 'crun1', 70)`,
	} {
		if _, err := lite.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	counts, err := Copy(ctx, pg, lite)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if counts["derive_intents"] != 1 || counts["run_input_events"] != 1 || counts["derivation_runs"] != 1 {
		t.Fatalf("copy counts: %+v", counts)
	}

	// Value-exact round trip for every new column.
	var first, last, nc, completed, latency int64
	var runRef string
	if err := pg.QueryRowContext(ctx, `SELECT first_notified_epoch, last_notified_epoch, notify_count,
		completed_at_epoch, completed_run_id, latency_seconds FROM derive_intents
		WHERE project = 'copyproj' AND session_id = 'cs1'`).Scan(&first, &last, &nc, &completed, &runRef, &latency); err != nil {
		t.Fatalf("intent row absent on PG: %v", err)
	}
	if first != 50 || last != 60 || nc != 2 || completed != 120 || runRef != "crun1" || latency != 70 {
		t.Fatalf("intent values drifted: %d %d %d %d %s %d", first, last, nc, completed, runRef, latency)
	}
	var watermark int64
	if err := pg.QueryRowContext(ctx, `SELECT snapshot_max_event_id FROM derivation_runs WHERE id = 'crun1'`).Scan(&watermark); err != nil || watermark != 7 {
		t.Fatalf("run watermark drifted: %d err %v", watermark, err)
	}
	var manifestEvt int64
	if err := pg.QueryRowContext(ctx, `SELECT event_id FROM run_input_events WHERE run_id = 'crun1'`).Scan(&manifestEvt); err != nil || manifestEvt != 7 {
		t.Fatalf("manifest row drifted: %d err %v", manifestEvt, err)
	}
}

// Zero-live-event copy (memoire:D165 round-5 falsifier for the round-4
// identity repair): a store whose events were ALL hard-purged still carries
// the sqlite_sequence high-water; the copy must advance the fresh PG
// identity past it so a subsequent ingest can never re-mint a purged id
// under a surviving (orphan-guarded) manifest.
func TestCopyAdvancesIdentityOnZeroEventRows(t *testing.T) {
	admin := os.Getenv("MEMOIRE_TEST_PG_URL")
	if admin == "" {
		t.Skip("MEMOIRE_TEST_PG_URL not set; postgres copy fixture skipped")
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("MEMOIRE_TEST_PG_URL: %v", err)
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	dbName := "memoire_copyzero_" + hex.EncodeToString(suffix[:])
	adminConn, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminConn.Exec("CREATE DATABASE " + dbName); err != nil {
		_ = adminConn.Close()
		t.Fatalf("create fixture database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminConn.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
		_ = adminConn.Close()
	})
	u.Path = "/" + dbName
	pg, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	ctx := context.Background()
	if err := Apply(ctx, pg, "../../../../schema-pg"); err != nil {
		t.Fatalf("apply: %v", err)
	}

	litePath := filepath.Join(t.TempDir(), "memoire.db")
	store, err := memdb.Open(litePath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	lite, err := sql.Open("sqlite3", litePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lite.Close() })
	for _, stmt := range []string{
		`INSERT INTO events(id, idempotency_key, source_event_id, session_id, project, source_adapter, event_type, payload_json, occurred_at_epoch, received_at_epoch)
		 VALUES (9, 'zk1', 'zk1', 'zs1', 'zeroproj', 'claude-code', 'note', '{}', 100, 100)`,
		`DELETE FROM events`,
	} {
		if _, err := lite.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}
	var hw int64
	if err := lite.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'events'`).Scan(&hw); err != nil || hw < 9 {
		t.Fatalf("fixture invalid: sqlite_sequence high-water = %d err %v (want >= 9)", hw, err)
	}

	counts, err := Copy(ctx, pg, lite)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if counts["events"] != 0 {
		t.Fatalf("copy counts: %+v (want zero events)", counts)
	}
	var nextID int64
	if err := pg.QueryRowContext(ctx, `INSERT INTO events(idempotency_key, session_id, project, source_adapter, event_type, payload_json, occurred_at_epoch, received_at_epoch)
		VALUES ('fresh-1', 'fs1', 'freshproj', 'claude-code', 'note', '{}', 200, 200) RETURNING id`).Scan(&nextID); err != nil {
		t.Fatalf("fresh PG ingest: %v", err)
	}
	if nextID <= hw {
		t.Fatalf("fresh PG id %d must exceed the purged high-water %d (id reuse under orphan manifests)", nextID, hw)
	}
}
