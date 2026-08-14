// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Round-3 finding R3-1: the observer open must refuse non-self-contained
// inputs through every spelling — direct path, symlinked path with a
// TARGET-side sidecar, and rollback-journal sidecars, not just WAL.
func TestOpenReadOnlyRefusesHotSidecars(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "frozen.db")
	store, err := Open(dbPath, schemaDirForTest(t))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	_ = store.Close()
	// Close leaves WAL-mode sidecars; a clean close truncates the WAL.
	// Remove any zero-byte leftovers so the baseline open succeeds.
	for _, sfx := range []string{"-wal", "-shm"} {
		_ = os.Remove(dbPath + sfx)
	}

	if ro, err := OpenReadOnly(dbPath); err != nil {
		t.Fatalf("baseline observer open: %v", err)
	} else {
		_ = ro.Close()
	}

	for _, sfx := range []string{"-wal", "-journal"} {
		if err := os.WriteFile(dbPath+sfx, []byte("hot"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenReadOnly(dbPath); err == nil || !strings.Contains(err.Error(), "sidecar") {
			t.Fatalf("hot %s: want sidecar refusal, got %v", sfx, err)
		}
		_ = os.Remove(dbPath + sfx)
	}

	// Symlink spelling: the sidecar lives beside the TARGET, the caller
	// passes the link. Resolution must happen before the preflight.
	link := filepath.Join(dir, "alias.db")
	if err := os.Symlink(dbPath, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.WriteFile(dbPath+"-wal", []byte("hot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(link); err == nil || !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("symlinked hot -wal: want sidecar refusal, got %v", err)
	}
}
