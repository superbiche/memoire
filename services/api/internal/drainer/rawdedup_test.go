// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package drainer

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"memoire/services/api/internal/db"
)

func rawNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func count(t *testing.T, _ *db.Store, dbPath, query string) int {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var n int
	if err := conn.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func rows(t *testing.T, dbPath, query string) string {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rs, err := conn.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rs.Close()
	cols, _ := rs.Columns()
	var b strings.Builder
	for rs.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			b.WriteString(v.String + "|")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Operator ruling a65: an exact duplicate (key stored AND session known —
// Ingest would write nothing) gets no raw/ copy; a duplicate under a NEW
// session keeps one (Ingest still inserts that session, so replay needs
// the file); a malformed item keeps one (forensics). Rebuilding a store
// from raw/ reproduces the live store.
func TestRawSkipsOnlyNoOpDuplicates(t *testing.T) {
	cfg := testConfig(t)
	store, err := db.Open(cfg.DBPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := New(cfg, store, logger)

	ev := func(session, id, text string) db.EventInput {
		return db.EventInput{
			SchemaVersion: 1, Project: "memoire-test", SessionID: session,
			SourceAdapter: "codex-cli", SourceEventID: id, EventType: "codex_item", OccurredAtEpoch: 1790812800,
			Payload: json.RawMessage(`{"text":"` + text + `"}`), Text: text,
		}
	}
	// Round 1: first writes.
	writeQueueFile(t, filepath.Join(cfg.Queue.Pending, "0001-a.json"), ev("s1", "e1", "first write"))
	writeQueueFile(t, filepath.Join(cfg.Queue.Pending, "0002-b.json"), ev("s1", "e2", "second event"))
	d.drain(context.Background())
	// Round 2: the reboot re-send shape — same keys, same session (no-op),
	// plus the same key under a new session, plus garbage.
	writeQueueFile(t, filepath.Join(cfg.Queue.Pending, "0003-c.json"), ev("s1", "e1", "RE-SENT must not reach raw"))
	writeQueueFile(t, filepath.Join(cfg.Queue.Pending, "0004-d.json"), ev("s1", "e2", "RE-SENT must not reach raw"))
	writeQueueFile(t, filepath.Join(cfg.Queue.Pending, "0005-e.json"), ev("s2", "e1", "same key new session"))
	if err := os.WriteFile(filepath.Join(cfg.Queue.Pending, "0006-f.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	d.drain(context.Background())

	got := rawNames(t, cfg.Raw)
	want := []string{"0001-a.json", "0002-b.json", "0005-e.json", "0006-f.json"}
	if len(got) != len(want) {
		t.Fatalf("raw/ = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("raw/ = %v, want %v", got, want)
		}
	}
	if left := rawNames(t, cfg.Queue.Pending); len(left) != 0 {
		t.Fatalf("pending not drained: %v", left)
	}
	if failed := rawNames(t, cfg.Queue.Failed); len(failed) != 2 { // item + .error.json sidecar
		t.Fatalf("malformed item must fail with its sidecar, failed/ = %v", failed)
	}

	// Rebuild from raw/ alone: the same events and sessions.
	rb := testConfig(t)
	rbStore, err := db.Open(rb.DBPath, "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer rbStore.Close()
	for _, name := range got {
		b, err := os.ReadFile(filepath.Join(cfg.Raw, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rb.Queue.Pending, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	New(rb, rbStore, logger).drain(context.Background())
	// Selected stable columns (review N5: not every column; NULL and ""
	// compare equal in this serializer): event identity, content and times,
	// session project/client/start/principal. received_at is stamped at
	// drain time on both sides and is excluded; occurred_at is pinned in the
	// fixture (unpinned, normalization stamps wall-clock time and the
	// comparison flaked across a second boundary).
	for _, q := range []string{
		`SELECT idempotency_key, session_id, project, source_adapter, source_event_id, event_type, payload_json, text, occurred_at_epoch FROM events ORDER BY idempotency_key`,
		`SELECT session_id, project, client, started_at_epoch, principal_id FROM sessions ORDER BY session_id`,
	} {
		live, rebuilt := rows(t, cfg.DBPath, q), rows(t, rb.DBPath, q)
		if live != rebuilt {
			t.Fatalf("%s:\nlive:\n%s\nrebuilt from raw:\n%s", q, live, rebuilt)
		}
	}
	if !strings.Contains(rows(t, cfg.DBPath, `SELECT text FROM events ORDER BY text`), "first write") {
		t.Fatal("first write must win")
	}
	if n := count(t, store, cfg.DBPath, `SELECT count(*) FROM sessions`); n != 2 {
		t.Fatalf("sessions = %d, want 2 (s1 and the new-session duplicate's s2)", n)
	}
}
