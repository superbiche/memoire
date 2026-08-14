// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	memdb "memoire/services/api/internal/db"
)

func fixtureHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	store, err := memdb.Open(filepath.Join(home, "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"raw", "queue/pending", "queue/failed"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestSelectorRequired(t *testing.T) {
	home := fixtureHome(t)
	var out, errBuf bytes.Buffer
	err := run([]string{"--home", home}, &out, &errBuf)
	if err == nil || !strings.Contains(err.Error(), "exactly one selector") {
		t.Fatalf("want selector-required error, got: %v", err)
	}
}

func TestApplyRefusedWithoutEnvGate(t *testing.T) {
	home := fixtureHome(t)
	t.Setenv("MEMOIRE_PURGE_APPLY", "")
	var out, errBuf bytes.Buffer
	err := run([]string{"--home", home, "--project", "x", "--apply", "--no-quarantine"}, &out, &errBuf)
	if err == nil || !strings.Contains(err.Error(), "double gate") {
		t.Fatalf("apply without env gate must refuse, got: %v", err)
	}
}

func TestResumeIntentAlsoDoubleGated(t *testing.T) {
	home := fixtureHome(t)
	t.Setenv("MEMOIRE_PURGE_APPLY", "")
	var out, errBuf bytes.Buffer
	err := run([]string{"--home", home, "--resume-intent"}, &out, &errBuf)
	if err == nil || !strings.Contains(err.Error(), "double gate") {
		t.Fatalf("resume-intent without env gate must refuse, got: %v", err)
	}
}

func TestApplyRefusedWhileGuardedProcessRuns(t *testing.T) {
	home := fixtureHome(t)
	decoy := filepath.Join(t.TempDir(), "memoire-api-decoy")
	if err := os.WriteFile(decoy, []byte("#!/usr/bin/env bash\nsleep 20\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(decoy)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	t.Setenv("MEMOIRE_PURGE_APPLY", "yes")
	t.Setenv("MEMOIRE_PURGE_SKIP_QUIESCE", "")
	var out, errBuf bytes.Buffer
	err := run([]string{"--home", home, "--project", "x", "--apply", "--no-quarantine"}, &out, &errBuf)
	if err == nil || !strings.Contains(err.Error(), "quiesce") {
		t.Fatalf("apply must refuse while a memoire-api process runs, got: %v", err)
	}
}

func TestDryRunMessageAndReportFile(t *testing.T) {
	home := fixtureHome(t)
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var out, errBuf bytes.Buffer
	if err := run([]string{"--home", home, "--project", "nothing-here", "--report", reportPath}, &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errBuf.String(), "dry-run: no changes made") {
		t.Fatalf("dry-run must announce itself on stderr, got: %s", errBuf.String())
	}
	var report struct {
		DryRun        bool  `json:"dry_run"`
		EventsMatched int64 `json:"events_matched"`
	}
	b, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	if !report.DryRun || report.EventsMatched != 0 {
		t.Fatalf("empty fixture dry-run report wrong: %+v", report)
	}
}

func TestGatedApplyRunsOnFixture(t *testing.T) {
	home := fixtureHome(t)
	t.Setenv("MEMOIRE_PURGE_APPLY", "yes")
	t.Setenv("MEMOIRE_PURGE_SKIP_QUIESCE", "yes")
	var out, errBuf bytes.Buffer
	if err := run([]string{"--home", home, "--project", "nothing-here", "--apply", "--no-quarantine"}, &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	var report struct {
		DryRun bool `json:"dry_run"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.DryRun {
		t.Fatal("gated apply must report dry_run=false")
	}
}

func TestBadIDListRejected(t *testing.T) {
	home := fixtureHome(t)
	var out, errBuf bytes.Buffer
	err := run([]string{"--home", home, "--event-ids", "1,zap"}, &out, &errBuf)
	if err == nil || !strings.Contains(err.Error(), "bad id") {
		t.Fatalf("want bad-id error, got: %v", err)
	}
}
