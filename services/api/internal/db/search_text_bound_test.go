// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	blob := strings.Repeat("QUJDabc1", 30) // 240 mixed-class base64 chars
	hexdump := strings.Repeat("0123456789abcdef", 20)
	wordpath := "/" + strings.Join(strings.Fields(strings.Repeat("some/deep/path/of/words ", 12)), "/")
	longid := strings.Repeat("Service-Name_v2-", 20)
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"prose untouched", "the zebrafish crossed the quantum meadow", "the zebrafish crossed the quantum meadow"},
		{"sha256 hex kept", "id " + sha256hex + " done", "id " + sha256hex + " done"},
		{"sha512 hex kept", "id " + sha512hex + " done", "id " + sha512hex + " done"},
		{"url kept", "see https://example.test/a/b/c?x=1&y=2 now", "see https://example.test/a/b/c?x=1&y=2 now"},
		{"long hex dump kept (no upper case)", hexdump, hexdump},
		{"long word path kept (no digits)", wordpath, wordpath},
		{"long hyphenated identifier kept", longid, longid},
		{"data url is a blob", `{"image_url":"data:image/png;base64,` + blob + `"}`, `{"image_url":"[blob 262 chars]"}`},
		{"data url with parameters is one blob", "data:image/png;charset=utf-8;base64," + blob, "[blob 276 chars]"},
		{"bare base64 run is a blob", "x " + blob + " y", "x [blob 240 chars] y"},
		{"nul dropped", "a\x00b", "ab"},
		{"invalid utf8 replaced", "a\xffb", "a�b"},
		// Composition: NUL inside a would-be blob is normalized first, so the
		// first call already sees (and replaces) the run the second would.
		{"nul inside a blob run", blob[:120] + "\x00" + blob[120:], "[blob 240 chars]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BoundSearchText(c.in)
			if got != c.want {
				t.Fatalf("BoundSearchText(%q) = %q, want %q", c.in, got, c.want)
			}
			if again := BoundSearchText(got); again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
		})
	}

	// Byte cap: prose past 256 KiB is cut; the result including the marker is
	// within the cap and stable under a second pass.
	long := strings.Repeat("plain words ", searchTextMaxBytes/12+1000)
	got := BoundSearchText(long)
	if !strings.HasSuffix(got, searchTruncMarker) || len(got) > searchTextMaxBytes {
		t.Fatalf("long prose: want cut within %d bytes with marker, got %d bytes", searchTextMaxBytes, len(got))
	}
	if again := BoundSearchText(got); again != got {
		t.Fatalf("truncated prose not idempotent")
	}
	// The cut lands on a rune boundary: a multibyte run straddling the cap
	// is not split into invalid UTF-8.
	multi := strings.Repeat("é", searchTextMaxBytes) // 2 bytes each, one unit
	gm := BoundSearchText(multi)
	if !strings.HasSuffix(gm, searchTruncMarker) || strings.ContainsRune(gm, '�') || len(gm) > searchTextMaxBytes {
		t.Fatalf("multibyte cut produced invalid or oversized text (%d bytes)", len(gm))
	}
	// Unit cap: token-dense text well under the byte cap is cut at 20,000
	// letter/digit runs.
	dense := strings.Repeat("ab-cd ", 15000) // 30,000 units, 90 KB
	gd := BoundSearchText(dense)
	if !strings.HasSuffix(gd, searchTruncMarker) || len(gd) >= len(dense) {
		t.Fatalf("token-dense text: want a unit-cap cut, got %d of %d bytes", len(gd), len(dense))
	}
	if n := strings.Count(gd, "ab"); n > searchTextMaxUnits/2 {
		t.Fatalf("token-dense text: %d compounds survive, want <= %d", n, searchTextMaxUnits/2)
	}
	if again := BoundSearchText(gd); again != gd {
		t.Fatalf("unit-cut text not idempotent")
	}
	under := strings.Repeat("ab-cd ", 9000) // 18,000 units: untouched
	if BoundSearchText(under) != under {
		t.Fatalf("text under both caps must be untouched")
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
	ingest := func(id, adapter, eventType, text string, payload []byte, age int64) IngestResult {
		t.Helper()
		res, err := store.Ingest(ctx, EventInput{
			Project:         project,
			SessionID:       "tb-session",
			SourceAdapter:   adapter,
			SourceEventID:   id,
			EventType:       eventType,
			OccurredAtEpoch: now - age,
			Text:            text,
			Payload:         json.RawMessage(payload),
		})
		if err != nil {
			t.Fatalf("ingest %s: %v", id, err)
		}
		return res
	}
	storedText := func(eventID int64) string {
		t.Helper()
		var text string
		if err := store.rdb.QueryRow(`SELECT text FROM observations WHERE event_id = ?`, eventID).Scan(&text); err != nil {
			t.Fatalf("raw observation text of %d: %v", eventID, err)
		}
		return text
	}
	hits := func(q string) int {
		t.Helper()
		h, err := store.Search(ctx, q, project, 10)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		return len(h)
	}

	// 1. A codex tool_result with a 2.9 MB inline screenshot: the shape of the
	// 85 hub failures. Ingest succeeds, the words around the blob are kept and
	// searchable, the blob is not, and the payload is stored whole.
	png := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\x89PNG screenshot bytes ", 100000)))
	payload, _ := json.Marshal(map[string]any{
		"call_id": "call_1",
		"output": []map[string]any{
			{"type": "input_text", "text": "Script completed quokka-marker"},
			{"type": "input_image", "image_url": "data:image/png;base64," + png},
		},
	})
	res := ingest("tb-evt-blob", "codex", "codex_tool_result", "", payload, 50)
	if text := storedText(res.ID); len(text) > 4096 || !strings.Contains(text, "[blob ") || !strings.Contains(text, "quokka-marker") {
		t.Fatalf("projection: want short text with the blob marker and the words, got %d bytes", len(text))
	}
	if n := hits("quokka-marker"); n != 1 {
		t.Fatalf("search for the kept word: want 1 hit, got %d", n)
	}
	if n := hits(png[1000:1040]); n != 0 {
		t.Fatalf("search for a blob fragment: want 0 hits, got %d", n)
	}
	ev, err := store.GetEvent(ctx, res.ID)
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	var got, want any
	if json.Unmarshal(ev.Payload, &got) != nil || json.Unmarshal(payload, &want) != nil || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("payload custody: stored payload differs from the ingested one")
	}

	// 2. Client-supplied text with a NUL (the acpx_prompt failure) ingests.
	ingest("tb-evt-nul", "acpx", "acpx_prompt", "before\x00after wallaby-marker", []byte(`{"k":"v"}`), 40)
	if n := hits("wallaby-marker"); n != 1 {
		t.Fatalf("search after NUL strip: want 1 hit, got %d", n)
	}

	// 3. Oversized prose is cut at the byte cap and still indexed.
	prose := strings.Repeat("plain words of a very long transcript ", 20000) + "numbat-marker"
	res = ingest("tb-evt-prose", "codex", "codex_tool_result", prose, []byte(`{}`), 30)
	if text := storedText(res.ID); !strings.HasSuffix(text, searchTruncMarker) || len(text) > searchTextMaxBytes {
		t.Fatalf("prose: want cut within the byte cap, got %d bytes", len(text))
	}
	if n := hits("transcript"); n != 1 {
		t.Fatalf("search in truncated prose: want 1 hit, got %d", n)
	}

	// 4. Token-dense text: 52,428 distinct hyphenated compounds "aaaa-xxxx"
	// (the reviewer's tsvector counterexample: ~1.15 MB of lexemes from 512 KiB
	// of text, every run far below the blob threshold). The unit cap cuts it
	// and Postgres accepts the generated tsvector.
	var sb strings.Builder
	for i := 0; i < 52428; i++ {
		n := i
		var suffix [4]byte
		for k := 3; k >= 0; k-- {
			suffix[k] = byte('a' + n%26)
			n /= 26
		}
		sb.WriteString("aaaa-")
		sb.Write(suffix[:])
		sb.WriteByte(' ')
	}
	res = ingest("tb-evt-dense", "codex", "codex_tool_result", sb.String(), []byte(`{}`), 20)
	if text := storedText(res.ID); !strings.HasSuffix(text, searchTruncMarker) || len(text) > searchTextMaxBytes {
		t.Fatalf("dense: want a cut within the caps, got %d bytes", len(text))
	}
	if n := hits("aaaa-aaab"); n != 1 {
		t.Fatalf("search for an early dense compound: want 1 hit, got %d", n)
	}
}
