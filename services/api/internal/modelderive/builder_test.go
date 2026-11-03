// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"memoire/services/api/internal/db"
)

func userPromptEvent(id, epoch int64, prompt string) db.EventRecord {
	payload, _ := json.Marshal(map[string]any{"prompt": prompt})
	return db.EventRecord{
		ID:              id,
		SessionID:       "session-1",
		Project:         "memoire",
		SourceAdapter:   "claude-code",
		SourceEventID:   "src-" + prompt,
		EventType:       "claude_UserPromptSubmit",
		Payload:         payload,
		OccurredAtEpoch: epoch,
	}
}

func baseRequest(events []db.EventRecord) Request {
	return Request{
		Provider:          "local-qwen",
		Model:             "qwen-7b",
		DerivationVersion: "observation-derive-v1",
		PromptVersion:     "derive-observations-v1",
		OutputSchema:      "1",
		Window: Window{
			Project:    "memoire",
			SessionID:  "session-1",
			StartEpoch: 1000,
			EndEpoch:   2000,
		},
		Events: events,
	}
}

func TestPromptBuilderEventOrder(t *testing.T) {
	events := []db.EventRecord{
		userPromptEvent(3, 200, "third"),
		userPromptEvent(1, 100, "first"),
		userPromptEvent(2, 100, "second"),
	}
	prompt, _, err := BuildPrompt(baseRequest(events))
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	pFirst := strings.Index(prompt, "first")
	pSecond := strings.Index(prompt, "second")
	pThird := strings.Index(prompt, "third")
	if !(pFirst < pSecond && pSecond < pThird) {
		t.Fatalf("events not ordered by (occurred_at_epoch, id): first=%d second=%d third=%d", pFirst, pSecond, pThird)
	}
}

func TestPromptContainsSealedKinds(t *testing.T) {
	prompt, _, err := BuildPrompt(baseRequest([]db.EventRecord{userPromptEvent(1, 100, "hi")}))
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	for _, k := range SealedKinds() {
		if !strings.Contains(prompt, k.Name) {
			t.Errorf("prompt missing sealed kind name %q", k.Name)
		}
		if !strings.Contains(prompt, k.Definition) {
			t.Errorf("prompt missing verbatim definition for %q", k.Name)
		}
	}
}

func TestPromptContainsGotchaDisambiguation(t *testing.T) {
	prompt, _, err := BuildPrompt(baseRequest([]db.EventRecord{userPromptEvent(1, 100, "hi")}))
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if !strings.Contains(prompt, "the best-fit kind from the seven") {
		t.Errorf("prompt should reference seven kinds")
	}
	if !strings.Contains(prompt, "Consider `gotcha` before `unknown`") {
		t.Errorf("prompt should instruct the model to consider gotcha before unknown")
	}
}

func TestPromptContainsAssistantTurnLimitation(t *testing.T) {
	prompt, _, err := BuildPrompt(baseRequest([]db.EventRecord{userPromptEvent(1, 100, "hi")}))
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if !strings.Contains(prompt, AssistantTurnLimitation) {
		t.Fatalf("prompt missing v1 assistant-turn limitation")
	}
}

func TestPromptContainsSecretHandling(t *testing.T) {
	prompt, _, err := BuildPrompt(baseRequest([]db.EventRecord{userPromptEvent(1, 100, "hi")}))
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if !strings.Contains(prompt, SecretHandlingGuidance) {
		t.Fatalf("prompt missing secret-handling guidance")
	}
	// Must not claim hard redaction.
	lower := strings.ToLower(prompt)
	if strings.Contains(lower, "we redact") || strings.Contains(lower, "secrets are removed") {
		t.Fatalf("prompt makes an unauthorized hard-redaction claim")
	}
}

func TestPromptBuilderExcludesDerivedRows(t *testing.T) {
	events := []db.EventRecord{
		userPromptEvent(1, 100, "kept-prompt"),
		{
			ID:              2,
			Project:         "memoire",
			SessionID:       "session-1",
			SourceAdapter:   "memoire",
			EventType:       "derived_observation",
			Payload:         json.RawMessage(`{"text":"EXCLUDED-DERIVED-TEXT"}`),
			OccurredAtEpoch: 150,
		},
		{
			ID:              3,
			Project:         "memoire",
			SessionID:       "session-1",
			SourceAdapter:   "memoire",
			EventType:       "audit_log",
			Payload:         json.RawMessage(`{"text":"EXCLUDED-AUDIT-TEXT"}`),
			OccurredAtEpoch: 160,
		},
	}
	prompt, meta, err := BuildPrompt(baseRequest(events))
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if strings.Contains(prompt, "EXCLUDED-DERIVED-TEXT") {
		t.Errorf("prompt included derived observation text")
	}
	if strings.Contains(prompt, "EXCLUDED-AUDIT-TEXT") {
		t.Errorf("prompt included audit row text")
	}
	if !strings.Contains(prompt, "kept-prompt") {
		t.Errorf("prompt dropped a canonical input event")
	}
	if got := meta["input_event_count"].(int); got != 1 {
		t.Errorf("input_event_count = %d, want 1", got)
	}
}

func assistantTurnEvent(id, epoch int64, eventType, text string) db.EventRecord {
	return db.EventRecord{
		ID:              id,
		SessionID:       "session-1",
		Project:         "memoire",
		SourceAdapter:   "claude_code_transcript_backfill",
		SourceEventID:   "turn-src",
		EventType:       eventType,
		Text:            text,
		OccurredAtEpoch: epoch,
	}
}

// The turn-ingest campaign's headliner (Phase 1, 2026-07-11): assistant turns
// from all three harnesses are canonical input and their prose reaches the
// prompt body. The 2026-07-10 backfill produced events that never reached a
// single prompt because the canonical set filtered them out.
func TestPromptIncludesAssistantTurns(t *testing.T) {
	for _, eventType := range []string{"claude_assistant_turn", "codex_assistant_turn", "qwen_assistant_turn", "opencode_assistant_turn", "pi_assistant_turn"} {
		if !canonicalInputEventTypes[eventType] {
			t.Errorf("canonical input set must admit %q", eventType)
		}
	}

	events := []db.EventRecord{
		userPromptEvent(1, 100, "user-question"),
		assistantTurnEvent(2, 110, "claude_assistant_turn", "CC-TURN verdict: all 8 tests pass"),
		assistantTurnEvent(3, 120, "codex_assistant_turn", "CODEX-TURN merged as abc123"),
		assistantTurnEvent(4, 130, "qwen_assistant_turn", "QWEN-TURN smoke green"),
		assistantTurnEvent(5, 140, "opencode_assistant_turn", "OPENCODE-TURN parity probe verbatim"),
		assistantTurnEvent(6, 150, "pi_assistant_turn", "PI-TURN live capture"),
	}
	prompt, meta, err := BuildPrompt(baseRequest(events))
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	for _, want := range []string{"CC-TURN verdict: all 8 tests pass", "CODEX-TURN merged as abc123", "QWEN-TURN smoke green", "OPENCODE-TURN parity probe verbatim", "PI-TURN live capture"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing assistant-turn text %q", want)
		}
	}
	if got := meta["input_event_count"].(int); got != 6 {
		t.Errorf("input_event_count = %d, want 6 (assistant turns are canonical input)", got)
	}

	window := meta["window"].(map[string]any)
	gotTypes := window["event_types"].([]string)
	wantTypes := CanonicalInputEventTypes()
	if len(gotTypes) != len(wantTypes) {
		t.Fatalf("meta event_types = %v, want %v", gotTypes, wantTypes)
	}
	for i := range wantTypes {
		if gotTypes[i] != wantTypes[i] {
			t.Fatalf("meta event_types = %v, want %v", gotTypes, wantTypes)
		}
	}
}

func TestPromptBuilderMaxInputEvents(t *testing.T) {
	events := []db.EventRecord{
		userPromptEvent(1, 100, "a"),
		userPromptEvent(2, 200, "b"),
		userPromptEvent(3, 300, "c"),
	}
	req := baseRequest(events)
	req.Limits.MaxInputEvents = 2
	_, _, err := BuildPrompt(req)
	if err == nil {
		t.Fatalf("expected fail-closed error when input events exceed MaxInputEvents")
	}
	if !strings.Contains(err.Error(), "MaxInputEvents") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPromptBuilderMaxPromptBytes(t *testing.T) {
	req := baseRequest([]db.EventRecord{userPromptEvent(1, 100, strings.Repeat("x", 500))})
	req.Limits.MaxPromptBytes = 50
	_, _, err := BuildPrompt(req)
	if err == nil {
		t.Fatalf("expected fail-closed error when prompt exceeds MaxPromptBytes")
	}
	if !strings.Contains(err.Error(), "MaxPromptBytes") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPromptBuilderReturnsMeta(t *testing.T) {
	prompt, meta, err := BuildPrompt(baseRequest([]db.EventRecord{userPromptEvent(1, 100, "hi")}))
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if meta == nil {
		t.Fatalf("expected non-nil meta")
	}
	if meta["provider"] != "local-qwen" {
		t.Errorf("meta provider = %v", meta["provider"])
	}
	if meta["response_schema_version"] != SchemaVersion {
		t.Errorf("meta response_schema_version = %v", meta["response_schema_version"])
	}
	if meta["prompt_bytes"].(int) != len(prompt) {
		t.Errorf("meta prompt_bytes mismatch")
	}
}

func TestPromptAlreadyCapturedSection(t *testing.T) {
	// Prompt v2.4: the already-captured section renders kind/state-tagged
	// entries with the do-not-re-emit instruction, and the prompt meta
	// carries the entry count. An empty list renders nothing.
	req := baseRequest([]db.EventRecord{userPromptEvent(1, 100, "hi")})
	prompt, meta, err := BuildPrompt(req)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if strings.Contains(prompt, "## Already Captured") {
		t.Fatal("section must be absent without entries")
	}
	if got, _ := meta["already_captured_count"].(int); got != 0 {
		t.Fatalf("already_captured_count = %v, want 0", meta["already_captured_count"])
	}

	req.AlreadyCaptured = []db.SessionObservationSummary{
		{ID: 9, Kind: "decision", Text: "chose Go for the API", ReviewState: "accepted"},
		{ID: 8, Kind: "fact", Text: "hub runs on HL3", ReviewState: "lapsed"},
	}
	prompt, meta, err = BuildPrompt(req)
	if err != nil {
		t.Fatalf("BuildPrompt with context: %v", err)
	}
	for _, want := range []string{
		"## Already Captured (this session)",
		"Do not re-emit them or",
		"- [decision/accepted] chose Go for the API",
		"- [fact/lapsed] hub runs on HL3",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
	if got, _ := meta["already_captured_count"].(int); got != 2 {
		t.Fatalf("already_captured_count = %v, want 2", meta["already_captured_count"])
	}
	// The section must precede the source events so instructions read as
	// context, not as trailing noise after the evidence.
	if strings.Index(prompt, "## Already Captured") > strings.Index(prompt, "## Source Events") {
		t.Fatal("already-captured section must precede source events")
	}
}

func TestPromptContextYieldsToMaxPromptBytes(t *testing.T) {
	// Round-1 finding 2: the advisory context must never push a window that
	// fits on its own past MaxPromptBytes. Base prompt near the cap ->
	// section shrinks or disappears, build succeeds; base alone over the
	// cap -> the typed refusal.
	events := []db.EventRecord{userPromptEvent(1, 100, strings.Repeat("window content ", 200))}
	req := baseRequest(events)
	base, _, err := BuildPrompt(req)
	if err != nil {
		t.Fatalf("uncapped build: %v", err)
	}

	req.AlreadyCaptured = []db.SessionObservationSummary{
		{ID: 1, Kind: "fact", Text: strings.Repeat("captured fact ", 30), ReviewState: "accepted"},
		{ID: 2, Kind: "fact", Text: strings.Repeat("other fact ", 30), ReviewState: "proposed"},
	}
	// Cap just above the base: no room for the section, but the window must
	// still build.
	req.Limits.MaxPromptBytes = len(base) + 10
	prompt, meta, err := BuildPrompt(req)
	if err != nil {
		t.Fatalf("context must yield to the cap, got error: %v", err)
	}
	if len(prompt) > req.Limits.MaxPromptBytes {
		t.Fatalf("prompt %d exceeds cap %d", len(prompt), req.Limits.MaxPromptBytes)
	}
	if strings.Contains(prompt, "## Already Captured") {
		t.Fatal("section must be fully yielded away at zero headroom")
	}
	if got, _ := meta["already_captured_truncated"].(bool); !got {
		t.Fatal("yielded-away section must report truncated")
	}
	if got, _ := meta["already_captured_available"].(int); got != 2 {
		t.Fatalf("available context must stay visible in meta: %v", meta["already_captured_available"])
	}

	// Generous cap: the full section fits.
	req.Limits.MaxPromptBytes = len(base) + 4096
	prompt, meta, err = BuildPrompt(req)
	if err != nil {
		t.Fatalf("generous cap: %v", err)
	}
	if !strings.Contains(prompt, "## Already Captured") {
		t.Fatal("section must render when headroom allows")
	}
	if got, _ := meta["already_captured_count"].(int); got != 2 {
		t.Fatalf("expected both entries rendered: %v", meta["already_captured_count"])
	}

	// The window alone over the cap: typed refusal, so the dry-run can
	// distinguish window-too-big from other failures.
	req.Limits.MaxPromptBytes = len(base) - 1
	if _, _, err := BuildPrompt(req); !errors.Is(err, ErrPromptExceedsMaxBytes) {
		t.Fatalf("want ErrPromptExceedsMaxBytes, got %v", err)
	}
}

// --- collapsed content-less runs (prompt v2.5, Fibery #365) -----------------

// toolEvent builds a claude_PostToolUse event in the shape the claude-code
// hook actually produces: tool_name and tool_input, NO result-summary key. No
// adapter in this repo writes result_summary/resultSummary/summary/result, so
// eventContentText measures every such event as zero content — which is why
// they render collapsed.
func contentLessToolEvent(id, epoch int64, tool string) db.EventRecord {
	payload, _ := json.Marshal(map[string]any{
		"tool_name":  tool,
		"tool_input": map[string]any{"file_path": "/some/path"},
	})
	return db.EventRecord{
		ID:              id,
		SessionID:       "session-1",
		Project:         "memoire",
		SourceAdapter:   "claude-code",
		SourceEventID:   fmt.Sprintf("claude-posttooluse-%d", id),
		EventType:       "claude_PostToolUse",
		Payload:         payload,
		OccurredAtEpoch: epoch,
	}
}

func TestPromptCollapsesContentLessRuns(t *testing.T) {
	// [content, toolless x3, content, toolless, content] -> exactly two
	// collapsed blocks, the content events untouched and still in order.
	events := []db.EventRecord{
		userPromptEvent(1, 100, "ALPHA-CONTENT"),
		contentLessToolEvent(2, 110, "Read"),
		contentLessToolEvent(3, 120, "Grep"),
		contentLessToolEvent(4, 130, "Read"),
		userPromptEvent(5, 140, "BETA-CONTENT"),
		contentLessToolEvent(6, 150, "Bash"),
		userPromptEvent(7, 160, "GAMMA-CONTENT"),
	}
	prompt, meta, err := BuildPrompt(baseRequest(events))
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	if n := strings.Count(prompt, "\n### events "); n != 2 {
		t.Fatalf("collapsed block count in prompt = %d, want 2; prompt:\n%s", n, prompt)
	}
	for _, want := range []string{
		"### events 2..4 (3 events, no captured content)\n- occurred_at_epoch: 110..130\n- event_types: claude_PostToolUse x3\n- tools: Read x2, Grep x1\n",
		"### events 6..6 (1 event, no captured content)\n- occurred_at_epoch: 150..150\n- event_types: claude_PostToolUse x1\n- tools: Bash x1\n",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing collapsed block %q; prompt:\n%s", want, prompt)
		}
	}
	// No per-event block for any collapsed id.
	for _, id := range []int{2, 3, 4, 6} {
		if header := fmt.Sprintf("### event %d\n", id); strings.Contains(prompt, header) {
			t.Fatalf("collapsed id %d still has a per-event block", id)
		}
	}
	// Content events keep their per-event block and their order around the
	// collapsed blocks.
	for _, id := range []int{1, 5, 7} {
		if header := fmt.Sprintf("### event %d\n", id); !strings.Contains(prompt, header) {
			t.Fatalf("content-bearing event %d lost its per-event block", id)
		}
	}
	pAlpha := strings.Index(prompt, "ALPHA-CONTENT")
	pFirstBlock := strings.Index(prompt, "### events 2..4")
	pBeta := strings.Index(prompt, "BETA-CONTENT")
	pSecondBlock := strings.Index(prompt, "### events 6..6")
	pGamma := strings.Index(prompt, "GAMMA-CONTENT")
	if !(pAlpha < pFirstBlock && pFirstBlock < pBeta && pBeta < pSecondBlock && pSecondBlock < pGamma) {
		t.Fatalf("section order broken: alpha=%d block1=%d beta=%d block2=%d gamma=%d",
			pAlpha, pFirstBlock, pBeta, pSecondBlock, pGamma)
	}
	// The intro must tell the model what the block is.
	if !strings.Contains(prompt, collapsedIntro) {
		t.Fatal("Source Events intro missing the collapsed-block sentence")
	}

	if got, _ := meta["collapsed_event_count"].(int); got != 4 {
		t.Fatalf("collapsed_event_count = %v, want 4", meta["collapsed_event_count"])
	}
	if got, _ := meta["collapsed_block_count"].(int); got != 2 {
		t.Fatalf("collapsed_block_count = %v, want 2", meta["collapsed_block_count"])
	}
	// The input manifest still lists every event: coverage (memoire:D211) is
	// a property of the input set, not of the rendering.
	if got, _ := meta["input_event_count"].(int); got != 7 {
		t.Fatalf("input_event_count = %v, want 7", meta["input_event_count"])
	}
	if ids, _ := meta["input_event_ids"].([]int64); len(ids) != 7 {
		t.Fatalf("input_event_ids = %v, want 7 entries", meta["input_event_ids"])
	}
}

func TestPromptCollapsesRunOfOne(t *testing.T) {
	// The rule is uniform: a content-less event never gets a per-event block,
	// even alone.
	events := []db.EventRecord{
		userPromptEvent(1, 100, "only real content here"),
		contentLessToolEvent(2, 110, "Read"),
	}
	prompt, meta, err := BuildPrompt(baseRequest(events))
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if !strings.Contains(prompt, "### events 2..2 (1 event, no captured content)") {
		t.Fatalf("single content-less event must collapse; prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "### event 2\n") {
		t.Fatal("single content-less event still rendered per-event")
	}
	if got, _ := meta["collapsed_event_count"].(int); got != 1 {
		t.Fatalf("collapsed_event_count = %v, want 1", meta["collapsed_event_count"])
	}
	if got, _ := meta["collapsed_block_count"].(int); got != 1 {
		t.Fatalf("collapsed_block_count = %v, want 1", meta["collapsed_block_count"])
	}
}

func TestPromptCollapsedBlockDeterministicAndBounded(t *testing.T) {
	// 14 distinct tools: at most 12 named, the rest folded into one
	// `other xN` counting the folded EVENTS. Counts descend so the named
	// order is unambiguous, and the two smallest tie so the fold is exact.
	events := []db.EventRecord{userPromptEvent(1, 100, "content anchor for the window")}
	var id int64 = 2
	var epoch int64 = 110
	for i := 1; i <= 14; i++ {
		n := 15 - i // 14, 13, ... 2, 1
		if i == 14 {
			n = 2 // tie with tool 13 so the folded total is 2+2 = 4
		}
		for j := 0; j < n; j++ {
			events = append(events, contentLessToolEvent(id, epoch, fmt.Sprintf("Tool%02d", i)))
			id++
			epoch++
		}
	}

	first, meta, err := BuildPrompt(baseRequest(events))
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	second, _, err := BuildPrompt(baseRequest(events))
	if err != nil {
		t.Fatalf("BuildPrompt (second): %v", err)
	}
	if first != second {
		t.Fatal("collapsed rendering is not byte-deterministic across builds")
	}

	toolsLine := ""
	for _, line := range strings.Split(first, "\n") {
		if strings.HasPrefix(line, "- tools: ") {
			toolsLine = line
			break
		}
	}
	if toolsLine == "" {
		t.Fatalf("no tools line rendered; prompt:\n%s", first)
	}
	want := "- tools: Tool01 x14, Tool02 x13, Tool03 x12, Tool04 x11, Tool05 x10, " +
		"Tool06 x9, Tool07 x8, Tool08 x7, Tool09 x6, Tool10 x5, Tool11 x4, Tool12 x3, other x4"
	if toolsLine != want {
		t.Fatalf("tools line =\n  %q\nwant\n  %q", toolsLine, want)
	}
	if n := strings.Count(first, "\n### events "); n != 1 {
		t.Fatalf("one maximal run must render one block, got %d", n)
	}
	if got, _ := meta["collapsed_event_count"].(int); got != len(events)-1 {
		t.Fatalf("collapsed_event_count = %v, want %d", meta["collapsed_event_count"], len(events)-1)
	}
}

func TestPromptCollapsedToolCountsTieBreakByName(t *testing.T) {
	events := []db.EventRecord{
		userPromptEvent(1, 100, "content anchor for the window"),
		contentLessToolEvent(2, 110, "Zebra"),
		contentLessToolEvent(3, 120, "Alpha"),
		contentLessToolEvent(4, 130, "Middle"),
		contentLessToolEvent(5, 140, "Middle"),
	}
	prompt, _, err := BuildPrompt(baseRequest(events))
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if !strings.Contains(prompt, "- tools: Middle x2, Alpha x1, Zebra x1\n") {
		t.Fatalf("tool counts must order by count desc then name asc; prompt:\n%s", prompt)
	}
}

func TestPromptAllContentLessWindowRendersOneBlock(t *testing.T) {
	// The min-content guard skips such a window in production, but the
	// builder must stay total: one block, no panic.
	events := []db.EventRecord{
		contentLessToolEvent(10, 100, "Read"),
		contentLessToolEvent(11, 110, "Read"),
		contentLessToolEvent(12, 120, "Edit"),
	}
	prompt, meta, err := BuildPrompt(baseRequest(events))
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if n := strings.Count(prompt, "\n### events "); n != 1 {
		t.Fatalf("collapsed block count = %d, want 1; prompt:\n%s", n, prompt)
	}
	if strings.Contains(prompt, "### event 1") {
		t.Fatal("no per-event block may render for an all-content-less window")
	}
	if !strings.Contains(prompt, "### events 10..12 (3 events, no captured content)") {
		t.Fatalf("unexpected block header; prompt:\n%s", prompt)
	}
	if got, _ := meta["collapsed_event_count"].(int); got != 3 {
		t.Fatalf("collapsed_event_count = %v, want 3", meta["collapsed_event_count"])
	}
	if got, _ := meta["collapsed_block_count"].(int); got != 1 {
		t.Fatalf("collapsed_block_count = %v, want 1", meta["collapsed_block_count"])
	}
}

func TestPromptCollapseCountersZeroWithoutContentLessEvents(t *testing.T) {
	events := []db.EventRecord{
		userPromptEvent(1, 100, "a real question with some substance"),
		assistantTurnEvent(2, 110, "claude_assistant_turn", "a real answer with some substance"),
	}
	prompt, meta, err := BuildPrompt(baseRequest(events))
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if strings.Contains(prompt, "\n### events ") {
		t.Fatal("no collapsed block may render for a fully content-bearing window")
	}
	if got, _ := meta["collapsed_event_count"].(int); got != 0 {
		t.Fatalf("collapsed_event_count = %v, want 0", meta["collapsed_event_count"])
	}
	if got, _ := meta["collapsed_block_count"].(int); got != 0 {
		t.Fatalf("collapsed_block_count = %v, want 0", meta["collapsed_block_count"])
	}
}

// TestPromptCollapse365Shape reproduces the stalled segment that motivated
// #365: 339 content-less claude_PostToolUse events (a subagent Explore burst)
// around 4 content-bearing events, against the hourly lane's 43,929-byte
// ceiling. Before collapsing, the same fixture rendered far past the ceiling
// while the guard measured the median halves under the content floor, so the
// segment was re-cut and skipped on every tick and never covered.
func TestPromptCollapse365Shape(t *testing.T) {
	const (
		wantEvents    = 343
		wantCollapsed = 339
		wantBlocks    = 4
		ceiling       = 43929
	)
	tools := []string{"Read", "Grep", "Bash", "Edit", "Glob"}

	events := make([]db.EventRecord, 0, wantEvents)
	var id int64 = 1
	var epoch int64 = 1788295958
	runLengths := []int{100, 100, 100, 39}
	for i, n := range runLengths {
		events = append(events, userPromptEvent(id, epoch, strings.Repeat(fmt.Sprintf("real content block %d. ", i), 12)))
		id++
		epoch++
		for j := 0; j < n; j++ {
			events = append(events, contentLessToolEvent(id, epoch, tools[j%len(tools)]))
			id++
			epoch++
		}
	}
	if len(events) != wantEvents {
		t.Fatalf("fixture built %d events, want %d", len(events), wantEvents)
	}

	req := baseRequest(events)
	req.Limits.MaxPromptBytes = ceiling
	prompt, meta, err := BuildPrompt(req)
	if err != nil {
		t.Fatalf("the #365 window must build under the %d-byte ceiling: %v", ceiling, err)
	}
	if len(prompt) >= ceiling {
		t.Fatalf("prompt_bytes = %d, must sit under the %d-byte ceiling", len(prompt), ceiling)
	}
	if got, _ := meta["prompt_bytes"].(int); got != len(prompt) {
		t.Fatalf("meta prompt_bytes = %v, want %d", meta["prompt_bytes"], len(prompt))
	}
	if got, _ := meta["input_event_count"].(int); got != wantEvents {
		t.Fatalf("input_event_count = %v, want %d", meta["input_event_count"], wantEvents)
	}
	ids, _ := meta["input_event_ids"].([]int64)
	if len(ids) != wantEvents {
		t.Fatalf("len(input_event_ids) = %d, want %d", len(ids), wantEvents)
	}
	if got, _ := meta["collapsed_event_count"].(int); got != wantCollapsed {
		t.Fatalf("collapsed_event_count = %v, want %d", meta["collapsed_event_count"], wantCollapsed)
	}
	if got, _ := meta["collapsed_block_count"].(int); got != wantBlocks {
		t.Fatalf("collapsed_block_count = %v, want %d", meta["collapsed_block_count"], wantBlocks)
	}
	// The real content must survive verbatim: collapsing drops envelope, not
	// evidence.
	for i := range runLengths {
		if !strings.Contains(prompt, fmt.Sprintf("real content block %d.", i)) {
			t.Fatalf("content-bearing event %d lost its text", i)
		}
	}
	t.Logf("#365 fixture: %d events, prompt_bytes=%d (ceiling %d), collapsed=%d in %d blocks",
		wantEvents, len(prompt), ceiling, wantCollapsed, wantBlocks)
}

func TestPromptTrimsEnvelopeAgainstWindow(t *testing.T) {
	// Session-scoped window: project and session_id repeat the
	// `## Source Window` header, so the per-event lines go. source_event_id
	// must ALWAYS stay — the parser maps string citations through it.
	inWindow := userPromptEvent(1, 100, "a question with enough substance to count")
	foreign := userPromptEvent(2, 110, "a foreign event with enough substance to count")
	foreign.Project = "other-project"
	foreign.SessionID = "other-session"

	req := baseRequest([]db.EventRecord{inWindow, foreign})
	prompt, _, err := BuildPrompt(req)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if strings.Contains(prompt, "- project: memoire\n") {
		t.Fatalf("project line must be omitted when it repeats the window; prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "- session_id: session-1\n") {
		t.Fatalf("session_id line must be omitted when it repeats the window; prompt:\n%s", prompt)
	}
	// An event that does NOT match the window still declares both.
	if !strings.Contains(prompt, "- project: other-project\n") {
		t.Fatal("a foreign project must still render its project line")
	}
	if !strings.Contains(prompt, "- session_id: other-session\n") {
		t.Fatal("a foreign session must still render its session_id line")
	}
	if n := strings.Count(prompt, "- source_event_id: "); n != 2 {
		t.Fatalf("source_event_id lines = %d, want 2 (always rendered)", n)
	}

	// Time-window mode (no session pinned): the header cannot state the
	// session, so every event carries it, as before v2.5.
	req.Window.SessionID = ""
	prompt, _, err = BuildPrompt(req)
	if err != nil {
		t.Fatalf("BuildPrompt (time-window mode): %v", err)
	}
	if !strings.Contains(prompt, "- session_id: session-1\n") {
		t.Fatalf("time-window mode must keep the session_id line; prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "- session_id: other-session\n") {
		t.Fatal("time-window mode must keep every session_id line")
	}
	if strings.Contains(prompt, "- project: memoire\n") {
		t.Fatal("the project trim is independent of the session mode")
	}
}
