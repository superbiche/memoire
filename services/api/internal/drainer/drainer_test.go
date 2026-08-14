// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package drainer

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
)

func TestReconcileMovesInflightBackToPendingThenDrains(t *testing.T) {
	cfg := testConfig(t)
	store, err := db.Open(cfg.DBPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	event := db.EventInput{
		SchemaVersion: 1,
		Project:       "memoire-test",
		SessionID:     "session-queue",
		SourceAdapter: "queue-test",
		SourceEventID: "queue-event-1",
		EventType:     "note",
		Payload:       json.RawMessage(`{"message":"queue reconcile needle"}`),
		Text:          "queue reconcile needle",
	}
	writeQueueFile(t, filepath.Join(cfg.Queue.Inflight, "001-event.json"), event)

	drain := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	drain.reconcile(context.Background())

	if _, err := os.Stat(filepath.Join(cfg.Queue.Pending, "001-event.json")); err != nil {
		t.Fatalf("expected item requeued to pending: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Queue.Inflight, "001-event.json")); !os.IsNotExist(err) {
		t.Fatalf("expected inflight item removed, got err=%v", err)
	}

	drain.drain(context.Background())

	if _, err := os.Stat(filepath.Join(cfg.Queue.Pending, "001-event.json")); !os.IsNotExist(err) {
		t.Fatalf("expected pending file removed after ingest, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Raw, "001-event.json")); err != nil {
		t.Fatalf("expected raw custody copy: %v", err)
	}
	hits, err := store.Search(context.Background(), "reconcile needle", "memoire-test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected drained event to be searchable, got %d hits", len(hits))
	}
}

func TestDrainMovesMalformedJSONToFailed(t *testing.T) {
	cfg := testConfig(t)
	store, err := db.Open(cfg.DBPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	name := "bad.json"
	if err := os.WriteFile(filepath.Join(cfg.Queue.Pending, name), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	drain := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	drain.drain(context.Background())

	if _, err := os.Stat(filepath.Join(cfg.Queue.Failed, name)); err != nil {
		t.Fatalf("expected failed queue file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Raw, name)); err != nil {
		t.Fatalf("expected malformed item copied to raw before failure: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(cfg.Queue.Failed, name+".error.json")); err != nil {
		t.Fatalf("expected error sidecar: %v", err)
	} else if len(b) == 0 {
		t.Fatal("expected non-empty error sidecar")
	}
}

func TestDrainDerivesStableSourceEventID(t *testing.T) {
	cfg := testConfig(t)
	store, err := db.Open(cfg.DBPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	body := []byte(`{
		"schema_version":1,
		"project":"memoire-test",
		"session_id":"session-derived",
		"source_adapter":"claude-code",
		"event_type":"claude_PostToolUse",
		"payload":{"tool_use_id":"tool-123","message":"derived needle"}
	}`)
	if err := os.WriteFile(filepath.Join(cfg.Queue.Pending, "derived-1.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Queue.Pending, "derived-2.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	drain := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	drain.drain(context.Background())

	hits, err := store.Search(context.Background(), "derived needle", "memoire-test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected derived duplicate to collapse to one row, got %d", len(hits))
	}
}

func TestDrainDerivesStableUserPromptSourceEventID(t *testing.T) {
	cfg := testConfig(t)
	store, err := db.Open(cfg.DBPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	body := []byte(`{
		"schema_version":1,
		"project":"memoire-test",
		"session_id":"session-prompt-derived",
		"source_adapter":"claude-code",
		"event_type":"claude_UserPromptSubmit",
		"payload":{"prompt":"derived prompt needle"}
	}`)
	if err := os.WriteFile(filepath.Join(cfg.Queue.Pending, "prompt-derived-1.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Queue.Pending, "prompt-derived-2.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	drain := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	drain.drain(context.Background())

	hits, err := store.Search(context.Background(), "derived prompt needle", "memoire-test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected prompt duplicate to collapse to one row, got %d", len(hits))
	}
}

func TestRawCanRebuildDerivedIndex(t *testing.T) {
	cfg := testConfig(t)
	store, err := db.Open(cfg.DBPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	event := db.EventInput{
		SchemaVersion: 1,
		Project:       "memoire-test",
		SessionID:     "session-rebuild",
		SourceAdapter: "queue-test",
		SourceEventID: "queue-event-rebuild",
		EventType:     "note",
		Payload:       json.RawMessage(`{"message":"rebuild needle"}`),
		Text:          "rebuild needle",
	}
	writeQueueFile(t, filepath.Join(cfg.Queue.Pending, "rebuild.json"), event)

	drain := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	drain.drain(context.Background())

	rawPath := filepath.Join(cfg.Raw, "rebuild.json")
	if _, err := os.Stat(rawPath); err != nil {
		t.Fatalf("expected raw copy: %v", err)
	}

	rebuildCfg := testConfig(t)
	rebuildStore, err := db.Open(rebuildCfg.DBPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer rebuildStore.Close()
	rawBytes, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rebuildCfg.Queue.Pending, "rebuild.json"), rawBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	New(rebuildCfg, rebuildStore, slog.New(slog.NewTextHandler(io.Discard, nil))).drain(context.Background())
	hits, err := rebuildStore.Search(context.Background(), "rebuild needle", "memoire-test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected rebuilt index to contain raw event, got %d hits", len(hits))
	}
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
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
	return cfg
}

func writeQueueFile(t *testing.T, path string, event db.EventInput) {
	t.Helper()
	b, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// liaison:D97 decision 8 (memoire:D147 slice 1): expired failed-queue items
// and their sidecars are reaped after the TTL; fresh items and a zero TTL
// are left alone.
func TestReapFailedHonorsTTL(t *testing.T) {
	cfg := testConfig(t)
	cfg.FailedTTLDays = 30
	store, err := db.Open(cfg.DBPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	old := filepath.Join(cfg.Queue.Failed, "old.json")
	oldSidecar := filepath.Join(cfg.Queue.Failed, "old.json.error.json")
	fresh := filepath.Join(cfg.Queue.Failed, "fresh.json")
	for _, path := range []string{old, oldSidecar, fresh} {
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	expired := time.Now().Add(-40 * 24 * time.Hour)
	for _, path := range []string{old, oldSidecar} {
		if err := os.Chtimes(path, expired, expired); err != nil {
			t.Fatal(err)
		}
	}

	// Unit rule: the sidecar of a FRESH item survives even when the sidecar
	// file itself is old (operator touch(1) on the item protects both); an
	// orphan sidecar (no base item) is reaped on its own mtime.
	freshSidecar := filepath.Join(cfg.Queue.Failed, "fresh.json.error.json")
	orphanSidecar := filepath.Join(cfg.Queue.Failed, "gone.json.error.json")
	for _, path := range []string{freshSidecar, orphanSidecar} {
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, expired, expired); err != nil {
			t.Fatal(err)
		}
	}

	drain := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	drain.ReapFailed(context.Background())

	for _, gone := range []string{old, oldSidecar, orphanSidecar} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("expected %s reaped, err=%v", gone, err)
		}
	}
	for _, kept := range []string{fresh, freshSidecar} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("%s must survive (unit rule keyed on base item): %v", kept, err)
		}
	}

	// TTL 0 disables the reaper entirely.
	cfg.FailedTTLDays = 0
	if err := os.Chtimes(fresh, expired, expired); err != nil {
		t.Fatal(err)
	}
	drainOff := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	drainOff.ReapFailed(context.Background())
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("disabled reaper must not delete: %v", err)
	}
}
