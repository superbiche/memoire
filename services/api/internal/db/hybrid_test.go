// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type stubEmbedder struct {
	vec   []float32
	err   error
	calls int
}

func (s *stubEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.vec, nil
}

// unitVec is a 384-dim one-hot vector — cosine distances between one-hots
// are exactly 0 (same axis) or 1 (different axes), so lane ordering in the
// PG test is deterministic by construction.
func unitVec(axis int) []float32 {
	v := make([]float32, EmbedDim)
	v[axis] = 1
	return v
}

func TestRRFFuse(t *testing.T) {
	mk := func(id int64, snippet string) SearchHit {
		// The run id rides along so fusion's whole-hit copy is checked on
		// the field lineage-at-retrieval keys on (Fibery #335).
		return SearchHit{ID: id, Snippet: snippet, DerivationRunID: fmt.Sprintf("drv_%d", id)}
	}
	fts := []SearchHit{mk(1, "fts-1"), mk(2, "fts-2"), mk(3, "fts-3")}
	vec := []SearchHit{mk(2, "vec-2"), mk(4, "vec-4")}

	got := rrfFuse(fts, vec, 10)
	// Scores: id2 = 1/62+1/61 (both lanes), id1 = 1/61, id4 = 1/62, id3 = 1/63.
	wantOrder := []int64{2, 1, 4, 3}
	if len(got) != len(wantOrder) {
		t.Fatalf("fused len = %d, want %d", len(got), len(wantOrder))
	}
	for i, id := range wantOrder {
		if got[i].ID != id {
			t.Fatalf("fused[%d].ID = %d, want %d (full: %+v)", i, got[i].ID, id, got)
		}
	}
	// Fusion copies whole hits, so derivation_run_id survives it.
	for _, h := range got {
		if want := fmt.Sprintf("drv_%d", h.ID); h.DerivationRunID != want {
			t.Fatalf("fused hit %d: derivation_run_id = %q, want %q", h.ID, h.DerivationRunID, want)
		}
	}
	// Dual-lane hit keeps the FTS struct (real snippet), Rank is the RRF score.
	if got[0].Snippet != "fts-2" {
		t.Fatalf("dual-lane hit snippet = %q, want the FTS lane's", got[0].Snippet)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Rank < got[i].Rank {
			t.Fatalf("fused ranks not descending at %d: %v then %v", i, got[i-1].Rank, got[i].Rank)
		}
	}

	// Equal scores tie-break by id descending (newest first).
	tied := rrfFuse([]SearchHit{mk(10, "")}, []SearchHit{mk(20, "")}, 10)
	if tied[0].ID != 20 || tied[1].ID != 10 {
		t.Fatalf("tie-break: got %d,%d want 20,10", tied[0].ID, tied[1].ID)
	}

	// Limit truncates after fusion.
	if cut := rrfFuse(fts, vec, 2); len(cut) != 2 || cut[0].ID != 2 || cut[1].ID != 1 {
		t.Fatalf("limit cut: got %+v", cut)
	}
}

// The exact-id guard must trigger on exactly the literal-carrying queries
// of the frozen D183 reference set (2,7,9,15,17,29) and never on the
// blind/context queries — a false trigger there silently forfeits the
// panel-demonstrated ranking win.
func TestExactIDGuardOnReferenceQueries(t *testing.T) {
	// Synthetic leg FIRST: these assertions need no private data and must
	// run in the curated export too (export review r1 F9 — the earlier
	// shape skipped the whole test, so the standalone suite reported the
	// guard green without judging it).
	syntheticExactIDGuardAssertions(t)

	data, err := os.ReadFile("../../../../docs/research/vectors/reference-queries.tsv")
	if os.IsNotExist(err) {
		// The reference panel is a home-side research fixture (real memory
		// content) and is not in the curated export (memoire:D226) — skip
		// only the panel leg, loudly; the synthetic leg above already ran.
		t.Skip("reference-queries.tsv absent (home-side fixture, not shipped) — panel leg skipped")
	}
	if err != nil {
		t.Fatalf("read reference queries: %v", err)
	}
	wantLiteral := map[string]bool{"2": true, "7": true, "9": true, "15": true, "17": true, "29": true}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		id, query := f[0], f[7]
		if got := queryHasExactIDLiteral(query); got != wantLiteral[id] {
			t.Errorf("#%s: guard = %v, want %v (query: %s)", id, got, wantLiteral[id], query)
		}
	}
}

// Synthetic shapes beyond the frozen set — reference-independent, runs in
// both the home suite and the curated export.
func syntheticExactIDGuardAssertions(t *testing.T) {
	t.Helper()
	for q, want := range map[string]bool{
		"what broke in commit 25248a8":         true,  // sha hex
		"derivation run drv_01hxyz failure":    true,  // run id
		"GO-2026-5970 vulnerability review":    true,  // advisory id
		"utf-8 encoding issue in x86-64 build": false, // lowercase dash tokens are not tickets
		"deploy 1234567 rows in 3DS flow":      false, // pure digits: quantity, not sha
		// Round-1 F5: uppercase standards and id-prefixed words must not
		// silently disable the semantic lane.
		"UTF-8 encoding of observation text": false,
		"SHA-256 digest verification":        false,
		"ISO-8601 timestamp parsing":         false,
		"TLS-13 handshake failures":          false,
		"US-9299foo is not a ticket":         false,
		"OAUTH-2 flow for the review UI":     false,
		"mixed UTF-8 text but also US-1234":  true, // a real id wins over nearby standards
	} {
		if got := queryHasExactIDLiteral(q); got != want {
			t.Errorf("guard(%q) = %v, want %v", q, got, want)
		}
	}
}

// The SQLite engine can never activate the hybrid lane, even with an
// embedder wired and the default flag on: the engine precondition wins and
// the embedder must not be consulted.
func TestHybridInactiveOnSQLite(t *testing.T) {
	t.Setenv("MEMOIRE_HOME", t.TempDir())
	store, err := Open(filepath.Join(t.TempDir(), "test.db"), schemaDirForTest(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	stub := &stubEmbedder{vec: unitVec(0)}
	store.EnableHybrid(stub, true, 0)
	if got := store.HybridState(); got != "absent" {
		t.Fatalf("HybridState() = %q, want absent on sqlite", got)
	}
	hy := true
	if _, err := store.SearchWithFilter(context.Background(), "anything", ObservationFilter{Hybrid: &hy}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if stub.calls != 0 {
		t.Fatalf("embedder consulted %d times on sqlite, want 0", stub.calls)
	}
}

// Postgres leg: real pgvector lane end-to-end — provisioning, fusion,
// per-request override, and the fail-open posture. Skips without
// MEMOIRE_TEST_PG_URL or a server that lacks the vector extension.
func TestHybridSearchPG(t *testing.T) {
	ctx := context.Background()
	dsn, _ := conformancePGURL(t)

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
	var avail int
	if err := raw.QueryRow(`SELECT count(*) FROM pg_available_extensions WHERE name = 'vector'`).Scan(&avail); err != nil {
		t.Fatalf("probe vector availability: %v", err)
	}
	if avail == 0 {
		t.Skip("server has no pgvector extension; hybrid PG leg skipped")
	}
	if _, err := raw.Exec(`CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Fatalf("create vector extension: %v", err)
	}
	if err := ProvisionVectorSchema(ctx, raw); err != nil {
		t.Fatalf("provision vector schema: %v", err)
	}

	store, err := Open(dsn, schemaDirForTest(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if !store.HasPGVector() {
		t.Fatal("store does not report the provisioned vector lane")
	}

	// Seed: one event, one run, two derived (accepted) observations.
	project, session := "hybrid-test", "sess-hybrid"
	now := time.Now().Unix()
	evt, err := store.Ingest(ctx, EventInput{
		Project: project, SessionID: session, SourceAdapter: "claude-code",
		SourceEventID: "hyb-evt-1", EventType: "claude_PostToolUse",
		OccurredAtEpoch: now - 100,
		Payload:         json.RawMessage(`{"tool_name":"Read"}`),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	run, err := store.EnsureDerivationRun(ctx, DerivationRunInput{
		Project: project, SessionID: session, Provider: "rules", DataBoundary: "local",
		DerivationVersion: "v1", InputHash: "hyb-hash-1",
		WindowStartEpoch: now - 200, WindowEndEpoch: now,
	})
	if err != nil {
		t.Fatalf("ensure run: %v", err)
	}
	var obsIDs []int64
	texts := []string{
		"kubernetes ingress gateway routing decision",
		"postgres vacuum maintenance cadence note",
	}
	for i, text := range texts {
		rec, err := store.InsertDerivedObservation(ctx, DerivedObservationInput{
			Project: project, SessionID: session, Kind: "fact", Text: text,
			DerivationRunID: run.ID, ContentHash: fmt.Sprintf("hyb-hash-obs-%d", i),
			SourceEventIDs: []int64{evt.ID}, OccurredAtEpoch: now - int64(50-i),
		})
		if err != nil {
			t.Fatalf("insert obs %d: %v", i, err)
		}
		obsIDs = append(obsIDs, rec.ID)
	}
	// One-hot embeddings: obs0 on axis 0, obs1 on axis 1. A query vector on
	// axis 1 puts obs1 at distance 0 and obs0 at distance 1. TextHash must
	// be the REAL sha256 of the text: the search path enforces the same
	// currency rule as the sweep (round-1 F1), so a stale hash is out of
	// the lane, not silently served.
	hashOf := func(text string) string {
		sum := sha256.Sum256([]byte(text))
		return hex.EncodeToString(sum[:])
	}
	if err := store.UpsertObservationEmbeddings(ctx, []EmbeddingUpsert{
		{ObservationID: obsIDs[0], Model: DefaultEmbedModel, Vector: unitVec(0), TextHash: hashOf(texts[0]), EpochNow: now},
		{ObservationID: obsIDs[1], Model: DefaultEmbedModel, Vector: unitVec(1), TextHash: hashOf(texts[1]), EpochNow: now},
	}); err != nil {
		t.Fatalf("upsert embeddings: %v", err)
	}

	stub := &stubEmbedder{vec: unitVec(1)}
	store.EnableHybrid(stub, false, 0) // deployment default OFF
	if got := store.HybridState(); got != "off" {
		t.Fatalf("HybridState() = %q, want off", got)
	}

	// Default off: no override → FTS-only, embedder untouched.
	hits, err := store.SearchWithFilter(ctx, "kubernetes", ObservationFilter{Project: project, Limit: 10})
	if err != nil {
		t.Fatalf("fts search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != obsIDs[0] {
		t.Fatalf("fts search: want exactly obs0, got %+v", hits)
	}
	if stub.calls != 0 {
		t.Fatalf("embedder consulted with hybrid off (%d calls)", stub.calls)
	}

	// Per-request hybrid=1: the FTS miss obs1 arrives through the vector
	// lane; the FTS hit obs0 survives fusion.
	hy := true
	hits, err = store.SearchWithFilter(ctx, "kubernetes", ObservationFilter{Project: project, Limit: 10, Hybrid: &hy})
	if err != nil {
		t.Fatalf("hybrid search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hybrid search: want 2 fused hits, got %+v", hits)
	}
	found := map[int64]SearchHit{}
	for _, h := range hits {
		found[h.ID] = h
	}
	if _, ok := found[obsIDs[0]]; !ok {
		t.Fatalf("hybrid search lost the FTS hit: %+v", hits)
	}
	vecHit, ok := found[obsIDs[1]]
	if !ok {
		t.Fatalf("hybrid search missing the vector-lane hit: %+v", hits)
	}
	if vecHit.Snippet == "" {
		t.Fatal("vector-lane hit has no snippet (want text prefix)")
	}
	// The vector lane projects derivation_run_id like every other lane, so
	// a fused hit carries its run whichever lane found it (Fibery #335).
	if vecHit.DerivationRunID != run.ID {
		t.Fatalf("vector-lane hit derivation_run_id = %q, want %q", vecHit.DerivationRunID, run.ID)
	}
	if ftsHit := found[obsIDs[0]]; ftsHit.DerivationRunID != run.ID {
		t.Fatalf("fused FTS hit derivation_run_id = %q, want %q", ftsHit.DerivationRunID, run.ID)
	}
	if stub.calls != 1 {
		t.Fatalf("embedder calls = %d, want 1", stub.calls)
	}

	// Project scoping holds on the vector lane: same query, other project.
	if hits, err := store.SearchWithFilter(ctx, "kubernetes vacuum", ObservationFilter{Project: "other", Limit: 10, Hybrid: &hy}); err != nil || len(hits) != 0 {
		t.Fatalf("vector-lane scoping: want 0 hits in other project, got %+v err %v", hits, err)
	}

	// Exact-id guard: a literal-carrying query routes FTS-only even with
	// hybrid forced on — embedder untouched, skip counter moves.
	skipsBefore := store.HybridLiteralSkipsTotal()
	callsBefore := stub.calls
	hits, err = store.SearchWithFilter(ctx, "US-1234 kubernetes", ObservationFilter{Project: project, Limit: 10, Hybrid: &hy})
	if err != nil {
		t.Fatalf("guarded search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != obsIDs[0] {
		t.Fatalf("guarded search: want the FTS result alone, got %+v", hits)
	}
	if stub.calls != callsBefore {
		t.Fatalf("embedder consulted on a literal query (%d -> %d calls)", callsBefore, stub.calls)
	}
	if got := store.HybridLiteralSkipsTotal(); got != skipsBefore+1 {
		t.Fatalf("literal skip counter = %d, want %d", got, skipsBefore+1)
	}

	// Fail-open: embed error degrades to FTS-only, counted, never an error.
	before := store.HybridFailOpenTotal()
	stub.err = errors.New("embed service down")
	hits, err = store.SearchWithFilter(ctx, "kubernetes", ObservationFilter{Project: project, Limit: 10, Hybrid: &hy})
	if err != nil {
		t.Fatalf("fail-open search errored: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != obsIDs[0] {
		t.Fatalf("fail-open: want the FTS result alone, got %+v", hits)
	}
	if got := store.HybridFailOpenTotal(); got != before+1 {
		t.Fatalf("fail-open counter = %d, want %d", got, before+1)
	}

	// Wrong-dimension query vector fails open the same way.
	stub.err = nil
	stub.vec = []float32{1, 2, 3}
	if hits, err := store.SearchWithFilter(ctx, "kubernetes", ObservationFilter{Project: project, Limit: 10, Hybrid: &hy}); err != nil || len(hits) != 1 {
		t.Fatalf("dim-mismatch fail-open: got %+v err %v", hits, err)
	}
	if got := store.HybridFailOpenTotal(); got != before+2 {
		t.Fatalf("fail-open counter after dim mismatch = %d, want %d", got, before+2)
	}

	// Currency (round-1 F1): a stale text_hash removes the row from the
	// vector lane — the fused result degrades to FTS content instead of
	// serving a vector for text the observation no longer carries.
	stub.vec = unitVec(1)
	if err := store.UpsertObservationEmbeddings(ctx, []EmbeddingUpsert{
		{ObservationID: obsIDs[1], Model: DefaultEmbedModel, Vector: unitVec(1), TextHash: "stale", EpochNow: now},
	}); err != nil {
		t.Fatalf("stale upsert: %v", err)
	}
	hits, err = store.SearchWithFilter(ctx, "kubernetes", ObservationFilter{Project: project, Limit: 10, Hybrid: &hy})
	if err != nil {
		t.Fatalf("stale-hash search: %v", err)
	}
	for _, h := range hits {
		if h.ID == obsIDs[1] {
			t.Fatalf("stale-hash row served through the vector lane: %+v", hits)
		}
	}
	// And the freshness surface counts it as missing again.
	fresh, err := store.EmbeddingFreshnessStats(ctx, DefaultEmbedModel)
	if err != nil || fresh.Missing != 1 {
		t.Fatalf("freshness after staleness: %+v err %v", fresh, err)
	}
}

// Provisioning must refuse to certify a same-name malformed table
// (round-1 F3): IF NOT EXISTS matches by name only, so the postcondition
// carries the shape contract.
func TestVectorProvisionRejectsMalformed(t *testing.T) {
	ctx := context.Background()
	dsn, _ := conformancePGURL(t)
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
	var avail int
	if err := raw.QueryRow(`SELECT count(*) FROM pg_available_extensions WHERE name = 'vector'`).Scan(&avail); err != nil || avail == 0 {
		t.Skip("server has no pgvector extension")
	}
	if _, err := raw.Exec(`CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Fatalf("create extension: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE observation_embeddings_pg (
		observation_id BIGINT PRIMARY KEY,
		embedding vector(384) NOT NULL)`); err != nil {
		t.Fatalf("create malformed table: %v", err)
	}
	if _, err := raw.Exec(`CREATE INDEX idx_obs_emb_hnsw
		ON observation_embeddings_pg USING hnsw (embedding vector_cosine_ops)`); err != nil {
		t.Fatalf("create index: %v", err)
	}
	if err := ProvisionVectorSchema(ctx, raw); err == nil {
		t.Fatal("provisioning certified a malformed table")
	}
	store, err := Open(dsn, schemaDirForTest(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if store.HasPGVector() {
		t.Fatal("Open-time probe accepted the malformed table")
	}
}

// Round-2 F3 delta: a near-valid table — all six ratified columns, PK,
// HNSW index — whose cascading FK sits on a DECOY column instead of
// observation_id must also refuse: deleting an observation would not
// cascade through the lane's actual join key.
func TestVectorProvisionRejectsDecoyFK(t *testing.T) {
	ctx := context.Background()
	dsn, _ := conformancePGURL(t)
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
	var avail int
	if err := raw.QueryRow(`SELECT count(*) FROM pg_available_extensions WHERE name = 'vector'`).Scan(&avail); err != nil || avail == 0 {
		t.Skip("server has no pgvector extension")
	}
	if _, err := raw.Exec(`CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Fatalf("create extension: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE observation_embeddings_pg (
		observation_id    BIGINT PRIMARY KEY,
		model             TEXT   NOT NULL,
		dim               INT    NOT NULL,
		embedding         vector(384) NOT NULL,
		text_hash         TEXT   NOT NULL,
		embedded_at_epoch BIGINT NOT NULL,
		decoy_observation_id BIGINT NOT NULL
			REFERENCES observations(id) ON DELETE CASCADE)`); err != nil {
		t.Fatalf("create decoy table: %v", err)
	}
	if _, err := raw.Exec(`CREATE INDEX idx_obs_emb_hnsw
		ON observation_embeddings_pg USING hnsw (embedding vector_cosine_ops)`); err != nil {
		t.Fatalf("create index: %v", err)
	}
	if err := ProvisionVectorSchema(ctx, raw); err == nil {
		t.Fatal("provisioning certified a table whose cascade rides a decoy column")
	}
	store, err := Open(dsn, schemaDirForTest(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if store.HasPGVector() {
		t.Fatal("Open-time probe accepted the decoy-FK table")
	}
}
