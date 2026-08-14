// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Hybrid read path (PORT-DESIGN §3, D218): RRF fusion of the FTS lane and
// the pgvector lane inside SearchWithFilter, behind a default-off flag.
// The vector lane is derived-only (kind != raw) and joins observations
// through the same observationWhere as FTS, so project scoping, review
// state, noise, and deletion visibility are identical by construction —
// the pilot's scoping gap cannot reopen here. Failure posture is fail-open:
// any embed or vector-lane error degrades to FTS-only, counted and logged,
// never a search outage.

// QueryEmbedder turns a raw query string into a vector at search time.
// Implemented by embedclient over the cluster-local embed service;
// injected at wiring so this package never speaks HTTP itself. The query
// is embedded RAW (no FTS sanitization) — syntax honesty carries over:
// operator-shaped tokens embed harmlessly.
type QueryEmbedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

const (
	// hybridLaneK is the per-lane candidate depth before fusion (the
	// piloted constant; a caller limit above it deepens both lanes).
	hybridLaneK = 50
	// rrfK is the reciprocal-rank-fusion smoothing constant (piloted).
	rrfK = 60
	// DefaultEmbedBudget bounds the in-search query-embed call; over
	// budget → fail open to FTS-only (PORT-DESIGN's ~150 ms line).
	DefaultEmbedBudget = 150 * time.Millisecond
)

// EnableHybrid wires the query embedder, the deployment-default flag, and
// the embed budget (0 → DefaultEmbedBudget). Wiring-time only, before the
// store serves requests — no lock guards these fields by design.
func (s *Store) EnableHybrid(e QueryEmbedder, defaultOn bool, budget time.Duration) {
	if budget <= 0 {
		budget = DefaultEmbedBudget
	}
	s.embedder = e
	s.hybridDefault = defaultOn
	s.embedBudget = budget
}

// HybridState names the lane state for /v1/health: "absent" (no wired
// embedder or no vector lane — fusion impossible), else "on"/"off" by the
// deployment default. Per-request overrides don't show here.
func (s *Store) HybridState() string {
	if s.embedder == nil || !s.hasPGVector || s.engine != EnginePostgres {
		return "absent"
	}
	if s.hybridDefault {
		return "on"
	}
	return "off"
}

// HybridFailOpenTotal is the process-lifetime count of hybrid searches
// that degraded to FTS-only (embed error/timeout or vector-lane error).
func (s *Store) HybridFailOpenTotal() int64 { return s.hybridFailOpen.Load() }

// hybridActive decides the lane for one search: the per-request override
// wins over the deployment default; either way the lane preconditions
// (postgres engine, provisioned pgvector, wired embedder) must hold.
func (s *Store) hybridActive(filter ObservationFilter) bool {
	want := s.hybridDefault
	if filter.Hybrid != nil {
		want = *filter.Hybrid
	}
	return want && s.engine == EnginePostgres && s.hasPGVector && s.embedder != nil
}

// exactIDPatterns names the literal-identifier shapes that route a query
// to the FTS-only path even when hybrid is active (D218 acceptance round
// 2, operator-ruled): a query carrying an exact id wants literal ranking,
// and fixed-depth RRF measurably dilutes it (fused top-10 ≈ FTS top-5 +
// vector top-5; recall sheet 2026-08-03). Shapes: UPPERCASE ticket ids
// (US-9299 — every fleet tracker is uppercase; lowercase would false-fire
// on utf-8/x86-64-style tokens), #-refs (#54), derivation run ids, and
// commit-sha hex (letter check in code — RE2 has no lookahead and a pure
// digit run is a quantity, not a sha). Queries WITHOUT a literal (e.g.
// D183 #22 "GlitchTip verdict…") keep full fusion — that genre is where
// the vector lane wins.
var (
	// Trailing \b so US-9299foo (an id-shaped PREFIX of a longer word)
	// does not trigger (round-1 F5).
	ticketIDRe = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-\d+\b`)
	// Ticket-shaped tokens that are actually technical standards, not
	// tracker ids (round-1 F5: UTF-8, SHA-256, ISO-8601, TLS-13 were
	// silently disabling the semantic lane). A heuristic denylist, not
	// exhaustive — a miss costs one query's fusion, never correctness.
	standardTokenRe = regexp.MustCompile(`^(UTF|SHA|ISO|TLS|RFC|AES|MD|CRC|HTTP|IPV|IEEE|ECMA|ANSI|X86|UTC|GMT|POSIX|OAUTH)-`)
	otherIDPatterns = []*regexp.Regexp{
		regexp.MustCompile(`#\d+\b`),
		regexp.MustCompile(`\bdrv_[a-z0-9]+\b`),
	}
	hexLiteralRe = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
)

func queryHasExactIDLiteral(q string) bool {
	for _, m := range ticketIDRe.FindAllString(q, -1) {
		if !standardTokenRe.MatchString(m) {
			return true
		}
	}
	for _, re := range otherIDPatterns {
		if re.MatchString(q) {
			return true
		}
	}
	for _, m := range hexLiteralRe.FindAllString(q, -1) {
		if strings.ContainsAny(m, "abcdef") {
			return true
		}
	}
	return false
}

// HybridLiteralSkipsTotal counts hybrid-active searches routed FTS-only
// by the exact-id guard (observability twin of the fail-open counter).
func (s *Store) HybridLiteralSkipsTotal() int64 { return s.hybridLiteralSkips.Load() }

// searchHybrid runs both lanes at depth K and fuses. rawQ feeds the
// embedder verbatim; match is the sanitized FTS input. Every failure past
// the FTS lane fails OPEN to the FTS result (counted + logged).
func (s *Store) searchHybrid(ctx context.Context, rawQ, match string, filter ObservationFilter) ([]SearchHit, error) {
	if queryHasExactIDLiteral(rawQ) {
		s.hybridLiteralSkips.Add(1)
		return s.searchFTS(ctx, match, filter, filter.Limit)
	}
	k := filter.Limit
	if k < hybridLaneK {
		k = hybridLaneK
	}
	ftsHits, err := s.searchFTS(ctx, match, filter, k)
	if err != nil {
		return nil, err
	}
	failOpen := func(stage string, cause error) []SearchHit {
		s.hybridFailOpen.Add(1)
		slog.Warn("hybrid search fail-open to FTS-only", "stage", stage, "err", cause)
		if len(ftsHits) > filter.Limit {
			return ftsHits[:filter.Limit]
		}
		return ftsHits
	}
	embedCtx, cancel := context.WithTimeout(ctx, s.embedBudget)
	defer cancel()
	vec, err := s.embedder.EmbedQuery(embedCtx, rawQ)
	if err != nil {
		return failOpen("embed", err), nil
	}
	if len(vec) != EmbedDim {
		return failOpen("embed", fmt.Errorf("query vector has %d dims, want %d", len(vec), EmbedDim)), nil
	}
	vecHits, err := s.searchVector(ctx, vec, filter, k)
	if err != nil {
		return failOpen("vector", err), nil
	}
	return rrfFuse(ftsHits, vecHits, filter.Limit), nil
}

// searchVector is the vector lane: nearest neighbors by cosine distance
// over CURRENT embeddings — model AND text_hash, the same currency rule
// as missingWhere (round-1 F1: a model-only filter served vectors for
// text the observation no longer contains, so hybrid could resurface a
// rewritten row by its OLD text; a stale row now stays out of the lane
// until the sweep re-embeds it). Joined through the same
// observationWhere as the FTS lane. Derived-only (kind != raw): raw rows
// reach results via FTS, which is exactly the division of labor D216
// measured. Runs in a read-only tx so the HNSW scan knobs are SET LOCAL
// — scoped to this query, invisible to the pool.
func (s *Store) searchVector(ctx context.Context, vec []float32, filter ObservationFilter, k int) ([]SearchHit, error) {
	where, filterArgs := observationWhere(filter, "o")
	lit := vectorLiteral(vec)
	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// HNSW yields at most ef_search candidates (default 40 < K) BEFORE the
	// join filters bite; iterative scan (pgvector >= 0.8) keeps fetching
	// until LIMIT survives the filters — without it a project-scoped query
	// could return near-empty from a well-filled index. strict_order keeps
	// exact distance order: RRF consumes ranks, so order is semantics.
	if _, err := tx.ExecContext(ctx, `SET LOCAL hnsw.ef_search = 150`); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL hnsw.iterative_scan = strict_order`); err != nil {
		return nil, err
	}
	// ORDER BY must be the bare distance expression — any secondary sort
	// key would force a full sort instead of the index-ordered scan. Rank
	// carries the cosine distance (lower = closer); fusion replaces it.
	// The snippet column is a plain prefix: vector matches have no term
	// hits to bracket, and clients already fall back to text.
	args := []any{lit, DefaultEmbedModel}
	args = append(args, filterArgs...)
	args = append(args, lit, k)
	rows, err := tx.QueryContext(ctx, `SELECT o.id, COALESCE(o.session_id, ''), o.project,
	o.kind, o.review_state, COALESCE(o.source_adapter, ''), COALESCE(o.event_type, ''),
	o.text, left(o.text, 200), o.occurred_at_epoch, (e.embedding <=> ?::vector)::float8,
	o.noise_candidate, o.can_instruct, COALESCE(o.provenance, '')
	FROM observation_embeddings_pg e
	JOIN observations o ON o.id = e.observation_id
	WHERE e.model = ?
	  AND e.text_hash = encode(sha256(convert_to(o.text, 'UTF8')), 'hex')
	  AND o.kind <> 'raw' AND `+where+`
	ORDER BY e.embedding <=> ?::vector
	LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.ID, &h.SessionID, &h.Project, &h.Kind, &h.ReviewState, &h.SourceAdapter, &h.EventType, &h.Text, &h.Snippet, &h.OccurredAtEpoch, &h.Rank, &h.NoiseCandidate, &h.CanInstruct, &h.Provenance); err != nil {
			return nil, err
		}
		h.syncOccurredAt()
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// rrfFuse merges the two lanes by reciprocal rank fusion:
// score(d) = Σ_lanes 1/(rrfK + rank_lane(d)), rank 1-based. The FTS hit
// struct wins for dual-lane documents (it carries the real snippet); the
// fused Rank is the RRF score (higher = better). Ties break by id
// descending (newest first, matching the FTS lanes' tiebreak).
func rrfFuse(fts, vec []SearchHit, limit int) []SearchHit {
	type fused struct {
		hit   SearchHit
		score float64
	}
	byID := make(map[int64]*fused, len(fts)+len(vec))
	order := make([]int64, 0, len(fts)+len(vec))
	for _, hits := range [][]SearchHit{fts, vec} {
		for i, h := range hits {
			f, ok := byID[h.ID]
			if !ok {
				f = &fused{hit: h}
				byID[h.ID] = f
				order = append(order, h.ID)
			}
			f.score += 1.0 / float64(rrfK+i+1)
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		fa, fb := byID[order[a]], byID[order[b]]
		if fa.score != fb.score {
			return fa.score > fb.score
		}
		return order[a] > order[b]
	})
	if limit > 0 && len(order) > limit {
		order = order[:limit]
	}
	out := make([]SearchHit, 0, len(order))
	for _, id := range order {
		f := byID[id]
		f.hit.Rank = f.score
		out = append(out, f.hit)
	}
	return out
}
