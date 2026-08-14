// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"memoire/services/api/internal/db"

	_ "github.com/mattn/go-sqlite3"
)

// fakeProvider is a test double for the Provider interface. respond receives the
// orchestrator-populated request (including resolved source events) so a test
// can build a response referencing real seeded event ids.
type fakeProvider struct {
	name     string
	boundary string
	calls    int
	respond  func(ctx context.Context, req Request) (ProviderResponse, error)
}

func (f *fakeProvider) Name() string         { return f.name }
func (f *fakeProvider) DataBoundary() string { return f.boundary }
func (f *fakeProvider) Generate(ctx context.Context, req Request) (ProviderResponse, error) {
	f.calls++
	return f.respond(ctx, req)
}

func localFake(respond func(ctx context.Context, req Request) (ProviderResponse, error)) *fakeProvider {
	return &fakeProvider{name: "local-qwen", boundary: "local", respond: respond}
}

func openTestStore(t *testing.T) (string, *db.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "memoire.db")
	store, err := db.Open(dbPath, "../../../../schema")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return dbPath, store
}

func seedPrompt(t *testing.T, store *db.Store, sessionID, sourceEventID string, occurredAt int64, prompt string) int64 {
	t.Helper()
	res, err := store.Ingest(context.Background(), db.EventInput{
		Project:         "memoire-test",
		SessionID:       sessionID,
		SourceAdapter:   "claude-code",
		SourceEventID:   sourceEventID,
		EventType:       "claude_UserPromptSubmit",
		OccurredAtEpoch: occurredAt,
		Payload:         json.RawMessage(fmt.Sprintf(`{"prompt":%q}`, prompt)),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return res.ID
}

func orchRequest(sessionID string) Request {
	return Request{
		Provider:          "local-qwen",
		Model:             "qwen2.5-7b",
		EndpointID:        "hl3",
		DerivationVersion: "observation-derive-v1",
		PromptVersion:     "derive-observations-v1",
		OutputSchema:      SchemaVersion,
		Window:            Window{Project: "memoire-test", SessionID: sessionID},
		Limits:            Limits{MaxInputEvents: 100, MaxPromptBytes: 200000, MaxOutputObservations: 20},
	}
}

func validResponse(req Request) ProviderResponse {
	id := req.Events[0].ID
	raw := fmt.Sprintf(`{"schema_version":"1","observations":[{"kind":"decision","text":"chose Go for the API service","confidence":0.86,"source_event_ids":[%d],"rationale":"explicit"}]}`, id)
	return ProviderResponse{
		RawResponse: json.RawMessage(raw),
		Usage:       Usage{PromptTokens: 120, CompletionTokens: 30, TotalTokens: 150},
	}
}

func newOrchestrator(store *db.Store) *Orchestrator {
	// Guard disabled: these tests seed minimal windows to target other
	// behavior; guard_test.go pins the default-on min-content guard.
	return &Orchestrator{Store: store, Timeout: 2 * time.Second, Guard: WindowGuard{Disabled: true}, Now: func() int64 { return 1_700_000_000 }}
}

func TestOrchestratorValidation(t *testing.T) {
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()

	valid := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return validResponse(req), nil
	})

	if _, err := o.Run(ctx, nil, orchRequest("s")); err == nil {
		t.Fatalf("expected error for nil provider")
	}

	// Provider/request mismatch.
	mismatch := orchRequest("s")
	mismatch.Provider = "openai"
	if _, err := o.Run(ctx, valid, mismatch); err == nil {
		t.Fatalf("expected provider/request mismatch error")
	}

	noModel := orchRequest("s")
	noModel.Model = ""
	if _, err := o.Run(ctx, valid, noModel); err == nil {
		t.Fatalf("expected missing-model error")
	}

	noProject := orchRequest("s")
	noProject.Window.Project = ""
	if _, err := o.Run(ctx, valid, noProject); err == nil {
		t.Fatalf("expected missing-project error")
	}

	badWindow := orchRequest("s")
	badWindow.Window.StartEpoch = 200
	badWindow.Window.EndEpoch = 100
	if _, err := o.Run(ctx, valid, badWindow); err == nil {
		t.Fatalf("expected window range error")
	}

	noEndpoint := orchRequest("s")
	noEndpoint.EndpointID = ""
	if _, err := o.Run(ctx, valid, noEndpoint); err == nil {
		t.Fatalf("expected missing endpoint_id error for local-qwen")
	}

	if valid.calls != 0 {
		t.Fatalf("provider must not be called on validation failure, calls=%d", valid.calls)
	}
}

func TestOrchestratorDataBoundary(t *testing.T) {
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	// local-qwen declaring an external boundary must be rejected before any call.
	bad := &fakeProvider{name: "local-qwen", boundary: "external", respond: func(ctx context.Context, req Request) (ProviderResponse, error) {
		return validResponse(req), nil
	}}
	if _, err := o.Run(context.Background(), bad, orchRequest("s")); err == nil {
		t.Fatalf("expected data-boundary discipline error")
	}
	if bad.calls != 0 {
		t.Fatalf("provider must not be called on boundary mismatch, calls=%d", bad.calls)
	}
}

func TestOrchestratorCreatesRun(t *testing.T) {
	dbPath, store := openTestStore(t)
	_ = dbPath
	o := newOrchestrator(store)
	ctx := context.Background()
	seedPrompt(t, store, "sess-create", "e1", 1000, "Memoire decision: chose Go")

	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return validResponse(req), nil
	})
	out, err := o.Run(ctx, provider, orchRequest("sess-create"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	rec, err := store.GetDerivationRun(ctx, out.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if rec.Provider != "local-qwen" || rec.Model != "qwen2.5-7b" || rec.EndpointID != "hl3" {
		t.Fatalf("unexpected run identity: %+v", rec)
	}
	if rec.DataBoundary != "local" || rec.DerivationVersion != "observation-derive-v1" {
		t.Fatalf("unexpected run derivation metadata: %+v", rec)
	}
	if rec.InputHash == "" {
		t.Fatalf("expected input hash recorded")
	}
}

func TestOrchestratorDuplicateShortCircuit(t *testing.T) {
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()
	seedPrompt(t, store, "sess-dup", "e1", 1000, "Memoire decision: chose Go")

	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return validResponse(req), nil
	})
	if _, err := o.Run(ctx, provider, orchRequest("sess-dup")); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// The completed run's manifest covers every event, so the rerun skips
	// at the fetch (fully_covered) — the provider is never re-called and
	// no duplicate observations can be minted.
	out2, err := o.Run(ctx, provider, orchRequest("sess-dup"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if out2.Skipped != SkipReasonFullyCovered {
		t.Fatalf("expected fully_covered skip, got %#v", out2)
	}
	if provider.calls != 1 {
		t.Fatalf("provider must be called only once across duplicate runs, calls=%d", provider.calls)
	}
}

func TestOrchestratorWritesProposedObservationsAndCompletes(t *testing.T) {
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()
	seedPrompt(t, store, "sess-ok", "e1", 1000, "Memoire decision: chose Go")

	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return validResponse(req), nil
	})
	out, err := o.Run(ctx, provider, orchRequest("sess-ok"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Status != StatusCompleted {
		t.Fatalf("expected completed, got %q", out.Status)
	}
	if out.WrittenCount != 1 {
		t.Fatalf("expected 1 written, got %d", out.WrittenCount)
	}
	obs, _, err := store.ListObservations(ctx, db.ObservationFilter{Project: "memoire-test", DerivationRunID: out.RunID, ReviewState: "proposed"})
	if err != nil {
		t.Fatalf("list observations: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("expected 1 proposed observation, got %d", len(obs))
	}
	if obs[0].ReviewState != "proposed" || obs[0].Kind != "decision" {
		t.Fatalf("unexpected observation: %+v", obs[0])
	}
}

func TestOrchestratorMarkPartialOnMixedResponse(t *testing.T) {
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()
	seedPrompt(t, store, "sess-mixed", "e1", 1000, "Memoire decision: chose Go")

	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		id := req.Events[0].ID
		raw := fmt.Sprintf(`{"schema_version":"1","observations":[`+
			`{"kind":"decision","text":"valid one","confidence":0.9,"source_event_ids":[%d]},`+
			`{"kind":"fact","text":"out of window","confidence":0.9,"source_event_ids":[999999]}`+
			`]}`, id)
		return ProviderResponse{RawResponse: json.RawMessage(raw)}, nil
	})
	out, err := o.Run(ctx, provider, orchRequest("sess-mixed"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Status != StatusPartial {
		t.Fatalf("expected partial, got %q", out.Status)
	}
	if out.WrittenCount != 1 || out.RejectedCount != 1 {
		t.Fatalf("expected 1 written 1 rejected, got written=%d rejected=%d", out.WrittenCount, out.RejectedCount)
	}
}

func TestOrchestratorProviderTimeout(t *testing.T) {
	_, store := openTestStore(t)
	o := &Orchestrator{Store: store, Timeout: 20 * time.Millisecond, Guard: WindowGuard{Disabled: true}, Now: func() int64 { return 1_700_000_000 }}
	ctx := context.Background()
	seedPrompt(t, store, "sess-timeout", "e1", 1000, "Memoire decision: chose Go")

	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		<-ctx.Done()
		return ProviderResponse{}, ctx.Err()
	})
	out, err := o.Run(ctx, provider, orchRequest("sess-timeout"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Status != StatusFailed {
		t.Fatalf("expected failed on timeout, got %q", out.Status)
	}
	if out.WrittenCount != 0 {
		t.Fatalf("expected no observations on timeout, got %d", out.WrittenCount)
	}
	obs, _, _ := store.ListObservations(ctx, db.ObservationFilter{Project: "memoire-test", DerivationRunID: out.RunID, ReviewState: "proposed"})
	if len(obs) != 0 {
		t.Fatalf("expected no proposed rows on timeout, got %d", len(obs))
	}
}

func TestOrchestratorProviderRefusal(t *testing.T) {
	ctx := context.Background()

	// Refusal with no salvageable observations -> failed.
	t.Run("no observations", func(t *testing.T) {
		_, store := openTestStore(t)
		o := newOrchestrator(store)
		seedPrompt(t, store, "sess-refuse-1", "e1", 1000, "Memoire decision: chose Go")
		provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
			return ProviderResponse{Refusal: "model declined"}, nil
		})
		out, err := o.Run(ctx, provider, orchRequest("sess-refuse-1"))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if out.Status != StatusFailed {
			t.Fatalf("expected failed, got %q", out.Status)
		}
		if !out.Refused {
			t.Fatalf("expected refused flag")
		}
	})

	// Refusal but valid observations present -> partial.
	t.Run("with observations", func(t *testing.T) {
		_, store := openTestStore(t)
		o := newOrchestrator(store)
		seedPrompt(t, store, "sess-refuse-2", "e1", 1000, "Memoire decision: chose Go")
		provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
			r := validResponse(req)
			r.Refusal = "partial content policy note"
			return r, nil
		})
		out, err := o.Run(ctx, provider, orchRequest("sess-refuse-2"))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if out.Status != StatusPartial {
			t.Fatalf("expected partial, got %q", out.Status)
		}
		if out.WrittenCount != 1 {
			t.Fatalf("expected 1 written, got %d", out.WrittenCount)
		}
	})
}

func TestOrchestratorUnknownMetadataOnly(t *testing.T) {
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()
	seedPrompt(t, store, "sess-unknown", "e1", 1000, "Memoire decision: chose Go")

	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		id := req.Events[0].ID
		raw := fmt.Sprintf(`{"schema_version":"1","observations":[`+
			`{"kind":"decision","text":"valid one","confidence":0.9,"source_event_ids":[%d]},`+
			`{"kind":"unknown","text":"genuine ontology gap","source_event_ids":[%d],"unknown_analysis":{"tried_kinds":[`+
			`{"kind":"decision","why_not":"no decision settled"},{"kind":"fact","why_not":"not stable"},{"kind":"task","why_not":"not actionable"}],`+
			`"proposed_kind_hypothesis":"timeline - a chronological narrative that none of the sealed kinds capture"}}`+
			`]}`, id, id)
		return ProviderResponse{RawResponse: json.RawMessage(raw)}, nil
	})
	out, err := o.Run(ctx, provider, orchRequest("sess-unknown"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.UnknownCount != 1 {
		t.Fatalf("expected 1 unknown candidate, got %d", out.UnknownCount)
	}
	// Unknown must never become an observation row.
	all, _, err := store.ListObservations(ctx, db.ObservationFilter{Project: "memoire-test", DerivationRunID: out.RunID, ReviewState: "proposed"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, ob := range all {
		if ob.Kind == "unknown" {
			t.Fatalf("unknown must not be written as an observation row")
		}
	}
	if len(all) != 1 {
		t.Fatalf("expected exactly 1 written observation (the decision), got %d", len(all))
	}
	// Unknown is preserved in run metadata under unmapped_candidates.
	rec, err := store.GetDerivationRun(ctx, out.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(rec.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	um, ok := meta["unmapped_candidates"].([]any)
	if !ok || len(um) != 1 {
		t.Fatalf("expected 1 unmapped candidate in run metadata, got %v", meta["unmapped_candidates"])
	}
}

func TestOrchestratorRunMetadata(t *testing.T) {
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()
	seedPrompt(t, store, "sess-meta", "e1", 1000, "Memoire decision: chose Go")

	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return validResponse(req), nil
	})
	out, err := o.Run(ctx, provider, orchRequest("sess-meta"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	rec, err := store.GetDerivationRun(ctx, out.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(rec.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	for _, key := range []string{"provider", "model", "prompt_version", "response_schema_version", "usage", "unmapped_ratio"} {
		if _, ok := meta[key]; !ok {
			t.Fatalf("run metadata missing key %q: %v", key, meta)
		}
	}
	if meta["provider"] != "local-qwen" || meta["model"] != "qwen2.5-7b" {
		t.Fatalf("unexpected provider/model in metadata: %v", meta)
	}
	if meta["response_schema_version"] != SchemaVersion {
		t.Fatalf("unexpected schema version: %v", meta["response_schema_version"])
	}
	usage, ok := meta["usage"].(map[string]any)
	if !ok || usage["total_tokens"].(float64) != 150 {
		t.Fatalf("unexpected usage metadata: %v", meta["usage"])
	}
	// Completed run must NOT retain the raw provider response.
	if _, ok := meta["raw_response"]; ok {
		t.Fatalf("completed run must not retain raw_response")
	}
}

func TestOrchestratorRawResponseRetention(t *testing.T) {
	ctx := context.Background()

	// Completed run: no raw response retained.
	t.Run("completed has no raw", func(t *testing.T) {
		_, store := openTestStore(t)
		o := newOrchestrator(store)
		seedPrompt(t, store, "sess-raw-ok", "e1", 1000, "Memoire decision: chose Go")
		provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
			return validResponse(req), nil
		})
		out, _ := o.Run(ctx, provider, orchRequest("sess-raw-ok"))
		meta := runMeta(t, store, out.RunID)
		if _, ok := meta["raw_response"]; ok {
			t.Fatalf("completed run retained raw_response")
		}
	})

	// Partial run: raw response retained.
	t.Run("partial retains raw", func(t *testing.T) {
		_, store := openTestStore(t)
		o := newOrchestrator(store)
		seedPrompt(t, store, "sess-raw-partial", "e1", 1000, "Memoire decision: chose Go")
		provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
			id := req.Events[0].ID
			raw := fmt.Sprintf(`{"schema_version":"1","observations":[`+
				`{"kind":"decision","text":"valid one","confidence":0.9,"source_event_ids":[%d]},`+
				`{"kind":"fact","text":"oow","confidence":0.9,"source_event_ids":[999999]}]}`, id)
			return ProviderResponse{RawResponse: json.RawMessage(raw)}, nil
		})
		out, _ := o.Run(ctx, provider, orchRequest("sess-raw-partial"))
		if out.Status != StatusPartial {
			t.Fatalf("expected partial, got %q", out.Status)
		}
		meta := runMeta(t, store, out.RunID)
		if _, ok := meta["raw_response"]; !ok {
			t.Fatalf("partial run must retain raw_response")
		}
	})

	// Failed run (malformed top-level): raw response retained.
	t.Run("failed retains raw", func(t *testing.T) {
		_, store := openTestStore(t)
		o := newOrchestrator(store)
		seedPrompt(t, store, "sess-raw-fail", "e1", 1000, "Memoire decision: chose Go")
		provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
			return ProviderResponse{RawResponse: json.RawMessage(`{"schema_version":"1","observations": not json`)}, nil
		})
		out, _ := o.Run(ctx, provider, orchRequest("sess-raw-fail"))
		if out.Status != StatusFailed {
			t.Fatalf("expected failed, got %q", out.Status)
		}
		meta := runMeta(t, store, out.RunID)
		if _, ok := meta["raw_response"]; !ok {
			t.Fatalf("failed run must retain raw_response")
		}
		if _, ok := meta["failure"]; !ok {
			t.Fatalf("failed run must record failure metadata")
		}
	})
}

func TestOrchestratorEnforcesMaxOutputObservations(t *testing.T) {
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()
	seedPrompt(t, store, "sess-cap", "e1", 1000, "Memoire decision: chose Go")

	// Provider returns three valid observations; the cap allows only two.
	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		id := req.Events[0].ID
		raw := fmt.Sprintf(`{"schema_version":"1","observations":[`+
			`{"kind":"fact","text":"one","confidence":0.9,"source_event_ids":[%d]},`+
			`{"kind":"fact","text":"two","confidence":0.9,"source_event_ids":[%d]},`+
			`{"kind":"fact","text":"three","confidence":0.9,"source_event_ids":[%d]}]}`, id, id, id)
		return ProviderResponse{RawResponse: json.RawMessage(raw)}, nil
	})

	req := orchRequest("sess-cap")
	req.Limits.MaxOutputObservations = 2
	out, err := o.Run(ctx, provider, req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.WrittenCount != 2 {
		t.Fatalf("expected exactly 2 written under the cap, got %d", out.WrittenCount)
	}
	// Dropping any observation for the cap forces partial, not completed.
	if out.Status != StatusPartial {
		t.Fatalf("expected partial when the cap drops observations, got %q", out.Status)
	}
	// The store must hold only the two written rows, never all three.
	obs, _, err := store.ListObservations(ctx, db.ObservationFilter{Project: "memoire-test", DerivationRunID: out.RunID, ReviewState: "proposed"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(obs) != 2 {
		t.Fatalf("expected 2 persisted observations under the cap, got %d", len(obs))
	}
	// The overflow is captured as a rejected candidate with the cap reason.
	rec, err := store.GetDerivationRun(ctx, out.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(rec.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	rejected, ok := meta["rejected_candidates"].([]any)
	if !ok || len(rejected) != 1 {
		t.Fatalf("expected 1 rejected (cap overflow) candidate, got %v", meta["rejected_candidates"])
	}
	first := rejected[0].(map[string]any)
	if reason, _ := first["reason"].(string); !strings.Contains(reason, "MaxOutputObservations") {
		t.Fatalf("expected cap reason on rejected candidate, got %q", reason)
	}
	if class, _ := first["class"].(string); class != RejectClassOutputCap {
		t.Fatalf("cap overflow class = %q, want %q", class, RejectClassOutputCap)
	}
	counts, ok := meta["candidate_reason_counts"].(map[string]any)
	if !ok || counts[RejectClassOutputCap].(float64) != 1 {
		t.Fatalf("candidate_reason_counts missing output_cap=1: %v", meta["candidate_reason_counts"])
	}
}

func TestOrchestratorCapNoopWhenUnset(t *testing.T) {
	// A 0/unset cap imposes no implicit limit (no documented numeric default).
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()
	seedPrompt(t, store, "sess-nocap", "e1", 1000, "Memoire decision: chose Go")

	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		id := req.Events[0].ID
		raw := fmt.Sprintf(`{"schema_version":"1","observations":[`+
			`{"kind":"fact","text":"one","confidence":0.9,"source_event_ids":[%d]},`+
			`{"kind":"fact","text":"two","confidence":0.9,"source_event_ids":[%d]}]}`, id, id)
		return ProviderResponse{RawResponse: json.RawMessage(raw)}, nil
	})
	req := orchRequest("sess-nocap")
	req.Limits.MaxOutputObservations = 0
	out, err := o.Run(ctx, provider, req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.WrittenCount != 2 || out.Status != StatusCompleted {
		t.Fatalf("unset cap must not drop observations: written=%d status=%q", out.WrittenCount, out.Status)
	}
}

func TestOrchestratorCapturesExtrasIntoMetadata(t *testing.T) {
	dbPath, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()
	seedPrompt(t, store, "sess-extra", "e1", 1000, "Memoire decision: chose Go")

	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		id := req.Events[0].ID
		raw := fmt.Sprintf(`{"schema_version":"1","model_note":"ran hot","observations":[`+
			`{"kind":"fact","text":"kept","confidence":0.9,"source_event_ids":[%d],"model_score":0.42}]}`, id)
		return ProviderResponse{RawResponse: json.RawMessage(raw)}, nil
	})
	out, err := o.Run(ctx, provider, orchRequest("sess-extra"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// A salvageable extra-bearing response still completes (the observation is
	// fully valid; the extra is provenance, not a defect).
	if out.Status != StatusCompleted || out.WrittenCount != 1 {
		t.Fatalf("expected completed with 1 written, got status=%q written=%d", out.Status, out.WrittenCount)
	}

	meta := runMeta(t, store, out.RunID)
	respExtra, ok := meta["response_extra"].(map[string]any)
	if !ok {
		t.Fatalf("expected response_extra in run metadata, got %v", meta["response_extra"])
	}
	if _, ok := respExtra["model_note"]; !ok {
		t.Fatalf("expected captured top-level extra in run metadata, got %v", respExtra)
	}

	conn, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open conn: %v", err)
	}
	defer conn.Close()
	var metaJSON string
	if err := conn.QueryRowContext(ctx, `SELECT meta_json FROM observations WHERE derivation_run_id = ? AND review_state = 'proposed'`, out.RunID).Scan(&metaJSON); err != nil {
		t.Fatalf("query observation meta: %v", err)
	}
	var obsMeta map[string]any
	if err := json.Unmarshal([]byte(metaJSON), &obsMeta); err != nil {
		t.Fatalf("unmarshal obs meta: %v", err)
	}
	mre, ok := obsMeta["model_response_extra"].(map[string]any)
	if !ok {
		t.Fatalf("expected model_response_extra in observation metadata, got %v", obsMeta["model_response_extra"])
	}
	if _, ok := mre["model_score"]; !ok {
		t.Fatalf("expected captured candidate extra in observation metadata, got %v", mre)
	}
}

func runMeta(t *testing.T, store *db.Store, runID string) map[string]any {
	t.Helper()
	rec, err := store.GetDerivationRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(rec.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	return meta
}

// TestOrchestratorRunMetaCandidateReasonCounts pins the per-class counters
// that make a "partial, written=0" run explainable from run metadata alone.
func TestOrchestratorRunMetaCandidateReasonCounts(t *testing.T) {
	ctx := context.Background()

	t.Run("ungrounded and empty-response counters", func(t *testing.T) {
		_, store := openTestStore(t)
		o := newOrchestrator(store)
		seedPrompt(t, store, "sess-counts", "e1", 1000, "Memoire decision: chose Go")

		// All candidates ungrounded: partial, written=0, counters explain why.
		provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
			raw := `{"schema_version":"1","observations":[
			  {"kind":"decision","text":"a","confidence":0.9,"source_event_ids":[987654]},
			  {"kind":"fact","text":"b","confidence":0.9,"source_event_ids":[987655]}
			]}`
			return ProviderResponse{RawResponse: json.RawMessage(raw)}, nil
		})
		out, err := o.Run(ctx, provider, orchRequest("sess-counts"))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if out.Status != StatusPartial || out.WrittenCount != 0 {
			t.Fatalf("want partial written=0, got %q written=%d", out.Status, out.WrittenCount)
		}
		meta := runMeta(t, store, out.RunID)
		counts, ok := meta["candidate_reason_counts"].(map[string]any)
		if !ok {
			t.Fatalf("run metadata missing candidate_reason_counts: %v", meta)
		}
		if counts[RejectClassUngrounded].(float64) != 2 {
			t.Fatalf("ungrounded count = %v, want 2 (%v)", counts[RejectClassUngrounded], counts)
		}
	})

	t.Run("zero-candidate response gets an explicit marker", func(t *testing.T) {
		_, store := openTestStore(t)
		o := newOrchestrator(store)
		seedPrompt(t, store, "sess-empty", "e1", 1000, "Memoire decision: chose Go")

		provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
			return ProviderResponse{RawResponse: json.RawMessage(`{"schema_version":"1","observations":[]}`)}, nil
		})
		out, err := o.Run(ctx, provider, orchRequest("sess-empty"))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		meta := runMeta(t, store, out.RunID)
		counts, ok := meta["candidate_reason_counts"].(map[string]any)
		if !ok {
			t.Fatalf("run metadata missing candidate_reason_counts: %v", meta)
		}
		if counts[RejectClassEmptyResponse].(float64) != 1 {
			t.Fatalf("empty_observations = %v, want 1 (%v)", counts[RejectClassEmptyResponse], counts)
		}
	})

	t.Run("completed clean run carries no counters", func(t *testing.T) {
		_, store := openTestStore(t)
		o := newOrchestrator(store)
		seedPrompt(t, store, "sess-clean", "e1", 1000, "Memoire decision: chose Go")

		provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
			return validResponse(req), nil
		})
		out, err := o.Run(ctx, provider, orchRequest("sess-clean"))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		meta := runMeta(t, store, out.RunID)
		if _, ok := meta["candidate_reason_counts"]; ok {
			t.Fatalf("clean completed run must not carry candidate_reason_counts")
		}
	})

	t.Run("provider failure carries failure metadata, not counters", func(t *testing.T) {
		_, store := openTestStore(t)
		o := newOrchestrator(store)
		seedPrompt(t, store, "sess-generr", "e1", 1000, "Memoire decision: chose Go")

		provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
			return ProviderResponse{}, errors.New("boom: endpoint unreachable")
		})
		out, err := o.Run(ctx, provider, orchRequest("sess-generr"))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if out.Status != StatusFailed {
			t.Fatalf("want failed, got %q", out.Status)
		}
		meta := runMeta(t, store, out.RunID)
		if _, ok := meta["candidate_reason_counts"]; ok {
			t.Fatalf("provider failure is explained by failure metadata, not counters")
		}
		if _, ok := meta["failure"]; !ok {
			t.Fatalf("failed run must record failure metadata")
		}
	})

	t.Run("refusal with no candidates carries refusal, not an empty marker", func(t *testing.T) {
		_, store := openTestStore(t)
		o := newOrchestrator(store)
		seedPrompt(t, store, "sess-refuse", "e1", 1000, "Memoire decision: chose Go")

		provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
			return ProviderResponse{Refusal: "content_filter"}, nil
		})
		out, err := o.Run(ctx, provider, orchRequest("sess-refuse"))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if out.Status != StatusFailed {
			t.Fatalf("want failed on unsalvaged refusal, got %q", out.Status)
		}
		meta := runMeta(t, store, out.RunID)
		if _, ok := meta["candidate_reason_counts"]; ok {
			t.Fatalf("refusal is explained by the refusal field, not counters")
		}
		if meta["refusal"] != "content_filter" {
			t.Fatalf("refusal field = %v, want content_filter", meta["refusal"])
		}
	})
}

// -force-rerun (turn-ingest Phase 1, slice 4): a forced request mints a new
// run where an unforced one dedups, calls the provider again, and records
// forced provenance in the final run metadata.
func TestOrchestratorForceRerun(t *testing.T) {
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()
	seedPrompt(t, store, "sess-force", "e1", 1000, "Memoire decision: chose Go")

	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return validResponse(req), nil
	})
	first, err := o.Run(ctx, provider, orchRequest("sess-force"))
	if err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Unforced rerun skips at the fetch (fully covered) and never reaches
	// the provider.
	dup, err := o.Run(ctx, provider, orchRequest("sess-force"))
	if err != nil {
		t.Fatalf("dup run: %v", err)
	}
	if dup.Skipped != SkipReasonFullyCovered || provider.calls != 1 {
		t.Fatalf("unforced rerun must skip fully_covered (got %#v calls=%d)", dup, provider.calls)
	}

	forcedReq := orchRequest("sess-force")
	forcedReq.ForceRerun = true
	forced, err := o.Run(ctx, provider, forcedReq)
	if err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if forced.Duplicate {
		t.Fatal("forced run must not dedup")
	}
	if !forced.Forced || forced.SupersededRunID != first.RunID {
		t.Fatalf("forced outcome = %#v, want Forced superseding %s", forced, first.RunID)
	}
	if forced.RunID == first.RunID {
		t.Fatal("forced run must mint a new run id")
	}
	if provider.calls != 2 {
		t.Fatalf("forced rerun must call the provider again, calls=%d", provider.calls)
	}
	if forced.Status != StatusCompleted || forced.WrittenCount != 1 {
		t.Fatalf("forced run outcome: %#v", forced)
	}

	rec, err := store.GetDerivationRun(ctx, forced.RunID)
	if err != nil {
		t.Fatalf("get forced run: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(rec.Metadata, &meta); err != nil {
		t.Fatalf("forced run meta: %v", err)
	}
	if meta["forced"] != true || meta["superseded_run_id"] != first.RunID {
		t.Fatalf("final meta missing forced provenance: forced=%v superseded=%v", meta["forced"], meta["superseded_run_id"])
	}
	canonical, ok := meta["canonical_input_hash"].(string)
	if !ok || canonical == "" {
		t.Fatalf("final meta missing canonical_input_hash: %v", meta["canonical_input_hash"])
	}
	if meta["input_hash"] != canonical {
		t.Fatalf("meta input_hash %v must stay canonical %q", meta["input_hash"], canonical)
	}
	if rec.InputHash == canonical || !strings.Contains(rec.InputHash, "#forced-") {
		t.Fatalf("stored input_hash must be the salted variant, got %q", rec.InputHash)
	}
}

func TestOrchestratorCapKeepsDowngradeCountHonest(t *testing.T) {
	// memoire:D136 R-1 + round-1 review finding 1: provenance_downgrades is a
	// STORED-row count. Two downgraded (observed->inferred) candidates under
	// MaxOutputObservations=1 must report exactly 1 downgrade, not 2.
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()
	seedPrompt(t, store, "sess-cap-downgrade", "e1", 1000, "Memoire decision: chose Go")

	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		id := req.Events[0].ID
		raw := fmt.Sprintf(`{"schema_version":"1","observations":[`+
			`{"kind":"fact","text":"kept","confidence":0.9,"source_event_ids":[%d],"provenance":"observed"},`+
			`{"kind":"fact","text":"capped","confidence":0.9,"source_event_ids":[%d],"provenance":"observed"}]}`, id, id)
		return ProviderResponse{RawResponse: json.RawMessage(raw)}, nil
	})

	req := orchRequest("sess-cap-downgrade")
	req.Limits.MaxOutputObservations = 1
	out, err := o.Run(ctx, provider, req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.WrittenCount != 1 {
		t.Fatalf("expected 1 written under the cap, got %d", out.WrittenCount)
	}
	rec, err := store.GetDerivationRun(ctx, out.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(rec.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if got, _ := meta["provenance_downgrades"].(float64); got != 1 {
		t.Fatalf("provenance_downgrades must count stored rows only: want 1, got %v", meta["provenance_downgrades"])
	}
	// The stored row carries the downgraded provenance.
	obs, _, err := store.ListObservations(ctx, db.ObservationFilter{Project: "memoire-test", DerivationRunID: out.RunID, ReviewState: "proposed"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(obs) != 1 || obs[0].Provenance != "inferred" {
		t.Fatalf("expected 1 stored row with provenance inferred, got %#v", obs)
	}
}

// raceIngestStore drives the round-1 F1 interleaving deterministically: the
// first (excluded) fetch runs against a fully covered window, and a new event
// commits immediately after it returns — before the unexcluded probe. The
// inflated MaxEventID mirrors the PG lower-id late-commit case
// (deriveintents.go round-3 HIGH-1): the raced row is INSIDE this attempt's
// snapshot bound, so the re-fetch must see and derive it.
type raceIngestStore struct {
	OrchestratorStore
	inject func()
}

func (s *raceIngestStore) MaxEventID(ctx context.Context) (int64, error) {
	id, err := s.OrchestratorStore.MaxEventID(ctx)
	return id + 1000, err
}

func (s *raceIngestStore) ListEventsForDerivation(ctx context.Context, f db.DerivationEventFilter) ([]db.EventRecord, error) {
	events, err := s.OrchestratorStore.ListEventsForDerivation(ctx, f)
	if s.inject != nil {
		inject := s.inject
		s.inject = nil
		inject()
	}
	return events, err
}

func TestOrchestratorFullyCoveredRaceDerivesLateCommit(t *testing.T) {
	_, store := openTestStore(t)
	ctx := context.Background()
	seedPrompt(t, store, "sess-race", "e1", 1000, "Memoire decision: chose Go")

	// The fixture text is the event's own content, not a shared template:
	// two runs emitting template-identical texts would collide in the
	// near-dup gate and mask this test's race assertion behind a rejection —
	// and the seeds are lexically disjoint for the same reason.
	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		e := req.Events[0]
		text, _ := json.Marshal(e.Text)
		raw := fmt.Sprintf(`{"schema_version":"1","observations":[{"kind":"decision","text":%s,"confidence":0.9,"source_event_ids":[%d],"rationale":"explicit"}]}`, text, e.ID)
		return ProviderResponse{RawResponse: json.RawMessage(raw)}, nil
	})
	if _, err := newOrchestrator(store).Run(ctx, provider, orchRequest("sess-race")); err != nil {
		t.Fatalf("first run: %v", err)
	}

	rs := &raceIngestStore{OrchestratorStore: store, inject: func() {
		seedPrompt(t, store, "sess-race", "e2-late-commit", 1001, "an ingest raced between the excluded fetch and the classifier snapshot")
	}}
	o := newOrchestrator(store)
	o.Store = rs
	out, err := o.Run(ctx, provider, orchRequest("sess-race"))
	if err != nil {
		t.Fatalf("raced run: %v", err)
	}
	if out.Skipped != "" {
		t.Fatalf("raced commit must be derived, never skipped: %#v", out)
	}
	if out.Status != StatusCompleted || out.WrittenCount != 1 {
		t.Fatalf("raced run must complete over the late event: %#v", out)
	}
	if provider.calls != 2 {
		t.Fatalf("provider must run for the raced event, calls=%d", provider.calls)
	}

	// The raced event is now covered: a plain rerun reports fully_covered.
	out3, err := newOrchestrator(store).Run(ctx, provider, orchRequest("sess-race"))
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if out3.Skipped != SkipReasonFullyCovered {
		t.Fatalf("post-race rerun must skip fully_covered, got %#v", out3)
	}
}

// obsResponse builds a single-observation provider response with the given
// text, grounded on the first window event.
func obsResponse(req Request, kind, text string) ProviderResponse {
	enc, _ := json.Marshal(text)
	raw := fmt.Sprintf(`{"schema_version":"1","observations":[{"kind":%q,"text":%s,"confidence":0.9,"source_event_ids":[%d],"rationale":"explicit"}]}`, kind, enc, req.Events[0].ID)
	return ProviderResponse{RawResponse: json.RawMessage(raw)}
}

func TestOrchestratorNearDupGateRejectsParaphrase(t *testing.T) {
	// D211 step 3 end-to-end on the founding pair (#347900/#348265): a new
	// event restating captured content is derived, its paraphrase candidate
	// is rejected by the gate, and the run still COMPLETES — coverage must
	// happen, or the same events would re-enter every future sweep fetch.
	_, store := openTestStore(t)
	ctx := context.Background()
	seedPrompt(t, store, "sess-neardup", "e1", 1000, "the load older button vanished after accepting the list")

	textA := `UI bug: "Load older" button disappears when a list is fully accepted/confirmed; expected to remain visible.`
	textB := `UI issue: 'Load older' button disappears after accepting/confirming a whole list; expected to remain visible.`

	first := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return obsResponse(req, "fact", textA), nil
	})
	out1, err := newOrchestrator(store).Run(ctx, first, orchRequest("sess-neardup"))
	if err != nil || out1.WrittenCount != 1 {
		t.Fatalf("first run: err=%v out=%#v", err, out1)
	}

	// A new event arrives (uncovered), restating the same content; the model
	// re-emits a paraphrase. The provider must see the already-captured
	// context carrying the first run's observation.
	seedPrompt(t, store, "sess-neardup", "e2", 1001, "still seeing that load older disappearance")
	var sawContext int
	second := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		sawContext = len(req.AlreadyCaptured)
		return obsResponse(req, "fact", textB), nil
	})
	out2, err := newOrchestrator(store).Run(ctx, second, orchRequest("sess-neardup"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if sawContext != 1 {
		t.Fatalf("already-captured context must carry the first observation, saw %d entries", sawContext)
	}
	if out2.WrittenCount != 0 || out2.RejectedCount != 1 {
		t.Fatalf("paraphrase must be gate-rejected: %#v", out2)
	}
	if out2.Status != StatusCompleted {
		t.Fatalf("near-dup-only response must still complete (coverage), got %q", out2.Status)
	}
	rec, err := store.GetDerivationRun(ctx, out2.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(rec.Metadata, &meta); err != nil {
		t.Fatalf("meta: %v", err)
	}
	counts, _ := meta["candidate_reason_counts"].(map[string]any)
	if got, _ := counts[RejectClassNearDuplicate].(float64); got != 1 {
		t.Fatalf("candidate_reason_counts[near_duplicate] = %v, want 1", counts[RejectClassNearDuplicate])
	}
	rejected, _ := meta["rejected_candidates"].([]any)
	if len(rejected) != 1 {
		t.Fatalf("rejected_candidates must retain the text (capture-don't-discard): %v", meta["rejected_candidates"])
	}
	rc, _ := rejected[0].(map[string]any)
	if rc["class"] != RejectClassNearDuplicate || rc["text"] != textB {
		t.Fatalf("rejected candidate must carry class+text: %v", rc)
	}
	ac, _ := meta["already_captured"].(map[string]any)
	if got, _ := ac["session_observations"].(float64); got != 1 {
		t.Fatalf("already_captured.session_observations = %v, want 1", ac)
	}

	// Loop prevention: the events are covered, a plain rerun skips.
	third := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return obsResponse(req, "fact", textB), nil
	})
	out3, err := newOrchestrator(store).Run(ctx, third, orchRequest("sess-neardup"))
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if out3.Skipped != SkipReasonFullyCovered || third.calls != 0 {
		t.Fatalf("gated run must still cover its events: %#v calls=%d", out3, third.calls)
	}
}

func TestOrchestratorNearDupDigitGuardProtectsNewOutcome(t *testing.T) {
	// A near-identical fact whose difference is the digit token (a new PR, a
	// new verdict value) is NEW content and must be written.
	_, store := openTestStore(t)
	ctx := context.Background()
	seedPrompt(t, store, "sess-digit", "e1", 1000, "review PR 67")

	first := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return obsResponse(req, "fact", "Background Codex xhigh security review of PR #67 completed successfully (exit code 0)."), nil
	})
	if out, err := newOrchestrator(store).Run(ctx, first, orchRequest("sess-digit")); err != nil || out.WrittenCount != 1 {
		t.Fatalf("first run: err=%v out=%#v", err, out)
	}

	seedPrompt(t, store, "sess-digit", "e2", 1001, "review PR 69")
	second := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return obsResponse(req, "fact", "Background Codex xhigh review of PR #69 completed successfully (exit code 0)."), nil
	})
	out, err := newOrchestrator(store).Run(ctx, second, orchRequest("sess-digit"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if out.WrittenCount != 1 || out.RejectedCount != 0 || out.Status != StatusCompleted {
		t.Fatalf("digit-differing outcome must survive the gate: %#v", out)
	}
}

func TestOrchestratorNearDupIntraRunDedup(t *testing.T) {
	// Two paraphrases inside ONE response: the first is written, the second
	// rejected against it — even on a session with no prior observations.
	_, store := openTestStore(t)
	ctx := context.Background()
	seedPrompt(t, store, "sess-intra", "e1", 1000, "the load older button vanished")

	textA := `UI bug: "Load older" button disappears when a list is fully accepted/confirmed; expected to remain visible.`
	textB := `UI issue: 'Load older' button disappears after accepting/confirming a whole list; expected to remain visible.`
	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		encA, _ := json.Marshal(textA)
		encB, _ := json.Marshal(textB)
		raw := fmt.Sprintf(`{"schema_version":"1","observations":[`+
			`{"kind":"fact","text":%s,"confidence":0.9,"source_event_ids":[%d]},`+
			`{"kind":"fact","text":%s,"confidence":0.9,"source_event_ids":[%d]}]}`,
			encA, req.Events[0].ID, encB, req.Events[0].ID)
		return ProviderResponse{RawResponse: json.RawMessage(raw)}, nil
	})
	out, err := newOrchestrator(store).Run(ctx, provider, orchRequest("sess-intra"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.WrittenCount != 1 || out.RejectedCount != 1 || out.Status != StatusCompleted {
		t.Fatalf("intra-run paraphrase must be rejected without degrading the run: %#v", out)
	}
	rec, err := store.GetDerivationRun(ctx, out.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(rec.Metadata, &meta); err != nil {
		t.Fatalf("meta: %v", err)
	}
	// Round-2: intra-run matches carry the inserted row's REAL id, so the
	// rejection names an observation — same shape (and same liveness path)
	// as a session-row match.
	obs, _, err := store.ListObservations(ctx, db.ObservationFilter{Project: "memoire-test", SessionID: "sess-intra", ReviewState: "proposed"})
	if err != nil || len(obs) != 1 {
		t.Fatalf("expected the single written row: %v %d", err, len(obs))
	}
	rejected, _ := meta["rejected_candidates"].([]any)
	rc, _ := rejected[0].(map[string]any)
	reason, _ := rc["reason"].(string)
	if !strings.Contains(reason, fmt.Sprintf("near-duplicate of observation %d", obs[0].ID)) {
		t.Fatalf("intra-run rejection must name the earlier candidate's row: %q", reason)
	}
}

func TestOrchestratorNearDupIntraRunMatchDeletedMidLoop(t *testing.T) {
	// Round-2 finding 1 residue: candidate B matching candidate A (written
	// moments earlier in the same response) must go through the SAME
	// in-transaction liveness check as any stored match. If A is
	// soft-deleted between the two iterations, B must be written — the old
	// index-only intra-run shortcut rejected B against a dead row.
	_, store := openTestStore(t)
	ctx := context.Background()
	seedPrompt(t, store, "sess-intra-del", "e1", 1000, "the load older button vanished")

	textA := `UI bug: "Load older" button disappears when a list is fully accepted/confirmed; expected to remain visible.`
	textB := `UI issue: 'Load older' button disappears after accepting/confirming a whole list; expected to remain visible.`
	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		encA, _ := json.Marshal(textA)
		encB, _ := json.Marshal(textB)
		raw := fmt.Sprintf(`{"schema_version":"1","observations":[`+
			`{"kind":"fact","text":%s,"confidence":0.9,"source_event_ids":[%d]},`+
			`{"kind":"fact","text":%s,"confidence":0.9,"source_event_ids":[%d]}]}`,
			encA, req.Events[0].ID, encB, req.Events[0].ID)
		return ProviderResponse{RawResponse: json.RawMessage(raw)}, nil
	})
	killer := &intraRunKillerStore{OrchestratorStore: store, store: store, t: t}
	o := newOrchestrator(store)
	o.Store = killer
	out, err := o.Run(ctx, provider, orchRequest("sess-intra-del"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.WrittenCount != 2 || out.RejectedCount != 0 {
		t.Fatalf("candidate matching a just-deleted intra-run row must be written: %#v", out)
	}
}

// intraRunKillerStore soft-deletes the first gated-insert's row right before
// the second gated insert runs, driving the between-iterations deletion
// deterministically.
type intraRunKillerStore struct {
	OrchestratorStore
	store   *db.Store
	t       *testing.T
	calls   int
	firstID int64
}

func (s *intraRunKillerStore) InsertDerivedObservationUnlessMatchedLive(ctx context.Context, in db.DerivedObservationInput, matchedIDs []int64) (db.DerivedObservationResult, int64, error) {
	s.calls++
	if s.calls == 2 && s.firstID > 0 {
		if _, err := s.store.TransitionObservationReview(ctx, s.firstID, "accepted", "intra fixture"); err != nil {
			s.t.Errorf("accept first: %v", err)
		}
		if _, err := s.store.TransitionObservationReview(ctx, s.firstID, "soft_deleted", "intra fixture"); err != nil {
			s.t.Errorf("soft-delete first: %v", err)
		}
	}
	res, live, err := s.OrchestratorStore.InsertDerivedObservationUnlessMatchedLive(ctx, in, matchedIDs)
	if s.calls == 1 {
		s.firstID = res.ID
	}
	return res, live, err
}

func TestOrchestratorForceRerunBypassesNearDupContext(t *testing.T) {
	// ForceRerun sees virgin conditions: no already-captured context, no
	// session refs — the rerun's whole point is regenerating output that
	// already exists.
	_, store := openTestStore(t)
	ctx := context.Background()
	seedPrompt(t, store, "sess-force-nd", "e1", 1000, "the load older button vanished")

	textA := `UI bug: "Load older" button disappears when a list is fully accepted/confirmed; expected to remain visible.`
	textB := `UI issue: 'Load older' button disappears after accepting/confirming a whole list; expected to remain visible.`
	first := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return obsResponse(req, "fact", textA), nil
	})
	if out, err := newOrchestrator(store).Run(ctx, first, orchRequest("sess-force-nd")); err != nil || out.WrittenCount != 1 {
		t.Fatalf("first run: err=%v out=%#v", err, out)
	}

	var sawContext int
	rerun := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		sawContext = len(req.AlreadyCaptured)
		return obsResponse(req, "fact", textB), nil
	})
	req := orchRequest("sess-force-nd")
	req.ForceRerun = true
	out, err := newOrchestrator(store).Run(ctx, rerun, req)
	if err != nil {
		t.Fatalf("forced rerun: %v", err)
	}
	if sawContext != 0 {
		t.Fatalf("forced rerun must not carry already-captured context, saw %d", sawContext)
	}
	if !out.Forced || out.WrittenCount != 1 || out.RejectedCount != 0 {
		t.Fatalf("forced rerun must write despite the session paraphrase: %#v", out)
	}
}

func TestOrchestratorNearDupMatchedRowDeletedMidGeneration(t *testing.T) {
	// Round-1 finding 1, case A: the observation a candidate matches is
	// soft-deleted WHILE the provider is generating. The reject decision is
	// made atomically against matched-row liveness inside the insert
	// transaction, so the candidate must be WRITTEN — a rejection standing
	// on a dead row would silently lose the content while the run's
	// manifest covers the events.
	_, store := openTestStore(t)
	ctx := context.Background()
	seedPrompt(t, store, "sess-nd-del", "e1", 1000, "the load older button vanished")

	textA := `UI bug: "Load older" button disappears when a list is fully accepted/confirmed; expected to remain visible.`
	textB := `UI issue: 'Load older' button disappears after accepting/confirming a whole list; expected to remain visible.`
	first := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return obsResponse(req, "fact", textA), nil
	})
	out1, err := newOrchestrator(store).Run(ctx, first, orchRequest("sess-nd-del"))
	if err != nil || out1.WrittenCount != 1 {
		t.Fatalf("first run: err=%v out=%#v", err, out1)
	}
	obs, _, err := store.ListObservations(ctx, db.ObservationFilter{Project: "memoire-test", SessionID: "sess-nd-del", ReviewState: "proposed"})
	if err != nil || len(obs) != 1 {
		t.Fatalf("expected the written observation: %v %d", err, len(obs))
	}
	matchedID := obs[0].ID

	seedPrompt(t, store, "sess-nd-del", "e2", 1001, "still seeing that load older disappearance")
	second := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		// Simulate the operator review racing the provider call: the matched
		// row dies before this run's write phase.
		if _, err := store.TransitionObservationReview(ctx, matchedID, "accepted", "race fixture"); err != nil {
			t.Errorf("accept: %v", err)
		}
		if _, err := store.TransitionObservationReview(ctx, matchedID, "soft_deleted", "race fixture"); err != nil {
			t.Errorf("soft-delete: %v", err)
		}
		return obsResponse(req, "fact", textB), nil
	})
	out2, err := newOrchestrator(store).Run(ctx, second, orchRequest("sess-nd-del"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if out2.WrittenCount != 1 || out2.RejectedCount != 0 {
		t.Fatalf("candidate matching a mid-generation-deleted row must be written: %#v", out2)
	}
}

func TestOrchestratorNearDupConcurrentWriteCaught(t *testing.T) {
	// Round-1 finding 1, case B (narrowed): another lane commits a
	// paraphrase WHILE the provider is generating. The gate reads the
	// session AFTER the provider returns, so the concurrent row is in the
	// ref set and this run's candidate is rejected — under the pre-fix
	// pre-provider snapshot both lanes would have admitted their copies.
	_, store := openTestStore(t)
	ctx := context.Background()
	seedPrompt(t, store, "sess-nd-race", "e1", 1000, "the load older button vanished")

	textA := `UI bug: "Load older" button disappears when a list is fully accepted/confirmed; expected to remain visible.`
	textB := `UI issue: 'Load older' button disappears after accepting/confirming a whole list; expected to remain visible.`
	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		// The "other lane": a run row plus its observation commit while this
		// run's provider call is in flight.
		foreignRun, err := store.EnsureDerivationRun(ctx, db.DerivationRunInput{
			Project: "memoire-test", SessionID: "sess-nd-race", Provider: "local-qwen",
			Model: "qwen2.5-7b", EndpointID: "hl3", DataBoundary: "local",
			DerivationVersion: "observation-derive-v1", InputHash: "race-lane-hash",
			WindowStartEpoch: 900, WindowEndEpoch: 1000, Status: "running",
		})
		if err != nil {
			t.Errorf("foreign run: %v", err)
		}
		conf := 0.9
		if _, err := store.InsertDerivedObservation(ctx, db.DerivedObservationInput{
			Project: "memoire-test", SessionID: "sess-nd-race", Kind: "fact", Text: textA,
			Confidence: &conf, ReviewState: "proposed", DerivationRunID: foreignRun.ID,
			ContentHash: "race-lane-obs-hash", SourceEventIDs: []int64{req.Events[0].ID},
			ModelDerived: true,
		}); err != nil {
			t.Errorf("foreign insert: %v", err)
		}
		return obsResponse(req, "fact", textB), nil
	})
	out, err := newOrchestrator(store).Run(ctx, provider, orchRequest("sess-nd-race"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.WrittenCount != 0 || out.RejectedCount != 1 || out.Status != StatusCompleted {
		t.Fatalf("mid-generation concurrent paraphrase must be caught by the post-provider gate read: %#v", out)
	}
}
