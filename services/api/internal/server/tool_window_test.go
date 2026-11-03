// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"memoire/services/api/internal/db"
)

func seedToolWindowEvents(t *testing.T, store *db.Store) {
	t.Helper()
	events := []struct {
		sourceEventID string
		occurred      int64
		payload       string
	}{
		{"win-100", 100, `{"tool_name":"Read","agent_id":"agent-a","tool_input":{"file_path":"/repo/a.go"},"tool_response":{"file":{"filePath":"/repo/a.go"}}}`},
		{"win-200", 200, `{"tool_name":"Write","agent_id":"agent-b","tool_input":{"file_path":"/repo/b.go"},"tool_response":{"ok":true}}`},
		{"win-300", 300, `{"tool_name":"Bash","agent_id":"agent-a","tool_input":{"command":"ls"},"tool_response":{"stdout":"x"}}`},
	}
	for _, e := range events {
		if _, err := store.Ingest(context.Background(), db.EventInput{
			Project:         "memoire-test",
			SessionID:       "win-session",
			SourceAdapter:   "claude-code",
			SourceEventID:   e.sourceEventID,
			EventType:       "claude_PostToolUse",
			OccurredAtEpoch: e.occurred,
			Payload:         json.RawMessage(e.payload),
			Text:            "tool event " + e.sourceEventID,
		}); err != nil {
			t.Fatalf("seed %s: %v", e.sourceEventID, err)
		}
	}
}

func TestToolWindowEndpoint(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	if _, err := store.EnsureTokenCredential(t.Context(), "read-token", "read-only", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	seedToolWindowEvents(t, store)

	// Read scope is sufficient; events in the window are returned with raw fields.
	assertBodyContains(t, handler, http.MethodGet, "/v1/events/tool-window?session_id=win-session&start=100&end=300", "read-token", "", http.StatusOK, `"tool_name":"Write"`)
	assertBodyContains(t, handler, http.MethodGet, "/v1/events/tool-window?session_id=win-session&start=100&end=300", "read-token", "", http.StatusOK, `"paths_touched":["/repo/a.go"]`)
	assertBodyContains(t, handler, http.MethodGet, "/v1/events/tool-window?session_id=win-session&start=100&end=300", "read-token", "", http.StatusOK, `"total":3`)

	// Boundary window [200,300] drops t=100.
	assertBodyContains(t, handler, http.MethodGet, "/v1/events/tool-window?session_id=win-session&start=200&end=300", "read-token", "", http.StatusOK, `"total":2`)

	// agent_id filter narrows within the session.
	assertBodyContains(t, handler, http.MethodGet, "/v1/events/tool-window?session_id=win-session&start=100&end=300&agent_id=agent-b", "read-token", "", http.StatusOK, `"total":1`)

	// Auth: no token is unauthorized; a write-only token lacks read scope.
	assertStatus(t, handler, http.MethodGet, "/v1/events/tool-window?session_id=win-session", "", "", http.StatusUnauthorized)
	if _, err := store.EnsureTokenCredential(t.Context(), "write-token", "write-only", []string{"write"}); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, handler, http.MethodGet, "/v1/events/tool-window?session_id=win-session", "write-token", "", http.StatusForbidden)

	// 400 on missing session_id and on a non-numeric epoch.
	assertStatus(t, handler, http.MethodGet, "/v1/events/tool-window?start=100&end=300", "read-token", "", http.StatusBadRequest)
	assertStatus(t, handler, http.MethodGet, "/v1/events/tool-window?session_id=win-session&start=notanumber", "read-token", "", http.StatusBadRequest)
}

// end < start is a client error: 400 with the store message, not a store fault.
func TestToolWindowInvertedRangeStays400(t *testing.T) {
	handler, _ := testServer(t, "read-token")
	assertBodyContains(t, handler, http.MethodGet, "/v1/events/tool-window?session_id=s&start=300&end=100", "read-token", "", http.StatusBadRequest, "end must be >= start")
}
