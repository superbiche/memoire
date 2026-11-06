// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The search-visible text bounds: pure function first, then both engines
// through Ingest (the hub failure of 2026-11-06 was a Postgres generated
// tsvector over an unbounded projection; SQLite must project identically).

func TestBoundSearchTextPure(t *testing.T) {
	sha256hex := strings.Repeat("ab", 32)
	sha512hex := strings.Repeat("cd", 64)
	blob := strings.Repeat("QUJD", 60) // 240 base64 chars
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"prose untouched", "the zebrafish crossed the quantum meadow", "the zebrafish crossed the quantum meadow"},
		{"sha256 hex kept", "id " + sha256hex + " done", "id " + sha256hex + " done"},
		{"sha512 hex kept", "id " + sha512hex + " done", "id " + sha512hex + " done"},
		{"url kept", "see https://example.test/a/b/c?x=1&y=2 now", "see https://example.test/a/b/c?x=1&y=2 now"},
		{"data url is a blob", `{"image_url":"data:image/png;base64,` + blob + `"}`, `{"image_url":"[blob 262 bytes]"}`},
		{"bare base64 run is a blob", "x " + blob + " y", "x [blob 240 bytes] y"},
		{"nul dropped", "a\x00b", "ab"},
		{"invalid utf8 replaced", "a\xffb", "a�b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BoundSearchText(c.in); got != c.want {
				t.Fatalf("BoundSearchText(%q) = %q, want %q", c.in, got, c.want)
			}
			if again := BoundSearchText(BoundSearchText(c.in)); again != c.want {
				t.Fatalf("not idempotent: %q", again)
			}
		})
	}

	long := strings.Repeat("word ", searchTextMaxBytes/5+1000) // > cap, ASCII
	got := BoundSearchText(long)
	if !strings.HasSuffix(got, " [truncated]") {
		t.Fatalf("long text: want [truncated] suffix, got tail %q", got[len(got)-30:])
	}
	if len(got) > searchTextMaxBytes+len(" [truncated]") {
		t.Fatalf("long text: %d bytes exceeds the cap", len(got))
	}
	// The cut lands on a rune boundary: a multibyte run straddling the cap
	// is not split into invalid UTF-8.
	multi := strings.Repeat("é", searchTextMaxBytes) // 2 bytes each
	gm := BoundSearchText(multi)
	if !strings.HasSuffix(gm, " [truncated]") || strings.ContainsRune(gm, '�') {
		t.Fatalf("multibyte cut produced invalid text")
	}
}

func TestSearchTextBoundThroughIngest(t *testing.T) {
	engines := []struct {
		name string
		open func(t *testing.T) *Store
	}{
		{"sqlite", func(t *testing.T) *Store {
			store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), schemaDirForTest(t))
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
		{"postgres", func(t *testing.T) *Store {
			dsn, _ := conformancePGURL(t)
			store, err := Open(dsn, schemaDirForTest(t))
			if err != nil {
				t.Fatalf("open postgres: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
	for _, eng := range engines {
		t.Run(eng.name, func(t *testing.T) {
			runSearchTextBoundBattery(t, eng.open(t))
		})
	}
}

func runSearchTextBoundBattery(t *testing.T, store *Store) {
	ctx := context.Background()
	now := time.Now().Unix()
	project := "textbound"

	// 1. A codex tool_result with a 2 MB inline screenshot: the shape of the
	// 85 hub failures. Ingest must succeed and the projection must keep the
	// words around the blob and drop the blob.
	png := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\x89PNG screenshot bytes ", 100000))) // ~2.9 MB
	payload, _ := json.Marshal(map[string]any{
		"call_id": "call_1",
		"output": []map[string]any{
			{"type": "input_text", "text": "Script completed quokka-marker"},
			{"type": "input_image", "image_url": "data:image/png;base64," + png},
		},
	})
	in := EventInput{
		Project:         project,
		SessionID:       "tb-session",
		SourceAdapter:   "codex",
		SourceEventID:   "tb-evt-blob",
		EventType:       "codex_tool_result",
		OccurredAtEpoch: now - 50,
		Payload:         json.RawMessage(payload),
	}
	res, err := store.Ingest(ctx, in)
	if err != nil {
		t.Fatalf("ingest with inline blob: %v", err)
	}
	var text string
	if err := store.rdb.QueryRow(`SELECT text FROM observations WHERE event_id = ?`, res.ID).Scan(&text); err != nil {
		t.Fatalf("raw observation text: %v", err)
	}
	if len(text) > 4096 || !strings.Contains(text, "[blob ") || !strings.Contains(text, "quokka-marker") {
		t.Fatalf("projection: want short text with the blob marker and the words, got %d bytes", len(text))
	}
	if hits, err := store.Search(ctx, "quokka-marker", project, 10); err != nil || len(hits) != 1 {
		t.Fatalf("search for the kept word: hits=%d err=%v", len(hits), err)
	}
	if hits, err := store.Search(ctx, png[1000:1040], project, 10); err != nil || len(hits) != 0 {
		t.Fatalf("search for a blob fragment: want 0 hits, got %d err=%v", len(hits), err)
	}
	// The payload keeps custody of the whole thing.
	ev, err := store.GetEvent(ctx, res.ID)
	if err != nil || !strings.Contains(string(ev.Payload), png[:64]) {
		t.Fatalf("payload custody lost: err=%v", err)
	}

	// 2. Client-supplied text with a NUL (the acpx_prompt failure) ingests.
	nul := EventInput{
		Project:         project,
		SessionID:       "tb-session",
		SourceAdapter:   "acpx",
		SourceEventID:   "tb-evt-nul",
		EventType:       "acpx_prompt",
		OccurredAtEpoch: now - 40,
		Text:            "before\x00after wallaby-marker",
		Payload:         json.RawMessage(`{"k":"v"}`),
	}
	if _, err := store.Ingest(ctx, nul); err != nil {
		t.Fatalf("ingest text with NUL: %v", err)
	}
	if hits, err := store.Search(ctx, "wallaby-marker", project, 10); err != nil || len(hits) != 1 {
		t.Fatalf("search after NUL strip: hits=%d err=%v", len(hits), err)
	}

	// 3. Oversized prose is cut at the cap and still indexed.
	prose := strings.Repeat("plain words of a very long transcript ", 20000) + "numbat-marker"
	big := EventInput{
		Project:         project,
		SessionID:       "tb-session",
		SourceAdapter:   "codex",
		SourceEventID:   "tb-evt-prose",
		EventType:       "codex_tool_result",
		OccurredAtEpoch: now - 30,
		Text:            prose,
		Payload:         json.RawMessage(`{}`),
	}
	res, err = store.Ingest(ctx, big)
	if err != nil {
		t.Fatalf("ingest oversized prose: %v", err)
	}
	if err := store.rdb.QueryRow(`SELECT text FROM observations WHERE event_id = ?`, res.ID).Scan(&text); err != nil {
		t.Fatalf("prose text: %v", err)
	}
	if !strings.HasSuffix(text, " [truncated]") || len(text) > searchTextMaxBytes+len(" [truncated]") {
		t.Fatalf("prose: want truncated at the cap, got %d bytes", len(text))
	}
	if hits, err := store.Search(ctx, "transcript", project, 10); err != nil || len(hits) != 1 {
		t.Fatalf("search in truncated prose: hits=%d err=%v", len(hits), err)
	}
}
