// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"memoire/services/api/internal/db"
	"memoire/services/api/internal/modelderive"
)

// Rhythm cuts (Fibery #343, operator ruling 2026-09-08). A derivation window
// used to be a pure TIME SLICE: the sweep dry-ran a whole session and, when the
// prompt exceeded the endpoint's byte ceiling, halved it on the median
// canonical-event epoch. A window therefore had no idea whether it held a whole
// arc or the middle of one. Since memoire:D260 made transcript capture a stream
// of checkpoints, the store holds live input worth cutting at the session's own
// rhythm instead.
//
// The cut classes shipped here are the MECHANICAL ones only:
//
//   - LONG PAUSE — a gap of at least GapSeconds between consecutive canonical
//     events. The operator stopped; the arc ended.
//   - EVENT-COUNT CAP — MaxEvents substantive events since the last cut. This
//     is the ruling's "simply N new substantive events" clause, and it also
//     bounds prompt growth semantically before the byte ceiling does it blindly.
//   - SESSION END — deliberately NOT a class. It is just "the last event so
//     far": the trailing segment ends at the end of the window, pinned or not.
//
// Topic-shift detection is OUT of this slice (it needs a model or an
// embedding), and a COMPACT CHECKPOINT cannot be a class yet: nothing in the
// store marks a compaction. The PreCompact hook records the cause in its log
// line only (scripts/memoire-derive-auto.sh), and the transcript adapter
// captures `.type == "assistant"` lines exclusively, so the JSONL
// `compact_boundary` marker never becomes an event. No marker was invented for
// it here.
//
// The byte ceiling stays the hard constraint: rhythm cuts are the FIRST
// splitter, and a segment that still exceeds the ceiling falls back to the
// existing median-epoch halving inside that segment. Never the reverse.
//
// Each segment is derived as its own persisted run with its own input manifest,
// so memoire:D211 coverage semantics are untouched — a completed run covers
// exactly the events it consumed, whatever the window bounds were — and
// memoire:D212's "Already Captured (this session)" prompt section is the
// cross-cut context, unchanged: it is scoped by (project, session_id) with no
// window predicate, so the previous cut's observations are already in the next
// cut's prompt.

const (
	// defaultCutGapSeconds is the long-pause threshold: 30 minutes.
	//
	// Justification. It must sit well above capture cadence so a pause can
	// never be a capture artifact: the quiet timer that streams assistant
	// turns runs on a 10-minute cadence with a 120 s file-quiet gate
	// (scripts/memoire-capture-tick.sh), and the sweep's own bounded waits are
	// smaller still (300 s capture, 15 m execute). It must also sit above the
	// longest plausible single turn — a slow local model, a long build, a
	// document read — which 10 minutes does not reliably clear. 30 minutes is
	// the conventional inactivity boundary for "the human left", it is 3x the
	// streaming cadence, and it is the upper end of the range the ruling's
	// brief called plausible (10-30 minutes).
	//
	// This is a CALIBRATION default in the memoire:D72 sense, not a measured
	// one: no gap distribution was read (this slice had no live-store access).
	// Revisit it against the first week of real rhythm-cut legs.
	defaultCutGapSeconds int64 = 1800

	// defaultCutMaxEvents is the event-count cap: 120 canonical events since
	// the last cut.
	//
	// Justification. It is deliberately a BACKSTOP in the first calibration
	// window rather than the primary cutter, because a count cap tight enough
	// to cut typical arcs would change the lane's provider-call count rather
	// than merely relocate its boundaries. At the deriver profile's ceiling
	// (43,929 bytes on the scheduled lane), 120 canonical events implies an
	// average under 366 rendered bytes per event; real windows are richer than
	// that — a single assistant turn runs to thousands of bytes — so in
	// practice the byte ceiling binds first and this cap only catches long,
	// low-content marathons that halving would otherwise cut mid-story.
	// Recalibrate from the real events-per-leg distribution (memoire:D72
	// pattern) once rhythm-cut legs exist.
	defaultCutMaxEvents = 120

	// defaultCutMaxSegments bounds how many rhythm segments one session yields
	// in one pass: 0, unbounded.
	//
	// Justification. Rhythm cuts always converge: every segment is non-empty
	// and strictly smaller. The sweep's per-session executed-leg budget
	// (SESSION_LEG_BUDGET in scripts/memoire-derive-sweep.sh) is the cost
	// governor; it counts executed legs only and leaves the remainder for the
	// next tick, logged. A segment cap on top of it does not bound cost. It
	// only merges the overflow into one large tail that halving then cuts
	// blind. The first live tick showed it (2026-09-10, Fibery #351): a
	// 92-hour, 1.58 MB tail entered halving at depth 1 with 32 leaves and
	// stranded 30 legs above the ceiling. Unbounded, every segment stays
	// rhythm-shaped and under the event-count cap; the operator can still
	// bound the pass with -cut-max-segments (overflow merges, never drops).
	defaultCutMaxSegments = 0
)

// cutPolicy configures one rhythm pass. A non-positive GapSeconds or MaxEvents
// disables that class; both disabled means no cuts at all. A non-positive
// MaxSegments means unbounded. Guard is the min-content guard the orchestrator
// will apply to each segment, which the floor pass below consults.
type cutPolicy struct {
	GapSeconds  int64
	MaxEvents   int
	MaxSegments int
	Guard       modelderive.WindowGuard
}

// computeCutPoints returns the ordered cut epochs for events, which MUST be the
// window's live, uncovered, canonical-input events in (occurred_at_epoch, id)
// order — exactly the population the prompt builder admits and the derivation
// fetch returns. A returned epoch c means "close a segment at c": the caller
// derives [lo, c] and continues from c+1.
//
// Three invariants make the result safe to hand to the sweep:
//
//  1. EVERY event lands in exactly one segment. Cuts partition the window;
//     nothing is dropped, so nothing loses its chance at a manifest.
//  2. No cut splits SAME-EPOCH siblings. occurred_at_epoch is whole seconds and
//     canonical events routinely share one, so a boundary is only emitted where
//     the next event's epoch is strictly greater — otherwise the [lo, c] /
//     [c+1, hi] partition would drop the siblings from both sides.
//  3. No cut creates a segment the min-content guard would SKIP. A guard skip
//     writes no derivation_runs row (services/api/internal/modelderive/guard.go),
//     hence no input manifest, hence no coverage — and because the same pause
//     re-cuts identically on every later tick, an orphaned fragment would stay
//     uncovered forever with its notify intent open (memoire:D245). So a
//     candidate cut is emitted only when the segment it closes survives the
//     guard; otherwise the segment keeps growing. The trailing segment gets the
//     same treatment by dropping cuts from the end until the tail survives.
func computeCutPoints(events []db.EventRecord, p cutPolicy) []int64 {
	if len(events) < 2 {
		return nil
	}
	if p.GapSeconds <= 0 && p.MaxEvents <= 0 {
		return nil
	}
	if p.MaxSegments > 0 && p.MaxSegments < 2 {
		return nil
	}

	// bounds holds indices i such that a segment closes AFTER events[i].
	var bounds []int
	segStart := 0
	for i := 0; i < len(events)-1; i++ {
		gap := events[i+1].OccurredAtEpoch - events[i].OccurredAtEpoch
		longPause := p.GapSeconds > 0 && gap >= p.GapSeconds
		countCap := p.MaxEvents > 0 && i-segStart+1 >= p.MaxEvents
		if !longPause && !countCap {
			continue
		}
		// Invariant 2: never partition same-epoch siblings.
		if events[i+1].OccurredAtEpoch <= events[i].OccurredAtEpoch {
			continue
		}
		// Invariant 3: only close a segment the guard would derive.
		if modelderive.EvaluateWindowGuard(events[segStart:i+1], p.Guard).Skip {
			continue
		}
		bounds = append(bounds, i)
		segStart = i + 1
	}
	if len(bounds) == 0 {
		return nil
	}

	// Segment cap: fold the remainder into the LAST segment rather than
	// dropping it. Truncating can only make the tail larger, so it runs before
	// the tail floor pass below.
	if p.MaxSegments > 0 && len(bounds) > p.MaxSegments-1 {
		bounds = bounds[:p.MaxSegments-1]
	}

	// Tail floor: drop cuts from the end until the trailing segment survives
	// the guard, merging the fragment backwards into its predecessor.
	for len(bounds) > 0 {
		tailStart := bounds[len(bounds)-1] + 1
		if !modelderive.EvaluateWindowGuard(events[tailStart:], p.Guard).Skip {
			break
		}
		bounds = bounds[:len(bounds)-1]
	}
	if len(bounds) == 0 {
		return nil
	}

	cuts := make([]int64, 0, len(bounds))
	for _, i := range bounds {
		cuts = append(cuts, events[i].OccurredAtEpoch)
	}
	return cuts
}
