// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scratchHome pins MEMOIRE_HOME at a scratch path: without it a test queue
// write would land in the operator's live ~/.memoire.
func scratchHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MEMOIRE_HOME", home)
	return home
}

func readPending(t *testing.T, home string) []map[string]any {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "queue", "pending"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read pending dir: %v", err)
	}
	var items []map[string]any
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(home, "queue", "pending", e.Name()))
		if err != nil {
			t.Fatalf("read queue item: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("queue item %s not JSON: %v", e.Name(), err)
		}
		items = append(items, m)
	}
	return items
}

// The -text-file flag must carry the event text through a file and produce the
// same queue envelope -text would (turn events ride -text-file because a turn
// larger than the argv limit cannot ride the command line).
func TestTextFileCarriesText(t *testing.T) {
	home := scratchHome(t)
	textPath := filepath.Join(t.TempDir(), "turn.txt")
	want := "turn text line\n\nwith a second block — más allá"
	if err := os.WriteFile(textPath, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{
		"-project", "p", "-session-id", "s", "-source-adapter", "codex-cli",
		"-event-type", "codex_assistant_turn", "-source-event-id", "codex:x:1",
		"-text-file", textPath,
	}, io.Discard)
	if err != nil {
		t.Fatalf("run with -text-file: %v", err)
	}
	items := readPending(t, home)
	if len(items) != 1 {
		t.Fatalf("want 1 queued item, got %d", len(items))
	}
	if got := items[0]["text"]; got != want {
		t.Fatalf("text not carried from -text-file:\nwant %q\ngot  %q", want, got)
	}
}

// -text and -text-file are mutually exclusive.
func TestTextAndTextFileExclusive(t *testing.T) {
	home := scratchHome(t)
	textPath := filepath.Join(t.TempDir(), "turn.txt")
	if err := os.WriteFile(textPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{
		"-project", "p", "-text", "a", "-text-file", textPath,
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "only one of -text or -text-file") {
		t.Fatalf("want mutual-exclusion error, got: %v", err)
	}
	if items := readPending(t, home); len(items) != 0 {
		t.Fatalf("rejected call must not queue: %d items", len(items))
	}
}

// A text larger than any argv limit must pass whole: no truncation anywhere in
// the file channel (capture-all rule).
func TestTextFileUncapped(t *testing.T) {
	home := scratchHome(t)
	textPath := filepath.Join(t.TempDir(), "big.txt")
	want := strings.Repeat("mémoire ", 128*1024) // 1 MiB, multi-byte, >> ARG_MAX/2
	if err := os.WriteFile(textPath, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{
		"-project", "p", "-session-id", "s", "-source-adapter", "codex-cli",
		"-event-type", "codex_assistant_turn", "-source-event-id", "codex:x:2",
		"-text-file", textPath,
	}, io.Discard)
	if err != nil {
		t.Fatalf("run with oversized -text-file: %v", err)
	}
	items := readPending(t, home)
	if len(items) != 1 {
		t.Fatalf("want 1 queued item, got %d", len(items))
	}
	got, _ := items[0]["text"].(string)
	if got != want {
		t.Fatalf("uncapped text mangled: want %d bytes, got %d", len(want), len(got))
	}
}

// A missing -text-file path is a hard error, never a silent empty text.
func TestTextFileMissingFails(t *testing.T) {
	scratchHome(t)
	err := run(context.Background(), []string{
		"-project", "p", "-text-file", filepath.Join(t.TempDir(), "absent.txt"),
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "-text-file") {
		t.Fatalf("want -text-file read error, got: %v", err)
	}
}

// Without either text flag the queue CLI still defaults the payload from the
// (empty) text — the pre-existing contract must survive the refactor.
func TestNoTextFlagStillQueues(t *testing.T) {
	home := scratchHome(t)
	err := run(context.Background(), []string{
		"-project", "p", "-session-id", "s", "-source-adapter", "manual",
		"-event-type", "note",
	}, io.Discard)
	if err != nil {
		t.Fatalf("plain run: %v", err)
	}
	if items := readPending(t, home); len(items) != 1 {
		t.Fatalf("want 1 queued item, got %d", len(items))
	}
}

func writeBatch(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "batch.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// -batch-file writes one queue item per line, in order, with the same
// envelope a per-event call gives; the command-line flags are defaults for the
// fields a line omits.
func TestBatchWritesOneItemPerLine(t *testing.T) {
	home := scratchHome(t)
	p := writeBatch(t,
		`{"source_event_id":"codex:x:1","event_type":"codex_prompt","occurred_at":"2026-07-02T10:00:01.000Z","payload":{"k":1}}`,
		`{"source_event_id":"codex:x:2","event_type":"codex_assistant_turn","text":"turn text\n\nsecond","payload":{"closed_by":"terminal"},"project_root":"/r","project_source":"registry"}`,
		`{"source_event_id":"codex:x:3","event_type":"note","text":"only text"}`,
	)
	var out strings.Builder
	err := run(context.Background(), []string{
		"-project", "p", "-session-id", "s", "-source-adapter", "codex-cli",
		"-cwd", "/w", "-batch-file", p,
	}, &out)
	if err != nil {
		t.Fatalf("batch run: %v", err)
	}
	if n := len(strings.Fields(out.String())); n != 3 {
		t.Fatalf("want 3 printed paths, got %d: %q", n, out.String())
	}
	byID := map[string]map[string]any{}
	for _, it := range readPending(t, home) {
		byID[it["source_event_id"].(string)] = it
	}
	if len(byID) != 3 {
		t.Fatalf("want 3 queued items, got %d", len(byID))
	}
	one := byID["codex:x:1"]
	if one["project"] != "p" || one["session_id"] != "s" || one["source_adapter"] != "codex-cli" ||
		one["cwd"] != "/w" || one["event_type"] != "codex_prompt" {
		t.Fatalf("flag defaults not applied: %v", one)
	}
	if one["occurred_at"] != "2026-07-02T10:00:01.000Z" {
		t.Fatalf("line occurred_at lost: %v", one["occurred_at"])
	}
	two := byID["codex:x:2"]
	if two["text"] != "turn text\n\nsecond" {
		t.Fatalf("text not carried: %q", two["text"])
	}
	pl := two["payload"].(map[string]any)
	if pl["closed_by"] != "terminal" || pl["memoire_project_root"] != "/r" || pl["memoire_project_source"] != "registry" {
		t.Fatalf("payload/identity merge wrong: %v", pl)
	}
	three := byID["codex:x:3"]
	if three["payload"].(map[string]any)["message"] != "only text" {
		t.Fatalf("payload must default from text as a single call does: %v", three["payload"])
	}
}

// A failing line stops the batch: the lines before it are durable, and the
// paths printed tell the caller how many.
func TestBatchStopsAtFirstFailure(t *testing.T) {
	home := scratchHome(t)
	p := writeBatch(t,
		`{"source_event_id":"a:1","event_type":"note"}`,
		`{"source_event_id":"a:2","event_type":"note"}`,
		`{"source_event_id":"a:3","event_type":"note","occurred_at":"not a date"}`,
		`{"source_event_id":"a:4","event_type":"note"}`,
	)
	var out strings.Builder
	err := run(context.Background(), []string{"-project", "p", "-batch-file", p}, &out)
	if err == nil || !strings.Contains(err.Error(), "batch line 3") {
		t.Fatalf("want an error naming batch line 3, got: %v", err)
	}
	if n := len(strings.Fields(out.String())); n != 2 {
		t.Fatalf("want exactly 2 printed paths before the failure, got %d", n)
	}
	if items := readPending(t, home); len(items) != 2 {
		t.Fatalf("want 2 durable items, got %d", len(items))
	}
}

// Capture-all: a multi-MB event passes a batch line whole.
func TestBatchLargeLine(t *testing.T) {
	home := scratchHome(t)
	big := strings.Repeat("mémoire ", 1024*1024) // ~9 MiB
	line, _ := json.Marshal(map[string]any{
		"source_event_id": "big:1", "event_type": "codex_assistant_turn", "text": big,
		"payload": map[string]any{"blob": big},
	})
	p := writeBatch(t, string(line), `{"source_event_id":"big:2","event_type":"note"}`)
	if err := run(context.Background(), []string{"-project", "p", "-batch-file", p}, io.Discard); err != nil {
		t.Fatalf("batch run: %v", err)
	}
	items := readPending(t, home)
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %d", len(items))
	}
	for _, it := range items {
		if it["source_event_id"] == "big:1" {
			if got, _ := it["text"].(string); got != big {
				t.Fatalf("large text mangled: want %d bytes, got %d", len(big), len(got))
			}
		}
	}
}

// The last line may lack its newline; a blank or invalid line is an error
// that names its number.
func TestBatchLastLineWithoutNewlineAndBadLine(t *testing.T) {
	home := scratchHome(t)
	p := filepath.Join(t.TempDir(), "b.jsonl")
	if err := os.WriteFile(p, []byte(`{"source_event_id":"n:1","event_type":"note"}`+"\n"+`{"source_event_id":"n:2","event_type":"note"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"-project", "p", "-batch-file", p}, io.Discard); err != nil {
		t.Fatalf("batch run: %v", err)
	}
	if items := readPending(t, home); len(items) != 2 {
		t.Fatalf("want 2 items, got %d", len(items))
	}
	bad := writeBatch(t, `{"source_event_id":"n:3","event_type":"note"}`, `not json`)
	err := run(context.Background(), []string{"-project", "p", "-batch-file", bad}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "batch line 2") {
		t.Fatalf("want an error naming batch line 2, got: %v", err)
	}
}

func TestBatchRejectsPerEventFlags(t *testing.T) {
	scratchHome(t)
	p := writeBatch(t, `{"event_type":"note"}`)
	for _, extra := range [][]string{{"-text", "x"}, {"-text", ""}, {"-payload-json", "{}"}, {"-source-event-id", "z"}} {
		args := append([]string{"-project", "p", "-batch-file", p}, extra...)
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Fatalf("-batch-file with %v must be rejected", extra)
		}
	}
}

// An explicit empty -text still conflicts with -text-file (F12), and -h keeps
// its historical meaning (flag.ErrHelp, exit 0 in main), a bad flag exit 2.
func TestFlagSemanticsUnchanged(t *testing.T) {
	scratchHome(t)
	textPath := filepath.Join(t.TempDir(), "t.txt")
	_ = os.WriteFile(textPath, []byte("x"), 0o600)
	err := run(context.Background(), []string{"-project", "p", "-text", "", "-text-file", textPath}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "only one of -text or -text-file") {
		t.Fatalf("empty -text with -text-file must conflict, got: %v", err)
	}
	if err := run(context.Background(), []string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h must surface flag.ErrHelp, got: %v", err)
	}
	var pe parseError
	if err := run(context.Background(), []string{"-nope"}, io.Discard); !errors.As(err, &pe) {
		t.Fatalf("a bad flag must be a parseError (exit 2), got: %v", err)
	}
}

// A batch removes the temp files an earlier, killed writer left in the
// pending directory (old enough, recognisable by name) and nothing else.
func TestBatchSweepsStaleTemps(t *testing.T) {
	home := scratchHome(t)
	pending := filepath.Join(home, "queue", "pending")
	if err := os.MkdirAll(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("ab", 32)
	stale := filepath.Join(pending, fmt.Sprintf(".1790000000000-codex-cli-"+sum+".json.tmp.%d.7", deadPID(t)))
	staleSingle := filepath.Join(pending, ".1790000000000-codex-cli-"+sum+".json.tmp")
	fresh := filepath.Join(pending, ".1790000000001-codex-cli-"+sum+".json.tmp.4243.1")
	realItem := filepath.Join(pending, "1790000000000-codex-cli-"+sum+".json")
	other := filepath.Join(pending, ".hidden-note")
	for _, p := range []string{stale, staleSingle, fresh, realItem, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	for _, p := range []string{stale, realItem, other} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	veryOld := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(staleSingle, veryOld, veryOld); err != nil {
		t.Fatal(err)
	}
	p := writeBatch(t, `{"source_event_id":"codex:sweep:1","event_type":"note","text":"t"}`)
	var out strings.Builder
	if err := run(context.Background(), []string{"-project", "p", "-session-id", "s", "-source-adapter", "codex-cli", "-batch-file", p}, &out); err != nil {
		t.Fatalf("batch run: %v", err)
	}
	for _, gone := range []string{stale, staleSingle} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("stale temp file survived the batch: %s", filepath.Base(gone))
		}
	}
	for _, kept := range []string{fresh, realItem, other} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("the sweep removed a file it must keep: %s", filepath.Base(kept))
		}
	}
}

// deadPID returns the pid of a process that has exited and been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	return c.Process.Pid
}
