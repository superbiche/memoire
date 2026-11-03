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
	"sync"
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
	// lastWrite is the highest received_at_epoch this process knows of:
	// seeded from the exact max off the probe path, raised by every committed
	// ingest and by the newest row HealthStats reads. It lets /v1/health
	// report the max-receipt last write without scanning events. Until the
	// seed lands, HealthStats reports the value as unknown.
	lastWrite atomic.Int64
	seed      lastWriteSeed
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
	// SupersedesID names the row this one replaced, when it replaced one: an
	// operator rewrite (memoire:D146) or a later derivation cut restating an
	// earlier cut's observation with more information (Fibery #344). The
	// column has existed since schema/0001 and was write-only until #344 —
	// filiation was recorded and unreadable. One pointer, no traversal: the
	// chain read at retrieval is #335's job.
	SupersedesID *int64 `json:"supersedes_id,omitempty"`
	// Usage is the attribution-at-Stop aggregate (usage.go), attached by the
	// read handlers that show it; nil when no memory_usage event names the
	// row, which the UI must render as "no signal", not as zero use.
	Usage *UsageSummary `json:"usage,omitempty"`
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
	ID            int64  `json:"id"`
	SessionID     string `json:"session_id,omitempty"`
	Project       string `json:"project"`
	Kind          string `json:"kind"`
	ReviewState   string `json:"review_state"`
	SourceAdapter string `json:"source_adapter,omitempty"`
	EventType     string `json:"event_type,omitempty"`
	Text          string `json:"text,omitempty"`
	Snippet       string `json:"snippet"`
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
	// DerivationRunID names the derivation run that produced the
	// observation (schema/0004). Since Fibery #343 a run is one rhythm
	// segment, so the run id is the story unit a hit belongs to. Empty for
	// raw capture projections and for rows derived before the column.
	DerivationRunID string `json:"derivation_run_id,omitempty"`
	// Siblings are the other observations of the same derivation run —
	// the co-derived story around this hit (Fibery #335). Handlers attach
	// them after the search lanes return; the lanes never fill them. No
	// text travels in a sibling, so the redaction and privacy surface is
	// unchanged.
	Siblings []ObservationSibling `json:"siblings,omitempty"`
}

// ObservationSibling is a textless pointer to a run-sibling observation.
// It carries only what a consumer needs to decide whether to spend a
// per-id fetch: the id, the kind, the governance state and the elevation
// flag.
type ObservationSibling struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	ReviewState string `json:"review_state"`
	CanInstruct int    `json:"can_instruct"`
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
	// OccurredAfterEpoch, when positive, bounds results to observations
	// whose occurred_at_epoch is at or after it: the L0010 dated read
	// window. Zero is no bound, additive like Limit; the handler layer
	// parses and rejects malformed or negative values before the filter is
	// ever built, so the store only ever sees a sane epoch.
	OccurredAfterEpoch int64 `json:"occurred_after,omitempty"`
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
	// IncludeAgentAccepted lifts the feed actor-gate (triage lane v1, R-D)
	// for reads that must show the operator everything — the review and
	// browse surfaces. Every read leaves it false, which is the fail-closed
	// default: an accepted row whose accept transition was rendered by an
	// `agent:*` actor is feed-INELIGIBLE. See agentAcceptedExclusion.
	IncludeAgentAccepted bool `json:"include_agent_accepted,omitempty"`
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
	// LastWriteKnown is false when LastWriteEpoch is only a lower bound
	// (HealthStats before its exact seed has landed). Stats always sets it.
	LastWriteKnown bool `json:"-"`
}

// lastWriteSeed runs the exact max(received_at_epoch) scan for HealthStats
// in the background, at most one at a time, retrying after a failure. Close
// cancels a running scan.
type lastWriteSeed struct {
	mu      sync.Mutex
	done    atomic.Bool
	running bool
	next    time.Time
	cancel  context.CancelFunc
	closed  bool
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
	// D0249: PG store must be C-collated (SQLite BINARY parity). Checked
	// BEFORE migrate so a mis-collated database never receives our DDL —
	// fail-loud, not half-materialized.
	if err := checkPGCollation(context.Background(), handle); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("REFUSING to open Postgres store: %w", err)
	}
	store := &Store{db: handle, rdb: handle, engine: EnginePostgres}
	// Schema appliers are serialized fleet-wide (2026-08-27 deadlock: two
	// derive CronJobs opening at :03 re-ran 0001_baseline.sql concurrently and
	// deadlocked). Taken AFTER the collation preflight on purpose — a
	// mis-collated database is refused without ever queueing for the lock.
	// Not deferred: the error branches close the pool before returning, and a
	// deferred release would then run against a closed pool.
	release, err := AcquirePGMigrateLock(context.Background(), conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	merr := store.migrate(schemaPGDir)
	release()
	if merr != nil {
		_ = conn.Close()
		return nil, merr
	}
	// Capability probe only — Open never provisions and never fails for
	// pg_search reasons (round-1 finding 1: the optional quality upgrade
	// must not be a hard startup dependency, and app-path DDL at Open
	// blocked writes for the whole index build).
	store.hasPGSearch = detectPGSearch(store.db)
	store.hasPGVector = detectPGVector(store.db)
	return store, nil
}

// pgCollationOK is the decision half of the D0249 preflight: a memoire
// Postgres store orders TEXT bytewise, like SQLite's BINARY collation. Only
// "C" and its byte-identical alias "POSIX" qualify. Everything else —
// including the frequently-confused "C.UTF-8", which is a UTF-8 locale and
// NOT C ordering — is out of contract.
func pgCollationOK(datcollate string) bool {
	return datcollate == "C" || datcollate == "POSIX"
}

// pgLocaleProviderOK is the provider half of the D0249 preflight (review
// residual 3, closed structurally): on PG15+ `datcollate` alone is blind —
// an ICU-provider database can report datcollate='C' while ICU governs
// ordering. Only the libc provider ('c', where datcollate is authoritative)
// and the PG17+ builtin provider pinned to the 'C' locale order bytewise.
// Everything else — ICU ('i') included — is out of contract.
func pgLocaleProviderOK(provider, datlocale string) bool {
	switch provider {
	case "c":
		return true
	case "b":
		return datlocale == "C"
	default:
		return false
	}
}

// checkPGCollation refuses to open a Postgres store whose database is not
// C-collated (memoire:D0249). A deployment mistake must not disguise itself
// as a working store that silently orders text differently from SQLite and
// from the replay port, so this fails startup instead of warning. The
// refusal names the found and required collation and never the DSN.
func checkPGCollation(ctx context.Context, conn dbc) error {
	var datcollate string
	if err := conn.QueryRowContext(ctx,
		`SELECT datcollate FROM pg_database WHERE datname = current_database()`).Scan(&datcollate); err != nil {
		return fmt.Errorf("collation preflight: %w", err)
	}
	if !pgCollationOK(datcollate) {
		return fmt.Errorf(`Postgres store out of contract (memoire:D0249): database collation is %q, memoire requires "C" (bytewise order, SQLite BINARY parity); recreate the database with LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0 and re-copy`, datcollate)
	}

	// Provider leg: pg_database.datlocprovider exists from PG15. Probing the
	// catalog first keeps pre-15 servers (libc-only, datcollate authoritative)
	// on the fast path without driver-specific undefined-column inspection.
	var one int
	err := conn.QueryRowContext(ctx,
		`SELECT 1 FROM pg_attribute WHERE attrelid = 'pg_database'::regclass AND attname = 'datlocprovider'`).Scan(&one)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("collation preflight: %w", err)
	}
	var provider string
	if err := conn.QueryRowContext(ctx,
		`SELECT datlocprovider::text FROM pg_database WHERE datname = current_database()`).Scan(&provider); err != nil {
		return fmt.Errorf("collation preflight: %w", err)
	}
	var datlocale string
	if provider == "b" {
		// The builtin provider is PG17+, where pg_database.datlocale exists.
		if err := conn.QueryRowContext(ctx,
			`SELECT COALESCE(datlocale, '') FROM pg_database WHERE datname = current_database()`).Scan(&datlocale); err != nil {
			return fmt.Errorf("collation preflight: %w", err)
		}
	}
	if !pgLocaleProviderOK(provider, datlocale) {
		return fmt.Errorf(`Postgres store out of contract (memoire:D0249): database locale provider is %q, memoire requires libc with "C" collation (an ICU- or non-C-builtin-provider database can order text differently from SQLite BINARY even when datcollate reads "C"); recreate the database with LOCALE_PROVIDER libc LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0 and re-copy`, provider)
	}
	return nil
}

// bm25IndexState is the catalog snapshot of the pg_search BM25 index on
// THE observations table. Two exact definitions are in force (a20 = A,
// 2026-11-01): the original (id, text) index, and v2, which carries the
// read-filter columns so pg_search can push the observationWhere
// predicates INTO the Tantivy query instead of re-checking them as a
// heap_filter (on the ~3M-row hub that heap_filter is the 12-15 s). Any
// other relation under the two reserved names — a different definition,
// an invalid (failed-build) index — is not usable and not droppable by
// the reindex command.
type bm25IndexState struct {
	old, v2 bool // an index under that name exists
	oldDef  bool // exists with the exact old definition + key_field=id (any validity)
	v2Def   bool // exists with the exact v2 definition + key_field=id (any validity)
	oldOK   bool // exists, valid, ready, exact old definition + key_field=id
	v2OK    bool // exists, valid, ready, exact v2 definition + key_field=id
}

// The exact index definitions as rendered by pg_get_indexdef, with only
// the schema qualification of the table left as a LIKE wildcard
// (verified against ParadeDB 18-v0.24.3, the hub's image): the pdb
// literal cast renders double-parenthesised, the reloption renders
// UNQUOTED (key_field=id, not key_field='id').
const (
	bm25IndexOldName = "idx_observations_bm25"
	bm25IndexV2Name  = "idx_observations_bm25_v2"

	bm25IndexLikeOld = "CREATE INDEX " + bm25IndexOldName +
		" ON %.observations USING bm25 (id, text) WITH (key_field=id)"
	bm25IndexLikeV2 = "CREATE INDEX " + bm25IndexV2Name +
		" ON %.observations USING bm25 (id, text, ((kind)::pdb.literal), ((project)::pdb.literal), ((review_state)::pdb.literal), ((session_id)::pdb.literal), ((derivation_run_id)::pdb.literal), occurred_at_epoch, deleted_at_epoch, noise_candidate, can_instruct) WITH (key_field=id)"
)

// pgSearchIndexV2Columns is the v2 index definition — the read-filter
// surface of observationWhere: kind, project, review_state, session_id,
// derivation_run_id as pdb.literal text columns (the cast is what makes
// the bare `col = ?` clause pushable; NULL stays NULL, nothing is folded
// to the empty string), and occurred_at_epoch, deleted_at_epoch,
// noise_candidate, can_instruct as numeric columns.
const pgSearchIndexV2Columns = "(id, text, (kind::pdb.literal), (project::pdb.literal), (review_state::pdb.literal), " +
	"(session_id::pdb.literal), (derivation_run_id::pdb.literal), " +
	"occurred_at_epoch, deleted_at_epoch, noise_candidate, can_instruct)"

func pgSearchIndexState(ctx context.Context, conn interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (bm25IndexState, error) {
	var st bm25IndexState
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_extension WHERE extname = 'pg_search'`).Scan(&n); err != nil {
		return st, err
	}
	if n == 0 {
		return st, nil
	}
	err := conn.QueryRowContext(ctx, `SELECT
			COALESCE(bool_or(ic.relname = '`+bm25IndexOldName+`'), false),
			COALESCE(bool_or(ic.relname = '`+bm25IndexOldName+`'
				AND am.amname = 'bm25'
				AND pg_get_indexdef(i.indexrelid) LIKE '`+bm25IndexLikeOld+`'
				AND EXISTS (SELECT 1 FROM unnest(ic.reloptions) o WHERE o = 'key_field=id')), false),
			COALESCE(bool_or(ic.relname = '`+bm25IndexOldName+`'
				AND i.indisvalid AND i.indisready
				AND am.amname = 'bm25'
				AND pg_get_indexdef(i.indexrelid) LIKE '`+bm25IndexLikeOld+`'
				AND EXISTS (SELECT 1 FROM unnest(ic.reloptions) o WHERE o = 'key_field=id')), false),
			COALESCE(bool_or(ic.relname = '`+bm25IndexV2Name+`'), false),
			COALESCE(bool_or(ic.relname = '`+bm25IndexV2Name+`'
				AND am.amname = 'bm25'
				AND pg_get_indexdef(i.indexrelid) LIKE '`+bm25IndexLikeV2+`'
				AND EXISTS (SELECT 1 FROM unnest(ic.reloptions) o WHERE o = 'key_field=id')), false),
			COALESCE(bool_or(ic.relname = '`+bm25IndexV2Name+`'
				AND i.indisvalid AND i.indisready
				AND am.amname = 'bm25'
				AND pg_get_indexdef(i.indexrelid) LIKE '`+bm25IndexLikeV2+`'
				AND EXISTS (SELECT 1 FROM unnest(ic.reloptions) o WHERE o = 'key_field=id')), false)
		FROM pg_index i
		JOIN pg_class ic ON ic.oid = i.indexrelid
		JOIN pg_am am ON am.oid = ic.relam
		WHERE i.indrelid = 'observations'::regclass
		  AND ic.relname IN ('`+bm25IndexOldName+`', '`+bm25IndexV2Name+`')`).Scan(&st.old, &st.oldDef, &st.oldOK, &st.v2, &st.v2Def, &st.v2OK)
	if err != nil {
		return st, err
	}
	return st, nil
}

// pgSearchIndexUsable is the single validator for the BM25 setup, shared
// by the Open-time capability probe and the provisioning postcondition
// (round-2 finding N1). It is anchored to THE observations table the
// session resolves ('observations'::regclass — not a name match across
// schemas), requires a valid+ready index under the bm25 access method,
// one of the two exact accepted definitions, and the key_field=id
// reloption. It accepts EITHER definition so the API runs on the old
// index before the operator swap and on v2 after it (a20 = A); any
// error means "not usable".
func pgSearchIndexUsable(ctx context.Context, conn interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) bool {
	st, err := pgSearchIndexState(ctx, conn)
	if err != nil {
		return false
	}
	return st.oldOK || st.v2OK
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

// ProvisionPGSearchIndex provisions the BM25 index at the deployment
// step — called from pgshadow Apply and test provisioning, deliberately
// never from Open (round-1 finding 1: app-path DDL made the optional
// upgrade a blocking startup dependency). Clean no-op without the
// pg_search extension. Post-a20 (2026-11-01, the one-one-time exception
// to "migrations additive" granted for this index only):
//   - a FRESH table (no index, no rows) gets v2 directly;
//   - a table that already holds the old (id, text) index is NOT rebuilt
//     here — on a multi-million-row store that build is an operator step
//     (ReindexBM25Index), never an apply side-effect;
//   - a table with rows but no index at all, and an index under either
//     reserved name that is not its exact definition or is invalid,
//     fail LOUD instead of touching anything.
func ProvisionPGSearchIndex(ctx context.Context, conn *sql.DB) error {
	// Clean no-op without the extension (the pre-2026-07-22 contract): a
	// plain-Postgres deployment falls back to tsvector ranking and the
	// pdb.* DDL below would fail with "schema pdb does not exist".
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_extension WHERE extname = 'pg_search'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	st, err := pgSearchIndexState(ctx, conn)
	if err != nil {
		return err
	}
	switch {
	case st.v2OK:
		return nil
	case st.oldOK:
		return nil
	case st.old && !st.oldDef:
		return fmt.Errorf("bm25 index provisioning postcondition failed: %s exists but is not the expected pg_search index on observations(id, text) with key_field=id — inspect it before running reindex-bm25", bm25IndexOldName)
	case st.v2 && !st.v2Def:
		return fmt.Errorf("bm25 index provisioning postcondition failed: %s exists but is not the expected v2 definition — inspect it before running reindex-bm25", bm25IndexV2Name)
	case st.old || st.v2:
		// exact definition but invalid (a failed build): not rebuilt
		// implicitly — reindex-bm25 drops the leftover and rebuilds.
		which := bm25IndexOldName
		if st.v2 {
			which = bm25IndexV2Name
		}
		return fmt.Errorf("bm25 index provisioning postcondition failed: %s exists but is not valid+ready (a failed build) — run memoire-pgshadow reindex-bm25 to drop the leftover and rebuild", which)
	}
	var hasRows bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM observations)`).Scan(&hasRows); err != nil {
		return err
	}
	if hasRows {
		return fmt.Errorf("bm25 index provisioning postcondition failed: observations carries rows but has no bm25 index at all — provisioning does not build the index implicitly on a data-bearing table (a large build is an operator step); run memoire-pgshadow reindex-bm25")
	}
	if _, err := conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS `+bm25IndexV2Name+
		` ON observations USING bm25 `+pgSearchIndexV2Columns+
		` WITH (key_field='id')`); err != nil {
		return err
	}
	// Postcondition (round-2 finding N1): IF NOT EXISTS matches by NAME
	// only, so a pre-existing wrong relation under this name would make
	// provisioning "succeed" while search later errors. The same
	// validator the capability probe uses must pass here, or deployment
	// fails loudly.
	st2, err := pgSearchIndexState(ctx, conn)
	if err != nil {
		return err
	}
	if !st2.v2OK {
		return fmt.Errorf("bm25 index provisioning postcondition failed: %s was created but does not validate as the expected v2 pg_search index", bm25IndexV2Name)
	}
	return nil
}

// ReindexBM25Index is the explicit operator step that swaps the BM25
// index (a20 = A, 2026-11-01): it builds v2 CONCURRENTLY beside whatever
// is present, verifies it is valid+ready (dropping an invalid leftover
// and failing loudly otherwise), then DROPs the old index
// CONCURRENTLY. Both indexes coexist the whole build (disk note,
// RUNBOOK), and pg_search serves reads from EITHER definition, so the
// swap is online — no window of unsearchability, no restart (the
// capability is probed at Open and the planner picks the index per
// query; a running hub starts using v2 as soon as the old one is
// gone).
//
// Idempotence, by (v2, old) state:
//   - v2 valid, old absent: no-op;
//   - v2 valid, old present (an interrupted first run): resume — drop
//     the old only, no rebuild (the v2 index OID is untouched);
//   - old present, v2 absent: build v2, verify, drop old;
//   - no index at all: build v2, verify;
//   - v2 present but not valid+ready: drop the invalid leftover first
//     (a normal DROP INDEX — a failed CONCURRENTLY build leaves an
//     invalid index that must not be read), then the build path;
//   - either name held by a relation that is NOT the exact expected
//     definition: fail loudly and change nothing (an unknown object
//     under a reserved name is the operator's to inspect).
//
// The build is NOT in a transaction (CONCURRENTLY forbids it); the
// command still takes the schema-applier advisory lock (the same key
// openPostgres and pgshadow.Apply take) so it never races another
// DDL applier. A pinned session carries explicit lock_timeout and
// statement_timeout sized for a large concurrent build: lock_timeout
// bounds the wait for the final AccessExclusive pass while writers
// continue (fail loud, re-run); statement_timeout is a backstop that
// turns a wedged build into a loud error, not a hang.
func ReindexBM25Index(ctx context.Context, conn *sql.DB, dryRun bool) (string, error) {
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_extension WHERE extname = 'pg_search'`).Scan(&n); err != nil {
		return "", fmt.Errorf("reindex-bm25: read extension: %w", err)
	}
	if n == 0 {
		return "", fmt.Errorf("reindex-bm25: the pg_search extension is not installed in this database — there is no BM25 index to swap (CREATE EXTENSION pg_search is an operator step, then re-run)")
	}
	if dryRun {
		// Advisory snapshot: the dry run reads the catalog WITHOUT taking
		// the schema-applier lock (a dry run must never serialize against
		// a running apply) and says so in its output — the real run
		// re-reads state under the lock and may derive a different plan
		// (a20 = A, F2).
		st, err := pgSearchIndexState(ctx, conn)
		if err != nil {
			return "", fmt.Errorf("reindex-bm25: read index state: %w", err)
		}
		report, _, _, err := bm25ReindexPlan(st)
		if err != nil {
			return "", err
		}
		return report + "reindex-bm25: dry-run: nothing executed (advisory snapshot: taken WITHOUT the schema-applier lock; the real run re-reads state under the lock and may derive a different plan)\n", nil
	}
	return reindexBM25Execute(ctx, conn)
}

// bm25ReindexSteps is the plan derived from one catalog read.
type bm25ReindexSteps struct {
	dropInvalidV2 bool
	buildV2       bool
	dropOld       bool
}

// bm25ReindexPlan derives the swap steps from a catalog read and renders
// the operator report. It fails loud on a reserved name carrying an
// unrecognized definition (an operator-minted index this command must
// not touch). The end state (v2 valid, old absent) is the no-op. The
// plan is derived from whatever read it is handed: the real run hands
// it the state read UNDER the schema-applier lock, so the steps can
// never describe a state a concurrent run has already finished
// (a20 = A, F2); the dry run hands it the advisory snapshot.
func bm25ReindexPlan(st bm25IndexState) (string, bool, bm25ReindexSteps, error) {
	switch {
	case st.old && !st.oldDef:
		return "", false, bm25ReindexSteps{}, fmt.Errorf("reindex-bm25: %s exists but is not the expected (id, text) pg_search definition — it is not the index this command swaps; inspect and remove it first", bm25IndexOldName)
	case st.v2 && !st.v2Def:
		return "", false, bm25ReindexSteps{}, fmt.Errorf("reindex-bm25: %s exists but is not the expected v2 pg_search definition — it is not the index this command manages; inspect and remove it first", bm25IndexV2Name)
	}
	steps := bm25ReindexSteps{}
	switch {
	case st.v2OK && !st.old:
		// already at the end state
	default:
		steps.dropInvalidV2 = st.v2 && !st.v2OK
		steps.buildV2 = !st.v2OK
		steps.dropOld = st.old
	}
	noop := !steps.dropInvalidV2 && !steps.buildV2 && !steps.dropOld
	var b strings.Builder
	fmt.Fprintf(&b, "reindex-bm25: state: %s=%s %s=%s\n",
		bm25IndexOldName, stateLabel(st.old, st.oldDef, st.oldOK),
		bm25IndexV2Name, stateLabel(st.v2, st.v2Def, st.v2OK))
	if noop {
		fmt.Fprintln(&b, "reindex-bm25: no-op: v2 already valid, old absent")
		return b.String(), true, steps, nil
	}
	fmt.Fprintf(&b, "reindex-bm25: plan (lock_timeout=%s statement_timeout=%s, not in a transaction):\n",
		bm25ReindexLockTimeout, bm25ReindexStatementTimeout)
	step := 0
	if steps.dropInvalidV2 {
		step++
		fmt.Fprintf(&b, "reindex-bm25:   %d. DROP INDEX CONCURRENTLY %s  (invalid leftover from a failed build — concurrent, never a plain DROP: that would stall every reader behind an open writer)\n", step, bm25IndexV2Name)
	}
	if steps.buildV2 {
		step++
		fmt.Fprintf(&b, "reindex-bm25:   %d. CREATE INDEX CONCURRENTLY %s ON observations USING bm25 %s WITH (key_field='id')\n", step, bm25IndexV2Name, pgSearchIndexV2Columns)
	}
	if steps.buildV2 {
		step++
		fmt.Fprintf(&b, "reindex-bm25:   %d. verify %s is valid+ready (an invalid leftover is dropped and the run fails loud)\n", step, bm25IndexV2Name)
	}
	if steps.dropOld {
		step++
		fmt.Fprintf(&b, "reindex-bm25:   %d. DROP INDEX CONCURRENTLY %s\n", step, bm25IndexOldName)
	}
	return b.String(), false, steps, nil
}

func stateLabel(present, def, ok bool) string {
	switch {
	case !present:
		return "absent"
	case !def:
		return "present (UNRECOGNIZED definition)"
	case !ok:
		return "present (invalid)"
	default:
		return "present (valid)"
	}
}

// bm25ReindexLockTimeout bounds the wait for the final exclusive pass of
// CREATE INDEX CONCURRENTLY while writers continue; past it the run
// fails loud (SQLSTATE 55P03) and the operator re-runs — it never hangs.
// bm25ReindexStatementTimeout is the backstop for the build itself
// (measured 2.55 s per 1M synthetic rows on a 16-worker workstation,
// v2 ≈ 11% of the heap size; the multi-million-row hub with real raw
// text scales off the heap, but stays far below hours).
// Every drop in the swap path is CONCURRENTLY (a20 = A, F1): a plain
// DROP INDEX queues an AccessExclusiveLock on observations, and behind
// any open writer that pending exclusive request blocks EVERY new read
// (AccessShare requests queue behind it) for up to lock_timeout — a
// 30-minute hub-wide read stall for a bookkeeping statement.
var (
	bm25ReindexLockTimeout      = 30 * time.Minute
	bm25ReindexStatementTimeout = 6 * time.Hour
	bm25ReindexDDLCreate        = "CREATE INDEX CONCURRENTLY " + bm25IndexV2Name +
		" ON observations USING bm25 " + pgSearchIndexV2Columns + " WITH (key_field='id')"
	bm25ReindexDDLDropOld       = "DROP INDEX CONCURRENTLY " + bm25IndexOldName
	bm25ReindexDDLDropInvalidV2 = "DROP INDEX CONCURRENTLY " + bm25IndexV2Name
)

// reindexBM25Execute runs the swap on ONE pinned session (a20 = A, F2):
// the session takes the schema-applier advisory lock, then reads the
// catalog, derives the steps, sets the DDL timeouts, runs the DDL and
// verifies — ALL under the lock. The pre-lock plan was the race: a
// queued second invocation derived its steps from a state a concurrent
// run had already finished (v2 invalid, old present) and would have
// dropped the freshly valid v2 the first run had built. Re-deriving
// under the lock turns that second run into the no-op the operator
// expects. database/sql pools connections, so the whole sequence must
// ride the one pinned session (a plain ExecContext could put the lock
// and the DDL on different sessions); the session is discarded (not
// returned to the pool) because it carries the explicit timeouts. All
// DDL statements are autocommit — CONCURRENTLY must not run inside a
// transaction.
func reindexBM25Execute(ctx context.Context, conn *sql.DB) (string, error) {
	c, err := conn.Conn(ctx)
	if err != nil {
		return "", fmt.Errorf("reindex-bm25: pin session: %w", err)
	}
	release, err := AcquirePGMigrateLockOn(ctx, c)
	if err != nil {
		return "", err
	}
	defer release()

	st, err := pgSearchIndexState(ctx, c)
	if err != nil {
		return "", fmt.Errorf("reindex-bm25: read index state (under the lock): %w", err)
	}
	report, noop, steps, err := bm25ReindexPlan(st)
	if err != nil {
		return "", err
	}
	if noop {
		return report, nil
	}
	// The DDL bounds take over from the applier's 300s acquisition window:
	// lock_timeout bounds the wait for the concurrent DDL while writers
	// continue (a loud 55P03 past it, never a hang), statement_timeout is
	// the build backstop. They take no placeholders; integer = ms.
	if _, err := c.ExecContext(ctx, fmt.Sprintf("SET lock_timeout = %d", bm25ReindexLockTimeout.Milliseconds())); err != nil {
		return "", fmt.Errorf("reindex-bm25: set lock_timeout: %w", err)
	}
	if _, err := c.ExecContext(ctx, fmt.Sprintf("SET statement_timeout = %d", bm25ReindexStatementTimeout.Milliseconds())); err != nil {
		return "", fmt.Errorf("reindex-bm25: set statement_timeout: %w", err)
	}
	if steps.dropInvalidV2 {
		if _, err := c.ExecContext(ctx, bm25ReindexDDLDropInvalidV2); err != nil {
			return "", fmt.Errorf("reindex-bm25: drop invalid %s: %w", bm25IndexV2Name, err)
		}
	}
	if steps.buildV2 {
		if _, err := c.ExecContext(ctx, bm25ReindexDDLCreate); err != nil {
			// a failed CONCURRENTLY build can leave an invalid index
			// behind: drop the leftover (best effort) so the next run
			// starts from the state the operator expects, then fail loud.
			if st, stErr := pgSearchIndexState(context.Background(), c); stErr == nil && st.v2 && !st.v2OK {
				if _, dErr := c.ExecContext(context.Background(), bm25ReindexDDLDropInvalidV2); dErr != nil {
					return "", fmt.Errorf("reindex-bm25: build failed and the invalid leftover could not be dropped either (build: %v; drop: %v)", err, dErr)
				}
			}
			return "", fmt.Errorf("reindex-bm25: build failed (the invalid leftover, if any, was dropped; re-run): %w", err)
		}
		st, err := pgSearchIndexState(ctx, c)
		if err != nil {
			return "", fmt.Errorf("reindex-bm25: verify after build: %w", err)
		}
		if !st.v2OK {
			if st.v2 && !st.v2OK {
				if _, dErr := c.ExecContext(context.Background(), bm25ReindexDDLDropInvalidV2); dErr != nil {
					return "", fmt.Errorf("reindex-bm25: %s built but is not valid+ready, and dropping the invalid leftover failed (drop: %v)", bm25IndexV2Name, dErr)
				}
			}
			return "", fmt.Errorf("reindex-bm25: %s built but is not valid+ready (invalid leftover dropped; re-run)", bm25IndexV2Name)
		}
	}
	if steps.dropOld {
		if _, err := c.ExecContext(ctx, bm25ReindexDDLDropOld); err != nil {
			return "", fmt.Errorf("reindex-bm25: drop old index: %w", err)
		}
		st, err := pgSearchIndexState(ctx, c)
		if err != nil {
			return "", fmt.Errorf("reindex-bm25: verify after drop: %w", err)
		}
		if st.old {
			return "", fmt.Errorf("reindex-bm25: %s still present after DROP INDEX CONCURRENTLY", bm25IndexOldName)
		}
		if !st.v2OK {
			return "", fmt.Errorf("reindex-bm25: %s no longer validates after the swap", bm25IndexV2Name)
		}
	}
	return report, nil
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
		// D0249 warn-path (operator-ruled 2026-08-23): an observer must be
		// able to open a misconfigured store to observe it, so a contract
		// violation is LOUD but never blocking here — the refusal belongs to
		// the write authority (openPostgres) alone.
		if err := checkPGCollation(context.Background(), handle); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: observer open of out-of-contract store: %v\n", err)
		}
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
	s.seed.mu.Lock()
	s.seed.closed = true
	if s.seed.cancel != nil {
		s.seed.cancel()
	}
	s.seed.mu.Unlock()
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
	res, _, err := s.insertDerivedObservationGated(ctx, in, nil, nil)
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
	return s.insertDerivedObservationGated(ctx, in, matchedIDs, nil)
}

// DeriveSupersedeActor is the attributable system actor for a
// derivation-time supersede (Fibery #344), in the LapseTTLActor mould: the
// review-log row on the target names the mechanism, never a human.
const DeriveSupersedeActor = "system:derive-supersede"

// ObservationSupersedeOffer is the caller's PER-TARGET offer to supersede a
// gate-matched row instead of being rejected against it (Fibery #344). The
// gate classifies in Go; the store decides, because only the store can hold
// the target's state and the insert in one transaction.
//
// Metadata replaces the input's meta_json when THIS offer is the one that
// wins, so the persisted supersede block (target id and the three scores)
// describes the supersede that actually happened. A run whose offer loses the
// CAS therefore never carries a supersede block for a supersede it did not
// perform.
type ObservationSupersedeOffer struct {
	TargetID int64
	Metadata json.RawMessage
}

// DerivedObservationGateResult reports which of the gate's three outcomes the
// transaction took. Exactly one of the three shapes:
//
//   - LiveMatchID > 0: rejected. No row was written, and the rejection stands
//     on that row's in-transaction liveness (D212, unchanged).
//   - SupersededID > 0: Result is the new proposed row, carrying
//     supersedes_id, and the named target moved to superseded in the same
//     transaction with its own review-log and audit rows.
//   - both zero: an ordinary write.
type DerivedObservationGateResult struct {
	Result       DerivedObservationResult
	LiveMatchID  int64
	SupersededID int64
}

// InsertDerivedObservationSuperseding is the gated insert with the supersede
// outcome available (Fibery #344). With no offers it behaves EXACTLY like
// InsertDerivedObservationUnlessMatchedLive — same liveness check, same
// rejection, same write — which is what the -no-supersede kill switch relies
// on.
//
// When the row the transaction finds live carries an offer, the store attempts
// the supersede: a compare-and-set to `superseded` guarded on the target still
// being `proposed` AND live. The CAS predicate is narrower than the D208
// verdict triple by construction, not by omission: `proposed` with
// can_instruct=1 or provenance=user_confirmed is unreachable (elevation and
// confirm both require `accepted`), so state plus liveness IS the full
// snapshot for a proposed row. Zero rows affected means a concurrent writer
// moved the target between the gate's read and this UPDATE; the outcome then
// falls back to the ordinary gate decision re-taken against FRESH in-tx
// state — never a duplicate insert, never a supersede of a row nobody
// checked.
func (s *Store) InsertDerivedObservationSuperseding(ctx context.Context, in DerivedObservationInput, matchedIDs []int64, offers []ObservationSupersedeOffer) (DerivedObservationGateResult, error) {
	res, live, superseded, err := s.insertDerivedObservationSuperseding(ctx, in, matchedIDs, offers)
	return DerivedObservationGateResult{Result: res, LiveMatchID: live, SupersededID: superseded}, err
}

func (s *Store) insertDerivedObservationGated(ctx context.Context, in DerivedObservationInput, matchedIDs []int64, offers []ObservationSupersedeOffer) (DerivedObservationResult, int64, error) {
	res, live, _, err := s.insertDerivedObservationSuperseding(ctx, in, matchedIDs, offers)
	return res, live, err
}

func (s *Store) insertDerivedObservationSuperseding(ctx context.Context, in DerivedObservationInput, matchedIDs []int64, offers []ObservationSupersedeOffer) (DerivedObservationResult, int64, int64, error) {
	if err := in.Validate(); err != nil {
		return DerivedObservationResult{}, 0, 0, err
	}
	now := time.Now().Unix()
	if in.CreatedAtEpoch > 0 {
		now = in.CreatedAtEpoch
	}
	if in.OccurredAtEpoch == 0 {
		occurred, err := s.maxOccurredAtForEvents(ctx, in.SourceEventIDs)
		if err != nil {
			return DerivedObservationResult{}, 0, 0, err
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
		return DerivedObservationResult{}, 0, 0, err
	}
	// Store-level provenance validation (contract: Derived observation insert).
	if !allowedProvenance(in.Provenance) {
		return DerivedObservationResult{}, 0, 0, fmt.Errorf("invalid provenance %q: must be one of observed, inferred, user_confirmed, imported, generated", in.Provenance)
	}
	if !allowedDerivationProvenance(in.Provenance) {
		return DerivedObservationResult{}, 0, 0, fmt.Errorf("invalid derivation provenance %q: must be one of observed, inferred, generated", in.Provenance)
	}
	if err := validateUsePolicy(in.Provenance, in.CanInstruct); err != nil {
		return DerivedObservationResult{}, 0, 0, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DerivedObservationResult{}, 0, 0, err
	}
	defer tx.Rollback()

	// The gate's decision, in one transaction (Fibery #344 extends the D212
	// shape rather than replacing it): a live matched row still blocks the
	// insert, unless the caller offered to supersede that row AND the CAS
	// below finds it exactly as the gate read it.
	live, err := firstLiveObservation(ctx, tx, matchedIDs)
	if err != nil {
		return DerivedObservationResult{}, 0, 0, err
	}
	var supersedeTarget int64
	if live > 0 {
		offer, offered := supersedeOfferFor(offers, live)
		if !offered {
			return DerivedObservationResult{}, live, 0, nil
		}
		casHook("derive-supersede", live)
		casRes, err := tx.ExecContext(ctx, `UPDATE observations SET review_state = 'superseded'
			WHERE id = ? AND review_state = 'proposed' AND deleted_at_epoch IS NULL`, live)
		if err != nil {
			return DerivedObservationResult{}, 0, 0, err
		}
		affected, err := casRes.RowsAffected()
		if err != nil {
			return DerivedObservationResult{}, 0, 0, err
		}
		if affected == 0 {
			// The target moved between the gate's read and this UPDATE. Re-take
			// the ordinary decision against FRESH in-transaction state: if it
			// (or a lower-scored match) is still live, this candidate is
			// rejected exactly as it would have been today; if nothing is live
			// any more, the insert proceeds ungated. Either way no supersede
			// happened and no row was written twice.
			refreshed, rerr := firstLiveObservation(ctx, tx, matchedIDs)
			if rerr != nil {
				return DerivedObservationResult{}, 0, 0, rerr
			}
			if refreshed > 0 {
				return DerivedObservationResult{}, refreshed, 0, nil
			}
		} else {
			supersedeTarget = live
			if len(offer.Metadata) > 0 {
				meta = offer.Metadata
			}
		}
	}

	if err := ensureDerivationRunExists(ctx, tx, in.DerivationRunID); err != nil {
		return DerivedObservationResult{}, 0, 0, err
	}

	downgraded := false
	if reviewState == "accepted" {
		overlap, err := acceptedOverlapExists(ctx, tx, in.Project, in.Kind, in.ContentHash, in.SourceEventIDs)
		if err != nil {
			return DerivedObservationResult{}, 0, 0, err
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
		review_state, derivation_run_id, content_hash, provenance, can_instruct, can_evidence, needs_confirmation, noise_candidate,
		supersedes_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		nil, in.Project, nullString(in.SessionID), in.Kind, in.Text, nullString(in.SourceAdapter), nullString(in.EventType),
		in.OccurredAtEpoch, now, principal, confidence, string(meta), reviewState, in.DerivationRunID, in.ContentHash,
		nullString(in.Provenance), in.CanInstruct, in.CanEvidence, in.NeedsConfirmation, boolInt(in.NoiseCandidate),
		nullInt64(supersedeTarget)).Scan(&observationID)
	if err != nil {
		return DerivedObservationResult{}, 0, 0, err
	}
	// Source-event links are THIS run's own (Fibery #344): a later cut has
	// events of its own, unlike a rewrite, which copies the original's
	// because an operator statement has none.
	for _, eventID := range in.SourceEventIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO observation_source_events(observation_id, event_id, created_at_epoch)
			VALUES (?, ?, ?)`, observationID, eventID, now); err != nil {
			return DerivedObservationResult{}, 0, 0, err
		}
	}
	if supersedeTarget > 0 {
		// The target's transition is attributable and auditable in both
		// directions, mirroring RewriteObservation: a review-log row on the
		// target under the system actor, and an audit entry naming the
		// successor. proposed -> superseded stays OUT of
		// validateReviewTransition: like rewrite, this is its own verb with
		// its own precondition (the CAS above), and the public state machine
		// is not widened (memoire:D146).
		if _, err := tx.ExecContext(ctx, `INSERT INTO observation_review_log(
			observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch
		) VALUES (?, 'proposed', 'superseded', ?, ?, ?)`,
			supersedeTarget, DeriveSupersedeActor,
			fmt.Sprintf("superseded by derivation run %s", in.DerivationRunID), now); err != nil {
			return DerivedObservationResult{}, 0, 0, err
		}
		auth, _ := authctx.From(ctx)
		detailsJSON, err := json.Marshal(map[string]any{
			"superseded_by":     observationID,
			"derivation_run_id": in.DerivationRunID,
			"from_state":        "proposed",
		})
		if err != nil {
			return DerivedObservationResult{}, 0, 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(
			principal_id, credential_id, request_id, action, resource_type, resource_id, details_json, created_at_epoch
		) VALUES (?, ?, ?, 'observation.supersede', 'observation', ?, ?, ?)`,
			nullString(auth.PrincipalID), nullString(auth.CredentialID), nullString(auth.RequestID),
			fmt.Sprint(supersedeTarget), string(detailsJSON), now); err != nil {
			return DerivedObservationResult{}, 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return DerivedObservationResult{}, 0, 0, err
	}
	return DerivedObservationResult{
		ID:                     observationID,
		ReviewState:            reviewState,
		DowngradedFromAccepted: downgraded,
	}, 0, supersedeTarget, nil
}

// supersedeOfferFor finds the offer for a target id, if the caller made one.
func supersedeOfferFor(offers []ObservationSupersedeOffer, targetID int64) (ObservationSupersedeOffer, bool) {
	for _, o := range offers {
		if o.TargetID == targetID {
			return o, true
		}
	}
	return ObservationSupersedeOffer{}, false
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

// IsNoOpDuplicate reports whether ingesting in would write NOTHING: its
// idempotency key is already stored AND its session row exists (Ingest's
// duplicate path otherwise still inserts the session). The drainer skips
// the raw/ custody copy of such an item (operator ruling a65): replaying
// raw/ reproduces the store without it, first write wins.
func (s *Store) IsNoOpDuplicate(ctx context.Context, in EventInput) (bool, error) {
	if in.SourceAdapter == "" && in.ProducerID != "" {
		in.SourceAdapter = in.ProducerID
	}
	payload := in.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	key := in.IdempotencyKey(payload)
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM events WHERE idempotency_key = ?) +
		(SELECT count(*) FROM sessions WHERE session_id = ?)`, key, in.SessionID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n == 2, nil
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
	if in.EventType == EventTypeMemoryUsage {
		if err := s.projectMemoryUsageTx(ctx, tx, id, in, payload, occurred); err != nil {
			return IngestResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return IngestResult{}, err
	}
	s.noteWrite(now)
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
		"memoire identity conflict: key %q is arriving from %s (source %s, %d events) but is already used by %s — capture continues unblocked; create .memoire / set MEMOIRE_PROJECT there, then have the operator run the re-key procedure (see the shared manual ~/.agents/shared/memoire-rekey.md; the SQLite-era scripts/memoire-project-move.sh does not apply to the Postgres hub) to fold the rows",
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
	// DerivationRunID is the run that wrote the row, EMPTY when it has none —
	// an operator rewrite carries no run, and a row written outside the model
	// path may not either. The near-dup gate's supersede outcome needs it to
	// tell an EARLIER run's proposal from one this same run wrote moments ago
	// (Fibery #344 review round-1 F1): a stale-running takeover REUSES the run
	// id, so "not from this response" was not the same statement as "from an
	// earlier run".
	DerivationRunID string `json:"derivation_run_id,omitempty"`
}

// ListSessionObservationsForDerivation returns the session's existing
// observations, newest first, capped at limit (0 means no cap). Nearly all
// non-deleted review states are included — lapsed and rejected content is
// deliberately visible so re-derivation neither resurrects reviewed-away
// text nor re-proposes it (operator-ruled 2026-08-02). Soft-deleted rows
// stay invisible like everywhere else. Raw rows are excluded: they mirror
// event text verbatim (Ingest writes one per event), and a derivation's
// whole job is restating event content — comparing candidates against raw
// mirrors would reject every good derivation of a captured prompt.
//
// SUPERSEDED rows are excluded too (Fibery #344). A superseded row's content
// lives on in its successor by construction — an operator rewrite replaced it,
// or a later cut restated it with containment >= 0.80 — and the successor is
// in this same list. Listing both spends prompt bytes and gate comparisons on
// a stale link, and a candidate rejected against a superseded row would be
// rejected against text the store itself no longer surfaces.
func (s *Store) ListSessionObservationsForDerivation(ctx context.Context, project, sessionID string, limit int) ([]SessionObservationSummary, error) {
	if project == "" || sessionID == "" {
		return nil, errors.New("project and session_id are required")
	}
	// COALESCE, not a nullable scan: derivation_run_id is NULL for rows with no
	// run (rewrite successors), and the gate's same-run comparison must see an
	// empty string there rather than a scan error or a fabricated match.
	q := `SELECT id, kind, text, review_state, COALESCE(derivation_run_id, '') FROM observations
		WHERE project = ? AND session_id = ? AND deleted_at_epoch IS NULL
		  AND kind <> 'raw'
		  AND review_state <> 'superseded'
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
		if err := rows.Scan(&rec.ID, &rec.Kind, &rec.Text, &rec.ReviewState, &rec.DerivationRunID); err != nil {
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
		o.provenance, o.can_instruct, o.can_evidence, o.needs_confirmation, o.noise_candidate, o.supersedes_id
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
		o.provenance, o.can_instruct, o.can_evidence, o.needs_confirmation, o.noise_candidate, o.supersedes_id
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

// Review actions are the memoire:D125 flag-only path: they change FLAGS, not
// review_state, and are recorded as append-only entries through the `action`
// column on observation_review_log. Widening review_state with a pseudo-state
// per verb was rejected there; adding an action VALUE here is the extension
// point that ruling left open.
//
// The triage actions carry the operator's ENCOUNTER with a row whose state
// already says what they want it to say. A TTL-lapsed row the operator reads
// and agrees with cannot travel the lapse route — lapsed -> lapsed is not a
// legal transition and must not become one — so without an action the human
// ruling would be discarded, the suggestion would stay pending forever, and
// the row would re-pin the queue on every load.
const (
	ReviewActionEvidenceOnly  = "evidence-only"
	ReviewActionRestrictScope = "restrict-scope"
	ReviewActionMarkStale     = "mark-stale"
	// ReviewActionMarkNoise flags noise the judge missed. The operator's
	// genre slug rides reason_text, prefixed, so the manual channel persists
	// in an existing audited field.
	ReviewActionMarkNoise = "mark-noise"
	// ReviewActionAffirmLapse is "I read this lapsed row and it stays
	// lapsed" — no flag changes at all, purely the record that a human
	// encountered it.
	ReviewActionAffirmLapse = "triage-affirm-lapse"
	// ReviewActionDeclineElevation is the stage-2 decline: the operator
	// read the confirm/elevate suggestion and is not taking it. Without it
	// a declined suggestion could never leave "pending", and a verdict
	// class that can never resolve is a metric that cannot be read.
	ReviewActionDeclineElevation = "triage-decline-elevation"
	// ReviewActionAffirmCurrent is "I read this and it already says what
	// the judge suggested" — a row already confirmed or already elevated.
	// The apply is a no-op on the row, but the ENCOUNTER is not: without a
	// record the suggestion sits pending forever and the row keeps coming
	// back, which is the same failure the lapsed encounter had.
	ReviewActionAffirmCurrent = "triage-affirm-current"
	// ReviewActionNotNoise is the de-noise pool's KEEP override: the
	// operator read a row the machine condemned as noise and is taking it
	// back out of the condemned batch. It does DOUBLE DUTY.
	//
	// (a) It marks the row's noise signal HANDLED, so the row leaves the
	// de-noise pool permanently and returns to the ordinary triage queue,
	// where the real accept/lapse question is still open. That is why it
	// deliberately does NOT count as a queue-handling ruling the way
	// mark-noise and triage-affirm-lapse do — a kept row has been rescued,
	// not ruled on.
	//
	// (b) It is the override signal for the NOISE verdict class. A keep is
	// exactly the flip a noise-lapse suggestion can suffer, and it is the
	// same flip the graduation falsifier reads: one row the operator kept is
	// one row a graduated genre would have destroyed unseen.
	//
	// It changes NO flag. `noise_candidate` is derivation's recorded opinion
	// and clearing it would rewrite what derivation said; the keep is
	// recorded as the append-only encounter it is, and the de-noise
	// predicate reads the action rather than the flag.
	ReviewActionNotNoise = "not-noise"
)

func reviewActions() []string {
	return []string{ReviewActionEvidenceOnly, ReviewActionRestrictScope, ReviewActionMarkStale,
		ReviewActionMarkNoise, ReviewActionAffirmLapse, ReviewActionDeclineElevation,
		ReviewActionAffirmCurrent, ReviewActionNotNoise}
}

// IsTriageReviewAction reports whether an action is one of the triage
// encounter actions — the ones that count as a human RULING on a suggestion,
// as opposed to the older governance flag edits, which say nothing about a
// suggested verdict.
func IsTriageReviewAction(action string) bool {
	return action == ReviewActionMarkNoise || action == ReviewActionAffirmLapse ||
		action == ReviewActionDeclineElevation || action == ReviewActionAffirmCurrent ||
		action == ReviewActionNotNoise
}

// ApplyObservationReviewAction applies a flag-only review action without
// changing review_state. It writes a review log entry recording the action.
func (s *Store) ApplyObservationReviewAction(ctx context.Context, id int64, action, reasonText string) (ObservationRecord, error) {
	if !inSlice(action, reviewActions()...) {
		return ObservationRecord{}, fmt.Errorf("review action must be one of %s, got %q",
			strings.Join(reviewActions(), ", "), action)
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
	case ReviewActionMarkNoise:
		updates = append(updates, "noise_candidate = 1")
	case ReviewActionAffirmLapse, ReviewActionDeclineElevation, ReviewActionAffirmCurrent,
		ReviewActionNotNoise:
		// No flag changes: the entry IS the record. See the guarded
		// snapshot re-read below, which stands in for the CAS these have
		// no columns to perform. `not-noise` in particular must NOT clear
		// `noise_candidate`: that flag is derivation's recorded opinion,
		// and the keep is the operator's answer to it, not an erasure of
		// it.
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
	} else {
		// A no-column action still must not log against a stale snapshot:
		// re-read the governance triple INSIDE the transaction and conflict
		// if it moved. This is the CAS these actions cannot express as an
		// UPDATE, written as the guard it is rather than skipped.
		fresh, ferr := getObservationForReview(ctx, tx, id)
		if ferr != nil {
			return ObservationRecord{}, ferr
		}
		if fresh.ReviewState != current.ReviewState || fresh.Provenance != current.Provenance ||
			fresh.CanInstruct != current.CanInstruct {
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

// ErrInvalidQuery marks a search query the store rejects by its own
// validation (empty after FTS sanitizing). The handler maps it to 400; any
// other search error is a store fault and maps to 500.
var ErrInvalidQuery = errors.New("invalid search query")

func (s *Store) Search(ctx context.Context, q, project string, limit int) ([]SearchHit, error) {
	return s.SearchWithFilter(ctx, q, ObservationFilter{Project: project, Limit: limit})
}

func (s *Store) SearchWithFilter(ctx context.Context, q string, filter ObservationFilter) ([]SearchHit, error) {
	match := SanitizeFTS(q)
	if match == "" {
		return nil, fmt.Errorf("%w: empty search query", ErrInvalidQuery)
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
// bm25SearchHeadPG / bm25SearchTailPG are the pg_search search SELECT of
// record (the searchFTS bm25 branch, single-query and the two-stage inner
// alike), split around the WHERE clause: <head> + where + <tail>. The
// EXPLAIN tests (a20 = A) wrap these exact strings, so the asserted plan
// is the plan of the production query, not a re-derivation of it.
const (
	bm25SearchHeadPG = `SELECT o.id, COALESCE(o.session_id, ''), o.project,
		o.kind, o.review_state, COALESCE(o.source_adapter, ''), COALESCE(o.event_type, ''),
		o.text, COALESCE(pdb.snippet(o.text, start_tag => '[', end_tag => ']'), ''),
		o.occurred_at_epoch, pdb.score(o.id)::float8, o.noise_candidate, o.can_instruct,
		COALESCE(o.provenance, ''), COALESCE(o.derivation_run_id, '')
		FROM observations o
		WHERE o.text ||| ? AND `
	bm25SearchTailPG = `
		ORDER BY pdb.score(o.id) DESC, o.id DESC
		LIMIT ?`
)

func (s *Store) searchFTS(ctx context.Context, match string, filter ObservationFilter, limit int) ([]SearchHit, error) {
	base, gate, filterArgs := observationWhereSplit(filter, "o")
	if s.engine == EnginePostgres && s.hasPGSearch && gate != "" {
		// The feed actor-gate is fail-closed and rides every shape (D257:
		// the predicate stays exclusion-shaped until a deliberate ruling
		// moves it — including on kind='raw, where a migrated corpus can
		// carry the pair the current API cannot write). But the NOT
		// EXISTS is the one non-pushable predicate of the v2 index: in
		// the inner read's WHERE it demotes the default shape to a
		// full-fetch scan plus anti-join (measured 489 ms unbounded
		// / 521 ms 30-day at 1M synthetic vs 25.5 / 26.9 for the
		// gate-free Top-K inner; docs/RUNBOOK.md BM25 swap section). So on the bm25
		// branch the gate moves to the outer of a two-stage read: the
		// inner fetches the top M by the full (score DESC, id DESC)
		// order — the Top-K scan, gate-free — and the outer applies the
		// same NOT EXISTS on the ≤ M rows, keeping the order and LIMIT k
		// (a20 = A, round-3 F4). The other branches keep the single
		// query: the tsvector twin hoists the anti-join clean (no
		// heap_filter either way), and the SQLite FTS lane's early-stop
		// rank order is not the property at risk.
		return s.searchBM25TwoStage(ctx, match, base, filterArgs, limit)
	}
	where := base
	if gate != "" {
		where = base + " AND " + gate
	}
	var query string
	var args []any
	if s.engine == EnginePostgres && s.hasPGSearch {
		// Real BM25 via pg_search (gate ruling option A, 2026-07-22): the
		// ||| match-disjunction operator tokenizes the input with the
		// FIELD's tokenizer (alignment by construction) and ORs the terms —
		// the same candidate semantics as SanitizeFTS + FTS5, with bm25
		// scoring (IDF included, the thing tsvector ranking lacks).
		// pdb.snippet carries the [/]-bracket shape of FTS5 snippet().
		// Gate-free shape only: the gated default read takes the two-stage
		// return above.
		args = append(args, ftsToMatchText(match))
		args = append(args, filterArgs...)
		args = append(args, limit)
		query = bm25SearchHeadPG + where + bm25SearchTailPG
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
		COALESCE(o.provenance, ''), COALESCE(o.derivation_run_id, '')
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
		COALESCE(o.provenance, ''), COALESCE(o.derivation_run_id, '')
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
	return scanFTSHits(rows)
}

// scanFTSHits maps the 15-column search projection (the shared shape of
// searchFTS's three lanes and the two-stage bm25 read) into SearchHit rows.
func scanFTSHits(rows *sql.Rows) ([]SearchHit, error) {
	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.ID, &h.SessionID, &h.Project, &h.Kind, &h.ReviewState, &h.SourceAdapter, &h.EventType, &h.Text, &h.Snippet, &h.OccurredAtEpoch, &h.Rank, &h.NoiseCandidate, &h.CanInstruct, &h.Provenance, &h.DerivationRunID); err != nil {
			return nil, err
		}
		h.syncOccurredAt()
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// bm25TwoStageWindowCap bounds the two-stage read's inner window: past it
// the exclusion density is pathological (almost the whole candidate top is
// agent-accepted) and the read falls back to the single-query form — exact,
// the pre-a20 plan. 100k rows keeps that fallback in the seconds, not the
// minutes, on the hub, while still out-reaching any realistic feed
// (k ≤ 200). Package var (not const) so the test can shrink it to
// exercise the clamp/fallback branch (round-4 F7); production never
// rewrites it.
var bm25TwoStageWindowCap = 100000

// bm25TwoStageQuerySQL is the two-stage read of record (a20 = A, round-3
// F4; one-statement form since round-4 F6). One SQL statement returns the
// gated hits AND the inner-window occupancy from the SAME snapshot: the
// window is a CTE (`w`) referenced twice — the gated branch and the
// occupancy count — so the planner materializes it ONCE and both read the
// same buffer. The round-3 form took the occupancy from a separate
// statement (a later snapshot): a concurrent delete/purge between the two
// could certify an underfilled window the read had seen full, and the
// read would return short. The occupancy rides a sentinel row (the only
// NULL-id row: a UNION ALL member fed by the one-row occupancy CTE), so
// it is reported even when zero rows survive the gate — and it sorts last
// (NULLS LAST) and is cut by the LIMIT exactly when k real rows survive
// (the one case where it is not needed). The explicit column list names
// the window's positional projection; the outer's score is the inner's
// float8 — the exact image of the inner's order key (this engine's
// pdb.score is float4, and float4 → float8 is exact) — so the order is
// the total order and the read returns exactly the top-k eligible rows
// of the single-query form.
func bm25TwoStageQuerySQL(base string) string {
	return `WITH w AS (` + bm25SearchHeadPG + base + bm25SearchTailPG + `),
	wn AS (SELECT count(*) AS n FROM w)
	SELECT g.id, g.session_id, g.project, g.kind, g.review_state,
		g.source_adapter, g.event_type, g.text, g.snippet, g.occurred_at_epoch,
		g.score, g.noise_candidate, g.can_instruct, g.provenance,
		g.derivation_run_id, g.occ
	FROM (
		SELECT r.id, r.session_id, r.project, r.kind, r.review_state,
			r.source_adapter, r.event_type, r.text, r.snippet,
			r.occurred_at_epoch, r.score, r.noise_candidate, r.can_instruct,
			r.provenance, r.derivation_run_id, NULL::bigint AS occ
		FROM w r(id, session_id, project, kind, review_state,
			source_adapter, event_type, text, snippet, occurred_at_epoch,
			score, noise_candidate, can_instruct, provenance, derivation_run_id)
		WHERE ` + agentAcceptedExclusion("r") + `
		UNION ALL
		SELECT NULL, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text,
			NULL::text, NULL::text, NULL::text, NULL::bigint, NULL::float8,
			NULL::int, NULL::int, NULL::text, NULL::text, wn.n
		FROM wn
	) g
	ORDER BY g.score DESC NULLS LAST, g.id DESC NULLS LAST
	LIMIT ?`
}

// bm25TwoStageAfterWindow is a test-only seam (round-4 F6 regression):
// called after each window's single-statement read and before the
// underfill decision. Nil in production. The test rewrites it to inject a
// mutation between the read and the decision: a design that re-introduced
// the separate occupancy probe would see the mutation (its probe is a
// later snapshot) and certify an underfilled window; the one-statement
// form cannot, because the decision uses the occupancy captured in the
// statement's own snapshot.
var bm25TwoStageAfterWindow func(ctx context.Context, s *Store, m int, n int)

// searchBM25TwoStage is the PG bm25 read with the feed actor-gate in the
// outer query (a20 = A, round-3 F4; D257's fail-closed predicate). It is
// exact by construction: in the (score DESC, id DESC) total order,
// whenever k rows survive the gate inside the window they are exactly the
// top-k eligible — the k-th eligible sits at or inside the window iff the
// window holds it, and everything above it in the order is in the window
// with it — and a window that does not fill IS the whole candidate set.
// The window starts at max(4k, 100) and grows x4 — clamped to
// bm25TwoStageWindowCap, so no window larger than the cap is ever
// executed (round-4 F7) — while the top keeps underfilling; at the cap
// the read falls back to the single-query form (the gate in the inner's
// WHERE — the pre-a20 shape).
func (s *Store) searchBM25TwoStage(ctx context.Context, match, base string, filterArgs []any, limit int) ([]SearchHit, error) {
	m := 4 * limit
	if m < 100 {
		m = 100
	}
	for {
		hits, n, err := s.bm25TwoStageRead(ctx, match, base, filterArgs, m, limit)
		if err != nil {
			return nil, err
		}
		if bm25TwoStageAfterWindow != nil {
			bm25TwoStageAfterWindow(ctx, s, m, n)
		}
		if len(hits) >= limit {
			return hits, nil
		}
		// Fewer than k survivors: the sentinel (present exactly in this
		// case — see the query) carries the window's occupancy from THIS
		// statement: the window CTE is materialized once and the count and
		// the gate read the same buffer, so a concurrent delete cannot
		// certify an underfilled window the read saw full (round-4 F6:
		// the round-3 two statements took two snapshots).
		if n < 0 {
			return nil, errors.New("two-stage bm25 read: underfilled window without its occupancy sentinel")
		}
		if n < m {
			// Not full: the window held every candidate in this snapshot,
			// so the gate ran over all of them — the result is the
			// complete eligible set (or its top k), in the total order.
			return hits, nil
		}
		if m >= bm25TwoStageWindowCap {
			// Pathological exclusion density: fall back to the single
			// query — exact, the pre-a20 plan.
			return s.bm25GateedRead(ctx, match, base, filterArgs, limit)
		}
		next := 4 * m
		if next > bm25TwoStageWindowCap {
			next = bm25TwoStageWindowCap
		}
		m = next
	}
}

// bm25TwoStageRead runs one window of the two-stage read: one statement
// returns the gated hits (≤ k) and the window's occupancy (the sentinel
// row; -1 when the sentinel was cut by the LIMIT — which only happens
// when k real rows survived, i.e. the occupancy is not needed).
func (s *Store) bm25TwoStageRead(ctx context.Context, match, base string, filterArgs []any, m, k int) ([]SearchHit, int, error) {
	args := make([]any, 0, len(filterArgs)+3)
	args = append(args, ftsToMatchText(match))
	args = append(args, filterArgs...)
	args = append(args, m, k)
	rows, err := s.rdb.QueryContext(ctx, bm25TwoStageQuerySQL(base), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var hits []SearchHit
	occupancy := -1
	for rows.Next() {
		// Pointer targets: the sentinel row (the only NULL-id row) is
		// NULL in every data column, and a NULL in a REAL row would be
		// a schema violation (the head COALESCEs the nullable columns),
		// so both are checked after the scan.
		var id, occ *int64
		var sessionID, project, kind, reviewState *string
		var sourceAdapter, eventType, text, snippet *string
		var occurred *int64
		var rank *float64
		var noise, instruct *int
		var provenance, runID *string
		if err := rows.Scan(&id, &sessionID, &project, &kind, &reviewState, &sourceAdapter, &eventType,
			&text, &snippet, &occurred, &rank, &noise, &instruct, &provenance, &runID, &occ); err != nil {
			return nil, 0, err
		}
		if id == nil {
			// The occupancy sentinel: the only NULL-id row.
			if occ != nil {
				occupancy = int(*occ)
			}
			continue
		}
		if sessionID == nil || project == nil || kind == nil || reviewState == nil || sourceAdapter == nil ||
			eventType == nil || text == nil || snippet == nil || occurred == nil || rank == nil ||
			noise == nil || instruct == nil || provenance == nil || runID == nil {
			return nil, 0, fmt.Errorf("two-stage bm25 read: NULL in a real row (id %d)", *id)
		}
		h := SearchHit{
			ID: *id, SessionID: *sessionID, Project: *project, Kind: *kind,
			ReviewState: *reviewState, SourceAdapter: *sourceAdapter, EventType: *eventType,
			Text: *text, Snippet: *snippet, OccurredAtEpoch: *occurred, Rank: *rank,
			NoiseCandidate: *noise, CanInstruct: *instruct, Provenance: *provenance,
			DerivationRunID: *runID,
		}
		h.syncOccurredAt()
		hits = append(hits, h)
	}
	return hits, occupancy, rows.Err()
}

// bm25GateedRead is the single-query form — the gate in the inner read's
// WHERE (the pre-a20 plan: the full candidate fetch with the hash
// anti-join). Exact; the fallback of searchBM25TwoStage past the window
// cap.
func (s *Store) bm25GateedRead(ctx context.Context, match, base string, filterArgs []any, limit int) ([]SearchHit, error) {
	args := make([]any, 0, len(filterArgs)+2)
	args = append(args, ftsToMatchText(match))
	args = append(args, filterArgs...)
	args = append(args, limit)
	rows, err := s.rdb.QueryContext(ctx, bm25SearchHeadPG+base+" AND "+agentAcceptedExclusion("o")+bm25SearchTailPG, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFTSHits(rows)
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
		COALESCE(o.provenance, ''), COALESCE(o.derivation_run_id, '')
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
		if err := rows.Scan(&h.ID, &h.SessionID, &h.Project, &h.Kind, &h.ReviewState, &h.SourceAdapter, &h.EventType, &h.Snippet, &h.OccurredAtEpoch, &h.NoiseCandidate, &h.CanInstruct, &h.Provenance, &h.DerivationRunID); err != nil {
			return nil, err
		}
		h.syncOccurredAt()
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// newestRowReceiptQuery reads received_at_epoch of the newest event row through
// the primary key: an index lookup on both engines. It is a lower bound on the
// max receipt only, because ingest accepts caller-supplied receipt stamps
// (forwarded, retried and backfilled envelopes arrive out of order).
const newestRowReceiptQuery = `SELECT COALESCE((SELECT received_at_epoch FROM events ORDER BY id DESC LIMIT 1), 0)`

// maxReceiptQuery is the exact last write: a full scan of events (no index on
// received_at_epoch). Never on a probe path.
const maxReceiptQuery = `SELECT COALESCE(max(received_at_epoch), 0) FROM events`

// Stats is the exact store census behind GET /v1/stats. The counts are full
// scans of events and observations: never call it from a probe path.
func (s *Store) Stats(ctx context.Context, dbPath string) (Stats, error) {
	var st Stats
	queries := map[*int64]string{
		&st.EventCount:              `SELECT count(*) FROM events WHERE deleted_at_epoch IS NULL`,
		&st.ObservationCount:        `SELECT count(*) FROM observations WHERE deleted_at_epoch IS NULL`,
		&st.DeletedEventCount:       `SELECT count(*) FROM events WHERE deleted_at_epoch IS NOT NULL`,
		&st.DeletedObservationCount: `SELECT count(*) FROM observations WHERE deleted_at_epoch IS NOT NULL`,
		&st.LastWriteEpoch:          maxReceiptQuery,
	}
	st.LastWriteKnown = true
	for dest, q := range queries {
		if err := s.rdb.QueryRowContext(ctx, q).Scan(dest); err != nil {
			return Stats{}, err
		}
	}
	if err := s.sizeStats(ctx, dbPath, &st); err != nil {
		return Stats{}, err
	}
	return st, nil
}

// HealthStats fills only the fields GET /v1/health reports (LastWriteEpoch,
// DBBytes, WALBytes), with no synchronous table scan: the liveness probe
// calls /v1/health. LastWriteEpoch is the max receipt this process knows of,
// and LastWriteKnown says whether the exact seed has landed; before that the
// value is a lower bound. Each call starts the background seed if it is not
// done, not running and not waiting out a retry delay. On error the fields
// read so far are still returned.
func (s *Store) HealthStats(ctx context.Context, dbPath string) (Stats, error) {
	s.startLastWriteSeed()
	var st Stats
	var newest int64
	err := s.rdb.QueryRowContext(ctx, newestRowReceiptQuery).Scan(&newest)
	if err == nil {
		s.noteWrite(newest)
	}
	// done before lastWrite: the seed publishes its max, then done, so a true
	// done read first guarantees the next load includes the seeded max.
	st.LastWriteKnown = s.seed.done.Load()
	st.LastWriteEpoch = s.lastWrite.Load()
	if err != nil {
		return st, err
	}
	if err := s.sizeStats(ctx, dbPath, &st); err != nil {
		return st, err
	}
	return st, nil
}

const (
	// lastWriteSeedTimeout bounds one exact max scan behind HealthStats.
	lastWriteSeedTimeout = 5 * time.Minute
	// lastWriteSeedRetry is the delay before a failed seed is retried.
	lastWriteSeedRetry = time.Minute
)

func (s *Store) startLastWriteSeed() {
	if s.seed.done.Load() {
		return
	}
	s.seed.mu.Lock()
	defer s.seed.mu.Unlock()
	if s.seed.done.Load() || s.seed.running || s.seed.closed || time.Now().Before(s.seed.next) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lastWriteSeedTimeout)
	s.seed.running, s.seed.cancel = true, cancel
	go func() {
		defer cancel()
		var max int64
		err := s.rdb.QueryRowContext(ctx, maxReceiptQuery).Scan(&max)
		if err == nil {
			s.noteWrite(max)
		}
		s.seed.mu.Lock()
		defer s.seed.mu.Unlock()
		s.seed.running, s.seed.cancel = false, nil
		if err == nil {
			s.seed.done.Store(true)
		} else {
			s.seed.next = time.Now().Add(lastWriteSeedRetry)
		}
	}()
}

// noteWrite raises lastWrite to epoch; it never lowers it.
func (s *Store) noteWrite(epoch int64) {
	for {
		cur := s.lastWrite.Load()
		if epoch <= cur || s.lastWrite.CompareAndSwap(cur, epoch) {
			return
		}
	}
}

func (s *Store) sizeStats(ctx context.Context, dbPath string, st *Stats) error {
	if s.engine == EnginePostgres {
		// dbPath is a DSN here; size comes from the server instead of the
		// filesystem. WALBytes stays 0 — Postgres WAL is instance-wide and
		// CNPG-managed, not a per-store health number.
		return s.rdb.QueryRowContext(ctx,
			`SELECT pg_database_size(current_database())`).Scan(&st.DBBytes)
	}
	if info, err := os.Stat(dbPath); err == nil {
		st.DBBytes = info.Size()
	}
	if info, err := os.Stat(dbPath + "-wal"); err == nil {
		st.WALBytes = info.Size()
	}
	return nil
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

// DeriveRunOutcome is one recent derivation-run row reduced to what failure
// classification needs: the ledger status and the run metadata that carries
// the failure detail. Neither field ever reaches an unauthenticated surface —
// the caller folds them into a compiled-in vocabulary first (D224).
type DeriveRunOutcome struct {
	Status   string
	Metadata json.RawMessage
}

// RecentDeriveRunOutcomes returns the newest `limit` derivation runs in
// ledger order (newest first), computed from derivation_runs at read time —
// no summary table (memoire:D165/D245: a cache that can lie). Ordering is
// the deterministic supersede ordering, not the coarser listing one: a sweep
// mints many runs inside one second, and a consecutive-failure streak read
// off an arbitrary within-second permutation would be noise.
//
// Cost note: a failed or partial run retains its raw provider response in
// meta_json (memoire:D44), so a window of a few hundred rows can carry
// megabytes. That is why this is a TTL-cached, owner-budgeted /metrics
// query and never a per-probe one; capping the window is the only bound.
func (s *Store) RecentDeriveRunOutcomes(ctx context.Context, limit int) ([]DeriveRunOutcome, error) {
	limit = normalizeLimit(limit, 200, 1000)
	rows, err := s.rdb.QueryContext(ctx,
		`SELECT status, meta_json FROM derivation_runs
		 ORDER BY created_at_epoch DESC, COALESCE(created_at_ns, 0) DESC, id DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeriveRunOutcome
	for rows.Next() {
		var status string
		var meta sql.NullString
		if err := rows.Scan(&status, &meta); err != nil {
			return nil, err
		}
		rec := DeriveRunOutcome{Status: status}
		if meta.Valid {
			rec.Metadata = json.RawMessage(meta.String)
		}
		out = append(out, rec)
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
	var eventID, deletedAt, supersedesID sql.NullInt64
	var sessionID, sourceAdapter, eventType, principal, derivationRunID, contentHash, provenance sql.NullString
	var confidence sql.NullFloat64
	var canInstruct, canEvidence, needsConfirmation, noiseCandidate int
	err := scanner.Scan(
		&rec.ID, &eventID, &rec.Project, &sessionID, &rec.Kind, &rec.Text,
		&sourceAdapter, &eventType, &rec.OccurredAtEpoch, &rec.CreatedAtEpoch, &deletedAt,
		&principal, &confidence, &rec.ReviewState, &derivationRunID, &contentHash,
		&provenance, &canInstruct, &canEvidence, &needsConfirmation, &noiseCandidate,
		&supersedesID,
	)
	if err != nil {
		return ObservationRecord{}, err
	}
	if supersedesID.Valid {
		id := supersedesID.Int64
		rec.SupersedesID = &id
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
		o.provenance, o.can_instruct, o.can_evidence, o.needs_confirmation, o.noise_candidate, o.supersedes_id
		FROM observations o
		WHERE o.id = ?`, id)
	return scanObservationRecord(row)
}

// observationWhereSplit is observationWhere's clause builder split around
// the feed actor-gate: it returns the pushable base (every clause except
// the gate) and the gate itself ("" when the caller opted in or the
// shape's band cannot carry accepted rows). observationWhere is their
// byte-identical join — the gate is appended last in the builder's
// clause order — so no other caller's SQL text changes. The PG bm25
// search uses the split form to keep the gate OUT of the inner read
// (a20 = A, round-3 F4).
func observationWhereSplit(filter ObservationFilter, alias string) (string, string, []any) {
	var clauses []string
	var args []any
	if filter.Project != "" {
		clauses = append(clauses, alias+`.project = ?`)
		args = append(args, filter.Project)
	}
	if filter.SessionID != "" {
		// Bare equality, not COALESCE(...,'') = ?: the parameter is
		// non-empty by the branch guard, and for a non-empty value the
		// two forms are semantically identical on BOTH engines (a NULL or
		// an empty session_id fails the test either way) — so the
		// coalesce buys nothing and costs the pushdown: pg_search can
		// only push a predicate that matches an indexed expression, and
		// (session_id::pdb.literal) is one while
		// COALESCE(session_id,'') is not (a20 = A: the v2 index carries
		// the session column for exactly this clause).
		clauses = append(clauses, alias+`.session_id = ?`)
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
	if filter.OccurredAfterEpoch > 0 {
		clauses = append(clauses, alias+`.occurred_at_epoch >= ?`)
		args = append(args, filter.OccurredAfterEpoch)
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
	// Feed actor-gate (triage lane v1, R-D). Exclusion-shaped and
	// fail-closed: a row is feed-INELIGIBLE iff its accept transition was
	// rendered by an `agent:*` actor. Rules-born accepted rows carry NO
	// review-log accept entry at all and human accepts carry a credential
	// actor, so both keep eligibility — the clause is a no-op over today's
	// corpus and the gate exists so a future machine verdict is
	// structurally excluded from the feed until a deliberate ruling moves
	// the predicate. It rides wherever `accepted` can be in the result set,
	// which is the band the liaison feed reads through
	// (`GET /v1/observations?review_state=accepted`) as well as the default
	// injection band.
	if !filter.IncludeAgentAccepted && reviewStateBandIncludesAccepted(filter) {
		return strings.Join(clauses, " AND "), agentAcceptedExclusion(alias), args
	}
	return strings.Join(clauses, " AND "), "", args
}

// observationWhere is observationWhereSplit combined: base + the gate,
// byte-identical to the pre-split clause join (the gate is the builder's
// last clause), so every caller of this function sees unchanged SQL.
func observationWhere(filter ObservationFilter, alias string) (string, []any) {
	base, gate, args := observationWhereSplit(filter, alias)
	if gate != "" {
		return base + " AND " + gate, args
	}
	return base, args
}

// reviewStateBandIncludesAccepted reports whether the resolved review-state
// band can contain accepted rows. An explicitly-requested non-accepted state
// (the proposed queue, the lapsed pool) never needs the anti-join, which
// keeps the 0008 partial-index plans untouched.
func reviewStateBandIncludesAccepted(filter ObservationFilter) bool {
	if filter.ReviewState != "" {
		return filter.ReviewState == "accepted"
	}
	return true
}

// agentAcceptedExclusion is the feed predicate of record (R-D). It is an
// anti-join rather than a stored flag: the review log is the source of the
// fact, and a column mirroring it would be a cache that can lie
// (memoire:D245). `LIKE 'agent:%'` reads identically on both engines.
func agentAcceptedExclusion(alias string) string {
	return `NOT EXISTS (SELECT 1 FROM observation_review_log rl
		WHERE rl.observation_id = ` + alias + `.id
		  AND rl.to_state = 'accepted'
		  AND rl.actor LIKE 'agent:%')`
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
