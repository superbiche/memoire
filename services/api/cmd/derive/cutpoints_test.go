// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
	"memoire/services/api/internal/modelderive"
)

// fatChars is a per-event text length that makes an event non-trivial (>= 20)
// and lets three of them clear DefaultMinContentChars (500). thinChars is below
// the non-trivial threshold, so a segment of thin events can never pass the
// min-content guard.
const (
	fatChars  = 200
	thinChars = 5
)

func cutEvent(epoch int64, chars int) db.EventRecord {
	return db.EventRecord{
		EventType:       "claude_assistant_turn",
		OccurredAtEpoch: epoch,
		Text:            strings.Repeat("x", chars),
	}
}

// fatRun returns n non-trivial events one second apart starting at epoch.
func fatRun(epoch int64, n int) []db.EventRecord {
	out := make([]db.EventRecord, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, cutEvent(epoch+int64(i), fatChars))
	}
	return out
}

func defaultPolicy() cutPolicy {
	return cutPolicy{
		GapSeconds:  defaultCutGapSeconds,
		MaxEvents:   defaultCutMaxEvents,
		MaxSegments: defaultCutMaxSegments,
	}
}

// assertPartition proves the load-bearing invariant: the cuts partition the
// events, so every event lands in exactly one segment and none is dropped.
// An event lost by a cut would never enter a run's input manifest and would
// therefore never be covered (memoire:D211).
func assertPartition(t *testing.T, events []db.EventRecord, cuts []int64) {
	t.Helper()
	seen := 0
	lo := int64(-1) // exclusive lower bound of the open segment
	for _, c := range cuts {
		if c <= lo {
			t.Fatalf("cuts must be strictly increasing: %v", cuts)
		}
		for _, e := range events {
			if e.OccurredAtEpoch > lo && e.OccurredAtEpoch <= c {
				seen++
			}
		}
		lo = c
	}
	for _, e := range events {
		if e.OccurredAtEpoch > lo {
			seen++
		}
	}
	if seen != len(events) {
		t.Fatalf("partition lost or duplicated events: %d of %d accounted for (cuts %v)", seen, len(events), cuts)
	}
}

// TestComputeCutPointsLongPause pins the long-pause class: a gap at or above
// the threshold closes a segment, and a tight cadence does not.
func TestComputeCutPointsLongPause(t *testing.T) {
	events := append(fatRun(1000, 3), fatRun(1000+defaultCutGapSeconds+3, 3)...)
	cuts := computeCutPoints(events, defaultPolicy())
	if len(cuts) != 1 || cuts[0] != 1002 {
		t.Fatalf("long pause: want one cut at 1002, got %v", cuts)
	}
	assertPartition(t, events, cuts)

	tight := fatRun(1000, 6)
	if cuts := computeCutPoints(tight, defaultPolicy()); cuts != nil {
		t.Fatalf("tight cadence must not cut, got %v", cuts)
	}

	// Exactly at the threshold cuts (the class is defined as >= G).
	atThreshold := append(fatRun(1000, 3), fatRun(1002+defaultCutGapSeconds, 3)...)
	if cuts := computeCutPoints(atThreshold, defaultPolicy()); len(cuts) != 1 {
		t.Fatalf("gap exactly at the threshold must cut, got %v", cuts)
	}
	// One second under the threshold does not.
	underThreshold := append(fatRun(1000, 3), fatRun(1002+defaultCutGapSeconds-1, 3)...)
	if cuts := computeCutPoints(underThreshold, defaultPolicy()); cuts != nil {
		t.Fatalf("gap under the threshold must not cut, got %v", cuts)
	}
}

// TestComputeCutPointsEventCountCap pins the "simply N new substantive events"
// class and that the count restarts at each cut.
func TestComputeCutPointsEventCountCap(t *testing.T) {
	events := fatRun(1000, 9)
	p := defaultPolicy()
	p.MaxEvents = 3
	cuts := computeCutPoints(events, p)
	if len(cuts) != 2 || cuts[0] != 1002 || cuts[1] != 1005 {
		t.Fatalf("count cap: want cuts [1002 1005], got %v", cuts)
	}
	assertPartition(t, events, cuts)
}

// TestComputeCutPointsNeverSplitsSameEpochSiblings pins invariant 2:
// occurred_at_epoch is whole seconds, so a boundary emitted on a duplicated
// epoch would drop the siblings from both sides of the [lo,c]/[c+1,hi]
// partition. The boundary must slide to the last sibling instead.
func TestComputeCutPointsNeverSplitsSameEpochSiblings(t *testing.T) {
	events := []db.EventRecord{
		cutEvent(1000, fatChars),
		cutEvent(1001, fatChars),
		cutEvent(1002, fatChars),
		cutEvent(1002, fatChars), // sibling sharing the candidate epoch
		cutEvent(1003, fatChars),
		cutEvent(1004, fatChars),
		cutEvent(1005, fatChars),
	}
	p := defaultPolicy()
	p.MaxEvents = 3
	cuts := computeCutPoints(events, p)
	if len(cuts) != 1 || cuts[0] != 1002 {
		t.Fatalf("tie handling: want one cut at 1002 (after both siblings), got %v", cuts)
	}
	assertPartition(t, events, cuts)

	// An all-same-epoch window can never be cut without splitting siblings.
	flat := []db.EventRecord{
		cutEvent(1000, fatChars), cutEvent(1000, fatChars), cutEvent(1000, fatChars),
		cutEvent(1000, fatChars), cutEvent(1000, fatChars), cutEvent(1000, fatChars),
	}
	if cuts := computeCutPoints(flat, p); cuts != nil {
		t.Fatalf("all-same-epoch window must not cut, got %v", cuts)
	}
}

// TestComputeCutPointsHonorsMinContentFloor pins invariant 3 and the coverage
// regression it prevents: a segment the min-content guard would skip writes no
// derivation_runs row, hence no input manifest, hence no coverage — and the
// same pause re-cuts identically on every later tick, so the fragment would be
// orphaned permanently.
func TestComputeCutPointsHonorsMinContentFloor(t *testing.T) {
	// Two envelope-noise events, then a long pause, then real content. The
	// gap is a genuine long pause but closing on it would orphan the pair.
	leading := []db.EventRecord{cutEvent(1000, thinChars), cutEvent(1001, thinChars)}
	events := append(leading, fatRun(1001+defaultCutGapSeconds+1, 4)...)
	if cuts := computeCutPoints(events, defaultPolicy()); cuts != nil {
		t.Fatalf("sub-floor leading segment must not be cut off, got %v", cuts)
	}

	// Same shape at the END: the trailing fragment merges backwards by
	// dropping the last cut.
	trailing := append(fatRun(1000, 4), cutEvent(1003+defaultCutGapSeconds+1, thinChars))
	if cuts := computeCutPoints(trailing, defaultPolicy()); cuts != nil {
		t.Fatalf("sub-floor trailing segment must merge backwards, got %v", cuts)
	}

	// A viable segment on both sides of the pause still cuts.
	viable := append(fatRun(1000, 3), fatRun(1002+defaultCutGapSeconds+1, 3)...)
	if cuts := computeCutPoints(viable, defaultPolicy()); len(cuts) != 1 {
		t.Fatalf("viable segments must cut, got %v", cuts)
	}

	// The floor tracks the guard, so a DISABLED guard removes it — the
	// detector and the orchestrator must never disagree about the floor.
	p := defaultPolicy()
	p.Guard = modelderive.WindowGuard{Disabled: true}
	cuts := computeCutPoints(events, p)
	if len(cuts) != 1 || cuts[0] != 1001 {
		t.Fatalf("guard disabled: want one cut at 1001, got %v", cuts)
	}
	assertPartition(t, events, cuts)
}

// TestComputeCutPointsSegmentCapMerges pins that cap overflow MERGES into the
// last segment. Dropping the overflow would leave events uncovered and would
// also hide from the sweep's all-legs-failed gate (a structural skip sits
// outside its counters).
func TestComputeCutPointsSegmentCapMerges(t *testing.T) {
	var events []db.EventRecord
	epoch := int64(1000)
	for group := 0; group < 5; group++ {
		events = append(events, fatRun(epoch, 3)...)
		epoch += 2 + defaultCutGapSeconds + 1
	}
	p := defaultPolicy()
	p.MaxSegments = 3
	cuts := computeCutPoints(events, p)
	if len(cuts) != 2 {
		t.Fatalf("segment cap 3: want 2 cuts (3 segments), got %v", cuts)
	}
	assertPartition(t, events, cuts)

	p.MaxSegments = 0 // unbounded
	if cuts := computeCutPoints(events, p); len(cuts) != 4 {
		t.Fatalf("unbounded: want 4 cuts, got %v", cuts)
	}
	p.MaxSegments = 1 // one segment means no cuts at all
	if cuts := computeCutPoints(events, p); cuts != nil {
		t.Fatalf("segment cap 1 must yield no cuts, got %v", cuts)
	}
}

// TestComputeCutPointsDegenerateInputs pins the off switches and the trivial
// windows: both classes disabled is a legal "rhythm cuts off" configuration.
func TestComputeCutPointsDegenerateInputs(t *testing.T) {
	events := append(fatRun(1000, 3), fatRun(1002+defaultCutGapSeconds+1, 3)...)
	off := cutPolicy{MaxSegments: defaultCutMaxSegments}
	if cuts := computeCutPoints(events, off); cuts != nil {
		t.Fatalf("both classes disabled must yield no cuts, got %v", cuts)
	}
	if cuts := computeCutPoints(nil, defaultPolicy()); cuts != nil {
		t.Fatalf("empty window: got %v", cuts)
	}
	if cuts := computeCutPoints(events[:1], defaultPolicy()); cuts != nil {
		t.Fatalf("single-event window: got %v", cuts)
	}
	gapOnly := cutPolicy{GapSeconds: defaultCutGapSeconds, MaxSegments: defaultCutMaxSegments}
	if cuts := computeCutPoints(events, gapOnly); len(cuts) != 1 {
		t.Fatalf("gap class alone must still cut, got %v", cuts)
	}
	countOnly := cutPolicy{MaxEvents: 3, MaxSegments: defaultCutMaxSegments}
	if cuts := computeCutPoints(fatRun(1000, 9), countOnly); len(cuts) != 2 {
		t.Fatalf("count class alone must still cut, got %v", cuts)
	}
}

// TestCLICutPointsValidation pins the helper's usage errors and its mutual
// exclusion with the other sweep helpers. None of these reach the store.
func TestCLICutPointsValidation(t *testing.T) {
	setupCLIEnv(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no project", []string{"-cut-points", "-session-id", "s"}, "requires -project"},
		{"no session", []string{"-cut-points", "-project", "p"}, "requires -project"},
		{"negative gap", []string{"-cut-points", "-project", "p", "-session-id", "s", "-cut-gap-seconds", "-1"}, "must not be negative"},
		{"negative events", []string{"-cut-points", "-project", "p", "-session-id", "s", "-cut-max-events", "-1"}, "must not be negative"},
		{"negative segments", []string{"-cut-points", "-project", "p", "-session-id", "s", "-cut-max-segments", "-1"}, "must not be negative"},
		{"inverted window", []string{"-cut-points", "-project", "p", "-session-id", "s", "-start-epoch", "500", "-end-epoch", "400"}, ">= -start-epoch"},
		{"exclusive with median", []string{"-cut-points", "-median-epoch", "-project", "p", "-session-id", "s"}, "mutually exclusive"},
		{"exclusive with list-targets", []string{"-cut-points", "-list-targets"}, "mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := run(tc.args, io.Discard, io.Discard)
			if err == nil {
				t.Fatalf("%v must error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}

	// Both classes disabled prints nothing and needs no store — the args
	// below name a project that was never seeded.
	var out bytes.Buffer
	if err := run([]string{"-cut-points", "-project", "nope", "-session-id", "nope",
		"-cut-gap-seconds", "0", "-cut-max-events", "0"}, &out, io.Discard); err != nil {
		t.Fatalf("rhythm cuts off must succeed: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("rhythm cuts off must print nothing, got %q", out.String())
	}
}

// TestCLICutPoints drives the helper end to end against a real scratch store:
// a session with one long pause yields one cut epoch, an unpinned window and a
// pinned window agree, and a session with no pause prints nothing.
func TestCLICutPoints(t *testing.T) {
	setupCLIEnv(t)
	// Prompts long enough to clear the min-content floor on both sides of the
	// pause (three events of ~200 chars each per segment).
	fat := strings.Repeat("substantive prompt text ", 9)
	epochs := []int64{1000, 1001, 1002, 1002 + defaultCutGapSeconds + 1, 1003 + defaultCutGapSeconds + 1, 1004 + defaultCutGapSeconds + 1}
	prompts := make([]string, 0, len(epochs))
	for i := range epochs {
		prompts = append(prompts, fmt.Sprintf("%s-%d", fat, i))
	}
	ids := seedSession(t, "cut-proj", "cut-sess", epochs, prompts)

	var out bytes.Buffer
	if err := run([]string{"-cut-points", "-project", "cut-proj", "-session-id", "cut-sess"}, &out, io.Discard); err != nil {
		t.Fatalf("-cut-points: %v", err)
	}
	got := strings.Fields(out.String())
	if len(got) != 1 {
		t.Fatalf("want one cut line, got %q", out.String())
	}
	if cut, err := strconv.ParseInt(got[0], 10, 64); err != nil || cut != 1002 {
		t.Fatalf("want cut at 1002, got %q (%v)", got[0], err)
	}

	// A pinned window that stops before the pause has no gap to cut on.
	out.Reset()
	if err := run([]string{"-cut-points", "-project", "cut-proj", "-session-id", "cut-sess",
		"-start-epoch", "1000", "-end-epoch", "1002"}, &out, io.Discard); err != nil {
		t.Fatalf("-cut-points pinned: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("pinned pre-pause window must print nothing, got %q", out.String())
	}

	// A different session with a tight cadence needs no cut.
	seedSession(t, "cut-proj", "cut-sess-tight", []int64{2000, 2001, 2002},
		[]string{fat + "-a", fat + "-b", fat + "-c"})
	out.Reset()
	if err := run([]string{"-cut-points", "-project", "cut-proj", "-session-id", "cut-sess-tight"}, &out, io.Discard); err != nil {
		t.Fatalf("-cut-points tight: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("tight session must print nothing, got %q", out.String())
	}

	// An unknown session is an empty window, not an error (the sweep's
	// fail-open contract, same as -median-epoch).
	out.Reset()
	if err := run([]string{"-cut-points", "-project", "cut-proj", "-session-id", "no-such-sess"}, &out, io.Discard); err != nil {
		t.Fatalf("-cut-points unknown session: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("unknown session must print nothing, got %q", out.String())
	}

	// Covered events are OUT of the population (memoire:D211): a completed
	// run's input manifest excludes its events from every derivation fetch, so
	// on a live session the stream the model sees is the uncovered stream — and
	// that is the stream whose rhythm the detector must read. Cover the
	// pre-pause events and the pause is no longer a boundary in what is left.
	coverEvents(t, "cut-proj", "cut-sess", ids[:3])
	out.Reset()
	if err := run([]string{"-cut-points", "-project", "cut-proj", "-session-id", "cut-sess"}, &out, io.Discard); err != nil {
		t.Fatalf("-cut-points after coverage: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("covered events must leave the pause outside the population, got %q", out.String())
	}
}

// coverEvents records a COMPLETED derivation run whose input manifest lists
// eventIDs, which is exactly what makes them covered for the derivation fetch
// family (memoire:D211). No provider call and no observations: coverage is a
// property of the manifest, not of the output.
func coverEvents(t *testing.T, project, sessionID string, eventIDs []int64) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	store, err := db.Open(cfg.DBPath, cfg.SchemaDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	res, err := store.EnsureDerivationRun(ctx, db.DerivationRunInput{
		Project: project, SessionID: sessionID, Provider: "local-qwen",
		Model: "qwen-7b", EndpointID: "hl3", DataBoundary: "local",
		DerivationVersion: "v2", InputHash: "cut-coverage-hash",
		WindowStartEpoch: 1, WindowEndEpoch: 2, Status: "running",
	})
	if err != nil {
		t.Fatalf("EnsureDerivationRun: %v", err)
	}
	if err := store.RecordRunInputEvents(ctx, res.ID, eventIDs); err != nil {
		t.Fatalf("RecordRunInputEvents: %v", err)
	}
	if err := store.UpdateDerivationRunStatus(ctx, db.DerivationRunStatusUpdate{
		ID: res.ID, Status: "completed", CompletedAtEpoch: 2,
	}); err != nil {
		t.Fatalf("UpdateDerivationRunStatus: %v", err)
	}
}
