// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package redactderived

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	memdb "memoire/services/api/internal/db"
	"memoire/services/api/internal/derive"
	"memoire/services/api/internal/redact"
)

const obsHashedText = "rotate ghp_zyxwvutsrqponmlkjihgfedcba9876543210 now"

// Fixture through the real migration chain (db.Open runs schema/*.sql, so
// FTS triggers and 0007 governance triggers are live — the test proves the
// tool coexists with both).
func openFixture(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "memoire.db")
	store, err := memdb.Open(dbPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	obsHash, err := derive.CanonicalContentHash("memoire", "task", obsHashedText)
	if err != nil {
		t.Fatal(err)
	}

	// All secret-shaped values below are synthetic test fixtures.
	stmts := []string{
		`INSERT INTO sessions(session_id, project, client, started_at_epoch, meta_json)
		 VALUES ('s1', 'memoire', 'cc', 100,
		         '{"note":"API_KEY=\"faketestsecretvalue1\""}')`,
		`INSERT INTO events(idempotency_key, session_id, project, source_adapter, event_type, cwd,
		                    payload_json, text, occurred_at_epoch, received_at_epoch)
		 VALUES ('k1', 's1', 'memoire', 'claude-code', 'PostToolUse',
		         '/home/x/sk-testfakecwd123456789012345/repo',
		         '{"cmd":"export GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789"}',
		         'the key is sk-testfake123456789012345678 right here', 100, 100)`,
		`INSERT INTO events(idempotency_key, session_id, project, source_adapter, event_type, cwd,
		                    payload_json, text, occurred_at_epoch, received_at_epoch)
		 VALUES ('k2', 's1', 'memoire', 'claude-code', 'PostToolUse', '/home/x/repo',
		         '{"ref":"op://vault/item/field"}',
		         'already handled: [REDACTED:openai-key:deadbeef] and op://vault/item', 100, 100)`,
		`INSERT INTO observations(project, session_id, kind, text, occurred_at_epoch, created_at_epoch,
		                          review_state, provenance)
		 VALUES ('memoire', 's1', 'fact', 'aws key AKIAABCDEFGHIJKLMNOP was used', 100, 100,
		         'accepted', 'inferred')`,
		`INSERT INTO observations(project, session_id, kind, text, occurred_at_epoch, created_at_epoch,
		                          review_state, provenance, can_instruct)
		 VALUES ('memoire', 's1', 'constraint', 'glpat-faketoken1234567890abc must rotate', 100, 100,
		         'accepted', 'user_confirmed', 1)`,
		`INSERT INTO observations(project, session_id, kind, text, occurred_at_epoch, created_at_epoch,
		                          review_state, provenance, content_hash)
		 VALUES ('memoire', 's1', 'task', '` + obsHashedText + `', 100, 100,
		         'accepted', 'inferred', '` + obsHash + `')`,
		`INSERT INTO derivation_runs(id, project, session_id, provider, data_boundary, derivation_version,
		                             window_start_epoch, window_end_epoch, input_hash, status, created_at_epoch, meta_json)
		 VALUES ('drv_1', 'memoire', 's1', 'rules', 'local', 'v1', 100, 200, 'h1', 'completed', 100,
		         '{"raw_response":"Authorization: Bearer faketokenabcdef123456"}')`,
		`INSERT INTO observation_review_log(observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch)
		 VALUES (1, 'proposed', 'accepted', 'local', 'contains xoxb-123456789012-fakeslack', 100)`,
		`INSERT INTO audit_log(action, details_json, created_at_epoch)
		 VALUES ('review_transition', '{"reason_text":"contains xoxb-123456789012-fakeslack"}', 100)`,
		`INSERT INTO project_roots(project, root, source, first_seen_epoch, last_seen_epoch)
		 VALUES ('memoire', '/home/x/dev/memoire', 'registry', 100, 100)`,
	}
	for _, s := range stmts {
		if _, err := conn.Exec(s); err != nil {
			t.Fatalf("fixture: %v\n%s", err, s)
		}
	}
	return dbPath
}

// keyPath returns a path to an EXISTING redaction key (the tool refuses to
// mint one — review finding 3).
func keyPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "redaction-key")
	if _, err := redact.LoadOrCreateKey(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func snapshot(t *testing.T, dbPath string) string {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var out strings.Builder
	for _, q := range []string{
		`SELECT group_concat(text || '|' || payload_json || '|' || COALESCE(meta_json,'') || '|' || COALESCE(cwd,'')) FROM events ORDER BY id`,
		`SELECT group_concat(text || '|' || COALESCE(content_hash,'')) FROM observations ORDER BY id`,
		`SELECT group_concat(COALESCE(meta_json,'')) FROM derivation_runs`,
		`SELECT group_concat(COALESCE(meta_json,'')) FROM sessions`,
		`SELECT group_concat(COALESCE(reason_text,'')) FROM observation_review_log`,
		`SELECT group_concat(COALESCE(details_json,'')) FROM audit_log`,
	} {
		var s sql.NullString
		if err := conn.QueryRow(q).Scan(&s); err != nil {
			t.Fatal(err)
		}
		out.WriteString(s.String + "\n")
	}
	return out.String()
}

func ftsCount(t *testing.T, dbPath, needle string) int {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM observations_fts WHERE observations_fts MATCH ?`, needle).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRefusesMissingRedactionKey(t *testing.T) {
	dbPath := openFixture(t)
	_, err := Run(context.Background(), Options{DBPath: dbPath,
		RedactionKeyPath: filepath.Join(t.TempDir(), "nonexistent-key")})
	if err == nil || !strings.Contains(err.Error(), "redaction key not found") {
		t.Fatalf("must refuse a missing key without creating it, got: %v", err)
	}
}

// Identity-key path columns are never rewritten: a secret-shaped root blocks
// apply (round-2 review finding 4) and is reported in dry-run.
func TestPreflightBlocksSecretShapedIdentityPaths(t *testing.T) {
	dbPath := openFixture(t)
	kp := keyPath(t)
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO project_identity_alerts(project, root, known_root, source, first_seen_epoch, last_seen_epoch)
		VALUES ('memoire', '/home/x/sk-testfakeroot1234567890123/repo', '/home/x/dev/memoire', 'basename', 100, 100)`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), Options{DBPath: dbPath, RedactionKeyPath: kp})
	if err != nil {
		t.Fatal(err)
	}
	if got := report.PreflightHits["project_identity_alerts.root"]; got != 1 {
		t.Fatalf("dry-run must report the identity-path hit, got %v", report.PreflightHits)
	}

	before := snapshot(t, dbPath)
	_, err = Run(context.Background(), Options{DBPath: dbPath, RedactionKeyPath: kp, Apply: true})
	if err == nil || !strings.Contains(err.Error(), "identity-key path") {
		t.Fatalf("apply must refuse on identity-path hits, got: %v", err)
	}
	if snapshot(t, dbPath) != before {
		t.Fatal("refused apply mutated the database")
	}
}

func TestDryRunCountsAndDoesNotMutate(t *testing.T) {
	dbPath := openFixture(t)
	before := snapshot(t, dbPath)

	report, err := Run(context.Background(), Options{DBPath: dbPath, RedactionKeyPath: keyPath(t)})
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied {
		t.Fatal("dry-run must not report applied")
	}
	if got := report.Tables["events"].RowsChanged; got != 1 {
		t.Fatalf("events rows_changed = %d, want 1 (marker+exempt row must not count)", got)
	}
	if got := report.Tables["events"].HitsByRule["openai-key"]; got != 2 {
		t.Fatalf("events openai-key hits = %d, want 2 (text + cwd)", got)
	}
	if got := report.Tables["observations"].RowsChanged; got != 3 {
		t.Fatalf("observations rows_changed = %d, want 3", got)
	}
	if got := report.Tables["observations"].HashesRecomputed; got != 1 {
		t.Fatalf("observations hashes_recomputed = %d, want 1 (only the hashed row)", got)
	}
	for _, table := range []string{"derivation_runs", "sessions", "observation_review_log", "audit_log"} {
		if got := report.Tables[table].RowsChanged; got != 1 {
			t.Fatalf("%s rows_changed = %d, want 1", table, got)
		}
	}
	if report.TotalChanged != 8 {
		t.Fatalf("total_changed = %d, want 8", report.TotalChanged)
	}
	if snapshot(t, dbPath) != before {
		t.Fatal("dry-run mutated the database")
	}
}

func TestApplyRedactsAllSurfacesAndConverges(t *testing.T) {
	dbPath := openFixture(t)
	kp := keyPath(t)

	if n := ftsCount(t, dbPath, "AKIAABCDEFGHIJKLMNOP"); n != 1 {
		t.Fatalf("fixture FTS must find the planted AWS key, got %d", n)
	}

	report, err := Run(context.Background(), Options{DBPath: dbPath, RedactionKeyPath: kp, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalChanged != 8 {
		t.Fatalf("apply total_changed = %d, want 8", report.TotalChanged)
	}

	after := snapshot(t, dbPath)
	for _, secret := range []string{
		"sk-testfake123456789012345678",
		"sk-testfakecwd123456789012345",
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"ghp_zyxwvutsrqponmlkjihgfedcba9876543210",
		"AKIAABCDEFGHIJKLMNOP",
		"glpat-faketoken1234567890abc",
		"faketokenabcdef123456",
		"faketestsecretvalue1",
		"xoxb-123456789012-fakeslack",
	} {
		if strings.Contains(after, secret) {
			t.Fatalf("secret %q survived apply", secret)
		}
	}
	for _, keep := range []string{
		"op://vault/item",
		"[REDACTED:openai-key:deadbeef]",
	} {
		if !strings.Contains(after, keep) {
			t.Fatalf("exempt/marker content %q must survive apply", keep)
		}
	}

	// content_hash tracks the redacted text (review finding 2): stored hash
	// equals CanonicalContentHash of the NEW text and differs from the old.
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var newText, newHash string
	if err := conn.QueryRow(`SELECT text, content_hash FROM observations WHERE kind = 'task'`).Scan(&newText, &newHash); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(newText, "[REDACTED:github-token:") {
		t.Fatalf("hashed observation text not redacted: %q", newText)
	}
	wantHash, err := derive.CanonicalContentHash("memoire", "task", newText)
	if err != nil {
		t.Fatal(err)
	}
	if newHash != wantHash {
		t.Fatalf("content_hash not recomputed for redacted text: got %s want %s", newHash, wantHash)
	}
	oldHash, err := derive.CanonicalContentHash("memoire", "task", obsHashedText)
	if err != nil {
		t.Fatal(err)
	}
	if newHash == oldHash {
		t.Fatal("content_hash unchanged despite text change")
	}
	// Rows without a stored hash stay hashless.
	var nullHashes int
	if err := conn.QueryRow(`SELECT count(*) FROM observations WHERE content_hash IS NULL`).Scan(&nullHashes); err != nil {
		t.Fatal(err)
	}
	if nullHashes != 2 {
		t.Fatalf("hashless observations = %d, want 2 (recompute must not mint hashes)", nullHashes)
	}

	// FTS synced through the external-content AU trigger.
	if n := ftsCount(t, dbPath, "AKIAABCDEFGHIJKLMNOP"); n != 0 {
		t.Fatalf("FTS still finds the AWS key after apply: %d", n)
	}
	if n := ftsCount(t, dbPath, "REDACTED"); n == 0 {
		t.Fatal("FTS must find redaction markers after apply")
	}

	// Idempotent: the second pass converges to zero.
	report2, err := Run(context.Background(), Options{DBPath: dbPath, RedactionKeyPath: kp, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if report2.TotalChanged != 0 {
		t.Fatalf("second apply total_changed = %d, want 0", report2.TotalChanged)
	}
}

// Enumeration tripwire (slice-A lesson), both levels. Table level: every
// table in the live schema is either a redaction target or explicitly
// allowlisted as non-content — a future migration adding a table fails here
// until the author decides which it is. Column level: every TEXT column on
// target tables is enrolled or allowlisted.
func TestTargetsCoverAllTextColumns(t *testing.T) {
	dbPath := openFixture(t)
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	nonContentTables := map[string]bool{
		"credentials":               true, // token hashes/labels, never envelope content
		"schema_version":            true,
		"sqlite_sequence":           true,
		"observation_source_events": true, // integer link table
		// Identity tables (0010): path columns are resolution keys, guarded by
		// the preflight refuse-on-hit instead of rewritten — see PreflightGuards.
		"project_roots":           true,
		"project_identity_alerts": true,
		// Derive notify intents (0013, memoire:D165): project/session keys,
		// epochs, and a run-id reference — no envelope content ever lands here.
		"derive_intents": true,
		// Run input manifest (0015): pure (run_id, event_id) link rows.
		"run_input_events": true,
	}

	// Every guarded identity column must exist in the live schema (pin
	// against renames silently emptying the preflight).
	for _, g := range PreflightGuards() {
		var n int
		if err := conn.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, g[0], g[1]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("preflight guard %s.%s does not exist in the live schema", g[0], g[1])
		}
	}

	enrolled := Targets()
	tables, err := conn.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	var allTables []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		allTables = append(allTables, name)
	}
	if err := tables.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range allTables {
		if strings.HasPrefix(name, "observations_fts") {
			continue // external-content FTS + shadow tables; content lives in observations
		}
		if _, ok := enrolled[name]; ok {
			continue
		}
		if !nonContentTables[name] {
			t.Errorf("table %s is neither a redaction target nor allowlisted as non-content", name)
		}
	}

	nonContentColumns := map[string]map[string]bool{
		"events": {"idempotency_key": true, "session_id": true, "content_session_id": true,
			"project": true, "source_adapter": true, "source_event_id": true, "event_type": true,
			"principal_id": true, "credential_id": true, "request_id": true},
		"observations": {"project": true, "session_id": true, "kind": true, "source_adapter": true,
			"event_type": true, "principal_id": true, "review_state": true, "provenance": true,
			"derivation_run_id": true, "content_hash": true},
		"derivation_runs": {"id": true, "project": true, "session_id": true, "provider": true,
			"data_boundary": true, "derivation_version": true, "prompt_version": true,
			"input_hash": true, "status": true, "model": true, "endpoint_id": true},
		"sessions":               {"session_id": true, "project": true, "client": true, "principal_id": true},
		"observation_review_log": {"from_state": true, "to_state": true, "actor": true, "action": true},
		"audit_log": {"principal_id": true, "credential_id": true, "request_id": true,
			"action": true, "resource_type": true, "resource_id": true},
	}

	for table, cols := range enrolled {
		enrolledSet := map[string]bool{}
		for _, c := range cols {
			enrolledSet[c] = true
		}
		rows, err := conn.Query(`SELECT name, type FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name, typ string
			if err := rows.Scan(&name, &typ); err != nil {
				t.Fatal(err)
			}
			if !strings.EqualFold(typ, "TEXT") {
				continue
			}
			if enrolledSet[name] {
				delete(enrolledSet, name)
				continue
			}
			if !nonContentColumns[table][name] {
				t.Errorf("%s.%s is a TEXT column neither enrolled as a redaction target nor allowlisted as non-content", table, name)
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		for missing := range enrolledSet {
			t.Errorf("enrolled column %s.%s does not exist in the live schema", table, missing)
		}
	}
}
