// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"memoire/services/api/internal/db"

	_ "github.com/mattn/go-sqlite3"
)

func TestGovernancePromptParserAndInvalidProvenance(t *testing.T) {
	req := Request{
		Provider:          "local-qwen",
		Model:             "qwen2.5-7b",
		EndpointID:        "hl3",
		DerivationVersion: "observation-derive-v1",
		PromptVersion:     "derive-observations-v1",
		OutputSchema:      SchemaVersion,
		Window:            Window{Project: "memoire-test", SessionID: "governance-parser"},
		Events: []db.EventRecord{{
			ID:              101,
			Project:         "memoire-test",
			SessionID:       "governance-parser",
			SourceAdapter:   "claude-code",
			EventType:       "claude_UserPromptSubmit",
			Payload:         json.RawMessage(`{"prompt":"scratch note"}`),
			OccurredAtEpoch: 1800000000,
		}},
		Limits: Limits{MaxInputEvents: 100, MaxPromptBytes: 200000, MaxOutputObservations: 20},
	}
	prompt, _, err := BuildPrompt(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"provenance"`, `"noise_candidate"`, "observed", "inferred", "generated"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt must instruct model governance field %q; prompt:\n%s", want, prompt)
		}
	}

	raw := governanceFixture(t, "testdata/governance-v1/noise_candidate_response.json", 101)
	parsed, err := ParseResponse(raw, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Observations) != 1 || len(parsed.RejectedCandidates) != 0 {
		t.Fatalf("expected one governance-tagged observation, got observations=%#v rejected=%#v", parsed.Observations, parsed.RejectedCandidates)
	}
	assertStringFieldValue(t, parsed.Observations[0], "Provenance", "generated")
	assertBoolFieldValue(t, parsed.Observations[0], "NoiseCandidate", true)
	if _, ok := parsed.Observations[0].Extra["provenance"]; ok {
		t.Fatalf("provenance must be parsed as a typed governance field, not left in Extra")
	}
	if _, ok := parsed.Observations[0].Extra["noise_candidate"]; ok {
		t.Fatalf("noise_candidate must be parsed as a typed governance field, not left in Extra")
	}

	invalid := governanceFixture(t, "testdata/governance-v1/invalid_user_confirmed_response.json", 101)
	parsed, err = ParseResponse(invalid, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Observations) != 0 || len(parsed.RejectedCandidates) != 1 {
		t.Fatalf("derivation must reject user_confirmed provenance, got observations=%#v rejected=%#v", parsed.Observations, parsed.RejectedCandidates)
	}
	if !strings.Contains(parsed.RejectedCandidates[0].Reason, "provenance") {
		t.Fatalf("expected rejected provenance reason, got %#v", parsed.RejectedCandidates[0])
	}
}

func TestGovernanceParserDowngradesObservedProvenance(t *testing.T) {
	// memoire:D136 R-1: a model never observes — a model-claimed "observed"
	// is downgraded to "inferred" fail-open (stored, never rejected) and
	// counted in ProvenanceDowngrades.
	req := Request{
		Window: Window{Project: "memoire-test", SessionID: "governance-parser-observed"},
		Events: []db.EventRecord{{
			ID:              101,
			Project:         "memoire-test",
			SessionID:       "governance-parser-observed",
			SourceAdapter:   "claude-code",
			EventType:       "claude_UserPromptSubmit",
			OccurredAtEpoch: 1800000000,
		}},
	}
	raw := []byte(`{"schema_version":"1","observations":[{"kind":"fact","text":"observed provenance is downgraded for model-derived rows","confidence":0.8,"source_event_ids":[101],"rationale":"source directly observed","provenance":"observed","noise_candidate":false}]}`)
	parsed, err := ParseResponse(raw, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Observations) != 1 || len(parsed.RejectedCandidates) != 0 {
		t.Fatalf("expected downgraded observation to be stored, got observations=%#v rejected=%#v", parsed.Observations, parsed.RejectedCandidates)
	}
	assertStringFieldValue(t, parsed.Observations[0], "Provenance", "inferred")
	if parsed.ProvenanceDowngrades != 1 {
		t.Fatalf("expected ProvenanceDowngrades=1, got %d", parsed.ProvenanceDowngrades)
	}

	// A candidate downgraded but then rejected on other grounds (ungrounded
	// source id) must NOT count — the counter reads stored rows only.
	rawRejected := []byte(`{"schema_version":"1","observations":[{"kind":"fact","text":"observed but ungrounded","confidence":0.8,"source_event_ids":[999],"rationale":"x","provenance":"observed","noise_candidate":false}]}`)
	parsed2, err := ParseResponse(rawRejected, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed2.Observations) != 0 || len(parsed2.RejectedCandidates) != 1 {
		t.Fatalf("expected ungrounded candidate rejected, got %#v", parsed2)
	}
	if parsed2.ProvenanceDowngrades != 0 {
		t.Fatalf("rejected candidates must not count as downgrades, got %d", parsed2.ProvenanceDowngrades)
	}

	// Reserved store/hook values still reject.
	rawReserved := []byte(`{"schema_version":"1","observations":[{"kind":"fact","text":"reserved provenance","confidence":0.8,"source_event_ids":[101],"rationale":"x","provenance":"user_confirmed","noise_candidate":false}]}`)
	parsed3, err := ParseResponse(rawReserved, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed3.Observations) != 0 || len(parsed3.RejectedCandidates) != 1 {
		t.Fatalf("expected user_confirmed rejected, got %#v", parsed3)
	}
}

func TestGovernanceOrchestratorWritesNoiseAndProvenanceAtDerivationTime(t *testing.T) {
	dbPath, store := openTestStore(t)
	sessionID := "governance-orchestrator-noise"
	eventID := seedPrompt(t, store, sessionID, "governance-orchestrator-noise-event", 1800000000, "record a scratch derivation note")

	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return ProviderResponse{
			RawResponse: governanceFixture(t, "testdata/governance-v1/noise_candidate_response.json", eventID),
			Usage:       Usage{PromptTokens: 120, CompletionTokens: 20, TotalTokens: 140},
		}, nil
	})
	out, err := newOrchestrator(store).Run(context.Background(), provider, orchRequest(sessionID))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusCompleted || out.WrittenCount != 1 {
		t.Fatalf("expected completed run with one write, got %#v", out)
	}

	var provenance string
	var noiseCandidate int
	conn, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.QueryRowContext(context.Background(), `SELECT provenance, noise_candidate FROM observations WHERE derivation_run_id = ?`, out.RunID).Scan(&provenance, &noiseCandidate); err != nil {
		t.Fatal(err)
	}
	if provenance != "generated" || noiseCandidate != 1 {
		t.Fatalf("expected orchestrator to write provenance=generated noise_candidate=1, got provenance=%q noise_candidate=%d", provenance, noiseCandidate)
	}
}

func governanceFixture(t *testing.T, path string, eventID int64) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(strings.ReplaceAll(string(b), "__EVENT_ID__", strconv.FormatInt(eventID, 10)))
}

func assertStringFieldValue(t *testing.T, target any, name, want string) {
	t.Helper()
	field := reflect.ValueOf(target).FieldByName(name)
	if !field.IsValid() {
		t.Fatalf("expected field %s on %T", name, target)
	}
	if field.Kind() != reflect.String {
		t.Fatalf("expected %s to be string, got %s", name, field.Kind())
	}
	if got := field.String(); got != want {
		t.Fatalf("expected %s=%q, got %q", name, want, got)
	}
}

func assertBoolFieldValue(t *testing.T, target any, name string, want bool) {
	t.Helper()
	field := reflect.ValueOf(target).FieldByName(name)
	if !field.IsValid() {
		t.Fatalf("expected field %s on %T", name, target)
	}
	var got bool
	switch field.Kind() {
	case reflect.Bool:
		got = field.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		got = field.Int() != 0
	default:
		t.Fatalf("expected %s to be bool or int-like, got %s", name, field.Kind())
	}
	if got != want {
		t.Fatalf("expected %s=%v, got %v", name, want, got)
	}
}
