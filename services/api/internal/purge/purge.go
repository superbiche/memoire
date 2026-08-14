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
// Not touched, deliberately: audit_log (tombstone history; details_json was
// already covered by memoire:D145 redaction), and — for id selectors —
// derivation_runs. Physical-media caveat: VACUUM rewrites the database
// file, but SSD wear-leveling means old blocks are not scrubbed; purge is
// application-level destruction, same posture as the D144 sweep.
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
	"strings"
	"time"

	"memoire/services/api/internal/drainer"
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
	Raw      []string `json:"raw"`
	Pending  []string `json:"pending"`
	Inflight []string `json:"inflight"`
	Failed   []string `json:"failed"`
}

func (f FileMatches) total() int {
	return len(f.Raw) + len(f.Pending) + len(f.Inflight) + len(f.Failed)
}

type Report struct {
	DryRun   bool   `json:"dry_run"`
	Selector string `json:"selector"`

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

	RawFilesMatched      int `json:"raw_files_matched"`
	RawFilesDeleted      int `json:"raw_files_deleted"`
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
	report := Report{DryRun: !opts.Apply, Selector: opts.Selector.kind()}
	if err := ValidateOptions(opts); err != nil {
		return report, err
	}
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

	if st, err := os.Stat(opts.DBPath); err == nil {
		report.DBBytesBefore = st.Size()
	}

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
	report.RawFilesMatched = len(files.Raw)
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
		r.DeriveIntentsDeleted + r.RunManifestRowsDeleted
}

// finishFiles is the post-commit half shared by Run and ResumeIntent:
// all-or-error file deletion, WAL truncate + optional VACUUM, generation
// bump, intent removal — in that order, so any failure keeps the intent
// for another resume. dbWork says whether the committed transaction
// mutated rows: a DB-only purge must still bump the generation even when
// no files were involved (round-2 finding N2).
func finishFiles(ctx context.Context, db *sql.DB, opts Options, files FileMatches, report *Report, dbWork bool) error {
	if err := deleteFiles(opts, files, report); err != nil {
		return fmt.Errorf("file deletion incomplete — intent journal retained, re-run --resume-intent after fixing: %w", err)
	}

	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("wal checkpoint (intent retained): %w", err)
	}
	if opts.Vacuum {
		if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
			return fmt.Errorf("vacuum (intent retained): %w", err)
		}
		report.Vacuumed = true
	}
	if st, err := os.Stat(opts.DBPath); err == nil {
		report.DBBytesAfter = st.Size()
	}

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
	report := Report{Selector: "resume-intent"}
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

	report.RawFilesMatched = len(intent.Files.Raw)
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

func probeRowsExist(ctx context.Context, db *sql.DB, intent intentJournal) (bool, error) {
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
		err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE id IN (`+sb.String()+`)`, args...).Scan(&n)
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
// the event/observation probe sets: sessions, derivation_runs, and the
// 0010 identity tables.
func projectScopedRowCount(ctx context.Context, db *sql.DB, project string) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM sessions WHERE project = ?) +
		(SELECT count(*) FROM derivation_runs WHERE project = ?) +
		(SELECT count(*) FROM project_roots WHERE project = ?) +
		(SELECT count(*) FROM project_identity_alerts WHERE project = ?) +
		(SELECT count(*) FROM derive_intents WHERE project = ?)`,
		project, project, project, project, project).Scan(&n)
	return n, err
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=10000&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	// Single connection: the IMMEDIATE transaction, the temp selector
	// tables, and every PRAGMA must ride the same underlying handle
	// (memoire:D145 lesson).
	db.SetMaxOpenConns(1)
	return db, nil
}

type idSets struct {
	eventIDs       []int64
	observationIDs []int64
	// pairKeys is source_adapter + "\x00" + source_event_id for the purged
	// events — the join key between DB rows and raw/queue envelope files.
	pairKeys map[string]struct{}
}

func resolveSets(ctx context.Context, db *sql.DB, sel Selector) (idSets, error) {
	sets := idSets{pairKeys: map[string]struct{}{}}
	tmp := func(name string, ids []int64) error {
		if _, err := db.ExecContext(ctx, `CREATE TEMP TABLE `+name+` (id INTEGER PRIMARY KEY)`); err != nil {
			return err
		}
		for i := 0; i < len(ids); i += 500 {
			end := min(i+500, len(ids))
			var sb strings.Builder
			sb.WriteString(`INSERT OR IGNORE INTO ` + name + `(id) VALUES `)
			args := make([]any, 0, end-i)
			for j := i; j < end; j++ {
				if j > i {
					sb.WriteString(",")
				}
				sb.WriteString("(?)")
				args = append(args, ids[j])
			}
			if _, err := db.ExecContext(ctx, sb.String(), args...); err != nil {
				return err
			}
		}
		return nil
	}

	switch {
	case sel.Project != "":
		if err := collectIDs(ctx, db, &sets.eventIDs, `SELECT id FROM events WHERE project = ?`, sel.Project); err != nil {
			return sets, err
		}
		if err := collectIDs(ctx, db, &sets.observationIDs, `SELECT id FROM observations WHERE project = ?`, sel.Project); err != nil {
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

func collectIDs(ctx context.Context, db *sql.DB, dst *[]int64, query string, args ...any) error {
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
		var werr error
		if werr = write(opts.RawDir, "raw", files.Raw); werr == nil {
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

func deleteRows(ctx context.Context, db *sql.DB, sets idSets, sel Selector, report *Report) error {
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

		// The AFTER DELETE trigger keeps observations_fts consistent.
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
		res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE project = ?`, sel.Project)
		if err != nil {
			return fmt.Errorf("delete sessions: %w", err)
		}
		report.SessionsDeleted = count(res)

		// Manifest rows die WITH their runs (0015) — delete before the runs
		// so the id subquery still resolves.
		res, err = tx.ExecContext(ctx, `DELETE FROM run_input_events
			WHERE run_id IN (SELECT id FROM derivation_runs WHERE project = ?)`, sel.Project)
		if err != nil {
			return fmt.Errorf("delete run input manifests: %w", err)
		}
		// += : the event-side manifest delete above may already have counted
		// rows for a project selector (its events populate purge_events).
		report.RunManifestRowsDeleted += count(res)

		res, err = tx.ExecContext(ctx, `DELETE FROM derivation_runs WHERE project = ?`, sel.Project)
		if err != nil {
			return fmt.Errorf("delete derivation runs: %w", err)
		}
		report.DerivationRunsDeleted = count(res)

		// Identity rows carry captured path data (schema 0010 roots come
		// from adapter-passed payloads) — a project purge removes them too.
		res, err = tx.ExecContext(ctx, `DELETE FROM project_roots WHERE project = ?`, sel.Project)
		if err != nil {
			return fmt.Errorf("delete project roots: %w", err)
		}
		report.IdentityRootsDeleted = count(res)

		res, err = tx.ExecContext(ctx, `DELETE FROM project_identity_alerts WHERE project = ?`, sel.Project)
		if err != nil {
			return fmt.Errorf("delete identity alerts: %w", err)
		}
		report.IdentityAlertsDeleted = count(res)

		// Derive notify intents (0013): project/session keys + notify epochs
		// are captured-under-the-project state; a project purge removes them
		// (memoire:D165 round-1 MED-6).
		res, err = tx.ExecContext(ctx, `DELETE FROM derive_intents WHERE project = ?`, sel.Project)
		if err != nil {
			return fmt.Errorf("delete derive intents: %w", err)
		}
		report.DeriveIntentsDeleted = count(res)
	}

	// Tombstone only when the transaction actually mutated rows — a
	// convergent re-apply must not mint no-op audit entries.
	if report.dbMutations() > 0 {
		stamp, _ := json.Marshal(map[string]any{
			"selector":             sel.kind(),
			"events_deleted":       report.EventsDeleted,
			"observations_deleted": report.ObservationsDeleted,
		})
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log (principal_id, action, resource_type, details_json, created_at_epoch)
			VALUES ('operator:purge', 'purge.apply', 'store', ?, ?)`, string(stamp), time.Now().Unix()); err != nil {
			return fmt.Errorf("audit tombstone: %w", err)
		}
	}

	return tx.Commit()
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
	del(opts.PendingDir, files.Pending, &report.PendingFilesDeleted)
	del(opts.InflightDir, files.Inflight, &report.InflightFilesDeleted)
	del(opts.FailedDir, files.Failed, &report.FailedFilesDeleted)
	return errors.Join(errs...)
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
