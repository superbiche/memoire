// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

// PG-gated legs of the a20 = A work (2026-11-01): the v2 BM25 index that
// carries the read-filter columns (so pg_search pushes the observationWhere
// predicates into the Tantivy query instead of a heap_filter), detection of
// BOTH accepted definitions, and the explicit operator reindex command.
//
// Convention (same as enginepair_test.go / purge / redactderived): skipped
// unless MEMOIRE_TEST_PG_URL is set (the pg-shadow admin DSN); each test
// creates a throwaway C-collated database and drops it on cleanup. The
// pg_search extension is created per database when the server offers it;
// a non-paradedb shadow (no extension) skips, since these tests exercise
// the pg_search-specific behavior, not the tsvector fallback.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// bm25PGURL is conformancePGURL plus the hard pg_search precondition: the
// battery here cannot run against a server without the extension, so it
// skips there instead of failing (the conformance battery grades the
// fallback instead).
func bm25PGURL(t *testing.T) string {
	t.Helper()
	dsn, wantBM25 := conformancePGURL(t)
	if !wantBM25 {
		t.Skip("pg_search extension not available on this server; bm25 legs skipped")
	}
	return dsn
}

// bm25Provisioned opens the throwaway store (running the schema-pg
// migrations through the normal Open path) and provisions the BM25
// index the way pgshadow Apply does. The store is returned open for
// capability assertions.
func bm25Provisioned(t *testing.T, dsn string) *Store {
	t.Helper()
	boot, err := Open(dsn, schemaDirForTest(t))
	if err != nil {
		t.Fatalf("bootstrap open: %v", err)
	}
	_ = boot.Close()
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()
	if err := ProvisionPGSearchIndex(context.Background(), raw); err != nil {
		t.Fatalf("provision bm25 index: %v", err)
	}
	store, err := Open(dsn, schemaDirForTest(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// bm25CreateOldIndex plants the pre-swap (id, text) definition — the
// shape every live database had before a20 — on an already-migrated
// database, so the keep/resume/drop legs can start from the state the
// operator's hub actually has. concurrent=true is required when another
// bm25 index (v2) is already present: ParadeDB allows only one bm25
// index per table, and the documented way to add the second one is the
// CONCURRENTLY swap.
func bm25CreateOldIndex(t *testing.T, dsn string, concurrent bool) {
	t.Helper()
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()
	kind := ""
	if concurrent {
		kind = " CONCURRENTLY"
	}
	if _, err := raw.ExecContext(context.Background(),
		`CREATE INDEX`+kind+` `+bm25IndexOldName+` ON observations USING bm25 (id, text) WITH (key_field='id')`); err != nil {
		t.Fatalf("create old index: %v", err)
	}
}

// TestBM25ProvisionFreshCreatesV2: a fresh database (schema applied,
// no rows, no index) gets the v2 definition directly from the
// deployment step — never the old (id, text) shape.
func TestBM25ProvisionFreshCreatesV2(t *testing.T) {
	dsn := bm25PGURL(t)
	store := bm25Provisioned(t, dsn)
	if !store.HasPGSearch() {
		t.Fatalf("HasPGSearch: want true after provisioning v2 (engine=%s)", store.SearchEngine())
	}
	st, err := pgSearchIndexState(context.Background(), store.db)
	if err != nil {
		t.Fatal(err)
	}
	if !st.v2OK {
		t.Fatalf("fresh provisioning: want valid v2 index, got %+v", st)
	}
	if st.old {
		t.Fatalf("fresh provisioning: old index must not be created, got %+v", st)
	}
}

// TestBM25ProvisionKeepsOldIndex: a database that ALREADY holds the old
// definition is not rebuilt by apply/startup (a20 = A: a large build is
// an operator step, never a provisioning side-effect) — and detection
// still accepts it, so the API keeps serving BM25.
func TestBM25ProvisionKeepsOldIndex(t *testing.T) {
	dsn := bm25PGURL(t)
	// Open migrates (schema-pg) but never provisions — the state of
	// every pre-a20 database, before the old index was planted.
	openStore(t, dsn)
	bm25CreateOldIndex(t, dsn, false)
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}

	// Re-run the deployment step: it must be a no-op on the old index.
	if err := ProvisionPGSearchIndex(context.Background(), raw); err != nil {
		t.Fatalf("provision over old index: %v", err)
	}
	st, err := pgSearchIndexState(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !st.oldOK {
		t.Fatalf("old index was disturbed by provisioning: %+v", st)
	}
	if st.v2 {
		t.Fatalf("provisioning implicitly rebuilt the index (v2 appeared): %+v", st)
	}
	_ = raw.Close()

	// Detection accepts the old definition: the store still reports BM25.
	store := openStore(t, dsn)
	if !store.HasPGSearch() {
		t.Fatalf("HasPGSearch: want true for the old definition (engine=%s)", store.SearchEngine())
	}
}

// openStore opens the store without provisioning (Open never
// provisions; this is the plain capability-probe leg).
func openStore(t *testing.T, dsn string) *Store {
	t.Helper()
	store, err := Open(dsn, schemaDirForTest(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestBM25Reindex: the operator command on a database holding the old
// index: builds v2 CONCURRENTLY, verifies it, drops the old index; a
// second run is a no-op; a run with BOTH present resumes by dropping the
// old only (the v2 index OID must be untouched — no rebuild); dry-run
// changes nothing.
func TestBM25Reindex(t *testing.T) {
	dsn := bm25PGURL(t)
	// the operator's starting state: a migrated database holding only
	// the pre-a20 (id, text) index.
	openStore(t, dsn)
	bm25CreateOldIndex(t, dsn, false)
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	ctx := context.Background()

	// 1. dry-run first: prints the plan, changes nothing.
	report, err := ReindexBM25Index(ctx, raw, true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	for _, want := range []string{
		"CREATE INDEX CONCURRENTLY idx_observations_bm25_v2",
		"DROP INDEX CONCURRENTLY idx_observations_bm25",
		"dry-run: nothing executed",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("dry-run report missing %q:\n%s", want, report)
		}
	}
	st, err := pgSearchIndexState(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !st.oldOK || st.v2 {
		t.Fatalf("dry-run mutated state: %+v", st)
	}

	// 2. the real swap.
	if _, err := ReindexBM25Index(ctx, raw, false); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	st, err = pgSearchIndexState(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !st.v2OK {
		t.Fatalf("after reindex: want valid v2, got %+v", st)
	}
	if st.old {
		t.Fatalf("after reindex: old index must be gone, got %+v", st)
	}

	// 3. idempotence: a second run is a no-op.
	report, err = ReindexBM25Index(ctx, raw, false)
	if err != nil {
		t.Fatalf("reindex (second run): %v", err)
	}
	if !strings.Contains(report, "no-op") {
		t.Fatalf("second run should be a no-op:\n%s", report)
	}

	// 4. resume: v2 valid + old present again (an interrupted first run
	// that dropped nothing, or a manual re-plant) → drop old only, and
	// the v2 index OID is untouched (no rebuild of a multi-million-row
	// index for a resume).
	v2OID, err := indexOID(ctx, raw, bm25IndexV2Name)
	if err != nil {
		t.Fatal(err)
	}
	// v2 is already present, so the old index can only be re-planted
	// CONCURRENTLY (one bm25 index per table; the documented swap is the
	// only way to add the second one).
	bm25CreateOldIndex(t, dsn, true)
	if _, err := ReindexBM25Index(ctx, raw, false); err != nil {
		t.Fatalf("reindex (resume): %v", err)
	}
	st, err = pgSearchIndexState(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !st.v2OK || st.old {
		t.Fatalf("after resume: want v2 valid and old gone, got %+v", st)
	}
	again, err := indexOID(ctx, raw, bm25IndexV2Name)
	if err != nil {
		t.Fatal(err)
	}
	if again != v2OID {
		t.Fatalf("resume rebuilt the v2 index (oid %d -> %d); it must only drop the old one", v2OID, again)
	}
	_ = raw.Close()

	// 5. the store now reports BM25 through v2.
	store := openStore(t, dsn)
	if !store.HasPGSearch() {
		t.Fatalf("HasPGSearch after swap: want true (engine=%s)", store.SearchEngine())
	}
}

// indexOID reads the index relfilenode-independent OID so the resume
// assertion can tell "same index" from "rebuilt index".
func indexOID(ctx context.Context, conn *sql.DB, name string) (int, error) {
	var oid int
	err := conn.QueryRowContext(ctx, `SELECT oid FROM pg_class WHERE relname = $1`, name).Scan(&oid)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("index %s not found", name)
	}
	return oid, err
}

// bm25Seed inserts a small corpus that exercises every v2 column: raw
// accepted rows with and without a session / derivation run, an
// can_instruct row, and a noise row.
func bm25Seed(t *testing.T, dsn string) {
	t.Helper()
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()
	rows := []string{
		`(1, 'p1', 'sess-1', 'raw', 'hindsight word one body', 'manual', 'note', 1790000000, 1790000000, 'local', '{}', NULL, 'observed', 0, 0)`,
		`(2, 'p1', 'sess-2', 'raw', 'hindsight word two body', 'manual', 'note', 1790000100, 1790000100, 'local', '{}', NULL, 'observed', 0, 0)`,
		// can_instruct=1 requires provenance user_confirmed/imported
		// (the governance trigger); 'imported' is the cleanest.
		`(3, 'p1', 'sess-1', 'raw', 'hindsight word three body', 'manual', 'note', 1790000200, 1790000200, 'local', '{}', NULL, 'imported', 1, 0)`,
		`(4, 'p2', NULL, 'raw', 'hindsight word four body', 'manual', 'note', 1790000300, 1790000300, 'local', '{}', NULL, 'observed', 0, 0)`,
		`(5, 'p2', 'sess-2', 'raw', 'hindsight word five body', 'manual', 'note', 1790000400, 1790000400, 'local', '{}', NULL, 'observed', 0, 1)`,
		`(6, 'p1', 'sess-1', 'task', 'hindsight derivation six body', 'manual', 'note', 1790000500, 1790000500, 'local', '{}', 'drv_1', 'observed', 0, 0)`,
	}
	for _, r := range rows {
		if _, err := raw.ExecContext(context.Background(),
			`INSERT INTO observations(event_id, project, session_id, kind, text, source_adapter, event_type, occurred_at_epoch, created_at_epoch, principal_id, meta_json, derivation_run_id, provenance, can_instruct, noise_candidate) VALUES `+r); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

// bm25Explain mirrors the bm25 branch of searchFTS (store.go) — the |||
// match plus observationWhere — wrapped in EXPLAIN, so the test asserts
// the PLAN of the actual production shape rather than a re-derivation of
// it. The WHERE is built by the production observationWhere itself.
func bm25Explain(t *testing.T, dsn string, filter ObservationFilter) string {
	t.Helper()
	where, args := observationWhere(filter, "o")
	query := `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF)
		SELECT o.id, pdb.score(o.id)::float8
		FROM observations o
		WHERE o.text ||| ? AND ` + where + `
		ORDER BY pdb.score(o.id) DESC, o.id DESC
		LIMIT ?`
	args = append([]any{ftsToMatchText("hindsight body")}, args...)
	args = append(args, 20)
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()
	rows, err := raw.QueryContext(context.Background(), rebind(EnginePostgres, query), args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "ParadeDB Base Scan") {
		t.Fatalf("plan does not use the pg_search custom scan (is the index present?):\n%s", plan.String())
	}
	return plan.String()
}

// TestBM25IndexPushdown: with the v2 index, the default raw read shape
// (kind + the default review band + noise exclusion + the feed actor-gate
// anti-join) carries NO heap_filter — every pushable predicate is inside
// the Tantivy query, and the non-pushable anti-join is hoisted out of the
// custom scan by the planner instead of re-checking indexed columns row
// by row. The full-filter shape (session / derivation_run_id equality,
// the two NULLABLE literal columns, can_instruct, the occurred bound) is
// asserted the same way, with the session and can_instruct predicates
// additionally checked to be present in the Tantivy query (pushed, not
// merely absent from the heap). The negative control — the same default
// shape against the OLD (id, text) index — must show the heap_filter the
// swap exists to remove, or this test cannot tell a good index from a
// bad one.
func TestBM25IndexPushdown(t *testing.T) {
	dsn := bm25PGURL(t)
	store := bm25Provisioned(t, dsn)
	if !store.HasPGSearch() {
		t.Fatalf("precondition: v2 index must be usable (engine=%s)", store.SearchEngine())
	}
	bm25Seed(t, dsn)

	// 1. the default raw shape (what L0010 /v1/search and /v1/context
	// send when unscoped): forced kind='raw', the default band
	// (deleted IS NULL + review_state='accepted' + noise=0) and the
	// fail-closed feed actor-gate anti-join.
	plan := bm25Explain(t, dsn, ObservationFilter{Kind: "raw", Limit: 20})
	if strings.Contains(plan, "heap_filter") {
		t.Fatalf("default raw shape carries a heap_filter against the v2 index:\n%s", plan)
	}

	// 2. the full-filter shape: every indexed column exercised,
	// including the two nullable literal columns and can_instruct.
	full := ObservationFilter{
		Kind:               "raw",
		Project:            "p1",
		SessionID:          "sess-1",
		DerivationRunID:    "drv_1",
		OccurredAfterEpoch: 1790000000,
		CanInstruct:        true,
		ReviewState:        "accepted",
		Limit:              20,
	}
	plan = bm25Explain(t, dsn, full)
	if strings.Contains(plan, "heap_filter") {
		t.Fatalf("full-filter shape carries a heap_filter against the v2 index:\n%s", plan)
	}
	for _, want := range []string{
		`"field":"session_id"`,
		`"field":"derivation_run_id"`,
		`"field":"can_instruct"`,
	} {
		if !strings.Contains(plan, want) {
			t.Fatalf("expected %q inside the Tantivy query (pushed down):\n%s", want, plan)
		}
	}

	// 3. negative control: the same default shape against the OLD
	// (id, text) index must bring the heap_filter back — the 12-15 s
	// on the hub, in plan form.
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.ExecContext(context.Background(), "DROP INDEX "+bm25IndexV2Name); err != nil {
		t.Fatalf("drop v2 (negative control): %v", err)
	}
	bm25CreateOldIndex(t, dsn, false)
	_ = raw.Close()
	plan = bm25Explain(t, dsn, ObservationFilter{Kind: "raw", Limit: 20})
	if !strings.Contains(plan, "heap_filter") {
		t.Fatalf("old (id, text) index: expected the heap_filter the v2 swap removes:\n%s", plan)
	}
}

// bm25Pinned draws one session from a throwaway pool: the contended
// legs need explicit session identity (open transactions, the
// advisory-lock holder, a timeout-bounded build attempt), which a
// pooled Exec cannot give.
func bm25Pinned(t *testing.T, dsn string) *sql.Conn {
	t.Helper()
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	c, err := raw.Conn(context.Background())
	if err != nil {
		t.Fatalf("pin session: %v", err)
	}
	return c
}

// bm25PlantInvalidV2 leaves a genuinely invalid v2 index behind: a
// CONCURRENTLY build under a tiny statement_timeout racing a writer
// that holds an open transaction. The build's first phase waits for
// the open transaction to end, the statement bound expires, and the
// failed build leaves the invalid index in the catalog — the exact
// shape a Ctrl-C mid-build leaves (the leftover the recovery must
// clear). Works with or without the old index present: the
// CONCURRENTLY form is the only way to add a second bm25 index of the
// table, and also the only one that can fail mid-flight.
func bm25PlantInvalidV2(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	// The open writer: BEGIN + an uncommitted UPDATE — ROW EXCLUSIVE
	// on the table, no rows required to hold it.
	w := bm25Pinned(t, dsn)
	if _, err := w.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatalf("writer begin: %v", err)
	}
	if _, err := w.ExecContext(ctx, `UPDATE observations SET text = text`); err != nil {
		t.Fatalf("writer update: %v", err)
	}
	// The doomed build: 1 s statement bound, then the CONCURRENTLY
	// create that must wait for the writer's transaction.
	b := bm25Pinned(t, dsn)
	if _, err := b.ExecContext(ctx, `SET statement_timeout = 1000`); err != nil {
		t.Fatalf("build set statement_timeout: %v", err)
	}
	if _, err := b.ExecContext(ctx, bm25ReindexDDLCreate); err == nil {
		t.Fatalf("the build must time out behind the open writer (the invalid leftover is the point)")
	}
	if _, err := w.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatalf("writer commit: %v", err)
	}
	st, err := pgSearchIndexState(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if !st.v2 || st.v2OK {
		t.Fatalf("want the invalid v2 leftover, got %+v", st)
	}
}

// TestBM25InvalidV2RecoveryDoesNotBlockReaders (a20 = A, F1): the
// invalid-v2 leftover is cleared with DROP INDEX CONCURRENTLY, never a
// plain DROP INDEX. A plain drop queues an AccessExclusiveLock on
// observations; behind any open writer that pending exclusive request
// blocks EVERY new read (AccessShare requests queue behind it) for up
// to the run's lock_timeout — a 30-minute hub-wide read stall for a
// bookkeeping statement. The leg plants a genuinely failed CONCURRENTLY
// build, then runs the command's recovery while a writer keeps a
// transaction open: the drop may only wait out open transactions
// (a virtualxid/transactionid wait), never queue an AccessExclusive on
// the table, and a reader with a short statement_timeout must still
// get through.
func TestBM25InvalidV2RecoveryDoesNotBlockReaders(t *testing.T) {
	dsn := bm25PGURL(t)
	openStore(t, dsn)
	ctx := context.Background()
	bm25PlantInvalidV2(t, dsn)
	// A live row for the writer to hold and the reader to count (the
	// table is otherwise empty — a 0-row read is not the hub's read).
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO observations(event_id, project, session_id, kind, text, source_adapter, event_type, occurred_at_epoch, created_at_epoch, principal_id, meta_json, derivation_run_id, provenance, can_instruct, noise_candidate)
		 VALUES (NULL, 'p1', 'sess-1', 'raw', 'recovery reader leg body', 'manual', 'note', 1790000000, 1790000000, 'local', '{}', NULL, 'observed', 0, 0)`); err != nil {
		t.Fatalf("insert leg row: %v", err)
	}

	// The writer holds an open transaction (ROW EXCLUSIVE) through the
	// recovery — the condition behind which a plain DROP stalls every
	// reader.
	w := bm25Pinned(t, dsn)
	if _, err := w.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatalf("writer begin: %v", err)
	}
	if _, err := w.ExecContext(ctx, `UPDATE observations SET text = text`); err != nil {
		t.Fatalf("writer update: %v", err)
	}

	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	done := make(chan error, 1)
	go func() {
		if _, err := ReindexBM25Index(ctx, pool, false); err != nil {
			done <- err
			return
		}
		done <- nil
	}()

	// Wait until the recovery is demonstrably waiting on a lock, and
	// inspect what it is. The concurrent drop's wait rides a
	// virtualxid/transactionid request (it waits out open transactions,
	// never an exclusive table lock): those are the harmless kind — a
	// plain DROP's signature, by contrast, is an un-granted
	// AccessExclusive on the table relation, and THAT is what blocks
	// every new read behind an open writer.
	waiting := 0
	mode := ""
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if err := pool.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_locks
			  WHERE NOT granted
			    AND (relation = (SELECT oid FROM pg_class WHERE relname = 'observations' AND relkind = 'r')
			       OR locktype IN ('virtualxid', 'transactionid'))`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			// The table-relation mode, when there IS one: the
			// wait may be a pure virtualxid/transactionid wait (the
			// harmless kind), which has no table-relation row.
			var m any
			mrows, err := pool.QueryContext(ctx,
				`SELECT mode FROM pg_locks
				  WHERE NOT granted
				    AND relation = (SELECT oid FROM pg_class WHERE relname = 'observations' AND relkind = 'r')
				  LIMIT 1`)
			if err != nil {
				t.Fatal(err)
			}
			if mrows.Next() {
				if err := mrows.Scan(&m); err != nil {
					t.Fatal(err)
				}
				mode, _ = m.(string)
			}
			_ = mrows.Close()
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if waiting == 0 {
		// The drop may have landed before the first poll (an invalid
		// index on an idle table needs no wait at all): the pending-
		// request assertion does not apply, the reader leg below still
		// runs.
		st, err := pgSearchIndexState(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		if st.v2 && !st.v2OK {
			t.Fatalf("the recovery neither waits nor cleared the invalid v2: %+v", st)
		}
	} else if mode == "AccessExclusiveLock" {
		t.Fatalf("the invalid-v2 drop queued an AccessExclusive request on the table — the plain-DROP hub stall (pending mode %s)", mode)
	}

	// The reader with a short statement_timeout must still succeed:
	// an AccessShare read is compatible with both the open writer's
	// lock and the concurrent drop's; behind a plain DROP it would
	// queue behind the pending AccessExclusive until the bound fired
	// (57013) — the 30-minute stall in miniature.
	r := bm25Pinned(t, dsn)
	if _, err := r.ExecContext(ctx, `SET statement_timeout = 3000`); err != nil {
		t.Fatalf("reader set bound: %v", err)
	}
	var n int
	if err := r.QueryRowContext(ctx, `SELECT count(*) FROM observations`).Scan(&n); err != nil {
		t.Fatalf("reader blocked by the invalid-v2 drop (the plain-DROP stall): %v", err)
	}
	if n == 0 {
		t.Fatalf("the leg's row must be readable (count=%d)", n)
	}

	// Release the writer; the drop and the rebuild land, the run
	// completes.
	if _, err := w.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatalf("writer commit: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("recovery: %v", err)
	}
	st, err := pgSearchIndexState(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if !st.v2OK || st.old {
		t.Fatalf("after recovery: want valid v2 and no old index, got %+v", st)
	}
	_ = raw.Close()
}

// TestBM25ReindexSecondInvocationNeverDropsFreshV2 (a20 = A, F2): a
// QUEUED second invocation must not execute a stale plan. The pre-F2
// code derived its plan BEFORE taking the advisory lock: a second run
// that read "v2 invalid + old present" while a first run was still
// mid-swap would, once it got the lock, execute that stale "drop
// invalid v2" against the first run's freshly valid v2. The leg
// reproduces that exact window deterministically: plant the state a
// pre-lock read would plan from, hold the schema-applier advisory lock
// on a separate session (the first invocation, in flight), start the
// second (it queues on the lock), finish the swap while the lock is
// held (fresh valid v2, old gone), release — the second must
// re-derive a no-op and leave the fresh v2 untouched (same OID).
func TestBM25ReindexSecondInvocationNeverDropsFreshV2(t *testing.T) {
	dsn := bm25PGURL(t)
	openStore(t, dsn)
	ctx := context.Background()

	// 1. The first invocation's starting state: the old index present
	// and the invalid v2 beside it (the failed-build leftover) — the
	// plan a pre-lock read would derive is drop-invalid + rebuild +
	// drop-old.
	bm25CreateOldIndex(t, dsn, false)
	bm25PlantInvalidV2(t, dsn)

	// 2. A separate session takes the schema-applier advisory lock —
	// the first invocation, in flight — and holds it.
	holder := bm25Pinned(t, dsn)
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, pgMigrateLockKey); err != nil {
		t.Fatalf("holder lock: %v", err)
	}

	// 3. The second invocation: post-F2 it derives nothing before the
	// lock and must queue on it.
	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	type outcome struct {
		report string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		report, err := ReindexBM25Index(ctx, pool, false)
		done <- outcome{report, err}
	}()

	// 3b. Handshake (a20 = A, round-3 F5): prove the second invocation is
	// actually WAITING on the advisory lock before the fixture changes —
	// without it a second run that never queued (a race, or a regression
	// that skips the lock entirely) would finish before step 4 and the
	// no-op assertion would pass vacuously. pg_advisory_lock(bigint)
	// appears in pg_locks split into classid/objid halves (see
	// pgMigrateLockKey); the waiter's request is the ungranted one, the
	// holder's is granted.
	hi, lo := pgMigrateLockKey>>32, pgMigrateLockKey&0xFFFFFFFF
	waiting := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		var n int
		err := pool.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND classid = $1 AND objid = $2 AND NOT granted`, hi, lo).Scan(&n)
		if err != nil {
			t.Fatalf("pg_locks probe: %v", err)
		}
		if n > 0 {
			waiting = true
			break
		}
		select {
		case out := <-done:
			t.Fatalf("the second invocation finished (%v, %q) before ever waiting on the advisory lock — the test would pass vacuously", out.err, out.report)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !waiting {
		t.Fatal("timed out waiting for the second invocation's ungranted advisory lock request in pg_locks — the lock wait is unproven")
	}

	// 4. While the second is queued, the first finishes the swap: the
	// invalid v2 and the old index go, a fresh valid v2 lands (plain
	// DDL: with the two drops done this is the table's only bm25
	// index, so no CONCURRENTLY is needed; the second invocation
	// holds no table lock yet, so nothing contends).
	if _, err := holder.ExecContext(ctx, `DROP INDEX `+bm25IndexV2Name); err != nil {
		t.Fatalf("first run drops the invalid v2: %v", err)
	}
	if _, err := holder.ExecContext(ctx, `DROP INDEX `+bm25IndexOldName); err != nil {
		t.Fatalf("first run drops the old index: %v", err)
	}
	if _, err := holder.ExecContext(ctx, `CREATE INDEX `+bm25IndexV2Name+` ON observations USING bm25 `+pgSearchIndexV2Columns+` WITH (key_field='id')`); err != nil {
		t.Fatalf("first run builds the fresh v2: %v", err)
	}
	freshOID, err := indexOID(ctx, pool, bm25IndexV2Name)
	if err != nil {
		t.Fatal(err)
	}

	// 5. Release; the second proceeds and must re-derive a no-op.
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, pgMigrateLockKey); err != nil {
		t.Fatalf("holder unlock: %v", err)
	}
	out := <-done
	if out.err != nil {
		t.Fatalf("queued second invocation: %v", out.err)
	}
	if !strings.Contains(out.report, "no-op") {
		t.Fatalf("the second run executed a plan (the stale-plan drop): %s", out.report)
	}

	// 6. The fresh v2 survived, untouched: still valid, old gone, and
	// the SAME index OID — a stale "drop invalid v2" would have
	// dropped and rebuilt it.
	st, err := pgSearchIndexState(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if !st.v2OK || st.old {
		t.Fatalf("after the second run: want valid v2 and no old index, got %+v", st)
	}
	oid, err := indexOID(ctx, pool, bm25IndexV2Name)
	if err != nil {
		t.Fatal(err)
	}
	if oid != freshOID {
		t.Fatalf("the queued second invocation dropped and rebuilt the fresh v2 (oid %d -> %d) — it executed the stale plan", freshOID, oid)
	}
}

// TestBM25DefaultReadIsTopK (a20 = A, F3; shape restated by round-3 F4,
// one-statement since round-4 F6): the FULL production default raw read
// — the two-stage form (D257's actor-gate rides every shape, including
// kind='raw, so the bm25 branch applies it in the outer to keep the
// inner's v2 Top-K plan) — wrapped in EXPLAIN. The inner is the CTE `w`
// — one statement returns the gated hits AND the window's occupancy
// from the same snapshot (the CTE is materialized once and read by both
// the gate and the occupancy count; a separate probe would take a
// later snapshot, round-4 F6) — and must plan as a Top-K scan on v2
// with NO heap_filter (every pushable predicate in the Tantivy query).
// The gate (anti-join on observation_review_log) must sit in the ROOT
// tree, not inside the CTE: before the two-stage, the same NOT EXISTS in
// the inner's WHERE demoted this exact shape to a full-fetch scan plus
// anti-join (measured 489 ms unbounded / 521 ms 30-day at 1M synthetic
// vs 25.5 / 26.9 ms for the gate-free Top-K inner; docs/RUNBOOK.md BM25
// swap section). The gate applies on at most M rows
// (M = max(4k, 100) — the production window for k = 20), so its work is
// bounded by the window, not the candidate set. The
// 30-day bounded shape — what /v1/context's injection window sends — is
// asserted the same way, and the real two-stage read proves the snippet
// is computed in the inner query.
func TestBM25DefaultReadIsTopK(t *testing.T) {
	dsn := bm25PGURL(t)
	store := bm25Provisioned(t, dsn)
	if !store.HasPGSearch() {
		t.Fatalf("precondition: v2 index must be usable (engine=%s)", store.SearchEngine())
	}
	bm25Seed(t, dsn)
	ctx := context.Background()

	assertTopK := func(filter ObservationFilter, what string) {
		t.Helper()
		base, gate, args := observationWhereSplit(filter, "o")
		if gate == "" {
			t.Fatalf("%s: the default raw read must carry the actor-gate (D257 fail-closed)", what)
		}
		raw, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("open raw: %v", err)
		}
		defer raw.Close()
		query := `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) ` + bm25TwoStageQuerySQL(base)
		args = append([]any{ftsToMatchText("hindsight body")}, args...)
		args = append(args, 100, 20) // the production window M = max(4k, 100), k = 20
		rows, err := raw.QueryContext(ctx, rebind(EnginePostgres, query), args...)
		if err != nil {
			t.Fatalf("explain %s: %v", what, err)
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, line)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		plan := strings.Join(lines, "\n")
		if !strings.Contains(plan, "TopKScanExecState") {
			t.Fatalf("%s: the inner is not a Top-K scan (it must end at LIMIT inside the index):\n%s", what, plan)
		}
		if strings.Contains(plan, "heap_filter") {
			t.Fatalf("%s: carries a heap_filter against the v2 index (pushdown broken):\n%s", what, plan)
		}
		// The gate is in the production read (fail-closed: it rides every
		// shape until a deliberate ruling moves the predicate). One
		// statement: the CTE `w` is materialized once and both the gate
		// and the occupancy count read the same buffer (round-4 F6). The
		// gate must sit in the ROOT tree — a gate inside the CTE's own
		// definition is the F2 demotion — and the Top-K scan must be
		// inside the CTE (the inner is the window fetch).
		gateFound, gateInCTE, topkInCTE := false, false, false
		inCTE, cteIndent := false, 0
		for _, line := range lines {
			trimmed := strings.TrimLeft(line, " ")
			if strings.HasPrefix(trimmed, "CTE ") && !strings.Contains(trimmed, "Scan") {
				inCTE = true
				cteIndent = len(line) - len(trimmed)
				continue
			}
			if inCTE && len(line)-len(strings.TrimLeft(line, " ")) <= cteIndent {
				inCTE = false
			}
			if strings.Contains(line, "Anti Join") || strings.Contains(line, "SubPlan") {
				gateFound = true
				if inCTE {
					gateInCTE = true
				}
			}
			if inCTE && strings.Contains(line, "TopKScanExecState") {
				topkInCTE = true
			}
		}
		if !gateFound {
			t.Fatalf("%s: the feed actor-gate anti-join is not in the plan (the gate must ride this shape — D257):\n%s", what, plan)
		}
		if gateInCTE {
			t.Fatalf("%s: the anti-join is INSIDE the CTE (the inner is demoted — F2 regression):\n%s", what, plan)
		}
		if !topkInCTE {
			t.Fatalf("%s: the CTE (the inner) is not the Top-K scan:\n%s", what, plan)
		}
	}

	assertTopK(ObservationFilter{Kind: "raw", Limit: 20}, "unscoped default raw read")
	assertTopK(ObservationFilter{Kind: "raw", OccurredAfterEpoch: time.Now().Unix() - 30*86400, Limit: 20}, "30-day bounded (the /v1/context injection shape)")

	// The snippet is computed in the inner query: the real two-stage read
	// must return seed hits with a non-empty [/]-bracket snippet.
	hits, err := store.SearchWithFilter(ctx, "hindsight body", ObservationFilter{Kind: "raw", Limit: 20})
	if err != nil {
		t.Fatalf("two-stage read: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("two-stage read: want seed hits, got none")
	}
	for _, h := range hits {
		if h.Snippet == "" || !strings.Contains(h.Snippet, "[") {
			t.Fatalf("two-stage read: the snippet is not computed in the inner query (hit %d: %q)", h.ID, h.Snippet)
		}
	}
}

// TestBM25FalsifierRawAgentAcceptExcluded (a20 = A, round-3 F4): the
// reviewer's falsifier — a SCHEMA-VALID raw observation carrying an
// `agent:*` accept review-log entry, planted by direct SQL. No CURRENT
// API path writes this pair (transitions on raw are raw-locked since
// governance v1, elevation since 6f7228e), but legacy corpora can hold
// it — the pgshadow copy carries rows and review-log actors unchecked
// (both the 0007 provenance trigger and the raw elevation lock postdate
// the oldest corpora) — so D257's fail-closed predicate must not depend
// on the current API's invariants. D257's feed predicate is
// exclusion-shaped and fail-closed: until a deliberate ruling moves it,
// that row stays OUT of the default raw read on every shape it rides —
// the unbounded /v1/search read, the 30-day /v1/context injection shape,
// and the timeline (all three funnel through this store's search; the
// handler layer is a thin wrapper over exactly these calls). The
// control (include_agent_accepted) proves the row is otherwise fully
// eligible — the exclusion is the gate, not the seed.
func TestBM25FalsifierRawAgentAcceptExcluded(t *testing.T) {
	dsn := bm25PGURL(t)
	store := bm25Provisioned(t, dsn)
	if !store.HasPGSearch() {
		t.Fatalf("precondition: v2 index must be usable (engine=%s)", store.SearchEngine())
	}
	ctx := context.Background()
	now := time.Now().Unix()

	// The raw row, through the real ingest path (event -> raw projection:
	// kind='raw', review_state='accepted', provenance='observed', noise 0).
	ev, err := store.Ingest(ctx, EventInput{
		Project:         "bm25-falsifier",
		SessionID:       "falsifier-session",
		SourceAdapter:   "claude-code",
		SourceEventID:   "falsifier-evt-1",
		EventType:       "claude_UserPromptSubmit",
		OccurredAtEpoch: now - 60,
		Payload:         json.RawMessage(`{"prompt":"the falsifier lighthouse burned"}`),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	var rawID int64
	if err := store.rdb.QueryRowContext(ctx, "SELECT id FROM observations WHERE event_id = ?", ev.ID).Scan(&rawID); err != nil {
		t.Fatalf("raw observation of the seed event: %v", err)
	}
	// The falsifying log row — direct SQL through the writer handle
	// (the state no API path writes).
	if _, err := store.db.ExecContext(ctx, `INSERT INTO observation_review_log
		(observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch)
		VALUES (?, 'proposed', 'accepted', 'agent:conf-falsifier', 'schema-valid counterexample', ?)`,
		rawID, now-50); err != nil {
		t.Fatalf("falsifier log row: %v", err)
	}

	shapes := map[string]func() ([]SearchHit, error){
		"search (unbounded default raw read)": func() ([]SearchHit, error) {
			return store.SearchWithFilter(ctx, "falsifier lighthouse", ObservationFilter{Project: "bm25-falsifier", Kind: "raw", Limit: 20})
		},
		"context (30-day /v1/context shape)": func() ([]SearchHit, error) {
			return store.SearchWithFilter(ctx, "falsifier lighthouse", ObservationFilter{Project: "bm25-falsifier", Kind: "raw", OccurredAfterEpoch: now - 30*86400, Limit: 20})
		},
		"timeline": func() ([]SearchHit, error) {
			return store.TimelineWithFilter(ctx, ObservationFilter{Project: "bm25-falsifier", Kind: "raw", Limit: 20})
		},
	}
	for name, read := range shapes {
		hits, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, h := range hits {
			if h.ID == rawID {
				t.Fatalf("%s: the agent-accepted raw row is visible — D257 fail-closed broken on %s: %+v", name, store.SearchEngine(), h)
			}
		}
	}

	// Control: the same row is fully eligible once the gate is opted out —
	// the exclusion is the predicate, not the seed.
	hits, err := store.SearchWithFilter(ctx, "falsifier lighthouse", ObservationFilter{Project: "bm25-falsifier", Kind: "raw", IncludeAgentAccepted: true, Limit: 20})
	if err != nil {
		t.Fatalf("control read: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.ID == rawID {
			found = true
		}
	}
	if !found {
		t.Fatal("control: the row must be visible with include_agent_accepted=true (it is otherwise eligible) — the exclusion is not the gate")
	}
}

// TestBM25TwoStageExactness (a20 = A, round-3 F4): the two-stage read must
// return EXACTLY the same k rows, in the same order, as the single-query
// form (the gate in the inner's WHERE). The seed makes the claim
// non-vacuous: 300 rows with IDENTICAL text carry identical bm25 scores,
// so the (score DESC, id DESC) total order degenerates to id DESC — and
// the 100 HIGHEST ids (the head of that order) carry `agent:*` accept
// review-log entries, so the first window (M = max(4k, 100) = 100 at
// k = 20) under-fills on the first pass and the retry must grow it past
// the excluded head before k eligible rows can sit in a window.
func TestBM25TwoStageExactness(t *testing.T) {
	dsn := bm25PGURL(t)
	store := bm25Provisioned(t, dsn)
	if !store.HasPGSearch() {
		t.Fatalf("precondition: v2 index must be usable (engine=%s)", store.SearchEngine())
	}
	ctx := context.Background()
	const (
		project  = "bm25-exactness"
		needle   = "bmx25exactness"
		rows     = 300
		excluded = 100
		k        = 20
	)
	now := time.Now().Unix()

	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()

	// 300 identical-text rows (ids ascending as inserted).
	vals, args := []string{}, []any{}
	for i := 0; i < rows; i++ {
		vals = append(vals, "(?, 'raw', ?, ?, ?, 'accepted', 'observed')")
		args = append(args, project, "the bmx25exactness needle holds", now-int64(i), now-int64(i))
	}
	q := `INSERT INTO observations (project, kind, text, occurred_at_epoch, created_at_epoch, review_state, provenance) VALUES ` +
		strings.Join(vals, ", ") + ` RETURNING id`
	ins, err := raw.QueryContext(ctx, rebind(EnginePostgres, q), args...)
	if err != nil {
		t.Fatalf("seed 300: %v", err)
	}
	defer ins.Close()
	var ids []int64
	for ins.Next() {
		var id int64
		if err := ins.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := ins.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != rows {
		t.Fatalf("seed 300: want %d ids, got %d", rows, len(ids))
	}

	// The 100 highest ids (the head of the total order) are agent-accepted.
	vals2, args2 := []string{}, []any{}
	for i := 0; i < excluded; i++ {
		vals2 = append(vals2, "(?, 'proposed', 'accepted', 'agent:bm25-exact', 'exactness', ?)")
		args2 = append(args2, ids[rows-1-i], now-int64(i))
	}
	q2 := `INSERT INTO observation_review_log (observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch) VALUES ` +
		strings.Join(vals2, ", ")
	if _, err := raw.ExecContext(ctx, rebind(EnginePostgres, q2), args2...); err != nil {
		t.Fatalf("seed agent-accepts: %v", err)
	}

	// The production read: the two-stage path (the gate rides kind=raw).
	got, err := store.SearchWithFilter(ctx, needle, ObservationFilter{Project: project, Kind: "raw", Limit: k})
	if err != nil {
		t.Fatalf("two-stage read: %v", err)
	}
	// The single-query form — the pre-a20 shape — is the exactness oracle.
	where, wargs := observationWhere(ObservationFilter{Project: project, Kind: "raw", Limit: k}, "o")
	wargs = append([]any{ftsToMatchText(needle)}, wargs...)
	wargs = append(wargs, k)
	wantRows, err := raw.QueryContext(ctx, rebind(EnginePostgres, bm25SearchHeadPG+where+bm25SearchTailPG), wargs...)
	if err != nil {
		t.Fatalf("single-query read: %v", err)
	}
	defer wantRows.Close()
	var want []SearchHit
	for wantRows.Next() {
		var h SearchHit
		if err := wantRows.Scan(&h.ID, &h.SessionID, &h.Project, &h.Kind, &h.ReviewState, &h.SourceAdapter, &h.EventType, &h.Text, &h.Snippet, &h.OccurredAtEpoch, &h.Rank, &h.NoiseCandidate, &h.CanInstruct, &h.Provenance, &h.DerivationRunID); err != nil {
			t.Fatal(err)
		}
		want = append(want, h)
	}
	if err := wantRows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(got) != k {
		t.Fatalf("two-stage: want %d hits, got %d", k, len(got))
	}
	if len(got) != len(want) {
		t.Fatalf("exactness: two-stage %d hits vs single-query %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Fatalf("exactness: position %d: two-stage %d vs single-query %d (the two-stage must return the same k rows in the same order)", i, got[i].ID, want[i].ID)
		}
	}
	// And the excluded head is truly out of the result: the top returned id
	// is the highest ELIGIBLE id (the row just below the excluded head).
	if got[0].ID != ids[rows-excluded-1] {
		t.Fatalf("exactness: top hit is %d, want the highest eligible id %d (the excluded head must be gone)", got[0].ID, ids[rows-excluded-1])
	}
}

// TestBM25TwoStageOccupancyIsSingleStatement (a20 = A, round-4 F6
// regression): the underfill decision must use the occupancy the
// statement itself captured, not a separate probe. Round 3 took the
// occupancy from a second statement on a later READ COMMITTED snapshot:
// a concurrent delete/purge between the two could certify an
// underfilled window the read had seen full, and the read would return
// short (the reviewer's counterexample: 101 tied candidates, the 100
// excluded filling the first window, the one eligible row just past it,
// and two excluded rows deleted between the read and the probe → the
// probe sees 99 < 100 → empty, though the eligible row was eligible in
// both snapshots). This test plants exactly that fixture and fires the
// bm25TwoStageAfterWindow hook — called by the production code with the
// statement's own occupancy — to perform the deletion between the
// window read and the decision. A design that re-introduces the
// separate probe would see the deletion (its probe is a later snapshot)
// and return empty; the one-statement form must return the one eligible
// row.
func TestBM25TwoStageOccupancyIsSingleStatement(t *testing.T) {
	dsn := bm25PGURL(t)
	store := bm25Provisioned(t, dsn)
	if !store.HasPGSearch() {
		t.Fatalf("precondition: v2 index must be usable (engine=%s)", store.SearchEngine())
	}
	ctx := context.Background()
	const (
		project = "bm25-occupancy"
		needle  = "bmx25occupancy"
		rows    = 101
		k       = 20
	)
	now := time.Now().Unix()

	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()

	// 101 identical-text rows: identical bm25 scores, so the (score
	// DESC, id DESC) total order degenerates to id DESC. The 100
	// HIGHEST ids (exactly the first window at k = 20: M = max(4k, 100)
	// = 100) carry agent-accept review-log entries; the lowest id is
	// the only eligible row — just outside the first window.
	vals, args := []string{}, []any{}
	for i := 0; i < rows; i++ {
		vals = append(vals, "(?, 'raw', ?, ?, ?, 'accepted', 'observed')")
		args = append(args, project, "the bmx25occupancy needle holds", now-int64(i), now-int64(i))
	}
	q := `INSERT INTO observations (project, kind, text, occurred_at_epoch, created_at_epoch, review_state, provenance) VALUES ` +
		strings.Join(vals, ", ") + ` RETURNING id`
	ins, err := raw.QueryContext(ctx, rebind(EnginePostgres, q), args...)
	if err != nil {
		t.Fatalf("seed 101: %v", err)
	}
	defer ins.Close()
	var ids []int64
	for ins.Next() {
		var id int64
		if err := ins.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := ins.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != rows {
		t.Fatalf("seed 101: want %d ids, got %d", rows, len(ids))
	}

	vals2, args2 := []string{}, []any{}
	for i := 1; i < rows; i++ {
		vals2 = append(vals2, "(?, 'proposed', 'accepted', 'agent:bm25-occ', 'occupancy', ?)")
		args2 = append(args2, ids[i], now-int64(i))
	}
	q2 := `INSERT INTO observation_review_log (observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch) VALUES ` +
		strings.Join(vals2, ", ")
	if _, err := raw.ExecContext(ctx, rebind(EnginePostgres, q2), args2...); err != nil {
		t.Fatalf("seed agent-accepts: %v", err)
	}

	// The reviewer's mutation: delete two EXCLUDED rows between the
	// first window's read and the underfill decision.
	rawID := ids[0]
	deleteIDs := []int64{ids[rows-1], ids[rows-2]}
	fired := 0
	windows := []int{}
	bm25TwoStageAfterWindow = func(ctx context.Context, s *Store, m int, n int) {
		fired++
		windows = append(windows, m)
		if fired == 1 && n == m { // first window, full — the pre-mutation state
			if _, err := s.db.ExecContext(ctx, "DELETE FROM observations WHERE id IN (?, ?)", deleteIDs[0], deleteIDs[1]); err != nil {
				t.Errorf("inject delete: %v", err)
			}
		}
	}
	t.Cleanup(func() { bm25TwoStageAfterWindow = nil })

	got, err := store.SearchWithFilter(ctx, needle, ObservationFilter{Project: project, Kind: "raw", Limit: k})
	if err != nil {
		t.Fatalf("two-stage read: %v", err)
	}
	if len(got) != 1 || got[0].ID != rawID {
		t.Fatalf("want the single eligible row %d (the decision must use the statement's own occupancy — a later-snapshot probe would see the two deletions, certify the window underfilled, and return empty), got %+v", rawID, got)
	}
	if len(windows) != 2 || windows[0] != 100 {
		t.Fatalf("expected two window reads starting at 100 (the grown window second), hook saw %v", windows)
	}
}

// TestBM25TwoStageCapClampsAndFallsBack (a20 = A, round-4 F7): the
// window growth is clamped to bm25TwoStageWindowCap — no window larger
// than the cap is ever executed, and at the cap the read falls back to
// the single-query form (exact, the pre-a20 plan). Round 3's loop
// checked the cap only after executing the grown window, so k = 200
// could run a 204800-row window before falling back. The seed puts the
// first eligible row at rank 201 (the top 200 of 300 are excluded): a
// window ≤ 120 can never reach it, so the ONLY path to k eligible
// rows is the fallback — and the hook records the window sizes to
// prove the clamp (100 → 120, not the unclamped 100 → 400, which would
// reach rank 201 and skip the fallback).
func TestBM25TwoStageCapClampsAndFallsBack(t *testing.T) {
	dsn := bm25PGURL(t)
	store := bm25Provisioned(t, dsn)
	if !store.HasPGSearch() {
		t.Fatalf("precondition: v2 index must be usable (engine=%s)", store.SearchEngine())
	}
	ctx := context.Background()
	const (
		project  = "bm25-cap"
		needle   = "bmx25cap"
		rows     = 300
		excluded = 200 // the eligible block starts at rank 201
		k        = 20
	)
	oldCap := bm25TwoStageWindowCap
	bm25TwoStageWindowCap = 120 // 100 → 120 (clamped from 400) → fallback
	t.Cleanup(func() { bm25TwoStageWindowCap = oldCap })
	now := time.Now().Unix()

	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()

	vals, args := []string{}, []any{}
	for i := 0; i < rows; i++ {
		vals = append(vals, "(?, 'raw', ?, ?, ?, 'accepted', 'observed')")
		args = append(args, project, "the bmx25cap needle holds", now-int64(i), now-int64(i))
	}
	q := `INSERT INTO observations (project, kind, text, occurred_at_epoch, created_at_epoch, review_state, provenance) VALUES ` +
		strings.Join(vals, ", ") + ` RETURNING id`
	ins, err := raw.QueryContext(ctx, rebind(EnginePostgres, q), args...)
	if err != nil {
		t.Fatalf("seed 300: %v", err)
	}
	defer ins.Close()
	var ids []int64
	for ins.Next() {
		var id int64
		if err := ins.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := ins.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != rows {
		t.Fatalf("seed 300: want %d ids, got %d", rows, len(ids))
	}

	vals2, args2 := []string{}, []any{}
	for i := 0; i < excluded; i++ {
		vals2 = append(vals2, "(?, 'proposed', 'accepted', 'agent:bm25-cap', 'cap', ?)")
		args2 = append(args2, ids[rows-1-i], now-int64(i))
	}
	q2 := `INSERT INTO observation_review_log (observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch) VALUES ` +
		strings.Join(vals2, ", ")
	if _, err := raw.ExecContext(ctx, rebind(EnginePostgres, q2), args2...); err != nil {
		t.Fatalf("seed agent-accepts: %v", err)
	}

	windows := []int{}
	bm25TwoStageAfterWindow = func(ctx context.Context, s *Store, m int, n int) {
		windows = append(windows, m)
	}
	t.Cleanup(func() { bm25TwoStageAfterWindow = nil })

	got, err := store.SearchWithFilter(ctx, needle, ObservationFilter{Project: project, Kind: "raw", Limit: k})
	if err != nil {
		t.Fatalf("two-stage read: %v", err)
	}

	// The clamp: windows run 100 → 120 (min(4·100, 120)); no window
	// beyond the cap is ever executed.
	if len(windows) != 2 || windows[0] != 100 || windows[1] != 120 {
		t.Fatalf("window growth: want [100 120] (clamped to the cap, then the fallback), got %v", windows)
	}

	// The answer comes from the single-query fallback and equals it:
	// no window ≤ 120 reaches the eligible block (rank 201), so only
	// the fallback can produce k hits.
	where, wargs := observationWhere(ObservationFilter{Project: project, Kind: "raw", Limit: k}, "o")
	wargs = append([]any{ftsToMatchText(needle)}, wargs...)
	wargs = append(wargs, k)
	wantRows, err := raw.QueryContext(ctx, rebind(EnginePostgres, bm25SearchHeadPG+where+bm25SearchTailPG), wargs...)
	if err != nil {
		t.Fatalf("single-query read: %v", err)
	}
	defer wantRows.Close()
	var want []SearchHit
	for wantRows.Next() {
		var h SearchHit
		if err := wantRows.Scan(&h.ID, &h.SessionID, &h.Project, &h.Kind, &h.ReviewState, &h.SourceAdapter, &h.EventType, &h.Text, &h.Snippet, &h.OccurredAtEpoch, &h.Rank, &h.NoiseCandidate, &h.CanInstruct, &h.Provenance, &h.DerivationRunID); err != nil {
			t.Fatal(err)
		}
		want = append(want, h)
	}
	if err := wantRows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != k || len(want) != k {
		t.Fatalf("fallback: want %d hits, got %d (single-query %d)", k, len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Fatalf("fallback: position %d: %d vs single-query %d (the fallback must equal the single-query form)", i, got[i].ID, want[i].ID)
		}
	}
	if got[0].ID != ids[rows-excluded-1] {
		t.Fatalf("top hit is %d, want the highest eligible id %d (rank 201 — unreachable by any window ≤ 120)", got[0].ID, ids[rows-excluded-1])
	}
}
