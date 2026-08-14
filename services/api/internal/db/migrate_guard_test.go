// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"path/filepath"
	"strings"
	"testing"
)

// D166 ruling (2): an unresolvable schema dir must be a hard Open failure,
// never a silent no-op. The defective variant (empty Glob falling through)
// ran production sweeps unmigrated for the whole 42P01 window.
func TestOpenRefusesUnresolvableSchemaDir(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.db")

	// Missing dir entirely.
	if _, err := Open(dbPath, filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("Open must refuse a missing schema dir")
	} else if !strings.Contains(err.Error(), "no migration files") {
		t.Fatalf("want the migration-guard error, got: %v", err)
	}

	// Present but empty dir (zero *.sql) — same refusal.
	if _, err := Open(dbPath, t.TempDir()); err == nil {
		t.Fatal("Open must refuse a schema dir with zero migration files")
	}
}
