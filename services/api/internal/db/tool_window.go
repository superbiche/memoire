// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// defaultToolWindowEventType is the Claude PostToolUse hook event. It is the
// event class that carries tool_name / tool_input / tool_response, i.e. the
// "tool events" a window query is about. Callers may override via EventType.
const defaultToolWindowEventType = "claude_PostToolUse"

// ToolEventFilter scopes a window query over raw tool events. session_id is
// required and indexed (idx_events_session); occurred_at_epoch in
// [StartEpoch, EndEpoch] is inclusive and indexed (idx_events_type_occurred).
// AgentID is OPTIONAL and NON-INDEXED — it is matched via
// json_extract(payload_json,'$.agent_id') after the indexed scan, so callers
// must narrow by session_id (and ideally the time window) first.
type ToolEventFilter struct {
	SessionID  string `json:"session_id"`
	StartEpoch int64  `json:"start_epoch,omitempty"`
	EndEpoch   int64  `json:"end_epoch,omitempty"`
	AgentID    string `json:"agent_id,omitempty"`
	Project    string `json:"project,omitempty"`
	EventType  string `json:"event_type,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

// ToolEventRecord is a RAW tool event returned by a window query. It is a dumb
// substrate row — no task-shape tuple, no verdict, no scoring. tool_input and
// tool_response are passed through as raw JSON; Liaison does the artifact match
// and any interpretation. PathsTouched is a best-effort convenience only.
type ToolEventRecord struct {
	ID              int64           `json:"id"`
	SessionID       string          `json:"session_id"`
	AgentID         string          `json:"agent_id,omitempty"`
	EventType       string          `json:"event_type"`
	ToolName        string          `json:"tool_name,omitempty"`
	ToolInput       json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse    json.RawMessage `json:"tool_response,omitempty"`
	CWD             string          `json:"cwd,omitempty"`
	OccurredAtEpoch int64           `json:"occurred_at_epoch"`
	// PathsTouched is a BEST-EFFORT, NON-AUTHORITATIVE convenience derived from
	// known tool shapes (Write/Edit tool_input.file_path; Read
	// tool_response.file.filePath). Liaison remains responsible for the
	// authoritative artifact match. Omitted when no path is derivable.
	PathsTouched []string `json:"paths_touched,omitempty"`
}

// ErrInvalidFilter marks a read filter the store rejects by its own
// validation (a client error). invalidFilterError keeps the bare message as
// its text and matches this sentinel through errors.Is.
var ErrInvalidFilter = errors.New("invalid filter")

type invalidFilterError struct{ msg string }

func (e invalidFilterError) Error() string        { return e.msg }
func (e invalidFilterError) Is(target error) bool { return target == ErrInvalidFilter }

// ListToolEventsInWindow returns RAW tool events filtered by session_id
// (required), an inclusive occurred_at_epoch window, and optional agent_id /
// project. Results are ordered by occurred_at_epoch ASC (then id ASC for a
// stable order) and capped by a sane limit (parseLimit-style normalization).
//
// Contract boundary: this returns the window's raw events only. It never emits
// a task-shape tuple, a verdict, or a score. The optional PathsTouched field is
// a best-effort convenience, not an authoritative artifact list.
func (s *Store) ListToolEventsInWindow(ctx context.Context, filter ToolEventFilter) ([]ToolEventRecord, error) {
	if strings.TrimSpace(filter.SessionID) == "" {
		return nil, invalidFilterError{"session_id is required"}
	}
	if filter.StartEpoch != 0 && filter.EndEpoch != 0 && filter.EndEpoch < filter.StartEpoch {
		return nil, invalidFilterError{"end must be >= start"}
	}

	eventType := filter.EventType
	if eventType == "" {
		eventType = defaultToolWindowEventType
	}

	clauses := []string{"session_id = ?", "event_type = ?", "deleted_at_epoch IS NULL"}
	args := []any{filter.SessionID, eventType}

	if filter.StartEpoch != 0 {
		clauses = append(clauses, "occurred_at_epoch >= ?")
		args = append(args, filter.StartEpoch)
	}
	if filter.EndEpoch != 0 {
		clauses = append(clauses, "occurred_at_epoch <= ?")
		args = append(args, filter.EndEpoch)
	}
	if filter.Project != "" {
		clauses = append(clauses, "project = ?")
		args = append(args, filter.Project)
	}
	if filter.AgentID != "" {
		// Non-indexed: applied after the session_id + time-window index scan.
		if s.engine == EnginePostgres {
			clauses = append(clauses, "(payload_json::jsonb ->> 'agent_id') = ?")
		} else {
			clauses = append(clauses, "json_extract(payload_json, '$.agent_id') = ?")
		}
		args = append(args, filter.AgentID)
	}

	limit := normalizeLimit(filter.Limit, 100, 500)
	args = append(args, limit)

	rows, err := s.rdb.QueryContext(ctx, `SELECT id, session_id, event_type, cwd, payload_json, occurred_at_epoch
		FROM events
		WHERE `+strings.Join(clauses, " AND ")+`
		ORDER BY occurred_at_epoch ASC, id ASC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ToolEventRecord{}
	for rows.Next() {
		var rec ToolEventRecord
		var cwd, payload sql.NullString
		if err := rows.Scan(&rec.ID, &rec.SessionID, &rec.EventType, &cwd, &payload, &rec.OccurredAtEpoch); err != nil {
			return nil, err
		}
		rec.CWD = cwd.String
		hydrateToolEvent(&rec, payload.String)
		out = append(out, rec)
	}
	return out, rows.Err()
}

// hydrateToolEvent pulls the tool-shaped fields out of the raw payload JSON.
// agent_id, tool_name, tool_input, tool_response are passed through as-is; the
// best-effort paths_touched is derived from known tool shapes.
func hydrateToolEvent(rec *ToolEventRecord, payload string) {
	if payload == "" {
		return
	}
	var p struct {
		AgentID      string          `json:"agent_id"`
		ToolName     string          `json:"tool_name"`
		ToolNameAlt  string          `json:"toolName"`
		ToolInput    json.RawMessage `json:"tool_input"`
		ToolResponse json.RawMessage `json:"tool_response"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return
	}
	rec.AgentID = p.AgentID
	rec.ToolName = p.ToolName
	if rec.ToolName == "" {
		rec.ToolName = p.ToolNameAlt
	}
	rec.ToolInput = nonEmptyRaw(p.ToolInput)
	rec.ToolResponse = nonEmptyRaw(p.ToolResponse)
	rec.PathsTouched = derivePathsTouched(p.ToolInput, p.ToolResponse)
}

func nonEmptyRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

// derivePathsTouched is a BEST-EFFORT path extractor over known tool shapes:
//   - Write/Edit: tool_input.file_path
//   - Read:       tool_response.file.filePath (fallback tool_input.file_path)
//
// It is intentionally conservative and never authoritative. Returns nil when no
// path is derivable so the field is omitted from the JSON.
func derivePathsTouched(toolInput, toolResponse json.RawMessage) []string {
	seen := map[string]struct{}{}
	var paths []string
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}

	if len(toolInput) > 0 {
		var in struct {
			FilePath string `json:"file_path"`
		}
		if err := json.Unmarshal(toolInput, &in); err == nil {
			add(in.FilePath)
		}
	}
	if len(toolResponse) > 0 {
		var resp struct {
			File struct {
				FilePath string `json:"filePath"`
			} `json:"file"`
		}
		if err := json.Unmarshal(toolResponse, &resp); err == nil {
			add(resp.File.FilePath)
		}
	}
	return paths
}
