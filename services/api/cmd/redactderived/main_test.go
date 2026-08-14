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
	"memoire/services/api/internal/redact"
)

func fixture(t *testing.T) (dbPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "memoire.db")
	store, err := memdb.Open(dbPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(dir, "redaction-key")
	if _, err := redact.LoadOrCreateKey(keyPath); err != nil {
		t.Fatal(err)
	}
	return dbPath, keyPath
}

func TestApplyRefusedWithoutEnvGate(t *testing.T) {
	dbPath, keyPath := fixture(t)
	t.Setenv("MEMOIRE_REDACT_DERIVED_APPLY", "")
	var out, errBuf bytes.Buffer
	err := run([]string{"--db", dbPath, "--redaction-key", keyPath, "--apply"}, &out, &errBuf)
	if err == nil || !strings.Contains(err.Error(), "double gate") {
		t.Fatalf("apply without env gate must refuse, got: %v", err)
	}
}

func TestApplyRefusedWhileDeriveRuns(t *testing.T) {
	dbPath, keyPath := fixture(t)
	decoy := filepath.Join(t.TempDir(), "memoire-derive-decoy")
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

	t.Setenv("MEMOIRE_REDACT_DERIVED_APPLY", "yes")
	t.Setenv("MEMOIRE_REDACT_DERIVED_SKIP_QUIESCE", "")
	var out, errBuf bytes.Buffer
	err := run([]string{"--db", dbPath, "--redaction-key", keyPath, "--apply"}, &out, &errBuf)
	if err == nil || !strings.Contains(err.Error(), "quiesce") {
		t.Fatalf("apply must refuse while a memoire-derive process runs, got: %v", err)
	}
}

func TestDryRunMessageAndReportFile(t *testing.T) {
	dbPath, keyPath := fixture(t)
	reportPath := filepath.Join(t.TempDir(), "report.json")
	var out, errBuf bytes.Buffer
	if err := run([]string{"--db", dbPath, "--redaction-key", keyPath, "--report", reportPath}, &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errBuf.String(), "dry-run: no changes made") {
		t.Fatalf("dry-run must announce itself on stderr, got: %s", errBuf.String())
	}
	var report struct {
		Applied      bool `json:"applied"`
		TotalChanged int  `json:"total_changed"`
	}
	b, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	if report.Applied || report.TotalChanged != 0 {
		t.Fatalf("empty fixture dry-run report wrong: %+v", report)
	}
}

func TestGatedApplyRunsOnFixture(t *testing.T) {
	dbPath, keyPath := fixture(t)
	t.Setenv("MEMOIRE_REDACT_DERIVED_APPLY", "yes")
	t.Setenv("MEMOIRE_REDACT_DERIVED_SKIP_QUIESCE", "yes")
	var out, errBuf bytes.Buffer
	if err := run([]string{"--db", dbPath, "--redaction-key", keyPath, "--apply"}, &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Applied bool `json:"applied"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Applied {
		t.Fatal("gated apply must report applied=true")
	}
}
