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

// DeriveTarget is one (project, session) pair with event activity inside the
// sweep's recency window — the enumeration unit of the derivation sweep.
// These queries used to live in scripts/memoire-derive-sweep.sh as raw
// sqlite3 invocations; the flip to the Postgres hub (memoire:D159 window,
// D161) moved them behind the dialect seam so the sweep works against either
// engine through the memoire-derive binary.
type DeriveTarget struct {
	Project   string
	SessionID string
}

// RecentDeriveTargets returns the distinct (project, session_id) pairs with
// at least one event after sinceEpoch. Matches the historical sweep
// enumeration exactly (no deleted_at filter: a session whose recent events
// were soft-deleted still had activity; derivation-side guards decide what
// to do with it). Ordered for deterministic sweep logs.
func (s *Store) RecentDeriveTargets(ctx context.Context, sinceEpoch int64) ([]DeriveTarget, error) {
	rows, err := s.rdb.QueryContext(ctx, `
SELECT project, session_id FROM events
WHERE occurred_at_epoch > ?
  AND session_id IS NOT NULL AND session_id != ''
  AND project IS NOT NULL AND project != ''
GROUP BY project, session_id
ORDER BY project, session_id`, sinceEpoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeriveTarget
	for rows.Next() {
		var t DeriveTarget
		if err := rows.Scan(&t.Project, &t.SessionID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// MedianCanonicalEpoch returns the median occurred_at_epoch of the session's
// live canonical-input events inside [lo, hi] — the adaptive-split point for
// an oversized derivation window. ok is false when the window holds no
// matching events. eventTypes is the caller's canonical-input list
// (modelderive.CanonicalInputEventTypes); it lives with the prompt builder,
// not here, so the split always sees exactly the events the builder admits.
func (s *Store) MedianCanonicalEpoch(ctx context.Context, project, sessionID string, lo, hi int64, eventTypes []string) (int64, bool, error) {
	if len(eventTypes) == 0 {
		return 0, false, fmt.Errorf("MedianCanonicalEpoch: empty eventTypes")
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(eventTypes)), ",")
	where := `project = ? AND session_id = ?
  AND deleted_at_epoch IS NULL
  AND event_type IN (` + placeholders + `)
  AND occurred_at_epoch >= ? AND occurred_at_epoch <= ?`
	args := make([]any, 0, 2*(len(eventTypes)+4))
	appendWhereArgs := func() {
		args = append(args, project, sessionID)
		for _, et := range eventTypes {
			args = append(args, et)
		}
		args = append(args, lo, hi)
	}
	appendWhereArgs()
	appendWhereArgs()
	// LIMIT 1 OFFSET (COUNT-1)/2 — the same scalar-subquery OFFSET both
	// engines accept; on an empty window the OFFSET subquery yields -1/2 = 0
	// and the outer query returns no rows (ok=false), never an error.
	query := `
SELECT occurred_at_epoch FROM events
WHERE ` + where + `
ORDER BY occurred_at_epoch, id
LIMIT 1 OFFSET (
  SELECT (COUNT(*) - 1) / 2 FROM events
  WHERE ` + where + `
)`
	var epoch int64
	err := s.rdb.QueryRowContext(ctx, query, args...).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return epoch, true, nil
}
