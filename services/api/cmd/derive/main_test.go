// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
)

// setupCLIEnv points config.Load at an isolated MEMOIRE_HOME under the test temp
// dir and the repo-relative schema directory. It never touches the live store.
func setupCLIEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MEMOIRE_HOME", home)
	// Pin MEMOIRE_DB explicitly: config.Load gives a non-empty ambient
	// MEMOIRE_DB precedence over MEMOIRE_HOME, so a post-cutover shell with
	// the production postgres:// DSN exported would otherwise have these
	// tests open — and seed fixtures into — the live store (derive-seam-port
	// round-2 HIGH-1).
	t.Setenv("MEMOIRE_DB", filepath.Join(home, "memoire.db"))
	t.Setenv("MEMOIRE_SCHEMA_DIR", "../../../../schema")
	return home
}

// seedSession ingests claude_UserPromptSubmit events and returns their ids.
func seedSession(t *testing.T, project, sessionID string, epochs []int64, prompts []string) []int64 {
	t.Helper()
	if len(epochs) != len(prompts) {
		t.Fatalf("epochs/prompts length mismatch")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	store, err := db.Open(cfg.DBPath, cfg.SchemaDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer store.Close()

	ids := make([]int64, 0, len(prompts))
	for i, prompt := range prompts {
		payload, _ := json.Marshal(map[string]any{"prompt": prompt})
		res, err := store.Ingest(context.Background(), db.EventInput{
			Project:         project,
			SessionID:       sessionID,
			SourceAdapter:   "claude-code",
			SourceEventID:   "src-" + prompt,
			EventType:       "claude_UserPromptSubmit",
			OccurredAtEpoch: epochs[i],
			Payload:         payload,
		})
		if err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		ids = append(ids, res.ID)
	}
	return ids
}

// seedToolEvents ingests content-less claude_PostToolUse events — a tool name
// and tool input, no result summary, the shape every adapter in this repo
// actually produces (Fibery #365). They carry zero content for the prompt
// renderer and for the min-content guard alike.
func seedToolEvents(t *testing.T, project, sessionID string, startEpoch int64, n int) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	store, err := db.Open(cfg.DBPath, cfg.SchemaDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer store.Close()

	tools := []string{"Read", "Grep", "Bash"}
	for i := 0; i < n; i++ {
		payload, _ := json.Marshal(map[string]any{
			"tool_name":  tools[i%len(tools)],
			"tool_input": map[string]any{"file_path": "/some/path"},
		})
		if _, err := store.Ingest(context.Background(), db.EventInput{
			Project:         project,
			SessionID:       sessionID,
			SourceAdapter:   "claude-code",
			EventType:       "claude_PostToolUse",
			SourceEventID:   fmt.Sprintf("claude-posttooluse-%s-%d", sessionID, i),
			OccurredAtEpoch: startEpoch + int64(i),
			Payload:         payload,
		}); err != nil {
			t.Fatalf("Ingest tool event %d: %v", i, err)
		}
	}
}

func countDerivationRuns(t *testing.T, project string) int {
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
	runs, err := store.ListDerivationRuns(context.Background(), db.DerivationRunFilter{Project: project})
	if err != nil {
		t.Fatalf("ListDerivationRuns: %v", err)
	}
	return len(runs)
}

func TestCLIModelRequiresDryRun(t *testing.T) {
	setupCLIEnv(t)
	err := run([]string{"-strategy=model", "-provider=local-qwen", "-model=qwen-7b", "-project=p", "-session-id=s"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatalf("expected error when -strategy=model without -dry-run")
	}
	if !strings.Contains(err.Error(), "dry-run") {
		t.Fatalf("error does not mention dry-run: %v", err)
	}
}

func TestCLIModelRequiresProvider(t *testing.T) {
	setupCLIEnv(t)
	err := run([]string{"-strategy=model", "-dry-run", "-model=qwen-7b", "-project=p", "-session-id=s"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatalf("expected error when -strategy=model without -provider")
	}
	if !strings.Contains(err.Error(), "-provider") {
		t.Fatalf("error does not mention -provider: %v", err)
	}
}

func TestCLIModelRequiresModel(t *testing.T) {
	setupCLIEnv(t)
	err := run([]string{"-strategy=model", "-dry-run", "-provider=local-qwen", "-project=p", "-session-id=s"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatalf("expected error when -strategy=model without -model")
	}
	if !strings.Contains(err.Error(), "-model") {
		t.Fatalf("error does not mention -model: %v", err)
	}
}

func TestCLIDryRunEmitsPromptMetadata(t *testing.T) {
	setupCLIEnv(t)
	seedSession(t, "memoire-test", "sess-meta", []int64{1000, 1100, 1200}, []string{"alpha", "beta", "gamma"})

	var stdout bytes.Buffer
	err := run([]string{
		"-strategy=model", "-dry-run", "-json",
		"-provider=local-qwen", "-model=qwen-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-meta",
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, stdout.String())
	}
	if out["strategy"] != "model" {
		t.Errorf("strategy = %v", out["strategy"])
	}
	if out["dry_run"] != true {
		t.Errorf("dry_run = %v", out["dry_run"])
	}
	if out["source_event_count"].(float64) != 3 {
		t.Errorf("source_event_count = %v, want 3", out["source_event_count"])
	}
	hash, _ := out["input_hash"].(string)
	if !strings.HasPrefix(hash, "sha256:") {
		t.Errorf("input_hash = %q, want sha256: prefix", hash)
	}
	meta, ok := out["prompt_meta"].(map[string]any)
	if !ok {
		t.Fatalf("prompt_meta missing or wrong type: %v", out["prompt_meta"])
	}
	if meta["provider"] != "local-qwen" {
		t.Errorf("prompt_meta.provider = %v", meta["provider"])
	}
	if meta["model"] != "qwen-7b" {
		t.Errorf("prompt_meta.model = %v", meta["model"])
	}
	if _, ok := out["prompt_preview"].(string); !ok {
		t.Errorf("prompt_preview missing")
	}
}

// TestCLIDryRunReportsCollapsedEvents pins the v2.5 surface at the CLI seam
// (Fibery #365): the dry-run's prompt_meta carries the collapse counters, so
// the ceiling calibration can read the saving per run, and prompt_version
// names the bumped prompt. The window holds two substantive prompts around a
// burst of 40 content-less tool events — one maximal run.
func TestCLIDryRunReportsCollapsedEvents(t *testing.T) {
	setupCLIEnv(t)
	seedSession(t, "memoire-test", "sess-collapse", []int64{3000, 3400}, []string{
		strings.Repeat("a substantive opening prompt. ", 12),
		strings.Repeat("a substantive closing prompt. ", 12),
	})
	seedToolEvents(t, "memoire-test", "sess-collapse", 3100, 40)

	var stdout bytes.Buffer
	err := run([]string{
		"-strategy=model", "-dry-run", "-json",
		"-provider=local-qwen", "-model=qwen-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-collapse",
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, stdout.String())
	}
	if got := out["source_event_count"].(float64); got != 42 {
		t.Fatalf("source_event_count = %v, want 42", got)
	}
	meta, ok := out["prompt_meta"].(map[string]any)
	if !ok {
		t.Fatalf("prompt_meta missing or wrong type: %v", out["prompt_meta"])
	}
	if got := meta["prompt_version"]; got != "derive-observations-v2.5" {
		t.Fatalf("prompt_meta.prompt_version = %v, want derive-observations-v2.5", got)
	}
	if got, _ := meta["collapsed_event_count"].(float64); got != 40 {
		t.Fatalf("prompt_meta.collapsed_event_count = %v, want 40", meta["collapsed_event_count"])
	}
	if got, _ := meta["collapsed_block_count"].(float64); got != 1 {
		t.Fatalf("prompt_meta.collapsed_block_count = %v, want 1", meta["collapsed_block_count"])
	}
	// The input manifest still covers the collapsed events (memoire:D211).
	if got, _ := meta["input_event_count"].(float64); got != 42 {
		t.Fatalf("prompt_meta.input_event_count = %v, want 42", meta["input_event_count"])
	}
	ids, _ := meta["input_event_ids"].([]any)
	if len(ids) != 42 {
		t.Fatalf("len(prompt_meta.input_event_ids) = %d, want 42", len(ids))
	}
}

func TestCLIDryRunJSONOutput(t *testing.T) {
	setupCLIEnv(t)
	// A long prompt body guarantees the full prompt exceeds the 800-byte preview
	// cap, so the truncation assertions below are meaningful (D35 / Finding 7).
	longPrompt := strings.Repeat("design rationale paragraph that pushes the rendered prompt well past the preview cap. ", 20)
	seedSession(t, "memoire-test", "sess-json", []int64{2000, 2100}, []string{longPrompt, "two"})

	var stdout bytes.Buffer
	err := run([]string{
		"-strategy=model", "-dry-run", "-json",
		"-provider=local-qwen", "-model=qwen-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-json",
		"-max-input-events=10", "-max-prompt-bytes=100000", "-max-output-observations=5",
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	meta := out["prompt_meta"].(map[string]any)
	limits := meta["limits"].(map[string]any)
	if limits["max_input_events"].(float64) != 10 {
		t.Errorf("max_input_events = %v, want 10", limits["max_input_events"])
	}
	if limits["max_prompt_bytes"].(float64) != 100000 {
		t.Errorf("max_prompt_bytes = %v, want 100000", limits["max_prompt_bytes"])
	}
	if limits["max_output_observations"].(float64) != 5 {
		t.Errorf("max_output_observations = %v, want 5", limits["max_output_observations"])
	}
	// Preview must be a real truncation: full prompt is only via -prompt-out.
	// Prove it concretely (Finding 7): the full prompt exceeds the cap, so the
	// preview is exactly promptPreviewBytes AND strictly shorter than the prompt.
	promptBytes := out["prompt_bytes"].(float64)
	if promptBytes <= float64(promptPreviewBytes) {
		t.Fatalf("fixture invalid: prompt_bytes=%v must exceed preview cap %d to test truncation", promptBytes, promptPreviewBytes)
	}
	preview := out["prompt_preview"].(string)
	if len(preview) != promptPreviewBytes {
		t.Errorf("prompt_preview length = %d, want exactly %d (truncated)", len(preview), promptPreviewBytes)
	}
	if !(float64(len(preview)) < promptBytes) {
		t.Errorf("prompt_preview (%d) must be strictly shorter than prompt_bytes (%v)", len(preview), promptBytes)
	}
}

func TestCLIPromptOut(t *testing.T) {
	setupCLIEnv(t)
	seedSession(t, "memoire-test", "sess-po", []int64{3000}, []string{"hello"})

	promptFile := filepath.Join(t.TempDir(), "prompt.txt")
	var stdout bytes.Buffer
	err := run([]string{
		"-strategy=model", "-dry-run",
		"-provider=local-qwen", "-model=qwen-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-po",
		"-prompt-out=" + promptFile,
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	data, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatalf("read prompt file: %v", err)
	}
	full := string(data)
	if !strings.Contains(full, "Memoire Observation Derivation") {
		t.Errorf("prompt file missing prompt header")
	}
	if !strings.Contains(full, "decision") {
		t.Errorf("prompt file missing sealed kinds")
	}
	if !strings.Contains(full, "hello") {
		t.Errorf("prompt file missing source event prompt")
	}
}

// The turn-ingest campaign's acceptance shape (Phase 1, 2026-07-11): a
// backfilled assistant turn in the window must land in the built prompt, and
// the model path must record derivation version v2 (the input re-keying).
func TestCLIPromptOutIncludesBackfilledAssistantTurn(t *testing.T) {
	setupCLIEnv(t)
	seedSession(t, "memoire-test", "sess-turn", []int64{3000}, []string{"please fix the bug"})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	store, err := db.Open(cfg.DBPath, cfg.SchemaDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if _, err := store.Ingest(context.Background(), db.EventInput{
		Project:         "memoire-test",
		SessionID:       "sess-turn",
		SourceAdapter:   "claude_code_transcript_backfill",
		SourceEventID:   "cc-turn:sess-turn:u1",
		EventType:       "claude_assistant_turn",
		OccurredAtEpoch: 3100,
		Payload:         json.RawMessage(`{"backfill_generation":"test"}`),
		Text:            "BACKFILLED-TURN: fixed in commit abc123, tests green",
	}); err != nil {
		t.Fatalf("Ingest assistant turn: %v", err)
	}
	store.Close()

	promptFile := filepath.Join(t.TempDir(), "prompt.txt")
	var stdout bytes.Buffer
	err = run([]string{
		"-strategy=model", "-dry-run", "-json",
		"-provider=local-qwen", "-model=qwen-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-turn",
		"-prompt-out=" + promptFile,
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	data, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatalf("read prompt file: %v", err)
	}
	full := string(data)
	if !strings.Contains(full, "BACKFILLED-TURN: fixed in commit abc123, tests green") {
		t.Errorf("prompt file missing backfilled assistant-turn text")
	}
	if !strings.Contains(full, "claude_assistant_turn") {
		t.Errorf("prompt file missing the assistant-turn event envelope")
	}
	if !strings.Contains(stdout.String(), `"observation-derive-v2"`) {
		t.Errorf("dry-run metadata must carry derivation version v2, got: %s", stdout.String())
	}
	if got := strings.Count(stdout.String(), `"source_event_count": 2`); got != 1 {
		t.Errorf("expected source_event_count 2 (prompt + turn), output: %s", stdout.String())
	}
}

func TestCLIForceRerunRequiresExecute(t *testing.T) {
	setupCLIEnv(t)
	err := run([]string{
		"-strategy=model", "-dry-run", "-force-rerun",
		"-provider=local-qwen", "-model=qwen-7b", "-endpoint-id=hl3",
		"-project=p", "-session-id=s",
	}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "-force-rerun requires -execute") {
		t.Fatalf("expected force-rerun/execute coupling error, got: %v", err)
	}
}

// -list-outstanding is the HL5 trigger's store-confirmation surface
// (memoire:D165 R-10.3): uncovered sessions print as project\x1fsession,
// covered ones vanish. Coverage-transition semantics are proven in the
// engine-pair battery (OutstandingDeriveWork); this test pins the CLI
// contract — separator, recency filter, and the [1, 720] bounds shared
// with GET /v1/derive/outstanding.
func TestCLIListOutstanding(t *testing.T) {
	setupCLIEnv(t)
	now := time.Now().Unix()
	seedSession(t, "memoire-test", "sess-outstanding", []int64{now - 300, now - 200}, []string{"alpha", "beta"})
	// A second session outside the since-hours window must not print.
	seedSession(t, "memoire-test", "sess-ancient", []int64{now - 48*3600}, []string{"old"})

	var stdout bytes.Buffer
	if err := run([]string{"-list-outstanding"}, &stdout, io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := stdout.String(), "memoire-test\x1fsess-outstanding\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}

	for _, bad := range []string{"0", "721"} {
		err := run([]string{"-list-outstanding", "-since-hours=" + bad}, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "-since-hours must be in [1, 720]") {
			t.Errorf("since-hours=%s: expected bounds error, got: %v", bad, err)
		}
	}

	err := run([]string{"-list-outstanding", "-list-open-intents"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutual-exclusion error, got: %v", err)
	}
}

func TestCLIParseResponse(t *testing.T) {
	setupCLIEnv(t)
	ids := seedSession(t, "memoire-test", "sess-parse", []int64{4000, 4100}, []string{"design choice", "follow up"})

	resp := map[string]any{
		"schema_version": "1",
		"observations": []map[string]any{
			{
				"kind":             "decision",
				"text":             "Adopt the dry-run CLI before any provider call.",
				"confidence":       0.82,
				"source_event_ids": []int64{ids[0]},
				"rationale":        "operator inspection precedes provider wiring",
			},
			{
				"kind":             "task",
				"text":             "Wire the provider registry in a later slice.",
				"confidence":       0.2,
				"source_event_ids": []int64{ids[1]},
			},
			{
				"kind":             "unknown",
				"text":             "a genuine ontology gap surfaced in this window",
				"source_event_ids": []int64{ids[0]},
				"vendor_hint":      "captured extra",
				"unknown_analysis": map[string]any{
					"tried_kinds": []map[string]any{
						{"kind": "decision", "why_not": "no decision settled"},
						{"kind": "fact", "why_not": "not a stable fact"},
						{"kind": "open_question", "why_not": "not phrased as a question"},
					},
					"proposed_kind_hypothesis": "timeline - a chronological narrative that none of the sealed kinds capture",
				},
			},
		},
	}
	respBytes, _ := json.MarshalIndent(resp, "", "  ")
	respFile := filepath.Join(t.TempDir(), "response.json")
	if err := os.WriteFile(respFile, respBytes, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var stdout bytes.Buffer
	err := run([]string{
		"-strategy=model", "-dry-run", "-json",
		"-provider=local-qwen", "-model=qwen-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-parse",
		"-parse-response=" + respFile,
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	parsed, ok := out["parsed"].(map[string]any)
	if !ok {
		t.Fatalf("parsed missing: %v", out)
	}
	// One valid/proposed observation (the 0.82 decision); the 0.2 task is dropped
	// below MinConfidence and reported as rejected; one valid unknown candidate.
	if parsed["proposed_observation_count"].(float64) != 1 {
		t.Errorf("proposed_observation_count = %v, want 1", parsed["proposed_observation_count"])
	}
	if parsed["rejected_candidate_count"].(float64) != 1 {
		t.Errorf("rejected_candidate_count = %v, want 1", parsed["rejected_candidate_count"])
	}
	if parsed["unknown_candidate_count"].(float64) != 1 {
		t.Errorf("unknown_candidate_count = %v, want 1", parsed["unknown_candidate_count"])
	}
	// Finding 9: parse-response output must expose unknown_candidates with their
	// captured extras for offline audit, not just the count.
	unknowns, ok := parsed["unknown_candidates"].([]any)
	if !ok || len(unknowns) != 1 {
		t.Fatalf("expected unknown_candidates emitted, got %v", parsed["unknown_candidates"])
	}
	uc := unknowns[0].(map[string]any)
	extra, ok := uc["extra"].(map[string]any)
	if !ok {
		t.Fatalf("expected captured extra on unknown candidate, got %v", uc["extra"])
	}
	if _, ok := extra["vendor_hint"]; !ok {
		t.Fatalf("expected vendor_hint captured in unknown candidate extra, got %v", extra)
	}
}

func TestCLIModelRejectsNonModelProvider(t *testing.T) {
	setupCLIEnv(t)
	// Finding 4: the model path accepts only contracted model providers. "manual"
	// is a derivation source but not a model provider; a bogus provider is also
	// rejected. Rejection happens before any store access, dry-run included.
	for _, provider := range []string{"manual", "rules", "anthropic"} {
		err := run([]string{
			"-strategy=model", "-dry-run",
			"-provider=" + provider, "-model=x",
			"-project=p", "-session-id=s",
		}, io.Discard, io.Discard)
		if err == nil {
			t.Fatalf("provider %q must be rejected on the model path", provider)
		}
		if !strings.Contains(err.Error(), "not a model provider") {
			t.Fatalf("provider %q: unexpected error: %v", provider, err)
		}
	}
}

func TestCLIDryRunNoPersistence(t *testing.T) {
	setupCLIEnv(t)
	seedSession(t, "memoire-test", "sess-nopersist", []int64{5000, 5100}, []string{"alpha", "beta"})

	var stdout bytes.Buffer
	err := run([]string{
		"-strategy=model", "-dry-run", "-json",
		"-provider=local-qwen", "-model=qwen-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-nopersist",
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := countDerivationRuns(t, "memoire-test"); n != 0 {
		t.Fatalf("dry-run persisted %d derivation runs, want 0", n)
	}
}

func TestCLIRulesStrategyStillWorks(t *testing.T) {
	setupCLIEnv(t)
	seedSession(t, "memoire-test", "sess-rules", []int64{6000}, []string{"Memoire decision: ship the dry-run CLI"})

	var stdout bytes.Buffer
	err := run([]string{
		"-strategy=rules", "-json",
		"-project=memoire-test", "-session-id=sess-rules",
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["strategy"] != "rules" {
		t.Errorf("strategy = %v", out["strategy"])
	}
	// Rules path persists a run; confirms dry-run non-persistence is model-only.
	if n := countDerivationRuns(t, "memoire-test"); n != 1 {
		t.Errorf("rules run persisted %d runs, want 1", n)
	}
}

func TestCLIUnknownStrategy(t *testing.T) {
	setupCLIEnv(t)
	err := run([]string{"-strategy=bogus", "-project=p"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unknown -strategy") {
		t.Fatalf("expected unknown strategy error, got %v", err)
	}
}

// TestCLIModelLocalRequiresEndpointID asserts the Slice 5 tightening: a local
// provider dry-run requires an explicit -endpoint-id (docs/MODEL-PROVIDER.md
// "Provider Naming" — host identity is never defaulted).
func TestCLIModelLocalRequiresEndpointID(t *testing.T) {
	setupCLIEnv(t)
	err := run([]string{
		"-strategy=model", "-dry-run",
		"-provider=local-qwen", "-model=qwen-7b",
		"-project=p", "-session-id=s",
	}, io.Discard, io.Discard)
	if err == nil {
		t.Fatalf("expected error when local provider lacks -endpoint-id")
	}
	if !strings.Contains(err.Error(), "endpoint-id") {
		t.Fatalf("error does not mention endpoint-id: %v", err)
	}
}

// fakeQwenServer returns an httptest server answering /v1/models (availability
// probe) and /v1/chat/completions with the supplied model content. No real
// network: tests drive the entire -execute path against this fake (LIVE-DATA
// FENCE).
func fakeQwenServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen-test"},{"id":"qwen2.5-7b"}]}`))
		case "/v1/chat/completions":
			resp := map[string]any{
				"choices": []map[string]any{
					{"message": map[string]any{"content": content}, "finish_reason": "stop"},
				},
				"usage": map[string]any{"prompt_tokens": 120, "completion_tokens": 30, "total_tokens": 150},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
}

// writeEndpointsConfig writes a model endpoint registry to the default path
// under MEMOIRE_HOME so config.LoadModelEndpoints finds it. apiKeyEnv is a NAME,
// never a value.
func writeEndpointsConfig(t *testing.T, home, baseURL, apiKeyEnv, model string) {
	t.Helper()
	cfgFile := map[string]any{
		"endpoints": []map[string]any{
			{
				"endpoint_id": "hl3",
				"base_url":    baseURL,
				"api_key_env": apiKeyEnv,
				"models":      []string{model},
			},
		},
	}
	b, _ := json.MarshalIndent(cfgFile, "", "  ")
	if err := os.WriteFile(filepath.Join(home, "model-endpoints.json"), b, 0o600); err != nil {
		t.Fatalf("write endpoints config: %v", err)
	}
}

// listProposedObservations returns model-derived proposed observations for a
// project. Ingestion writes "raw"/"accepted" observations of its own, so the
// -execute assertion targets the proposed review state specifically.
func listProposedObservations(t *testing.T, project string) []db.ObservationRecord {
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
	obs, _, err := store.ListObservations(context.Background(), db.ObservationFilter{Project: project, ReviewState: "proposed"})
	if err != nil {
		t.Fatalf("ListObservations: %v", err)
	}
	return obs
}

// seedSubstantiveSession seeds a session that clears the min-content guard
// (>= 3 non-trivial events, >= 500 chars of prompt text) so execute-path tests
// reach the provider call.
func seedSubstantiveSession(t *testing.T, project, sessionID string, base int64) []int64 {
	t.Helper()
	long := strings.Repeat("the operator ruled the api service ships in Go with sqlite fts5. ", 3)
	return seedSession(t, project, sessionID,
		[]int64{base, base + 100, base + 200},
		[]string{long + "one", long + "two", long + "three"})
}

// TestCLIExecuteEndToEnd drives -execute through a fake-HTTP provider end to end:
// the model returns a valid response, the orchestrator writes a proposed
// observation to an isolated store, and the run completes. No real network.
func TestCLIExecuteEndToEnd(t *testing.T) {
	home := setupCLIEnv(t)
	ids := seedSubstantiveSession(t, "memoire-test", "sess-exec", 7000)

	modelJSON := fmt.Sprintf(`{"schema_version":"1","observations":[{"kind":"decision","text":"use Go for the API service","confidence":0.86,"source_event_ids":[%d],"rationale":"explicit"}]}`, ids[0])
	srv := fakeQwenServer(t, modelJSON)
	defer srv.Close()

	writeEndpointsConfig(t, home, srv.URL, "MEMOIRE_TEST_QWEN_KEY", "qwen2.5-7b")
	t.Setenv("MEMOIRE_TEST_QWEN_KEY", "placeholder-not-a-real-token")

	var stdout bytes.Buffer
	err := run([]string{
		"-strategy=model", "-execute", "-json",
		"-provider=local-qwen", "-model=qwen2.5-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-exec",
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if out["status"] != "completed" {
		t.Fatalf("status = %v, want completed (out=%v)", out["status"], out)
	}
	if out["written_count"].(float64) != 1 {
		t.Fatalf("written_count = %v, want 1", out["written_count"])
	}
	if runID, _ := out["run_id"].(string); runID == "" {
		t.Fatalf("expected non-empty run_id, got %v", out["run_id"])
	}
	// The run and the proposed observation must be persisted in the isolated store.
	if n := countDerivationRuns(t, "memoire-test"); n != 1 {
		t.Fatalf("expected 1 derivation run persisted, got %d", n)
	}
	obs := listProposedObservations(t, "memoire-test")
	if len(obs) != 1 {
		for _, o := range obs {
			t.Logf("obs id=%d kind=%q text=%q review=%q run=%q", o.ID, o.Kind, o.Text, o.ReviewState, o.DerivationRunID)
		}
		t.Fatalf("expected 1 persisted proposed observation, got %d", len(obs))
	}
	if obs[0].ReviewState != "proposed" {
		t.Fatalf("observation review_state = %q, want proposed", obs[0].ReviewState)
	}
	if obs[0].Kind != "decision" {
		t.Fatalf("observation kind = %q, want decision", obs[0].Kind)
	}
}

// TestCLIExecuteFailedRunIsNotError asserts that a recorded provider failure is
// surfaced as a failed run (status + run id, exit 0), NOT as a Go/operator
// error. /v1/models passes; /v1/chat/completions returns 503, so the orchestrator
// records status=failed and writes no proposed observation.
func TestCLIExecuteFailedRunIsNotError(t *testing.T) {
	home := setupCLIEnv(t)
	seedSubstantiveSession(t, "memoire-test", "sess-fail", 9000)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen-test"},{"id":"qwen2.5-7b"}]}`))
		case "/v1/chat/completions":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	writeEndpointsConfig(t, home, srv.URL, "MEMOIRE_TEST_QWEN_KEY", "qwen2.5-7b")
	t.Setenv("MEMOIRE_TEST_QWEN_KEY", "placeholder-not-a-real-token")

	var stdout bytes.Buffer
	err := run([]string{
		"-strategy=model", "-execute", "-json",
		"-provider=local-qwen", "-model=qwen2.5-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-fail",
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("a recorded failed run must NOT be a Go error, got: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if out["status"] != "failed" {
		t.Fatalf("status = %v, want failed", out["status"])
	}
	if runID, _ := out["run_id"].(string); runID == "" {
		t.Fatalf("failed run must still carry a run_id, got %v", out["run_id"])
	}
	if out["written_count"].(float64) != 0 {
		t.Fatalf("failed run wrote %v observations, want 0", out["written_count"])
	}
	// A failed run is still persisted (for audit/retention), but writes no
	// proposed observation.
	if n := countDerivationRuns(t, "memoire-test"); n != 1 {
		t.Fatalf("expected 1 persisted (failed) derivation run, got %d", n)
	}
	if obs := listProposedObservations(t, "memoire-test"); len(obs) != 0 {
		t.Fatalf("failed run must write no proposed observations, got %d", len(obs))
	}
}

// TestCLIExecuteMissingEnvFailsLoud asserts a configured-but-unset secret env
// var fails loud as an operator error naming ONLY the variable, before any
// run is persisted, and never printing a value (there is none).
func TestCLIExecuteMissingEnvFailsLoud(t *testing.T) {
	home := setupCLIEnv(t)
	seedSession(t, "memoire-test", "sess-noenv", []int64{8000}, []string{"alpha"})

	srv := fakeQwenServer(t, `{"schema_version":"1","observations":[]}`)
	defer srv.Close()

	// Reference an env var that is intentionally never set in the process env.
	writeEndpointsConfig(t, home, srv.URL, "MEMOIRE_TEST_UNSET_KEY_8001", "qwen2.5-7b")

	err := run([]string{
		"-strategy=model", "-execute",
		"-provider=local-qwen", "-model=qwen2.5-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-noenv",
	}, io.Discard, io.Discard)
	if err == nil {
		t.Fatalf("expected error when required secret env var is unset")
	}
	if !strings.Contains(err.Error(), "MEMOIRE_TEST_UNSET_KEY_8001") {
		t.Fatalf("error must name the missing env var, got: %v", err)
	}
	// Pre-flight happens before store access: no run persisted.
	if n := countDerivationRuns(t, "memoire-test"); n != 0 {
		t.Fatalf("missing-secret failure must not persist a run, got %d", n)
	}
}

// TestCLIExecuteLocalRequiresEndpointID asserts -execute with a local provider
// and no -endpoint-id is an operator error (reuses the shared local-provider
// gate; rejected before config/store access).
func TestCLIExecuteLocalRequiresEndpointID(t *testing.T) {
	setupCLIEnv(t)
	err := run([]string{
		"-strategy=model", "-execute",
		"-provider=local-qwen", "-model=qwen2.5-7b",
		"-project=p", "-session-id=s",
	}, io.Discard, io.Discard)
	if err == nil {
		t.Fatalf("expected error when -execute local provider lacks -endpoint-id")
	}
	if !strings.Contains(err.Error(), "endpoint-id") {
		t.Fatalf("error does not mention endpoint-id: %v", err)
	}
}

// TestCLIExecuteAndDryRunMutuallyExclusive guards the two-mode contract.
func TestCLIExecuteAndDryRunMutuallyExclusive(t *testing.T) {
	setupCLIEnv(t)
	err := run([]string{
		"-strategy=model", "-execute", "-dry-run",
		"-provider=local-qwen", "-model=qwen2.5-7b", "-endpoint-id=hl3",
		"-project=p", "-session-id=s",
	}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("expected mutually-exclusive error, got %v", err)
	}
}

// TestCLINoExternalProviderFlag confirms the OpenAI external opt-in flag is NOT
// present in Slice 5 (OpenAI adapter deferred). Parsing it must fail.
func TestCLINoExternalProviderFlag(t *testing.T) {
	setupCLIEnv(t)
	err := run([]string{"-strategy=model", "-allow-external-provider"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatalf("expected error for unknown -allow-external-provider flag")
	}
}

func TestExecuteTimeoutFlagValidation(t *testing.T) {
	// D78: -execute-timeout must be a positive duration.
	var out, errOut bytes.Buffer
	err := run([]string{"-strategy", "model", "-execute-timeout", "-5s"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "execute-timeout") {
		t.Fatalf("expected execute-timeout validation error, got %v", err)
	}
}

// TestReasoningEffortFlagValidation pins the preflight value domain of the
// chat-template effort hint (operator ruling 2026-08-26): a level the template
// does not know is a silent no-op at the server, so a typo must fail before the
// run rather than sweep at the wrong effort. Empty stays valid — no kwarg sent.
func TestReasoningEffortFlagValidation(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"-strategy", "model", "-reasoning-effort", "lo"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "reasoning-effort") {
		t.Fatalf("expected reasoning-effort validation error, got %v", err)
	}

	// Explicitly empty is part of the flag's contract (= send no kwarg), not
	// just the unset default: it must pass the value gate like the levels do.
	for _, effort := range []string{"", "none", "low", "medium", "high", "xhigh"} {
		// Accepted levels pass the value gate and fail later, on the model
		// strategy's own requirements — never on the effort value itself.
		err := run([]string{"-strategy", "model", "-reasoning-effort", effort}, &out, &errOut)
		if err == nil || strings.Contains(err.Error(), "reasoning-effort") {
			t.Fatalf("-reasoning-effort %q must be accepted at preflight, got %v", effort, err)
		}
	}
}

// TestCLIExecuteDeniedProjectSkips pins the sweep targeting filter: a project
// matching the denylist is skipped with a marker before any endpoint or store
// access (no endpoints registry exists in this home, which would otherwise be
// a loud operator error).
func TestCLIExecuteDeniedProjectSkips(t *testing.T) {
	home := setupCLIEnv(t)
	deny := `{"deny_projects":["tmp",".agents","agent-*"]}`
	if err := os.WriteFile(filepath.Join(home, "derive-denylist.json"), []byte(deny), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	err := run([]string{
		"-strategy=model", "-execute", "-json",
		"-provider=local-qwen", "-model=qwen2.5-7b", "-endpoint-id=hl3",
		"-project=agent-a3ed1dd23680b2e9b", "-session-id=s1",
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("denied project must skip cleanly, got error: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if out["skipped"] != "project_denied" {
		t.Fatalf("skipped = %v, want project_denied (out=%v)", out["skipped"], out)
	}
	if out["persisted"] != false {
		t.Fatalf("persisted = %v, want false", out["persisted"])
	}
	// No intent existed, so none was served.
	if out["intent_served"] != false {
		t.Fatalf("intent_served = %v, want false with no open intent", out["intent_served"])
	}

	// A denial is a TERMINAL verdict for an open intent (round-2 MED-4 /
	// round-3 falsifier): notify first, deny again, and the intent must
	// close as skipped:project_denied.
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	store, err := db.Open(cfg.DBPath, cfg.SchemaDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer store.Close()
	if _, err := store.NotifyDeriveIntent(context.Background(), "agent-a3ed1dd23680b2e9b", "s1", time.Now().Unix()-30); err != nil {
		t.Fatalf("notify: %v", err)
	}
	stdout.Reset()
	err = run([]string{
		"-strategy=model", "-execute", "-json",
		"-provider=local-qwen", "-model=qwen2.5-7b", "-endpoint-id=hl3",
		"-project=agent-a3ed1dd23680b2e9b", "-session-id=s1",
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("denied project with intent must skip cleanly, got: %v", err)
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if out["intent_served"] != true {
		t.Fatalf("intent_served = %v, want true (out=%v)", out["intent_served"], out)
	}
	open, err := store.OpenDeriveIntents(context.Background())
	if err != nil || len(open) != 0 {
		t.Fatalf("denied intent must be terminally settled, open=%d err=%v", len(open), err)
	}

	// Round-4 LOW-NEW-2 falsifier: a BLOCKED store open must return within
	// the bound with the denial marker intact, not hang the sweep.
	oldOpen, oldTimeout := settleDeniedOpenStore, settleDeniedTimeout
	settleDeniedOpenStore = func() (*db.Store, func(), error) {
		time.Sleep(5 * time.Second)
		return nil, nil, fmt.Errorf("blackholed")
	}
	settleDeniedTimeout = 200 * time.Millisecond
	t.Cleanup(func() { settleDeniedOpenStore, settleDeniedTimeout = oldOpen, oldTimeout })
	start := time.Now()
	stdout.Reset()
	err = run([]string{
		"-strategy=model", "-execute", "-json",
		"-provider=local-qwen", "-model=qwen2.5-7b", "-endpoint-id=hl3",
		"-project=agent-a3ed1dd23680b2e9b", "-session-id=s1",
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("denied project with blocked store must still skip cleanly, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("blocked store open must be bounded, took %v", elapsed)
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if out["skipped"] != "project_denied" || out["intent_served"] != false {
		t.Fatalf("bounded denial must keep the marker and report unserved: %v", out)
	}
}

// TestCLIExecuteIgnoreDenylistBypasses proves -ignore-denylist reaches past the
// filter: with no endpoints registry the run then fails on the registry, which
// is only reachable if the denylist gate was bypassed.
func TestCLIExecuteIgnoreDenylistBypasses(t *testing.T) {
	home := setupCLIEnv(t)
	deny := `{"deny_projects":["agent-*"]}`
	if err := os.WriteFile(filepath.Join(home, "derive-denylist.json"), []byte(deny), 0o600); err != nil {
		t.Fatal(err)
	}

	err := run([]string{
		"-strategy=model", "-execute", "-ignore-denylist",
		"-provider=local-qwen", "-model=qwen2.5-7b", "-endpoint-id=hl3",
		"-project=agent-a3ed1dd23680b2e9b", "-session-id=s1",
	}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "model endpoints config") {
		t.Fatalf("expected endpoints-registry error past the denylist, got: %v", err)
	}
}

// TestCLIExecuteMalformedDenylistFailsLoud pins that a broken denylist file is
// an operator error, not a silently disabled filter.
func TestCLIExecuteMalformedDenylistFailsLoud(t *testing.T) {
	home := setupCLIEnv(t)
	if err := os.WriteFile(filepath.Join(home, "derive-denylist.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{
		"-strategy=model", "-execute",
		"-provider=local-qwen", "-model=qwen2.5-7b", "-endpoint-id=hl3",
		"-project=memoire", "-session-id=s1",
	}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "denylist") {
		t.Fatalf("expected loud denylist error, got: %v", err)
	}
}

// TestCLIExecuteDegenerateWindowSkips drives the min-content guard through the
// CLI: a near-empty session emits a skip marker, persists no run, and never
// reaches the provider (the fake server would fail the test on contact).
func TestCLIExecuteDegenerateWindowSkips(t *testing.T) {
	home := setupCLIEnv(t)
	seedSession(t, "memoire-test", "sess-tiny", []int64{7000, 7100}, []string{"y", "ok proceed"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("provider must not be contacted for a degenerate window: %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	writeEndpointsConfig(t, home, srv.URL, "MEMOIRE_TEST_QWEN_KEY", "qwen2.5-7b")
	t.Setenv("MEMOIRE_TEST_QWEN_KEY", "placeholder-not-a-real-token")

	var stdout bytes.Buffer
	err := run([]string{
		"-strategy=model", "-execute", "-json",
		"-provider=local-qwen", "-model=qwen2.5-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-tiny",
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("degenerate window must skip cleanly, got error: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if out["skipped"] != "degenerate_window" {
		t.Fatalf("skipped = %v, want degenerate_window (out=%v)", out["skipped"], out)
	}
	if n := countDerivationRuns(t, "memoire-test"); n != 0 {
		t.Fatalf("degenerate skip must persist no run, got %d", n)
	}
}

// TestCLIListTargets pins the sweep-helper contract end to end through the
// real CLI: store selection via config.Load, the 0x1f field framing the
// sweep script splits on, deterministic ordering, and the -since-hours
// recency filter (derive-seam-port round-1 MED-1: the CLI seam between the
// store methods and the shell fakes had no coverage of its own).
func TestCLIListTargets(t *testing.T) {
	setupCLIEnv(t)
	now := time.Now().Unix()
	seedSession(t, "helper-proj", "helper-sess", []int64{now - 300}, []string{"lt-one"})
	seedSession(t, "helper-proj2", "helper-sess-b", []int64{now - 200}, []string{"lt-two"})
	seedSession(t, "helper-old", "helper-sess-old", []int64{now - 3*3600}, []string{"lt-old"})

	var out bytes.Buffer
	if err := run([]string{"-list-targets", "-since-hours", "1"}, &out, io.Discard); err != nil {
		t.Fatalf("-list-targets: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	want := []string{
		"helper-proj\x1fhelper-sess",
		"helper-proj2\x1fhelper-sess-b",
	}
	if len(got) != len(want) {
		t.Fatalf("targets: want %d lines %q, got %d %q", len(want), want, len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("target line %d: want %q, got %q", i, want[i], got[i])
		}
	}

	out.Reset()
	if err := run([]string{"-list-targets", "-since-hours", "4"}, &out, io.Discard); err != nil {
		t.Fatalf("-list-targets wide: %v", err)
	}
	if !strings.Contains(out.String(), "helper-old\x1fhelper-sess-old") {
		t.Fatalf("wide window must include the old session, got %q", out.String())
	}

	if err := run([]string{"-list-targets", "-since-hours", "0"}, io.Discard, io.Discard); err == nil {
		t.Fatal("-since-hours 0 must error")
	}
	if err := run([]string{"-list-targets", "-median-epoch"}, io.Discard, io.Discard); err == nil {
		t.Fatal("-list-targets with -median-epoch must error (mutually exclusive)")
	}
}

// TestCLIMedianEpoch pins the split-point helper: odd/even truncation
// through the real CLI, empty-window empty output (exit nil), and loud
// usage errors for missing arguments.
func TestCLIMedianEpoch(t *testing.T) {
	setupCLIEnv(t)
	now := time.Now().Unix()
	epochs := []int64{now - 500, now - 400, now - 300, now - 200, now - 100}
	seedSession(t, "median-proj", "median-sess", epochs,
		[]string{"me-a", "me-b", "me-c", "me-d", "me-e"})

	var out bytes.Buffer
	args := func(lo, hi int64) []string {
		return []string{"-median-epoch", "-project", "median-proj", "-session-id", "median-sess",
			"-start-epoch", fmt.Sprintf("%d", lo), "-end-epoch", fmt.Sprintf("%d", hi)}
	}
	if err := run(args(now-500, now-100), &out, io.Discard); err != nil {
		t.Fatalf("median odd: %v", err)
	}
	if want := fmt.Sprintf("%d\n", now-300); out.String() != want {
		t.Fatalf("median odd: want %q, got %q", want, out.String())
	}

	out.Reset()
	if err := run(args(now-400, now-100), &out, io.Discard); err != nil {
		t.Fatalf("median even: %v", err)
	}
	if want := fmt.Sprintf("%d\n", now-300); out.String() != want {
		t.Fatalf("median even (truncation): want %q, got %q", want, out.String())
	}

	out.Reset()
	if err := run(args(now+100, now+200), &out, io.Discard); err != nil {
		t.Fatalf("median empty window must not error: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("median empty window: want empty output, got %q", out.String())
	}

	if err := run([]string{"-median-epoch", "-project", "median-proj", "-session-id", "median-sess"},
		io.Discard, io.Discard); err == nil {
		t.Fatal("median without window must error")
	}
	if err := run([]string{"-median-epoch", "-session-id", "median-sess",
		"-start-epoch", "1", "-end-epoch", "2"}, io.Discard, io.Discard); err == nil {
		t.Fatal("median without project must error")
	}
}

// serveDeriveIntent (memoire:D165 R-10.2, round-1 MED-3): the FIRST
// completed run closes an open intent with latency; a second completion is
// a no-op; store errors emit a stderr marker instead of silently degrading.
func TestServeDeriveIntent(t *testing.T) {
	setupCLIEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	store, err := db.Open(cfg.DBPath, cfg.SchemaDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if _, err := store.NotifyDeriveIntent(context.Background(), "svc-proj", "svc-sess", time.Now().Unix()-30); err != nil {
		t.Fatalf("notify: %v", err)
	}
	lat, served := serveDeriveIntent(store, "svc-proj", "svc-sess", "run-x", time.Now().Unix())
	if !served || lat < 25 || lat > 60 {
		t.Fatalf("first completion must serve with ~30s latency, got served=%v lat=%d", served, lat)
	}
	if _, served := serveDeriveIntent(store, "svc-proj", "svc-sess", "run-y", time.Now().Unix()); served {
		t.Fatal("second completion must be a no-op")
	}
	// Round-2 MED-NEW-1: a run created before the notify must not serve.
	if _, err := store.NotifyDeriveIntent(context.Background(), "svc-proj", "svc-pre", time.Now().Unix()); err != nil {
		t.Fatalf("pre notify: %v", err)
	}
	if _, served := serveDeriveIntent(store, "svc-proj", "svc-pre", "run-old", time.Now().Unix()-3600); served {
		t.Fatal("pre-notify run must not serve the intent")
	}
	if _, served := serveDeriveIntent(store, "", "svc-sess", "run-z", time.Now().Unix()); served {
		t.Fatal("empty project must not serve")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// A closed store is a real error: not served, and the helper must emit
	// its non-secret stderr marker (captured via os.Pipe around os.Stderr).
	if _, err := store.NotifyDeriveIntent(context.Background(), "svc-proj", "svc-2", 1); err == nil {
		t.Fatal("sanity: closed store should error")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	_, served = serveDeriveIntent(store, "svc-proj", "svc-sess2", "run-e", time.Now().Unix())
	os.Stderr = oldStderr
	_ = w.Close()
	msg, _ := io.ReadAll(r)
	if served {
		t.Fatal("errored completion must not report served")
	}
	if !strings.Contains(string(msg), "could not be recorded") {
		t.Fatalf("store error must emit the stderr marker, got: %q", string(msg))
	}
}

func TestCLIDryRunCarriesAlreadyCapturedContext(t *testing.T) {
	// Prompt v2.4 parity: the dry-run renders the same already-captured
	// section a run would send, and reports its count in prompt_meta. Raw
	// event mirrors must not feed the section — only a real derived
	// observation makes it appear.
	setupCLIEnv(t)
	eventIDs := seedSession(t, "memoire-test", "sess-ctx", []int64{2000, 2100}, []string{"first prompt", "second prompt"})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	store, err := db.Open(cfg.DBPath, cfg.SchemaDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer store.Close()
	dryRun := func() map[string]any {
		var stdout bytes.Buffer
		err := run([]string{
			"-strategy=model", "-dry-run", "-json",
			"-provider=local-qwen", "-model=qwen-7b", "-endpoint-id=hl3",
			"-project=memoire-test", "-session-id=sess-ctx",
		}, &stdout, io.Discard)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return out
	}

	meta := dryRun()["prompt_meta"].(map[string]any)
	if got := meta["already_captured_count"].(float64); got != 0 {
		t.Fatalf("raw event mirrors must not count as captured context, got %v", got)
	}

	runRes, err := store.EnsureDerivationRun(context.Background(), db.DerivationRunInput{
		Project: "memoire-test", SessionID: "sess-ctx", Provider: "local-qwen",
		Model: "qwen-7b", EndpointID: "hl3", DataBoundary: "local",
		DerivationVersion: "v2", InputHash: "ctx-hash", WindowStartEpoch: 2000,
		WindowEndEpoch: 2100, Status: "running",
	})
	if err != nil {
		t.Fatalf("run row: %v", err)
	}
	conf := 0.9
	if _, err := store.InsertDerivedObservation(context.Background(), db.DerivedObservationInput{
		Project: "memoire-test", SessionID: "sess-ctx", Kind: "fact",
		Text: "the first prompt was captured", Confidence: &conf, ReviewState: "proposed",
		DerivationRunID: runRes.ID, ContentHash: "ctx-obs-hash",
		SourceEventIDs: []int64{eventIDs[0]}, ModelDerived: true,
	}); err != nil {
		t.Fatalf("insert derived: %v", err)
	}

	out := dryRun()
	meta = out["prompt_meta"].(map[string]any)
	if got := meta["already_captured_count"].(float64); got != 1 {
		t.Fatalf("already_captured_count = %v, want 1", got)
	}
	if bytes := out["prompt_bytes"].(float64); bytes <= 0 {
		t.Fatalf("prompt_bytes missing: %v", bytes)
	}
}

func TestCLIDryRunReportsOverCapWindow(t *testing.T) {
	// Round-1 finding 2 follow-through: when the WINDOW alone exceeds
	// -max-prompt-bytes, the dry-run's job is sizing, not gating — it must
	// report the capless base size (with prompt_over_cap) so the sweep's
	// split loop can halve the window instead of falling into the
	// fail-open direct execute.
	setupCLIEnv(t)
	seedSession(t, "memoire-test", "sess-overcap", []int64{2000}, []string{strings.Repeat("big window prompt ", 100)})

	var stdout bytes.Buffer
	err := run([]string{
		"-strategy=model", "-dry-run", "-json",
		"-provider=local-qwen", "-model=qwen-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-overcap",
		"-max-prompt-bytes=500",
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("over-cap dry-run must size, not error: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["prompt_over_cap"] != true {
		t.Fatalf("prompt_over_cap marker missing: %v", out["prompt_over_cap"])
	}
	if pb := out["prompt_bytes"].(float64); pb <= 500 {
		t.Fatalf("prompt_bytes must report the real base size, got %v", pb)
	}
}

// --- byok CLI guards (docs/BYOK-DERIVE-DESIGN.md; memoire:D232) -------------

// TestCLIByokExecuteRequiresExplicitDialect pins ruling Q1 (2026-08-05):
// no dialect auto-detection — a byok execute without MEMOIRE_BYOK_DIALECT
// (or -byok-dialect) refuses naming the closed set.
func TestCLIByokExecuteRequiresExplicitDialect(t *testing.T) {
	setupCLIEnv(t)
	t.Setenv("MEMOIRE_BYOK_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("MEMOIRE_BYOK_DATA_BOUNDARY", "local")
	err := run([]string{"-strategy=model", "-provider=byok", "-model=m", "-execute",
		"-project=p", "-session-id=s"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "closed dialect set") {
		t.Fatalf("byok execute without a dialect must refuse naming the closed set, got %v", err)
	}
}

// TestCLIByokExecuteRequiresBoundaryDeclaration pins ruling Q2: no default
// data boundary — the refusal names the declaration, loudly.
func TestCLIByokExecuteRequiresBoundaryDeclaration(t *testing.T) {
	setupCLIEnv(t)
	t.Setenv("MEMOIRE_BYOK_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("MEMOIRE_BYOK_DIALECT", "openai-v1")
	err := run([]string{"-strategy=model", "-provider=byok", "-model=m", "-execute",
		"-project=p", "-session-id=s"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "data boundary") {
		t.Fatalf("byok execute without a boundary declaration must refuse, got %v", err)
	}
}

// TestCLIByokExternalRequiresOptIn pins the per-run external opt-in
// (docs/MODEL-PROVIDER.md "Data Boundary Discipline"): a declared-external
// endpoint refuses without -allow-external on the invocation.
func TestCLIByokExternalRequiresOptIn(t *testing.T) {
	setupCLIEnv(t)
	t.Setenv("MEMOIRE_BYOK_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("MEMOIRE_BYOK_DIALECT", "openai-v1")
	t.Setenv("MEMOIRE_BYOK_DATA_BOUNDARY", "external")
	err := run([]string{"-strategy=model", "-provider=byok", "-model=m", "-execute",
		"-project=p", "-session-id=s"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "-allow-external") {
		t.Fatalf("external boundary without -allow-external must refuse, got %v", err)
	}
}

// TestCLIByokWorldReadableKeyFileRefused pins key-file custody: a
// world-readable key file is refused before any store or network access.
func TestCLIByokWorldReadableKeyFileRefused(t *testing.T) {
	home := setupCLIEnv(t)
	keyPath := filepath.Join(home, "byok-key")
	if err := os.WriteFile(keyPath, []byte("placeholder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMOIRE_BYOK_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("MEMOIRE_BYOK_DIALECT", "openai-v1")
	t.Setenv("MEMOIRE_BYOK_DATA_BOUNDARY", "local")
	t.Setenv("MEMOIRE_BYOK_API_KEY_FILE", keyPath)
	err := run([]string{"-strategy=model", "-provider=byok", "-model=m", "-execute",
		"-project=p", "-session-id=s"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "world-readable") {
		t.Fatalf("world-readable key file must refuse, got %v", err)
	}
}

// TestCLIByokModelFromEnv pins the env-first config surface: with
// MEMOIRE_BYOK_MODEL set, the generic -model flag may be omitted — the
// dry-run proceeds past flag validation to the (empty) window fetch.
func TestCLIByokModelFromEnv(t *testing.T) {
	setupCLIEnv(t)
	t.Setenv("MEMOIRE_BYOK_MODEL", "env-model")
	err := run([]string{"-strategy=model", "-provider=byok", "-dry-run",
		"-project=p", "-session-id=s"}, io.Discard, io.Discard)
	if err == nil || strings.Contains(err.Error(), "requires -model") {
		t.Fatalf("MEMOIRE_BYOK_MODEL must satisfy the model requirement, got %v", err)
	}
	if !strings.Contains(err.Error(), "no source events") {
		t.Fatalf("expected the empty-window error after passing validation, got %v", err)
	}
}

// profiledEndpointsConfig writes a registry whose single endpoint carries a
// derive profile: the 2026-08-25 re-bench geometry, whose ceiling is
// floor((32768-12288-512)*2.2) = 43929.
func profiledEndpointsConfig(t *testing.T, home, baseURL, model string) {
	t.Helper()
	cfgFile := map[string]any{
		"endpoints": []map[string]any{
			{
				"endpoint_id": "hl3",
				"base_url":    baseURL,
				"models":      []string{model},
				"profile": map[string]any{
					"ctx_tokens":            32768,
					"max_completion_tokens": 12288,
					"bytes_per_token":       2.2,
					"margin_tokens":         512,
					"thinking":              map[string]any{"policy": "budget", "budget_tokens": 8192},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(cfgFile, "", "  ")
	if err := os.WriteFile(filepath.Join(home, "model-endpoints.json"), b, 0o600); err != nil {
		t.Fatalf("write endpoints config: %v", err)
	}
}

// capturingQwenServer is fakeQwenServer plus the chat request the adapter
// actually sent, so a test can prove the resolved completion cap reached the
// wire rather than only the run record.
func capturingQwenServer(t *testing.T, content string, sent *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen2.5-7b"}]}`))
		case "/v1/chat/completions":
			body := map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			*sent = body
			resp := map[string]any{
				"choices": []map[string]any{
					{"message": map[string]any{"content": content}, "finish_reason": "stop"},
				},
				"usage": map[string]any{"prompt_tokens": 120, "completion_tokens": 30, "total_tokens": 150},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
}

// derivationRunMeta returns the single persisted run's decoded meta_json.
func derivationRunMeta(t *testing.T, project string) map[string]any {
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
	runs, err := store.ListDerivationRuns(context.Background(), db.DerivationRunFilter{Project: project})
	if err != nil {
		t.Fatalf("ListDerivationRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 derivation run, got %d", len(runs))
	}
	meta := map[string]any{}
	if err := json.Unmarshal(runs[0].Metadata, &meta); err != nil {
		t.Fatalf("decode run meta_json: %v", err)
	}
	return meta
}

// TestCLIExecuteAppliesEndpointProfile asserts the ruled precedence at its
// core: with no limit flags passed, the endpoint's declared geometry becomes
// the numbers the run enforces — the completion cap on the wire, and the
// resolved ceilings plus the serving thinking declaration in run provenance.
func TestCLIExecuteAppliesEndpointProfile(t *testing.T) {
	home := setupCLIEnv(t)
	ids := seedSubstantiveSession(t, "memoire-test", "sess-profile", 11000)

	modelJSON := fmt.Sprintf(`{"schema_version":"1","observations":[{"kind":"decision","text":"the endpoint profile supplies the ceiling","confidence":0.9,"source_event_ids":[%d],"rationale":"explicit"}]}`, ids[0])
	var sent map[string]any
	srv := capturingQwenServer(t, modelJSON, &sent)
	defer srv.Close()
	profiledEndpointsConfig(t, home, srv.URL, "qwen2.5-7b")

	err := run([]string{
		"-strategy=model", "-execute", "-json",
		"-provider=local-qwen", "-model=qwen2.5-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-profile",
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got, _ := sent["max_tokens"].(float64); got != 12288 {
		t.Fatalf("max_tokens on the wire = %v, want the profile's 12288", sent["max_tokens"])
	}
	meta := derivationRunMeta(t, "memoire-test")
	if got, _ := meta["max_prompt_bytes"].(float64); got != 43929 {
		t.Fatalf("run meta max_prompt_bytes = %v, want 43929", meta["max_prompt_bytes"])
	}
	if got, _ := meta["max_completion_tokens"].(float64); got != 12288 {
		t.Fatalf("run meta max_completion_tokens = %v, want 12288", meta["max_completion_tokens"])
	}
	if meta["thinking_policy"] != "budget" {
		t.Fatalf("run meta thinking_policy = %v, want budget", meta["thinking_policy"])
	}
	if got, _ := meta["thinking_budget_tokens"].(float64); got != 8192 {
		t.Fatalf("run meta thinking_budget_tokens = %v, want 8192", meta["thinking_budget_tokens"])
	}
}

// TestCLIExecuteFlagOutranksProfile pins the top of the precedence chain,
// including the subtle half: an explicitly passed 0 keeps its own meaning (no
// prompt cap) rather than reading as "unset, take the profile's".
func TestCLIExecuteFlagOutranksProfile(t *testing.T) {
	home := setupCLIEnv(t)
	ids := seedSubstantiveSession(t, "memoire-test", "sess-flagwins", 12000)

	modelJSON := fmt.Sprintf(`{"schema_version":"1","observations":[{"kind":"fact","text":"an explicit flag outranks the endpoint profile","confidence":0.8,"source_event_ids":[%d],"rationale":"explicit"}]}`, ids[0])
	var sent map[string]any
	srv := capturingQwenServer(t, modelJSON, &sent)
	defer srv.Close()
	profiledEndpointsConfig(t, home, srv.URL, "qwen2.5-7b")

	err := run([]string{
		"-strategy=model", "-execute", "-json",
		"-provider=local-qwen", "-model=qwen2.5-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-flagwins",
		"-max-prompt-bytes=0", "-max-completion-tokens=777",
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got, _ := sent["max_tokens"].(float64); got != 777 {
		t.Fatalf("max_tokens on the wire = %v, want the flag's 777", sent["max_tokens"])
	}
	meta := derivationRunMeta(t, "memoire-test")
	if got, _ := meta["max_prompt_bytes"].(float64); got != 0 {
		t.Fatalf("run meta max_prompt_bytes = %v, want 0 — an explicit 0 is a cap decision, not an absence", meta["max_prompt_bytes"])
	}
	// The thinking declaration describes how the endpoint is SERVED, so it is
	// recorded whether or not the operator overrode the numbers.
	if meta["thinking_policy"] != "budget" {
		t.Fatalf("run meta thinking_policy = %v, want budget", meta["thinking_policy"])
	}
}

// TestCLIDryRunEffectiveLimits covers both dry-run halves: the profile is
// applied when the registry has one, and a dry-run with NO registry at all
// keeps working exactly as before, reporting the built-in default. The
// resolution is best-effort by design — sizing must not gain a failure mode
// the execute path will report anyway.
func TestCLIDryRunEffectiveLimits(t *testing.T) {
	t.Run("registry absent keeps today's behavior", func(t *testing.T) {
		setupCLIEnv(t)
		seedSession(t, "memoire-test", "sess-nolimits", []int64{3000, 3100}, []string{"alpha", "beta"})

		var stdout bytes.Buffer
		err := run([]string{
			"-strategy=model", "-dry-run", "-json",
			"-provider=local-qwen", "-model=qwen-7b", "-endpoint-id=hl3",
			"-project=memoire-test", "-session-id=sess-nolimits",
		}, &stdout, io.Discard)
		if err != nil {
			t.Fatalf("a dry-run without a registry must still work: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
		}
		limits, ok := out["effective_limits"].(map[string]any)
		if !ok {
			t.Fatalf("effective_limits missing: %v", out["effective_limits"])
		}
		if limits["source"] != "default" {
			t.Fatalf("source = %v, want default", limits["source"])
		}
		if got, _ := limits["max_prompt_bytes"].(float64); got != 0 {
			t.Fatalf("max_prompt_bytes = %v, want 0 (uncapped, as today)", limits["max_prompt_bytes"])
		}
	})

	t.Run("profile supplies the sizing ceiling", func(t *testing.T) {
		home := setupCLIEnv(t)
		seedSession(t, "memoire-test", "sess-dryprofile", []int64{4000, 4100}, []string{"alpha", "beta"})
		profiledEndpointsConfig(t, home, "http://127.0.0.1:1", "qwen-7b")

		var stdout bytes.Buffer
		err := run([]string{
			"-strategy=model", "-dry-run", "-json",
			"-provider=local-qwen", "-model=qwen-7b", "-endpoint-id=hl3",
			"-project=memoire-test", "-session-id=sess-dryprofile",
		}, &stdout, io.Discard)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
		}
		limits, _ := out["effective_limits"].(map[string]any)
		if limits == nil {
			t.Fatalf("effective_limits missing: %v", out)
		}
		if limits["source"] != "profile" {
			t.Fatalf("source = %v, want profile", limits["source"])
		}
		if got, _ := limits["max_prompt_bytes"].(float64); got != 43929 {
			t.Fatalf("max_prompt_bytes = %v, want 43929", limits["max_prompt_bytes"])
		}
		if got, _ := limits["max_completion_tokens"].(float64); got != 12288 {
			t.Fatalf("max_completion_tokens = %v, want 12288", limits["max_completion_tokens"])
		}
	})
}

// TestCLIDryRunFullyCoveredCarriesEffectiveLimits pins review r1 F2: the
// fully_covered early return is still a dry-run JSON, so it must report the
// resolved ceilings like every other one. A caller learning an endpoint's
// geometry must not have to find an uncovered window first.
func TestCLIDryRunFullyCoveredCarriesEffectiveLimits(t *testing.T) {
	home := setupCLIEnv(t)
	ids := seedSubstantiveSession(t, "memoire-test", "sess-covered", 13000)

	modelJSON := fmt.Sprintf(`{"schema_version":"1","observations":[{"kind":"fact","text":"this window is now covered","confidence":0.8,"source_event_ids":[%d],"rationale":"explicit"}]}`, ids[0])
	var sent map[string]any
	srv := capturingQwenServer(t, modelJSON, &sent)
	defer srv.Close()
	profiledEndpointsConfig(t, home, srv.URL, "qwen2.5-7b")

	// Cover the window with a real run, so the dry-run below takes the
	// fully_covered branch rather than the normal path.
	if err := run([]string{
		"-strategy=model", "-execute", "-json",
		"-provider=local-qwen", "-model=qwen2.5-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-covered",
	}, io.Discard, io.Discard); err != nil {
		t.Fatalf("seeding execute run: %v", err)
	}

	var stdout bytes.Buffer
	if err := run([]string{
		"-strategy=model", "-dry-run", "-json",
		"-provider=local-qwen", "-model=qwen2.5-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-covered",
	}, &stdout, io.Discard); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if out["skipped"] != "fully_covered" {
		t.Fatalf("expected the fully_covered branch, got %v", out)
	}
	limits, ok := out["effective_limits"].(map[string]any)
	if !ok {
		t.Fatalf("fully_covered dry-run omitted effective_limits: %v", out)
	}
	if limits["source"] != "profile" {
		t.Fatalf("source = %v, want profile", limits["source"])
	}
	if got, _ := limits["max_prompt_bytes"].(float64); got != 43929 {
		t.Fatalf("max_prompt_bytes = %v, want 43929", limits["max_prompt_bytes"])
	}
}
