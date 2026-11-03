// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package queuewriter

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"memoire/services/api/internal/db"
)

func batchInput(id string) db.EventInput {
	return db.EventInput{
		Project: "p", SessionID: "s", SourceAdapter: "codex-cli", EventType: "codex_record",
		SourceEventID: id, OccurredAt: "2026-07-02T10:00:00Z", ReceivedAtEpoch: 1790000000,
		Payload: PayloadFromText("batch " + id), Text: "batch " + id,
	}
}

func pendingNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A batch item is byte-identical to the item Write produces for the same
// input: the queue format does not change.
func TestBatchItemEqualsWriteItem(t *testing.T) {
	single, batched := testConfig(t), testConfig(t)
	in := batchInput("codex:t:1")
	res, err := Write(t.Context(), single, in)
	if err != nil {
		t.Fatal(err)
	}
	b := NewBatch(batched)
	if err := b.Add(in); err != nil {
		t.Fatal(err)
	}
	got, err := b.Commit()
	if err != nil || len(got) != 1 {
		t.Fatalf("commit: %v (%d results)", err, len(got))
	}
	want, _ := os.ReadFile(res.Path)
	have, _ := os.ReadFile(got[0].Path)
	if string(want) != string(have) {
		t.Fatalf("batch item differs from Write item:\n%s\n---\n%s", want, have)
	}
	if !strings.HasSuffix(got[0].Name, res.Name[strings.Index(res.Name, "-"):]) {
		t.Fatalf("file name shape differs: %s vs %s", got[0].Name, res.Name)
	}
}

// Commit returns every item, in Add order, and leaves no temp file. An
// identical item added twice gets two names (the stamp is unique per item).
func TestBatchCommitOrderAndNoTempFiles(t *testing.T) {
	cfg := testConfig(t)
	b := NewBatch(cfg)
	for _, id := range []string{"a", "b", "c"} {
		if err := b.Add(batchInput(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Add(batchInput("a")); err != nil { // duplicate of the first
		t.Fatal(err)
	}
	if n, _ := b.Pending(); n != 4 {
		t.Fatalf("want 4 staged, got %d", n)
	}
	res, err := b.Commit()
	if err != nil || len(res) != 4 {
		t.Fatalf("commit: %v (%d results)", err, len(res))
	}
	for _, r := range res {
		if _, err := os.Stat(r.Path); err != nil {
			t.Fatalf("result path not durable: %v", err)
		}
	}
	for _, n := range pendingNames(t, cfg.Queue.Pending) {
		if strings.HasPrefix(n, ".") {
			t.Fatalf("temp file left behind: %s", n)
		}
	}
	if n, _ := b.Pending(); n != 0 {
		t.Fatalf("staging area not emptied: %d", n)
	}
}

// When the rename of item k fails, the items before k are visible and
// reported, the rest leave nothing behind.
func TestBatchCommitPrefixOnFailure(t *testing.T) {
	cfg := testConfig(t)
	b := NewBatch(cfg)
	for _, id := range []string{"a", "b", "c"} {
		if err := b.Add(batchInput(id)); err != nil {
			t.Fatal(err)
		}
	}
	// Sabotage item 1: its final name is a non-empty directory, so the
	// rename over it fails.
	second := b.staged[1].final
	if err := os.MkdirAll(filepath.Join(second, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := b.Commit()
	if err == nil {
		t.Fatal("want the rename error")
	}
	if len(res) != 1 {
		t.Fatalf("want a 1-item durable prefix, got %d", len(res))
	}
	for _, n := range pendingNames(t, cfg.Queue.Pending) {
		if strings.HasPrefix(n, ".") {
			t.Fatalf("temp file left behind after failure: %s", n)
		}
	}
}

// The names of a batch follow the order of addition: sorted by name, the
// items come back in Add order, whatever order the workers finished in.
func TestBatchNamesFollowAddOrder(t *testing.T) {
	cfg := testConfig(t)
	b := NewBatch(cfg)
	const n = 600
	added, err := b.AddAll(n, func(i int) (db.EventInput, error) {
		return batchInput(fmt.Sprintf("codex:order:%04d", i)), nil
	})
	if err != nil || added != n {
		t.Fatalf("add: %v (%d)", err, added)
	}
	res, err := b.Commit()
	if err != nil || len(res) != n {
		t.Fatalf("commit: %v (%d)", err, len(res))
	}
	names := make([]string, len(res))
	for i, r := range res {
		names[i] = r.Name
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	prev := int64(0)
	for i, nm := range names {
		if nm != sorted[i] {
			t.Fatalf("name order differs from add order at %d: %s vs %s", i, nm, sorted[i])
		}
		ms, _ := strconv.ParseInt(nm[:strings.Index(nm, "-")], 10, 64)
		if ms <= prev {
			t.Fatalf("stamps must strictly increase inside a batch: %d after %d", ms, prev)
		}
		prev = ms
	}
	if drift := prev - time.Now().UnixMilli(); drift > 5000 {
		t.Fatalf("stamps ran %d ms ahead of the clock", drift)
	}
}

// The redaction key is created once, before the workers start: the same
// secret in every item of a batch on a fresh home gets one marker.
func TestBatchRedactionKeyCreatedOnce(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = true
	if _, err := os.Stat(filepath.Join(cfg.Home, "redaction-key")); err == nil {
		t.Fatal("test setup: the key must not exist yet")
	}
	b := NewBatch(cfg)
	const n = 200
	added, err := b.AddAll(n, func(i int) (db.EventInput, error) {
		in := batchInput(fmt.Sprintf("codex:key:%03d", i))
		in.Payload = PayloadFromText("key AKIA" + "IOSFODNN7EXAMPLE leaked")
		return in, nil
	})
	if err != nil || added != n {
		t.Fatalf("add: %v (%d)", err, added)
	}
	res, err := b.Commit()
	if err != nil || len(res) != n {
		t.Fatalf("commit: %v (%d)", err, len(res))
	}
	markers := map[string]int{}
	for _, r := range res {
		body, _ := os.ReadFile(r.Path)
		if strings.Contains(string(body), "AKIA" + "IOSFODNN7EXAMPLE") {
			t.Fatal("secret survived redaction")
		}
		i := strings.Index(string(body), "[REDACTED:")
		if i < 0 {
			t.Fatalf("no marker in %s", r.Name)
		}
		markers[string(body)[i:i+strings.Index(string(body)[i:], "]")+1]]++
	}
	if len(markers) != 1 {
		t.Fatalf("want one marker for one secret, got %d: %v", len(markers), markers)
	}
	if key, err := os.ReadFile(filepath.Join(cfg.Home, "redaction-key")); err != nil || len(key) != 32 {
		t.Fatalf("want one 32-byte key on disk, got %d bytes, err %v", len(key), err)
	}
}

// Abort while items are still being prepared leaves no temp file: the staged
// ones, the ones written by workers that had not yet reported, and none is
// created afterwards.
func TestBatchAbortDuringAddAllLeavesNoTempFile(t *testing.T) {
	cfg := testConfig(t)
	b := NewBatch(cfg)
	big := strings.Repeat("x", 200000)
	_, err := b.AddAll(400, func(i int) (db.EventInput, error) {
		if i == 150 { // the signal handler's goroutine
			b.Abort()
		}
		in := batchInput(fmt.Sprintf("codex:abort:%03d", i))
		in.Payload = PayloadFromText(big)
		return in, nil
	})
	if err == nil {
		t.Fatal("an aborted batch must report an error")
	}
	for _, n := range pendingNames(t, cfg.Queue.Pending) {
		if strings.HasPrefix(n, ".") {
			t.Fatalf("temp file left behind after Abort: %s", n)
		}
	}
	if _, cerr := b.Commit(); cerr == nil {
		if n := len(pendingNames(t, cfg.Queue.Pending)); n != 0 {
			t.Fatalf("an aborted batch made %d items visible", n)
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

func touchAt(t *testing.T, p string, age time.Duration) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

// SweepStaleTemps removes only files that carry this package's temp names:
// a batch temp when it is old AND its writer is gone, a single-item temp when
// it is older than 24 hours; never a queue item, never a fresh temp file.
func TestSweepStaleTemps(t *testing.T) {
	cfg := testConfig(t)
	sum := strings.Repeat("0f", 32)
	dead := deadPID(t)
	files := map[string]struct {
		age  time.Duration
		gone bool
	}{
		fmt.Sprintf(".1790000000000-codex-cli-%s.json.tmp.%d.7", sum, dead):        {time.Hour, true},
		fmt.Sprintf(".1790000000001-codex-cli-%s.json.tmp.%d.7", sum, dead):        {time.Minute, false}, // dead writer, not old enough
		fmt.Sprintf(".1790000000002-codex-cli-%s.json.tmp.%d.1", sum, os.Getpid()): {time.Hour, false},   // live writer
		".1790000000003-codex-cli-" + sum + ".json.tmp":                            {25 * time.Hour, true},
		".1790000000004-codex-cli-" + sum + ".json.tmp.":                           {25 * time.Hour, false},
		".1790000000005-codex-cli-" + sum + ".json.tmp":                            {11 * time.Minute, false}, // single form: 24 h
		".1790000000006-codex-cli-" + sum + ".json.tmp.9.1x":                       {25 * time.Hour, false},
		"1790000000000-codex-cli-" + sum + ".json":                                 {25 * time.Hour, false},
		".unrelated": {25 * time.Hour, false},
	}
	want := 0
	for name, f := range files {
		touchAt(t, filepath.Join(cfg.Queue.Pending, name), f.age)
		if f.gone {
			want++
		}
	}
	n, err := SweepStaleTemps(cfg.Queue.Pending, StaleTempAge)
	if err != nil || n != want {
		t.Fatalf("sweep: removed %d, err %v (want %d)", n, err, want)
	}
	for name, f := range files {
		_, err := os.Stat(filepath.Join(cfg.Queue.Pending, name))
		if f.gone != os.IsNotExist(err) {
			t.Fatalf("%s: removed=%v, want %v", name, os.IsNotExist(err), f.gone)
		}
	}
	if n, err := SweepStaleTemps(filepath.Join(cfg.Home, "absent"), StaleTempAge); err != nil || n != 0 {
		t.Fatalf("a missing directory is not an error: %d, %v", n, err)
	}
}

// A writer frozen for longer than the age bound (SIGSTOP: a laptop asleep in
// the middle of a write) keeps its temp files; once it is gone they are swept.
func TestSweepKeepsFrozenWriter(t *testing.T) {
	cfg := testConfig(t)
	sum := strings.Repeat("0f", 32)
	c := exec.Command("sleep", "60")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Process.Kill(); _ = c.Wait() }()
	if err := c.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(cfg.Queue.Pending, fmt.Sprintf(".1790000000000-codex-cli-%s.json.tmp.%d.1", sum, c.Process.Pid))
	touchAt(t, p, time.Minute)
	if n, err := SweepStaleTemps(cfg.Queue.Pending, time.Second); err != nil || n != 0 {
		t.Fatalf("a frozen writer's temp file was swept: removed %d, err %v", n, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("the frozen writer's temp file is gone")
	}
	_ = c.Process.Kill()
	_ = c.Wait()
	if n, err := SweepStaleTemps(cfg.Queue.Pending, time.Second); err != nil || n != 1 {
		t.Fatalf("after the writer is gone the file must be swept: removed %d, err %v", n, err)
	}
}
