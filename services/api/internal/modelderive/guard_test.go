// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"memoire/services/api/internal/db"
)

func promptEvent(id int64, text string) db.EventRecord {
	return db.EventRecord{
		ID:        id,
		EventType: "claude_UserPromptSubmit",
		Payload:   []byte(fmt.Sprintf(`{"prompt":%q}`, text)),
	}
}

func toolEvent(id int64, summary string) db.EventRecord {
	return db.EventRecord{
		ID:        id,
		EventType: "claude_PostToolUse",
		Payload:   []byte(fmt.Sprintf(`{"tool_name":"Bash","result_summary":%q}`, summary)),
	}
}

func TestEvaluateWindowGuardThresholds(t *testing.T) {
	long := strings.Repeat("substantive prompt text. ", 8) // 200 chars

	cases := []struct {
		name     string
		events   []db.EventRecord
		wantSkip bool
	}{
		{
			name:     "three long prompts pass both gates",
			events:   []db.EventRecord{promptEvent(1, long), promptEvent(2, long), promptEvent(3, long)},
			wantSkip: false,
		},
		{
			name:     "two long prompts fail the event gate despite enough chars",
			events:   []db.EventRecord{promptEvent(1, long), promptEvent(2, long)},
			wantSkip: true,
		},
		{
			name: "three short prompts fail the char gate (the a7312b73 shape)",
			events: []db.EventRecord{
				promptEvent(1, strings.Repeat("x", 90)),
				promptEvent(2, strings.Repeat("y", 90)),
				promptEvent(3, strings.Repeat("z", 90)),
			},
			wantSkip: true,
		},
		{
			name: "trivial events do not count toward the event gate",
			events: []db.EventRecord{
				promptEvent(1, long), promptEvent(2, long),
				promptEvent(3, "y"),         // sub-20-char prompt is trivial
				toolEvent(4, ""),            // tool call with no summary is trivial
				{ID: 5, EventType: "other"}, // unknown type contributes nothing
			},
			wantSkip: true,
		},
		{
			name: "tool result summaries count as content",
			events: []db.EventRecord{
				promptEvent(1, long),
				toolEvent(2, long),
				toolEvent(3, long),
			},
			wantSkip: false,
		},
		{
			name: "assistant turn text column counts as content",
			events: []db.EventRecord{
				promptEvent(1, long),
				promptEvent(2, long),
				{ID: 3, EventType: "claude_assistant_turn", Text: long},
			},
			wantSkip: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateWindowGuard(tc.events, WindowGuard{})
			if got.Skip != tc.wantSkip {
				t.Fatalf("Skip = %v, want %v (measured %+v)", got.Skip, tc.wantSkip, got)
			}
		})
	}
}

func TestEvaluateWindowGuardExactThresholdPasses(t *testing.T) {
	// Exactly 3 non-trivial events and exactly 500 chars must pass: the gates
	// are strict less-than.
	events := []db.EventRecord{
		promptEvent(1, strings.Repeat("a", 200)),
		promptEvent(2, strings.Repeat("b", 200)),
		promptEvent(3, strings.Repeat("c", 100)),
	}
	got := EvaluateWindowGuard(events, WindowGuard{})
	if got.ContentChars != 500 || got.NonTrivialEvents != 3 {
		t.Fatalf("measured %+v, want exactly 3 events / 500 chars", got)
	}
	if got.Skip {
		t.Fatalf("exact-threshold window must not skip: %+v", got)
	}
}

func TestEvaluateWindowGuardOverridesAndDisable(t *testing.T) {
	events := []db.EventRecord{promptEvent(1, strings.Repeat("a", 30))}

	if got := EvaluateWindowGuard(events, WindowGuard{MinNonTrivialEvents: 1, MinContentChars: 30}); got.Skip {
		t.Fatalf("loosened thresholds must pass: %+v", got)
	}
	if got := EvaluateWindowGuard(events, WindowGuard{Disabled: true}); got.Skip {
		t.Fatalf("disabled guard must never skip")
	} else if got.ContentChars != 30 || got.NonTrivialEvents != 1 {
		t.Fatalf("disabled guard must still measure, got %+v", got)
	}
}

// TestOrchestratorSkipsDegenerateWindow drives the guard through Run: a
// degenerate window returns a skip marker without calling the provider and
// without creating a derivation run.
func TestOrchestratorSkipsDegenerateWindow(t *testing.T) {
	dbPath, store := openTestStore(t)
	seedPrompt(t, store, "sess-degenerate", "src-tiny-1", 1000, "y")
	seedPrompt(t, store, "sess-degenerate", "src-tiny-2", 1100, "ok proceed")

	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		t.Fatalf("provider must not be called for a degenerate window")
		return ProviderResponse{}, nil
	})
	o := &Orchestrator{Store: store, Timeout: 2 * time.Second, Now: func() int64 { return 1_700_000_000 }}

	out, err := o.Run(context.Background(), provider, orchRequest("sess-degenerate"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Skipped != SkipReasonDegenerateWindow {
		t.Fatalf("Skipped = %q, want %q", out.Skipped, SkipReasonDegenerateWindow)
	}
	if out.RunID != "" || out.Status != "" {
		t.Fatalf("skip must not carry a run id/status, got %+v", out)
	}
	if !out.Guard.Skip || out.Guard.NonTrivialEvents != 0 || out.Guard.ContentChars >= DefaultMinContentChars {
		t.Fatalf("unexpected guard measurements: %+v", out.Guard)
	}

	sqlDB, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer sqlDB.Close()
	var runs int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM derivation_runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("degenerate skip must not create a derivation run, found %d", runs)
	}
}

// TestOrchestratorPassesSubstantiveWindow pins the complement: a window above
// both thresholds proceeds to the provider under the default guard.
func TestOrchestratorPassesSubstantiveWindow(t *testing.T) {
	_, store := openTestStore(t)
	long := strings.Repeat("the operator ruled the api ships in Go. ", 5)
	seedPrompt(t, store, "sess-real", "src-real-1", 1000, long)
	seedPrompt(t, store, "sess-real", "src-real-2", 1100, long)
	seedPrompt(t, store, "sess-real", "src-real-3", 1200, long)

	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return validResponse(req), nil
	})
	o := &Orchestrator{Store: store, Timeout: 2 * time.Second, Now: func() int64 { return 1_700_000_000 }}

	out, err := o.Run(context.Background(), provider, orchRequest("sess-real"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Skipped != "" {
		t.Fatalf("substantive window must not skip: %+v", out)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
	if out.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", out.Status)
	}
}
