// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Derive notify intents (memoire:D165, R-10.1). A client SessionEnd marks
// intent over HTTP instead of running a local derive binary. Q1 ruling:
// sweep-only — recording an intent NEVER triggers immediate derivation; the
// hourly sweep serializes the work. Q2 ruling: intents are SOURCE DATA
// (client statements with timestamps), not a coverage cache — coverage is
// computed from events x derivation_runs (OutstandingDeriveWork below).

// DeriveIntent is the current state of one (project, session) notify intent.
type DeriveIntent struct {
	Project            string `json:"project"`
	SessionID          string `json:"session_id"`
	FirstNotifiedEpoch int64  `json:"first_notified_epoch"`
	LastNotifiedEpoch  int64  `json:"last_notified_epoch"`
	NotifyCount        int64  `json:"notify_count"`
	CompletedAtEpoch   int64  `json:"completed_at_epoch,omitempty"`
	CompletedRunID     string `json:"completed_run_id,omitempty"`
	LatencySeconds     int64  `json:"latency_seconds,omitempty"`
}

// NotifyDeriveIntent records (idempotently) that a session wants derivation.
// Repeat notifies for the same open (project, session_id) collapse into one
// intent with a bumped notify_count — hooks are fail-open and re-fire
// (ratified R-10.1 addition: the surface must be safe under that). A notify
// AFTER completion reopens the intent as fresh: first_notified resets so the
// Q1 latency instrumentation measures the new round, not the old one.
// Every self-reference on the DO UPDATE RHS is table-qualified (memoire:D164
// incident 1: unqualified references are ambiguous against excluded on PG).
func (s *Store) NotifyDeriveIntent(ctx context.Context, project, sessionID string, nowEpoch int64) (DeriveIntent, error) {
	if project == "" || sessionID == "" {
		return DeriveIntent{}, fmt.Errorf("NotifyDeriveIntent: project and session_id are required")
	}
	var out DeriveIntent
	err := s.db.QueryRowContext(ctx, `
INSERT INTO derive_intents(project, session_id, first_notified_epoch, last_notified_epoch, notify_count)
VALUES (?, ?, ?, ?, 1)
ON CONFLICT(project, session_id) DO UPDATE SET
  first_notified_epoch = CASE WHEN derive_intents.completed_at_epoch IS NOT NULL
    THEN excluded.first_notified_epoch ELSE derive_intents.first_notified_epoch END,
  last_notified_epoch = excluded.last_notified_epoch,
  notify_count = CASE WHEN derive_intents.completed_at_epoch IS NOT NULL
    THEN 1 ELSE derive_intents.notify_count + 1 END,
  completed_at_epoch = NULL,
  completed_run_id = NULL,
  latency_seconds = NULL
RETURNING project, session_id, first_notified_epoch, last_notified_epoch, notify_count`,
		project, sessionID, nowEpoch, nowEpoch).Scan(
		&out.Project, &out.SessionID, &out.FirstNotifiedEpoch, &out.LastNotifiedEpoch, &out.NotifyCount)
	if err != nil {
		return DeriveIntent{}, err
	}
	return out, nil
}

// CompleteDeriveIntent closes the open intent for (project, session_id),
// stamping the run that served it and the notify->completion latency (Q1's
// mandatory instrumentation). ok=false when no open intent exists — a swept
// session nobody notified about is the normal case, not an error.
//
// Eligibility (round-2 MED-NEW-1, tightened rounds 3-4): the CURRENT
// attempt's input fetch must STRICTLY postdate the LATEST notify —
// servedAtEpoch is the attempt's own fetch timestamp (same hub clock as
// the notify handler; guard-skip/denylist verdicts use their evaluation
// time), never a run row's original created_at (an in-place retry reuses
// the row but fetches fresh — the old stamp would pin a post-failure
// notify open forever). The predicate guards last_notified_epoch: a
// re-notify DURING a run postdates that run's fetch and must hold the
// intent open (round-4: first_notified alone let an old open intent be
// swallowed by a fetch that predated the fresh ask). Latency still
// journals from first_notified — the oldest unserved ask. Strict `<`
// makes a same-second notify/fetch HOLD and converge on the next tick
// instead of claiming an unprovable ordering.
func (s *Store) CompleteDeriveIntent(ctx context.Context, project, sessionID, runID string, servedAtEpoch, nowEpoch int64) (DeriveIntent, bool, error) {
	var out DeriveIntent
	var latency sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
UPDATE derive_intents SET
  completed_at_epoch = ?,
  completed_run_id = ?,
  latency_seconds = ? - first_notified_epoch
WHERE project = ? AND session_id = ? AND completed_at_epoch IS NULL
  AND last_notified_epoch < ?
RETURNING project, session_id, first_notified_epoch, last_notified_epoch, notify_count, completed_at_epoch, completed_run_id, latency_seconds`,
		nowEpoch, runID, nowEpoch, project, sessionID, servedAtEpoch).Scan(
		&out.Project, &out.SessionID, &out.FirstNotifiedEpoch, &out.LastNotifiedEpoch,
		&out.NotifyCount, &out.CompletedAtEpoch, &out.CompletedRunID, &latency)
	if errors.Is(err, sql.ErrNoRows) {
		return DeriveIntent{}, false, nil
	}
	if err != nil {
		return DeriveIntent{}, false, err
	}
	out.LatencySeconds = latency.Int64
	return out, true, nil
}

// OpenDeriveIntents lists intents not yet served, oldest first — the sweep's
// visibility surface (the sweep still enumerates its own targets; intents do
// not reorder it in R-10.1/2, per the Q1 sweep-only ruling).
func (s *Store) OpenDeriveIntents(ctx context.Context) ([]DeriveIntent, error) {
	rows, err := s.rdb.QueryContext(ctx, `
SELECT project, session_id, first_notified_epoch, last_notified_epoch, notify_count
FROM derive_intents
WHERE completed_at_epoch IS NULL
ORDER BY first_notified_epoch, project, session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeriveIntent
	for rows.Next() {
		var it DeriveIntent
		if err := rows.Scan(&it.Project, &it.SessionID, &it.FirstNotifiedEpoch, &it.LastNotifiedEpoch, &it.NotifyCount); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// MaxEventID returns the store's current MAX(events.id) — read by the
// orchestrator BEFORE its input fetch to bound it, and recorded on the run
// as a DIAGNOSTIC (memoire:D165 round-2 HIGH-1). Coverage does NOT use it:
// a PG identity id is allocated before its transaction commits, so a
// high-water id cannot prove snapshot membership (round-3) — the
// run_input_events manifest is the authority. Zero on an empty store.
func (s *Store) MaxEventID(ctx context.Context) (int64, error) {
	var maxID sql.NullInt64
	if err := s.rdb.QueryRowContext(ctx, `SELECT MAX(id) FROM events`).Scan(&maxID); err != nil {
		return 0, err
	}
	return maxID.Int64, nil
}

// RecordRunInputEvents records the run's input manifest — the EXACT event
// ids its fetch returned (memoire:D165 round-3 HIGH-1). Idempotent
// (INSERT-ignore on the PK): retry claims and completed-dedup upgrades
// re-record safely, and a crash mid-manifest converges on the next claim.
// Called with the identical fetched set on a completed dedup too — the
// matching input_hash PROVES the sets are identical, so upgrading a
// pre-0015 run with a manifest is exact. Attaching the manifest is the
// ONLY way a manifest-less completed run begins to provide coverage: no
// timestamp/id heuristic exists (rounds 2-4 rejected them all).
func (s *Store) RecordRunInputEvents(ctx context.Context, runID string, eventIDs []int64) error {
	if runID == "" {
		return fmt.Errorf("RecordRunInputEvents: runID is required")
	}
	if len(eventIDs) == 0 {
		return nil
	}
	// Chunked (round-4 LOW-NEW-1): PG's bind protocol caps a statement at
	// 65535 parameters and SQLite has its own variable ceiling — a large
	// manual window must not wedge the run in a claim/fail/reclaim loop.
	// INSERT-ignore keeps the whole operation idempotent, so per-chunk
	// statements need no wrapping transaction: a crash mid-manifest
	// converges on the next claim's re-record.
	const chunk = 500
	ignorePre, ignoreSuf := insertIgnore(s.engine)
	for start := 0; start < len(eventIDs); start += chunk {
		end := start + chunk
		if end > len(eventIDs) {
			end = len(eventIDs)
		}
		var sb strings.Builder
		args := make([]any, 0, 2*(end-start))
		sb.WriteString(`INSERT ` + ignorePre + `INTO run_input_events(run_id, event_id) VALUES `)
		for i, id := range eventIDs[start:end] {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?)")
			args = append(args, runID, id)
		}
		sb.WriteString(ignoreSuf)
		if _, err := s.db.ExecContext(ctx, sb.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// SettleNoWorkIntents closes open intents older than beforeEpoch whose
// (project, session) holds ZERO live canonical events — sessions that never
// produced derivable work (capture failed entirely, or only non-canonical
// events): no run or guard skip can ever settle them, so left alone they sit
// in OpenDeriveIntents forever with no latency datum (round-1 MED-4;
// policy RATIFIED by the operator 2026-07-23: 48h default TTL, and
// intents for sessions WITH events deliberately never age out — an
// outage-stalled intent is the truthful open alarm. The TTL is the
// caller's knob). Closed rows carry completed_run_id 'expired:no_events'
// and NULL latency: an expiry is not a served intent and must never pollute
// the Q1 latency journal. The age gate means a slow thin-client forward
// still has the whole TTL to land events before an intent can expire.
func (s *Store) SettleNoWorkIntents(ctx context.Context, beforeEpoch, nowEpoch int64, eventTypes []string) (int64, error) {
	if len(eventTypes) == 0 {
		return 0, fmt.Errorf("SettleNoWorkIntents: empty eventTypes")
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(eventTypes)), ",")
	args := make([]any, 0, len(eventTypes)+2)
	args = append(args, nowEpoch, beforeEpoch)
	for _, et := range eventTypes {
		args = append(args, et)
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE derive_intents SET
  completed_at_epoch = ?,
  completed_run_id = 'expired:no_events',
  latency_seconds = NULL
WHERE completed_at_epoch IS NULL
  AND first_notified_epoch < ?
  AND NOT EXISTS (
    SELECT 1 FROM events e
    WHERE e.project = derive_intents.project
      AND e.session_id = derive_intents.session_id
      AND e.deleted_at_epoch IS NULL
      AND e.event_type IN (`+placeholders+`)
  )`, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RunObservationContentHashes returns the content hashes of the LIVE
// observations already attributed to a run WITH their multiplicities — the
// replay-idempotency guard for retried attempts (memoire:D165 rounds 6-7):
// observations commit one at a time, so an attempt that failed mid-way must
// skip exactly the candidates its predecessor landed. A MULTISET, not a
// boolean set (round-7): content hash is semantic identity only —
// same-content candidates from distinct source events are deliberate
// separate rows, so the retry consumes one committed occurrence per
// candidate in deterministic order instead of collapsing them.
func (s *Store) RunObservationContentHashes(ctx context.Context, runID string) (map[string]int, error) {
	rows, err := s.rdb.QueryContext(ctx, `
SELECT COALESCE(content_hash, '') FROM observations
WHERE derivation_run_id = ? AND deleted_at_epoch IS NULL`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		if h != "" {
			out[h]++
		}
	}
	return out, rows.Err()
}

// OutstandingWork is one (project, session) with live canonical events not
// covered by any completed derivation run window — computed from source on
// every call (Q2 ruling: no coverage table; "a cache that can lie").
// UncoveredBytes approximates prompt weight (payload + text lengths) so
// callers like the D163 HL5 trigger can classify size without the store
// claiming a size policy.
type OutstandingWork struct {
	Project         string `json:"project"`
	SessionID       string `json:"session_id"`
	UncoveredEvents int64  `json:"uncovered_events"`
	UncoveredBytes  int64  `json:"uncovered_bytes"`
	OldestEpoch     int64  `json:"oldest_epoch"`
	NewestEpoch     int64  `json:"newest_epoch"`
}

// OutstandingDeriveWork returns, per (project, session), the live canonical
// events since sinceEpoch that no completed derivation run window covers.
// eventTypes is the caller's canonical-input list
// (modelderive.CanonicalInputEventTypes) — same contract as
// MedianCanonicalEpoch: the admission set lives with the prompt builder.
//
// Coverage requires the event to have been IN the run's input (round-1
// HIGH-1, exact since rounds 3-4): a completed run covers exactly its
// recorded input manifest (run_input_events) — set membership, immune to
// id-allocation-vs-commit ordering (a PG identity id is allocated before
// its transaction commits, so no id/timestamp comparison can prove
// snapshot membership), queue-time received_at stamping, and clock ties.
// Manifest-less runs (pre-0015 rows, salted forced legacies) provide NO
// coverage — round 4 proved every timestamp heuristic unsound, so absence
// of evidence reads as outstanding, never as covered. That is self-healing
// for windows sweeps still visit: the next identical-input_hash fetch
// dedups to the old row and attaches its manifest (RecordRunInputEvents;
// every completed-run writer records — model orchestrator AND rules path).
// The one-time cost after deploy is honest re-reporting of recently-derived
// sessions until their first post-0015 sweep tick upgrades them.
//
// Row semantics (of record, round-1 MED-1): a row is a per-(project,
// session) AGGREGATE of uncovered events — count, byte weight, and the
// bounds of the UNCOVERED set. It is NOT a derivable window: covered
// islands may lie inside [oldest, newest], and consumers must never build
// a prompt from these bounds. Windowing/splitting stays with the sweep's
// own split logic; size classification uses UncoveredBytes.
func (s *Store) OutstandingDeriveWork(ctx context.Context, sinceEpoch int64, eventTypes []string) ([]OutstandingWork, error) {
	if len(eventTypes) == 0 {
		return nil, fmt.Errorf("OutstandingDeriveWork: empty eventTypes")
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(eventTypes)), ",")
	args := make([]any, 0, len(eventTypes)+1)
	args = append(args, sinceEpoch)
	for _, et := range eventTypes {
		args = append(args, et)
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT e.project, e.session_id,
  COUNT(*),
  SUM(LENGTH(e.payload_json) + LENGTH(COALESCE(e.text, ''))),
  MIN(e.occurred_at_epoch), MAX(e.occurred_at_epoch)
FROM events e
WHERE e.deleted_at_epoch IS NULL
  AND e.occurred_at_epoch >= ?
  AND e.session_id IS NOT NULL AND e.session_id != ''
  AND e.project IS NOT NULL AND e.project != ''
  AND e.event_type IN (`+placeholders+`)
  AND NOT EXISTS (
    SELECT 1 FROM derivation_runs r
    JOIN run_input_events m ON m.run_id = r.id AND m.event_id = e.id
    WHERE r.project = e.project
      AND r.session_id = e.session_id
      AND r.status = 'completed'
  )
GROUP BY e.project, e.session_id
ORDER BY e.project, e.session_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutstandingWork
	for rows.Next() {
		var w OutstandingWork
		if err := rows.Scan(&w.Project, &w.SessionID, &w.UncoveredEvents, &w.UncoveredBytes, &w.OldestEpoch, &w.NewestEpoch); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
