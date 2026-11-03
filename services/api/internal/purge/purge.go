// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Package purge implements liaison:D97 decision 4 (hard purge) and the
// compaction lever of decision 3, scoped by memoire:D147 slice 1. It is the
// operator-facing destruction path: hard-DELETE from SQLite (soft-delete
// visibility is ignored — purge destroys regardless), matching raw/ custody
// files and queue residue removed after an age-encrypted quarantine archive
// is written, then WAL truncate + VACUUM so the freed pages actually leave
// the file. The custody generation in MANIFEST.json is bumped on every
// apply that deleted anything, so mirrors (docs/MIRROR-PROTOCOL.md) can
// detect that their copy predates a purge.
//
// Apply ordering and crash safety (review finding, round 1): the intent
// journal is written BEFORE anything destructive with phase "pre-commit",
// and flipped to "committed" only after the DB transaction commits (the
// quarantine archive is verified in between). A leftover intent makes the
// next Run refuse; ResumeIntent reconciles the phase against the database —
// a "pre-commit" intent whose probe rows still exist means the transaction
// rolled back, so nothing may be deleted. File deletions are all-or-error:
// any failure keeps the intent in place and fails the run before the
// generation bump.
//
// Selector semantics (exactly one per run):
//   - Project: everything captured under a project key — events,
//     observations, sessions, derivation_runs, project_roots and
//     project_identity_alerts rows (identity roots are captured path data),
//     raw/queue files whose envelope carries the project.
//   - EventIDs: the events, their raw-projection observations, every
//     derived observation linked to any of them via
//     observation_source_events (conservative over-delete: an observation
//     with one purged source among several is purged — its text derives
//     from the purged content), and the matching raw/queue files.
//     derivation_runs and sessions rows stay (idempotency watermarks).
//   - ObservationIDs: DB-only — the observations, their review log and
//     source links. Source events and raw files survive; purging a derived
//     row does not purge its inputs.
//
// File matching uses the drainer's own envelope normalization
// (drainer.NormalizeEnvelope) so files whose on-disk envelope omitted
// source_event_id (direct HTTP producers) still match the derived identity
// the database row carries. All four file surfaces are scanned: raw/,
// queue/pending, queue/inflight (crashed-drainer residue would otherwise be
// re-ingested at next API start), queue/failed (+ sidecars).
//
// Packed raw custody (cold archive, internal/rawstore): raw/ envelopes may
// live in immutable day parts (rawstore.PartName). Matching walks flat and
// packed envelopes alike; a matched member is quarantined under
// raw-pack/<part>/<name> and removed by rewriting its part without it
// (verified write + atomic rename, idempotent on resume; a part left empty
// is removed). The intent journal lists packed matches per part; deletion
// also locates every journaled name wherever it now lives, so a file moved
// between a crash and the resume is still destroyed. Apply and resume
// first discard stale part temps and drainer staging copies (they may hold
// the purged bytes). Run and ResumeIntent hold the raw/ custody lock, and
// the packer refuses while an intent journal exists.
//
// Not touched, deliberately: audit_log (tombstone history; details_json was
// already covered by memoire:D145 redaction), and — for id selectors —
// derivation_runs. Physical-media caveat: VACUUM rewrites the database
// file, but SSD wear-leveling means old blocks are not scrubbed; purge is
// application-level destruction, same posture as the D144 sweep.
//
// Two engines (memoire:D157 seam, Fibery #616 slice B): Options.DBPath is a
// SQLite file path or a postgres:// DSN (db.IsPostgresDSN). Selectors,
// gates, intent journal, tombstone, generation bump and file custody are
// shared; the SQLite statements are byte-identical to the single-engine
// tool. Postgres differences:
//   - placeholders rebound to $n (db.Rebind); temp selector tables use
//     BIGINT ids and ON CONFLICT DO NOTHING; one pooled connection so the
//     temp tables and the transaction share a session.
//   - schema-pg has no FTS table (text_tsv is a generated column) and no
//     foreign keys; the runtime-provisioned observation_embeddings_pg
//     cascades from observations, and is deleted explicitly first so the
//     report counts it. observation_usage and observation_triage_suggestions
//     rows of purged observations/events (and, for a project selector, the
//     project's usage rows) are deleted on BOTH engines since #616 slice E
//     (SQLite has the same tables without ON DELETE CASCADE); the project
//     probe counts the usage rows on both engines too.
//   - no WAL truncate / VACUUM: the database is a server, not a file we
//     rewrite; Vacuum is reported skipped (vacuum_skipped_reason), never an
//     error, and the tool never runs VACUUM FULL. Dead tuples persist until
//     autovacuum, and in WAL archives/base backups until retention expiry.
//   - the store is never migrated or opened through db.Open: a purge must
//     not apply DDL to the hub.
package purge

import (
	"archive/tar"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	memdb "memoire/services/api/internal/db"
	"memoire/services/api/internal/drainer"
	"memoire/services/api/internal/rawstore"
)

type Selector struct {
	Project        string
	EventIDs       []int64
	ObservationIDs []int64
}

func (s Selector) kind() string {
	switch {
	case s.Project != "":
		return "project"
	case len(s.EventIDs) > 0:
		return "event_ids"
	case len(s.ObservationIDs) > 0:
		return "observation_ids"
	}
	return ""
}

type Options struct {
	DBPath      string
	RawDir      string
	PendingDir  string
	InflightDir string
	FailedDir   string

	Selector Selector

	Apply         bool
	NoQuarantine  bool
	AgeRecipient  string
	QuarantineOut string
	Vacuum        bool

	// IntentPath is the crash-safety journal. See the package comment for
	// the phase protocol.
	IntentPath string

	ManifestPath string
}

type FileMatches struct {
	Raw []string `json:"raw"`
	// RawPacked lists matched envelopes held in day packs: pack name ->
	// member names.
	RawPacked map[string][]string `json:"raw_packed,omitempty"`
	Pending   []string            `json:"pending"`
	Inflight  []string            `json:"inflight"`
	Failed    []string            `json:"failed"`
}

func (f FileMatches) rawCount() int {
	n := len(f.Raw)
	for _, members := range f.RawPacked {
		n += len(members)
	}
	return n
}

func (f FileMatches) total() int {
	return f.rawCount() + len(f.Pending) + len(f.Inflight) + len(f.Failed)
}

type Report struct {
	DryRun   bool   `json:"dry_run"`
	Selector string `json:"selector"`
	Engine   string `json:"engine"`

	EventsMatched       int64 `json:"events_matched"`
	ObservationsMatched int64 `json:"observations_matched"`

	EventsDeleted          int64 `json:"events_deleted"`
	ObservationsDeleted    int64 `json:"observations_deleted"`
	SourceLinksDeleted     int64 `json:"source_links_deleted"`
	ReviewLogDeleted       int64 `json:"review_log_deleted"`
	SupersedesNulled       int64 `json:"supersedes_nulled"`
	SessionsDeleted        int64 `json:"sessions_deleted"`
	DerivationRunsDeleted  int64 `json:"derivation_runs_deleted"`
	IdentityRootsDeleted   int64 `json:"identity_roots_deleted"`
	IdentityAlertsDeleted  int64 `json:"identity_alerts_deleted"`
	DeriveIntentsDeleted   int64 `json:"derive_intents_deleted"`
	RunManifestRowsDeleted int64 `json:"run_manifest_rows_deleted"`
	// Derived children, cascaded on BOTH engines since #616 slice E (triage
	// suggestions, usage rows by observation/event/project, and the
	// embeddings row where the store has an embeddings table — Postgres
	// today; SQLite reports 0 while schema/0002 stays unapplied).
	UsageRowsDeleted         int64 `json:"usage_rows_deleted,omitempty"`
	TriageSuggestionsDeleted int64 `json:"triage_suggestions_deleted,omitempty"`
	EmbeddingsDeleted        int64 `json:"embeddings_deleted,omitempty"`

	RawFilesMatched int `json:"raw_files_matched"`
	RawFilesDeleted int `json:"raw_files_deleted"`
	// RawPacksRewritten counts day packs rewritten (or removed when
	// emptied) to drop purged members; RawFiles* count members too.
	RawPacksRewritten    int `json:"raw_packs_rewritten,omitempty"`
	PendingFilesMatched  int `json:"pending_files_matched"`
	PendingFilesDeleted  int `json:"pending_files_deleted"`
	InflightFilesMatched int `json:"inflight_files_matched"`
	InflightFilesDeleted int `json:"inflight_files_deleted"`
	FailedFilesMatched   int `json:"failed_files_matched"`
	FailedFilesDeleted   int `json:"failed_files_deleted"`

	QuarantineOut     string `json:"quarantine_out,omitempty"`
	QuarantineMembers int    `json:"quarantine_members"`
	QuarantineBytes   int64  `json:"quarantine_bytes"`

	DBBytesBefore int64 `json:"db_bytes_before"`
	DBBytesAfter  int64 `json:"db_bytes_after"`
	Vacuumed      bool  `json:"vacuumed"`
	// VacuumSkippedReason is set when Vacuum was requested but the engine
	// has no compaction lever the tool controls (Postgres).
	VacuumSkippedReason string `json:"vacuum_skipped_reason,omitempty"`

	Generation int64 `json:"generation"`
}

const (
	phasePreCommit = "pre-commit"
	phaseCommitted = "committed"
	probeLimit     = 10
)

type intentJournal struct {
	CreatedAtEpoch int64       `json:"created_at_epoch"`
	Phase          string      `json:"phase"`
	Selector       string      `json:"selector"`
	Files          FileMatches `json:"files"`
	// Probe ids reconcile a "pre-commit" intent against the database: the
	// transaction is atomic, so any surviving probe row proves rollback.
	// With NO probes recorded, commit cannot be proven and resume must
	// refuse deletion (round-2 finding N1).
	ProbeEventIDs       []int64 `json:"probe_event_ids,omitempty"`
	ProbeObservationIDs []int64 `json:"probe_observation_ids,omitempty"`
	// Project selectors also mutate rows outside the event/observation
	// sets (sessions, derivation_runs, identity tables). ProbeProjectRows
	// records their combined count at intent time so a lost flip after a
	// residue-only commit is still provable on resume (round-3 finding
	// R3-1): count gone to zero = committed, count surviving = rolled back.
	ProbeProject     string `json:"probe_project,omitempty"`
	ProbeProjectRows int64  `json:"probe_project_rows,omitempty"`
	// DBDeleted records (at the committed flip) whether the transaction
	// mutated any row, so a resumed DB-only purge still bumps the custody
	// generation (round-2 finding N2).
	DBDeleted bool `json:"db_deleted,omitempty"`
}

type manifest struct {
	Generation     int64       `json:"generation"`
	UpdatedAtEpoch int64       `json:"updated_at_epoch"`
	LastPurge      *purgeStamp `json:"last_purge,omitempty"`
}

type purgeStamp struct {
	AtEpoch             int64  `json:"at_epoch"`
	Selector            string `json:"selector"`
	EventsDeleted       int64  `json:"events_deleted"`
	ObservationsDeleted int64  `json:"observations_deleted"`
	FilesDeleted        int    `json:"files_deleted"`
}

func ValidateOptions(opts Options) error {
	kinds := 0
	if opts.Selector.Project != "" {
		kinds++
	}
	if len(opts.Selector.EventIDs) > 0 {
		kinds++
	}
	if len(opts.Selector.ObservationIDs) > 0 {
		kinds++
	}
	if kinds != 1 {
		return errors.New("exactly one selector required: --project, --event-ids, or --observation-ids")
	}
	if opts.Apply && !opts.NoQuarantine {
		if opts.AgeRecipient == "" {
			return errors.New("missing --age-recipient (or pass --no-quarantine for direct destruction)")
		}
		if opts.QuarantineOut == "" {
			return errors.New("missing --quarantine-out")
		}
		if _, err := os.Stat(opts.QuarantineOut); err == nil {
			return fmt.Errorf("refusing to clobber existing quarantine archive %s", opts.QuarantineOut)
		}
	}
	return nil
}

func Run(ctx context.Context, opts Options) (Report, error) {
	report := Report{DryRun: !opts.Apply, Selector: opts.Selector.kind(), Engine: engineOf(opts.DBPath).String()}
	if err := ValidateOptions(opts); err != nil {
		return report, err
	}
	unlock, err := lockRaw(opts, opts.Apply)
	if err != nil {
		return report, err
	}
	defer unlock()
	if _, err := os.Stat(opts.IntentPath); err == nil {
		return report, fmt.Errorf("intent journal %s exists — a prior apply died mid-flight; inspect it, then re-run with --resume-intent", opts.IntentPath)
	}
	gen, err := readGeneration(opts.ManifestPath)
	if err != nil {
		return report, err // corrupt manifest: stop before anything else (mirror lineage)
	}
	report.Generation = gen

	db, err := openDB(opts.DBPath)
	if err != nil {
		return report, err
	}
	defer db.Close()

	report.DBBytesBefore = dbBytes(ctx, db, opts.DBPath)

	sets, err := resolveSets(ctx, db, opts.Selector)
	if err != nil {
		return report, err
	}
	report.EventsMatched = int64(len(sets.eventIDs))
	report.ObservationsMatched = int64(len(sets.observationIDs))

	files, err := scanFiles(opts, sets)
	if err != nil {
		return report, err
	}
	report.RawFilesMatched = files.rawCount()
	report.PendingFilesMatched = len(files.Pending)
	report.InflightFilesMatched = len(files.Inflight)
	report.FailedFilesMatched = len(files.Failed)

	if !opts.Apply {
		report.DBBytesAfter = report.DBBytesBefore
		return report, nil
	}
	// Zero-work shortcut for id selectors only: a project apply must always
	// reach deleteRows — sessions, derivation_runs, and identity rows are
	// project-scoped and invisible to the event/observation match counts
	// (round-2 finding N3).
	if opts.Selector.kind() != "project" && len(sets.eventIDs)+len(sets.observationIDs)+files.total() == 0 {
		report.DBBytesAfter = report.DBBytesBefore
		return report, nil
	}

	// --- apply ---
	// Review R3: an abandoned part temp or drainer staging copy may hold the
	// bytes being purged. They are never custody (their originals still
	// exist flat or in the queue), so discard them under the lock before
	// anything is journaled.
	if err := removeStaleRawTemps(opts); err != nil {
		return report, err
	}
	intent := intentJournal{
		CreatedAtEpoch:      time.Now().Unix(),
		Phase:               phasePreCommit,
		Selector:            opts.Selector.kind(),
		Files:               files,
		ProbeEventIDs:       probe(sets.eventIDs),
		ProbeObservationIDs: probe(sets.observationIDs),
	}
	if opts.Selector.Project != "" {
		n, err := projectScopedRowCount(ctx, db, opts.Selector.Project)
		if err != nil {
			return report, err
		}
		intent.ProbeProject = opts.Selector.Project
		intent.ProbeProjectRows = n
	}
	if err := writeIntent(opts.IntentPath, intent); err != nil {
		return report, err
	}

	if !opts.NoQuarantine && files.total() > 0 {
		members, bytes, err := quarantine(opts, files)
		if err != nil {
			return report, err
		}
		if members != files.total() {
			return report, fmt.Errorf("quarantine reconciliation failed: archived %d members, matched %d files", members, files.total())
		}
		report.QuarantineOut = opts.QuarantineOut
		report.QuarantineMembers = members
		report.QuarantineBytes = bytes
	}

	if err := deleteRows(ctx, db, sets, opts.Selector, &report); err != nil {
		return report, err
	}

	// The DB half is committed: flip the intent so a crash from here on
	// resumes into file deletion instead of refusing.
	intent.Phase = phaseCommitted
	intent.DBDeleted = report.dbMutations() > 0
	if err := writeIntent(opts.IntentPath, intent); err != nil {
		return report, fmt.Errorf("DB purge committed but intent flip failed (resume will reconcile via probes): %w", err)
	}

	if err := finishFiles(ctx, db, opts, files, &report, intent.DBDeleted); err != nil {
		return report, err
	}
	return report, nil
}

// dbMutations counts every row the purge transaction touched — the custody
// generation must advance on ANY store mutation, including project-scoped
// rows outside the event/observation counts (round-2 findings N2/N3).
func (r Report) dbMutations() int64 {
	return r.EventsDeleted + r.ObservationsDeleted + r.SourceLinksDeleted +
		r.ReviewLogDeleted + r.SupersedesNulled + r.SessionsDeleted +
		r.DerivationRunsDeleted + r.IdentityRootsDeleted + r.IdentityAlertsDeleted +
		r.DeriveIntentsDeleted + r.RunManifestRowsDeleted +
		r.UsageRowsDeleted + r.TriageSuggestionsDeleted + r.EmbeddingsDeleted
}

// finishFiles is the post-commit half shared by Run and ResumeIntent:
// all-or-error file deletion, WAL truncate + optional VACUUM, generation
// bump, intent removal — in that order, so any failure keeps the intent
// for another resume. dbWork says whether the committed transaction
// mutated rows: a DB-only purge must still bump the generation even when
// no files were involved (round-2 finding N2).
func finishFiles(ctx context.Context, db *conn, opts Options, files FileMatches, report *Report, dbWork bool) error {
	if err := deleteFiles(opts, files, report); err != nil {
		return fmt.Errorf("file deletion incomplete — intent journal retained, re-run --resume-intent after fixing: %w", err)
	}

	if db.pg() {
		// Locked decision 4 (#616 slice B): no file-rewrite lever on a
		// server; reported, never an error, never VACUUM FULL.
		if opts.Vacuum {
			report.VacuumSkippedReason = pgVacuumSkipped
		}
	} else {
		if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			return fmt.Errorf("wal checkpoint (intent retained): %w", err)
		}
		if opts.Vacuum {
			if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
				return fmt.Errorf("vacuum (intent retained): %w", err)
			}
			report.Vacuumed = true
		}
	}
	report.DBBytesAfter = dbBytes(ctx, db, opts.DBPath)

	filesDeleted := report.RawFilesDeleted + report.PendingFilesDeleted + report.InflightFilesDeleted + report.FailedFilesDeleted
	if dbWork || filesDeleted > 0 {
		gen, err := bumpGeneration(opts.ManifestPath, purgeStamp{
			AtEpoch:             time.Now().Unix(),
			Selector:            report.Selector,
			EventsDeleted:       report.EventsDeleted,
			ObservationsDeleted: report.ObservationsDeleted,
			FilesDeleted:        filesDeleted,
		})
		if err != nil {
			return fmt.Errorf("generation bump (intent retained): %w", err)
		}
		report.Generation = gen
	}

	if err := os.Remove(opts.IntentPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove intent journal: %w", err)
	}
	return nil
}

// ResumeIntent finishes an apply that died after writing the intent
// journal. Phase decides what is safe:
//   - "committed": the DB transaction landed; finish file deletion, WAL/
//     VACUUM, and the generation bump.
//   - "pre-commit" with surviving probe rows: the transaction rolled back
//     (it is atomic); nothing destructive happened — the intent is removed
//     and the operator re-runs a normal apply. NO files are deleted.
//   - "pre-commit" with all probe rows gone: the transaction committed but
//     the phase flip was lost; treated as committed.
//
// A duplicate generation bump after a resume-retry is possible and
// harmless (mirrors resync once more); a missed bump is not, which is why
// the bump stays behind successful file deletion.
func ResumeIntent(ctx context.Context, opts Options) (Report, error) {
	report := Report{Selector: "resume-intent", Engine: engineOf(opts.DBPath).String()}
	unlock, err := lockRaw(opts, true)
	if err != nil {
		return report, err
	}
	defer unlock()
	b, err := os.ReadFile(opts.IntentPath)
	if err != nil {
		return report, fmt.Errorf("read intent journal: %w", err)
	}
	var intent intentJournal
	if err := json.Unmarshal(b, &intent); err != nil {
		return report, fmt.Errorf("parse intent journal: %w", err)
	}
	if _, err := readGeneration(opts.ManifestPath); err != nil {
		return report, err // corrupt manifest: repair before resuming
	}
	if err := removeStaleRawTemps(opts); err != nil {
		return report, err
	}

	db, err := openDB(opts.DBPath)
	if err != nil {
		return report, err
	}
	defer db.Close()

	dbWork := false
	switch intent.Phase {
	case phaseCommitted:
		dbWork = intent.DBDeleted
	case phasePreCommit:
		// Commit can only be proven through probes: recorded row ids
		// (events/observations) or the project-scoped row count (round-3
		// finding R3-1). With no probe of any kind, the transaction had
		// nothing to delete — commit vs rollback is indistinguishable AND
		// irrelevant for the DB, but the quarantine archive is unverified,
		// so file deletion must still refuse; clearing the intent and
		// re-running the apply is always safe (round-2 finding N1).
		if len(intent.ProbeEventIDs)+len(intent.ProbeObservationIDs) == 0 && intent.ProbeProjectRows == 0 {
			if err := os.Remove(opts.IntentPath); err != nil {
				return report, fmt.Errorf("remove unprovable pre-commit intent: %w", err)
			}
			return report, errors.New("intent was pre-commit with no probe rows: commit cannot be proven, nothing was deleted — intent cleared, re-run the apply (a stale quarantine archive may need removing first)")
		}
		alive, err := probeRowsExist(ctx, db, intent)
		if err != nil {
			return report, err
		}
		if alive {
			if err := os.Remove(opts.IntentPath); err != nil {
				return report, fmt.Errorf("remove pre-commit intent: %w", err)
			}
			return report, errors.New("intent was pre-commit and the DB rows survived: the purge transaction rolled back, nothing was deleted — intent cleared, re-run the apply (a stale quarantine archive may need removing first)")
		}
		// Probes existed and are gone: the transaction committed (and
		// deleted rows), only the flip was lost.
		dbWork = true
	default:
		return report, fmt.Errorf("intent journal has unknown phase %q — refusing", intent.Phase)
	}

	report.RawFilesMatched = intent.Files.rawCount()
	report.PendingFilesMatched = len(intent.Files.Pending)
	report.InflightFilesMatched = len(intent.Files.Inflight)
	report.FailedFilesMatched = len(intent.Files.Failed)
	return report, finishFiles(ctx, db, opts, intent.Files, &report, dbWork)
}

func probe(ids []int64) []int64 {
	if len(ids) <= probeLimit {
		return ids
	}
	return ids[:probeLimit]
}

func probeRowsExist(ctx context.Context, db *conn, intent intentJournal) (bool, error) {
	check := func(table string, ids []int64) (bool, error) {
		if len(ids) == 0 {
			return false, nil
		}
		var sb strings.Builder
		args := make([]any, 0, len(ids))
		for i, id := range ids {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString("?")
			args = append(args, id)
		}
		var n int
		err := db.QueryRowContext(ctx, db.q(`SELECT count(*) FROM `+table+` WHERE id IN (`+sb.String()+`)`), args...).Scan(&n)
		return n > 0, err
	}
	if alive, err := check("events", intent.ProbeEventIDs); alive || err != nil {
		return alive, err
	}
	if alive, err := check("observations", intent.ProbeObservationIDs); alive || err != nil {
		return alive, err
	}
	// Project-scoped probe (R3-1): rows counted at intent time still
	// present means the transaction rolled back. (New rows arriving for
	// the project between crash and resume make this read "alive" too —
	// the clear-and-rerun path then re-deletes them, which converges.)
	if intent.ProbeProjectRows > 0 {
		n, err := projectScopedRowCount(ctx, db, intent.ProbeProject)
		if err != nil {
			return false, err
		}
		return n > 0, nil
	}
	return false, nil
}

// projectScopedRowCount counts the project-selector rows that live outside
// the event/observation probe sets: sessions, derivation_runs, the 0010
// identity tables, derive intents and the project's observation_usage rows.
// One query for both engines (#616 slice E review round 1, F1): every table
// the project purge deletes is probed, so a residue-only purge — usage rows
// included — records a probe, and a lost committed-intent flip can still be
// proven by the rows' absence instead of being dropped as unprovable.
func projectScopedRowCount(ctx context.Context, db *conn, project string) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, db.q(`SELECT
		(SELECT count(*) FROM sessions WHERE project = ?) +
		(SELECT count(*) FROM derivation_runs WHERE project = ?) +
		(SELECT count(*) FROM project_roots WHERE project = ?) +
		(SELECT count(*) FROM project_identity_alerts WHERE project = ?) +
		(SELECT count(*) FROM derive_intents WHERE project = ?) +
		(SELECT count(*) FROM observation_usage WHERE project = ?)`),
		project, project, project, project, project, project).Scan(&n)
	return n, err
}

// pgVacuumSkipped is the report reason for --vacuum on Postgres.
const pgVacuumSkipped = "postgres: no VACUUM from the tool (server-managed storage; autovacuum reclaims dead tuples, WAL archives and base backups keep them until retention expiry)"

// conn is the purge handle: a single-connection pool tagged with its engine.
// Every statement that carries '?' placeholders goes through q, which is
// the identity on SQLite (byte-for-byte the single-engine statements).
type conn struct {
	*sql.DB
	eng memdb.Engine
}

func (c *conn) pg() bool { return c.eng == memdb.EnginePostgres }

func (c *conn) q(query string) string { return memdb.Rebind(c.eng, query) }

func engineOf(path string) memdb.Engine {
	if memdb.IsPostgresDSN(path) {
		return memdb.EnginePostgres
	}
	return memdb.EngineSQLite
}

func openDB(path string) (*conn, error) {
	if engineOf(path) == memdb.EnginePostgres {
		// Raw pgx handle, never db.Open: no migrations, no migrate lock, no
		// provisioning — a purge must not apply DDL to the hub.
		db, err := sql.Open("pgx", path)
		if err != nil {
			return nil, err
		}
		// Single connection: the temp selector tables are session-scoped
		// and must be visible to the delete transaction.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		if err := db.Ping(); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("connect postgres store: %w", err)
		}
		return &conn{DB: db, eng: memdb.EnginePostgres}, nil
	}
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=10000&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	// Single connection: the IMMEDIATE transaction, the temp selector
	// tables, and every PRAGMA must ride the same underlying handle
	// (memoire:D145 lesson).
	db.SetMaxOpenConns(1)
	return &conn{DB: db, eng: memdb.EngineSQLite}, nil
}

// dbBytes is the store size: the file on SQLite, pg_database_size on
// Postgres (which does not shrink after a purge — no VACUUM FULL).
func dbBytes(ctx context.Context, db *conn, path string) int64 {
	if db.pg() {
		var n int64
		if err := db.QueryRowContext(ctx, `SELECT pg_database_size(current_database())`).Scan(&n); err == nil {
			return n
		}
		return 0
	}
	if st, err := os.Stat(path); err == nil {
		return st.Size()
	}
	return 0
}

type idSets struct {
	eventIDs       []int64
	observationIDs []int64
	// pairKeys is source_adapter + "\x00" + source_event_id for the purged
	// events — the join key between DB rows and raw/queue envelope files.
	pairKeys map[string]struct{}
}

func resolveSets(ctx context.Context, db *conn, sel Selector) (idSets, error) {
	sets := idSets{pairKeys: map[string]struct{}{}}
	// Postgres: BIGINT ids (INTEGER is int4 there; events/observations ids
	// are BIGINT) and ON CONFLICT DO NOTHING for SQLite's OR IGNORE.
	idType, insertHead, insertTail := "INTEGER", `INSERT OR IGNORE INTO `, ""
	if db.pg() {
		idType, insertHead, insertTail = "BIGINT", `INSERT INTO `, ` ON CONFLICT DO NOTHING`
	}
	tmp := func(name string, ids []int64) error {
		if _, err := db.ExecContext(ctx, `CREATE TEMP TABLE `+name+` (id `+idType+` PRIMARY KEY)`); err != nil {
			return err
		}
		for i := 0; i < len(ids); i += 500 {
			end := min(i+500, len(ids))
			var sb strings.Builder
			sb.WriteString(insertHead + name + `(id) VALUES `)
			args := make([]any, 0, end-i)
			for j := i; j < end; j++ {
				if j > i {
					sb.WriteString(",")
				}
				sb.WriteString("(?)")
				args = append(args, ids[j])
			}
			sb.WriteString(insertTail)
			if _, err := db.ExecContext(ctx, db.q(sb.String()), args...); err != nil {
				return err
			}
		}
		return nil
	}

	switch {
	case sel.Project != "":
		if err := collectIDs(ctx, db, &sets.eventIDs, db.q(`SELECT id FROM events WHERE project = ?`), sel.Project); err != nil {
			return sets, err
		}
		if err := collectIDs(ctx, db, &sets.observationIDs, db.q(`SELECT id FROM observations WHERE project = ?`), sel.Project); err != nil {
			return sets, err
		}
	case len(sel.EventIDs) > 0:
		if err := tmp("purge_sel_events", sel.EventIDs); err != nil {
			return sets, err
		}
		if err := collectIDs(ctx, db, &sets.eventIDs, `SELECT id FROM events WHERE id IN (SELECT id FROM purge_sel_events)`); err != nil {
			return sets, err
		}
		if err := collectIDs(ctx, db, &sets.observationIDs, `
			SELECT id FROM observations WHERE event_id IN (SELECT id FROM purge_sel_events)
			UNION
			SELECT observation_id FROM observation_source_events WHERE event_id IN (SELECT id FROM purge_sel_events)`); err != nil {
			return sets, err
		}
	case len(sel.ObservationIDs) > 0:
		if err := tmp("purge_sel_obs", sel.ObservationIDs); err != nil {
			return sets, err
		}
		if err := collectIDs(ctx, db, &sets.observationIDs, `SELECT id FROM observations WHERE id IN (SELECT id FROM purge_sel_obs)`); err != nil {
			return sets, err
		}
	}

	if len(sets.eventIDs) > 0 {
		if err := tmp("purge_events", sets.eventIDs); err != nil {
			return sets, err
		}
		rows, err := db.QueryContext(ctx, `SELECT source_adapter, source_event_id FROM events WHERE id IN (SELECT id FROM purge_events)`)
		if err != nil {
			return sets, err
		}
		defer rows.Close()
		for rows.Next() {
			var adapter, sourceID sql.NullString
			if err := rows.Scan(&adapter, &sourceID); err != nil {
				return sets, err
			}
			// NULL source identity: no envelope file can match this event.
			if adapter.Valid && sourceID.Valid {
				sets.pairKeys[adapter.String+"\x00"+sourceID.String] = struct{}{}
			}
		}
		if err := rows.Err(); err != nil {
			return sets, err
		}
	}
	if len(sets.observationIDs) > 0 {
		if err := tmp("purge_obs", sets.observationIDs); err != nil {
			return sets, err
		}
	}
	return sets, nil
}

func collectIDs(ctx context.Context, db *conn, dst *[]int64, query string, args ...any) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		*dst = append(*dst, id)
	}
	return rows.Err()
}

// envelopeKey is the plain-field fallback for files the drainer
// normalization rejects. Parse errors return no match: an unparseable file
// is never silently purged.
type envelopeKey struct {
	Project       string `json:"project"`
	SourceAdapter string `json:"source_adapter"`
	SourceEventID string `json:"source_event_id"`
}

func scanFiles(opts Options, sets idSets) (FileMatches, error) {
	var files FileMatches
	if opts.Selector.kind() == "observation_ids" {
		return files, nil // DB-only purge: inputs survive by design.
	}
	match := func(b []byte) bool {
		var key envelopeKey
		if err := json.Unmarshal(b, &key); err != nil {
			return false // unparseable: never silently purged
		}
		if opts.Selector.Project != "" {
			return key.Project == opts.Selector.Project
		}
		// Event-id matching: prefer the drainer's normalization so a file
		// that omitted source_event_id on disk still matches the derived
		// identity its DB row carries; fall back to the plain fields.
		if event, err := drainer.NormalizeEnvelope(b); err == nil {
			if _, ok := sets.pairKeys[event.SourceAdapter+"\x00"+event.SourceEventID]; ok {
				return true
			}
		}
		_, ok := sets.pairKeys[key.SourceAdapter+"\x00"+key.SourceEventID]
		return ok && key.SourceAdapter != "" && key.SourceEventID != ""
	}
	for _, dir := range []struct {
		path     string
		dst      *[]string
		sidecars bool
	}{
		{opts.RawDir, &files.Raw, false},
		{opts.PendingDir, &files.Pending, false},
		{opts.InflightDir, &files.Inflight, false},
		{opts.FailedDir, &files.Failed, true},
	} {
		if dir.path == "" {
			continue
		}
		if dir.path == opts.RawDir {
			if err := scanRaw(opts.RawDir, match, &files); err != nil {
				return files, err
			}
			continue
		}
		entries, err := os.ReadDir(dir.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return files, fmt.Errorf("scan %s: %w", dir.path, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".error.json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir.path, name))
			if err != nil {
				return files, fmt.Errorf("read %s: %w", filepath.Join(dir.path, name), err)
			}
			if match(b) {
				*dir.dst = append(*dir.dst, name)
				// A failed item's .error.json sidecar is part of its
				// residue: quarantined and deleted alongside.
				if dir.sidecars {
					if sidecar := name + ".error.json"; fileExists(filepath.Join(dir.path, sidecar)) {
						*dir.dst = append(*dir.dst, sidecar)
					}
				}
			}
		}
	}
	return files, nil
}

// scanRaw matches raw/ envelopes, flat or packed. A packed name whose flat
// copy survives (packer crash window) is listed on both sides so both go.
func scanRaw(rawDir string, match func([]byte) bool, files *FileMatches) error {
	err := rawstore.Walk(rawDir, func(e rawstore.Entry, body []byte) error {
		if !match(body) {
			return nil
		}
		if e.Pack == "" || e.AlsoFlat {
			files.Raw = append(files.Raw, e.Name)
		}
		if e.Pack != "" {
			if files.RawPacked == nil {
				files.RawPacked = map[string][]string{}
			}
			files.RawPacked[e.Pack] = append(files.RawPacked[e.Pack], e.Name)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("scan %s: %w", rawDir, err)
	}
	return nil
}

// removeStaleRawTemps discards part temps and drainer staging copies
// (rawstore.RemoveStaleTemps). Purge runs with the store quiesced, so no
// live writer owns them.
func removeStaleRawTemps(opts Options) error {
	if opts.RawDir == "" {
		return nil
	}
	if _, err := rawstore.RemoveStaleTemps(opts.RawDir); err != nil {
		return fmt.Errorf("discard stale raw temps: %w", err)
	}
	return nil
}

// lockRaw enters raw/ custody (rawstore.Custody, contract item 1): Move
// for an apply or resume (stale part temps discarded), Read for a dry run.
// Purge owns its intent journal, so the interlock is skipped here; Run and
// ResumeIntent apply their own intent rules under the same lock.
func lockRaw(opts Options, mutate bool) (func(), error) {
	if opts.RawDir == "" {
		return func() {}, nil
	}
	if _, err := os.Stat(opts.RawDir); errors.Is(err, os.ErrNotExist) {
		return func() {}, nil
	}
	mode := rawstore.ModeRead
	if mutate {
		mode = rawstore.ModeMove
	}
	s, err := rawstore.Custody{RawDir: opts.RawDir, SkipIntents: true}.Enter(mode)
	if err != nil {
		return nil, err
	}
	return s.Release, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// quarantine streams every matched file into one tar piped through the age
// binary (same mechanism as internal/sweep). Tar member names are prefixed
// by origin dir so raw/ and queue copies of the same filename both survive.
func quarantine(opts Options, files FileMatches) (members int, bytes int64, err error) {
	if _, err := exec.LookPath("age"); err != nil {
		return 0, 0, fmt.Errorf("age binary not found")
	}
	pr, pw := io.Pipe()
	cmd := exec.Command("age", "-r", opts.AgeRecipient, "-o", opts.QuarantineOut)
	cmd.Stdin = pr

	count := 0
	go func() {
		tw := tar.NewWriter(pw)
		write := func(dir, prefix string, names []string) error {
			for _, name := range names {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					return fmt.Errorf("re-read %s for quarantine: %w", name, err)
				}
				hdr := &tar.Header{Name: prefix + "/" + name, Mode: 0o600, Size: int64(len(data))}
				if err := tw.WriteHeader(hdr); err != nil {
					return err
				}
				if _, err := tw.Write(data); err != nil {
					return err
				}
				count++
			}
			return nil
		}
		writePacked := func() error {
			for _, pack := range sortedKeys(files.RawPacked) {
				want := map[string]bool{}
				for _, name := range files.RawPacked[pack] {
					want[name] = true
				}
				err := rawstore.ReadPack(filepath.Join(opts.RawDir, pack), func(m rawstore.Member) error {
					if !want[m.Name] {
						return nil
					}
					hdr := &tar.Header{Name: "raw-pack/" + pack + "/" + m.Name, Mode: 0o600, Size: int64(len(m.Body))}
					if err := tw.WriteHeader(hdr); err != nil {
						return err
					}
					if _, err := tw.Write(m.Body); err != nil {
						return err
					}
					count++
					return nil
				})
				if err != nil {
					return fmt.Errorf("re-read %s for quarantine: %w", pack, err)
				}
			}
			return nil
		}
		var werr error
		if werr = write(opts.RawDir, "raw", files.Raw); werr == nil {
			werr = writePacked()
		}
		if werr == nil {
			if werr = write(opts.PendingDir, "queue-pending", files.Pending); werr == nil {
				if werr = write(opts.InflightDir, "queue-inflight", files.Inflight); werr == nil {
					werr = write(opts.FailedDir, "queue-failed", files.Failed)
				}
			}
		}
		if werr == nil {
			werr = tw.Close()
		}
		pw.CloseWithError(werr)
	}()

	if err := cmd.Run(); err != nil {
		pr.Close()
		return 0, 0, fmt.Errorf("age encrypt: %w", err)
	}
	st, err := os.Stat(opts.QuarantineOut)
	if err != nil {
		return 0, 0, fmt.Errorf("stat quarantine archive: %w", err)
	}
	return count, st.Size(), nil
}

func deleteRows(ctx context.Context, db *conn, sets idSets, sel Selector, report *Report) error {
	tx, err := db.BeginTx(ctx, nil) // _txlock=immediate in the DSN
	if err != nil {
		return err
	}
	defer tx.Rollback()

	count := func(res sql.Result) int64 {
		n, _ := res.RowsAffected()
		return n
	}

	if len(sets.observationIDs) > 0 {
		res, err := tx.ExecContext(ctx, `UPDATE observations SET supersedes_id = NULL
			WHERE supersedes_id IN (SELECT id FROM purge_obs) AND id NOT IN (SELECT id FROM purge_obs)`)
		if err != nil {
			return fmt.Errorf("null supersedes: %w", err)
		}
		report.SupersedesNulled = count(res)

		res, err = tx.ExecContext(ctx, `DELETE FROM observation_source_events
			WHERE observation_id IN (SELECT id FROM purge_obs)`)
		if err != nil {
			return fmt.Errorf("delete source links: %w", err)
		}
		report.SourceLinksDeleted = count(res)

		res, err = tx.ExecContext(ctx, `DELETE FROM observation_review_log
			WHERE observation_id IN (SELECT id FROM purge_obs)`)
		if err != nil {
			return fmt.Errorf("delete review log: %w", err)
		}
		report.ReviewLogDeleted = count(res)

		if err := deleteObservationChildren(ctx, tx, db, report); err != nil {
			return err
		}

		// The AFTER DELETE trigger keeps observations_fts consistent
		// (SQLite); on Postgres text_tsv is a generated column of the row.
		res, err = tx.ExecContext(ctx, `DELETE FROM observations WHERE id IN (SELECT id FROM purge_obs)`)
		if err != nil {
			return fmt.Errorf("delete observations: %w", err)
		}
		report.ObservationsDeleted = count(res)
	}

	if len(sets.eventIDs) > 0 {
		res, err := tx.ExecContext(ctx, `DELETE FROM observation_source_events
			WHERE event_id IN (SELECT id FROM purge_events)`)
		if err != nil {
			return fmt.Errorf("delete residual event links: %w", err)
		}
		report.SourceLinksDeleted += count(res)

		// Usage rows whose usage EVENT is purged (observation_usage.event_id
		// is the memory_usage event that produced the row). Both engines
		// (#616 slice E, decision 4): the SQLite table has no ON DELETE
		// CASCADE and its reason column is free text.
		res, err = tx.ExecContext(ctx, `DELETE FROM observation_usage WHERE event_id IN (SELECT id FROM purge_events)`)
		if err != nil {
			return fmt.Errorf("delete purged-event usage rows: %w", err)
		}
		report.UsageRowsDeleted += count(res)

		// Manifest links for hard-purged events go WITH them (memoire:D165
		// round-4 MED-NEW-1: SQLite AUTOINCREMENT never reuses ids, but a
		// bulk copy into a fresh PG store re-mints identities — an orphan
		// manifest row could then claim coverage of an unrelated new event).
		res, err = tx.ExecContext(ctx, `DELETE FROM run_input_events WHERE event_id IN (SELECT id FROM purge_events)`)
		if err != nil {
			return fmt.Errorf("delete purged-event manifest links: %w", err)
		}
		report.RunManifestRowsDeleted += count(res)

		res, err = tx.ExecContext(ctx, `DELETE FROM events WHERE id IN (SELECT id FROM purge_events)`)
		if err != nil {
			return fmt.Errorf("delete events: %w", err)
		}
		report.EventsDeleted = count(res)
	}

	if sel.Project != "" {
		res, err := tx.ExecContext(ctx, db.q(`DELETE FROM sessions WHERE project = ?`), sel.Project)
		if err != nil {
			return fmt.Errorf("delete sessions: %w", err)
		}
		report.SessionsDeleted = count(res)

		// Manifest rows die WITH their runs (0015) — delete before the runs
		// so the id subquery still resolves.
		res, err = tx.ExecContext(ctx, db.q(`DELETE FROM run_input_events
			WHERE run_id IN (SELECT id FROM derivation_runs WHERE project = ?)`), sel.Project)
		if err != nil {
			return fmt.Errorf("delete run input manifests: %w", err)
		}
		// += : the event-side manifest delete above may already have counted
		// rows for a project selector (its events populate purge_events).
		report.RunManifestRowsDeleted += count(res)

		res, err = tx.ExecContext(ctx, db.q(`DELETE FROM derivation_runs WHERE project = ?`), sel.Project)
		if err != nil {
			return fmt.Errorf("delete derivation runs: %w", err)
		}
		report.DerivationRunsDeleted = count(res)

		// Identity rows carry captured path data (schema 0010 roots come
		// from adapter-passed payloads) — a project purge removes them too.
		res, err = tx.ExecContext(ctx, db.q(`DELETE FROM project_roots WHERE project = ?`), sel.Project)
		if err != nil {
			return fmt.Errorf("delete project roots: %w", err)
		}
		report.IdentityRootsDeleted = count(res)

		res, err = tx.ExecContext(ctx, db.q(`DELETE FROM project_identity_alerts WHERE project = ?`), sel.Project)
		if err != nil {
			return fmt.Errorf("delete identity alerts: %w", err)
		}
		report.IdentityAlertsDeleted = count(res)

		// Derive notify intents (0013): project/session keys + notify epochs
		// are captured-under-the-project state; a project purge removes them
		// (memoire:D165 round-1 MED-6).
		res, err = tx.ExecContext(ctx, db.q(`DELETE FROM derive_intents WHERE project = ?`), sel.Project)
		if err != nil {
			return fmt.Errorf("delete derive intents: %w", err)
		}
		report.DeriveIntentsDeleted = count(res)

		// The project's usage rows (project/session keys + free-text reason
		// are captured-under-the-project state). Both engines (#616 slice E).
		res, err = tx.ExecContext(ctx, db.q(`DELETE FROM observation_usage WHERE project = ?`), sel.Project)
		if err != nil {
			return fmt.Errorf("delete project usage rows: %w", err)
		}
		report.UsageRowsDeleted += count(res)
	}

	// Tombstone only when the transaction actually mutated rows — a
	// convergent re-apply must not mint no-op audit entries.
	if report.dbMutations() > 0 {
		stamp, _ := json.Marshal(map[string]any{
			"selector":             sel.kind(),
			"events_deleted":       report.EventsDeleted,
			"observations_deleted": report.ObservationsDeleted,
		})
		if _, err := tx.ExecContext(ctx, db.q(`INSERT INTO audit_log (principal_id, action, resource_type, details_json, created_at_epoch)
			VALUES ('operator:purge', 'purge.apply', 'store', ?, ?)`), string(stamp), time.Now().Unix()); err != nil {
			return fmt.Errorf("audit tombstone: %w", err)
		}
	}

	return tx.Commit()
}

// deleteObservationChildren removes the derived children of the purge_obs
// set on BOTH engines (#616 slice E, operator decision 4: the SQLite engine
// used to leave them as orphans), inside the purge transaction and before
// the observation delete: the vector-lane rows (Postgres: runtime-provisioned
// observation_embeddings_pg with an ON DELETE CASCADE FK — deleted explicitly
// so the report counts them; SQLite: schema table observation_embeddings, no
// FK), triage suggestions (judge reasoning about the observation) and usage
// rows (the consuming agent's free-text reason lines). All three can quote
// the purged text; none has an ON DELETE CASCADE on SQLite.
func deleteObservationChildren(ctx context.Context, tx *sql.Tx, db *conn, report *Report) error {
	embeddingsTable, probe := "observation_embeddings", `SELECT COUNT(*) > 0 FROM sqlite_master WHERE type = 'table' AND name = 'observation_embeddings'`
	if db.pg() {
		embeddingsTable, probe = "observation_embeddings_pg", `SELECT to_regclass('observation_embeddings_pg') IS NOT NULL`
	}
	var hasEmbeddings bool
	if err := tx.QueryRowContext(ctx, probe).Scan(&hasEmbeddings); err != nil {
		return fmt.Errorf("probe embeddings table: %w", err)
	}
	if hasEmbeddings {
		res, err := tx.ExecContext(ctx, `DELETE FROM `+embeddingsTable+` WHERE observation_id IN (SELECT id FROM purge_obs)`)
		if err != nil {
			return fmt.Errorf("delete embeddings: %w", err)
		}
		report.EmbeddingsDeleted, _ = res.RowsAffected()
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM observation_triage_suggestions WHERE observation_id IN (SELECT id FROM purge_obs)`)
	if err != nil {
		return fmt.Errorf("delete triage suggestions: %w", err)
	}
	report.TriageSuggestionsDeleted, _ = res.RowsAffected()
	res, err = tx.ExecContext(ctx, `DELETE FROM observation_usage WHERE observation_id IN (SELECT id FROM purge_obs)`)
	if err != nil {
		return fmt.Errorf("delete usage rows: %w", err)
	}
	n, _ := res.RowsAffected()
	report.UsageRowsDeleted += n
	return nil
}

// deleteFiles is all-or-error: a matched file that cannot be removed fails
// the run (already-gone files are fine — resume retries are idempotent).
func deleteFiles(opts Options, files FileMatches, report *Report) error {
	var errs []error
	del := func(dir string, names []string, deleted *int) {
		for _, name := range names {
			err := os.Remove(filepath.Join(dir, name))
			if err == nil || errors.Is(err, os.ErrNotExist) {
				*deleted++
				continue
			}
			errs = append(errs, err)
		}
	}
	del(opts.RawDir, files.Raw, &report.RawFilesDeleted)
	// Packed members: rewrite each part without them. Representation-
	// independent (review R2): every journaled raw name — listed flat or
	// packed — is also searched in the parts of its day, so a name that
	// moved between the scan and this deletion (a packer run between a crash
	// and the resume) is still destroyed. Idempotent — a part that no longer
	// holds a name drops nothing and writes nothing — so, like ErrNotExist
	// for flat files, a listed name counts as deleted once no part holds it.
	if opts.RawDir != "" {
		var names []string
		names = append(names, files.Raw...)
		for _, members := range files.RawPacked {
			names = append(names, members...)
		}
		located, err := rawstore.Locate(opts.RawDir, names)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("locate packed raw names: %w", err))
		}
		dropByPart := map[string]map[string]bool{}
		for part, members := range files.RawPacked {
			if _, err := os.Stat(filepath.Join(opts.RawDir, part)); errors.Is(err, os.ErrNotExist) {
				continue // emptied and removed by an earlier attempt
			}
			for _, name := range members {
				if dropByPart[part] == nil {
					dropByPart[part] = map[string]bool{}
				}
				dropByPart[part][name] = true
			}
		}
		for name, parts := range located {
			for _, part := range parts {
				if dropByPart[part] == nil {
					dropByPart[part] = map[string]bool{}
				}
				dropByPart[part][name] = true
			}
		}
		for _, part := range sortedSetKeys(dropByPart) {
			res, err := rawstore.DropMembers(opts.RawDir, part, dropByPart[part])
			if err != nil {
				errs = append(errs, fmt.Errorf("rewrite %s: %w", part, err))
				continue
			}
			if res.Written {
				report.RawPacksRewritten++
			}
		}
		for _, members := range files.RawPacked {
			report.RawFilesDeleted += len(members)
		}
	}
	del(opts.PendingDir, files.Pending, &report.PendingFilesDeleted)
	del(opts.InflightDir, files.Inflight, &report.InflightFilesDeleted)
	del(opts.FailedDir, files.Failed, &report.FailedFilesDeleted)
	return errors.Join(errs...)
}

func sortedSetKeys(m map[string]map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func writeIntent(path string, intent intentJournal) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readGeneration distinguishes a missing manifest (fresh store: generation
// 0) from a corrupt or unreadable one, which must STOP any destructive run:
// resetting the counter would break the mirror protocol's stale-artifact
// detection (docs/MIRROR-PROTOCOL.md).
func readGeneration(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("custody manifest %s unreadable — repair it before purging: %w", path, err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return 0, fmt.Errorf("custody manifest %s is corrupt — repair it before purging: %w", path, err)
	}
	return m.Generation, nil
}

func bumpGeneration(path string, stamp purgeStamp) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	gen, err := readGeneration(path)
	if err != nil {
		return 0, err
	}
	m := manifest{Generation: gen + 1, UpdatedAtEpoch: time.Now().Unix(), LastPurge: &stamp}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return 0, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, err
	}
	return m.Generation, nil
}
