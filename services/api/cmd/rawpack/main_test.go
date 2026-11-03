// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"memoire/services/api/internal/rawstore"
)

func TestDryRunDefaultThenApply(t *testing.T) {
	home := t.TempDir()
	raw := filepath.Join(home, "raw")
	if err := os.MkdirAll(raw, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC).UnixMilli()
	name := fmt.Sprintf("%d-http-%064x.json", old, 1)
	if err := os.WriteFile(filepath.Join(raw, name), []byte(`{"n":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return time.Date(2026, 10, 11, 4, 0, 0, 0, time.UTC) }

	var out, errOut bytes.Buffer
	verifyDigest := func() string {
		t.Helper()
		var vo bytes.Buffer
		if err := run([]string{"--home", home, "--verify"}, &vo, &errOut, now); err != nil {
			t.Fatal(err)
		}
		var v verifyReport
		if err := json.Unmarshal(vo.Bytes(), &v); err != nil || v.Envelopes != 1 {
			t.Fatalf("verify report %s %v", vo.String(), err)
		}
		return v.Digest
	}
	digestBefore := verifyDigest()
	if err := run([]string{"--home", home}, &out, &errOut, now); err != nil {
		t.Fatal(err)
	}
	var dry rawstore.PackReport
	if err := json.Unmarshal(out.Bytes(), &dry); err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || dry.FlatFilesPacked != 1 || dry.PartsWritten != 0 {
		t.Fatalf("dry-run report %+v", dry)
	}
	if _, err := os.Stat(filepath.Join(raw, name)); err != nil {
		t.Fatal("dry-run must not touch raw/")
	}

	out.Reset()
	if err := run([]string{"--home", home, "--apply"}, &out, &errOut, now); err != nil {
		t.Fatal(err)
	}
	var applied rawstore.PackReport
	if err := json.Unmarshal(out.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	if applied.PartsWritten != 1 || applied.FlatFilesDeleted != 1 {
		t.Fatalf("apply report %+v", applied)
	}
	if _, err := os.Stat(filepath.Join(raw, "pack-2026-10-01.000.tar.zst")); err != nil {
		t.Fatalf("pack missing: %v", err)
	}
	if got := verifyDigest(); got != digestBefore {
		t.Fatalf("store digest changed by packing: %s -> %s", digestBefore, got)
	}
	// A purge intent blocks packing.
	later := fmt.Sprintf("%d-http-%064x.json", old+1, 2)
	if err := os.WriteFile(filepath.Join(raw, later), []byte(`{"n":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "purge-intent.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--home", home, "--apply"}, &out, &errOut, now); err == nil {
		t.Fatal("apply must refuse while <home>/purge-intent.json exists")
	}
	if _, err := os.Stat(filepath.Join(raw, later)); err != nil {
		t.Fatal("a refused run must leave raw/ untouched")
	}
	// Review N2/N5: --verify enters custody. With the intent present it
	// refuses for the intent; with the intent gone and the lock held, it
	// refuses for the lock. Each refusal is told apart by its reason.
	if err := run([]string{"--home", home, "--verify"}, &out, &errOut, now); err == nil || !strings.Contains(err.Error(), "purge is unfinished") {
		t.Fatalf("--verify with a purge intent: %v, want an interlock refusal", err)
	}
	if err := os.Remove(filepath.Join(home, "purge-intent.json")); err != nil {
		t.Fatal(err)
	}
	unlock, err := rawstore.Lock(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--home", home, "--verify"}, &out, &errOut, now); err == nil || !strings.Contains(err.Error(), "custody lock") {
		t.Fatalf("--verify with the lock held: %v, want a custody-lock refusal", err)
	}
	unlock()
	if err := run([]string{"--home", home, "--grace-days", "0"}, &out, &errOut, now); err == nil {
		t.Fatal("grace-days 0 must refuse (the current day is never closed)")
	}
}
