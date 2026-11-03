// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package redactderived

// Postgres-engine contract for the derived-store re-sweep (redact-v5 S2a:
// the hub store is CNPG, so the #616-style re-sweep needs this engine).
// Runs only with MEMOIRE_TEST_PG_URL set — the repo's PG test convention
// (`make test-pg` brings up the local ParadeDB shadow via
// scripts/pg-shadow.sh). Each test creates and drops a throwaway C-collated
// database and applies schema-pg through pgshadow.Apply, the hub's own
// deployment step. The seed is fixtureStmts, shared with the SQLite
// fixture: the engine pair must produce the same report and the same
// redacted store.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"memoire/services/api/internal/derive"
	"memoire/services/api/internal/pgshadow"
	"memoire/services/api/internal/redact"
)

// newPGStore returns the DSN of a seeded throwaway database and an open
// handle on it for assertions.
func newPGStore(t *testing.T) (string, *sql.DB) {
	t.Helper()
	admin := os.Getenv("MEMOIRE_TEST_PG_URL")
	if admin == "" {
		t.Skip("MEMOIRE_TEST_PG_URL not set; postgres redact-derived leg skipped")
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("MEMOIRE_TEST_PG_URL unparseable: %v", err)
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	dbName := "memoire_redactderived_" + hex.EncodeToString(suffix[:])
	adminConn, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	// C collation: the store contract (D0249), same as the hub.
	if _, err := adminConn.Exec("CREATE DATABASE " + dbName + " TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
		_ = adminConn.Close()
		t.Fatalf("create fixture database (role needs CREATEDB): %v", err)
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
	t.Cleanup(func() { _ = pg.Close() })
	if err := pgshadow.Apply(context.Background(), pg, "../../../../schema-pg"); err != nil {
		t.Fatalf("apply schema-pg: %v", err)
	}
	for _, s := range fixtureStmts(t) {
		if _, err := pg.Exec(s); err != nil {
			t.Fatalf("pg fixture: %v\n%s", err, s)
		}
	}
	return dsn, pg
}

// targetRows renders every enrolled column (plus observations.content_hash)
// of every target table in id order: the engine-neutral store image the
// pair is compared on.
func targetRows(t *testing.T, conn *sql.DB, pg bool) string {
	t.Helper()
	var out strings.Builder
	for _, tg := range targets {
		cols := append([]string{}, Targets()[tg.table]...)
		if tg.table == "observations" {
			cols = append(cols, "content_hash")
		}
		exprs := make([]string, len(cols))
		for i, c := range cols {
			exprs[i] = "COALESCE(" + c + ", '<NULL>')"
		}
		q := "SELECT " + strings.Join(exprs, ", ") + " FROM " + tg.table + " ORDER BY " + tg.idCol
		rows, err := conn.Query(q)
		if err != nil {
			t.Fatalf("read %s: %v", tg.table, err)
		}
		for rows.Next() {
			vals := make([]string, len(cols))
			dest := make([]any, len(cols))
			for i := range vals {
				dest[i] = &vals[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			out.WriteString(tg.table + ": " + strings.Join(vals, " | ") + "\n")
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out.String()
}

func openSQLite(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// comparable strips the engine-specific field (the store locator).
func comparable(r Report) Report {
	r.DBPath = ""
	return r
}

func tsvCount(t *testing.T, pg *sql.DB, needle string) int {
	t.Helper()
	var n int
	if err := pg.QueryRow(`SELECT count(*) FROM observations WHERE text_tsv @@ plainto_tsquery('simple', $1)`, needle).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPGDryRunMatchesSQLiteAndDoesNotMutate(t *testing.T) {
	dsn, pg := newPGStore(t)
	kp := keyPath(t)
	sqlitePath := openFixture(t)

	before := targetRows(t, pg, true)
	pgReport, err := Run(context.Background(), Options{DBPath: dsn, RedactionKeyPath: kp})
	if err != nil {
		t.Fatalf("postgres dry-run: %v", err)
	}
	if pgReport.Applied {
		t.Fatal("dry-run must not report applied")
	}
	// The report names the store without its credentials: the hub DSN comes
	// from a secret and the report lands in logs and files.
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if want := u.Redacted(); pgReport.DBPath != want {
		t.Fatalf("report db_path = %q, want the password-redacted DSN %q", pgReport.DBPath, want)
	}
	if pw, ok := u.User.Password(); ok && strings.Contains(pgReport.DBPath, pw) {
		t.Fatal("report db_path carries the DSN password")
	}
	if got := targetRows(t, pg, true); got != before {
		t.Fatal("postgres dry-run mutated the store")
	}

	sqliteReport, err := Run(context.Background(), Options{DBPath: sqlitePath, RedactionKeyPath: kp})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(comparable(pgReport), comparable(sqliteReport)) {
		t.Fatalf("engine pair disagrees on the dry-run report:\npostgres: %+v\nsqlite:   %+v", pgReport, sqliteReport)
	}
	if pgReport.TotalChanged != 8 {
		t.Fatalf("postgres total_changed = %d, want 8 (same seed as the SQLite fixture)", pgReport.TotalChanged)
	}
}

func TestPGApplyMatchesSQLiteRedactsAndConverges(t *testing.T) {
	dsn, pg := newPGStore(t)
	kp := keyPath(t)
	sqlitePath := openFixture(t)

	if n := tsvCount(t, pg, "AKIA" + "ABCDEFGHIJKLMNOP"); n != 1 {
		t.Fatalf("fixture text_tsv must find the planted AWS key, got %d", n)
	}

	pgReport, err := Run(context.Background(), Options{DBPath: dsn, RedactionKeyPath: kp, Apply: true})
	if err != nil {
		t.Fatalf("postgres apply: %v", err)
	}
	sqliteReport, err := Run(context.Background(), Options{DBPath: sqlitePath, RedactionKeyPath: kp, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if !pgReport.Applied {
		t.Fatal("apply must report applied")
	}
	if !reflect.DeepEqual(comparable(pgReport), comparable(sqliteReport)) {
		t.Fatalf("engine pair disagrees on the apply report:\npostgres: %+v\nsqlite:   %+v", pgReport, sqliteReport)
	}

	// Same key, same rules, same seed: the redacted stores are identical,
	// markers and recomputed content hashes included.
	pgRows := targetRows(t, pg, true)
	sqliteRows := targetRows(t, openSQLite(t, sqlitePath), false)
	if pgRows != sqliteRows {
		t.Fatalf("engine pair disagrees on the redacted store:\npostgres:\n%s\nsqlite:\n%s", pgRows, sqliteRows)
	}
	for _, secret := range []string{
		"sk-" + "testfake123456789012345678",
		"sk-" + "testfakecwd123456789012345",
		"ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789",
		"ghp_" + "zyxwvutsrqponmlkjihgfedcba9876543210",
		"AKIA" + "ABCDEFGHIJKLMNOP",
		"glpat-faketoken1234567890abc",
		"faketokenabcdef123456",
		"faketestsecretvalue1",
		"xoxb-123456789012-fakeslack",
	} {
		if strings.Contains(pgRows, secret) {
			t.Fatalf("secret %q survived the postgres apply", secret)
		}
	}

	// content_hash tracks the redacted text on Postgres too.
	var newText, newHash string
	if err := pg.QueryRow(`SELECT text, content_hash FROM observations WHERE kind = 'task'`).Scan(&newText, &newHash); err != nil {
		t.Fatal(err)
	}
	wantHash, err := derive.CanonicalContentHash("memoire", "task", newText)
	if err != nil {
		t.Fatal(err)
	}
	if newHash != wantHash {
		t.Fatalf("content_hash not recomputed on postgres: got %s want %s", newHash, wantHash)
	}

	// The generated search column follows the rewritten text.
	if n := tsvCount(t, pg, "AKIA" + "ABCDEFGHIJKLMNOP"); n != 0 {
		t.Fatalf("text_tsv still finds the AWS key after apply: %d", n)
	}

	// Idempotent: the second pass converges to zero.
	again, err := Run(context.Background(), Options{DBPath: dsn, RedactionKeyPath: kp, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if again.TotalChanged != 0 {
		t.Fatalf("second postgres apply total_changed = %d, want 0", again.TotalChanged)
	}
}

// Identity-key path columns are never rewritten on Postgres either: a hit
// is reported by the dry-run and blocks apply with the store untouched.
func TestPGPreflightBlocksSecretShapedIdentityPaths(t *testing.T) {
	dsn, pg := newPGStore(t)
	kp := keyPath(t)
	if _, err := pg.Exec(`INSERT INTO project_identity_alerts(project, root, known_root, source, first_seen_epoch, last_seen_epoch)
		VALUES ('memoire', '/home/x/sk-` +
		`testfakeroot1234567890123/repo', '/home/x/dev/memoire', 'basename', 100, 100)`); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), Options{DBPath: dsn, RedactionKeyPath: kp})
	if err != nil {
		t.Fatal(err)
	}
	if got := report.PreflightHits["project_identity_alerts.root"]; got != 1 {
		t.Fatalf("dry-run must report the identity-path hit, got %v", report.PreflightHits)
	}

	before := targetRows(t, pg, true)
	_, err = Run(context.Background(), Options{DBPath: dsn, RedactionKeyPath: kp, Apply: true})
	if err == nil || !strings.Contains(err.Error(), "identity-key path") {
		t.Fatalf("apply must refuse on identity-path hits, got: %v", err)
	}
	if targetRows(t, pg, true) != before {
		t.Fatal("refused postgres apply mutated the store")
	}
}

// Apply is all-or-nothing: a failure on the LAST target table (audit_log)
// must leave every earlier table as it was.
func TestPGApplyIsAtomic(t *testing.T) {
	dsn, pg := newPGStore(t)
	kp := keyPath(t)
	for _, s := range []string{
		`CREATE FUNCTION redactderived_test_refuse() RETURNS trigger AS $$
		 BEGIN RAISE EXCEPTION 'test: audit_log update refused'; END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER redactderived_test_refuse BEFORE UPDATE ON audit_log
		 FOR EACH ROW EXECUTE FUNCTION redactderived_test_refuse()`,
	} {
		if _, err := pg.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if targets[len(targets)-1].table != "audit_log" {
		t.Fatalf("test premise: audit_log must be the last target, got %s", targets[len(targets)-1].table)
	}

	before := targetRows(t, pg, true)
	_, err := Run(context.Background(), Options{DBPath: dsn, RedactionKeyPath: kp, Apply: true})
	if err == nil || !strings.Contains(err.Error(), "audit_log update refused") {
		t.Fatalf("apply must surface the audit_log failure, got: %v", err)
	}
	if targetRows(t, pg, true) != before {
		t.Fatal("failed postgres apply left earlier tables rewritten (not atomic)")
	}
}

// Fail closed on Postgres: a payload nested past the depth limit aborts the
// run instead of being skipped or written unredacted.
func TestPGScanRefusesDepthError(t *testing.T) {
	dsn, pg := newPGStore(t)
	if _, err := pg.Exec(`UPDATE events SET payload_json = $1 WHERE idempotency_key = 'k2'`, deepDoc()); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Options{DBPath: dsn, RedactionKeyPath: keyPath(t)})
	if !errors.Is(err, redact.ErrDepth) {
		t.Fatalf("a depth-limit payload must fail the postgres run closed with ErrDepth, got %v", err)
	}
}

// The tool must not apply DDL or migrations to the hub: running it leaves
// the schema exactly as schema-pg made it.
func TestPGRunAppliesNoDDL(t *testing.T) {
	dsn, pg := newPGStore(t)
	schema := func() string {
		var s sql.NullString
		if err := pg.QueryRow(`SELECT string_agg(table_name || '.' || column_name || ':' || data_type, ',' ORDER BY table_name, column_name)
			FROM information_schema.columns WHERE table_schema = 'public'`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		var v sql.NullString
		if err := pg.QueryRow(`SELECT string_agg(version::text, ',' ORDER BY version) FROM schema_version`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return s.String + "|" + v.String
	}
	before := schema()
	if _, err := Run(context.Background(), Options{DBPath: dsn, RedactionKeyPath: keyPath(t), Apply: true}); err != nil {
		t.Fatal(err)
	}
	if schema() != before {
		t.Fatal("the run changed the postgres schema or its schema_version rows")
	}
}

// Enumeration tripwire on the Postgres schema (the hub's store): every
// public table is a redaction target or allowlisted as non-content, and
// every text column of a target table is enrolled or allowlisted. The
// allowlists are the SQLite test's (TestTargetsCoverAllTextColumns) plus
// the Postgres-only tables named below; a schema-pg migration adding a
// table or a text column fails here until its author decides.
func TestPGTargetsCoverAllTextColumns(t *testing.T) {
	_, pg := newPGStore(t)

	nonContentTables := map[string]bool{
		"credentials":               true,
		"schema_version":            true,
		"observation_source_events": true,
		"project_roots":             true,
		"project_identity_alerts":   true,
		"derive_intents":            true,
		"run_input_events":          true,
		// Vector lane (PG only): embedding + sha256(text) computed from the
		// observation's text. Not rewritable in place; the apply DELETES the
		// embedding of every observation whose text it changed, in the same
		// transaction, and the embed worker re-embeds the redacted text
		// (TestPGApplyInvalidatesEmbeddingsOfRewrittenObservations).
		"observation_embeddings_pg": true,
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
		"observation_triage_suggestions": {"prompt_version": true, "model": true, "actor": true,
			"suggested_verdict": true, "reason_genre": true, "arm": true},
		"audit_log": {"principal_id": true, "credential_id": true, "request_id": true,
			"action": true, "resource_type": true, "resource_id": true},
		"observation_usage": {"session_id": true, "project": true, "verdict": true},
	}

	for _, g := range PreflightGuards() {
		var n int
		if err := pg.QueryRow(`SELECT count(*) FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`, g[0], g[1]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("preflight guard %s.%s does not exist in schema-pg", g[0], g[1])
		}
	}

	enrolled := Targets()
	rows, err := pg.Query(`SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	var allTables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		allTables = append(allTables, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range allTables {
		if _, ok := enrolled[name]; ok {
			continue
		}
		if !nonContentTables[name] {
			t.Errorf("schema-pg table %s is neither a redaction target nor allowlisted as non-content", name)
		}
	}

	for table, cols := range enrolled {
		enrolledSet := map[string]bool{}
		for _, c := range cols {
			enrolledSet[c] = true
		}
		crows, err := pg.Query(`SELECT column_name, data_type FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1`, table)
		if err != nil {
			t.Fatal(err)
		}
		for crows.Next() {
			var name, typ string
			if err := crows.Scan(&name, &typ); err != nil {
				t.Fatal(err)
			}
			if typ != "text" && typ != "character varying" && typ != "json" && typ != "jsonb" {
				continue
			}
			if enrolledSet[name] {
				delete(enrolledSet, name)
				continue
			}
			if !nonContentColumns[table][name] {
				t.Errorf("%s.%s is a %s column in schema-pg neither enrolled nor allowlisted as non-content", table, name, typ)
			}
		}
		if err := crows.Close(); err != nil {
			t.Fatal(err)
		}
		for missing := range enrolledSet {
			t.Errorf("enrolled column %s.%s does not exist in schema-pg", table, missing)
		}
	}
}

// The vector lane holds an embedding and an unkeyed sha256 of the PRE-redaction
// text. Apply removes the embedding row of every observation whose text it
// rewrote (same transaction), keeps the others, and reports the count; the
// embed worker's missing-work query then re-embeds the redacted text.
func TestPGApplyInvalidatesEmbeddingsOfRewrittenObservations(t *testing.T) {
	dsn, pg := newPGStore(t)
	kp := keyPath(t)
	var hasTable bool
	if err := pg.QueryRow(`SELECT to_regclass('public.observation_embeddings_pg') IS NOT NULL`).Scan(&hasTable); err != nil {
		t.Fatal(err)
	}
	if !hasTable {
		t.Fatal("test premise: schema-pg apply on the ParadeDB shadow provisions observation_embeddings_pg (pgvector)")
	}
	// One clean observation that the sweep must leave alone, plus an
	// embedding for every observation.
	if _, err := pg.Exec(`INSERT INTO observations(project, session_id, kind, text, occurred_at_epoch, created_at_epoch, review_state, provenance)
		VALUES ('memoire', 's1', 'fact', 'nothing secret in this one', 100, 100, 'accepted', 'inferred')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`INSERT INTO observation_embeddings_pg(observation_id, model, dim, embedding, text_hash, embedded_at_epoch)
		SELECT id, 'test-model', 384, array_fill(0.1::real, ARRAY[384])::vector, 'stale-hash', 100 FROM observations`); err != nil {
		t.Fatal(err)
	}

	dry, err := Run(context.Background(), Options{DBPath: dsn, RedactionKeyPath: kp})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pg.QueryRow(`SELECT count(*) FROM observation_embeddings_pg`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("dry-run touched embeddings: %d rows left, want 4", n)
	}
	if got := dry.Tables["observations"].EmbeddingsInvalidated; got != 3 {
		t.Fatalf("dry-run embeddings_invalidated = %d, want 3 (the three rewritten observations)", got)
	}

	report, err := Run(context.Background(), Options{DBPath: dsn, RedactionKeyPath: kp, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Tables["observations"].EmbeddingsInvalidated; got != 3 {
		t.Fatalf("apply embeddings_invalidated = %d, want 3", got)
	}
	var left []string
	rows, err := pg.Query(`SELECT o.text FROM observation_embeddings_pg e JOIN observations o ON o.id = e.observation_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		left = append(left, s)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0] != "nothing secret in this one" {
		t.Fatalf("embeddings left after apply = %q, want only the untouched observation's", left)
	}
}
