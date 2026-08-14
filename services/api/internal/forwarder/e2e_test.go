// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package forwarder

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
	"memoire/services/api/internal/drainer"
	"memoire/services/api/internal/queuewriter"
	"memoire/services/api/internal/server"
)

// The slice-3 round-1 review's sharpest missing test (finding 7): the unit
// suite proves the forwarder obeys a FAKE ack; this proves the ack is safe
// and idempotent across the ACTUAL hub pipeline — real server.New routing,
// real queuewriter.WriteBytes (redaction, hub-arrival stamping), real hub
// drainer ingest — including the crash-before-delete re-forward for both
// envelope shapes: with source_event_id (the memoire-queue path) and without
// (the direct-HTTP shape whose identity must come from the hub's
// original-bytes stamp).
func TestEndToEndForwardCustodyAndIdempotency(t *testing.T) {
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Real hub: API handler over a real store + schema.
	hubHome := t.TempDir()
	hubCfg := config.Config{
		Addr:      "127.0.0.1:0",
		Home:      hubHome,
		DBPath:    filepath.Join(hubHome, "memoire.db"),
		SchemaDir: "../../../../schema",
		TokenPath: filepath.Join(hubHome, "token"),
		Raw:       filepath.Join(hubHome, "raw"),
		Queue: config.QueuePaths{
			Root:     filepath.Join(hubHome, "queue"),
			Pending:  filepath.Join(hubHome, "queue", "pending"),
			Inflight: filepath.Join(hubHome, "queue", "inflight"),
			Failed:   filepath.Join(hubHome, "queue", "failed"),
			Log:      filepath.Join(hubHome, "log"),
		},
	}
	if err := hubCfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	hubStore, err := db.Open(hubCfg.DBPath, hubCfg.SchemaDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hubStore.Close() })
	if _, err := hubStore.EnsureLocalTokenCredential(ctx, "hub-secret"); err != nil {
		t.Fatal(err)
	}
	hub := httptest.NewServer(server.New(hubCfg, hubStore, quiet))
	defer hub.Close()

	// Client: local queue + forwarder pointed at the real hub.
	cfg := testConfig(t)
	cfg.ForwardURL = hub.URL + "/v1"
	if err := os.WriteFile(cfg.ForwardTokenPath, []byte("hub-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Shape 1: the memoire-queue path — source_event_id pre-filled.
	if _, err := queuewriter.Write(ctx, cfg, db.EventInput{
		SchemaVersion: 1,
		Project:       "e2e-remote",
		SessionID:     "e2e-session",
		SourceAdapter: "e2e-adapter",
		SourceEventID: "e2e-with-id",
		EventType:     "note",
		Payload:       json.RawMessage(`{"message":"alpha snowdrop"}`),
		Text:          "alpha snowdrop",
	}); err != nil {
		t.Fatal(err)
	}
	// Shape 2: direct-HTTP shape — no source_event_id on disk; identity must
	// come from the hub's original-bytes stamp.
	idless := []byte(`{"schema_version":1,"project":"e2e-remote","session_id":"e2e-session","source_adapter":"e2e-adapter","event_type":"note","payload":{"message":"beta glacier"},"text":"beta glacier"}`)
	if err := os.WriteFile(filepath.Join(cfg.Queue.Pending, "900-idless.json"), idless, 0o600); err != nil {
		t.Fatal(err)
	}

	// Snapshot pending bytes so the crash-before-delete can be replayed.
	type saved struct{ name string; body []byte }
	var snapshot []saved
	for _, e := range mustDir(t, cfg.Queue.Pending) {
		b, err := os.ReadFile(filepath.Join(cfg.Queue.Pending, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		snapshot = append(snapshot, saved{e.Name(), b})
	}
	if len(snapshot) != 2 {
		t.Fatalf("expected 2 pending items, got %d", len(snapshot))
	}

	forward := func() {
		f := newTestForwarder(t, cfg)
		f.ForwardOnce(ctx)
		if n := len(mustDir(t, cfg.Queue.Pending)); n != 0 {
			t.Fatalf("expected pending drained after acked forward, found %d", n)
		}
	}

	// First delivery, hub drains.
	forward()
	drainer.New(hubCfg, hubStore, quiet).RunOnce(ctx)

	// Crash-before-delete: the same files reappear locally and re-forward.
	for _, s := range snapshot {
		if err := os.WriteFile(filepath.Join(cfg.Queue.Pending, s.name), s.body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	forward()
	drainer.New(hubCfg, hubStore, quiet).RunOnce(ctx)

	// Both shapes must have collapsed to exactly one event each.
	for _, needle := range []string{"alpha snowdrop", "beta glacier"} {
		hits, err := hubStore.Search(ctx, needle, "e2e-remote", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 {
			t.Fatalf("re-forwarded %q must dedup to 1 event at the hub, got %d hits", needle, len(hits))
		}
	}
	// Nothing may be stuck hub-side: every arrival either ingested or failed
	// loudly — and none failed.
	if n := len(mustDir(t, hubCfg.Queue.Failed)); n != 0 {
		t.Fatalf("hub failed queue must be empty, found %d", n)
	}
	if n := len(mustDir(t, hubCfg.Queue.Pending)); n != 0 {
		t.Fatalf("hub pending must be drained, found %d", n)
	}
	// Raw custody exists hub-side for the arrivals.
	if n := len(mustDir(t, hubCfg.Raw)); n < 2 {
		t.Fatalf("expected hub raw custody copies, found %d", n)
	}
}
