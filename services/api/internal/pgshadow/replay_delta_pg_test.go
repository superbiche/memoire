// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package pgshadow

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// The round-2 finding-1 fixture: the idempotency key ignores session_id when
// source_event_id is set, so a tail raw file can repeat an existing key under
// a NEW session. The delta skip must NOT swallow that file — the full path's
// session insert (which runs before the event-conflict dedup) is a real side
// effect the hub drainer would have performed. Runs only with
// MEMOIRE_TEST_PG_URL (an admin DSN with CREATEDB — the local pg-shadow
// container); creates and drops a throwaway database.
func TestReplayDeltaSameKeyNewSession(t *testing.T) {
	admin := os.Getenv("MEMOIRE_TEST_PG_URL")
	if admin == "" {
		t.Skip("MEMOIRE_TEST_PG_URL not set; postgres replay fixture skipped")
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("MEMOIRE_TEST_PG_URL: %v", err)
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	dbName := "memoire_replay_" + hex.EncodeToString(suffix[:])
	adminConn, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminConn.Exec("CREATE DATABASE " + dbName); err != nil {
		_ = adminConn.Close()
		t.Fatalf("create fixture database (role needs CREATEDB): %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminConn.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
		_ = adminConn.Close()
	})
	u.Path = "/" + dbName
	pg, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })

	ctx := context.Background()
	if err := Apply(ctx, pg, "../../../../schema-pg"); err != nil {
		t.Fatalf("apply: %v", err)
	}

	rawDir := t.TempDir()
	envelope := func(name, session, sourceEventID string, occurred int64) {
		body := fmt.Sprintf(`{"schema_version":1,"project":"fixture","session_id":%q,"source_adapter":"claude-code","source_event_id":%q,"event_type":"claude_Stop","occurred_at_epoch":%d,"received_at_epoch":%d,"payload":{"n":1}}`,
			session, sourceEventID, occurred, occurred)
		if err := os.WriteFile(filepath.Join(rawDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	envelope("0001-a.json", "sess-one", "evt-shared", 1000)
	full, err := Replay(ctx, pg, rawDir, false)
	if err != nil {
		t.Fatalf("full replay: %v", err)
	}
	if full.Ingested != 1 || full.Failed != 0 {
		t.Fatalf("full replay: %+v", full)
	}

	// Same key (identical source_adapter:source_event_id), NEW session.
	envelope("0002-b.json", "sess-two", "evt-shared", 2000)
	delta1, err := Replay(ctx, pg, rawDir, true)
	if err != nil {
		t.Fatalf("delta replay 1: %v", err)
	}
	// File A: key known + session known -> skipped. File B: key known but
	// session UNKNOWN -> full path -> session insert + event-conflict dedup.
	if delta1.Skipped != 1 || delta1.Duplicates != 1 || delta1.Ingested != 0 || delta1.Failed != 0 {
		t.Fatalf("delta replay 1: want skipped=1 duplicates=1, got %+v", delta1)
	}
	var events, sessions int
	if err := pg.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := pg.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if events != 1 || sessions != 2 {
		t.Fatalf("after delta 1: want 1 event + 2 sessions (the new session row is the point), got %d/%d", events, sessions)
	}

	// Steady state: both files now fully known -> both skip, nothing changes.
	delta2, err := Replay(ctx, pg, rawDir, true)
	if err != nil {
		t.Fatalf("delta replay 2: %v", err)
	}
	if delta2.Skipped != 2 || delta2.Duplicates != 0 || delta2.Ingested != 0 {
		t.Fatalf("delta replay 2: want all skipped, got %+v", delta2)
	}
}
