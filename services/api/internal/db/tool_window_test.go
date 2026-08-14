// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

// seedToolEvent ingests a claude_PostToolUse-shaped event so window queries have
// raw rows to read. payload is the full hook payload (top-level tool_name,
// tool_input, tool_response, agent_id, mirroring scripts/memoire-capture-claude.sh).
func seedToolEvent(t *testing.T, store *Store, sourceEventID string, occurred int64, payload string) int64 {
	t.Helper()
	res, err := store.Ingest(context.Background(), EventInput{
		Project:         "memoire-test",
		SessionID:       "window-session",
		SourceAdapter:   "claude-code",
		SourceEventID:   sourceEventID,
		EventType:       "claude_PostToolUse",
		OccurredAtEpoch: occurred,
		Payload:         json.RawMessage(payload),
		Text:            "tool event " + sourceEventID,
	})
	if err != nil {
		t.Fatalf("seed %s: %v", sourceEventID, err)
	}
	return res.ID
}

func TestListToolEventsInWindow(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	// Events at t=100,200,300 in the target session.
	seedToolEvent(t, store, "ev-100", 100, `{"tool_name":"Read","agent_id":"agent-a","tool_input":{"file_path":"/repo/a.go"},"tool_response":{"file":{"filePath":"/repo/a.go"}}}`)
	seedToolEvent(t, store, "ev-200", 200, `{"tool_name":"Write","agent_id":"agent-b","tool_input":{"file_path":"/repo/b.go"},"tool_response":{"ok":true}}`)
	seedToolEvent(t, store, "ev-300", 300, `{"tool_name":"Bash","agent_id":"agent-a","tool_input":{"command":"ls"},"tool_response":{"stdout":"x"}}`)
	// Out-of-session event must never surface.
	if _, err := store.Ingest(ctx, EventInput{
		Project: "memoire-test", SessionID: "other-session", SourceAdapter: "claude-code",
		SourceEventID: "ev-other", EventType: "claude_PostToolUse", OccurredAtEpoch: 200,
		Payload: json.RawMessage(`{"tool_name":"Read","agent_id":"agent-a"}`), Text: "other",
	}); err != nil {
		t.Fatal(err)
	}

	// Window [100,300] inclusive returns all three, ordered ascending.
	got, err := store.ListToolEventsInWindow(ctx, ToolEventFilter{SessionID: "window-session", StartEpoch: 100, EndEpoch: 300})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("inclusive window: want 3 events, got %d", len(got))
	}
	if got[0].OccurredAtEpoch != 100 || got[2].OccurredAtEpoch != 300 {
		t.Fatalf("ordering wrong: %d..%d", got[0].OccurredAtEpoch, got[2].OccurredAtEpoch)
	}

	// Boundary inclusivity: [200,300] keeps both endpoints, drops t=100.
	got, err = store.ListToolEventsInWindow(ctx, ToolEventFilter{SessionID: "window-session", StartEpoch: 200, EndEpoch: 300})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].OccurredAtEpoch != 200 {
		t.Fatalf("boundary inclusivity: want [200,300], got %+v", epochs(got))
	}

	// agent_id filter (non-indexed json_extract): agent-a matches t=100 and t=300.
	got, err = store.ListToolEventsInWindow(ctx, ToolEventFilter{SessionID: "window-session", StartEpoch: 100, EndEpoch: 300, AgentID: "agent-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("agent_id filter: want 2, got %d (%+v)", len(got), epochs(got))
	}
	for _, e := range got {
		if e.AgentID != "agent-a" {
			t.Fatalf("agent_id leak: %s", e.AgentID)
		}
	}

	// Raw passthrough + best-effort paths_touched.
	read := got[0] // t=100 Read
	if read.ToolName != "Read" {
		t.Fatalf("tool_name = %q", read.ToolName)
	}
	if string(read.ToolInput) == "" || string(read.ToolResponse) == "" {
		t.Fatal("raw tool_input/tool_response not passed through")
	}
	if len(read.PathsTouched) != 1 || read.PathsTouched[0] != "/repo/a.go" {
		t.Fatalf("Read paths_touched = %v", read.PathsTouched)
	}

	// Write derives paths_touched from tool_input.file_path.
	writeEvents, err := store.ListToolEventsInWindow(ctx, ToolEventFilter{SessionID: "window-session", StartEpoch: 200, EndEpoch: 200, AgentID: "agent-b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(writeEvents) != 1 || len(writeEvents[0].PathsTouched) != 1 || writeEvents[0].PathsTouched[0] != "/repo/b.go" {
		t.Fatalf("Write paths_touched = %+v", writeEvents)
	}

	// Deleted events are excluded.
	id300 := mustEventID(t, store, "window-session", 300)
	if err := store.DeleteEvent(ctx, id300); err != nil {
		t.Fatal(err)
	}
	got, err = store.ListToolEventsInWindow(ctx, ToolEventFilter{SessionID: "window-session", StartEpoch: 100, EndEpoch: 300})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("deleted exclusion: want 2, got %d (%+v)", len(got), epochs(got))
	}

	// Missing session_id is an error.
	if _, err := store.ListToolEventsInWindow(ctx, ToolEventFilter{StartEpoch: 100, EndEpoch: 300}); err == nil {
		t.Fatal("expected error for missing session_id")
	}
}

func epochs(recs []ToolEventRecord) []int64 {
	out := make([]int64, len(recs))
	for i, r := range recs {
		out[i] = r.OccurredAtEpoch
	}
	return out
}

func mustEventID(t *testing.T, store *Store, session string, occurred int64) int64 {
	t.Helper()
	var id int64
	if err := store.db.QueryRow(`SELECT id FROM events WHERE session_id = ? AND occurred_at_epoch = ? AND deleted_at_epoch IS NULL`, session, occurred).Scan(&id); err != nil {
		t.Fatalf("lookup event id: %v", err)
	}
	return id
}
