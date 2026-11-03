// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package rawstore

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// dayMS is the millisecond epoch of 00:00 UTC on the given day.
func dayMS(t *testing.T, day string) int64 {
	t.Helper()
	d, err := time.Parse("2006-01-02", day)
	if err != nil {
		t.Fatal(err)
	}
	return d.UnixMilli()
}

// rawName mimics the drainer's name shape: <ms>-http-<hex>.json.
func rawName(ms int64, n int) string {
	return fmt.Sprintf("%d-http-%064x.json", ms, n)
}

type flat struct {
	name  string
	body  string
	mtime time.Time
}

func writeFlat(t *testing.T, dir string, files ...flat) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		if err := os.WriteFile(p, []byte(f.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if !f.mtime.IsZero() {
			if err := os.Chtimes(p, f.mtime, f.mtime); err != nil {
				t.Fatal(err)
			}
		}
	}
}

type walked struct {
	name  string
	body  string
	mtime int64
	pack  string
}

func walkAll(t *testing.T, dir string) []walked {
	t.Helper()
	var out []walked
	err := Walk(dir, func(e Entry, body []byte) error {
		out = append(out, walked{e.Name, string(body), e.ModTime.Unix(), e.Pack})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

// strip drops the location so flat and packed walks compare on content.
func strip(ws []walked) []walked {
	out := make([]walked, len(ws))
	for i, w := range ws {
		w.pack = ""
		out[i] = w
	}
	return out
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func equalWalks(t *testing.T, got, want []walked) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("walk length %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("walk[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// fixtureCorpus spreads files over three UTC days plus the current day,
// including a name without a millisecond prefix and a non-envelope file.
func fixtureCorpus(t *testing.T, dir string) []flat {
	t.Helper()
	d1, d2, d3 := dayMS(t, "2026-10-01"), dayMS(t, "2026-10-02"), dayMS(t, "2026-10-03")
	mt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	files := []flat{
		{rawName(d1+5, 1), `{"n":1}`, mt},
		{rawName(d1+86_399_999, 2), `{"n":2,"multi":"ü "}`, mt.Add(time.Hour)},
		{rawName(d1+7, 3), `{not json`, mt},
		{rawName(d2+1, 4), `{"n":4}`, mt.Add(24 * time.Hour)},
		{rawName(d2+2, 5), strings.Repeat("x", 300_000), mt.Add(25 * time.Hour)},
		{rawName(d3+9, 6), `{"n":6}`, mt.Add(48 * time.Hour)},
		{"legacy-claude-0001.json", `{"n":7}`, mt},
	}
	writeFlat(t, dir, files...)
	// Not an envelope: no reader may yield it.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return files
}
