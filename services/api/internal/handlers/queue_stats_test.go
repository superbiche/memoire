// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package handlers

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFailedQueueDepthExcludesErrorSidecars(t *testing.T) {
	dir := t.TempDir()
	// One poison item + its drainer sidecar must count as depth 1
	// (round-1 review finding 2).
	for _, name := range []string{"1-poison.json", "1-poison.json.error.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := failedQueueDepth(dir); got != 1 {
		t.Fatalf("expected failed depth 1, got %d", got)
	}
	if got := failedQueueDepth(filepath.Join(dir, "missing")); got != 0 {
		t.Fatalf("expected 0 for missing dir, got %d", got)
	}
}

// Accounting and the consumers share ONE predicate (queuefile.IsItem):
// depth/age report exactly what the drainer would pick. A crash-leaked
// readiness probe or writer temp (no .json suffix) is invisible; a
// hand-written dot-prefixed .json IS backlog because the drainer would
// drain it (review 2026-08-03 F3 + round-2 N2).
func TestQueueStatsMatchConsumerPredicate(t *testing.T) {
	dir := t.TempDir()
	stamp := func(name string, age time.Duration) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
	stamp("1000-item.json", 10*time.Minute)
	// Consumer-pickable despite the dot prefix: counted.
	stamp(".hand-written.json", 10*time.Minute)
	// Crash-leaked temps, OLDER than the real items: not consumer-pickable,
	// must influence neither depth nor oldest-age.
	stamp(".readiness-probe-crash.tmp", 24*time.Hour)
	stamp(".1000-writer.json.tmp", 24*time.Hour)
	// A stray non-item file no consumer would ever take: also not backlog.
	stamp("README", 24*time.Hour)

	if got := queueDepth(dir); got != 2 {
		t.Fatalf("queueDepth = %d, want 2 (consumer-pickable items only)", got)
	}
	if got := failedQueueDepth(dir); got != 2 {
		t.Fatalf("failedQueueDepth = %d, want 2", got)
	}
	if got := oldestQueueAgeSeconds(dir); got > 15*60 {
		t.Fatalf("oldestQueueAgeSeconds = %d, keyed on a non-item entry", got)
	}
	depth, age := pendingQueueStats(dir)
	if depth != 2 {
		t.Fatalf("pendingQueueStats depth = %d, want 2", depth)
	}
	if age > 15*60 {
		t.Fatalf("pendingQueueStats age = %d, keyed on a non-item entry", age)
	}
}
