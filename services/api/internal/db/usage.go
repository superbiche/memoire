// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"encoding/json"
	"strings"
)

// EventTypeMemoryUsage is the attribution-at-Stop event
// (docs/rework/retrieval-consumer/attribution-at-stop.md): one per operator
// turn that consulted memory, written by scripts/memoire-attribute-claude.py.
// Its payload carries per-served-id verdicts; Ingest projects them into
// observation_usage (schema/0019) so the review surfaces can show, per
// observation, how the consuming agents said they used it.
const EventTypeMemoryUsage = "memory_usage"

// usageVerdicts is the measured grammar; the CHECK constraint on the table
// mirrors it. An unknown verdict is dropped at projection (the raw event keeps
// it), never widened here.
var usageVerdicts = map[string]struct{}{
	"used": {}, "unused": {}, "misapplied": {}, "contradicted-by-files": {}, "fetch-failed": {},
}

// UsageSummary is the per-observation aggregate shown next to the review
// state. Absent (nil) when no memory_usage event names the observation: an
// absent signal is absent, never "unused" (design reference chapter 07 §3).
type UsageSummary struct {
	Used            int   `json:"used"`
	Unused          int   `json:"unused"`
	Misapplied      int   `json:"misapplied"`
	Contradicted    int   `json:"contradicted_by_files"`
	FetchFailed     int   `json:"fetch_failed"`
	Sessions        int   `json:"sessions"`
	LastAtEpoch     int64 `json:"last_at_epoch"`
	LastUsedAtEpoch int64 `json:"last_used_at_epoch,omitempty"`
}

// UsageLine is one attribution of one observation: the verdict and the
// reason line the agent wrote, with the session and the event it came from,
// so the UI can show the hit next to the claim (the campaign's `used`-not-
// carried errors are claim-level, not grammar-level).
type UsageLine struct {
	EventID         int64  `json:"event_id"`
	SessionID       string `json:"session_id"`
	Project         string `json:"project"`
	Verdict         string `json:"verdict"`
	Reason          string `json:"reason,omitempty"`
	OccurredAtEpoch int64  `json:"occurred_at_epoch"`
}

type memoryUsagePayload struct {
	// ServedIDs is what the turn was actually served, read by the hook from
	// the harness's own tool results. Verdicts come from model text and are
	// untrusted: only a verdict whose id is in ServedIDs is projected.
	ServedIDs []int64 `json:"served_ids"`
	Verdicts  []struct {
		ID      *int64 `json:"id"`
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	} `json:"verdicts"`
}

// projectMemoryUsageTx writes the observation_usage rows of one memory_usage
// event inside the ingest transaction. A verdict is projected only when its id
// is in the event's served_ids (ids named must be a subset of ids served: an
// id the model invented must never become another observation's usage
// signal). Ids that name no observation row are skipped (WHERE EXISTS), duplicates are ignored (unique index + insertIgnore),
// so a queue replay is idempotent. A malformed payload projects nothing and
// does not fail the ingest: the event is kept, the projection is empty.
func (s *Store) projectMemoryUsageTx(ctx context.Context, tx dbtx, eventID int64, in EventInput, payload json.RawMessage, occurred int64) error {
	var p memoryUsagePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	served := make(map[int64]struct{}, len(p.ServedIDs))
	for _, id := range p.ServedIDs {
		served[id] = struct{}{}
	}
	ignorePre, ignoreSuf := insertIgnore(s.engine)
	seen := map[int64]struct{}{}
	for _, v := range p.Verdicts {
		if v.ID == nil || *v.ID <= 0 {
			continue
		}
		if _, ok := served[*v.ID]; !ok {
			continue
		}
		if _, ok := usageVerdicts[v.Verdict]; !ok {
			continue
		}
		if _, dup := seen[*v.ID]; dup {
			continue
		}
		seen[*v.ID] = struct{}{}
		reason := strings.TrimSpace(v.Reason)
		if len(reason) > 2000 {
			reason = reason[:2000]
		}
		if _, err := tx.ExecContext(ctx, `INSERT `+ignorePre+`INTO observation_usage(
			observation_id, event_id, session_id, project, verdict, reason, occurred_at_epoch)
			SELECT ?, ?, ?, ?, ?, ?, ?
			WHERE EXISTS (SELECT 1 FROM observations WHERE id = ?)`+ignoreSuf,
			*v.ID, eventID, in.SessionID, in.Project, v.Verdict, nullString(reason), occurred, *v.ID); err != nil {
			return err
		}
	}
	return nil
}

// AttachUsage fills Usage on the given records from observation_usage in two
// grouped queries over the id set; records with no rows keep Usage nil.
func (s *Store) AttachUsage(ctx context.Context, recs []ObservationRecord) error {
	if len(recs) == 0 {
		return nil
	}
	ids := make([]any, 0, len(recs))
	index := map[int64][]int{}
	for i, r := range recs {
		if _, ok := index[r.ID]; !ok {
			ids = append(ids, r.ID)
		}
		index[r.ID] = append(index[r.ID], i)
	}
	summaries := map[int64]*UsageSummary{}
	get := func(id int64) *UsageSummary {
		if u, ok := summaries[id]; ok {
			return u
		}
		u := &UsageSummary{}
		summaries[id] = u
		return u
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT observation_id, verdict, COUNT(*), MAX(occurred_at_epoch)
		FROM observation_usage WHERE observation_id IN (`+placeholders(len(ids))+`)
		GROUP BY observation_id, verdict`, ids...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, n, last int64
		var verdict string
		if err := rows.Scan(&id, &verdict, &n, &last); err != nil {
			rows.Close()
			return err
		}
		u := get(id)
		switch verdict {
		case "used":
			u.Used = int(n)
			u.LastUsedAtEpoch = last
		case "unused":
			u.Unused = int(n)
		case "misapplied":
			u.Misapplied = int(n)
		case "contradicted-by-files":
			u.Contradicted = int(n)
		case "fetch-failed":
			u.FetchFailed = int(n)
		}
		if last > u.LastAtEpoch {
			u.LastAtEpoch = last
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	rows, err = s.rdb.QueryContext(ctx, `SELECT observation_id, COUNT(DISTINCT session_id)
		FROM observation_usage WHERE observation_id IN (`+placeholders(len(ids))+`)
		GROUP BY observation_id`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		get(id).Sessions = int(n)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id, u := range summaries {
		for _, i := range index[id] {
			recs[i].Usage = u
		}
	}
	return nil
}

// ListObservationUsage returns the attribution lines of one observation,
// newest first, capped at limit (default 100, max 500).
func (s *Store) ListObservationUsage(ctx context.Context, observationID int64, limit int) ([]UsageLine, error) {
	limit = normalizeLimit(limit, 100, 500)
	rows, err := s.rdb.QueryContext(ctx, `SELECT event_id, session_id, project, verdict, reason, occurred_at_epoch
		FROM observation_usage WHERE observation_id = ?
		ORDER BY occurred_at_epoch DESC, event_id DESC LIMIT ?`, observationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageLine{}
	for rows.Next() {
		var l UsageLine
		var reason *string
		if err := rows.Scan(&l.EventID, &l.SessionID, &l.Project, &l.Verdict, &reason, &l.OccurredAtEpoch); err != nil {
			return nil, err
		}
		if reason != nil {
			l.Reason = *reason
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
