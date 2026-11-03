// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package rawstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// custodyFixture: <root>/raw with a packed day and flat files, a custody
// manifest, and the default purge-intent path beside raw/.
func custodyFixture(t *testing.T) (root, dir, manifest string) {
	t.Helper()
	root = t.TempDir()
	dir = filepath.Join(root, "raw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest = filepath.Join(root, "custody", "MANIFEST.json")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(`{"generation": 7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureCorpus(t, dir)
	packApply(t, dir)
	return root, dir, manifest
}

func snapshotDir(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	for _, n := range dirNames(t, dir) {
		b.WriteString(n)
		b.WriteString("\n")
	}
	return b.String()
}

// entryPoints are every rawstore custody operation (contract items 1–2).
func entryPoints(dir, manifest string) map[string]func() error {
	c := Custody{RawDir: dir, ManifestPath: manifest}
	return map[string]func() error{
		"Custody.Read":   func() error { return c.Read(func() error { return nil }) },
		"Custody.Move":   func() error { return c.Move(func(*Session) error { return nil }) },
		"Custody.Rename": func() error { return c.Rename("t", nil, func(int64) error { return nil }) },
		"PackClosedDays apply": func() error {
			_, err := PackClosedDays(dir, PackOptions{Now: packNow, GraceDays: 1, Apply: true})
			return err
		},
		"PackClosedDays dry-run": func() error {
			_, err := PackClosedDays(dir, PackOptions{Now: packNow, GraceDays: 1})
			return err
		},
		"Unpack": func() error {
			_, err := Unpack(dir, "pack-2026-10-01.000.tar.zst", manifest, nil)
			return err
		},
	}
}

// Oracle C2 (lock coverage): with the custody lock held elsewhere, every
// entry point refuses, and neither raw/ nor the custody generation moves.
func TestCustodyEntryPointsRefuseWhileLockHeld(t *testing.T) {
	_, dir, manifest := custodyFixture(t)
	before, gen := snapshotDir(t, dir), readGen(t, manifest)
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for name, op := range entryPoints(dir, manifest) {
		if err := op(); err == nil || !strings.Contains(err.Error(), "custody lock") {
			t.Fatalf("%s while the lock is held: err=%v, want a custody-lock refusal", name, err)
		}
	}
	if snapshotDir(t, dir) != before || readGen(t, manifest) != gen {
		t.Fatal("a refused entry point changed raw/ or the custody generation")
	}
}

// Oracle C3 (intent under lock): a purge intent journaled AFTER the lock is
// acquired but before the interlock runs (the race N3 found) is seen by
// every entry point; nothing moves, no generation advances.
func TestCustodyIntentCheckedUnderLock(t *testing.T) {
	root, dir, manifest := custodyFixture(t)
	before, gen := snapshotDir(t, dir), readGen(t, manifest)
	intent := DefaultIntentPath(dir)
	if intent != filepath.Join(root, "purge-intent.json") {
		t.Fatalf("default intent path %s", intent)
	}
	orig := beforeIntentCheck
	t.Cleanup(func() { beforeIntentCheck = orig })
	for name, op := range entryPoints(dir, manifest) {
		_ = os.Remove(intent)
		beforeIntentCheck = func() {
			if err := os.WriteFile(intent, []byte(`{"phase":"committed"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := op(); err == nil || !strings.Contains(err.Error(), "purge is unfinished") {
			t.Fatalf("%s with an intent journaled after the lock: err=%v, want an interlock refusal", name, err)
		}
		if _, err := os.Stat(LockPath(dir)); err != nil {
			t.Fatalf("%s: lock file missing: %v", name, err)
		}
		// The lock must have been released on refusal.
		unlock, err := Lock(dir)
		if err != nil {
			t.Fatalf("%s left the custody lock held after refusing: %v", name, err)
		}
		unlock()
	}
	beforeIntentCheck = orig
	if snapshotDir(t, dir) != before || readGen(t, manifest) != gen {
		t.Fatal("a refused entry point changed raw/ or the custody generation")
	}
	_ = os.Remove(intent)
	if err := (Custody{RawDir: dir}).Read(func() error { return nil }); err != nil {
		t.Fatalf("with no intent, Read must proceed: %v", err)
	}
	// SkipIntents is purge's own mode: it proceeds while its intent exists.
	if err := os.WriteFile(intent, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (Custody{RawDir: dir, SkipIntents: true}).Read(func() error { return nil }); err != nil {
		t.Fatalf("SkipIntents must proceed: %v", err)
	}
}

// Contract item 2: Rename advances the generation BEFORE its body runs and
// not at all when the precheck fails.
func TestRenameBumpsBeforeBodyAndOnlyAfterPrecheck(t *testing.T) {
	_, dir, manifest := custodyFixture(t)
	c := Custody{RawDir: dir, ManifestPath: manifest}
	gen := readGen(t, manifest)
	if err := c.Rename("t", func() error { return os.ErrNotExist }, func(int64) error {
		t.Fatal("body must not run after a failed precheck")
		return nil
	}); err == nil {
		t.Fatal("failed precheck must fail Rename")
	}
	if readGen(t, manifest) != gen {
		t.Fatal("a failed precheck must not advance the generation")
	}
	if err := c.Rename("t", nil, func(g int64) error {
		if readGen(t, manifest) != g || g != gen+1 {
			t.Fatalf("body ran before the generation was durable: manifest %d, passed %d", readGen(t, manifest), g)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
