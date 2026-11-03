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
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"memoire/services/api/internal/rawstore"
)

// freshDB creates a throwaway database with schema-pg applied.
func freshDB(t *testing.T, admin string) *sql.DB {
	t.Helper()
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("MEMOIRE_TEST_PG_URL: %v", err)
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	dbName := "memoire_replay_pack_" + hex.EncodeToString(suffix[:])
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
	if err := Apply(context.Background(), pg, "../../../../schema-pg"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return pg
}

// replayCorpus writes the same raw/ custody into dir: first-write-wins
// duplicates (the re-POST shape of 2026-10), a same-key/new-session file,
// an envelope without received_at (mtime fallback), an unparseable file,
// and files spread over three UTC days plus one still-open day.
func replayCorpus(t *testing.T, dir string) {
	t.Helper()
	day := func(d string) int64 {
		tm, err := time.Parse("2006-01-02", d)
		if err != nil {
			t.Fatal(err)
		}
		return tm.UnixMilli()
	}
	n := 0
	write := func(ms int64, body string, mtime time.Time) {
		n++
		name := fmt.Sprintf("%d-http-%064x.json", ms, n)
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	env := func(session, sourceEventID, text string, occurred, received int64) string {
		recv := ""
		if received != 0 {
			recv = fmt.Sprintf(`,"received_at_epoch":%d`, received)
		}
		return fmt.Sprintf(`{"schema_version":1,"project":"fixture","session_id":%q,"source_adapter":"codex-cli","source_event_id":%q,"event_type":"codex_item","occurred_at_epoch":%d%s,"payload":{"text":%q},"text":%q}`,
			session, sourceEventID, occurred, recv, text, text)
	}
	mt := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	d1, d2, d3, open := day("2026-10-01"), day("2026-10-02"), day("2026-10-03"), day("2026-10-09")
	write(d1+10, env("s1", "e1", "first write wins", 1000, 1001), mt)
	write(d1+20, env("s1", "e2", "second event", 1002, 1003), mt)
	write(d1+30, env("s1", "e3", "no received stamp", 1004, 0), mt)
	write(d1+40, `{not json`, mt)
	write(d2+10, env("s1", "e1", "LATER DUPLICATE must lose", 1000, 2001), mt.Add(24*time.Hour))
	write(d2+20, env("s2", "e2", "same key new session", 1002, 2003), mt.Add(24*time.Hour))
	write(d3+10, env("s3", "e4", "third day", 3000, 3001), mt.Add(48*time.Hour))
	write(open+10, env("s4", "e5", "still open day", 9000, 9001), mt.Add(8*24*time.Hour))
}

func dumpTables(t *testing.T, pg *sql.DB) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		// e3 has no received_at: drainer.NormalizeEnvelope stamps now(), so
		// its value is wall-clock and masked (Replay's mtime fallback is
		// unreachable for that reason — pre-existing, not a pack effect).
		`SELECT idempotency_key, session_id, source_event_id, text, occurred_at_epoch,
		        CASE WHEN source_event_id = 'e3' THEN -1 ELSE received_at_epoch END, payload_json
		   FROM events ORDER BY idempotency_key`,
		`SELECT session_id, project, client, started_at_epoch FROM sessions ORDER BY session_id`,
		`SELECT session_id, kind, text, occurred_at_epoch, review_state FROM observations ORDER BY session_id, text`,
	} {
		rows, err := pg.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				fmt.Fprintf(&b, "%v|", v)
			}
			b.WriteString("\n")
		}
		_ = rows.Close()
		b.WriteString("--\n")
	}
	return b.String()
}

// Oracle: Replay over packed raw/ produces exactly the store Replay over
// the same corpus kept flat produces — full and delta passes.
func TestReplayPackedEquivalentToFlat(t *testing.T) {
	admin := os.Getenv("MEMOIRE_TEST_PG_URL")
	if admin == "" {
		t.Skip("MEMOIRE_TEST_PG_URL not set; postgres replay fixture skipped")
	}
	ctx := context.Background()
	flatDir, packDir := t.TempDir(), t.TempDir()
	replayCorpus(t, flatDir)
	replayCorpus(t, packDir)
	rep, err := rawstore.PackClosedDays(packDir, rawstore.PackOptions{
		Now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), GraceDays: 2, Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.PartsWritten != 3 || rep.FlatFilesDeleted != 7 {
		t.Fatalf("pack report %+v, want 3 parts and 7 files packed", rep)
	}

	flatPG, packPG := freshDB(t, admin), freshDB(t, admin)
	flatStats, err := Replay(ctx, flatPG, flatDir, false)
	if err != nil {
		t.Fatalf("flat replay: %v", err)
	}
	packStats, err := Replay(ctx, packPG, packDir, false)
	if err != nil {
		t.Fatalf("packed replay: %v", err)
	}
	if flatStats != packStats {
		t.Fatalf("replay stats differ: packed %+v, flat %+v", packStats, flatStats)
	}
	if flatStats.Files != 8 || flatStats.Failed != 1 || flatStats.Duplicates != 2 {
		t.Fatalf("flat replay stats %+v, want 8 files, 1 failed, 2 duplicates", flatStats)
	}
	flatDump, packDump := dumpTables(t, flatPG), dumpTables(t, packPG)
	if flatDump != packDump {
		t.Fatalf("stores differ:\npacked:\n%s\nflat:\n%s", packDump, flatDump)
	}
	if strings.Contains(flatDump, "LATER DUPLICATE") {
		t.Fatal("first-write-wins broken: the later duplicate replaced the first write")
	}

	// Delta passes skip everything on both layouts.
	flatDelta, err := Replay(ctx, flatPG, flatDir, true)
	if err != nil {
		t.Fatal(err)
	}
	packDelta, err := Replay(ctx, packPG, packDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if flatDelta != packDelta {
		t.Fatalf("delta stats differ: packed %+v, flat %+v", packDelta, flatDelta)
	}

	// Tail: new arrivals on both layouts — a late drain into a packed day
	// (it becomes that day's next part on the packed side), a new event in
	// the open day, and a duplicate of an ingested key — then a delta pass.
	tail := func(dir string) {
		for _, f := range []struct{ name, body string }{
			{fmt.Sprintf("%d-http-%064x.json", time.Date(2026, 10, 1, 0, 0, 0, 15, time.UTC).UnixMilli(), 901),
				`{"schema_version":1,"project":"fixture","session_id":"s5","source_adapter":"codex-cli","source_event_id":"e6","event_type":"codex_item","occurred_at_epoch":1005,"received_at_epoch":5001,"payload":{"text":"late into packed day"},"text":"late into packed day"}`},
			{fmt.Sprintf("%d-http-%064x.json", time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC).UnixMilli(), 902),
				`{"schema_version":1,"project":"fixture","session_id":"s4","source_adapter":"codex-cli","source_event_id":"e7","event_type":"codex_item","occurred_at_epoch":9100,"received_at_epoch":9101,"payload":{"text":"open day tail"},"text":"open day tail"}`},
			{fmt.Sprintf("%d-http-%064x.json", time.Date(2026, 10, 9, 2, 0, 0, 0, time.UTC).UnixMilli(), 903),
				`{"schema_version":1,"project":"fixture","session_id":"s1","source_adapter":"codex-cli","source_event_id":"e2","event_type":"codex_item","occurred_at_epoch":1002,"received_at_epoch":9200,"payload":{"text":"re-POST duplicate"},"text":"re-POST duplicate"}`},
		} {
			if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	tail(flatDir)
	tail(packDir)
	rep, err = rawstore.PackClosedDays(packDir, rawstore.PackOptions{
		Now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), GraceDays: 2, Apply: true,
	})
	if err != nil || rep.PartsWritten != 1 {
		t.Fatalf("late part: %+v %v", rep, err)
	}
	if _, err := os.Stat(filepath.Join(packDir, "pack-2026-10-01.001.tar.zst")); err != nil {
		t.Fatalf("late drain must become part .001: %v", err)
	}
	flatTail, err := Replay(ctx, flatPG, flatDir, true)
	if err != nil {
		t.Fatal(err)
	}
	packTail, err := Replay(ctx, packPG, packDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if flatTail != packTail || flatTail.Ingested != 2 {
		t.Fatalf("tail delta: packed %+v, flat %+v (want 2 ingested)", packTail, flatTail)
	}
	if f, p := dumpTables(t, flatPG), dumpTables(t, packPG); f != p {
		t.Fatalf("stores differ after the tail:\npacked:\n%s\nflat:\n%s", p, f)
	}
}

// Custody oracle C2 for Replay (no Postgres needed: the custody gate runs
// before any database access on a full pass): Replay refuses while the
// custody lock is held, and while a purge intent sits beside raw/.
func TestReplayEntersCustody(t *testing.T) {
	root := t.TempDir()
	rawDir := filepath.Join(root, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := rawstore.Lock(rawDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Replay(context.Background(), nil, rawDir, false); err == nil || !strings.Contains(err.Error(), "custody lock") {
		t.Fatalf("Replay with the lock held: %v", err)
	}
	unlock()
	if err := os.WriteFile(filepath.Join(root, "purge-intent.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Replay(context.Background(), nil, rawDir, false); err == nil || !strings.Contains(err.Error(), "purge is unfinished") {
		t.Fatalf("Replay with a purge intent: %v", err)
	}
}
