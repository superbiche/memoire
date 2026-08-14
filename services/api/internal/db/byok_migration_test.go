// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestByokMigrationPreservesRuns pins schema/0016_byok_provider.sql — the
// first migration under the strict-widening carve-out (operator-ruled
// 2026-08-05): a pre-0016 store with existing derivation_runs rows must come
// through the SQLite table rebuild with every row intact, the widened CHECK
// admitting 'byok', and the idempotency index still enforcing uniqueness.
func TestByokMigrationPreservesRuns(t *testing.T) {
	// Stage a pre-0016 schema dir: every migration below 0016.
	src := "../../../../schema"
	oldDir := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	staged := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") || name >= "0016" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(oldDir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
		staged++
	}
	if staged < 15 {
		t.Fatalf("expected to stage at least 15 pre-0016 migrations, staged %d", staged)
	}

	dbPath := filepath.Join(t.TempDir(), "memoire.db")
	old, err := Open(dbPath, oldDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// A row under the OLD constraint, with the post-0006 columns populated.
	if _, err := old.db.ExecContext(ctx, `INSERT INTO derivation_runs
		(id, project, session_id, provider, data_boundary, derivation_version, prompt_version,
		 input_hash, window_start_epoch, window_end_epoch, status, created_at_epoch, meta_json,
		 model, endpoint_id, created_at_ns, snapshot_max_event_id)
		VALUES ('run-old-1', 'p', 's', 'local-qwen', 'local', 'v2', 'pv', 'hash-1',
		 100, 200, 'completed', 150, '{}', 'qwen-test', 'hl3', 1, 42)`); err != nil {
		t.Fatal(err)
	}
	// The OLD constraint must still refuse byok — proves the fixture really
	// predates the widening.
	if _, err := old.db.ExecContext(ctx, `INSERT INTO derivation_runs
		(id, project, provider, data_boundary, derivation_version, input_hash,
		 window_start_epoch, window_end_epoch, status, created_at_epoch)
		VALUES ('run-old-2', 'p', 'byok', 'local', 'v2', 'hash-2', 100, 200, 'completed', 150)`); err == nil {
		t.Fatal("pre-0016 CHECK must refuse provider byok")
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen against the FULL schema dir: 0016 rebuilds the table.
	store, err := Open(dbPath, src)
	if err != nil {
		t.Fatalf("0016 migration on a populated store: %v", err)
	}
	defer store.Close()

	var project, model, endpoint string
	var ns, watermark int64
	if err := store.rdb.QueryRowContext(ctx, `SELECT project, model, endpoint_id, created_at_ns, snapshot_max_event_id
		FROM derivation_runs WHERE id = 'run-old-1'`).Scan(&project, &model, &endpoint, &ns, &watermark); err != nil {
		t.Fatalf("pre-migration row lost in the rebuild: %v", err)
	}
	if project != "p" || model != "qwen-test" || endpoint != "hl3" || ns != 1 || watermark != 42 {
		t.Fatalf("pre-migration row mutated: %s %s %s %d %d", project, model, endpoint, ns, watermark)
	}

	// The widened CHECK admits byok with either declared boundary.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO derivation_runs
		(id, project, provider, data_boundary, derivation_version, input_hash,
		 window_start_epoch, window_end_epoch, status, created_at_epoch, model, endpoint_id)
		VALUES ('run-byok-1', 'p', 'byok', 'external', 'v2', 'hash-3', 100, 200, 'completed', 150, 'm', 'byok')`); err != nil {
		t.Fatalf("widened CHECK must admit byok: %v", err)
	}
	// A non-member value must still refuse — the constraint was widened,
	// not dropped.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO derivation_runs
		(id, project, provider, data_boundary, derivation_version, input_hash,
		 window_start_epoch, window_end_epoch, status, created_at_epoch)
		VALUES ('run-bad', 'p', 'mystery', 'local', 'v2', 'hash-4', 100, 200, 'completed', 150)`); err == nil {
		t.Fatal("widened CHECK must still refuse non-member providers")
	}
	// The rebuilt idempotency index still enforces the unique tuple.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO derivation_runs
		(id, project, session_id, provider, data_boundary, derivation_version, prompt_version,
		 input_hash, window_start_epoch, window_end_epoch, status, created_at_epoch, model, endpoint_id)
		VALUES ('run-dupe', 'p', 's', 'local-qwen', 'local', 'v2', 'pv', 'hash-1',
		 100, 200, 'completed', 150, 'qwen-test', 'hl3')`); err == nil {
		t.Fatal("rebuilt idempotency index must refuse the duplicate tuple")
	}
}
