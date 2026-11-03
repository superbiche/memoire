// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"

	_ "github.com/mattn/go-sqlite3"
)

// GET /metrics is the unauthenticated Prometheus scrape surface (infra seam
// ruling (b), inbox 2026-08-03). This pins the whole contract: no token
// required, text exposition format, the cheap series always present, the
// vector-lane series absent on a store without the lane, the derive-run
// family exposed as a GAUGE under the closed status vocabulary with NO
// ledger-sourced string in any label (review 2026-08-03 F1/F2), a canceled
// scrape context still served (F3) — and the auth boundary for everything
// else unmoved, including path-spelling variants of /metrics itself.
func TestMetricsUnauthenticatedSurface(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{
		Addr:             "127.0.0.1:0",
		Home:             home,
		DBPath:           filepath.Join(home, "memoire.db"),
		SchemaDir:        "../../../../schema",
		TokenPath:        filepath.Join(home, "token"),
		Raw:              filepath.Join(home, "raw"),
		SkewAlertSeconds: 60,
		Queue: config.QueuePaths{
			Root:     filepath.Join(home, "queue"),
			Pending:  filepath.Join(home, "queue", "pending"),
			Inflight: filepath.Join(home, "queue", "inflight"),
			Failed:   filepath.Join(home, "queue", "failed"),
			Log:      filepath.Join(home, "log"),
		},
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(cfg.DBPath, cfg.SchemaDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	base := int64(1_800_000_000)
	if _, err := store.Ingest(t.Context(), db.EventInput{
		SchemaVersion:   1,
		Project:         "metrics-test",
		SessionID:       "metrics-session",
		SourceAdapter:   "metrics-adapter",
		SourceEventID:   "future-fault",
		EventType:       "note",
		Payload:         json.RawMessage(`{}`),
		OccurredAtEpoch: base + 3600,
		ReceivedAtEpoch: base,
	}); err != nil {
		t.Fatal(err)
	}
	// A run whose endpoint_id is an operator-private string and whose status
	// carries a suffix: the family must fold status to its class and must
	// never emit the endpoint string (F1).
	if _, err := store.EnsureDerivationRun(t.Context(), db.DerivationRunInput{
		Project:           "metrics-test",
		Provider:          "local-qwen",
		Model:             "test-model",
		EndpointID:        "rack-private-codename",
		DataBoundary:      "local",
		DerivationVersion: "v1",
		InputHash:         "metrics-test-hash",
		WindowStartEpoch:  base,
		WindowEndEpoch:    base + 60,
		Status:            "skipped:project_denied",
	}); err != nil {
		t.Fatal(err)
	}
	// The memoire:D222 shape: a failed run whose stored failure text is the
	// client-side registry-allowlist rejection, naming the private endpoint.
	// The genre must surface; the error text must not.
	if _, err := store.EnsureDerivationRun(t.Context(), db.DerivationRunInput{
		Project:           "metrics-test",
		Provider:          "local-qwen",
		Model:             "test-model",
		EndpointID:        "rack-private-codename",
		DataBoundary:      "local",
		DerivationVersion: "v1",
		InputHash:         "metrics-test-hash-failed",
		WindowStartEpoch:  base,
		WindowEndEpoch:    base + 60,
		Status:            "failed",
		Metadata: json.RawMessage(`{"failure":{"stage":"provider_call",` +
			`"error":"modelderive: model \"test-model\" is not allowlisted for endpoint \"rack-private-codename\""}}`),
	}); err != nil {
		t.Fatal(err)
	}

	handler := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A canceled request context must still be served from the shared cache
	// path — client cancellation can never poison later scrapes (F3).
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil).WithContext(canceled))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("metrics content-type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"memoire_queue_depth 0\n",
		"memoire_queue_failed_depth 0\n",
		"memoire_queue_oldest_age_seconds 0\n",
		"memoire_redaction_contract_violations_total 0\n",
		"memoire_clock_skew_future_events 1\n",
		"memoire_search_engine_info{engine=\"fts5\"} 1\n",
		"memoire_vector_lane_on 0\n",
		"memoire_hybrid_search_on 0\n",
		"memoire_hybrid_fail_open_total 0\n",
		"memoire_hybrid_literal_skips_total 0\n",
		"# TYPE memoire_derive_runs gauge\n",
		"memoire_derive_runs{status=\"skipped\"} 1\n",
		// The memoire:D222 observable: the repeating failure GENRE is
		// visible, every genre cell is present so an alert never has to
		// cope with an appearing/disappearing series, and the streak
		// counts the one failed run.
		"# TYPE memoire_derive_failed_runs_recent gauge\n",
		"memoire_derive_failed_runs_recent{genre=\"client_config\"} 1\n",
		"memoire_derive_failed_runs_recent{genre=\"provider_http\"} 0\n",
		"memoire_derive_failed_runs_recent{genre=\"provider_transport\"} 0\n",
		"memoire_derive_failed_runs_recent{genre=\"provider_response\"} 0\n",
		"memoire_derive_failed_runs_recent{genre=\"parse\"} 0\n",
		"memoire_derive_failed_runs_recent{genre=\"refusal\"} 0\n",
		"memoire_derive_failed_runs_recent{genre=\"unknown\"} 0\n",
		"memoire_derive_consecutive_failed_runs 1\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q\nbody:\n%s", want, body)
		}
	}
	// No ledger-sourced string may cross the unauthenticated boundary (F1),
	// the family must not claim counter semantics (F2), and on a SQLite
	// store the vector-lane series must be ABSENT, not fabricated zeros.
	for _, absent := range []string{
		"rack-private-codename",
		"project_denied",
		"endpoint_id=",
		// The classifier reads the stored failure text; it must never
		// forward a byte of it (D224).
		"modelderive:",
		"not allowlisted",
		"memoire_derive_runs_total",
		"memoire_embeddings_missing",
		"memoire_embeddings_oldest_missing_epoch",
	} {
		if strings.Contains(body, absent) {
			t.Errorf("metrics body must not carry %q\nbody:\n%s", absent, body)
		}
	}

	// The exemption is exactly GET /metrics: spelling variants and other
	// methods stay behind the boundary.
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/v1/stats", http.StatusUnauthorized},
		{http.MethodGet, "/metrics/", http.StatusUnauthorized},
		{http.MethodGet, "/metrics/x", http.StatusUnauthorized},
		{http.MethodPost, "/metrics", http.StatusMethodNotAllowed},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s %s status = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}
}

// The R-D metrics actor split on the wire: every review-log-derived number
// separates human from system:* from agent:* BEFORE the first suggestion is
// written, and the actor STRING itself never crosses the unauthenticated
// boundary (memoire:D224).
func TestMetricsReviewTransitionsActorSplit(t *testing.T) {
	home := t.TempDir()
	cfg := config.Config{
		Addr:      "127.0.0.1:0",
		Home:      home,
		DBPath:    filepath.Join(home, "memoire.db"),
		SchemaDir: "../../../../schema",
		TokenPath: filepath.Join(home, "token"),
		Raw:       filepath.Join(home, "raw"),
		Queue: config.QueuePaths{
			Root:     filepath.Join(home, "queue"),
			Pending:  filepath.Join(home, "queue", "pending"),
			Inflight: filepath.Join(home, "queue", "inflight"),
			Failed:   filepath.Join(home, "queue", "failed"),
			Log:      filepath.Join(home, "log"),
		},
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(cfg.DBPath, cfg.SchemaDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The agent:* accept is the row no API path can mint — that is the
	// point of the gate — so the census is seeded over a direct connection,
	// the same pattern the governance contract tests use.
	conn, err := sql.Open("sqlite3", cfg.DBPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, seed := range []struct{ toState, actor string }{
		{"accepted", "cred-operator-private-name"},
		{"lapsed", db.LapseTTLActor},
		{"accepted", db.TriageActor("some-judge-model")},
	} {
		if _, err := conn.Exec(`INSERT INTO observation_review_log(
			observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch
		) VALUES (1, 'proposed', ?, ?, NULL, 1800000000)`, seed.toState, seed.actor); err != nil {
			t.Fatal(err)
		}
	}

	handler := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	for _, want := range []string{
		"# TYPE memoire_review_transitions gauge\n",
		"memoire_review_transitions{to_state=\"accepted\",actor_class=\"human\"} 1\n",
		"memoire_review_transitions{to_state=\"accepted\",actor_class=\"agent\"} 1\n",
		"memoire_review_transitions{to_state=\"lapsed\",actor_class=\"system\"} 1\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q\nbody:\n%s", want, body)
		}
	}
	for _, absent := range []string{"cred-operator-private-name", "some-judge-model", "actor=\""} {
		if strings.Contains(body, absent) {
			t.Errorf("metrics body must not carry the actor string %q", absent)
		}
	}
}
