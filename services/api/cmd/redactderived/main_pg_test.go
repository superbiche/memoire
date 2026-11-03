// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

// Postgres engine selection and hub gating (redact-v5 S2a), on the
// memoire-purge model: --db postgres://… or a postgres DSN in MEMOIRE_DB
// selects the engine; a Postgres apply needs the explicit operator
// attestation --i-have-quiesced-the-hub (the process check is meaningless
// across pods); the DSN password never reaches stdout, stderr or the
// report file. The refusal tests use an unreachable DSN: they must fail on
// the gate, before any connection. The end-to-end legs need
// MEMOIRE_TEST_PG_URL (`make test-pg`).

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"memoire/services/api/internal/pgshadow"
	"memoire/services/api/internal/redact"
)

const unreachableDSN = "postgres://memoire:not-a-real-password-zz9@127.0.0.1:1/memoire"

// isolateHome points HOME at a fresh directory: a test whose engine
// selection regresses must fall onto an empty default path, never the
// operator's real ~/.memoire/memoire.db.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func keyOnly(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "redaction-key")
	if _, err := redact.LoadOrCreateKey(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPGApplyRefusedWithoutHubAttestation(t *testing.T) {
	isolateHome(t)
	t.Setenv("MEMOIRE_REDACT_DERIVED_APPLY", "yes")
	var out, errBuf bytes.Buffer
	err := run([]string{"--db", unreachableDSN, "--redaction-key", keyOnly(t), "--apply"}, &out, &errBuf)
	if err == nil || !strings.Contains(err.Error(), "--i-have-quiesced-the-hub") {
		t.Fatalf("a postgres apply without the attestation must refuse naming it, got: %v", err)
	}
	if strings.Contains(err.Error()+out.String()+errBuf.String(), "not-a-real-password-zz9") {
		t.Fatal("the refusal leaked the DSN password")
	}
}

func TestPGEnvGateStillRequired(t *testing.T) {
	isolateHome(t)
	t.Setenv("MEMOIRE_REDACT_DERIVED_APPLY", "")
	var out, errBuf bytes.Buffer
	err := run([]string{"--db", unreachableDSN, "--redaction-key", keyOnly(t), "--apply", "--i-have-quiesced-the-hub"}, &out, &errBuf)
	if err == nil || !strings.Contains(err.Error(), "double gate") {
		t.Fatalf("the attestation must not replace the env gate, got: %v", err)
	}
}

func TestHubAttestationRejectedOnSQLite(t *testing.T) {
	isolateHome(t)
	dbPath, keyPath := fixture(t)
	var out, errBuf bytes.Buffer
	err := run([]string{"--db", dbPath, "--redaction-key", keyPath, "--i-have-quiesced-the-hub"}, &out, &errBuf)
	if err == nil || !strings.Contains(err.Error(), "Postgres") {
		t.Fatalf("--i-have-quiesced-the-hub on a SQLite store must be refused, got: %v", err)
	}
}

// MEMOIRE_DB is consulted only for a Postgres DSN (the hub pods carry it
// from the memoire-pg-app secret); an explicit --db wins.
func TestPGDSNFromMEMOIRE_DB(t *testing.T) {
	isolateHome(t)
	t.Setenv("MEMOIRE_REDACT_DERIVED_APPLY", "yes")
	t.Setenv("MEMOIRE_DB", unreachableDSN)
	var out, errBuf bytes.Buffer
	err := run([]string{"--redaction-key", keyOnly(t), "--apply"}, &out, &errBuf)
	if err == nil || !strings.Contains(err.Error(), "--i-have-quiesced-the-hub") {
		t.Fatalf("a postgres DSN in MEMOIRE_DB must select the postgres engine, got: %v", err)
	}

	dbPath, keyPath := fixture(t)
	out.Reset()
	errBuf.Reset()
	t.Setenv("MEMOIRE_REDACT_DERIVED_APPLY", "")
	if err := run([]string{"--db", dbPath, "--redaction-key", keyPath}, &out, &errBuf); err != nil {
		t.Fatalf("an explicit --db must win over MEMOIRE_DB, got: %v", err)
	}
}

func TestNonPostgresMEMOIRE_DBIsIgnored(t *testing.T) {
	home := isolateHome(t)
	t.Setenv("MEMOIRE_DB", filepath.Join(t.TempDir(), "elsewhere.db"))
	var out, errBuf bytes.Buffer
	err := run([]string{"--redaction-key", keyOnly(t)}, &out, &errBuf)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(home, ".memoire", "memoire.db")) {
		t.Fatalf("a non-postgres MEMOIRE_DB must be ignored (default SQLite path), got: %v", err)
	}
}

func newPGDSN(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("MEMOIRE_TEST_PG_URL")
	if admin == "" {
		t.Skip("MEMOIRE_TEST_PG_URL not set; postgres cmd leg skipped")
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	dbName := "memoire_redactderived_cmd_" + hex.EncodeToString(suffix[:])
	adminConn, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminConn.Exec("CREATE DATABASE " + dbName + " TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
		_ = adminConn.Close()
		t.Fatalf("create fixture database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminConn.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
		_ = adminConn.Close()
	})
	u.Path = "/" + dbName
	dsn := u.String()
	pg, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	if err := pgshadow.Apply(context.Background(), pg, "../../../../schema-pg"); err != nil {
		t.Fatalf("apply schema-pg: %v", err)
	}
	if _, err := pg.Exec(`INSERT INTO sessions(session_id, project, client, started_at_epoch) VALUES ('s1', 'memoire', 'cc', 100)`); err != nil {
		t.Fatal(err)
	}
	// Synthetic test fixture value.
	if _, err := pg.Exec(`INSERT INTO events(idempotency_key, session_id, project, source_adapter, event_type, cwd, payload_json, text, occurred_at_epoch, received_at_epoch)
		VALUES ('k1', 's1', 'memoire', 'claude-code', 'note', '/x', '{}', 'aws key AKIA` +
		`ABCDEFGHIJKLMNOP here', 100, 100)`); err != nil {
		t.Fatal(err)
	}
	return dsn
}

func TestPGGatedApplyRunsWithAttestation(t *testing.T) {
	isolateHome(t)
	dsn := newPGDSN(t)
	u, _ := url.Parse(dsn)
	pw, _ := u.User.Password()
	reportPath := filepath.Join(t.TempDir(), "report.json")
	t.Setenv("MEMOIRE_REDACT_DERIVED_APPLY", "yes")
	var out, errBuf bytes.Buffer
	err := run([]string{"--db", dsn, "--redaction-key", keyOnly(t), "--apply", "--i-have-quiesced-the-hub", "--report", reportPath}, &out, &errBuf)
	if err != nil {
		t.Fatalf("attested postgres apply: %v (stderr: %s)", err, errBuf.String())
	}
	var report struct {
		Applied      bool   `json:"applied"`
		DBPath       string `json:"db_path"`
		TotalChanged int    `json:"total_changed"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not the JSON report: %v\n%s", err, out.String())
	}
	if !report.Applied || report.TotalChanged != 1 {
		t.Fatalf("report applied=%v total_changed=%d, want true/1", report.Applied, report.TotalChanged)
	}
	file, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if pw != "" && strings.Contains(out.String()+errBuf.String()+string(file), pw) {
		t.Fatal("the DSN password reached stdout, stderr or the report file")
	}
}

func TestPGDryRunNeedsNoAttestation(t *testing.T) {
	isolateHome(t)
	dsn := newPGDSN(t)
	var out, errBuf bytes.Buffer
	if err := run([]string{"--db", dsn, "--redaction-key", keyOnly(t)}, &out, &errBuf); err != nil {
		t.Fatalf("a postgres dry-run must run without gates: %v", err)
	}
	if !strings.Contains(errBuf.String(), "dry-run: no changes made") {
		t.Fatalf("dry-run notice missing from stderr: %q", errBuf.String())
	}
}
