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
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"memoire/services/api/internal/authctx"
	"memoire/services/api/internal/contenthash"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	// db is the write handle. SQLite: the sole write authority with
	// MaxOpenConns(1), so writes serialize among themselves and SQLITE_BUSY
	// between in-process writers is impossible by construction; reads
	// embedded in write flows (inside transactions or between a write and
	// its read-back) also use it. Postgres: an ordinary pool — MVCC makes
	// the single-writer discipline unnecessary; the dedup/CAS statements
	// this store relies on (ON CONFLICT arbiter, compare-and-swap UPDATEs)
	// are atomic per statement on both engines.
	db dbc
	// rdb is the standalone-read handle. SQLite: a separate read-only pool
	// (mode=ro, query_only); WAL guarantees a fresh read transaction sees
	// the latest committed write, so read-your-committed-write across
	// handles holds. Postgres: the same pool as db — read-committed
	// visibility gives the same guarantee without a second pool.
	rdb dbc
	// engine tags the dialect for the few sites that must branch
	// (search, health-count predicates, stats, index hints).
	engine Engine
	// hasPGSearch is true when the Postgres deployment carries the
	// pg_search (ParadeDB) extension: search then uses its real BM25
	// ranking (the 2026-07-22 injection-quality gate ruling, option A).
	// Plain-Postgres deployments fall back to tsvector ranking — pg_search
	// is a quality upgrade, never a hard dependency (D157 release
	// consideration).
	hasPGSearch bool
	// hasPGVector is true when the Postgres deployment carries the
	// provisioned vector lane (pgvector table + HNSW index, D218).
	// Absence is graceful everywhere — hybrid fusion simply stays off.
	hasPGVector bool
	// embedder turns raw query strings into vectors at search time
	// (hybrid lane, D218). nil until EnableHybrid wires one; nil keeps
	// SearchWithFilter FTS-only regardless of any flag.
	embedder QueryEmbedder
	// hybridDefault is the deployment-level default for hybrid fusion
	// (MEMOIRE_HYBRID_SEARCH). Per-request Hybrid overrides win; the
	// default stays OFF until the D218 acceptance gate passes.
	hybridDefault bool
	// embedBudget bounds the query-embed call inside a search; over
	// budget → fail open to FTS-only (never a search outage).
	embedBudget time.Duration
	// hybridFailOpen counts hybrid searches that fell back to FTS-only
	// (embed error/timeout or vector-lane error). Surfaced on /v1/health
	// so a dead embed service is visible, not silent.
	hybridFailOpen atomic.Int64
	// hybridLiteralSkips counts hybrid-active searches the exact-id guard
	// routed to FTS-only (a policy skip, not a failure — separate counter
	// so the two motives never blur on the health surface).
	hybridLiteralSkips atomic.Int64
	// path is the SQLite file path ("" on Postgres); Stats sizes the DB
	// from it.
	path string
}

// Engine reports which SQL engine this store speaks.
func (s *Store) Engine() Engine { return s.engine }

type EventInput struct {
	SchemaVersion    int             `json:"schema_version,omitempty"`
	Project          string          `json:"project"`
	SessionID        string          `json:"session_id"`
	ContentSessionID string          `json:"content_session_id,omitempty"`
	SourceAdapter    string          `json:"source_adapter"`
	ProducerID       string          `json:"producer_id,omitempty"`
	SourceEventID    string          `json:"source_event_id,omitempty"`
	EventType        string          `json:"event_type"`
	OccurredAt       string          `json:"occurred_at,omitempty"`
	OccurredAtEpoch  int64           `json:"occurred_at_epoch,omitempty"`
	ReceivedAtEpoch  int64           `json:"received_at_epoch,omitempty"`
	CWD              string          `json:"cwd,omitempty"`
	Payload          json.RawMessage `json:"payload"`
	Text             string          `json:"text,omitempty"`
	PrincipalID      string          `json:"principal_id,omitempty"`
	Metadata         json.RawMessage `json:"metadata,omitempty"`
	RequestID        string          `json:"request_id,omitempty"`
}

type EventRecord struct {
	ID              int64           `json:"id"`
	IdempotencyKey  string          `json:"-"`
	SessionID       string          `json:"session_id"`
	Project         string          `json:"project"`
	SourceAdapter   string          `json:"source_adapter"`
	SourceEventID   string          `json:"source_event_id,omitempty"`
	EventType       string          `json:"event_type"`
	CWD             string          `json:"cwd,omitempty"`
	Payload         json.RawMessage `json:"payload"`
	Text            string          `json:"text,omitempty"`
	OccurredAtEpoch int64           `json:"occurred_at_epoch"`
	ReceivedAtEpoch int64           `json:"received_at_epoch"`
	PrincipalID     string          `json:"principal_id,omitempty"`
}

type DerivationEventFilter struct {
	Project          string   `json:"project"`
	SessionID        string   `json:"session_id,omitempty"`
	WindowStartEpoch int64    `json:"window_start_epoch,omitempty"`
	WindowEndEpoch   int64    `json:"window_end_epoch,omitempty"`
	EventTypes       []string `json:"event_types,omitempty"`
	// MaxEventID bounds the fetch to rows that existed at snapshot time
	// (memoire:D165 round-2 HIGH-1): the orchestrator reads MaxEventID()
	// FIRST, fetches with that bound, and records it on the run row — so
	// "was in this run's input" is an exact id comparison, not a clock
	// heuristic. Zero means unbounded (legacy callers).
	MaxEventID int64 `json:"max_event_id,omitempty"`
	// ExcludeCovered drops events already recorded in a COMPLETED run's
	// input manifest (run_input_events), same coverage rule as
	// OutstandingDeriveWork. Derivation callers set it so a sweep window
	// over a still-active session never re-feeds covered islands to the
	// model — re-derivation emits paraphrases whose fresh content_hash
	// defeats exact-hash dedupe (the D210-era duplicate factory). Failed
	// and manifest-less runs provide no coverage, so their events stay
	// eligible.
	ExcludeCovered bool `json:"exclude_covered,omitempty"`
}

type ObservationRecord struct {
	ID                int64   `json:"id"`
	EventID           int64   `json:"event_id,omitempty"`
	Project           string  `json:"project"`
	SessionID         string  `json:"session_id,omitempty"`
	Kind              string  `json:"kind"`
	Text              string  `json:"text"`
	SourceAdapter     string  `json:"source_adapter,omitempty"`
	EventType         string  `json:"event_type,omitempty"`
	OccurredAtEpoch   int64   `json:"occurred_at_epoch"`
	CreatedAtEpoch    int64   `json:"created_at_epoch"`
	DeletedAtEpoch    int64   `json:"deleted_at_epoch,omitempty"`
	PrincipalID       string  `json:"principal_id,omitempty"`
	Confidence        float64 `json:"confidence,omitempty"`
	ReviewState       string  `json:"review_state"`
	DerivationRunID   string  `json:"derivation_run_id,omitempty"`
	ContentHash       string  `json:"content_hash,omitempty"`
	SourceEventIDs    []int64 `json:"source_event_ids,omitempty"`
	Provenance        string  `json:"provenance,omitempty"`
	CanInstruct       int     `json:"can_instruct"`
	CanEvidence       int     `json:"can_evidence"`
	NeedsConfirmation int     `json:"needs_confirmation"`
	NoiseCandidate    int     `json:"noise_candidate"`
}

type ObservationReviewLogRecord struct {
	ObservationID   int64  `json:"observation_id"`
	FromState       string `json:"from_state"`
	ToState         string `json:"to_state"`
	Actor           string `json:"actor"`
	ReasonText      string `json:"reason_text,omitempty"`
	OccurredAtEpoch int64  `json:"occurred_at_epoch"`
	Action          string `json:"action,omitempty"`
}

type DerivationRunInput struct {
	ID                string          `json:"id,omitempty"`
	Project           string          `json:"project"`
	SessionID         string          `json:"session_id,omitempty"`
	Provider          string          `json:"provider"`
	Model             string          `json:"model,omitempty"`
	EndpointID        string          `json:"endpoint_id,omitempty"`
	DataBoundary      string          `json:"data_boundary"`
	DerivationVersion string          `json:"derivation_version"`
	PromptVersion     string          `json:"prompt_version,omitempty"`
	InputHash         string          `json:"input_hash"`
	WindowStartEpoch  int64           `json:"window_start_epoch"`
	WindowEndEpoch    int64           `json:"window_end_epoch"`
	Status            string          `json:"status,omitempty"`
	CreatedAtEpoch    int64           `json:"created_at_epoch,omitempty"`
	CompletedAtEpoch  int64           `json:"completed_at_epoch,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
	// SnapshotMaxEventID is the store's MAX(events.id) read BEFORE the
	// run's input fetch, with the fetch bounded by it. DIAGNOSTIC ONLY
	// (memoire:D165 rounds 2-4): a PG identity id is allocated before its
	// transaction commits, so this can NEVER prove input membership —
	// coverage authority is the run_input_events manifest. Zero = not
	// recorded.
	SnapshotMaxEventID int64 `json:"snapshot_max_event_id,omitempty"`
	// StaleRunningRetrySeconds is the age ceiling past which an existing
	// `running` run for the same window is treated as abandoned (its process
	// was killed mid-call: systemd timeout, reboot, OOM) and retried in place
	// like a failed run. Zero applies DefaultStaleRunningRetrySeconds. Callers
	// that know the execute timeout should pass 2× that value.
	StaleRunningRetrySeconds int64 `json:"stale_running_retry_seconds,omitempty"`
	// Force mints a NEW run for a window that would otherwise dedup (prompt or
	// model A/B reruns; operator flag -force-rerun). The idempotency index is
	// UNIQUE over the full window key including input_hash and the schema is
	// additive-only, so the forced run stores the canonical input hash with a
	// visible "#forced-<nonce>" suffix; meta_json records forced=true, the
	// canonical hash, and the run it supersedes. Default idempotency is
	// untouched: an unforced caller still dedups against the original run.
	Force bool `json:"force,omitempty"`
}

// DefaultStaleRunningRetrySeconds is the fallback stale-`running` ceiling when
// the caller records no execute timeout (turn-ingest campaign Phase 1, slice
// 3): without recovery, a derive killed mid-call watermarks its window
// forever, because running rows dedup unconditionally.
const DefaultStaleRunningRetrySeconds = 2 * 60 * 60

type DerivationRunResult struct {
	ID             string
	CreatedAtEpoch int64
	Duplicate      bool
	// Status is the EXISTING run's status when Duplicate is true (empty on a
	// fresh mint — the caller chose the initial status). Lets intent
	// bookkeeping distinguish a completed-run dedup (serves the intent) from
	// a running one (does not) — memoire:D165 round-1 MED-3.
	Status     string
	RetryCount int // Non-zero when a failed or stale-running run was retried in-place (retry_count in meta_json).
	// Forced is true when the run was minted by DerivationRunInput.Force;
	// SupersededRunID names the pre-existing run for the same window, when one
	// existed.
	Forced          bool
	SupersededRunID string
}

type DerivationRunRecord struct {
	ID                       string          `json:"id"`
	Project                  string          `json:"project"`
	SessionID                string          `json:"session_id,omitempty"`
	Provider                 string          `json:"provider"`
	Model                    string          `json:"model,omitempty"`
	EndpointID               string          `json:"endpoint_id,omitempty"`
	DataBoundary             string          `json:"data_boundary"`
	DerivationVersion        string          `json:"derivation_version"`
	PromptVersion            string          `json:"prompt_version,omitempty"`
	InputHash                string          `json:"input_hash"`
	WindowStartEpoch         int64           `json:"window_start_epoch"`
	WindowEndEpoch           int64           `json:"window_end_epoch"`
	Status                   string          `json:"status"`
	CreatedAtEpoch           int64           `json:"created_at_epoch"`
	CompletedAtEpoch         int64           `json:"completed_at_epoch,omitempty"`
	Metadata                 json.RawMessage `json:"metadata,omitempty"`
	ObservationCount         int64           `json:"observation_count"`
	ProposedObservationCount int64           `json:"proposed_observation_count"`
}

type DerivationRunFilter struct {
	Project    string `json:"project,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	EndpointID string `json:"endpoint_id,omitempty"`
	Status     string `json:"status,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

// DerivationRunStatusUpdate carries a post-provider-call status and metadata
// update for an existing derivation run. It is additive: it never creates a run
// and never rewrites the idempotency-bearing columns.
type DerivationRunStatusUpdate struct {
	ID               string          `json:"id"`
	Status           string          `json:"status"`
	CompletedAtEpoch int64           `json:"completed_at_epoch,omitempty"`
	Metadata         json.RawMessage `json:"metadata,omitempty"`
}

type DerivedObservationInput struct {
	Project           string          `json:"project"`
	SessionID         string          `json:"session_id,omitempty"`
	Kind              string          `json:"kind"`
	Text              string          `json:"text"`
	OccurredAtEpoch   int64           `json:"occurred_at_epoch,omitempty"`
	CreatedAtEpoch    int64           `json:"created_at_epoch,omitempty"`
	PrincipalID       string          `json:"principal_id,omitempty"`
	Confidence        *float64        `json:"confidence,omitempty"`
	SourceAdapter     string          `json:"source_adapter,omitempty"`
	EventType         string          `json:"event_type,omitempty"`
	ReviewState       string          `json:"review_state,omitempty"`
	DerivationRunID   string          `json:"derivation_run_id"`
	ContentHash       string          `json:"content_hash"`
	SourceEventIDs    []int64         `json:"source_event_ids"`
	ModelDerived      bool            `json:"model_derived,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
	Provenance        string          `json:"provenance,omitempty"`
	CanInstruct       int             `json:"can_instruct,omitempty"`
	CanEvidence       int             `json:"can_evidence,omitempty"`
	NeedsConfirmation int             `json:"needs_confirmation,omitempty"`
	NoiseCandidate    bool            `json:"noise_candidate,omitempty"`
}

type DerivedObservationResult struct {
	ID                     int64
	ReviewState            string
	DowngradedFromAccepted bool
}

type SessionRecord struct {
	SessionID           string `json:"session_id"`
	Project             string `json:"project"`
	Client              string `json:"client"`
	StartedAtEpoch      int64  `json:"started_at_epoch"`
	EndedAtEpoch        int64  `json:"ended_at_epoch,omitempty"`
	PrincipalID         string `json:"principal_id,omitempty"`
	EventCount          int64  `json:"event_count"`
	ObservationCount    int64  `json:"observation_count"`
	LastObservedAtEpoch int64  `json:"last_observed_at_epoch,omitempty"`
}

type IngestResult struct {
	ID              int64
	ReceivedAtEpoch int64
	Duplicate       bool
}

type SearchHit struct {
	ID              int64   `json:"id"`
	SessionID       string  `json:"session_id,omitempty"`
	Project         string  `json:"project"`
	Kind            string  `json:"kind"`
	ReviewState     string  `json:"review_state"`
	SourceAdapter   string  `json:"source_adapter,omitempty"`
	EventType       string  `json:"event_type,omitempty"`
	Text    string `json:"text,omitempty"`
	Snippet string `json:"snippet"`
	// CapturedAtEpoch carries the observation's occurred_at_epoch under a
	// misleading legacy name. It stays for back-compat — the review UI, the
	// inject scripts and the parity tests read `captured_at_epoch` — and
	// always holds the same value as OccurredAtEpoch, which is the honest
	// name new consumers should read.
	CapturedAtEpoch int64 `json:"captured_at_epoch"`
	// OccurredAtEpoch is the honest name for the same column.
	OccurredAtEpoch int64 `json:"occurred_at_epoch"`
	// Provenance is the governance column (observed|inferred|user_confirmed|
	// imported|generated, schema/0007), surfaced so a consumer can weigh a
	// hit without a per-id fetch. Empty when the row predates the column.
	Provenance     string  `json:"provenance,omitempty"`
	Rank           float64 `json:"rank,omitempty"`
	NoiseCandidate int     `json:"noise_candidate"`
	CanInstruct    int     `json:"can_instruct"`
}

// syncOccurredAt fills the legacy CapturedAtEpoch alias from the value the
// queries scan into OccurredAtEpoch: one column, two JSON names. Every hit
// scan calls it, so the two fields can never disagree.
func (h *SearchHit) syncOccurredAt() { h.CapturedAtEpoch = h.OccurredAtEpoch }

type ObservationFilter struct {
	Project         string `json:"project,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	Kind            string `json:"kind,omitempty"`
	ExcludeKind     string `json:"exclude_kind,omitempty"`
	ReviewState     string `json:"review_state,omitempty"`
	DerivationRunID string `json:"derivation_run_id,omitempty"`
	Limit           int    `json:"limit,omitempty"`
	IncludeNoise    bool   `json:"include_noise,omitempty"`
	IncludeProposed bool   `json:"include_proposed,omitempty"`
	// CanInstruct, when true, restricts results to elevated observations
	// (can_instruct=1) — the operating-notes injection read. Additive opt-in
	// like IncludeProposed: false adds no clause at all, it does NOT mean
	// can_instruct=0.
	CanInstruct bool `json:"can_instruct,omitempty"`
	// Order controls ListObservations sort direction by occurred_at_epoch
	// (then id as tiebreak). "" and "desc" mean descending (default,
	// newest first). "asc" means ascending (oldest first). Any other
	// value falls back to the default, mirroring normalizeLimit's
	// permissive fallback style.
	Order string `json:"order,omitempty"`
	// Cursor is an opaque continuation token returned as next_cursor by a
	// previous page; format is internal. Only ListObservations consumes it.
	Cursor string `json:"cursor,omitempty"`
	// Hybrid is the per-request override for hybrid fusion (D218): nil
	// follows the deployment default (MEMOIRE_HYBRID_SEARCH), true/false
	// force the lane on/off. Only SearchWithFilter consumes it; forcing
	// true on a store without a wired vector lane stays FTS-only (the
	// lane preconditions always win over the wish).
	Hybrid *bool `json:"hybrid,omitempty"`
}

type ObservationReviewLogFilter struct {
	ObservationID int64 `json:"observation_id,omitempty"`
	Limit         int   `json:"limit,omitempty"`
}

type Stats struct {
	EventCount              int64 `json:"event_count"`
	ObservationCount        int64 `json:"observation_count"`
	DeletedEventCount       int64 `json:"deleted_event_count"`
	DeletedObservationCount int64 `json:"deleted_observation_count"`
	LastWriteEpoch          int64 `json:"last_write_epoch"`
	DBBytes                 int64 `json:"db_bytes"`
	WALBytes                int64 `json:"wal_bytes"`
}

type Credential struct {
	ID              string   `json:"id"`
	Prefix          string   `json:"prefix"`
	Label           string   `json:"label,omitempty"`
	Scopes          []string `json:"scopes"`
	RevokedAtEpoch  int64    `json:"revoked_at_epoch,omitempty"`
	CreatedAtEpoch  int64    `json:"created_at_epoch"`
	LastUsedAtEpoch int64    `json:"last_used_at_epoch,omitempty"`
}

// Open opens the store. path is either a SQLite file path (the historical
// contract of MEMOIRE_DB) or a postgres://... DSN, which selects the
// Postgres engine (memoire:D157 dual-engine seam). schemaDir is the SQLite
// migration dir; the Postgres twin is resolved as its sibling "schema-pg"
// (the two evolve in lockstep per D157).
func Open(path, schemaDir string) (*Store, error) {
	if IsPostgresDSN(path) {
		return openPostgres(path, filepath.Join(filepath.Dir(schemaDir), "schema-pg"))
	}
	return openSQLite(path, schemaDir)
}

func openSQLite(path, schemaDir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	conn, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	store := &Store{db: dbc{conn: conn, eng: EngineSQLite}, engine: EngineSQLite, path: path}
	if err := store.migrate(schemaDir); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Read pool: WAL supports N readers concurrent with the single writer, so
	// read-only surfaces get their own connections and can never head-of-line
	// block a write (a ~4s FTS search used to stall verdict/ingest writes for
	// its full duration behind MaxOpenConns(1)). mode=ro + _query_only is
	// belt-and-suspenders: the pool cannot write even if a statement slips
	// through routing. The writer stays the only connection that can mutate.
	// Opened after migrate() so the file is guaranteed to exist.
	ro, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_busy_timeout=5000&_query_only=true")
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	ro.SetMaxOpenConns(4)
	if err := ro.Ping(); err != nil {
		_ = ro.Close()
		_ = conn.Close()
		return nil, err
	}
	store.rdb = dbc{conn: ro, eng: EngineSQLite}
	return store, nil
}

func openPostgres(dsn, schemaPGDir string) (*Store, error) {
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(8)
	if err := conn.Ping(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	handle := dbc{conn: conn, eng: EnginePostgres}
	store := &Store{db: handle, rdb: handle, engine: EnginePostgres}
	if err := store.migrate(schemaPGDir); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Capability probe only — Open never provisions and never fails for
	// pg_search reasons (round-1 finding 1: the optional quality upgrade
	// must not be a hard startup dependency, and app-path DDL at Open
	// blocked writes for the whole index build).
	store.hasPGSearch = detectPGSearch(store.db)
	store.hasPGVector = detectPGVector(store.db)
	return store, nil
}

// pgSearchIndexUsable is the single validator for the BM25 setup, shared
// by the Open-time capability probe and the provisioning postcondition
// (round-2 finding N1). It is anchored to THE observations table the
// session resolves ('observations'::regclass — not a name match across
// schemas), requires a valid+ready index under the bm25 access method,
// the exact indexed attributes, and the key_field=id reloption. Any
// error means "not usable".
func pgSearchIndexUsable(ctx context.Context, conn interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) bool {
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_extension WHERE extname = 'pg_search'`).Scan(&n); err != nil || n == 0 {
		return false
	}
	var ok bool
	err := conn.QueryRowContext(ctx, `SELECT
			i.indisvalid AND i.indisready
			AND am.amname = 'bm25'
			AND pg_get_indexdef(i.indexrelid) LIKE '%USING bm25 (id, text)%'
			AND EXISTS (
				SELECT 1 FROM unnest(ic.reloptions) o WHERE o = 'key_field=id'
			)
		FROM pg_index i
		JOIN pg_class ic ON ic.oid = i.indexrelid
		JOIN pg_am am ON am.oid = ic.relam
		WHERE i.indrelid = 'observations'::regclass
		  AND ic.relname = 'idx_observations_bm25'`).Scan(&ok)
	if err != nil {
		return false
	}
	return ok
}

// detectPGSearch is the Open-time capability probe: read-only, never
// provisions, never fails Open. Provisioning is a deployment step
// (ProvisionPGSearchIndex from pgshadow Apply); the API just observes the
// capability — surfaced on /v1/health as search_engine, so an accidental
// fallback is visible instead of silent (round-1 findings 1 and 2).
func detectPGSearch(conn dbc) bool {
	return pgSearchIndexUsable(context.Background(), conn)
}

// HasPGSearch reports whether search runs on pg_search BM25 (vs the
// tsvector fallback). Exposed for health reporting and conformance
// assertions.
func (s *Store) HasPGSearch() bool { return s.hasPGSearch }

// ProvisionPGSearchIndex creates the BM25 index when the pg_search
// extension is installed (clean no-op otherwise). This is the DEPLOYMENT
// step — called from pgshadow Apply and test provisioning, deliberately
// never from Open (round-1 finding 1: app-path DDL made the optional
// upgrade a blocking startup dependency).
func ProvisionPGSearchIndex(ctx context.Context, conn *sql.DB) error {
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_extension WHERE extname = 'pg_search'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if _, err := conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_observations_bm25
		ON observations USING bm25 (id, text) WITH (key_field='id')`); err != nil {
		return err
	}
	// Postcondition (round-2 finding N1): IF NOT EXISTS matches by NAME
	// only, so a pre-existing wrong relation under this name would make
	// provisioning "succeed" while search later errors. The same validator
	// the capability probe uses must pass here, or deployment fails loudly.
	if !pgSearchIndexUsable(ctx, conn) {
		return errors.New("bm25 index provisioning postcondition failed: idx_observations_bm25 exists but is not the expected pg_search index on observations(id, text) with key_field=id")
	}
	return nil
}

// OpenReadOnly opens an OBSERVER store: no migrations, no provisioning,
// no write pool — for comparison/measurement tools that must not mutate
// what they observe (round-1 finding 3, tightened round 2 N2/N3).
//
// SQLite: the input must be a SELF-CONTAINED frozen file (a .backup
// artifact). mode=ro alone still creates -wal/-shm sidecars beside the
// file (SQLite WAL §5 — caught live in the round-2 review), so the open
// uses immutable=1, which performs no locking and creates nothing; a hot
// -wal sidecar next to the input means it is NOT self-contained and the
// open refuses rather than reading a possibly-uncommitted state.
//
// Postgres: the pool forces default_transaction_read_only by APPENDING to
// any existing options value (never clobbering caller settings), and only
// the capability probe runs.
func OpenReadOnly(path string) (*Store, error) {
	if IsPostgresDSN(path) {
		u, err := neturl.Parse(path)
		if err != nil {
			return nil, err
		}
		q := u.Query()
		opts := q.Get("options")
		if opts != "" {
			opts += " "
		}
		opts += "-c default_transaction_read_only=on"
		q.Set("options", opts)
		u.RawQuery = q.Encode()
		conn, err := sql.Open("pgx", u.String())
		if err != nil {
			return nil, err
		}
		conn.SetMaxOpenConns(4)
		if err := conn.Ping(); err != nil {
			_ = conn.Close()
			return nil, err
		}
		handle := dbc{conn: conn, eng: EnginePostgres}
		store := &Store{db: handle, rdb: handle, engine: EnginePostgres}
		store.hasPGSearch = detectPGSearch(handle)
		store.hasPGVector = detectPGVector(handle)
		return store, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Resolve symlinks BEFORE the sidecar preflight and URI construction
	// (round-3 finding R3-1): SQLite's VFS canonicalizes the database name,
	// so a symlinked input with a target-side sidecar would otherwise pass
	// the check and be read at an incomplete state under immutable=1
	// (which disables journal recovery entirely).
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, err
	}
	// Both hot-sidecar classes are refused, and stat errors other than
	// not-exist fail CLOSED — an unreadable sidecar is indistinguishable
	// from a hot one.
	for _, suffix := range []string{"-wal", "-journal"} {
		info, err := os.Stat(abs + suffix)
		switch {
		case err == nil && info.Size() > 0:
			return nil, fmt.Errorf("%s has a hot %s sidecar: not a self-contained frozen backup (immutable open would read an incomplete state)", abs, suffix)
		case err != nil && !os.IsNotExist(err):
			return nil, fmt.Errorf("cannot verify %s sidecar of %s: %w", suffix, abs, err)
		}
	}
	fileURL := neturl.URL{Scheme: "file", Path: abs, RawQuery: "immutable=1"}
	ro, err := sql.Open("sqlite3", fileURL.String())
	if err != nil {
		return nil, err
	}
	ro.SetMaxOpenConns(4)
	if err := ro.Ping(); err != nil {
		_ = ro.Close()
		return nil, err
	}
	handle := dbc{conn: ro, eng: EngineSQLite}
	return &Store{db: handle, rdb: handle, engine: EngineSQLite, path: abs}, nil
}

// SearchEngine names the active search implementation for observability
// (health endpoint): fts5 on SQLite, bm25 or tsvector on Postgres.
func (s *Store) SearchEngine() string {
	if s.engine != EnginePostgres {
		return "fts5"
	}
	if s.hasPGSearch {
		return "bm25"
	}
	return "tsvector"
}

// Ping is the readiness round-trip (MISSING #8): one trivial query on the
// READ pool, so the probe answers "is the database reachable" and nothing
// more. Deliberately NOT the write handle: SQLite's sole write connection
// is legitimately held for the duration of bulk transactions (session-wide
// lapse, TTL sweeps), and a probe that queues behind it turns healthy
// write work into failed readiness on the single replica — shedding
// capture traffic from a pod that is merely busy (review 2026-08-03 F2).
// Not a stats scan either — the probe runs at kubelet frequency and must
// stay cheap by construction.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	if err := s.rdb.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		return fmt.Errorf("read handle: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	rerr := s.rdb.Close()
	werr := s.db.Close()
	if werr != nil {
		return werr
	}
	return rerr
}

func (s *Store) migrate(schemaDir string) error {
	files, err := filepath.Glob(filepath.Join(schemaDir, "*.sql"))
	if err != nil {
		return err
	}
	// An unresolvable schema dir (missing, or zero migration files) must be
	// a hard failure, never a silent no-op: a consumer opening the store from
	// the wrong cwd would otherwise run unmigrated against a live store for
	// hours (memoire:D166 — the 42P01 window). Same doctrine as the quadlet
	// db.env hard-fail.
	if len(files) == 0 {
		return fmt.Errorf("no migration files found in schema dir %q: refusing to open unmigrated (set MEMOIRE_SCHEMA_DIR or run from the repo root)", schemaDir)
	}
	sort.Strings(files)
	for _, file := range files {
		base := filepath.Base(file)
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if base != "0001_initial.sql" && !registersSchemaVersion(string(b)) {
			continue
		}
		version, err := migrationVersion(base)
		if err != nil {
			return err
		}
		if version > 1 {
			applied, err := s.migrationApplied(version)
			if err != nil {
				return err
			}
			if applied {
				continue
			}
		}
		if _, err := s.db.Exec(string(b)); err != nil {
			return fmt.Errorf("apply %s: %w", base, err)
		}
	}
	return nil
}

func (s *Store) migrationApplied(version int) (bool, error) {
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM schema_version WHERE version = ?`, version).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func migrationVersion(base string) (int, error) {
	prefix, _, ok := strings.Cut(base, "_")
	if !ok {
		return 0, fmt.Errorf("invalid migration name %q", base)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, fmt.Errorf("invalid migration version in %q: %w", base, err)
	}
	return version, nil
}

func registersSchemaVersion(sqlText string) bool {
	return strings.Contains(sqlText, "INSERT OR IGNORE INTO schema_version") ||
		strings.Contains(sqlText, "INSERT INTO schema_version")
}

func (s *Store) EnsureDerivationRun(ctx context.Context, in DerivationRunInput) (DerivationRunResult, error) {
	if err := in.Validate(); err != nil {
		return DerivationRunResult{}, err
	}
	now := time.Now().Unix()
	if in.CreatedAtEpoch > 0 {
		now = in.CreatedAtEpoch
	}
	if in.Status == "" {
		in.Status = "pending"
	}
	if in.Force {
		return s.forceDerivationRun(ctx, in, now)
	}
	if in.ID == "" {
		in.ID = derivationRunID(in)
	}
	affected, err := s.insertDerivationRun(ctx, in, now)
	if err != nil {
		return DerivationRunResult{}, err
	}
	if affected == 0 {
		// The run already exists. Check whether it is in a failed state that
		// should be retried in-place (the human operator's ruling: failed never dedups).
		// Atomic reset: only one caller can transition failed→running.
		var existingID, existingStatus string
		var createdAt int64
		var metaJSON string
		err := s.db.QueryRowContext(ctx, `SELECT id, status, created_at_epoch, meta_json
			FROM derivation_runs
			WHERE provider = ?
			  AND COALESCE(model, '') = COALESCE(?, '')
			  AND derivation_version = ?
			  AND project = ?
			  AND COALESCE(session_id, '') = COALESCE(?, '')
			  AND window_start_epoch = ?
			  AND window_end_epoch = ?
			  AND input_hash = ?`,
			in.Provider, in.Model, in.DerivationVersion, in.Project, in.SessionID, in.WindowStartEpoch, in.WindowEndEpoch, in.InputHash,
		).Scan(&existingID, &existingStatus, &createdAt, &metaJSON)
		if err != nil {
			return DerivationRunResult{}, err
		}

		// Try to atomically reset a failed run to running. Status flip,
		// retry_count bump, and the attempt-start stamp travel in ONE statement:
		// the stamp keeps this fresh retry from being stolen by the
		// stale-running recovery below (whose age check reads
		// last_attempt_start_epoch before falling back to created_at_epoch), so
		// it must never be observable-late relative to the status flip
		// (orthogonal review 2026-07-11, MAJOR). The meta_json guard makes the
		// reset a full compare-and-swap against the row we inspected.
		failedRetryCount, failedMeta := parseRetryMeta(metaJSON)
		failedRetryCount++
		failedMeta["retry_count"] = float64(failedRetryCount)
		failedMeta["last_attempt_start_epoch"] = float64(time.Now().Unix())
		failedNewMeta, _ := json.Marshal(failedMeta)
		// The retry's fresh fetch proved the SAME input set (identical
		// input_hash) at the caller's newer snapshot — advance the watermark
		// with the claim (memoire:D165 round-2 HIGH-1). COALESCE keeps an
		// existing watermark when the caller has none (rules path).
		// Audit-provenance refresh (BYOK review r2 N1): the idempotency
		// identity excludes data_boundary/endpoint_id/prompt_version, so the
		// retry may run under DIFFERENT values than the row records — the
		// claim must rewrite them to the attempt that actually executes, or
		// the queryable columns lie about where the window's events went.
		res2, err := s.db.ExecContext(ctx,
			`UPDATE derivation_runs
				SET status = 'running',
				    completed_at_epoch = NULL,
				    meta_json = ?,
				    data_boundary = ?,
				    endpoint_id = ?,
				    prompt_version = ?,
				    snapshot_max_event_id = COALESCE(?, snapshot_max_event_id)
			WHERE id = ? AND status = 'failed' AND meta_json = ?`,
			string(failedNewMeta), in.DataBoundary, nullString(in.EndpointID), nullString(in.PromptVersion), nullInt64(in.SnapshotMaxEventID), existingID, metaJSON,
		)
		if err != nil {
			return DerivationRunResult{}, err
		}
		affected2, _ := res2.RowsAffected()
		if affected2 > 0 {
			return DerivationRunResult{ID: existingID, CreatedAtEpoch: createdAt, Duplicate: false, RetryCount: failedRetryCount}, nil
		}

		// Stale-running recovery (turn-ingest Phase 1, slice 3): a run left at
		// `running` past the age ceiling was killed mid-call and would dedup
		// forever, permanently watermarking its window. Treat it as retryable
		// with failed-retry semantics: reuse the run id, bump retry_count. The
		// meta_json compare-and-swap makes the takeover single-winner without
		// changing status (running stays running): the winner rewrites meta, so
		// a concurrent caller's CAS misses and dedups normally.
		if existingStatus == "running" {
			ceiling := in.StaleRunningRetrySeconds
			if ceiling <= 0 {
				ceiling = DefaultStaleRunningRetrySeconds
			}
			nowEpoch := time.Now().Unix()
			retryCount, meta := parseRetryMeta(metaJSON)
			attemptStart := createdAt
			if v, ok := meta["last_attempt_start_epoch"].(float64); ok {
				attemptStart = int64(v)
			}
			if nowEpoch-attemptStart >= ceiling {
				retryCount++
				meta["retry_count"] = float64(retryCount)
				meta["last_attempt_start_epoch"] = float64(nowEpoch)
				newMeta, _ := json.Marshal(meta)
				// Same audit-provenance refresh as the failed→running claim
				// above (r2 N1): the takeover attempt runs under the CALLER's
				// boundary/endpoint/prompt, not the dead attempt's.
				res3, err := s.db.ExecContext(ctx,
					`UPDATE derivation_runs
						SET completed_at_epoch = NULL,
						    meta_json = ?,
						    data_boundary = ?,
						    endpoint_id = ?,
						    prompt_version = ?,
						    snapshot_max_event_id = COALESCE(?, snapshot_max_event_id)
					WHERE id = ? AND status = 'running' AND meta_json = ?`,
					string(newMeta), in.DataBoundary, nullString(in.EndpointID), nullString(in.PromptVersion), nullInt64(in.SnapshotMaxEventID), existingID, metaJSON,
				)
				if err != nil {
					return DerivationRunResult{}, err
				}
				if affected3, _ := res3.RowsAffected(); affected3 > 0 {
					return DerivationRunResult{ID: existingID, CreatedAtEpoch: createdAt, Duplicate: false, RetryCount: retryCount}, nil
				}
			}
		}

		// Not retryable (or another caller already claimed the retry): fall
		// through to the standard dedup path, carrying the existing status.
		return DerivationRunResult{ID: existingID, CreatedAtEpoch: createdAt, Duplicate: true, Status: existingStatus}, nil
	}
	return DerivationRunResult{ID: in.ID, CreatedAtEpoch: now}, nil
}

// insertDerivationRun performs the INSERT OR IGNORE for a fully-resolved run
// input and reports how many rows landed (0 = the idempotency key already
// exists).
func (s *Store) insertDerivationRun(ctx context.Context, in DerivationRunInput, now int64) (int64, error) {
	meta := in.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	}
	ignorePre, ignoreSuf := insertIgnore(s.engine)
	res, err := s.db.ExecContext(ctx, `INSERT `+ignorePre+`INTO derivation_runs(
		id, project, session_id, provider, model, endpoint_id, data_boundary, derivation_version, prompt_version,
		input_hash, window_start_epoch, window_end_epoch, status, created_at_epoch, created_at_ns, completed_at_epoch, meta_json, snapshot_max_event_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`+ignoreSuf,
		in.ID, in.Project, nullString(in.SessionID), in.Provider, nullString(in.Model), nullString(in.EndpointID), in.DataBoundary, in.DerivationVersion, nullString(in.PromptVersion),
		in.InputHash, in.WindowStartEpoch, in.WindowEndEpoch, in.Status, now, time.Now().UnixNano(), nullInt64(in.CompletedAtEpoch), string(meta), nullInt64(in.SnapshotMaxEventID))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// forceDerivationRun mints a NEW run for a window that would otherwise dedup
// (turn-ingest Phase 1, slice 4; operator flag -force-rerun). The idempotency
// index is UNIQUE over the window key including input_hash, so the forced row
// stores the canonical hash with a visible "#forced-<nonce>" suffix. The
// insert-time meta_json records forced provenance immediately, so even a
// forced run killed mid-call is identifiable; the orchestrator re-records the
// same fields in the final run metadata.
func (s *Store) forceDerivationRun(ctx context.Context, in DerivationRunInput, now int64) (DerivationRunResult, error) {
	canonical := in.InputHash
	// The superseded run is the most recent one for the same window and
	// canonical input — the original, or the latest prior forced rerun.
	var superseded string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM derivation_runs
		WHERE provider = ?
		  AND COALESCE(model, '') = COALESCE(?, '')
		  AND derivation_version = ?
		  AND project = ?
		  AND COALESCE(session_id, '') = COALESCE(?, '')
		  AND window_start_epoch = ?
		  AND window_end_epoch = ?
		  AND (input_hash = ? OR input_hash LIKE ? || '#forced-%')
		ORDER BY created_at_epoch DESC, COALESCE(created_at_ns, 0) DESC, id DESC
		LIMIT 1`,
		in.Provider, in.Model, in.DerivationVersion, in.Project, in.SessionID,
		in.WindowStartEpoch, in.WindowEndEpoch, canonical, canonical,
	).Scan(&superseded)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return DerivationRunResult{}, err
	}

	meta := map[string]any{
		"forced":               true,
		"canonical_input_hash": canonical,
	}
	if superseded != "" {
		meta["superseded_run_id"] = superseded
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return DerivationRunResult{}, err
	}

	// Nanosecond nonce; the retry loop covers the theoretical collision.
	for attempt := 0; attempt < 3; attempt++ {
		forced := in
		forced.InputHash = canonical + "#forced-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		forced.ID = derivationRunID(forced)
		forced.Metadata = metaJSON
		affected, err := s.insertDerivationRun(ctx, forced, now)
		if err != nil {
			return DerivationRunResult{}, err
		}
		if affected > 0 {
			return DerivationRunResult{ID: forced.ID, CreatedAtEpoch: now, Forced: true, SupersededRunID: superseded}, nil
		}
	}
	return DerivationRunResult{}, errors.New("could not mint a forced derivation run (nonce collision)")
}

// parseRetryMeta extracts retry_count from a run's meta_json, returning the
// decoded map for in-place amendment. Malformed or empty meta yields a fresh
// map and zero count.
func parseRetryMeta(metaJSON string) (int, map[string]any) {
	meta := map[string]any{}
	retryCount := 0
	if len(metaJSON) > 0 {
		if json.Unmarshal([]byte(metaJSON), &meta) == nil {
			if rc, ok := meta["retry_count"].(float64); ok {
				retryCount = int(rc)
			}
		}
	}
	return retryCount, meta
}

// UpdateDerivationRunStatus updates the status, completion timestamp, and
// metadata of an existing derivation run after the provider call returns. It is
// the additive post-call companion to EnsureDerivationRun: no schema rewrite, no
// run creation. A zero CompletedAtEpoch leaves completed_at_epoch NULL; empty
// Metadata leaves meta_json untouched.
func (s *Store) UpdateDerivationRunStatus(ctx context.Context, in DerivationRunStatusUpdate) error {
	if strings.TrimSpace(in.ID) == "" {
		return errors.New("derivation run id is required")
	}
	if !allowedRunStatus(in.Status) {
		return errors.New("status must be one of pending, running, completed, partial, failed")
	}
	if len(in.Metadata) > 0 && !json.Valid(in.Metadata) {
		return errors.New("metadata must be valid JSON")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE derivation_runs
		SET status = ?,
		    completed_at_epoch = ?,
		    meta_json = COALESCE(?, meta_json)
		WHERE id = ?`,
		in.Status, nullInt64(in.CompletedAtEpoch), nullJSON(in.Metadata), in.ID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("derivation run %q not found", in.ID)
	}
	return nil
}

func (s *Store) GetDerivationRun(ctx context.Context, id string) (DerivationRunRecord, error) {
	if strings.TrimSpace(id) == "" {
		return DerivationRunRecord{}, errors.New("derivation run id is required")
	}
	return s.getDerivationRun(ctx, `SELECT dr.id, dr.project, dr.session_id, dr.provider, dr.data_boundary,
		dr.model, dr.endpoint_id, dr.derivation_version, dr.prompt_version, dr.input_hash, dr.window_start_epoch, dr.window_end_epoch,
		dr.status, dr.created_at_epoch, dr.completed_at_epoch, dr.meta_json,
		(SELECT count(*) FROM observations o WHERE o.derivation_run_id = dr.id AND o.deleted_at_epoch IS NULL),
		(SELECT count(*) FROM observations o WHERE o.derivation_run_id = dr.id AND o.review_state = 'proposed' AND o.deleted_at_epoch IS NULL)
		FROM derivation_runs dr
		WHERE dr.id = ?`, id)
}

func (s *Store) ListDerivationRuns(ctx context.Context, filter DerivationRunFilter) ([]DerivationRunRecord, error) {
	filter.Limit = normalizeLimit(filter.Limit, 50, 200)
	where, args := derivationRunWhere(filter)
	args = append(args, filter.Limit)
	rows, err := s.rdb.QueryContext(ctx, `SELECT dr.id, dr.project, dr.session_id, dr.provider, dr.data_boundary,
		dr.model, dr.endpoint_id, dr.derivation_version, dr.prompt_version, dr.input_hash, dr.window_start_epoch, dr.window_end_epoch,
		dr.status, dr.created_at_epoch, dr.completed_at_epoch, dr.meta_json,
		(SELECT count(*) FROM observations o WHERE o.derivation_run_id = dr.id AND o.deleted_at_epoch IS NULL),
		(SELECT count(*) FROM observations o WHERE o.derivation_run_id = dr.id AND o.review_state = 'proposed' AND o.deleted_at_epoch IS NULL)
		FROM derivation_runs dr
		WHERE `+where+`
		ORDER BY dr.created_at_epoch DESC, dr.id DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DerivationRunRecord
	for rows.Next() {
		rec, err := scanDerivationRunRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) InsertDerivedObservation(ctx context.Context, in DerivedObservationInput) (DerivedObservationResult, error) {
	res, _, err := s.insertDerivedObservationGated(ctx, in, nil)
	return res, err
}

// InsertDerivedObservationUnlessMatchedLive is the near-dup gate's atomic
// decision (2026-08-02, D211 step 3 review round-1 finding 1): the liveness
// of the matched observation rows is checked inside the SAME transaction as
// the would-be insert, so a matched row soft-deleted while the provider was
// generating can never justify a rejection — a serial check-then-insert pair
// would. matchedIDs arrive in the caller's preference order (score
// descending); the first still-live id is returned as liveMatchID with NO
// insert performed. When none are live, the insert proceeds and liveMatchID
// is 0.
func (s *Store) InsertDerivedObservationUnlessMatchedLive(ctx context.Context, in DerivedObservationInput, matchedIDs []int64) (DerivedObservationResult, int64, error) {
	return s.insertDerivedObservationGated(ctx, in, matchedIDs)
}

func (s *Store) insertDerivedObservationGated(ctx context.Context, in DerivedObservationInput, matchedIDs []int64) (DerivedObservationResult, int64, error) {
	if err := in.Validate(); err != nil {
		return DerivedObservationResult{}, 0, err
	}
	now := time.Now().Unix()
	if in.CreatedAtEpoch > 0 {
		now = in.CreatedAtEpoch
	}
	if in.OccurredAtEpoch == 0 {
		occurred, err := s.maxOccurredAtForEvents(ctx, in.SourceEventIDs)
		if err != nil {
			return DerivedObservationResult{}, 0, err
		}
		in.OccurredAtEpoch = occurred
	}
	reviewState := in.ReviewState
	if reviewState == "" {
		reviewState = DefaultReviewState(in.ModelDerived)
	}
	// Default provenance for derived observations.
	if in.Provenance == "" {
		in.Provenance = "inferred"
	}
	if in.CanEvidence == 0 {
		in.CanEvidence = 1
	}
	meta := in.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	}
	principal := in.PrincipalID
	if principal == "" {
		principal = authctx.PrincipalLocal
	}

	// App-level invariant check (mirrors DB trigger).
	if err := appInvariantCheck(in.Provenance, in.CanInstruct); err != nil {
		return DerivedObservationResult{}, 0, err
	}
	// Store-level provenance validation (contract: Derived observation insert).
	if !allowedProvenance(in.Provenance) {
		return DerivedObservationResult{}, 0, fmt.Errorf("invalid provenance %q: must be one of observed, inferred, user_confirmed, imported, generated", in.Provenance)
	}
	if !allowedDerivationProvenance(in.Provenance) {
		return DerivedObservationResult{}, 0, fmt.Errorf("invalid derivation provenance %q: must be one of observed, inferred, generated", in.Provenance)
	}
	if err := validateUsePolicy(in.Provenance, in.CanInstruct); err != nil {
		return DerivedObservationResult{}, 0, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DerivedObservationResult{}, 0, err
	}
	defer tx.Rollback()

	if live, err := firstLiveObservation(ctx, tx, matchedIDs); err != nil {
		return DerivedObservationResult{}, 0, err
	} else if live > 0 {
		return DerivedObservationResult{}, live, nil
	}

	if err := ensureDerivationRunExists(ctx, tx, in.DerivationRunID); err != nil {
		return DerivedObservationResult{}, 0, err
	}

	downgraded := false
	if reviewState == "accepted" {
		overlap, err := acceptedOverlapExists(ctx, tx, in.Project, in.Kind, in.ContentHash, in.SourceEventIDs)
		if err != nil {
			return DerivedObservationResult{}, 0, err
		}
		if overlap {
			reviewState = "proposed"
			downgraded = true
		}
	}

	var confidence any
	if in.Confidence != nil {
		confidence = *in.Confidence
	}
	var observationID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO observations(
		event_id, project, session_id, kind, text, source_adapter, event_type,
		occurred_at_epoch, created_at_epoch, principal_id, confidence, meta_json,
		review_state, derivation_run_id, content_hash, provenance, can_instruct, can_evidence, needs_confirmation, noise_candidate
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		nil, in.Project, nullString(in.SessionID), in.Kind, in.Text, nullString(in.SourceAdapter), nullString(in.EventType),
		in.OccurredAtEpoch, now, principal, confidence, string(meta), reviewState, in.DerivationRunID, in.ContentHash,
		nullString(in.Provenance), in.CanInstruct, in.CanEvidence, in.NeedsConfirmation, boolInt(in.NoiseCandidate)).Scan(&observationID)
	if err != nil {
		return DerivedObservationResult{}, 0, err
	}
	for _, eventID := range in.SourceEventIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO observation_source_events(observation_id, event_id, created_at_epoch)
			VALUES (?, ?, ?)`, observationID, eventID, now); err != nil {
			return DerivedObservationResult{}, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return DerivedObservationResult{}, 0, err
	}
	return DerivedObservationResult{
		ID:                     observationID,
		ReviewState:            reviewState,
		DowngradedFromAccepted: downgraded,
	}, 0, nil
}

// firstLiveObservation returns the first id (in the given order) that is a
// live (non-soft-deleted) observation row, or 0 when none are.
func firstLiveObservation(ctx context.Context, tx dbtx, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM observations
		WHERE id IN (`+strings.Join(placeholders, ",")+`) AND deleted_at_epoch IS NULL`, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	live := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		live[id] = true
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if live[id] {
			return id, nil
		}
	}
	return 0, nil
}

func (s *Store) EnsureLocalTokenCredential(ctx context.Context, token string) (Credential, error) {
	return s.EnsureTokenCredential(ctx, token, "local token", []string{"read", "write", "admin"})
}

func (s *Store) EnsureTokenCredential(ctx context.Context, token, label string, scopes []string) (Credential, error) {
	if token == "" {
		return Credential{}, errors.New("token is required")
	}
	if len(scopes) == 0 {
		return Credential{}, errors.New("at least one scope is required")
	}
	now := time.Now().Unix()
	hash := TokenHash(token)
	cred := Credential{
		ID:             "local-" + hash[:16],
		Prefix:         tokenPrefix(token),
		Label:          label,
		Scopes:         scopes,
		CreatedAtEpoch: now,
	}
	scopesJSON, err := json.Marshal(cred.Scopes)
	if err != nil {
		return Credential{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO credentials(
		id, token_hash, prefix, label, scopes_json, created_at_epoch
	) VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(token_hash) DO UPDATE SET
		prefix = excluded.prefix,
		label = COALESCE(credentials.label, excluded.label),
		scopes_json = excluded.scopes_json`,
		cred.ID, hash, cred.Prefix, cred.Label, string(scopesJSON), now)
	if err != nil {
		return Credential{}, err
	}
	return cred, nil
}

func (s *Store) AuthenticateToken(ctx context.Context, token, source, requestID string) (authctx.Context, bool, error) {
	if token == "" {
		return authctx.Context{}, false, nil
	}
	hash := TokenHash(token)
	var id, scopesJSON string
	var revoked sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, scopes_json, revoked_at_epoch
		FROM credentials WHERE token_hash = ?`, hash).Scan(&id, &scopesJSON, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return authctx.Context{}, false, nil
	}
	if err != nil {
		return authctx.Context{}, false, err
	}
	if revoked.Valid {
		return authctx.Context{}, false, nil
	}
	var scopes []string
	if err := json.Unmarshal([]byte(scopesJSON), &scopes); err != nil {
		return authctx.Context{}, false, err
	}
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, `UPDATE credentials SET last_used_at_epoch = ? WHERE id = ?`, now, id); err != nil {
		return authctx.Context{}, false, err
	}
	return authctx.Context{
		PrincipalID:  authctx.PrincipalLocal,
		CredentialID: id,
		RequestID:    requestID,
		Source:       source,
		Scopes:       scopes,
	}, true, nil
}

func (s *Store) RevokeCredential(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE credentials SET revoked_at_epoch = ? WHERE id = ? AND revoked_at_epoch IS NULL`, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}
	return s.Audit(ctx, "credential.revoke", "credential", id, nil)
}

func (s *Store) CredentialByID(ctx context.Context, id string) (Credential, error) {
	var cred Credential
	var scopesJSON string
	var revoked, lastUsed sql.NullInt64
	err := s.rdb.QueryRowContext(ctx, `SELECT id, prefix, COALESCE(label, ''), scopes_json, revoked_at_epoch, created_at_epoch, last_used_at_epoch
		FROM credentials WHERE id = ?`, id).Scan(&cred.ID, &cred.Prefix, &cred.Label, &scopesJSON, &revoked, &cred.CreatedAtEpoch, &lastUsed)
	if err != nil {
		return Credential{}, err
	}
	if err := json.Unmarshal([]byte(scopesJSON), &cred.Scopes); err != nil {
		return Credential{}, err
	}
	if lastUsed.Valid {
		cred.LastUsedAtEpoch = lastUsed.Int64
	}
	if revoked.Valid {
		cred.RevokedAtEpoch = revoked.Int64
	}
	return cred, nil
}

func (s *Store) ListCredentials(ctx context.Context) ([]Credential, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, prefix, COALESCE(label, ''), scopes_json, revoked_at_epoch, created_at_epoch, last_used_at_epoch
		FROM credentials ORDER BY created_at_epoch DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var creds []Credential
	for rows.Next() {
		var cred Credential
		var scopesJSON string
		var revoked, lastUsed sql.NullInt64
		if err := rows.Scan(&cred.ID, &cred.Prefix, &cred.Label, &scopesJSON, &revoked, &cred.CreatedAtEpoch, &lastUsed); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(scopesJSON), &cred.Scopes); err != nil {
			return nil, err
		}
		if revoked.Valid {
			cred.RevokedAtEpoch = revoked.Int64
		}
		if lastUsed.Valid {
			cred.LastUsedAtEpoch = lastUsed.Int64
		}
		creds = append(creds, cred)
	}
	return creds, rows.Err()
}

func (s *Store) Audit(ctx context.Context, action, resourceType, resourceID string, details any) error {
	now := time.Now().Unix()
	auth, _ := authctx.From(ctx)
	detailsJSON := "{}"
	if details != nil {
		b, err := json.Marshal(details)
		if err != nil {
			return err
		}
		detailsJSON = string(b)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_log(
		principal_id, credential_id, request_id, action, resource_type, resource_id, details_json, created_at_epoch
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		nullString(auth.PrincipalID), nullString(auth.CredentialID), nullString(auth.RequestID),
		action, nullString(resourceType), nullString(resourceID), detailsJSON, now)
	return err
}

func (s *Store) AuditCount(ctx context.Context, action string) (int64, error) {
	var count int64
	err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM audit_log WHERE action = ?`, action).Scan(&count)
	return count, err
}

func (s *Store) Ingest(ctx context.Context, in EventInput) (IngestResult, error) {
	if in.SourceAdapter == "" && in.ProducerID != "" {
		in.SourceAdapter = in.ProducerID
	}
	if err := in.Validate(); err != nil {
		return IngestResult{}, err
	}
	now := time.Now().Unix()
	if in.ReceivedAtEpoch > 0 {
		now = in.ReceivedAtEpoch
	}
	occurred, err := in.OccurredEpoch()
	if err != nil {
		return IngestResult{}, err
	}
	payload := in.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	meta := in.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	}
	principal := in.PrincipalID
	if principal == "" {
		principal = authctx.PrincipalLocal
	}
	credentialID := ""
	if auth, ok := authctx.From(ctx); ok {
		principal = auth.PrincipalID
		credentialID = auth.CredentialID
		if in.RequestID == "" {
			in.RequestID = auth.RequestID
		}
	}
	text := in.Text
	if text == "" {
		text = projectText(in, payload)
	}
	key := in.IdempotencyKey(payload)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IngestResult{}, err
	}
	defer tx.Rollback()

	ignorePre, ignoreSuf := insertIgnore(s.engine)
	_, _ = tx.ExecContext(ctx, `INSERT `+ignorePre+`INTO sessions(session_id, project, client, started_at_epoch, principal_id, meta_json)
		VALUES (?, ?, ?, ?, ?, '{}')`+ignoreSuf, in.SessionID, in.Project, in.SourceAdapter, occurred, principal)

	// RETURNING id works on both engines (SQLite >= 3.35); with the ignore
	// clause a duplicate insert returns NO row, so ErrNoRows IS the dedup
	// signal — replacing the SQLite-only Exec + RowsAffected + LastInsertId
	// dance with one shared shape.
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT `+ignorePre+`INTO events(
		idempotency_key, session_id, content_session_id, project, source_adapter, source_event_id,
		event_type, cwd, payload_json, text, occurred_at_epoch, received_at_epoch,
		principal_id, credential_id, request_id, meta_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`+ignoreSuf+` RETURNING id`,
		key, in.SessionID, nullString(in.ContentSessionID), in.Project, in.SourceAdapter, nullString(in.SourceEventID),
		in.EventType, nullString(in.CWD), string(payload), nullString(text), occurred, now,
		principal, nullString(credentialID), nullString(in.RequestID), string(meta)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		var existingID, received int64
		err := tx.QueryRowContext(ctx, `SELECT id, received_at_epoch FROM events WHERE idempotency_key = ?`, key).Scan(&existingID, &received)
		if err != nil {
			return IngestResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return IngestResult{}, err
		}
		return IngestResult{ID: existingID, ReceivedAtEpoch: received, Duplicate: true}, nil
	}
	if err != nil {
		return IngestResult{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT `+ignorePre+`INTO observations(
		event_id, project, session_id, kind, text, source_adapter, event_type,
		occurred_at_epoch, created_at_epoch, principal_id, meta_json, provenance
	) VALUES (?, ?, ?, 'raw', ?, ?, ?, ?, ?, ?, '{}', 'observed')`+ignoreSuf,
		id, in.Project, in.SessionID, text, in.SourceAdapter, in.EventType, occurred, now, principal)
	if err != nil {
		return IngestResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(principal_id, credential_id, request_id, action, resource_type, resource_id, details_json, created_at_epoch)
		VALUES (?, ?, ?, 'event.ingest', 'event', ?, '{}', ?)`, principal, nullString(credentialID), nullString(in.RequestID), fmt.Sprint(id), now); err != nil {
		return IngestResult{}, err
	}
	if err := recordProjectRootTx(ctx, tx, in.Project, payload, now); err != nil {
		return IngestResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return IngestResult{}, err
	}
	return IngestResult{ID: id, ReceivedAtEpoch: now}, nil
}

// recordProjectRootTx maintains project_roots and project_identity_alerts
// from the identity fields adapters merge into event payloads (liaison:D239 item 4,
// mechanic ratified 2026-07-18: adapter-passed resolved roots). Rules:
//   - no root in the payload → nothing recorded (old adapters, http clients);
//   - a root already known for the key → refresh last_seen;
//   - the first root a key is ever seen under → learned silently, any source;
//   - a NEW root via source=registry → learned silently (the registry is the
//     operator-curated multi-home mechanism);
//   - a NEW root via any other source (basename collision, duplicated
//     .memoire key) → project_identity_alerts upsert. Loud, never blocking:
//     surfaced on timeline/search reads, cleared when the root becomes known.
//
// Identity tracking is advisory: a malformed payload never blocks ingest.
func recordProjectRootTx(ctx context.Context, tx dbtx, project string, payload json.RawMessage, now int64) error {
	var ident struct {
		Root   string `json:"memoire_project_root"`
		Source string `json:"memoire_project_source"`
	}
	if len(payload) == 0 || json.Unmarshal(payload, &ident) != nil {
		return nil
	}
	root := strings.TrimSpace(ident.Root)
	if project == "" || root == "" {
		return nil
	}
	source := strings.TrimSpace(ident.Source)
	if source == "" {
		source = "unknown"
	}
	res, err := tx.ExecContext(ctx, `UPDATE project_roots SET last_seen_epoch = ? WHERE project = ? AND root = ?`, now, project, root)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	// Same-second first_seen ties break on root bytewise: SQLite's BINARY
	// collation does that natively; Postgres needs COLLATE "C" or a non-C
	// database locale could pick a different known_root than SQLite/the
	// replay port (round-1 finding 2; pgshadow.recordProjectRootPG already
	// orders this way).
	rootOrder := `root ASC`
	if tx.eng == EnginePostgres {
		rootOrder = `root COLLATE "C" ASC`
	}
	var knownRoot string
	err = tx.QueryRowContext(ctx, `SELECT root FROM project_roots WHERE project = ?
		ORDER BY first_seen_epoch ASC, `+rootOrder+` LIMIT 1`, project).Scan(&knownRoot)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && source == "registry") {
		ignorePre, ignoreSuf := insertIgnore(tx.eng)
		if _, err := tx.ExecContext(ctx, `INSERT `+ignorePre+`INTO project_roots(project, root, source, first_seen_epoch, last_seen_epoch)
			VALUES (?, ?, ?, ?, ?)`+ignoreSuf, project, root, source, now, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM project_identity_alerts WHERE project = ? AND root = ?`, project, root)
		return err
	}
	if err != nil {
		return err
	}
	// The DO UPDATE increment must qualify event_count with the table name:
	// unqualified, Postgres rejects it as ambiguous against excluded (42702)
	// while SQLite silently resolves it to the target row.
	_, err = tx.ExecContext(ctx, `INSERT INTO project_identity_alerts(
		project, root, known_root, source, first_seen_epoch, last_seen_epoch, event_count
	) VALUES (?, ?, ?, ?, ?, ?, 1)
	ON CONFLICT(project, root) DO UPDATE SET
		last_seen_epoch = excluded.last_seen_epoch,
		event_count = project_identity_alerts.event_count + 1`, project, root, knownRoot, source, now, now)
	return err
}

// ProjectIdentityAlert is an active identity conflict for a project key,
// rendered on project-filtered timeline/search reads (injection header and
// MCP surfaces both ride those).
type ProjectIdentityAlert struct {
	Project    string `json:"project"`
	Root       string `json:"root"`
	KnownRoot  string `json:"known_root"`
	Source     string `json:"source"`
	EventCount int64  `json:"event_count"`
	LastSeen   int64  `json:"last_seen_epoch"`
}

// Text renders the operator-facing alert line (liaison:D239 item 5 wording).
func (a ProjectIdentityAlert) Text() string {
	return fmt.Sprintf(
		"memoire identity conflict: key %q is arriving from %s (source %s, %d events) but is already used by %s — capture continues unblocked; create .memoire / set MEMOIRE_PROJECT there, then run memoire-project-move to fold the rows",
		a.Project, a.Root, a.Source, a.EventCount, a.KnownRoot)
}

// ListProjectIdentityAlerts returns the active alerts for one project key.
func (s *Store) ListProjectIdentityAlerts(ctx context.Context, project string) ([]ProjectIdentityAlert, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT project, root, known_root, source, event_count, last_seen_epoch
		FROM project_identity_alerts WHERE project = ? ORDER BY last_seen_epoch DESC`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectIdentityAlert
	for rows.Next() {
		var a ProjectIdentityAlert
		if err := rows.Scan(&a.Project, &a.Root, &a.KnownRoot, &a.Source, &a.EventCount, &a.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetEvent(ctx context.Context, id int64) (EventRecord, error) {
	var rec EventRecord
	var payload, sourceEventID, cwd, text, principal sql.NullString
	err := s.rdb.QueryRowContext(ctx, `SELECT id, idempotency_key, session_id, project, source_adapter, source_event_id,
		event_type, cwd, payload_json, text, occurred_at_epoch, received_at_epoch, principal_id
		FROM events WHERE id = ? AND deleted_at_epoch IS NULL`, id).Scan(
		&rec.ID, &rec.IdempotencyKey, &rec.SessionID, &rec.Project, &rec.SourceAdapter, &sourceEventID,
		&rec.EventType, &cwd, &payload, &text, &rec.OccurredAtEpoch, &rec.ReceivedAtEpoch, &principal)
	if err != nil {
		return EventRecord{}, err
	}
	rec.SourceEventID = sourceEventID.String
	rec.CWD = cwd.String
	rec.Payload = json.RawMessage(payload.String)
	rec.Text = text.String
	rec.PrincipalID = principal.String
	return rec, nil
}

// coveredEventPredicate is the per-event coverage rule shared by
// ListEventsForDerivation, ListEventsForDerivationCoverage and
// OutstandingDeriveWork: an event is covered exactly when a COMPLETED run of
// its own project/session lists it in its input manifest.
const coveredEventPredicate = `EXISTS (
			SELECT 1 FROM derivation_runs r
			JOIN run_input_events m ON m.run_id = r.id AND m.event_id = events.id
			WHERE r.project = events.project
			  AND r.session_id = events.session_id
			  AND r.status = 'completed'
		)`

// derivationEventClauses builds the shared WHERE clauses for the derivation
// fetch family. filter.ExcludeCovered is honored here; callers wanting the
// classification shape use ListEventsForDerivationCoverage instead.
func derivationEventClauses(filter DerivationEventFilter) (clauses []string, args []any, err error) {
	if filter.Project == "" {
		return nil, nil, errors.New("project is required")
	}
	args = []any{filter.Project}
	clauses = append(clauses, "project = ?")
	clauses = append(clauses, "deleted_at_epoch IS NULL")
	if filter.SessionID != "" {
		clauses = append(clauses, "session_id = ?")
		args = append(args, filter.SessionID)
	}
	if filter.WindowStartEpoch > 0 {
		clauses = append(clauses, "occurred_at_epoch >= ?")
		args = append(args, filter.WindowStartEpoch)
	}
	if filter.WindowEndEpoch > 0 {
		clauses = append(clauses, "occurred_at_epoch <= ?")
		args = append(args, filter.WindowEndEpoch)
	}
	if filter.MaxEventID > 0 {
		clauses = append(clauses, "id <= ?")
		args = append(args, filter.MaxEventID)
	}
	if filter.ExcludeCovered {
		clauses = append(clauses, "NOT "+coveredEventPredicate)
	}
	if len(filter.EventTypes) > 0 {
		placeholders := make([]string, 0, len(filter.EventTypes))
		for _, eventType := range filter.EventTypes {
			if eventType == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, eventType)
		}
		if len(placeholders) > 0 {
			clauses = append(clauses, "event_type IN ("+strings.Join(placeholders, ",")+")")
		}
	}
	return clauses, args, nil
}

func (s *Store) ListEventsForDerivation(ctx context.Context, filter DerivationEventFilter) ([]EventRecord, error) {
	clauses, args, err := derivationEventClauses(filter)
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, session_id, project, source_adapter, source_event_id,
		idempotency_key, event_type, cwd, payload_json, text, occurred_at_epoch, received_at_epoch, principal_id
		FROM events
		WHERE `+strings.Join(clauses, " AND ")+`
		ORDER BY occurred_at_epoch ASC, id ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EventRecord
	for rows.Next() {
		var rec EventRecord
		var payload, sourceEventID, cwd, text, principal sql.NullString
		if err := rows.Scan(&rec.ID, &rec.SessionID, &rec.Project, &rec.SourceAdapter, &sourceEventID,
			&rec.IdempotencyKey, &rec.EventType, &cwd, &payload, &text, &rec.OccurredAtEpoch, &rec.ReceivedAtEpoch, &principal); err != nil {
			return nil, err
		}
		rec.SourceEventID = sourceEventID.String
		rec.CWD = cwd.String
		rec.Payload = json.RawMessage(payload.String)
		rec.Text = text.String
		rec.PrincipalID = principal.String
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ListEventsForDerivationCoverage is the single-snapshot classifier behind the
// fully_covered skip (round-2 R2-F2): ONE statement returns every live
// matching event with its per-row coverage flag, so "how many events exist"
// and "which are uncovered" are facts of the same read — serial probe +
// re-fetch pairs can race soft-deletes and late commits into a false skip.
// filter.ExcludeCovered is ignored (the classification subsumes it); total is
// the full live count, uncovered the events a derivation should feed.
func (s *Store) ListEventsForDerivationCoverage(ctx context.Context, filter DerivationEventFilter) (total int, uncovered []EventRecord, err error) {
	filter.ExcludeCovered = false
	clauses, args, err := derivationEventClauses(filter)
	if err != nil {
		return 0, nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, session_id, project, source_adapter, source_event_id,
		idempotency_key, event_type, cwd, payload_json, text, occurred_at_epoch, received_at_epoch, principal_id,
		`+coveredEventPredicate+` AS covered
		FROM events
		WHERE `+strings.Join(clauses, " AND ")+`
		ORDER BY occurred_at_epoch ASC, id ASC`, args...)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rec EventRecord
		var payload, sourceEventID, cwd, text, principal sql.NullString
		var covered bool
		if err := rows.Scan(&rec.ID, &rec.SessionID, &rec.Project, &rec.SourceAdapter, &sourceEventID,
			&rec.IdempotencyKey, &rec.EventType, &cwd, &payload, &text, &rec.OccurredAtEpoch, &rec.ReceivedAtEpoch, &principal, &covered); err != nil {
			return 0, nil, err
		}
		total++
		if covered {
			continue
		}
		rec.SourceEventID = sourceEventID.String
		rec.CWD = cwd.String
		rec.Payload = json.RawMessage(payload.String)
		rec.Text = text.String
		rec.PrincipalID = principal.String
		uncovered = append(uncovered, rec)
	}
	return total, uncovered, rows.Err()
}

// SessionObservationSummary is the compact per-observation shape the
// derivation lane reads back for a session: the already-captured prompt
// context and the near-dup gate both consume it. Deliberately narrow — no
// payloads, no metadata — so a long session's read stays cheap.
type SessionObservationSummary struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	Text        string `json:"text"`
	ReviewState string `json:"review_state"`
}

// ListSessionObservationsForDerivation returns the session's existing
// observations, newest first, capped at limit (0 means no cap). ALL
// non-deleted review states are included — lapsed and rejected content is
// deliberately visible so re-derivation neither resurrects reviewed-away
// text nor re-proposes it (operator-ruled 2026-08-02). Soft-deleted rows
// stay invisible like everywhere else. Raw rows are excluded: they mirror
// event text verbatim (Ingest writes one per event), and a derivation's
// whole job is restating event content — comparing candidates against raw
// mirrors would reject every good derivation of a captured prompt.
func (s *Store) ListSessionObservationsForDerivation(ctx context.Context, project, sessionID string, limit int) ([]SessionObservationSummary, error) {
	if project == "" || sessionID == "" {
		return nil, errors.New("project and session_id are required")
	}
	q := `SELECT id, kind, text, review_state FROM observations
		WHERE project = ? AND session_id = ? AND deleted_at_epoch IS NULL
		  AND kind <> 'raw'
		ORDER BY id DESC`
	args := []any{project, sessionID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionObservationSummary
	for rows.Next() {
		var rec SessionObservationSummary
		if err := rows.Scan(&rec.ID, &rec.Kind, &rec.Text, &rec.ReviewState); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) GetObservation(ctx context.Context, id int64) (ObservationRecord, error) {
	return s.GetObservationWithFilter(ctx, id, ObservationFilter{})
}

// GetObservationWithFilter is the opt-in by-id read: callers that got an id
// from a surface configured with include_proposed/include_noise pass the same
// flags here so those ids never 404. The zero filter keeps the ratified
// default (accepted-only, noise excluded); rejected, superseded, and
// soft-deleted rows stay hidden on every path.
func (s *Store) GetObservationWithFilter(ctx context.Context, id int64, filter ObservationFilter) (ObservationRecord, error) {
	where, args := observationWhere(filter, "o")
	args = append([]any{id}, args...)
	rec, err := s.getObservation(ctx, `SELECT o.id, o.event_id, o.project, o.session_id, o.kind, o.text,
		o.source_adapter, o.event_type, o.occurred_at_epoch, o.created_at_epoch, o.deleted_at_epoch,
		o.principal_id, o.confidence, o.review_state, o.derivation_run_id, o.content_hash,
		o.provenance, o.can_instruct, o.can_evidence, o.needs_confirmation, o.noise_candidate
		FROM observations o
		WHERE o.id = ? AND `+where, args...)
	if err != nil {
		return ObservationRecord{}, err
	}
	ids, err := s.sourceEventIDs(ctx, rec.ID)
	if err != nil {
		return ObservationRecord{}, err
	}
	rec.SourceEventIDs = ids
	return rec, nil
}

// observationListOrderDesc and observationListOrderAsc are the only two
// ORDER BY clauses ListObservations may use. The caller-supplied Order value
// is never interpolated into SQL directly; it is mapped to one of these
// constants.
const (
	observationListOrderDesc = "ORDER BY o.occurred_at_epoch DESC, o.id DESC"
	observationListOrderAsc  = "ORDER BY o.occurred_at_epoch ASC, o.id ASC"
)

// ErrBadCursor is returned by ListObservations when filter.Cursor is
// non-empty but malformed. Handlers map this to HTTP 400.
var ErrBadCursor = errors.New("malformed cursor")

// encodeObservationCursor formats an opaque keyset continuation token from
// the (occurred_at_epoch, id) of the last row on a page. The format is
// internal to the db package: "<epoch>:<id>", both base-10 ints.
func encodeObservationCursor(epoch, id int64) string {
	return strconv.FormatInt(epoch, 10) + ":" + strconv.FormatInt(id, 10)
}

// parseObservationCursor parses a cursor produced by encodeObservationCursor.
// It returns ErrBadCursor (wrapped, so errors.Is works) for any malformed
// input: wrong segment count or non-integer parts.
func parseObservationCursor(cursor string) (epoch, id int64, err error) {
	parts := strings.Split(cursor, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%w: expected 2 segments, got %d", ErrBadCursor, len(parts))
	}
	epoch, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: invalid epoch segment %q", ErrBadCursor, parts[0])
	}
	id, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: invalid id segment %q", ErrBadCursor, parts[1])
	}
	return epoch, id, nil
}

// ListObservations returns a page of observations matching filter, plus an
// opaque next_cursor continuation token. The token is empty when the page
// reached the end of the result set. filter.Cursor, if non-empty, continues
// a previous page in the same direction (filter.Order); a malformed cursor
// returns ErrBadCursor.
func (s *Store) ListObservations(ctx context.Context, filter ObservationFilter) ([]ObservationRecord, string, error) {
	filter.Limit = normalizeLimit(filter.Limit, 50, 200)
	desc := filter.Order != "asc"
	orderBy := observationListOrderDesc
	if !desc {
		orderBy = observationListOrderAsc
	}
	where, args := observationWhere(filter, "o")
	if cursor := strings.TrimSpace(filter.Cursor); cursor != "" {
		epoch, id, err := parseObservationCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		if desc {
			where += ` AND (o.occurred_at_epoch < ? OR (o.occurred_at_epoch = ? AND o.id < ?))`
		} else {
			where += ` AND (o.occurred_at_epoch > ? OR (o.occurred_at_epoch = ? AND o.id > ?))`
		}
		args = append(args, epoch, epoch, id)
	}
	fetchLimit := filter.Limit + 1
	args = append(args, fetchLimit)
	rows, err := s.rdb.QueryContext(ctx, `SELECT o.id, o.event_id, o.project, o.session_id, o.kind, o.text,
		o.source_adapter, o.event_type, o.occurred_at_epoch, o.created_at_epoch, o.deleted_at_epoch,
		o.principal_id, o.confidence, o.review_state, o.derivation_run_id, o.content_hash,
		o.provenance, o.can_instruct, o.can_evidence, o.needs_confirmation, o.noise_candidate
		FROM observations o`+s.reviewQueueIndexHint(filter)+`
		WHERE `+where+`
		`+orderBy+`
		LIMIT ?`, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var out []ObservationRecord
	for rows.Next() {
		rec, err := scanObservationRecord(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	var nextCursor string
	if len(out) > filter.Limit {
		last := out[filter.Limit-1]
		nextCursor = encodeObservationCursor(last.OccurredAtEpoch, last.ID)
		out = out[:filter.Limit]
	}

	for i := range out {
		ids, err := s.sourceEventIDs(ctx, out[i].ID)
		if err != nil {
			return nil, "", err
		}
		out[i].SourceEventIDs = ids
	}
	return out, nextCursor, nil
}

// reviewQueueIndexHint returns an INDEXED BY clause forcing the schema 0008
// proposed-only partial indexes for the review-queue read, and "" for every
// other filter shape. The no-stats planner never picks these indexes on its
// own (it prefers the equality-column indexes and temp-sorts ~all rows,
// seconds at 250k+ observations), and making them attractive to the planner
// makes them steal plans from more selective non-queue reads — so the queue
// query names its index explicitly instead. Forced only on the exact shape
// the review UI sends (review_state=proposed, no other filters): the
// predicates observationWhere emits for that shape are precisely the
// indexes' partial predicates, so the hint can always be honored. If the
// shape ever drifts, SQLite fails the prepare loudly instead of regressing
// silently. Postgres has no INDEXED BY: its planner keeps real statistics,
// and the parity harness's EXPLAIN spot checks guard the plan instead
// (design-table ruling: pins are dropped at the seam).
func (s *Store) reviewQueueIndexHint(filter ObservationFilter) string {
	if s.engine == EnginePostgres {
		return ""
	}
	if filter.ReviewState != "proposed" || filter.Project != "" || filter.SessionID != "" ||
		filter.Kind != "" || filter.ExcludeKind != "" || filter.DerivationRunID != "" ||
		filter.CanInstruct {
		return ""
	}
	if filter.IncludeNoise {
		return ` INDEXED BY idx_obs_review_queue_all`
	}
	return ` INDEXED BY idx_obs_review_queue`
}

// BroadReviewQueueIndexResidue reports 0008 review-queue indexes whose live
// shape is the hand-applied round-1 broad form instead of the proposed-only
// partial form shipped in schema/0008 (memoire:D142). CREATE INDEX IF NOT
// EXISTS matches by NAME only and the version-8 row was pre-stamped wherever
// the broad shape was hand-applied, so a residual broad index never
// self-heals — it silently steals plans from more selective indexes
// (idx_obs_project_occurred, idx_observations_derivation_run). INDEXED BY
// already fails loudly when the index is ABSENT; this covers the inverse.
// The caller logs a warning telling the operator to hand-swap (drop+create
// in one transaction) to the schema/0008 shape.
func (s *Store) BroadReviewQueueIndexResidue() ([]string, error) {
	if s.engine == EnginePostgres {
		// The broad-shape residue is a SQLite deployment-history artifact
		// (hand-applied round-1 indexes predating schema/0008). The Postgres
		// baseline was born with the proposed-only partial shape
		// (schema-pg/0001), so there is no residue class to detect.
		return nil, nil
	}
	rows, err := s.rdb.Query(`SELECT name, COALESCE(sql, '') FROM sqlite_master
		WHERE type = 'index' AND name IN ('idx_obs_review_queue', 'idx_obs_review_queue_all')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var residue []string
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			return nil, err
		}
		if !strings.Contains(ddl, "review_state = 'proposed'") {
			residue = append(residue, name)
		}
	}
	return residue, rows.Err()
}

// sourceEventIDs returns the provenance event ids backing an observation, in
// ascending order. Surfaced on review-queue listings so a human or agent can
// trace a proposed observation back to the raw events it was derived from.
func (s *Store) sourceEventIDs(ctx context.Context, observationID int64) ([]int64, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT event_id FROM observation_source_events
		WHERE observation_id = ? ORDER BY event_id ASC`, observationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) GetSession(ctx context.Context, id string) (SessionRecord, error) {
	var rec SessionRecord
	var ended sql.NullInt64
	var principal sql.NullString
	err := s.rdb.QueryRowContext(ctx, `SELECT session_id, project, client, started_at_epoch, ended_at_epoch, principal_id
		FROM sessions WHERE session_id = ?`, id).Scan(
		&rec.SessionID, &rec.Project, &rec.Client, &rec.StartedAtEpoch, &ended, &principal)
	if err != nil {
		return SessionRecord{}, err
	}
	if ended.Valid {
		rec.EndedAtEpoch = ended.Int64
	}
	rec.PrincipalID = principal.String
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE session_id = ? AND deleted_at_epoch IS NULL`, id).Scan(&rec.EventCount); err != nil {
		return SessionRecord{}, err
	}
	obsWhere, obsArgs := observationWhere(ObservationFilter{SessionID: id}, "o")
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*), COALESCE(max(o.occurred_at_epoch), 0)
		FROM observations o WHERE `+obsWhere, obsArgs...).Scan(&rec.ObservationCount, &rec.LastObservedAtEpoch); err != nil {
		return SessionRecord{}, err
	}
	return rec, nil
}

func (s *Store) TransitionObservationReview(ctx context.Context, id int64, toState, reasonText string) (ObservationRecord, error) {
	return s.transitionReview(ctx, id, toState, reasonText, false, "", nil)
}

// ReviewPrecondition is an optional compare-and-set guard on a review
// verdict (memoire:D207 residual 1, lean cut): any non-nil field must match
// the row's CURRENT value inside the mutation's transaction, or the store
// returns *StateConflictError and writes NOTHING — no transition, no
// review-log row, no audit row. With every client sending its snapshot as
// the precondition, duplicate deliveries of one gesture become structurally
// impossible: whichever of two identical calls commits first, the other
// conflicts deterministically instead of double-writing (the accepted->
// accepted confirm row that corrupted the D194 confirm/accept metric).
type ReviewPrecondition struct {
	ReviewState *string
	Provenance  *string
	CanInstruct *int
}

// StateConflictError: the row exists but its current state does not admit
// the requested mutation — a precondition mismatch, or an intrinsic guard
// (double elevation, elevate below accepted). Carries METADATA ONLY (never
// text/payload), so it is safe to return for rows the read surface hides
// (rejected/superseded/soft-deleted/disputed): a client whose verdict's
// response was lost can retry the same preconditioned call and read the
// outcome from this error instead of a blind truth-read. Wraps Err so
// errors.Is(err, ErrElevationConflict) keeps working for the elevation
// family.
type StateConflictError struct {
	Err         error
	Msg         string
	ID          int64  `json:"id"`
	ReviewState string `json:"review_state"`
	Provenance  string `json:"provenance"`
	CanInstruct int    `json:"can_instruct"`
}

func (e *StateConflictError) Error() string { return e.Msg }
func (e *StateConflictError) Unwrap() error { return e.Err }

func stateConflict(wrap error, current ObservationRecord, format string, args ...any) *StateConflictError {
	return &StateConflictError{
		Err:         wrap,
		Msg:         fmt.Sprintf(format, args...),
		ID:          current.ID,
		ReviewState: current.ReviewState,
		Provenance:  current.Provenance,
		CanInstruct: current.CanInstruct,
	}
}

func (p *ReviewPrecondition) check(current ObservationRecord) *StateConflictError {
	if p == nil {
		return nil
	}
	if p.ReviewState != nil && *p.ReviewState != current.ReviewState {
		return stateConflict(nil, current, "precondition failed: review_state is %q, expected %q", current.ReviewState, *p.ReviewState)
	}
	if p.Provenance != nil && *p.Provenance != current.Provenance {
		return stateConflict(nil, current, "precondition failed: provenance is %q, expected %q", current.Provenance, *p.Provenance)
	}
	if p.CanInstruct != nil && *p.CanInstruct != current.CanInstruct {
		return stateConflict(nil, current, "precondition failed: can_instruct is %d, expected %d", current.CanInstruct, *p.CanInstruct)
	}
	return nil
}

// reviewCASTestHook, when non-nil, is invoked between a review-family
// writer's pre-read and its guarded UPDATE (sites: "transition", "rewrite",
// "elevation", "review-action"). Test-only: the engine-pair battery
// installs a barrier here to force two writers past their pre-reads before
// either updates, deterministically exercising the CAS-loser and
// fresh-re-read branches (round-2 precondition review NEW MAJOR 3 — a
// goroutine race alone can serialize whole calls and pass vacuously).
// Always nil in production; set strictly before the racing goroutines
// start and cleared after they join.
var reviewCASTestHook func(site string, id int64)

func casHook(site string, id int64) {
	if h := reviewCASTestHook; h != nil {
		h(site, id)
	}
}

// TransitionObservationReviewPre is TransitionObservationReview with an
// optional precondition (nil = legacy unconditional behavior).
func (s *Store) TransitionObservationReviewPre(ctx context.Context, id int64, toState, reasonText string, pre *ReviewPrecondition) (ObservationRecord, error) {
	return s.transitionReview(ctx, id, toState, reasonText, false, "", pre)
}

// ConfirmObservation is the deliberate accept-and-promote verdict (slice E,
// memoire:D146, R-5): review_state -> accepted AND provenance ->
// user_confirmed in one statement. This is exactly what `accept` used to do
// implicitly; the R-5 split makes plain accept mean "seen, not contested,
// usable as memory" (provenance untouched) and reserves the provenance
// promotion for this explicit gesture. Valid from proposed, disputed,
// lapsed (the on-encounter revival path), and — memoire:D192 — from
// accepted, where it is a pure provenance promotion (state unchanged).
func (s *Store) ConfirmObservation(ctx context.Context, id int64, reasonText string) (ObservationRecord, error) {
	return s.transitionReview(ctx, id, "accepted", reasonText, true, "", nil)
}

// ConfirmObservationPre is ConfirmObservation with an optional precondition
// (nil = legacy unconditional behavior).
func (s *Store) ConfirmObservationPre(ctx context.Context, id int64, reasonText string, pre *ReviewPrecondition) (ObservationRecord, error) {
	return s.transitionReview(ctx, id, "accepted", reasonText, true, "", pre)
}

func (s *Store) transitionReview(ctx context.Context, id int64, toState, reasonText string, promoteProvenance bool, actorOverride string, pre *ReviewPrecondition) (ObservationRecord, error) {
	if !allowedReviewState(toState) {
		return ObservationRecord{}, errors.New("review_state must be one of proposed, accepted, rejected, superseded, soft_deleted, disputed, lapsed")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ObservationRecord{}, err
	}
	defer tx.Rollback()

	current, err := getObservationForReview(ctx, tx, id)
	if err != nil {
		return ObservationRecord{}, err
	}
	// Precondition first (memoire:D207 residual 1): a mismatch conflicts
	// BEFORE transition validation so the caller always gets the row's
	// current metadata, even when the transition would also be illegal.
	if conflict := pre.check(current); conflict != nil {
		return ObservationRecord{}, conflict
	}
	// Confirm-on-accepted (memoire:D192): a plain-accepted observation was
	// previously a dead end for provenance — accepted->accepted is not a
	// state transition, so the vouching act was unreachable after a fast
	// `a` in the queue. Confirm on an accepted row is a PURE provenance
	// promotion: state unchanged, provenance -> user_confirmed, ordinary
	// review-log row. Only the confirm verdict gets this; a plain accept
	// on an accepted row stays invalid. Raw stays locked (kind guard).
	if current.ReviewState == "accepted" && toState == "accepted" && promoteProvenance {
		if current.Kind == "raw" {
			return ObservationRecord{}, errors.New("raw observations are locked at accepted")
		}
		// An already-vouched row makes this confirm a PURE NO-OP: the CAS
		// below would "succeed" writing identical values, so a duplicate
		// delivery (lost-response retry) would log a second confirm row —
		// the D194 corruption (round-2 review NEW BLOCKER 1). A changing
		// state tuple is the only version token this schema has, so the
		// no-op must conflict instead; for a retrying client this 409 IS
		// the landed signal.
		if current.Provenance == "user_confirmed" {
			return ObservationRecord{}, stateConflict(nil, current,
				"observation %d is already confirmed (accepted, user_confirmed)", id)
		}
	} else if err := validateReviewTransition(current.Kind, current.ReviewState, toState); err != nil {
		return ObservationRecord{}, err
	}

	now := time.Now().Unix()
	deletedAt := current.DeletedAtEpoch
	if toState == "soft_deleted" && deletedAt == 0 {
		deletedAt = now
	}

	// Slice E (memoire:D146, R-5): plain accept no longer coerces provenance —
	// user_confirmed is promoted only by the explicit confirm verdict.
	var provenance *string
	if promoteProvenance {
		p := "user_confirmed"
		provenance = &p
	}

	casHook("transition", id)

	setClauses := []string{"review_state = ?", "deleted_at_epoch = ?"}
	setArgs := []any{toState, nullInt64(deletedAt)}
	if provenance != nil {
		setClauses = append(setClauses, "provenance = ?")
		setArgs = append(setArgs, *provenance)
	}
	setArgs = append(setArgs, id, current.ReviewState, current.Provenance, current.CanInstruct)

	// The UPDATE is the authority, not the pre-read (precondition review
	// round-1 BLOCKER 1): it is a full compare-and-set against the pre-read
	// snapshot — review_state alone cannot serialize confirm-on-accepted,
	// where the state does not change — and RowsAffected decides. Under
	// PostgreSQL READ COMMITTED a concurrent committed write makes the
	// predicate re-evaluate against the new row version and match zero
	// rows; the transaction then rolls back having written NOTHING, and
	// the conflict carries FRESH metadata (never the stale pre-read).
	res, err := tx.ExecContext(ctx, `UPDATE observations SET `+strings.Join(setClauses, ", ")+
		` WHERE id = ? AND review_state = ? AND provenance = ? AND can_instruct = ?`, setArgs...)
	if err != nil {
		return ObservationRecord{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return ObservationRecord{}, err
	} else if n == 0 {
		fresh, ferr := getObservationForReview(ctx, tx, id)
		if ferr != nil {
			return ObservationRecord{}, ferr
		}
		return ObservationRecord{}, stateConflict(nil, fresh,
			"state changed concurrently: review_state is %q", fresh.ReviewState)
	}
	actor := reviewActor(ctx)
	if actorOverride != "" {
		actor = actorOverride
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO observation_review_log(
		observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch
	) VALUES (?, ?, ?, ?, ?, ?)`,
		id, current.ReviewState, toState, actor, nullString(strings.TrimSpace(reasonText)), now); err != nil {
		return ObservationRecord{}, err
	}
	auth, _ := authctx.From(ctx)
	details := map[string]any{
		"from_state":  current.ReviewState,
		"to_state":    toState,
		"reason_text": strings.TrimSpace(reasonText),
	}
	if promoteProvenance {
		details["verdict"] = "confirm"
	}
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return ObservationRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(
		principal_id, credential_id, request_id, action, resource_type, resource_id, details_json, created_at_epoch
	) VALUES (?, ?, ?, 'observation.review', 'observation', ?, ?, ?)`,
		nullString(auth.PrincipalID), nullString(auth.CredentialID), nullString(auth.RequestID), fmt.Sprint(id), string(detailsJSON), now); err != nil {
		return ObservationRecord{}, err
	}
	updated, err := getObservationForReview(ctx, tx, id)
	if err != nil {
		return ObservationRecord{}, err
	}
	ids, err := sourceEventIDsTx(ctx, tx, id)
	if err != nil {
		return ObservationRecord{}, err
	}
	updated.SourceEventIDs = ids
	if err := tx.Commit(); err != nil {
		return ObservationRecord{}, err
	}
	return updated, nil
}

// RewriteObservation is the operator's "say it right" verdict (slice E,
// memoire:D146, R-5): NOT an edit. It creates a NEW observation carrying the
// operator's text with provenance=user_confirmed and review_state=accepted,
// links it to the original via supersedes_id, copies the original's
// source-event links (the raw provenance still backs the statement), and
// transitions the original to superseded. Valid from proposed, lapsed, and
// disputed. The new row gets event_id NULL (the (event_id, kind) unique
// index belongs to the original), confidence 1.0 (operator statement), and
// a content_hash of the new text so dedupe stays consistent. The original's
// review-log row records the supersede; the audit_log entry (action
// observation.rewrite) records the filiation both ways.
func (s *Store) RewriteObservation(ctx context.Context, id int64, newText, reasonText string) (ObservationRecord, error) {
	newText = strings.TrimSpace(newText)
	if newText == "" {
		return ObservationRecord{}, errors.New("rewrite text is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ObservationRecord{}, err
	}
	defer tx.Rollback()

	current, err := getObservationForReview(ctx, tx, id)
	if err != nil {
		return ObservationRecord{}, err
	}
	if current.Kind == "raw" {
		return ObservationRecord{}, errors.New("raw observations are locked at accepted")
	}
	if !inSlice(current.ReviewState, "proposed", "lapsed", "disputed") {
		// A state conflict, not a malformed request (precondition review
		// round-1 B3 follow-through): the unified shape carries current
		// metadata so a racing/retrying client can read the outcome.
		return ObservationRecord{}, stateConflict(nil, current,
			"rewrite is valid from proposed, lapsed, or disputed, not %s", current.ReviewState)
	}

	casHook("rewrite", id)

	hash, err := contenthash.Canonical(current.Project, current.Kind, newText)
	if err != nil {
		return ObservationRecord{}, err
	}
	now := time.Now().Unix()
	actor := reviewActor(ctx)

	metaJSON, err := json.Marshal(map[string]any{"rewrite_of": id})
	if err != nil {
		return ObservationRecord{}, err
	}
	var newID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO observations(
		event_id, project, session_id, kind, text, source_adapter, event_type,
		occurred_at_epoch, created_at_epoch, principal_id, confidence, meta_json,
		review_state, content_hash, provenance, supersedes_id,
		can_instruct, can_evidence, needs_confirmation, noise_candidate
	) VALUES (NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1.0, ?, 'accepted', ?, 'user_confirmed', ?, 0, ?, 0, 0) RETURNING id`,
		current.Project, nullString(current.SessionID), current.Kind, newText,
		nullString(current.SourceAdapter), nullString(current.EventType),
		current.OccurredAtEpoch, now, nullString(current.PrincipalID), string(metaJSON),
		hash, id, current.CanEvidence).Scan(&newID)
	if err != nil {
		return ObservationRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO observation_source_events(observation_id, event_id, created_at_epoch)
		SELECT ?, event_id, ? FROM observation_source_events WHERE observation_id = ?`, newID, now, id); err != nil {
		return ObservationRecord{}, err
	}

	// CAS discipline (precondition review round-1 BLOCKER 3): if a
	// concurrent writer moved the original between our read and this
	// UPDATE, zero rows match — the transaction rolls back WHOLE (the
	// accepted replacement inserted above included), instead of committing
	// a second replacement for an already-superseded original.
	supersedeRes, err := tx.ExecContext(ctx, `UPDATE observations SET review_state = 'superseded'
		WHERE id = ? AND review_state = ? AND provenance = ? AND can_instruct = ?`,
		id, current.ReviewState, current.Provenance, current.CanInstruct)
	if err != nil {
		return ObservationRecord{}, err
	}
	if n, err := supersedeRes.RowsAffected(); err != nil {
		return ObservationRecord{}, err
	} else if n == 0 {
		fresh, ferr := getObservationForReview(ctx, tx, id)
		if ferr != nil {
			return ObservationRecord{}, ferr
		}
		return ObservationRecord{}, stateConflict(nil, fresh,
			"state changed concurrently: review_state is %q", fresh.ReviewState)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO observation_review_log(
		observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch
	) VALUES (?, ?, 'superseded', ?, ?, ?)`,
		id, current.ReviewState, actor, nullString(strings.TrimSpace(reasonText)), now); err != nil {
		return ObservationRecord{}, err
	}
	auth, _ := authctx.From(ctx)
	detailsJSON, err := json.Marshal(map[string]any{
		"rewritten_as": newID,
		"from_state":   current.ReviewState,
		"reason_text":  strings.TrimSpace(reasonText),
	})
	if err != nil {
		return ObservationRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(
		principal_id, credential_id, request_id, action, resource_type, resource_id, details_json, created_at_epoch
	) VALUES (?, ?, ?, 'observation.rewrite', 'observation', ?, ?, ?)`,
		nullString(auth.PrincipalID), nullString(auth.CredentialID), nullString(auth.RequestID), fmt.Sprint(id), string(detailsJSON), now); err != nil {
		return ObservationRecord{}, err
	}

	created, err := getObservationForReview(ctx, tx, newID)
	if err != nil {
		return ObservationRecord{}, err
	}
	ids, err := sourceEventIDsTx(ctx, tx, newID)
	if err != nil {
		return ObservationRecord{}, err
	}
	created.SourceEventIDs = ids
	if err := tx.Commit(); err != nil {
		return ObservationRecord{}, err
	}
	return created, nil
}

// LapseSessionRemaining is the per-session bulk verdict (slice E,
// memoire:D146, R-6): every still-proposed observation of the session
// transitions to lapsed in one transaction, each with its own review-log
// row. Returns the number of rows lapsed.
func (s *Store) LapseSessionRemaining(ctx context.Context, sessionID, reasonText string) (int, error) {
	if strings.TrimSpace(sessionID) == "" {
		return 0, errors.New("session_id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT id FROM observations
		WHERE session_id = ? AND review_state = 'proposed' AND deleted_at_epoch IS NULL`, sessionID)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, tx.Commit()
	}

	now := time.Now().Unix()
	actor := reviewActor(ctx)
	reason := nullString(strings.TrimSpace(reasonText))
	// The guarded UPDATE decides per row (precondition review round-1
	// MAJOR 1): a row a concurrent verdict moved out of proposed is
	// SKIPPED — no fabricated lapse log, no inflated count.
	lapsed := 0
	for _, id := range ids {
		casHook("bulk-lapse", id)
		res, err := tx.ExecContext(ctx, `UPDATE observations SET review_state = 'lapsed'
			WHERE id = ? AND review_state = 'proposed'`, id)
		if err != nil {
			return 0, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return 0, err
		} else if n == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO observation_review_log(
			observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch
		) VALUES (?, 'proposed', 'lapsed', ?, ?, ?)`, id, actor, reason, now); err != nil {
			return 0, err
		}
		lapsed++
	}
	auth, _ := authctx.From(ctx)
	detailsJSON, err := json.Marshal(map[string]any{"session_id": sessionID, "lapsed_count": lapsed})
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(
		principal_id, credential_id, request_id, action, resource_type, resource_id, details_json, created_at_epoch
	) VALUES (?, ?, ?, 'observation.lapse_session', 'session', ?, ?, ?)`,
		nullString(auth.PrincipalID), nullString(auth.CredentialID), nullString(auth.RequestID), sessionID, string(detailsJSON), now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return lapsed, nil
}

// LapseTTLActor is the attributable system actor for TTL-driven lapses.
const LapseTTLActor = "system:lapse-ttl"

// DefaultLapseTTLDays is the review TTL (slice E, memoire:D146, R-6):
// proposed rows nobody has touched for this long lapse automatically. 21
// days — three weekly review passes' worth of grace; configurable per call.
const DefaultLapseTTLDays = 21

// LapseExpiredProposed lapses proposed observations older than the TTL that
// were NEVER touched (no observation_review_log row — "never opened").
// Actor is the attributable system principal, never a human. Returns the
// number of rows lapsed.
func (s *Store) LapseExpiredProposed(ctx context.Context, ttlDays int) (int, error) {
	if ttlDays <= 0 {
		ttlDays = DefaultLapseTTLDays
	}
	cutoff := time.Now().Unix() - int64(ttlDays)*86400
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT id FROM observations
		WHERE review_state = 'proposed' AND deleted_at_epoch IS NULL
		  AND created_at_epoch < ?
		  AND NOT EXISTS (SELECT 1 FROM observation_review_log l WHERE l.observation_id = observations.id)`, cutoff)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, tx.Commit()
	}

	now := time.Now().Unix()
	reason := nullString(fmt.Sprintf("review TTL (%dd) expired with no operator touch", ttlDays))
	// Per-row winner discipline (precondition review round-1 MAJOR 1),
	// same as LapseSessionRemaining.
	lapsed := 0
	for _, id := range ids {
		casHook("ttl-lapse", id)
		res, err := tx.ExecContext(ctx, `UPDATE observations SET review_state = 'lapsed'
			WHERE id = ? AND review_state = 'proposed'`, id)
		if err != nil {
			return 0, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return 0, err
		} else if n == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO observation_review_log(
			observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch
		) VALUES (?, 'proposed', 'lapsed', ?, ?, ?)`, id, LapseTTLActor, reason, now); err != nil {
			return 0, err
		}
		lapsed++
	}
	detailsJSON, err := json.Marshal(map[string]any{"ttl_days": ttlDays, "lapsed_count": lapsed})
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(
		action, resource_type, details_json, created_at_epoch
	) VALUES ('observation.lapse_ttl', 'observation', ?, ?)`, string(detailsJSON), now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return lapsed, nil
}

// SessionSummary is the catchup-mode session listing row (slice E, E3): a
// session plus its review workload, so the UI can order the deposit to skim.
type SessionSummary struct {
	SessionID           string `json:"session_id"`
	Project             string `json:"project"`
	Client              string `json:"client"`
	StartedAtEpoch      int64  `json:"started_at_epoch"`
	EndedAtEpoch        int64  `json:"ended_at_epoch,omitempty"`
	ObservationCount    int    `json:"observation_count"`
	ProposedCount       int    `json:"proposed_count"`
	LapsedCount         int    `json:"lapsed_count"`
	LastObservedAtEpoch int64  `json:"last_observed_at_epoch,omitempty"`
}

// ListSessions lists sessions newest-first for the session review surface.
// project filters exactly; proposedOnly keeps only sessions with review
// work outstanding. Counts exclude raw-kind rows (they are not review
// material) and soft-deleted rows.
func (s *Store) ListSessions(ctx context.Context, project string, proposedOnly bool, limit int) ([]SessionSummary, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	where := "1=1"
	args := []any{}
	if project != "" {
		where = "s.project = ?"
		args = append(args, project)
	}
	// Portability note (dialect seam): the historical shape used
	// GROUP BY s.session_id (redundant — session_id is the primary key, rows
	// are already unique) plus HAVING on a SELECT alias, which SQLite accepts
	// and Postgres rejects. The subquery wrapper filters on the alias in the
	// engine-shared subset instead; semantics are identical.
	proposedFilter := ""
	if proposedOnly {
		proposedFilter = "WHERE t.proposed_count > 0"
	}
	query := `SELECT * FROM (
		SELECT s.session_id, s.project, s.client, s.started_at_epoch, COALESCE(s.ended_at_epoch, 0) AS ended_at_epoch,
		COALESCE((SELECT count(*) FROM observations o WHERE o.session_id = s.session_id AND o.deleted_at_epoch IS NULL AND o.kind != 'raw'), 0) AS observation_count,
		COALESCE((SELECT count(*) FROM observations o WHERE o.session_id = s.session_id AND o.deleted_at_epoch IS NULL AND o.kind != 'raw' AND o.review_state = 'proposed'), 0) AS proposed_count,
		COALESCE((SELECT count(*) FROM observations o WHERE o.session_id = s.session_id AND o.deleted_at_epoch IS NULL AND o.kind != 'raw' AND o.review_state = 'lapsed'), 0) AS lapsed_count,
		COALESCE((SELECT max(o.occurred_at_epoch) FROM observations o WHERE o.session_id = s.session_id AND o.deleted_at_epoch IS NULL), 0) AS last_observed_at_epoch
		FROM sessions s
		WHERE ` + where + `
	) t ` + proposedFilter + `
		ORDER BY t.started_at_epoch DESC
		LIMIT ?`
	args = append(args, limit)
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionSummary
	for rows.Next() {
		var rec SessionSummary
		if err := rows.Scan(&rec.SessionID, &rec.Project, &rec.Client, &rec.StartedAtEpoch, &rec.EndedAtEpoch,
			&rec.ObservationCount, &rec.ProposedCount, &rec.LapsedCount, &rec.LastObservedAtEpoch); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ApplyObservationReviewAction applies a flag-only review action (evidence-only,
// restrict-scope, mark-stale) without changing review_state. It writes a review
// log entry recording the action.
func (s *Store) ApplyObservationReviewAction(ctx context.Context, id int64, action, reasonText string) (ObservationRecord, error) {
	if !inSlice(action, "evidence-only", "restrict-scope", "mark-stale") {
		return ObservationRecord{}, fmt.Errorf("review action must be one of evidence-only, restrict-scope, mark-stale, got %q", action)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ObservationRecord{}, err
	}
	defer tx.Rollback()

	current, err := getObservationForReview(ctx, tx, id)
	if err != nil {
		return ObservationRecord{}, err
	}
	if current.Kind == "raw" {
		return ObservationRecord{}, errors.New("raw observations are locked at accepted")
	}

	now := time.Now().Unix()
	actor := reviewActor(ctx)

	var updates []string
	switch action {
	case "evidence-only":
		updates = append(updates, "can_instruct = 0")
		updates = append(updates, "can_evidence = 1")
		updates = append(updates, "needs_confirmation = 0")
	case "restrict-scope":
		updates = append(updates, "can_instruct = 0")
		updates = append(updates, "can_evidence = 1")
		updates = append(updates, "needs_confirmation = 1")
	case "mark-stale":
		updates = append(updates, "can_instruct = 0")
		updates = append(updates, "can_evidence = 0")
		updates = append(updates, "needs_confirmation = 1")
	}

	casHook("review-action", id)

	// Full-snapshot CAS with RowsAffected as the authority (round-2 review
	// NEW MAJOR 2): the old review_state-only predicate ignored the result,
	// so a transition committed between read and write still produced a
	// "successful" action log and returned the stale pre-read.
	if len(updates) > 0 {
		res, err := tx.ExecContext(ctx,
			`UPDATE observations SET `+strings.Join(updates, ", ")+
				` WHERE id = ? AND review_state = ? AND provenance = ? AND can_instruct = ?`,
			id, current.ReviewState, current.Provenance, current.CanInstruct)
		if err != nil {
			return ObservationRecord{}, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return ObservationRecord{}, err
		} else if n == 0 {
			fresh, ferr := getObservationForReview(ctx, tx, id)
			if ferr != nil {
				return ObservationRecord{}, ferr
			}
			return ObservationRecord{}, stateConflict(nil, fresh,
				"state changed concurrently: review_state is %q", fresh.ReviewState)
		}
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO observation_review_log(
		observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch, action
	) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, current.ReviewState, current.ReviewState, actor, nullString(strings.TrimSpace(reasonText)), now, action); err != nil {
		return ObservationRecord{}, err
	}

	auth, _ := authctx.From(ctx)
	detailsJSON, err := json.Marshal(map[string]any{
		"action":      action,
		"from_state":  current.ReviewState,
		"to_state":    current.ReviewState,
		"reason_text": strings.TrimSpace(reasonText),
	})
	if err != nil {
		return ObservationRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(
		principal_id, credential_id, request_id, action, resource_type, resource_id, details_json, created_at_epoch
	) VALUES (?, ?, ?, 'observation.review_action', 'observation', ?, ?, ?)`,
		nullString(auth.PrincipalID), nullString(auth.CredentialID), nullString(auth.RequestID), fmt.Sprint(id), string(detailsJSON), now); err != nil {
		return ObservationRecord{}, err
	}
	// Return the row the action PRODUCED, not the pre-read (round-2 review
	// NEW MAJOR 2): the flags just changed.
	updated, err := getObservationForReview(ctx, tx, id)
	if err != nil {
		return ObservationRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return ObservationRecord{}, err
	}
	return updated, nil
}

// ErrElevationConflict marks an elevate/demote precondition failure: the
// observation exists but is not in the state the action requires. Handlers
// map it to HTTP 409.
var ErrElevationConflict = errors.New("elevation conflict")

// ElevateObservation sets can_instruct=1 on an accepted observation: the
// operator-pulled act that makes it ride the operating-notes block of every
// future session for its project (docs/ELEVATION-DESIGN.md). Elevation is a
// second gate above accept, never a shortcut past it: anything not accepted
// is an ErrElevationConflict.
//
// provenance='user_confirmed' and can_instruct=1 are set in ONE statement so
// the schema 0007 trigger (can_instruct=1 requires provenance in
// user_confirmed/imported) sees both new values together; a can_instruct-first
// two-statement order would abort on rows whose provenance is still inferred.
func (s *Store) ElevateObservation(ctx context.Context, id int64, reasonText string) (ObservationRecord, error) {
	return s.setElevation(ctx, id, reasonText, true)
}

// DemoteObservation is the inverse of ElevateObservation: can_instruct back
// to 0, provenance untouched, review-log entry. Elevation at this blast
// radius must be reversible. Demoting an observation that is not currently
// elevated is an ErrElevationConflict.
func (s *Store) DemoteObservation(ctx context.Context, id int64, reasonText string) (ObservationRecord, error) {
	return s.setElevation(ctx, id, reasonText, false)
}

func (s *Store) setElevation(ctx context.Context, id int64, reasonText string, elevate bool) (ObservationRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ObservationRecord{}, err
	}
	defer tx.Rollback()

	current, err := getObservationForReview(ctx, tx, id)
	if err != nil {
		return ObservationRecord{}, err
	}

	var action, update string
	if elevate {
		// Raw capture projections are locked out of review actions
		// (ApplyObservationReviewAction) and out of elevation alike: they sit
		// at accepted by construction, and the operating-notes read excludes
		// kind=raw, so an elevated raw would silently never ride.
		// Conflicts carry current-state metadata (memoire:D207 residual 1):
		// "already elevated" is the LANDED signal for a client whose elevate
		// response was lost — it must be machine-readable, not prose.
		if current.Kind == "raw" {
			return ObservationRecord{}, stateConflict(ErrElevationConflict, current, "elevation conflict: raw observations cannot be elevated")
		}
		if current.ReviewState != "accepted" {
			return ObservationRecord{}, stateConflict(ErrElevationConflict, current, "elevation conflict: elevate requires review_state=accepted, got %q", current.ReviewState)
		}
		if current.CanInstruct == 1 {
			return ObservationRecord{}, stateConflict(ErrElevationConflict, current, "elevation conflict: observation %d is already elevated", id)
		}
		// Slice E (memoire:D146, R-5): elevation no longer promotes provenance
		// — that is the confirm verdict's job. Requiring it here keeps the
		// double-human-gesture invariant real: confirm (one review-log row),
		// then elevate (a second). 0007 enforces the same at the trigger
		// level; this check turns the trigger abort into a clean 409.
		if !inSlice(current.Provenance, "user_confirmed", "imported") {
			return ObservationRecord{}, stateConflict(ErrElevationConflict, current, "elevation conflict: elevate requires provenance user_confirmed or imported (confirm the observation first), got %q", current.Provenance)
		}
		action = "elevate"
		update = `UPDATE observations SET can_instruct = 1
			WHERE id = ? AND review_state = ? AND provenance = ? AND can_instruct = 0`
	} else {
		if current.CanInstruct != 1 {
			return ObservationRecord{}, stateConflict(ErrElevationConflict, current, "elevation conflict: observation %d is not elevated", id)
		}
		action = "demote"
		update = `UPDATE observations SET can_instruct = 0
			WHERE id = ? AND review_state = ? AND provenance = ? AND can_instruct = 1`
	}
	casHook("elevation", id)

	// Full-snapshot CAS, same discipline as transitionReview (round-2
	// review NEW MAJOR 2): a can_instruct-only predicate let a demote land
	// on a row a concurrent dispute had already moved, logging a stale
	// from_state — winner order decided correctness. Now every field of the
	// pre-read participates and the log below records a snapshot the CAS
	// just proved current.
	res, err := tx.ExecContext(ctx, update, id, current.ReviewState, current.Provenance)
	if err != nil {
		return ObservationRecord{}, err
	}
	// Enforce the optimistic guard: a zero-row update means the state moved
	// between the pre-read and the write — surface the conflict instead of
	// logging an action that never happened.
	if rows, err := res.RowsAffected(); err != nil {
		return ObservationRecord{}, err
	} else if rows == 0 {
		// The optimistic UPDATE guard caught a race: the conflict must carry
		// the WINNING row's state, not the stale pre-read (precondition
		// review round-1 MAJOR 2) — a lost-elevation retry recognizes its
		// landed elevate only from fresh can_instruct=1.
		fresh, ferr := getObservationForReview(ctx, tx, id)
		if ferr != nil {
			return ObservationRecord{}, ferr
		}
		return ObservationRecord{}, stateConflict(ErrElevationConflict, fresh, "elevation conflict: observation %d changed concurrently", id)
	}

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `INSERT INTO observation_review_log(
		observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch, action
	) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, current.ReviewState, current.ReviewState, reviewActor(ctx), nullString(strings.TrimSpace(reasonText)), now, action); err != nil {
		return ObservationRecord{}, err
	}

	auth, _ := authctx.From(ctx)
	detailsJSON, err := json.Marshal(map[string]any{
		"action":      action,
		"reason_text": strings.TrimSpace(reasonText),
	})
	if err != nil {
		return ObservationRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(
		principal_id, credential_id, request_id, action, resource_type, resource_id, details_json, created_at_epoch
	) VALUES (?, ?, ?, ?, 'observation', ?, ?, ?)`,
		nullString(auth.PrincipalID), nullString(auth.CredentialID), nullString(auth.RequestID), "observation."+action, fmt.Sprint(id), string(detailsJSON), now); err != nil {
		return ObservationRecord{}, err
	}

	updated, err := getObservationForReview(ctx, tx, id)
	if err != nil {
		return ObservationRecord{}, err
	}
	ids, err := sourceEventIDsTx(ctx, tx, id)
	if err != nil {
		return ObservationRecord{}, err
	}
	updated.SourceEventIDs = ids
	if err := tx.Commit(); err != nil {
		return ObservationRecord{}, err
	}
	return updated, nil
}

func (s *Store) ListObservationReviewLog(ctx context.Context, filter ObservationReviewLogFilter) ([]ObservationReviewLogRecord, error) {
	filter.Limit = normalizeLimit(filter.Limit, 50, 200)
	where, args := observationReviewLogWhere(filter)
	args = append(args, filter.Limit)
	rows, err := s.rdb.QueryContext(ctx, `SELECT observation_id, from_state, to_state, actor,
		COALESCE(reason_text, ''), occurred_at_epoch, COALESCE(action, '')
		FROM observation_review_log
		WHERE `+where+`
		ORDER BY occurred_at_epoch DESC, id DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ObservationReviewLogRecord
	for rows.Next() {
		var rec ObservationReviewLogRecord
		if err := rows.Scan(&rec.ObservationID, &rec.FromState, &rec.ToState, &rec.Actor, &rec.ReasonText, &rec.OccurredAtEpoch, &rec.Action); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) DeleteEvent(ctx context.Context, id int64) error {
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `UPDATE events SET deleted_at_epoch = ? WHERE id = ? AND deleted_at_epoch IS NULL`, now, id)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}
	_, err = tx.ExecContext(ctx, `UPDATE observations SET deleted_at_epoch = ? WHERE event_id = ? AND deleted_at_epoch IS NULL`, now, id)
	if err != nil {
		return err
	}
	auth, _ := authctx.From(ctx)
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_log(
		principal_id, credential_id, request_id, action, resource_type, resource_id, details_json, created_at_epoch
	) VALUES (?, ?, ?, 'event.delete', 'event', ?, '{}', ?)`,
		nullString(auth.PrincipalID), nullString(auth.CredentialID), nullString(auth.RequestID), fmt.Sprint(id), now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Search(ctx context.Context, q, project string, limit int) ([]SearchHit, error) {
	return s.SearchWithFilter(ctx, q, ObservationFilter{Project: project, Limit: limit})
}

func (s *Store) SearchWithFilter(ctx context.Context, q string, filter ObservationFilter) ([]SearchHit, error) {
	match := SanitizeFTS(q)
	if match == "" {
		return nil, errors.New("empty search query")
	}
	filter.Limit = normalizeLimit(filter.Limit, 20, 200)
	if s.hybridActive(filter) {
		return s.searchHybrid(ctx, q, match, filter)
	}
	return s.searchFTS(ctx, match, filter, filter.Limit)
}

// searchFTS is the FTS lane, exactly the pre-hybrid SearchWithFilter body.
// limit is explicit because the hybrid path fetches a deeper candidate
// list (hybridLaneK) than the caller's limit before fusion.
func (s *Store) searchFTS(ctx context.Context, match string, filter ObservationFilter, limit int) ([]SearchHit, error) {
	where, filterArgs := observationWhere(filter, "o")
	var query string
	var args []any
	if s.engine == EnginePostgres && s.hasPGSearch {
		// Real BM25 via pg_search (gate ruling option A, 2026-07-22): the
		// ||| match-disjunction operator tokenizes the input with the
		// FIELD's tokenizer (alignment by construction) and ORs the terms —
		// the same candidate semantics as SanitizeFTS + FTS5, with bm25
		// scoring (IDF included, the thing tsvector ranking lacks).
		// pdb.snippet carries the [/]-bracket shape of FTS5 snippet().
		args = append(args, ftsToMatchText(match))
		args = append(args, filterArgs...)
		args = append(args, limit)
		query = `SELECT o.id, COALESCE(o.session_id, ''), o.project,
		o.kind, o.review_state, COALESCE(o.source_adapter, ''), COALESCE(o.event_type, ''),
		o.text, COALESCE(pdb.snippet(o.text, start_tag => '[', end_tag => ']'), ''),
		o.occurred_at_epoch, pdb.score(o.id)::float8, o.noise_candidate, o.can_instruct,
		COALESCE(o.provenance, '')
		FROM observations o
		WHERE o.text ||| ? AND ` + where + `
		ORDER BY pdb.score(o.id) DESC, o.id DESC
		LIMIT ?`
	} else if s.engine == EnginePostgres {
		// tsvector twin of the FTS5 read: same sanitized tokens, same OR
		// semantics, GIN-driven candidates. Two bm25-alignment choices from
		// the 2026-07-22 injection-quality measurement (129 real queries,
		// judged: 42 degradations before, see the gate report):
		//   - websearch_to_tsquery instead of to_tsquery with pre-quoted
		//     literals: query terms run through the SAME parser as
		//     to_tsvector, so tokens like ext_log_entries match however the
		//     parser split the documents (to_tsquery literals bypassed the
		//     parser and silently lost whole candidate classes). SanitizeFTS
		//     output is passed VERBATIM: its double quotes are websearch
		//     phrase syntax, which neutralizes operator-shaped tokens — a
		//     literal "OR" token stays a term and "-rf" stays a term instead
		//     of NOT syntax (round-1 finding 4); the unquoted OR joiners are
		//     the disjunction, exactly as in FTS5.
		//   - ts_rank_cd normalization 1|4 (log-length + extent-density):
		//     unnormalized rank_cd let long notification dumps outrank
		//     short specific facts; bm25 length-normalizes, so must we.
		// ts_rank_cd is higher-is-better so the sort is DESC where bm25
		// sorts ASC; residual ranking DELTAS are measured and journaled
		// (D157/D158), not a bug. ts_headline carries the same [/]/16-word
		// shape as snippet(); its ellipsis rendering differs — known delta.
		args = append(args, match)
		args = append(args, filterArgs...)
		args = append(args, limit)
		query = `SELECT o.id, COALESCE(o.session_id, ''), o.project,
		o.kind, o.review_state, COALESCE(o.source_adapter, ''), COALESCE(o.event_type, ''),
		o.text, ts_headline('simple', o.text, q, 'StartSel=[, StopSel=], MaxWords=16, MinWords=1'),
		o.occurred_at_epoch, ts_rank_cd(o.text_tsv, q, 5)::float8, o.noise_candidate, o.can_instruct,
		COALESCE(o.provenance, '')
		FROM observations o, websearch_to_tsquery('simple', ?) q
		WHERE o.text_tsv @@ q AND ` + where + `
		ORDER BY ts_rank_cd(o.text_tsv, q, 5) DESC, o.id DESC
		LIMIT ?`
	} else {
		// CROSS JOIN is SQLite's explicit join-order directive: the FTS index must
		// drive (produce MATCH candidates, then point-join observations by rowid).
		// With a plain JOIN the planner inverts the order — outer loop over an
		// observations index, FTS probed once PER ROW — which cost 25s at 256k
		// rows where the driven shape costs ~20ms. ORDER BY rank (identical
		// ordering to bm25 with default weights) additionally enables FTS5's
		// sorted-by-rank early-stop, so the scan ends at LIMIT accepted rows
		// instead of ranking every candidate.
		args = append(args, match)
		args = append(args, filterArgs...)
		args = append(args, limit)
		query = `SELECT o.id, COALESCE(o.session_id, ''), o.project,
		o.kind, o.review_state, COALESCE(o.source_adapter, ''), COALESCE(o.event_type, ''),
		o.text, snippet(observations_fts, 0, '[', ']', '...', 16), o.occurred_at_epoch, bm25(observations_fts), o.noise_candidate, o.can_instruct,
		COALESCE(o.provenance, '')
		FROM observations_fts
		CROSS JOIN observations o ON o.id = observations_fts.rowid
		WHERE observations_fts MATCH ? AND ` + where + `
		ORDER BY rank
		LIMIT ?`
	}
	rows, err := s.rdb.QueryContext(ctx, query, args...)
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

func (s *Store) Timeline(ctx context.Context, project string, limit int) ([]SearchHit, error) {
	return s.TimelineWithFilter(ctx, ObservationFilter{Project: project, Limit: limit})
}

func (s *Store) TimelineWithFilter(ctx context.Context, filter ObservationFilter) ([]SearchHit, error) {
	filter.Limit = normalizeLimit(filter.Limit, 50, 500)
	where, args := observationWhere(filter, "o")
	args = append(args, filter.Limit)
	rows, err := s.rdb.QueryContext(ctx, `SELECT o.id, COALESCE(o.session_id, ''), o.project,
		o.kind, o.review_state, COALESCE(o.source_adapter, ''), COALESCE(o.event_type, ''), o.text, o.occurred_at_epoch, o.noise_candidate, o.can_instruct,
		COALESCE(o.provenance, '')
		FROM observations o WHERE `+where+`
		ORDER BY o.occurred_at_epoch DESC, o.id DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.ID, &h.SessionID, &h.Project, &h.Kind, &h.ReviewState, &h.SourceAdapter, &h.EventType, &h.Snippet, &h.OccurredAtEpoch, &h.NoiseCandidate, &h.CanInstruct, &h.Provenance); err != nil {
			return nil, err
		}
		h.syncOccurredAt()
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

func (s *Store) Stats(ctx context.Context, dbPath string) (Stats, error) {
	var st Stats
	queries := map[*int64]string{
		&st.EventCount:              `SELECT count(*) FROM events WHERE deleted_at_epoch IS NULL`,
		&st.ObservationCount:        `SELECT count(*) FROM observations WHERE deleted_at_epoch IS NULL`,
		&st.DeletedEventCount:       `SELECT count(*) FROM events WHERE deleted_at_epoch IS NOT NULL`,
		&st.DeletedObservationCount: `SELECT count(*) FROM observations WHERE deleted_at_epoch IS NOT NULL`,
		&st.LastWriteEpoch:          `SELECT COALESCE(max(received_at_epoch), 0) FROM events`,
	}
	for dest, q := range queries {
		if err := s.rdb.QueryRowContext(ctx, q).Scan(dest); err != nil {
			return Stats{}, err
		}
	}
	if s.engine == EnginePostgres {
		// dbPath is a DSN here; size comes from the server instead of the
		// filesystem. WALBytes stays 0 — Postgres WAL is instance-wide and
		// CNPG-managed, not a per-store health number.
		if err := s.rdb.QueryRowContext(ctx,
			`SELECT pg_database_size(current_database())`).Scan(&st.DBBytes); err != nil {
			return Stats{}, err
		}
		return st, nil
	}
	if info, err := os.Stat(dbPath); err == nil {
		st.DBBytes = info.Size()
	}
	if info, err := os.Stat(dbPath + "-wal"); err == nil {
		st.WALBytes = info.Size()
	}
	return st, nil
}

// CountRedactionContractViolations counts ingested events carrying the
// liaison-evidence backstop violation stamp (memoire:D136 R-2; stamped by
// queuewriter, surfaced on /v1/health). The literal predicate must textually
// match the schema/0011 partial-index WHERE clause — a bound parameter cannot
// prove a partial-index predicate, and INDEXED BY protects the plan from the
// no-stats planner (0008 lesson).
func (s *Store) CountRedactionContractViolations(ctx context.Context) (int64, error) {
	var n int64
	if s.engine == EnginePostgres {
		// The literal predicate must textually match the schema-pg/0001
		// partial index (jsonb-typed: exactly boolean true or numeric 1, the
		// D157 round-2 F5 ruling; the design table pins this expression).
		err := s.rdb.QueryRowContext(ctx,
			`SELECT count(*) FROM events
			 WHERE (meta_json::jsonb -> 'redaction_contract_violation') IN ('true'::jsonb, '1'::jsonb)`).Scan(&n)
		return n, err
	}
	err := s.rdb.QueryRowContext(ctx,
		`SELECT count(*) FROM events INDEXED BY idx_events_redaction_violation
		 WHERE json_extract(meta_json, '$.redaction_contract_violation') = 1`).Scan(&n)
	return n, err
}

// DeriveRunStatusCount is one status cell of the derivation-run ledger,
// feeding the /metrics run-state gauges. endpoint_id is deliberately NOT
// carried: it is a ledger-sourced operator string and must not cross the
// unauthenticated /metrics boundary (review 2026-08-03 F1); per-lane labels
// return only if a ruled-public lane vocabulary exists.
type DeriveRunStatusCount struct {
	Status string
	Count  int64
}

// CountDerivationRunsByStatus aggregates the full derivation_runs ledger by
// status. Full-table aggregate — callers must cache it (the /metrics TTL
// cache does); it is never a per-probe query.
func (s *Store) CountDerivationRunsByStatus(ctx context.Context) ([]DeriveRunStatusCount, error) {
	rows, err := s.rdb.QueryContext(ctx,
		`SELECT status, count(*) FROM derivation_runs GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeriveRunStatusCount
	for rows.Next() {
		var c DeriveRunStatusCount
		if err := rows.Scan(&c.Status, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountFutureSkewEvents is the memoire:D152 one-sided future-skew alert
// source: events whose occurred_at_epoch is AHEAD of received_at_epoch by
// more than threshold seconds are unambiguous producer clock faults. The lag
// direction is deliberately not counted — it is indistinguishable from
// legitimate queue buffering (capture-and-forward-later, D147 slice 3).
// Computed on read like queue_failed_depth: no state, survives restarts.
// The first WHERE conjunct textually matches the schema/0012 partial-index
// predicate (bound parameters cannot prove one; 0008/0011 lesson) and
// INDEXED BY makes a planner regression a loud error instead of a silent
// full scan of events on every health poll (slice-3 round-1 finding 6).
func (s *Store) CountFutureSkewEvents(ctx context.Context, threshold int64) (int64, error) {
	var n int64
	if s.engine == EnginePostgres {
		// Same textually-matching first conjunct as the schema-pg/0001
		// partial index; no INDEXED BY on Postgres (planner has statistics;
		// the parity harness EXPLAIN spot check guards the plan).
		err := s.rdb.QueryRowContext(ctx,
			`SELECT count(*) FROM events
			 WHERE occurred_at_epoch > received_at_epoch
			   AND occurred_at_epoch > received_at_epoch + ?`,
			threshold).Scan(&n)
		return n, err
	}
	err := s.rdb.QueryRowContext(ctx,
		`SELECT count(*) FROM events INDEXED BY idx_events_future_skew
		 WHERE occurred_at_epoch > received_at_epoch
		   AND occurred_at_epoch > received_at_epoch + ?`,
		threshold).Scan(&n)
	return n, err
}

// getObservation and getDerivationRun run on the read pool: their only
// production callers are the public GET surfaces (GetObservationWithFilter,
// GetDerivationRun) — write flows read back through tx-scoped helpers
// instead, so nothing here ever needs uncommitted visibility.
func (s *Store) getObservation(ctx context.Context, query string, args ...any) (ObservationRecord, error) {
	row := s.rdb.QueryRowContext(ctx, query, args...)
	return scanObservationRecord(row)
}

func (s *Store) getDerivationRun(ctx context.Context, query string, args ...any) (DerivationRunRecord, error) {
	row := s.rdb.QueryRowContext(ctx, query, args...)
	return scanDerivationRunRecord(row)
}

func scanObservationRecord(scanner interface{ Scan(...any) error }) (ObservationRecord, error) {
	var rec ObservationRecord
	var eventID, deletedAt sql.NullInt64
	var sessionID, sourceAdapter, eventType, principal, derivationRunID, contentHash, provenance sql.NullString
	var confidence sql.NullFloat64
	var canInstruct, canEvidence, needsConfirmation, noiseCandidate int
	err := scanner.Scan(
		&rec.ID, &eventID, &rec.Project, &sessionID, &rec.Kind, &rec.Text,
		&sourceAdapter, &eventType, &rec.OccurredAtEpoch, &rec.CreatedAtEpoch, &deletedAt,
		&principal, &confidence, &rec.ReviewState, &derivationRunID, &contentHash,
		&provenance, &canInstruct, &canEvidence, &needsConfirmation, &noiseCandidate,
	)
	if err != nil {
		return ObservationRecord{}, err
	}
	if eventID.Valid {
		rec.EventID = eventID.Int64
	}
	if deletedAt.Valid {
		rec.DeletedAtEpoch = deletedAt.Int64
	}
	rec.SessionID = sessionID.String
	rec.SourceAdapter = sourceAdapter.String
	rec.EventType = eventType.String
	rec.PrincipalID = principal.String
	rec.DerivationRunID = derivationRunID.String
	rec.ContentHash = contentHash.String
	if confidence.Valid {
		rec.Confidence = confidence.Float64
	}
	rec.Provenance = provenance.String
	rec.CanInstruct = canInstruct
	rec.CanEvidence = canEvidence
	rec.NeedsConfirmation = needsConfirmation
	rec.NoiseCandidate = noiseCandidate
	return rec, nil
}

func scanDerivationRunRecord(scanner interface{ Scan(...any) error }) (DerivationRunRecord, error) {
	var rec DerivationRunRecord
	var sessionID, model, endpointID, promptVersion, meta sql.NullString
	var completed sql.NullInt64
	err := scanner.Scan(
		&rec.ID, &rec.Project, &sessionID, &rec.Provider, &rec.DataBoundary,
		&model, &endpointID, &rec.DerivationVersion, &promptVersion, &rec.InputHash, &rec.WindowStartEpoch, &rec.WindowEndEpoch,
		&rec.Status, &rec.CreatedAtEpoch, &completed, &meta,
		&rec.ObservationCount, &rec.ProposedObservationCount,
	)
	if err != nil {
		return DerivationRunRecord{}, err
	}
	rec.SessionID = sessionID.String
	rec.Model = model.String
	rec.EndpointID = endpointID.String
	rec.PromptVersion = promptVersion.String
	if completed.Valid {
		rec.CompletedAtEpoch = completed.Int64
	}
	if meta.Valid && strings.TrimSpace(meta.String) != "" {
		rec.Metadata = json.RawMessage(meta.String)
	}
	return rec, nil
}

func sourceEventIDsTx(ctx context.Context, tx dbtx, observationID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT event_id FROM observation_source_events
		WHERE observation_id = ? ORDER BY event_id ASC`, observationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func getObservationForReview(ctx context.Context, tx dbtx, id int64) (ObservationRecord, error) {
	row := tx.QueryRowContext(ctx, `SELECT o.id, o.event_id, o.project, o.session_id, o.kind, o.text,
		o.source_adapter, o.event_type, o.occurred_at_epoch, o.created_at_epoch, o.deleted_at_epoch,
		o.principal_id, o.confidence, o.review_state, o.derivation_run_id, o.content_hash,
		o.provenance, o.can_instruct, o.can_evidence, o.needs_confirmation, o.noise_candidate
		FROM observations o
		WHERE o.id = ?`, id)
	return scanObservationRecord(row)
}

func observationWhere(filter ObservationFilter, alias string) (string, []any) {
	var clauses []string
	var args []any
	if filter.Project != "" {
		clauses = append(clauses, alias+`.project = ?`)
		args = append(args, filter.Project)
	}
	if filter.SessionID != "" {
		clauses = append(clauses, `COALESCE(`+alias+`.session_id, '') = ?`)
		args = append(args, filter.SessionID)
	}
	if filter.Kind != "" {
		clauses = append(clauses, alias+`.kind = ?`)
		args = append(args, filter.Kind)
	}
	if filter.ExcludeKind != "" {
		clauses = append(clauses, alias+`.kind != ?`)
		args = append(args, filter.ExcludeKind)
	}
	if filter.DerivationRunID != "" {
		clauses = append(clauses, alias+`.derivation_run_id = ?`)
		args = append(args, filter.DerivationRunID)
	}
	if filter.CanInstruct {
		clauses = append(clauses, alias+`.can_instruct = 1`)
	}
	if filter.ReviewState != "" {
		// Known review states are inlined as literals, not bound: SQLite can
		// only match a partial index (e.g. the 0008 review-queue indexes,
		// WHERE review_state = 'proposed') when the query text proves the
		// predicate, and a bound parameter never does. Values are whitelisted
		// via allowedReviewState before inlining; anything else keeps the
		// bound-parameter path and its match-nothing behavior.
		if allowedReviewState(filter.ReviewState) {
			clauses = append(clauses, alias+`.review_state = '`+filter.ReviewState+`'`)
		} else {
			clauses = append(clauses, alias+`.review_state = ?`)
			args = append(args, filter.ReviewState)
		}
		if filter.ReviewState != "soft_deleted" {
			clauses = append(clauses, alias+`.deleted_at_epoch IS NULL`)
		}
	} else {
		clauses = append(clauses, alias+`.deleted_at_epoch IS NULL`)
		if filter.IncludeProposed || filter.Kind == "open_question" {
			// Slice E (memoire:D146, R-6): lapsed rides the same low-trust
			// band as proposed on opt-in reads — searchable and ~-marked at
			// injection, reversible on-encounter. Default reads (accepted
			// only) and the feed (explicit review_state=accepted) never see
			// either.
			clauses = append(clauses, alias+`.review_state IN ('accepted', 'proposed', 'lapsed')`)
		} else {
			clauses = append(clauses, alias+`.review_state = 'accepted'`)
		}
	}
	// Default noise exclusion: noise_candidate=1 observations are excluded
	// unless the caller explicitly opts in. This applies on explicit
	// review_state reads too — the review UI keys distinct capped server
	// windows on include_noise, and otherwise-equivalent filter combinations
	// (e.g. can_instruct=true with and without review_state=accepted) must
	// agree on noise visibility.
	if !filter.IncludeNoise {
		clauses = append(clauses, alias+`.noise_candidate = 0`)
	}
	return strings.Join(clauses, " AND "), args
}

func derivationRunWhere(filter DerivationRunFilter) (string, []any) {
	clauses := []string{"1 = 1"}
	args := []any{}
	if filter.Project != "" {
		clauses = append(clauses, "dr.project = ?")
		args = append(args, filter.Project)
	}
	if filter.SessionID != "" {
		clauses = append(clauses, "COALESCE(dr.session_id, '') = ?")
		args = append(args, filter.SessionID)
	}
	if filter.Provider != "" {
		clauses = append(clauses, "dr.provider = ?")
		args = append(args, filter.Provider)
	}
	if filter.Model != "" {
		clauses = append(clauses, "COALESCE(dr.model, '') = ?")
		args = append(args, filter.Model)
	}
	if filter.EndpointID != "" {
		clauses = append(clauses, "COALESCE(dr.endpoint_id, '') = ?")
		args = append(args, filter.EndpointID)
	}
	if filter.Status != "" {
		clauses = append(clauses, "dr.status = ?")
		args = append(args, filter.Status)
	}
	return strings.Join(clauses, " AND "), args
}

func observationReviewLogWhere(filter ObservationReviewLogFilter) (string, []any) {
	if filter.ObservationID > 0 {
		return "observation_id = ?", []any{filter.ObservationID}
	}
	return "1 = 1", nil
}

func normalizeLimit(limit, fallback, max int) int {
	if limit <= 0 || limit > max {
		return fallback
	}
	return limit
}

func validateReviewTransition(kind, fromState, toState string) error {
	if kind == "raw" {
		return errors.New("raw observations are locked at accepted")
	}
	switch {
	case fromState == "proposed" && (toState == "accepted" || toState == "rejected" || toState == "lapsed"):
		return nil
	case fromState == "accepted" && (toState == "superseded" || toState == "soft_deleted" || toState == "disputed"):
		return nil
	case fromState == "disputed" && (toState == "accepted" || toState == "rejected" || toState == "superseded"):
		return nil
	// Slice E (memoire:D146, R-6): lapsed is reversible on-encounter — a human
	// revives or buries a lapsed row the moment it resurfaces. TTL/bulk entry
	// is proposed -> lapsed above; raw stays locked at accepted.
	case fromState == "lapsed" && (toState == "accepted" || toState == "rejected"):
		return nil
	default:
		return fmt.Errorf("unsupported review transition %s -> %s", fromState, toState)
	}
}

func reviewActor(ctx context.Context) string {
	if auth, ok := authctx.From(ctx); ok {
		if auth.CredentialID != "" {
			return auth.CredentialID
		}
		if auth.PrincipalID != "" {
			return auth.PrincipalID
		}
	}
	return authctx.PrincipalLocal
}

func (in DerivationRunInput) Validate() error {
	if in.Project == "" || in.Provider == "" || in.DataBoundary == "" || in.DerivationVersion == "" || in.InputHash == "" {
		return errors.New("project, provider, data_boundary, derivation_version, and input_hash are required")
	}
	if in.WindowStartEpoch <= 0 || in.WindowEndEpoch <= 0 {
		return errors.New("window_start_epoch and window_end_epoch must be positive")
	}
	if in.WindowEndEpoch < in.WindowStartEpoch {
		return errors.New("window_end_epoch must be greater than or equal to window_start_epoch")
	}
	if len(in.Metadata) > 0 && !json.Valid(in.Metadata) {
		return errors.New("metadata must be valid JSON")
	}
	if !allowedProvider(in.Provider) {
		return errors.New("provider must be one of rules, local-qwen, openai, manual")
	}
	if !allowedDataBoundary(in.DataBoundary) {
		return errors.New("data_boundary must be local or external")
	}
	if !providerDataBoundaryAllowed(in.Provider, in.DataBoundary) {
		return errors.New("provider/data_boundary mismatch")
	}
	if (in.Provider == "local-qwen" || in.Provider == "openai" || in.Provider == "byok") && in.Model == "" {
		return errors.New("model is required for model-backed providers")
	}
	if (in.Provider == "local-qwen" || in.Provider == "byok") && in.EndpointID == "" {
		return errors.New("endpoint_id is required for local-qwen and byok")
	}
	return nil
}

func (in DerivedObservationInput) Validate() error {
	if in.Project == "" || in.Kind == "" || in.Text == "" || in.DerivationRunID == "" || in.ContentHash == "" {
		return errors.New("project, kind, text, derivation_run_id, and content_hash are required")
	}
	// Defense in depth: "unknown" is the audit channel, never a storable kind
	// (docs/MODEL-PROVIDER.md "Unknown Audit Channel"). The parser already keeps
	// it out of write targets, but any direct caller of InsertDerivedObservation
	// must be blocked too. The schema intentionally has no kind CHECK constraint
	// (kind is unconstrained TEXT so additive kinds like "gotcha" need no
	// migration), so this guardrail lives in validation.
	if in.Kind == "unknown" {
		return errors.New(`kind "unknown" is the audit channel and is not a storable observation kind`)
	}
	if len(in.SourceEventIDs) == 0 {
		return errors.New("at least one source_event_id is required")
	}
	if len(in.Metadata) > 0 && !json.Valid(in.Metadata) {
		return errors.New("metadata must be valid JSON")
	}
	if in.ReviewState != "" && !allowedReviewState(in.ReviewState) {
		return errors.New("review_state must be one of proposed, accepted, rejected, superseded, soft_deleted")
	}
	return nil
}

func (in EventInput) Validate() error {
	if in.Project == "" || in.SessionID == "" || in.SourceAdapter == "" || in.EventType == "" {
		return errors.New("project, session_id, source_adapter, and event_type are required")
	}
	if len(in.Payload) > 0 && !json.Valid(in.Payload) {
		return errors.New("payload must be valid JSON")
	}
	if len(in.Metadata) > 0 && !json.Valid(in.Metadata) {
		return errors.New("metadata must be valid JSON")
	}
	return nil
}

func (in EventInput) OccurredEpoch() (int64, error) {
	if in.OccurredAtEpoch > 0 {
		return in.OccurredAtEpoch, nil
	}
	if in.OccurredAt != "" {
		t, err := time.Parse(time.RFC3339Nano, in.OccurredAt)
		if err != nil {
			return 0, err
		}
		return t.Unix(), nil
	}
	return time.Now().Unix(), nil
}

func (in EventInput) IdempotencyKey(payload json.RawMessage) string {
	if in.SourceEventID != "" {
		sum := sha256.Sum256([]byte(in.SourceAdapter + ":" + in.SourceEventID))
		return hex.EncodeToString(sum[:])
	}
	type stable struct {
		Project         string          `json:"project"`
		SessionID       string          `json:"session_id"`
		SourceAdapter   string          `json:"source_adapter"`
		EventType       string          `json:"event_type"`
		OccurredAt      string          `json:"occurred_at,omitempty"`
		OccurredAtEpoch int64           `json:"occurred_at_epoch,omitempty"`
		Payload         json.RawMessage `json:"payload"`
	}
	b, _ := json.Marshal(stable{in.Project, in.SessionID, in.SourceAdapter, in.EventType, in.OccurredAt, in.OccurredAtEpoch, payload})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var tokenRE = regexp.MustCompile(`[\pL\pN_@./:-]+`)

func SanitizeFTS(q string) string {
	parts := tokenRE.FindAllString(q, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, `"`+strings.ReplaceAll(p, `"`, `""`)+`"`)
	}
	return strings.Join(out, " OR ")
}

// IngestText is the exact search-visible text Ingest stores for an event —
// exported for the pgshadow replay lane (slice-5 groundwork, D157): the
// Postgres replay must project text identically or parity hashes diverge.
func IngestText(in EventInput, payload json.RawMessage) string {
	if in.Text != "" {
		return in.Text
	}
	return projectText(in, payload)
}

func projectText(in EventInput, payload json.RawMessage) string {
	if text, ok := claudeUserPromptText(in, payload); ok {
		return text
	}
	var payloadText string
	if len(payload) > 0 && string(payload) != "{}" {
		payloadText = string(payload)
	}
	return strings.TrimSpace(strings.Join([]string{in.Project, in.SourceAdapter, in.EventType, in.CWD, in.Text, payloadText}, " "))
}

func claudeUserPromptText(in EventInput, payload json.RawMessage) (string, bool) {
	if in.SourceAdapter != "claude-code" || !strings.EqualFold(in.EventType, "claude_UserPromptSubmit") {
		return "", false
	}
	if strings.TrimSpace(in.Text) != "" {
		return strings.TrimSpace(in.Text), true
	}
	prompt := payloadFieldString(payload, "prompt", "user_prompt", "userPrompt", "message", "text")
	if prompt == "" {
		return "", true
	}
	return strings.TrimSpace(prompt), true
}

func payloadFieldString(payload json.RawMessage, keys ...string) string {
	var v map[string]any
	if err := json.Unmarshal(payload, &v); err != nil {
		return ""
	}
	for _, key := range keys {
		if s, ok := v[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func (s *Store) maxOccurredAtForEvents(ctx context.Context, eventIDs []int64) (int64, error) {
	args := make([]any, 0, len(eventIDs))
	placeholders := make([]string, 0, len(eventIDs))
	for _, id := range eventIDs {
		args = append(args, id)
		placeholders = append(placeholders, "?")
	}
	var occurred int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(occurred_at_epoch), 0)
		FROM events
		WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...).Scan(&occurred)
	if err != nil {
		return 0, err
	}
	if occurred == 0 {
		return 0, sql.ErrNoRows
	}
	return occurred, nil
}

func ensureDerivationRunExists(ctx context.Context, tx dbtx, id string) error {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM derivation_runs WHERE id = ?`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("derivation run %q not found", id)
	}
	return err
}

func acceptedOverlapExists(ctx context.Context, tx dbtx, project, kind, contentHash string, eventIDs []int64) (bool, error) {
	args := []any{project, kind, contentHash}
	placeholders := make([]string, 0, len(eventIDs))
	for _, eventID := range eventIDs {
		args = append(args, eventID)
		placeholders = append(placeholders, "?")
	}
	var observationID int64
	err := tx.QueryRowContext(ctx, `SELECT o.id
		FROM observations o
		JOIN observation_source_events ose ON ose.observation_id = o.id
		WHERE o.project = ?
		  AND o.kind = ?
		  AND o.content_hash = ?
		  AND o.review_state = 'accepted'
		  AND o.deleted_at_epoch IS NULL
		  AND ose.event_id IN (`+strings.Join(placeholders, ",")+`)
		LIMIT 1`, args...).Scan(&observationID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return observationID > 0, nil
}

func DefaultReviewState(modelDerived bool) string {
	if modelDerived {
		return "proposed"
	}
	return "accepted"
}

func derivationRunID(in DerivationRunInput) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		in.Provider,
		in.Model,
		in.DerivationVersion,
		in.Project,
		in.SessionID,
		fmt.Sprint(in.WindowStartEpoch),
		fmt.Sprint(in.WindowEndEpoch),
		in.InputHash,
	}, "\x00")))
	return "drv_" + hex.EncodeToString(sum[:16])
}

func allowedProvider(v string) bool {
	switch v {
	case "rules", "local-qwen", "openai", "manual", "byok":
		return true
	default:
		return false
	}
}

func allowedDataBoundary(v string) bool {
	switch v {
	case "local", "external":
		return true
	default:
		return false
	}
}

func allowedReviewState(v string) bool {
	switch v {
	case "proposed", "accepted", "rejected", "superseded", "soft_deleted", "disputed", "lapsed":
		return true
	default:
		return false
	}
}

func providerDataBoundaryAllowed(provider, dataBoundary string) bool {
	switch provider {
	case "openai":
		return dataBoundary == "external"
	case "rules", "local-qwen", "manual":
		return dataBoundary == "local"
	case "byok":
		// The user declares where their endpoint lives (BYOK design Q2,
		// operator-ruled 2026-08-05): either boundary is legal, but it must
		// be one of the two — allowedDataBoundary already enforced that.
		return dataBoundary == "local" || dataBoundary == "external"
	default:
		return false
	}
}

func allowedRunStatus(v string) bool {
	switch v {
	case "pending", "running", "completed", "partial", "failed":
		return true
	default:
		return false
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// allowedProvenance returns provenance values that the store accepts as valid.
func allowedProvenance(v string) bool {
	switch v {
	case "observed", "inferred", "generated", "user_confirmed", "imported":
		return true
	default:
		return false
	}
}

// allowedDerivationProvenance returns provenance values allowed on model-derived
// observations at write time.
func allowedDerivationProvenance(v string) bool {
	switch v {
	case "observed", "inferred", "generated":
		return true
	default:
		return false
	}
}

// validateUsePolicy enforces the can_instruct invariant: can_instruct=1 requires
// provenance in {'user_confirmed','imported'}.
func validateUsePolicy(provenance string, canInstruct int) error {
	if canInstruct == 1 && !inSlice(provenance, "user_confirmed", "imported") {
		return fmt.Errorf("can_instruct requires provenance user_confirmed or imported, got %q", provenance)
	}
	return nil
}

func inSlice(v string, list ...string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// appInvariantCheck mirrors the DB trigger: can_instruct=1 requires
// provenance in {'user_confirmed','imported'}. Called before the DB insert so
// the application fails fast and reports a clean error.
func appInvariantCheck(provenance string, canInstruct int) error {
	if canInstruct == 1 && !inSlice(provenance, "user_confirmed", "imported") {
		return fmt.Errorf("app invariant: can_instruct requires provenance user_confirmed or imported, got %q", provenance)
	}
	return nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func tokenPrefix(token string) string {
	if len(token) <= 12 {
		return token
	}
	return token[:12]
}
