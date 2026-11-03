// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"memoire/services/api/internal/db"
)

// Read telemetry (L0009 A1): successful reads are counted by route and raw
// scope, returned items by kind class and age, and /metrics carries only the
// closed vocabularies — never a project, session or query string.
func TestReadTelemetryOnMetrics(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	occurred := time.Now().Add(-40 * 24 * time.Hour).Unix()
	if _, err := store.Ingest(t.Context(), db.EventInput{
		SchemaVersion:   1,
		Project:         "telemetry-private-project",
		SessionID:       "telemetry-private-session",
		SourceAdapter:   "telemetry-adapter",
		SourceEventID:   "e1",
		EventType:       "note",
		Payload:         json.RawMessage(`{"text":"telemetry-private-text"}`),
		OccurredAtEpoch: occurred,
		ReceivedAtEpoch: occurred,
	}); err != nil {
		t.Fatal(err)
	}

	read := func(method, target, body string) {
		t.Helper()
		assertStatus(t, handler, method, target, "secret-token", body, http.StatusOK)
	}
	read(http.MethodGet, "/v1/timeline?project=telemetry-private-project", "")
	read(http.MethodGet, "/v1/timeline?project=telemetry-private-project&exclude_kind=raw&include_derived=true", "")
	read(http.MethodGet, "/v1/search?q=telemetry&exclude_kind=raw&include_derived=true", "")
	read(http.MethodPost, "/v1/context", `{"q":"telemetry","kind":"raw"}`)
	read(http.MethodGet, "/v1/sessions", "")
	// A failed read is not counted.
	assertStatus(t, handler, http.MethodGet, "/v1/observations/999999", "secret-token", "", http.StatusNotFound)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE memoire_read_requests_total counter",
		`memoire_read_requests_total{route="timeline",raw="included"} 1`,
		`memoire_read_requests_total{route="timeline",raw="excluded"} 1`,
		`memoire_read_requests_total{route="search",raw="excluded"} 1`,
		`memoire_read_requests_total{route="context",raw="included"} 1`,
		`memoire_read_requests_total{route="session_list",raw="n_a"} 1`,
		`memoire_read_requests_total{route="observation_get",raw="included"} 0`,
		`memoire_read_items_total{route="timeline",kind="raw",age="30d_90d"} 1`,
		`memoire_read_items_total{route="timeline",kind="derived",age="30d_90d"} 0`,
		`memoire_read_items_total{route="search",kind="raw",age="30d_90d"} 0`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("/metrics lacks %q", want)
		}
	}
	for _, leak := range []string{"telemetry-private", "telemetry-adapter"} {
		if strings.Contains(body, leak) {
			t.Errorf("/metrics leaks stored string %q", leak)
		}
	}
	if strings.Contains(body, `route="session_list",raw="included"`) || strings.Contains(body, `route="timeline",raw="n_a"`) {
		t.Error("raw scope n_a must be emitted for session routes only")
	}
}
