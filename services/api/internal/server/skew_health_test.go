// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
)

// memoire:D152: /v1/health carries clock_skew_future_events — a one-sided
// counter of events whose occurred_at is AHEAD of received_at beyond the
// threshold (producer clock fault). Events merely BEHIND (queue buffering,
// the slice-3 capture-and-forward-later path) must never count.
func TestHealthReportsOneSidedFutureSkew(t *testing.T) {
	for _, tc := range []struct {
		name      string
		threshold int64
		want      float64
	}{
		{"enabled", 60, 1},
		{"disabled", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			cfg := config.Config{
				Addr:             "127.0.0.1:0",
				Home:             home,
				DBPath:           filepath.Join(home, "memoire.db"),
				SchemaDir:        "../../../../schema",
				TokenPath:        filepath.Join(home, "token"),
				Raw:              filepath.Join(home, "raw"),
				SkewAlertSeconds: tc.threshold,
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
			seed := func(id string, occurred, received int64) {
				t.Helper()
				if _, err := store.Ingest(t.Context(), db.EventInput{
					SchemaVersion:   1,
					Project:         "skew-test",
					SessionID:       "skew-session",
					SourceAdapter:   "skew-adapter",
					SourceEventID:   id,
					EventType:       "note",
					Payload:         json.RawMessage(`{}`),
					OccurredAtEpoch: occurred,
					ReceivedAtEpoch: received,
				}); err != nil {
					t.Fatal(err)
				}
			}
			seed("future-fault", base+3600, base)  // clock fault: 1h ahead
			seed("in-band", base+30, base)         // ahead but inside threshold
			seed("buffered", base, base+7*24*3600) // a week behind: forward-later, never counted

			handler := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("health status = %d body=%s", rec.Code, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			got, ok := body["clock_skew_future_events"].(float64)
			if !ok {
				t.Fatalf("health missing clock_skew_future_events: %s", rec.Body.String())
			}
			if got != tc.want {
				t.Fatalf("clock_skew_future_events = %v, want %v (body=%s)", got, tc.want, rec.Body.String())
			}
		})
	}
}
