// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"strings"

	"memoire/services/api/internal/db"
)

// Min-content window guard (operator brief 2026-07-10, sweep hygiene).
//
// Degenerate windows — skill-pack smoke sessions, near-empty captures — cost a
// full provider call and can never yield memory; a ~270-char window hung a
// local model to context-deadline twice. The guard measures the window's
// meaningful content and skips the run before any run record or provider call.
// No derivation_runs row is written for a skip (the run-status vocabulary is
// sealed to the contract states), so the guard re-evaluates cheaply on every
// sweep until the window grows past the thresholds.

const (
	// DefaultMinNonTrivialEvents is the minimum number of non-trivial events a
	// window needs to be worth a provider call.
	DefaultMinNonTrivialEvents = 3
	// DefaultMinContentChars is the minimum total extracted-text length (bytes)
	// across the window's events.
	DefaultMinContentChars = 500
	// nonTrivialEventChars is the per-event extracted-text length at or above
	// which an event counts as non-trivial. Below it an event is envelope noise
	// (a bare "y", a tool call with no summary).
	nonTrivialEventChars = 20
)

// SkipReasonFullyCovered is the RunOutcome.Skipped marker for a window whose
// every canonical event already sits in a completed run's input manifest:
// nothing uncovered to derive, no run row written, no provider call. The
// normal end state of a finished session; -force-rerun bypasses it.
const SkipReasonFullyCovered = "fully_covered"

// SkipReasonDegenerateWindow is the RunOutcome.Skipped marker for a window
// skipped by the min-content guard.
const SkipReasonDegenerateWindow = "degenerate_window"

// WindowGuard configures the min-content guard. Zero thresholds fall back to
// the package defaults; Disabled turns the guard off entirely.
type WindowGuard struct {
	MinNonTrivialEvents int
	MinContentChars     int
	Disabled            bool
}

// GuardResult reports what the guard measured and the thresholds it applied,
// so a skip marker in CLI output/logs is self-explanatory.
type GuardResult struct {
	Skip                bool `json:"skip"`
	NonTrivialEvents    int  `json:"non_trivial_events"`
	ContentChars        int  `json:"content_chars"`
	MinNonTrivialEvents int  `json:"min_non_trivial_events"`
	MinContentChars     int  `json:"min_content_chars"`
}

// EvaluateWindowGuard measures a window's meaningful content against the
// guard thresholds. A disabled guard still measures (the numbers are useful
// diagnostics) but never asks for a skip.
func EvaluateWindowGuard(events []db.EventRecord, g WindowGuard) GuardResult {
	res := GuardResult{
		MinNonTrivialEvents: g.MinNonTrivialEvents,
		MinContentChars:     g.MinContentChars,
	}
	if res.MinNonTrivialEvents <= 0 {
		res.MinNonTrivialEvents = DefaultMinNonTrivialEvents
	}
	if res.MinContentChars <= 0 {
		res.MinContentChars = DefaultMinContentChars
	}
	for _, e := range events {
		n := len(eventContentText(e))
		res.ContentChars += n
		if n >= nonTrivialEventChars {
			res.NonTrivialEvents++
		}
	}
	if g.Disabled {
		return res
	}
	res.Skip = res.NonTrivialEvents < res.MinNonTrivialEvents || res.ContentChars < res.MinContentChars
	return res
}

// eventContentText extracts the meaningful text of one event the way the
// prompt renderer would: user prompt text, tool result summaries, assistant
// prose. Tool names and envelope metadata are not content.
func eventContentText(e db.EventRecord) string {
	switch e.EventType {
	case "claude_UserPromptSubmit":
		return strings.TrimSpace(payloadString(e.Payload, "prompt", "user_prompt", "userPrompt", "message", "text"))
	case "claude_PostToolUse":
		return strings.TrimSpace(payloadString(e.Payload, "result_summary", "resultSummary", "summary", "result"))
	case "claude_assistant_turn", "codex_assistant_turn", "qwen_assistant_turn", "opencode_assistant_turn", "pi_assistant_turn":
		// Assistant turns carry their prose in the text column, not payload.
		return strings.TrimSpace(e.Text)
	}
	return ""
}
