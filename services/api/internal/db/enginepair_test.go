// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// enginepair_test is the dialect-seam conformance battery (memoire:D157):
// the SAME behavioral assertions run against both engines. The SQLite leg
// always runs (hermetic, temp file). The Postgres leg runs only when
// MEMOIRE_TEST_PG_URL names an ADMIN DSN (a role with CREATEDB — the local
// pg-shadow.sh container's superuser); the test creates a throwaway
// database, runs the battery, and drops it. Pointing this at a production
// role without CREATEDB fails loudly before any write.
//
// This battery checks BEHAVIOR parity (dedup, RETURNING ids, governance
// triggers, review transitions, cursor pagination, health counts, JSON
// predicates). DATA parity on the real corpus is the pgshadow harness's
// job; ranking deltas are measured there, not asserted here.

// conformancePGURL creates a throwaway database and returns its DSN plus
// whether pg_search should be active there. CREATE DATABASE clones
// template1, which does NOT carry extensions (round-1 finding 2: the old
// version silently ran the tsvector fallback while claiming pg_search
// coverage) — so the extension is created HERE, in the throwaway database
// itself, whenever the server offers it.
func conformancePGURL(t *testing.T) (string, bool) {
	t.Helper()
	admin := os.Getenv("MEMOIRE_TEST_PG_URL")
	if admin == "" {
		t.Skip("MEMOIRE_TEST_PG_URL not set; postgres conformance leg skipped")
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("MEMOIRE_TEST_PG_URL: %v", err)
	}

	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	dbName := "memoire_conf_" + hex.EncodeToString(suffix[:])

	adminConn, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	if _, err := adminConn.Exec("CREATE DATABASE " + dbName); err != nil {
		_ = adminConn.Close()
		t.Fatalf("create conformance database (role needs CREATEDB; use the pg-shadow admin DSN): %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminConn.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
		_ = adminConn.Close()
	})

	u.Path = "/" + dbName
	scratch, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open scratch db: %v", err)
	}
	defer scratch.Close()
	var avail int
	if err := scratch.QueryRow(`SELECT count(*) FROM pg_available_extensions WHERE name = 'pg_search'`).Scan(&avail); err != nil {
		t.Fatalf("probe pg_search availability: %v", err)
	}
	wantBM25 := avail > 0
	if wantBM25 {
		if _, err := scratch.Exec(`CREATE EXTENSION IF NOT EXISTS pg_search`); err != nil {
			t.Fatalf("create pg_search in conformance database: %v", err)
		}
	}
	return u.String(), wantBM25
}

func schemaDirForTest(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestEnginePairConformance(t *testing.T) {
	engines := []struct {
		name string
		open func(t *testing.T) *Store
	}{
		{"sqlite", func(t *testing.T) *Store {
			store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), schemaDirForTest(t))
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
		{"postgres", func(t *testing.T) *Store {
			// Deployment order mirrored (round-1 finding 2): first Open runs
			// the migrations (tables exist), then the deployment step
			// provisions the BM25 index, then the store opens for real and
			// MUST report the expected capability — a silent fallback fails
			// the battery instead of masquerading as coverage.
			dsn, wantBM25 := conformancePGURL(t)
			boot, err := Open(dsn, schemaDirForTest(t))
			if err != nil {
				t.Fatalf("bootstrap open postgres: %v", err)
			}
			_ = boot.Close()
			raw, err := sql.Open("pgx", dsn)
			if err != nil {
				t.Fatalf("open raw for provisioning: %v", err)
			}
			if err := ProvisionPGSearchIndex(context.Background(), raw); err != nil {
				_ = raw.Close()
				t.Fatalf("provision bm25 index: %v", err)
			}
			_ = raw.Close()
			store, err := Open(dsn, schemaDirForTest(t))
			if err != nil {
				t.Fatalf("open postgres: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if store.HasPGSearch() != wantBM25 {
				t.Fatalf("pg_search capability: want %v, got %v (engine=%s)", wantBM25, store.HasPGSearch(), store.SearchEngine())
			}
			return store
		}},
	}
	for _, eng := range engines {
		t.Run(eng.name, func(t *testing.T) {
			runConformanceBattery(t, eng.open(t))
		})
	}
}

func runConformanceBattery(t *testing.T, store *Store) {
	ctx := context.Background()
	now := time.Now().Unix()
	project := "conformance"
	session := "conf-session-1"

	// --- Ingest: insert, dedup, raw projection, audit ---
	in := EventInput{
		Project:         project,
		SessionID:       session,
		SourceAdapter:   "claude-code",
		SourceEventID:   "conf-evt-1",
		EventType:       "claude_UserPromptSubmit",
		OccurredAtEpoch: now - 100,
		Payload:         json.RawMessage(`{"prompt":"the zebrafish crossed the quantum meadow"}`),
	}
	first, err := store.Ingest(ctx, in)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if first.Duplicate || first.ID == 0 {
		t.Fatalf("first ingest: want fresh row with id, got %+v", first)
	}
	dup, err := store.Ingest(ctx, in)
	if err != nil {
		t.Fatalf("dup ingest: %v", err)
	}
	if !dup.Duplicate || dup.ID != first.ID {
		t.Fatalf("dup ingest: want Duplicate with id %d, got %+v", first.ID, dup)
	}
	if _, err := store.GetEvent(ctx, first.ID); err != nil {
		t.Fatalf("get event: %v", err)
	}
	if n, err := store.AuditCount(ctx, "event.ingest"); err != nil || n != 1 {
		t.Fatalf("audit count after dedup: want 1, got %d err %v", n, err)
	}

	// --- Search: FTS/tsvector needle, snippet, project scoping ---
	hits, err := store.Search(ctx, "zebrafish quantum", project, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].Snippet == "" {
		t.Fatalf("search: want 1 hit with snippet, got %d %+v", len(hits), hits)
	}
	// Hit shape parity: every dialect's SELECT projects occurred_at under
	// both names (captured_at_epoch is the legacy alias of the same column)
	// and carries the provenance governance column. A raw capture
	// projection is provenance=observed.
	if hits[0].OccurredAtEpoch != now-100 || hits[0].CapturedAtEpoch != now-100 {
		t.Fatalf("search hit epochs: want occurred=captured=%d, got occurred=%d captured=%d",
			now-100, hits[0].OccurredAtEpoch, hits[0].CapturedAtEpoch)
	}
	if hits[0].Provenance != "observed" {
		t.Fatalf("search hit provenance: want observed, got %q", hits[0].Provenance)
	}
	if hits, err := store.Search(ctx, "zebrafish", "other-project", 10); err != nil || len(hits) != 0 {
		t.Fatalf("search scoping: want 0 hits in other project, got %d err %v", len(hits), err)
	}

	// --- Timeline ordering (desc by occurred, then id) ---
	for i, when := range []int64{now - 90, now - 80} {
		if _, err := store.Ingest(ctx, EventInput{
			Project: project, SessionID: session, SourceAdapter: "claude-code",
			SourceEventID: fmt.Sprintf("conf-evt-t%d", i), EventType: "claude_PostToolUse",
			OccurredAtEpoch: when,
			Payload:         json.RawMessage(fmt.Sprintf(`{"tool_name":"Read","agent_id":"agent-%d","seq":%d}`, i, i)),
		}); err != nil {
			t.Fatalf("timeline ingest %d: %v", i, err)
		}
	}
	tl, err := store.Timeline(ctx, project, 10)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if len(tl) != 3 {
		t.Fatalf("timeline: want 3 rows, got %d", len(tl))
	}
	for i := 1; i < len(tl); i++ {
		if tl[i-1].CapturedAtEpoch < tl[i].CapturedAtEpoch {
			t.Fatalf("timeline not descending at %d", i)
		}
	}
	for _, hit := range tl {
		if hit.OccurredAtEpoch == 0 || hit.OccurredAtEpoch != hit.CapturedAtEpoch {
			t.Fatalf("timeline hit %d: occurred_at_epoch %d must equal the captured_at_epoch alias %d and be non-zero",
				hit.ID, hit.OccurredAtEpoch, hit.CapturedAtEpoch)
		}
		if hit.Provenance != "observed" {
			t.Fatalf("timeline hit %d provenance: want observed, got %q", hit.ID, hit.Provenance)
		}
	}

	// --- Tool window with agent_id JSON predicate ---
	tw, err := store.ListToolEventsInWindow(ctx, ToolEventFilter{SessionID: session, AgentID: "agent-1"})
	if err != nil {
		t.Fatalf("tool window: %v", err)
	}
	if len(tw) != 1 || tw[0].AgentID != "agent-1" {
		t.Fatalf("tool window agent filter: want exactly agent-1's event, got %+v", tw)
	}

	// --- Derivation runs: mint, dedup, failed-retry CAS ---
	runIn := DerivationRunInput{
		Project: project, SessionID: session, Provider: "rules", DataBoundary: "local",
		DerivationVersion: "v1", InputHash: "conf-hash-1",
		WindowStartEpoch: now - 100, WindowEndEpoch: now,
	}
	run, err := store.EnsureDerivationRun(ctx, runIn)
	if err != nil {
		t.Fatalf("ensure run: %v", err)
	}
	if run.Duplicate {
		t.Fatalf("first run minted as duplicate: %+v", run)
	}
	runDup, err := store.EnsureDerivationRun(ctx, runIn)
	if err != nil || !runDup.Duplicate || runDup.ID != run.ID {
		t.Fatalf("run dedup: want duplicate of %s, got %+v err %v", run.ID, runDup, err)
	}
	// A duplicate carries the EXISTING run's status (memoire:D165 round-1
	// MED-3: intent bookkeeping must distinguish completed dedups).
	if runDup.Status != "pending" {
		t.Fatalf("run dedup status: want existing 'pending', got %q", runDup.Status)
	}
	if err := store.UpdateDerivationRunStatus(ctx, DerivationRunStatusUpdate{ID: run.ID, Status: "failed", CompletedAtEpoch: now}); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	retry, err := store.EnsureDerivationRun(ctx, runIn)
	if err != nil {
		t.Fatalf("failed retry: %v", err)
	}
	if retry.Duplicate || retry.ID != run.ID || retry.RetryCount != 1 {
		t.Fatalf("failed retry: want in-place retry #1 of %s, got %+v", run.ID, retry)
	}
	if err := store.UpdateDerivationRunStatus(ctx, DerivationRunStatusUpdate{ID: run.ID, Status: "completed", CompletedAtEpoch: now}); err != nil {
		t.Fatalf("complete run: %v", err)
	}
	if dupDone, err := store.EnsureDerivationRun(ctx, runIn); err != nil || !dupDone.Duplicate || dupDone.Status != "completed" {
		t.Fatalf("completed-run dedup must carry status completed: %+v err %v", dupDone, err)
	}

	// --- Derived observations: RETURNING id, accepted, proposed ---
	accepted, err := store.InsertDerivedObservation(ctx, DerivedObservationInput{
		Project: project, SessionID: session, Kind: "decision", Text: "conformance accepted decision",
		DerivationRunID: run.ID, ContentHash: "confhash-a", SourceEventIDs: []int64{first.ID},
		OccurredAtEpoch: now - 50,
	})
	if err != nil {
		t.Fatalf("insert accepted: %v", err)
	}
	if accepted.ID == 0 || accepted.ReviewState != "accepted" {
		t.Fatalf("accepted insert: got %+v", accepted)
	}
	proposed, err := store.InsertDerivedObservation(ctx, DerivedObservationInput{
		Project: project, SessionID: session, Kind: "fact", Text: "conformance proposed fact",
		DerivationRunID: run.ID, ContentHash: "confhash-p", SourceEventIDs: []int64{first.ID},
		ModelDerived: true, OccurredAtEpoch: now - 40,
	})
	if err != nil {
		t.Fatalf("insert proposed: %v", err)
	}
	if proposed.ReviewState != "proposed" {
		t.Fatalf("proposed insert: got state %q", proposed.ReviewState)
	}

	// --- Governance trigger parity: the DB itself must refuse ---
	_, err = store.db.ExecContext(ctx, `INSERT INTO observations(
		event_id, project, kind, text, occurred_at_epoch, created_at_epoch,
		review_state, provenance, can_instruct, can_evidence, needs_confirmation, noise_candidate
	) VALUES (NULL, ?, 'decision', 'trigger probe', ?, ?, 'accepted', 'inferred', 1, 1, 0, 0)`,
		project, now, now)
	if err == nil || !strings.Contains(err.Error(), "can_instruct requires provenance") {
		t.Fatalf("governance trigger: want can_instruct/provenance abort from the DB, got %v", err)
	}

	// --- Review transitions + elevation gate ---
	rec, err := store.TransitionObservationReview(ctx, proposed.ID, "accepted", "conf accept")
	if err != nil || rec.ReviewState != "accepted" {
		t.Fatalf("accept transition: %+v err %v", rec, err)
	}
	if _, err := store.ElevateObservation(ctx, proposed.ID, "premature"); !errors.Is(err, ErrElevationConflict) {
		t.Fatalf("elevate before confirm: want ErrElevationConflict, got %v", err)
	}
	if _, err := store.ConfirmObservation(ctx, accepted.ID, ""); err != nil {
		// accepted -> accepted is not a legal transition; confirm is valid from
		// proposed/disputed/lapsed. Use the proposed->accepted row instead.
		_ = err
	}
	// Confirm path on a fresh proposed row, then elevate.
	p2, err := store.InsertDerivedObservation(ctx, DerivedObservationInput{
		Project: project, SessionID: session, Kind: "constraint", Text: "conformance elevatable",
		DerivationRunID: run.ID, ContentHash: "confhash-e", SourceEventIDs: []int64{first.ID},
		ModelDerived: true, OccurredAtEpoch: now - 30,
	})
	if err != nil {
		t.Fatalf("insert elevatable: %v", err)
	}
	if _, err := store.ConfirmObservation(ctx, p2.ID, "conf confirm"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	elevated, err := store.ElevateObservation(ctx, p2.ID, "conf elevate")
	if err != nil || elevated.CanInstruct != 1 {
		t.Fatalf("elevate after confirm: %+v err %v", elevated, err)
	}

	// --- Rewrite: new id, supersede, copied source links ---
	p3, err := store.InsertDerivedObservation(ctx, DerivedObservationInput{
		Project: project, SessionID: session, Kind: "fact", Text: "rewrite me",
		DerivationRunID: run.ID, ContentHash: "confhash-r", SourceEventIDs: []int64{first.ID},
		ModelDerived: true, OccurredAtEpoch: now - 20,
	})
	if err != nil {
		t.Fatalf("insert rewritable: %v", err)
	}
	rewritten, err := store.RewriteObservation(ctx, p3.ID, "rewritten cleanly", "conf rewrite")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if rewritten.ID == p3.ID || rewritten.ReviewState != "accepted" || len(rewritten.SourceEventIDs) != 1 {
		t.Fatalf("rewrite result: %+v", rewritten)
	}

	// --- Cursor pagination over observations ---
	page1, cursor, err := store.ListObservations(ctx, ObservationFilter{Project: project, Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 || cursor == "" {
		t.Fatalf("page1: want 2 rows + cursor, got %d %q", len(page1), cursor)
	}
	page2, _, err := store.ListObservations(ctx, ObservationFilter{Project: project, Limit: 50, Cursor: cursor})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	for _, a := range page1 {
		for _, b := range page2 {
			if a.ID == b.ID {
				t.Fatalf("pagination overlap on id %d", a.ID)
			}
		}
	}
	if _, _, err := store.ListObservations(ctx, ObservationFilter{Project: project, Cursor: "not-a-cursor"}); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("bad cursor: want ErrBadCursor, got %v", err)
	}

	// --- Sessions surface (positive proposedOnly assertion, round-1 F5) ---
	sess, err := store.GetSession(ctx, session)
	if err != nil || sess.EventCount != 3 {
		t.Fatalf("get session: want 3 events, got %+v err %v", sess, err)
	}
	// A guaranteed-proposed row so proposedOnly MUST return the session.
	pending, err := store.InsertDerivedObservation(ctx, DerivedObservationInput{
		Project: project, SessionID: session, Kind: "fact", Text: "conformance pending review",
		DerivationRunID: run.ID, ContentHash: "confhash-pending", SourceEventIDs: []int64{first.ID},
		ModelDerived: true, OccurredAtEpoch: now - 15,
	})
	if err != nil || pending.ReviewState != "proposed" {
		t.Fatalf("insert pending: %+v err %v", pending, err)
	}
	// A lapsed row too, so the band and the count are both exercised
	// (round-2 finding 2: exact counts, not floors).
	lapsedSeed, err := store.InsertDerivedObservation(ctx, DerivedObservationInput{
		Project: project, SessionID: session, Kind: "fact", Text: "conformance lapsed fact",
		DerivationRunID: run.ID, ContentHash: "confhash-lapsed", SourceEventIDs: []int64{first.ID},
		ModelDerived: true, OccurredAtEpoch: now - 12,
	})
	if err != nil {
		t.Fatalf("insert lapsed seed: %v", err)
	}
	if _, err := store.TransitionObservationReview(ctx, lapsedSeed.ID, "lapsed", "conf lapse"); err != nil {
		t.Fatalf("lapse transition: %v", err)
	}
	summaries, err := store.ListSessions(ctx, project, true, 10)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(summaries) != 1 || summaries[0].SessionID != session {
		t.Fatalf("list sessions proposedOnly: want exactly [%s], got %+v", session, summaries)
	}
	// Exact state at this point: `pending` is the only proposed row (the
	// first ModelDerived insert was accepted, p2 confirmed, p3 superseded by
	// rewrite), lapsedSeed the only lapsed one and — at now-12 — the newest
	// non-deleted observation (the future-skew event is ingested later in
	// the battery).
	if summaries[0].ProposedCount != 1 || summaries[0].LapsedCount != 1 {
		t.Fatalf("list sessions exact counts: want proposed=1 lapsed=1, got %+v", summaries[0])
	}
	if summaries[0].LastObservedAtEpoch != now-12 {
		t.Fatalf("list sessions last_observed: want %d (lapsed seed), got %d", now-12, summaries[0].LastObservedAtEpoch)
	}
	// proposedOnly=false must also list it; a bogus project must list nothing.
	if all, err := store.ListSessions(ctx, project, false, 10); err != nil || len(all) != 1 {
		t.Fatalf("list sessions (all): want 1, got %d err %v", len(all), err)
	}
	if none, err := store.ListSessions(ctx, "no-such-project", true, 10); err != nil || len(none) != 0 {
		t.Fatalf("list sessions scoping: want 0, got %d err %v", len(none), err)
	}

	// --- Ascending order + REAL cursor continuation (Limit 1 pages) ---
	ascFilter := ObservationFilter{Project: project, Order: "asc", Limit: 50, IncludeProposed: true}
	asc, _, err := store.ListObservations(ctx, ascFilter)
	if err != nil {
		t.Fatalf("asc list: %v", err)
	}
	for i := 1; i < len(asc); i++ {
		if asc[i-1].OccurredAtEpoch > asc[i].OccurredAtEpoch {
			t.Fatalf("asc order violated at %d", i)
		}
	}
	sawProposed, sawLapsed := false, false
	for _, o := range asc {
		if o.ID == pending.ID {
			sawProposed = true
		}
		if o.ID == lapsedSeed.ID {
			sawLapsed = true
		}
	}
	if !sawProposed || !sawLapsed {
		t.Fatalf("IncludeProposed band: proposed seen=%v lapsed seen=%v", sawProposed, sawLapsed)
	}
	// Walk the whole set one row per page through the asc cursor predicate
	// and require exactly the un-paginated sequence.
	var walked []int64
	pageFilter := ascFilter
	pageFilter.Limit = 1
	for {
		page, next, err := store.ListObservations(ctx, pageFilter)
		if err != nil {
			t.Fatalf("asc page: %v", err)
		}
		for _, o := range page {
			walked = append(walked, o.ID)
		}
		if next == "" {
			break
		}
		pageFilter.Cursor = next
		if len(walked) > len(asc)+1 {
			t.Fatalf("asc cursor walk runaway: %d rows vs %d expected", len(walked), len(asc))
		}
	}
	if len(walked) != len(asc) {
		t.Fatalf("asc cursor walk: want %d rows, got %d", len(asc), len(walked))
	}
	for i, id := range walked {
		if id != asc[i].ID {
			t.Fatalf("asc cursor walk diverged at %d: %d vs %d", i, id, asc[i].ID)
		}
	}
	if def, _, err := store.ListObservations(ctx, ObservationFilter{Project: project, Limit: 50}); err == nil {
		for _, o := range def {
			if o.ID == pending.ID {
				t.Fatal("default read leaked a proposed row")
			}
		}
	} else {
		t.Fatalf("default list: %v", err)
	}

	// --- Disjunction semantics: one-of-two-terms must return BOTH docs
	// (round-1 finding 2: a single both-terms fixture cannot distinguish
	// OR from AND). Each event's text carries exactly one distinct term.
	for i, term := range []string{"axolotl", "pangolin"} {
		if _, err := store.Ingest(ctx, EventInput{
			Project: project, SessionID: session, SourceAdapter: "claude-code",
			SourceEventID: fmt.Sprintf("conf-evt-disj-%d", i), EventType: "claude_Stop",
			OccurredAtEpoch: now - 70 + int64(i),
			Text:            "disjunction fixture " + term,
		}); err != nil {
			t.Fatalf("disjunction ingest %d: %v", i, err)
		}
	}
	disj, err := store.Search(ctx, "axolotl pangolin", project, 10)
	if err != nil {
		t.Fatalf("disjunction search: %v", err)
	}
	foundTerms := map[string]bool{}
	for _, h := range disj {
		for _, term := range []string{"axolotl", "pangolin"} {
			if strings.Contains(h.Text, term) {
				foundTerms[term] = true
			}
		}
		if h.Snippet == "" {
			t.Fatalf("disjunction hit without snippet: %+v", h)
		}
	}
	if !foundTerms["axolotl"] || !foundTerms["pangolin"] {
		t.Fatalf("disjunction (OR) semantics: want both single-term docs, got %v (%d hits)", foundTerms, len(disj))
	}
	// A literal OR token must stay a TERM, not become query syntax
	// (round-1 finding 4; round-2 N4: no-error alone can false-green an
	// empty tsquery — the fixture doc's ID must actually come back).
	orDoc, err := store.Ingest(ctx, EventInput{
		Project: project, SessionID: session, SourceAdapter: "claude-code",
		SourceEventID: "conf-evt-literal-or", EventType: "claude_Stop",
		OccurredAtEpoch: now - 65,
		Text:            "choose the narwhal OR the quokka fixture",
	})
	if err != nil {
		t.Fatalf("literal-OR ingest: %v", err)
	}
	orHits, err := store.Search(ctx, "OR", project, 10)
	if err != nil {
		t.Fatalf("literal-OR search must not error: %v", err)
	}
	orFound := false
	for _, h := range orHits {
		// The hit is the event's RAW observation; match by text identity.
		if strings.Contains(h.Text, "narwhal OR the quokka") {
			orFound = true
		}
	}
	if !orFound {
		t.Fatalf("literal-OR search must return the OR-bearing doc (event %d): got %d hits", orDoc.ID, len(orHits))
	}
	// Score ties: identical texts must order deterministically on Postgres
	// (id DESC tiebreak); on SQLite FTS5 ORDER BY rank has no id tiebreak,
	// so assert set membership only.
	for i := 0; i < 2; i++ {
		if _, err := store.Ingest(ctx, EventInput{
			Project: project, SessionID: session, SourceAdapter: "claude-code",
			SourceEventID: fmt.Sprintf("conf-evt-tie-%d", i), EventType: "claude_Stop",
			OccurredAtEpoch: now - 60 + int64(i),
			Text:            "capybara tie fixture identical",
		}); err != nil {
			t.Fatalf("tie ingest %d: %v", i, err)
		}
	}
	ties, err := store.Search(ctx, "capybara", project, 10)
	if err != nil || len(ties) != 2 {
		t.Fatalf("tie search: want 2 hits, got %d err %v", len(ties), err)
	}
	// Round-2 N4: prove the ranks are actually EQUAL (identical texts) so
	// the id ordering assertion genuinely exercises the secondary sort key.
	if ties[0].Rank != ties[1].Rank {
		t.Fatalf("tie fixture ranks differ (%v vs %v) — fixture no longer exercises the tiebreak", ties[0].Rank, ties[1].Rank)
	}
	if store.Engine() == EnginePostgres && ties[0].ID < ties[1].ID {
		t.Fatalf("tie order on postgres: want id DESC, got %d then %d", ties[0].ID, ties[1].ID)
	}

	// --- Project-identity alert upsert (both engines) ---
	// The alert INSERT ... ON CONFLICT DO UPDATE runs inside the ingest
	// transaction, so a dialect break here fails ALL capture for the
	// conflicting project (live 42702 on Postgres, 2026-07-22: unqualified
	// event_count in the DO UPDATE). Learn a root, then hit it twice from a
	// second root so both the alert INSERT and the conflict increment execute.
	identProject := "conformance-ident"
	identIngest := func(n int, root, source string) {
		t.Helper()
		if _, err := store.Ingest(ctx, EventInput{
			Project: identProject, SessionID: "ident-conf-session", SourceAdapter: "claude-code",
			SourceEventID: fmt.Sprintf("conf-evt-ident-%d", n), EventType: "claude_Stop",
			OccurredAtEpoch: now - 9 + int64(n),
			Payload:         json.RawMessage(fmt.Sprintf(`{"memoire_project_root":%q,"memoire_project_source":%q}`, root, source)),
		}); err != nil {
			t.Fatalf("identity ingest %d: %v", n, err)
		}
	}
	identIngest(0, "/home/u/dev/ident", "memoire-file")
	identIngest(1, "/tmp/elsewhere/ident", "basename")
	identIngest(2, "/tmp/elsewhere/ident", "basename")
	identAlerts, err := store.ListProjectIdentityAlerts(ctx, identProject)
	if err != nil {
		t.Fatalf("list identity alerts: %v", err)
	}
	if len(identAlerts) != 1 || identAlerts[0].EventCount != 2 || identAlerts[0].KnownRoot != "/home/u/dev/ident" {
		t.Fatalf("identity alert upsert: want 1 alert with event_count=2 known_root=/home/u/dev/ident, got %+v", identAlerts)
	}
	// Registry-sourced join clears the alert on both engines.
	identIngest(3, "/tmp/elsewhere/ident", "registry")
	if cleared, err := store.ListProjectIdentityAlerts(ctx, identProject); err != nil || len(cleared) != 0 {
		t.Fatalf("registry join must clear the alert: %+v err %v", cleared, err)
	}

	// --- Health counters: JSON predicate + future skew ---
	if _, err := store.Ingest(ctx, EventInput{
		Project: project, SessionID: session, SourceAdapter: "claude-code",
		SourceEventID: "conf-evt-violation", EventType: "claude_Stop",
		OccurredAtEpoch: now - 10,
		Metadata:        json.RawMessage(`{"redaction_contract_violation":true}`),
	}); err != nil {
		t.Fatalf("violation ingest: %v", err)
	}
	if n, err := store.CountRedactionContractViolations(ctx); err != nil || n != 1 {
		t.Fatalf("redaction violations: want 1, got %d err %v", n, err)
	}
	if _, err := store.Ingest(ctx, EventInput{
		Project: project, SessionID: session, SourceAdapter: "claude-code",
		SourceEventID: "conf-evt-skew", EventType: "claude_Stop",
		OccurredAtEpoch: now + 3600, ReceivedAtEpoch: now,
	}); err != nil {
		t.Fatalf("skew ingest: %v", err)
	}
	if n, err := store.CountFutureSkewEvents(ctx, 60); err != nil || n != 1 {
		t.Fatalf("future skew: want 1, got %d err %v", n, err)
	}

	// --- Soft delete cascades to projections ---
	if err := store.DeleteEvent(ctx, first.ID); err != nil {
		t.Fatalf("delete event: %v", err)
	}
	if _, err := store.GetEvent(ctx, first.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted event visible: %v", err)
	}
	if hits, err := store.Search(ctx, "zebrafish", project, 10); err != nil || len(hits) != 0 {
		t.Fatalf("search after delete: want 0, got %d err %v", len(hits), err)
	}

	// --- Stats ---
	st, err := store.Stats(ctx, store.path)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.EventCount == 0 || st.DeletedEventCount != 1 || st.DBBytes == 0 {
		t.Fatalf("stats: %+v", st)
	}

	// --- Credentials round trip ---
	cred, err := store.EnsureLocalTokenCredential(ctx, "conformance-token-value")
	if err != nil {
		t.Fatalf("ensure credential: %v", err)
	}
	auth, ok, err := store.AuthenticateToken(ctx, "conformance-token-value", "test", "req-1")
	if err != nil || !ok || auth.CredentialID != cred.ID {
		t.Fatalf("authenticate: ok=%v auth=%+v err=%v", ok, auth, err)
	}
	if _, ok, _ := store.AuthenticateToken(ctx, "wrong-token", "test", "req-2"); ok {
		t.Fatal("wrong token authenticated")
	}

	// --- Sweep helpers: RecentDeriveTargets + MedianCanonicalEpoch ---
	// (derive-seam-port: these replace the sweep script's raw sqlite3
	// queries, so both engines must agree on shape and semantics.)
	medianProject := "conformance-median"
	medianSession := "median-session-1"
	canonical := []string{"claude_UserPromptSubmit", "claude_assistant_turn"}
	medianEpochs := []int64{now - 500, now - 400, now - 300, now - 200, now - 100}
	medianIDs := make([]int64, 0, len(medianEpochs))
	for i, ep := range medianEpochs {
		if res, err := store.Ingest(ctx, EventInput{
			Project:         medianProject,
			SessionID:       medianSession,
			SourceAdapter:   "claude-code",
			SourceEventID:   fmt.Sprintf("median-evt-%d", i),
			EventType:       canonical[i%2],
			OccurredAtEpoch: ep,
			// received_at pinned to occurred_at: the outstanding-coverage
			// watermark (round-1 HIGH-1) requires events to have ARRIVED
			// before a covering run's creation; wall-clock ingest stamps
			// would tie with the coverage run minted below.
			ReceivedAtEpoch: ep,
			Payload:         json.RawMessage(`{"prompt":"median fixture"}`),
		}); err != nil {
			t.Fatalf("median fixture ingest %d: %v", i, err)
		} else {
			medianIDs = append(medianIDs, res.ID)
		}
	}
	// A non-canonical event inside the window must not shift the median.
	if _, err := store.Ingest(ctx, EventInput{
		Project:         medianProject,
		SessionID:       medianSession,
		SourceAdapter:   "claude-code",
		SourceEventID:   "median-evt-noncanon",
		EventType:       "claude_SessionStart",
		OccurredAtEpoch: now - 250,
		Payload:         json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("median noncanonical ingest: %v", err)
	}

	targets, err := store.RecentDeriveTargets(ctx, now-3600)
	if err != nil {
		t.Fatalf("recent derive targets: %v", err)
	}
	foundMedian := false
	for i, tg := range targets {
		if tg.Project == "" || tg.SessionID == "" {
			t.Fatalf("derive target %d has empty field: %+v", i, tg)
		}
		if i > 0 {
			prev := targets[i-1]
			if prev.Project > tg.Project || (prev.Project == tg.Project && prev.SessionID >= tg.SessionID) {
				t.Fatalf("derive targets not strictly ordered at %d: %+v then %+v", i, prev, tg)
			}
		}
		if tg.Project == medianProject && tg.SessionID == medianSession {
			foundMedian = true
		}
	}
	if !foundMedian {
		t.Fatalf("derive targets missing %s/%s: %+v", medianProject, medianSession, targets)
	}
	if future, err := store.RecentDeriveTargets(ctx, now+3600); err != nil || len(future) != 0 {
		t.Fatalf("future-window targets: want none, got %d err %v", len(future), err)
	}

	// Odd count (5): median is the 3rd element.
	med, ok, err := store.MedianCanonicalEpoch(ctx, medianProject, medianSession, now-500, now-100, canonical)
	if err != nil || !ok || med != now-300 {
		t.Fatalf("median odd: want %d ok, got %d ok=%v err=%v", now-300, med, ok, err)
	}
	// Even count (4, trimming the oldest): OFFSET (4-1)/2 = 1 -> the 2nd
	// element of the remaining window — pins the truncation semantics the
	// sweep script relied on.
	med, ok, err = store.MedianCanonicalEpoch(ctx, medianProject, medianSession, now-400, now-100, canonical)
	if err != nil || !ok || med != now-300 {
		t.Fatalf("median even: want %d ok, got %d ok=%v err=%v", now-300, med, ok, err)
	}
	// Empty window -> ok=false, no error.
	if _, ok, err := store.MedianCanonicalEpoch(ctx, medianProject, medianSession, now+100, now+200, canonical); err != nil || ok {
		t.Fatalf("median empty window: want ok=false, got ok=%v err=%v", ok, err)
	}
	// Soft-deleted canonical events are excluded: deleting the newest
	// fixture leaves 4 live epochs, so the full-window median shifts to
	// the 2nd remaining element.
	if err := store.DeleteEvent(ctx, medianIDs[len(medianIDs)-1]); err != nil {
		t.Fatalf("median soft delete: %v", err)
	}
	med, ok, err = store.MedianCanonicalEpoch(ctx, medianProject, medianSession, now-500, now-100, canonical)
	if err != nil || !ok || med != now-400 {
		t.Fatalf("median after soft delete: want %d ok, got %d ok=%v err=%v", now-400, med, ok, err)
	}

	// --- Derive intents: idempotent notify, completion latency, reopen ---
	// (memoire:D165 R-10.1; the notify upsert's DO UPDATE self-references
	// are table-qualified — same 42702 class as the D164 incident, so both
	// engines must execute the statement here.)
	intProject := "conformance-intents"
	intSession := "intent-session-1"
	i1, err := store.NotifyDeriveIntent(ctx, intProject, intSession, now-30)
	if err != nil || i1.NotifyCount != 1 || i1.FirstNotifiedEpoch != now-30 {
		t.Fatalf("first notify: %+v err %v", i1, err)
	}
	i2, err := store.NotifyDeriveIntent(ctx, intProject, intSession, now-20)
	if err != nil || i2.NotifyCount != 2 || i2.FirstNotifiedEpoch != now-30 || i2.LastNotifiedEpoch != now-20 {
		t.Fatalf("repeat notify must collapse (count=2, first preserved): %+v err %v", i2, err)
	}
	open, err := store.OpenDeriveIntents(ctx)
	if err != nil {
		t.Fatalf("open intents: %v", err)
	}
	foundIntent := false
	for _, it := range open {
		if it.Project == intProject && it.SessionID == intSession {
			foundIntent = true
		}
	}
	if !foundIntent {
		t.Fatalf("open intents missing %s/%s: %+v", intProject, intSession, open)
	}
	// Eligibility (round-2 MED-NEW-1): a run created BEFORE the notify never
	// serves the intent — the notify announced work that run never saw.
	if _, ok2, err := store.CompleteDeriveIntent(ctx, intProject, intSession, "run-stale", now-60, now-10); err != nil || ok2 {
		t.Fatalf("pre-notify run must not serve the intent, got ok=%v err=%v", ok2, err)
	}
	// Round-4: the predicate guards the LATEST notify — a fetch after the
	// first ask but before a re-notify must hold (the fresh ask would be
	// swallowed otherwise).
	if _, ok2, err := store.CompleteDeriveIntent(ctx, intProject, intSession, "run-mid", now-25, now-10); err != nil || ok2 {
		t.Fatalf("fetch predating the LATEST notify must not serve, got ok=%v err=%v", ok2, err)
	}
	done, ok2, err := store.CompleteDeriveIntent(ctx, intProject, intSession, "run-intent-1", now-10, now-10)
	if err != nil || !ok2 || done.LatencySeconds != 20 || done.CompletedRunID != "run-intent-1" {
		t.Fatalf("complete intent: want latency 20s run-intent-1, got %+v ok=%v err=%v", done, ok2, err)
	}
	if _, ok2, err := store.CompleteDeriveIntent(ctx, intProject, intSession, "run-intent-2", now-5, now-5); err != nil || ok2 {
		t.Fatalf("second complete must be a no-op (ok=false), got ok=%v err=%v", ok2, err)
	}
	reopened, err := store.NotifyDeriveIntent(ctx, intProject, intSession, now-3)
	if err != nil || reopened.NotifyCount != 1 || reopened.FirstNotifiedEpoch != now-3 {
		t.Fatalf("notify after completion must reopen fresh: %+v err %v", reopened, err)
	}
	// Round-3 eligibility is STRICT: a same-second notify/fetch tie holds
	// (unprovable ordering); the next tick serves.
	if _, ok3, err := store.CompleteDeriveIntent(ctx, intProject, intSession, "run-tie", now-3, now); err != nil || ok3 {
		t.Fatalf("same-second notify/fetch tie must hold, got ok=%v err=%v", ok3, err)
	}
	if _, ok3, err := store.CompleteDeriveIntent(ctx, intProject, intSession, "run-next-tick", now-2, now); err != nil || !ok3 {
		t.Fatalf("strictly later fetch must serve, got ok=%v err=%v", ok3, err)
	}

	// --- Outstanding derive work: computed from events x derivation_runs ---
	// (Q2 ruling: no coverage table.) The median fixtures above left 4 live
	// canonical events (now-500..now-200) with NO completed run covering
	// them; completing a run over [now-500, now-250] must shrink the
	// uncovered set to exactly the now-200 event.
	outWork := func() (OutstandingWork, bool) {
		all, err := store.OutstandingDeriveWork(ctx, now-3600, canonical)
		if err != nil {
			t.Fatalf("outstanding: %v", err)
		}
		for _, w := range all {
			if w.Project == "conformance-ident" {
				t.Fatalf("non-canonical events must never be outstanding: %+v", w)
			}
			if w.Project == medianProject && w.SessionID == medianSession {
				return w, true
			}
		}
		return OutstandingWork{}, false
	}
	before, found := outWork()
	if !found || before.UncoveredEvents != 4 || before.OldestEpoch != now-500 || before.NewestEpoch != now-200 {
		t.Fatalf("outstanding before coverage: want 4 events [%d..%d], got %+v found=%v", now-500, now-200, before, found)
	}
	if before.UncoveredBytes <= 0 {
		t.Fatalf("outstanding bytes must be positive: %+v", before)
	}
	// Covered ISLAND (round-1 MED-1 fixture, manifest-only since round 4):
	// a completed run whose MANIFEST lists only the middle two events. The
	// row's bounds still span the island, but the count and bytes must
	// reflect ONLY the uncovered edges — pinning the ruled aggregate
	// semantics (bounds are not a derivable window; identical fixture
	// payloads make bytes exactly halve).
	islandRun, err := store.EnsureDerivationRun(ctx, DerivationRunInput{
		Project: medianProject, SessionID: medianSession, Provider: "rules", DataBoundary: "local",
		DerivationVersion: "v1", InputHash: "cov-hash-island",
		WindowStartEpoch: now - 450, WindowEndEpoch: now - 250,
	})
	if err != nil {
		t.Fatalf("island run: %v", err)
	}
	if err := store.RecordRunInputEvents(ctx, islandRun.ID, []int64{medianIDs[1], medianIDs[2]}); err != nil {
		t.Fatalf("island manifest: %v", err)
	}
	// A manifested run that is not yet completed must NOT count as coverage
	// (status gates the manifest arm).
	mid, found := outWork()
	if !found || mid.UncoveredEvents != 4 {
		t.Fatalf("pending run must not cover: got %+v found=%v", mid, found)
	}
	if err := store.UpdateDerivationRunStatus(ctx, DerivationRunStatusUpdate{ID: islandRun.ID, Status: "completed", CompletedAtEpoch: now}); err != nil {
		t.Fatalf("complete island run: %v", err)
	}
	island, found := outWork()
	if !found || island.UncoveredEvents != 2 || island.OldestEpoch != now-500 || island.NewestEpoch != now-200 {
		t.Fatalf("covered island: want 2 uncovered edge events spanning [%d..%d], got %+v found=%v", now-500, now-200, island, found)
	}
	if island.UncoveredBytes*2 != before.UncoveredBytes {
		t.Fatalf("covered island bytes must count ONLY uncovered events: before=%d island=%d", before.UncoveredBytes, island.UncoveredBytes)
	}
	// A manifest-less completed run provides NO coverage (round 4: every
	// timestamp heuristic proved unsound; absence of evidence reads
	// outstanding) — the uncovered set must not shrink.
	bareRun, err := store.EnsureDerivationRun(ctx, DerivationRunInput{
		Project: medianProject, SessionID: medianSession, Provider: "rules", DataBoundary: "local",
		DerivationVersion: "v1", InputHash: "cov-hash-bare",
		WindowStartEpoch: now - 500, WindowEndEpoch: now - 100,
	})
	if err != nil {
		t.Fatalf("bare run: %v", err)
	}
	if err := store.UpdateDerivationRunStatus(ctx, DerivationRunStatusUpdate{ID: bareRun.ID, Status: "completed", CompletedAtEpoch: now}); err != nil {
		t.Fatalf("complete bare run: %v", err)
	}
	if bare, found := outWork(); !found || bare.UncoveredEvents != 2 {
		t.Fatalf("manifest-less completed run must cover NOTHING: got %+v found=%v", bare, found)
	}
	// Coverage over the left edge via manifest: exactly the now-200 event stays.
	covRun, err := store.EnsureDerivationRun(ctx, DerivationRunInput{
		Project: medianProject, SessionID: medianSession, Provider: "rules", DataBoundary: "local",
		DerivationVersion: "v1", InputHash: "cov-hash-1",
		WindowStartEpoch: now - 500, WindowEndEpoch: now - 250,
	})
	if err != nil {
		t.Fatalf("coverage run: %v", err)
	}
	if err := store.RecordRunInputEvents(ctx, covRun.ID, []int64{medianIDs[0]}); err != nil {
		t.Fatalf("coverage manifest: %v", err)
	}
	if err := store.UpdateDerivationRunStatus(ctx, DerivationRunStatusUpdate{ID: covRun.ID, Status: "completed", CompletedAtEpoch: now}); err != nil {
		t.Fatalf("complete coverage run: %v", err)
	}
	after, found := outWork()
	if !found || after.UncoveredEvents != 1 || after.OldestEpoch != now-200 || after.NewestEpoch != now-200 {
		t.Fatalf("outstanding after coverage: want exactly the now-200 event, got %+v found=%v", after, found)
	}
	// LATE ARRIVAL (round-1 HIGH-1 fixture): an event whose occurred_at lies
	// INSIDE a completed window but which ARRIVED after that run was created
	// (thin-client forwarding lag, transcript backfill) was never in the
	// run's input snapshot and must stay outstanding.
	lateIngest, err := store.Ingest(ctx, EventInput{
		Project: medianProject, SessionID: medianSession, SourceAdapter: "claude-code",
		SourceEventID: "median-evt-late", EventType: canonical[0],
		OccurredAtEpoch: now - 300,
		ReceivedAtEpoch: now + 100,
		Payload:         json.RawMessage(`{"prompt":"late arrival"}`),
	})
	if err != nil {
		t.Fatalf("late-arrival ingest: %v", err)
	}
	lateID := lateIngest.ID
	late, found := outWork()
	if !found || late.UncoveredEvents != 2 || late.OldestEpoch != now-300 {
		t.Fatalf("late arrival must stay outstanding despite enclosing window: got %+v found=%v", late, found)
	}
	// MANIFEST coverage is exact set membership (round-3 HIGH-1): a run
	// with recorded input rows covers EXACTLY its manifest. A run whose
	// manifest lists the two remaining uncovered events covers them.
	lateRes, err := store.Ingest(ctx, EventInput{
		Project: medianProject, SessionID: medianSession, SourceAdapter: "claude-code",
		SourceEventID: "median-evt-late-id-probe", EventType: canonical[1],
		OccurredAtEpoch: now - 140, ReceivedAtEpoch: now - 140,
		Payload: json.RawMessage(`{"prompt":"manifest member"}`),
	})
	if err != nil {
		t.Fatalf("manifest member ingest: %v", err)
	}
	mRun, err := store.EnsureDerivationRun(ctx, DerivationRunInput{
		Project: medianProject, SessionID: medianSession, Provider: "rules", DataBoundary: "local",
		DerivationVersion: "v1", InputHash: "cov-hash-manifest",
		WindowStartEpoch: now - 500, WindowEndEpoch: now - 100,
	})
	if err != nil {
		t.Fatalf("manifest run: %v", err)
	}
	// Manifest lists the late event, the now-200 event, and the new probe —
	// the exact uncovered set at this point.
	if err := store.RecordRunInputEvents(ctx, mRun.ID, []int64{lateID, medianIDs[3], lateRes.ID}); err != nil {
		t.Fatalf("record manifest: %v", err)
	}
	// Idempotent re-record (retry claims / dedup upgrades).
	if err := store.RecordRunInputEvents(ctx, mRun.ID, []int64{lateID, medianIDs[3], lateRes.ID}); err != nil {
		t.Fatalf("re-record manifest: %v", err)
	}
	if err := store.UpdateDerivationRunStatus(ctx, DerivationRunStatusUpdate{ID: mRun.ID, Status: "completed", CompletedAtEpoch: now}); err != nil {
		t.Fatalf("complete manifest run: %v", err)
	}
	if covered, found := outWork(); found {
		t.Fatalf("manifested run must cover exactly its manifest (all uncovered events), got %+v", covered)
	}
	// THE round-3 killer, encoded: an event INSIDE a manifested run's
	// window whose id/timestamps would satisfy every prior heuristic
	// (received before the run was created — the PG lower-id late-commit
	// interleaving) but which is ABSENT from the manifest. A manifested run
	// covers manifest members ONLY; the legacy timestamp arm is disabled
	// for it, so this event must stay outstanding.
	if _, err := store.Ingest(ctx, EventInput{
		Project: medianProject, SessionID: medianSession, SourceAdapter: "claude-code",
		SourceEventID: "median-evt-not-in-manifest", EventType: canonical[0],
		OccurredAtEpoch: now - 130,
		ReceivedAtEpoch: now - 400,
		Payload:         json.RawMessage(`{"prompt":"committed late, id allocated early"}`),
	}); err != nil {
		t.Fatalf("non-manifest ingest: %v", err)
	}
	race, found := outWork()
	if !found || race.UncoveredEvents != 1 || race.OldestEpoch != now-130 {
		t.Fatalf("event absent from the manifest must stay outstanding: got %+v found=%v", race, found)
	}
	// A future-bounded window returns nothing.
	if none, err := store.OutstandingDeriveWork(ctx, now+3600, canonical); err != nil || len(none) != 0 {
		t.Fatalf("future outstanding: want none, got %d err %v", len(none), err)
	}

	// --- ListEventsForDerivation ExcludeCovered: the fetch-side twin of the
	// coverage rule above. At this point the session holds 7 canonical
	// events, all covered by completed manifests EXCEPT the round-3 killer
	// (now-130). A derivation fetch with ExcludeCovered must feed the model
	// exactly the uncovered set — the sweep's whole-session windows re-fed
	// covered islands otherwise, and paraphrase re-derivation defeats
	// exact-hash dedupe (the duplicate factory this flag closes).
	fetchFilter := DerivationEventFilter{
		Project: medianProject, SessionID: medianSession, EventTypes: canonical,
	}
	allEvents, err := store.ListEventsForDerivation(ctx, fetchFilter)
	if err != nil {
		t.Fatalf("fetch all: %v", err)
	}
	if len(allEvents) != 7 {
		t.Fatalf("fixture drift: want 7 canonical events in session, got %d", len(allEvents))
	}
	fetchFilter.ExcludeCovered = true
	uncov, err := store.ListEventsForDerivation(ctx, fetchFilter)
	if err != nil {
		t.Fatalf("fetch uncovered: %v", err)
	}
	if len(uncov) != 1 || uncov[0].OccurredAtEpoch != now-130 {
		t.Fatalf("ExcludeCovered must return exactly the manifest-absent event (now-130), got %d rows %+v", len(uncov), uncov)
	}
	// Coverage is per-(project, session): a completed manifested run in a
	// FOREIGN session listing our uncovered event id claims nothing here.
	foreignRun, err := store.EnsureDerivationRun(ctx, DerivationRunInput{
		Project: "conformance-foreign", SessionID: "foreign-1", Provider: "rules", DataBoundary: "local",
		DerivationVersion: "v1", InputHash: "cov-hash-foreign",
		WindowStartEpoch: now - 500, WindowEndEpoch: now - 100,
	})
	if err != nil {
		t.Fatalf("foreign run: %v", err)
	}
	if err := store.RecordRunInputEvents(ctx, foreignRun.ID, []int64{uncov[0].ID}); err != nil {
		t.Fatalf("foreign manifest: %v", err)
	}
	if err := store.UpdateDerivationRunStatus(ctx, DerivationRunStatusUpdate{ID: foreignRun.ID, Status: "completed", CompletedAtEpoch: now}); err != nil {
		t.Fatalf("complete foreign run: %v", err)
	}
	if still, err := store.ListEventsForDerivation(ctx, fetchFilter); err != nil || len(still) != 1 {
		t.Fatalf("foreign-session manifest must not cover: want 1, got %d err %v", len(still), err)
	}
	// Single-snapshot classifier (round-2 R2-F2): total and uncovered are
	// facts of ONE statement. Same fixture: 7 live canonical events in the
	// session, exactly 1 uncovered (the foreign run added none here).
	// ExcludeCovered on the filter is ignored by the classifier.
	totalCls, uncovCls, err := store.ListEventsForDerivationCoverage(ctx, fetchFilter)
	if err != nil {
		t.Fatalf("coverage classify: %v", err)
	}
	if totalCls != 7 || len(uncovCls) != 1 || uncovCls[0].OccurredAtEpoch != now-130 {
		t.Fatalf("classifier: want total=7 uncovered=1 (now-130), got total=%d uncovered=%d %+v", totalCls, len(uncovCls), uncovCls)
	}
	// Soft-deleting the sole uncovered event: 6 covered live rows remain,
	// so the classification becomes total=6/uncovered=0 — the
	// fully_covered shape. The delete is visible to the SAME statement
	// that counts, so no serial-read pair can misreport the window.
	if err := store.DeleteEvent(ctx, uncovCls[0].ID); err != nil {
		t.Fatalf("soft-delete uncovered: %v", err)
	}
	totalCls, uncovCls, err = store.ListEventsForDerivationCoverage(ctx, fetchFilter)
	if err != nil || totalCls != 6 || len(uncovCls) != 0 {
		t.Fatalf("classifier after soft-delete: want total=6 uncovered=0, got total=%d uncovered=%d err %v", totalCls, len(uncovCls), err)
	}

	// --- ListSessionObservationsForDerivation: the derivation lane's session
	// read-back (D211 step 3 — already-captured context + near-dup gate).
	// Raw event mirrors are EXCLUDED (Ingest writes one per event, and a
	// derivation's whole job is restating event content); every other
	// non-deleted review state is visible, newest first; limit honored.
	soProject, soSession := "conformance-sessionobs", "so-1"
	soEvt, err := store.Ingest(ctx, EventInput{
		Project: soProject, SessionID: soSession, SourceAdapter: "claude-code",
		SourceEventID: "so-src-1", EventType: canonical[0], OccurredAtEpoch: now - 100,
		Payload: json.RawMessage(`{"prompt":"session readback seed"}`),
	})
	if err != nil {
		t.Fatalf("sessionobs ingest: %v", err)
	}
	soRun, err := store.EnsureDerivationRun(ctx, DerivationRunInput{
		Project: soProject, SessionID: soSession, Provider: "local-qwen", Model: "qwen2.5-7b",
		EndpointID: "hl3", DataBoundary: "local", DerivationVersion: "v2", InputHash: "so-hash",
		WindowStartEpoch: now - 200, WindowEndEpoch: now, Status: "running",
	})
	if err != nil {
		t.Fatalf("sessionobs run: %v", err)
	}
	soConf := 0.9
	soIDs := make([]int64, 0, 3)
	for i, text := range []string{"session fact alpha", "session fact bravo", "session fact charlie"} {
		res, err := store.InsertDerivedObservation(ctx, DerivedObservationInput{
			Project: soProject, SessionID: soSession, Kind: "fact", Text: text,
			Confidence: &soConf, ReviewState: "proposed", DerivationRunID: soRun.ID,
			ContentHash: fmt.Sprintf("so-obs-hash-%d", i), SourceEventIDs: []int64{soEvt.ID},
			ModelDerived: true,
		})
		if err != nil {
			t.Fatalf("sessionobs insert %d: %v", i, err)
		}
		soIDs = append(soIDs, res.ID)
	}
	// A reviewed-away state stays visible (rejected content must not be
	// re-proposed); a soft-deleted row disappears.
	if _, err := store.TransitionObservationReview(ctx, soIDs[1], "rejected", "conformance"); err != nil {
		t.Fatalf("sessionobs reject: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE observations SET deleted_at_epoch = ? WHERE id = ?`, now, soIDs[2]); err != nil {
		t.Fatalf("sessionobs soft-delete: %v", err)
	}
	sessionObs, err := store.ListSessionObservationsForDerivation(ctx, soProject, soSession, 0)
	if err != nil {
		t.Fatalf("sessionobs list: %v", err)
	}
	if len(sessionObs) != 2 {
		t.Fatalf("want 2 rows (raw mirror + deleted excluded), got %d: %+v", len(sessionObs), sessionObs)
	}
	if sessionObs[0].ID != soIDs[1] || sessionObs[0].ReviewState != "rejected" || sessionObs[1].ID != soIDs[0] {
		t.Fatalf("want newest-first [bravo/rejected, alpha/proposed], got %+v", sessionObs)
	}
	for _, obs := range sessionObs {
		if obs.Kind == "raw" {
			t.Fatalf("raw event mirror leaked into session read-back: %+v", obs)
		}
	}
	if limited, err := store.ListSessionObservationsForDerivation(ctx, soProject, soSession, 1); err != nil || len(limited) != 1 || limited[0].ID != soIDs[1] {
		t.Fatalf("limit=1 must return the newest row only, got %+v err %v", limited, err)
	}
	// Gated insert (near-dup gate atomic decision, D211 step 3 round-1
	// finding 1): a live matched row blocks the insert and is named; once
	// the row is soft-deleted the same call inserts. Liveness is read in
	// the insert transaction on both engines.
	gatedIn := DerivedObservationInput{
		Project: soProject, SessionID: soSession, Kind: "fact",
		Text: "session fact alpha, reworded", Confidence: &soConf, ReviewState: "proposed",
		DerivationRunID: soRun.ID, ContentHash: "so-gated-hash",
		SourceEventIDs: []int64{soEvt.ID}, ModelDerived: true,
	}
	if _, liveMatch, err := store.InsertDerivedObservationUnlessMatchedLive(ctx, gatedIn, []int64{soIDs[0]}); err != nil || liveMatch != soIDs[0] {
		t.Fatalf("gated insert must refuse on the live match: live=%d err %v", liveMatch, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE observations SET deleted_at_epoch = ? WHERE id = ?`, now, soIDs[0]); err != nil {
		t.Fatalf("soft-delete matched: %v", err)
	}
	if res, liveMatch, err := store.InsertDerivedObservationUnlessMatchedLive(ctx, gatedIn, []int64{soIDs[0]}); err != nil || liveMatch != 0 || res.ID == 0 {
		t.Fatalf("gated insert must proceed once the match is dead: res=%+v live=%d err %v", res, liveMatch, err)
	}

	// --- No-work intent settlement (round-1 MED-4 policy) ---
	// An old intent with zero canonical events expires; an old intent WITH
	// events survives; a recent no-work intent survives (age gate).
	if _, err := store.NotifyDeriveIntent(ctx, "conformance-ghost", "ghost-1", now-7200); err != nil {
		t.Fatalf("ghost notify: %v", err)
	}
	if _, err := store.NotifyDeriveIntent(ctx, medianProject, medianSession, now-7200); err != nil {
		t.Fatalf("median notify: %v", err)
	}
	if _, err := store.NotifyDeriveIntent(ctx, "conformance-ghost", "ghost-recent", now-60); err != nil {
		t.Fatalf("recent ghost notify: %v", err)
	}
	settled, err := store.SettleNoWorkIntents(ctx, now-3600, now, canonical)
	if err != nil || settled != 1 {
		t.Fatalf("settle: want exactly the old ghost settled, got %d err %v", settled, err)
	}
	stillOpen, err := store.OpenDeriveIntents(ctx)
	if err != nil {
		t.Fatalf("open after settle: %v", err)
	}
	openKeys := map[string]bool{}
	for _, it := range stillOpen {
		openKeys[it.Project+"/"+it.SessionID] = true
	}
	if openKeys["conformance-ghost/ghost-1"] {
		t.Fatal("old no-work intent must be settled")
	}
	if !openKeys[medianProject+"/"+medianSession] || !openKeys["conformance-ghost/ghost-recent"] {
		t.Fatalf("with-events and recent intents must survive settle: %+v", openKeys)
	}

	conformanceConcurrentVerdicts(t, store)
}

// conformanceConcurrentVerdicts is the concurrency falsifier for the
// preconditioned-verdict CAS discipline (precondition review round-1
// BLOCKERs 1 & 3, MAJORs 1 & 2): the guarded UPDATE with RowsAffected as
// the authority must let exactly ONE of N racing writers commit, with
// every loser conflicting cleanly (no fabricated review-log/audit rows)
// and every conflict carrying the WINNER's fresh metadata. On SQLite the
// races serialize at the file lock (the invariants still bind); the
// Postgres leg (MEMOIRE_TEST_PG_URL) exercises the real READ COMMITTED
// interleavings the round-1 review proved unprotected.
func conformanceConcurrentVerdicts(t *testing.T, store *Store) {
	ctx := context.Background()
	now := time.Now().Unix()
	project := "conformance-race"
	session := "conf-race-1"
	ev, err := store.Ingest(ctx, EventInput{
		Project: project, SessionID: session, SourceAdapter: "claude-code",
		SourceEventID: "race-evt-1", EventType: "claude_UserPromptSubmit",
		OccurredAtEpoch: now - 50, Payload: json.RawMessage(`{"prompt":"race seed"}`),
	})
	if err != nil {
		t.Fatalf("race ingest: %v", err)
	}
	run, err := store.EnsureDerivationRun(ctx, DerivationRunInput{
		Project: project, SessionID: session, Provider: "manual", DataBoundary: "local",
		DerivationVersion: "observation-derive-v1", InputHash: "race-hash",
		WindowStartEpoch: now - 60, WindowEndEpoch: now - 40, Status: "completed",
	})
	if err != nil {
		t.Fatalf("race run: %v", err)
	}
	seed := func(hash string) int64 {
		t.Helper()
		rec, err := store.InsertDerivedObservation(ctx, DerivedObservationInput{
			Project: project, SessionID: session, Kind: "fact", Text: "race target " + hash,
			DerivationRunID: run.ID, ContentHash: hash, SourceEventIDs: []int64{ev.ID},
			ModelDerived: true, OccurredAtEpoch: now - 30,
		})
		if err != nil {
			t.Fatalf("race seed %s: %v", hash, err)
		}
		return rec.ID
	}
	logCount := func(id int64) int {
		t.Helper()
		recs, err := store.ListObservationReviewLog(ctx, ObservationReviewLogFilter{ObservationID: id, Limit: 50})
		if err != nil {
			t.Fatalf("race log count: %v", err)
		}
		return len(recs)
	}
	proposedState := "proposed"
	inferred := "inferred"
	zero := 0

	// --- N racing preconditioned confirms: exactly one commits ---
	confirmID := seed("race-confirm")
	const racers = 8
	var wg sync.WaitGroup
	results := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = store.ConfirmObservationPre(ctx, confirmID, "race", &ReviewPrecondition{
				ReviewState: &proposedState, Provenance: &inferred, CanInstruct: &zero,
			})
		}(i)
	}
	wg.Wait()
	wins, conflicts := 0, 0
	for _, rerr := range results {
		var sce *StateConflictError
		switch {
		case rerr == nil:
			wins++
		case errors.As(rerr, &sce):
			conflicts++
			if sce.ReviewState != "accepted" || sce.Provenance != "user_confirmed" {
				t.Fatalf("confirm loser got stale metadata: %+v", sce)
			}
		default:
			t.Fatalf("confirm racer unexpected error: %v", rerr)
		}
	}
	if wins != 1 || conflicts != racers-1 {
		t.Fatalf("racing confirms: want 1 win %d conflicts, got %d/%d", racers-1, wins, conflicts)
	}
	if n := logCount(confirmID); n != 1 {
		t.Fatalf("racing confirms wrote %d review-log rows, want exactly 1", n)
	}

	// --- N racing rewrites: exactly one accepted replacement ---
	rewriteID := seed("race-rewrite")
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = store.RewriteObservation(ctx, rewriteID, fmt.Sprintf("racer %d wording", i), "race")
		}(i)
	}
	wg.Wait()
	wins = 0
	for _, rerr := range results {
		var sce *StateConflictError
		if rerr == nil {
			wins++
		} else if !errors.As(rerr, &sce) {
			t.Fatalf("rewrite racer unexpected error: %v", rerr)
		}
	}
	var children int
	if err := store.rdb.QueryRowContext(ctx,
		`SELECT count(*) FROM observations WHERE supersedes_id = ?`, rewriteID).Scan(&children); err != nil {
		t.Fatalf("rewrite children count: %v", err)
	}
	if wins != 1 || children != 1 {
		t.Fatalf("racing rewrites: want exactly 1 win and 1 replacement, got wins=%d children=%d", wins, children)
	}
	if n := logCount(rewriteID); n != 1 {
		t.Fatalf("racing rewrites wrote %d review-log rows on the original, want 1", n)
	}

	// --- bulk lapse racing a preconditioned accept: one winner, honest count ---
	bulkSession := "conf-race-bulk"
	bulkID := func() int64 {
		rec, err := store.InsertDerivedObservation(ctx, DerivedObservationInput{
			Project: project, SessionID: bulkSession, Kind: "fact", Text: "bulk race target",
			DerivationRunID: run.ID, ContentHash: "race-bulk", SourceEventIDs: []int64{ev.ID},
			ModelDerived: true, OccurredAtEpoch: now - 20,
		})
		if err != nil {
			t.Fatalf("bulk seed: %v", err)
		}
		return rec.ID
	}()
	var lapsedCount int
	var lapseErr, acceptErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		lapsedCount, lapseErr = store.LapseSessionRemaining(ctx, bulkSession, "race")
	}()
	go func() {
		defer wg.Done()
		_, acceptErr = store.TransitionObservationReviewPre(ctx, bulkID, "accepted", "race", &ReviewPrecondition{
			ReviewState: &proposedState, Provenance: &inferred, CanInstruct: &zero,
		})
	}()
	wg.Wait()
	if lapseErr != nil {
		t.Fatalf("bulk lapse racer: %v", lapseErr)
	}
	var sce *StateConflictError
	acceptWon := acceptErr == nil
	if !acceptWon {
		if !errors.As(acceptErr, &sce) {
			t.Fatalf("accept racer unexpected error: %v", acceptErr)
		}
		if sce.ReviewState != "lapsed" {
			t.Fatalf("accept loser conflict carries %q, want fresh lapsed", sce.ReviewState)
		}
	}
	var freshState string
	if err := store.rdb.QueryRowContext(ctx,
		`SELECT review_state FROM observations WHERE id = ?`, bulkID).Scan(&freshState); err != nil {
		t.Fatalf("bulk fresh read: %v", err)
	}
	// One row, one winner: the surviving state must match whichever racer
	// reported success (the interleaving where bulk commits first and the
	// accept conflicts is equally legal).
	wantState := map[bool]string{true: "accepted", false: "lapsed"}[acceptWon]
	if freshState != wantState {
		t.Fatalf("bulk race: winner/state mismatch: acceptWon=%v state=%s", acceptWon, freshState)
	}
	if n := logCount(bulkID); n != 1 {
		t.Fatalf("bulk race wrote %d review-log rows, want exactly 1", n)
	}
	if acceptWon && lapsedCount != 0 {
		t.Fatalf("bulk lapse count fabricated: %d (accept won)", lapsedCount)
	}
	if !acceptWon && lapsedCount != 1 {
		t.Fatalf("bulk lapse count wrong: %d (lapse won)", lapsedCount)
	}

	// --- N racing elevations: one winner; every conflict carries FRESH
	// can_instruct=1 (round-1 MAJOR 2) ---
	elevID := seed("race-elevate")
	if _, err := store.ConfirmObservation(ctx, elevID, "race prep"); err != nil {
		t.Fatalf("elevate prep confirm: %v", err)
	}
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = store.ElevateObservation(ctx, elevID, "race")
		}(i)
	}
	wg.Wait()
	wins = 0
	for _, rerr := range results {
		var esce *StateConflictError
		switch {
		case rerr == nil:
			wins++
		case errors.As(rerr, &esce):
			if esce.CanInstruct != 1 {
				t.Fatalf("elevation loser got stale can_instruct=%d, want fresh 1", esce.CanInstruct)
			}
		default:
			t.Fatalf("elevate racer unexpected error: %v", rerr)
		}
	}
	if wins != 1 {
		t.Fatalf("racing elevations: want exactly 1 win, got %d", wins)
	}

	// --- DETERMINISTIC CAS-loser exercise (round-2 review NEW MAJOR 3) ---
	// A pure goroutine race can legally serialize whole calls, in which
	// case every loser fails at its pre-read guard and the zero-row branch
	// is never exercised. The reviewCASTestHook barrier forces BOTH writers
	// past their pre-reads before either issues its UPDATE, so exactly one
	// CAS wins and the loser MUST come through the zero-row/fresh-re-read
	// branch: on Postgres (READ COMMITTED) the loser's UPDATE waits on the
	// winner's row lock, re-evaluates against the committed winner, and
	// matches zero rows. POSTGRES-ONLY by construction: the SQLite write
	// pool is SetMaxOpenConns(1), so a second writer cannot even open its
	// transaction while the first holds the barrier — the check-then-act
	// interleaving this exercises is impossible there, which is exactly why
	// the round-1 finding named Postgres.
	if store.engine != EnginePostgres {
		return
	}
	barrierRaceSites := func(name string, match func(site string) bool, id int64, a, b func() error) (winnerErr, loserErr error) {
		t.Helper()
		arrived := make(chan struct{}, 2)
		proceed := make(chan struct{})
		reviewCASTestHook = func(s string, hid int64) {
			if match(s) && hid == id {
				arrived <- struct{}{}
				<-proceed
			}
		}
		var errA, errB error
		var bwg sync.WaitGroup
		bwg.Add(2)
		go func() { defer bwg.Done(); errA = a() }()
		go func() { defer bwg.Done(); errB = b() }()
		<-arrived
		<-arrived
		close(proceed)
		bwg.Wait()
		reviewCASTestHook = nil
		switch {
		case errA == nil && errB == nil:
			t.Fatalf("%s: both barrier racers claimed success", name)
		case errA != nil && errB != nil:
			t.Fatalf("%s: both barrier racers failed: %v / %v", name, errA, errB)
		case errA == nil:
			return errA, errB
		}
		return errB, errA
	}
	barrierRace := func(name, site string, id int64, a, b func() error) (winnerErr, loserErr error) {
		t.Helper()
		return barrierRaceSites(name, func(s string) bool { return s == site }, id, a, b)
	}
	assertLoser := func(name string, loserErr error, wantState string) {
		t.Helper()
		var lsce *StateConflictError
		if !errors.As(loserErr, &lsce) {
			t.Fatalf("%s: loser must get StateConflictError with fresh metadata, got: %v", name, loserErr)
		}
		if lsce.ReviewState != wantState {
			t.Fatalf("%s: loser conflict carries %q, want fresh %q", name, lsce.ReviewState, wantState)
		}
	}

	// transition vs transition: accept vs reject from the same snapshot.
	bID := seed("race-barrier-transition")
	_, bLoser := barrierRace("barrier-transition", "transition", bID,
		func() error {
			_, err := store.TransitionObservationReviewPre(ctx, bID, "accepted", "barrier", &ReviewPrecondition{
				ReviewState: &proposedState, Provenance: &inferred, CanInstruct: &zero,
			})
			return err
		},
		func() error {
			_, err := store.TransitionObservationReviewPre(ctx, bID, "rejected", "barrier", &ReviewPrecondition{
				ReviewState: &proposedState, Provenance: &inferred, CanInstruct: &zero,
			})
			return err
		})
	var bState string
	if err := store.rdb.QueryRowContext(ctx,
		`SELECT review_state FROM observations WHERE id = ?`, bID).Scan(&bState); err != nil {
		t.Fatalf("barrier fresh read: %v", err)
	}
	if bState != "accepted" && bState != "rejected" {
		t.Fatalf("barrier-transition: unexpected final state %q", bState)
	}
	assertLoser("barrier-transition", bLoser, bState)
	if n := logCount(bID); n != 1 {
		t.Fatalf("barrier-transition wrote %d review-log rows, want exactly 1", n)
	}

	// rewrite vs rewrite: exactly one accepted replacement survives.
	rwID := seed("race-barrier-rewrite")
	_, rwLoser := barrierRace("barrier-rewrite", "rewrite", rwID,
		func() error { _, err := store.RewriteObservation(ctx, rwID, "barrier racer A", "b"); return err },
		func() error { _, err := store.RewriteObservation(ctx, rwID, "barrier racer B", "b"); return err })
	assertLoser("barrier-rewrite", rwLoser, "superseded")
	var rwChildren int
	if err := store.rdb.QueryRowContext(ctx,
		`SELECT count(*) FROM observations WHERE supersedes_id = ?`, rwID).Scan(&rwChildren); err != nil {
		t.Fatalf("barrier rewrite children: %v", err)
	}
	if rwChildren != 1 {
		t.Fatalf("barrier-rewrite: %d replacements survived, want exactly 1", rwChildren)
	}
	if n := logCount(rwID); n != 1 {
		t.Fatalf("barrier-rewrite wrote %d review-log rows on the original, want 1", n)
	}

	// elevation vs elevation: the loser must traverse setElevation's
	// zero-row fresh-re-read branch and report the winner's can_instruct=1
	// (round-3 review NEW MAJOR 2 — the stress race could serialize and
	// never reach it).
	elbID := seed("race-barrier-elevate")
	if _, err := store.ConfirmObservation(ctx, elbID, "barrier prep"); err != nil {
		t.Fatalf("barrier elevate prep: %v", err)
	}
	_, elLoser := barrierRace("barrier-elevate", "elevation", elbID,
		func() error { _, err := store.ElevateObservation(ctx, elbID, "barrier"); return err },
		func() error { _, err := store.ElevateObservation(ctx, elbID, "barrier"); return err })
	var elsce *StateConflictError
	if !errors.As(elLoser, &elsce) {
		t.Fatalf("barrier-elevate: loser must get StateConflictError, got: %v", elLoser)
	}
	if elsce.CanInstruct != 1 {
		t.Fatalf("barrier-elevate: loser carries stale can_instruct=%d, want fresh 1", elsce.CanInstruct)
	}
	if n := logCount(elbID); n != 2 { // confirm + exactly one elevate
		t.Fatalf("barrier-elevate: %d review-log rows, want 2", n)
	}

	// bulk lapse vs preconditioned accept, barrier-forced (cross-site):
	// exactly one wins the row; the bulk loser SKIPS (no fabricated log, an
	// honest zero count), the accept loser conflicts with fresh 'lapsed'.
	// Custom orchestration: LapseSessionRemaining "loses" with error==nil,
	// so the one-winner contract of barrierRace does not apply.
	crossSiteRace := func(name, bulkSite string, id int64, bulk func() (int, error)) (count int, acceptErr error) {
		t.Helper()
		arrived := make(chan struct{}, 2)
		proceed := make(chan struct{})
		reviewCASTestHook = func(s string, hid int64) {
			if (s == bulkSite || s == "transition") && hid == id {
				arrived <- struct{}{}
				<-proceed
			}
		}
		var bErr error
		var xwg sync.WaitGroup
		xwg.Add(2)
		go func() { defer xwg.Done(); count, bErr = bulk() }()
		go func() {
			defer xwg.Done()
			_, acceptErr = store.TransitionObservationReviewPre(ctx, id, "accepted", "barrier", &ReviewPrecondition{
				ReviewState: &proposedState, Provenance: &inferred, CanInstruct: &zero,
			})
		}()
		<-arrived
		<-arrived
		close(proceed)
		xwg.Wait()
		reviewCASTestHook = nil
		if bErr != nil {
			t.Fatalf("%s: bulk racer failed: %v", name, bErr)
		}
		return count, acceptErr
	}
	assertBulkOutcome := func(name string, id int64, count int, acceptErr error) {
		t.Helper()
		var st string
		if err := store.rdb.QueryRowContext(ctx,
			`SELECT review_state FROM observations WHERE id = ?`, id).Scan(&st); err != nil {
			t.Fatalf("%s fresh read: %v", name, err)
		}
		if acceptErr == nil {
			if st != "accepted" || count != 0 {
				t.Fatalf("%s: accept won but state=%s count=%d", name, st, count)
			}
		} else {
			var bsce *StateConflictError
			if !errors.As(acceptErr, &bsce) || bsce.ReviewState != "lapsed" {
				t.Fatalf("%s: accept loser must carry fresh lapsed, got: %v", name, acceptErr)
			}
			if st != "lapsed" || count != 1 {
				t.Fatalf("%s: lapse won but state=%s count=%d", name, st, count)
			}
		}
		if n := logCount(id); n != 1 {
			t.Fatalf("%s: %d review-log rows, want exactly 1", name, n)
		}
	}
	bulkbSession := "conf-race-bulk-barrier"
	bulkbID := func() int64 {
		rec, err := store.InsertDerivedObservation(ctx, DerivedObservationInput{
			Project: project, SessionID: bulkbSession, Kind: "fact", Text: "bulk barrier target",
			DerivationRunID: run.ID, ContentHash: "race-bulk-barrier", SourceEventIDs: []int64{ev.ID},
			ModelDerived: true, OccurredAtEpoch: now - 10,
		})
		if err != nil {
			t.Fatalf("bulk barrier seed: %v", err)
		}
		return rec.ID
	}()
	bCount, bAcceptErr := crossSiteRace("barrier-bulk", "bulk-lapse", bulkbID,
		func() (int, error) { return store.LapseSessionRemaining(ctx, bulkbSession, "barrier") })
	assertBulkOutcome("barrier-bulk", bulkbID, bCount, bAcceptErr)

	// TTL sweep vs preconditioned accept — the TTL loop is a distinct
	// guarded implementation and gets its own barrier proof.
	ttlbID := func() int64 {
		rec, err := store.InsertDerivedObservation(ctx, DerivedObservationInput{
			Project: project, SessionID: "conf-race-ttl-barrier", Kind: "fact", Text: "ttl barrier target",
			DerivationRunID: run.ID, ContentHash: "race-ttl-barrier", SourceEventIDs: []int64{ev.ID},
			ModelDerived: true, OccurredAtEpoch: now - 10,
		})
		if err != nil {
			t.Fatalf("ttl barrier seed: %v", err)
		}
		return rec.ID
	}()
	if _, err := store.db.ExecContext(ctx,
		`UPDATE observations SET created_at_epoch = created_at_epoch - 40*86400 WHERE id = ?`, ttlbID); err != nil {
		t.Fatalf("ttl barrier aging: %v", err)
	}
	tCount, tAcceptErr := crossSiteRace("barrier-ttl", "ttl-lapse", ttlbID,
		func() (int, error) { return store.LapseExpiredProposed(ctx, 21) })
	assertBulkOutcome("barrier-ttl", ttlbID, tCount, tAcceptErr)
}
