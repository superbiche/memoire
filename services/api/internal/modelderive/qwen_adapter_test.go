// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"memoire/services/api/internal/db"
)

// qwenTestEnv is an EnvLookup backed by a literal map so adapter tests never
// touch the process environment or a real secret. The "value" is a placeholder,
// never a live token.
func qwenTestEnv(m map[string]string) EnvLookup {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// qwenTestEvents returns a one-event input window so BuildPrompt succeeds and the
// parser can validate source_event_ids against a real window id.
func qwenTestEvents() []db.EventRecord {
	return []db.EventRecord{
		{
			ID:              101,
			Project:         "memoire-test",
			SessionID:       "sess-1",
			SourceAdapter:   "claude-code",
			EventType:       "claude_UserPromptSubmit",
			OccurredAtEpoch: 1_700_000_000,
			Payload:         json.RawMessage(`{"prompt":"use Go for the API service"}`),
		},
	}
}

func qwenTestRequest(endpointID, model string) Request {
	return Request{
		Provider:          ProviderLocalQwen,
		Model:             model,
		EndpointID:        endpointID,
		DerivationVersion: "observation-derive-v1",
		PromptVersion:     "derive-observations-v1",
		OutputSchema:      SchemaVersion,
		Window:            Window{Project: "memoire-test", SessionID: "sess-1"},
		Events:            qwenTestEvents(),
		Limits:            Limits{MaxInputEvents: 100, MaxPromptBytes: 200000, MaxOutputObservations: 20},
	}
}

// qwenChatHandler returns an httptest handler that answers /v1/models for the
// availability probe and /v1/chat/completions with the supplied content.
func qwenChatHandler(t *testing.T, content string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
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
	}
}

func newQwenAdapterForServer(t *testing.T, srv *httptest.Server, env map[string]string) *QwenAdapter {
	t.Helper()
	cfg := QwenConfig{
		Endpoints: []QwenEndpoint{
			{EndpointID: "hl3", BaseURL: srv.URL, APIKeyEnv: "HL3_QWEN_API_KEY", Models: []string{"qwen2.5-7b"}},
		},
		Timeout:        2 * time.Second,
		MaxPromptBytes: 200000,
	}
	a, err := NewQwenAdapter(cfg, srv.Client(), qwenTestEnv(env))
	if err != nil {
		t.Fatalf("NewQwenAdapter: %v", err)
	}
	return a
}

func TestQwenAdapterIdentity(t *testing.T) {
	cfg := QwenConfig{Endpoints: []QwenEndpoint{{EndpointID: "hl3", BaseURL: "http://x", Models: []string{"m"}}}}
	a, err := NewQwenAdapter(cfg, http.DefaultClient, qwenTestEnv(nil))
	if err != nil {
		t.Fatalf("NewQwenAdapter: %v", err)
	}
	if a.Name() != "local-qwen" {
		t.Fatalf("Name()=%q want local-qwen", a.Name())
	}
	if a.DataBoundary() != "local" {
		t.Fatalf("DataBoundary()=%q want local", a.DataBoundary())
	}
}

func TestQwenAdapterRefusesMissingConfig(t *testing.T) {
	// No endpoints at all.
	if _, err := NewQwenAdapter(QwenConfig{}, http.DefaultClient, qwenTestEnv(nil)); err == nil {
		t.Fatalf("expected error for empty endpoint list")
	}
	// Endpoint missing base url.
	if _, err := NewQwenAdapter(QwenConfig{Endpoints: []QwenEndpoint{{EndpointID: "hl3", Models: []string{"m"}}}}, http.DefaultClient, qwenTestEnv(nil)); err == nil {
		t.Fatalf("expected error for missing base_url")
	}
	// Endpoint missing model allowlist.
	if _, err := NewQwenAdapter(QwenConfig{Endpoints: []QwenEndpoint{{EndpointID: "hl3", BaseURL: "http://x"}}}, http.DefaultClient, qwenTestEnv(nil)); err == nil {
		t.Fatalf("expected error for empty model allowlist")
	}
	// Endpoint missing id.
	if _, err := NewQwenAdapter(QwenConfig{Endpoints: []QwenEndpoint{{BaseURL: "http://x", Models: []string{"m"}}}}, http.DefaultClient, qwenTestEnv(nil)); err == nil {
		t.Fatalf("expected error for missing endpoint_id")
	}
}

func TestQwenAdapterRequiresEndpointID(t *testing.T) {
	srv := httptest.NewServer(qwenChatHandler(t, ""))
	defer srv.Close()
	a := newQwenAdapterForServer(t, srv, map[string]string{"HL3_QWEN_API_KEY": "placeholder"})

	req := qwenTestRequest("", "qwen2.5-7b")
	if _, err := a.Generate(context.Background(), req); err == nil {
		t.Fatalf("expected error when endpoint_id is empty")
	} else if !strings.Contains(err.Error(), "endpoint_id") {
		t.Fatalf("error %q should mention endpoint_id", err)
	}
}

func TestQwenAdapterRequiresModel(t *testing.T) {
	srv := httptest.NewServer(qwenChatHandler(t, ""))
	defer srv.Close()
	a := newQwenAdapterForServer(t, srv, map[string]string{"HL3_QWEN_API_KEY": "placeholder"})

	req := qwenTestRequest("hl3", "")
	if _, err := a.Generate(context.Background(), req); err == nil {
		t.Fatalf("expected error when model is empty")
	} else if !strings.Contains(err.Error(), "model") {
		t.Fatalf("error %q should mention model", err)
	}
}

func TestQwenAdapterAllowlist(t *testing.T) {
	srv := httptest.NewServer(qwenChatHandler(t, ""))
	defer srv.Close()
	a := newQwenAdapterForServer(t, srv, map[string]string{"HL3_QWEN_API_KEY": "placeholder"})

	// Endpoint not in allowlist.
	if _, err := a.Generate(context.Background(), qwenTestRequest("hl-bogus", "qwen2.5-7b")); err == nil {
		t.Fatalf("expected error for non-allowlisted endpoint")
	} else if !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("error %q should mention allowlist", err)
	}

	// Model not allowlisted for the endpoint.
	if _, err := a.Generate(context.Background(), qwenTestRequest("hl3", "gpt-4o")); err == nil {
		t.Fatalf("expected error for non-allowlisted model")
	} else if !strings.Contains(err.Error(), "allowlisted") {
		t.Fatalf("error %q should mention allowlisted model", err)
	}
}

func TestQwenAdapterAvailabilityCheck(t *testing.T) {
	// /v1/models returns 503 -> availability check fails, Generate must not call
	// the chat endpoint.
	chatCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/v1/chat/completions":
			chatCalled = true
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	a := newQwenAdapterForServer(t, srv, map[string]string{"HL3_QWEN_API_KEY": "placeholder"})

	_, err := a.Generate(context.Background(), qwenTestRequest("hl3", "qwen2.5-7b"))
	if err == nil {
		t.Fatalf("expected availability error")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("error %q should mention unavailable", err)
	}
	if chatCalled {
		t.Fatalf("chat endpoint must not be called when availability check fails")
	}
}

func TestQwenAdapterModelPresenceEnforced(t *testing.T) {
	// D69/D79: a live endpoint that does not serve the requested model fails the
	// availability check; Generate must not call the chat endpoint.
	chatCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"some-other-model"}]}`))
		case "/v1/chat/completions":
			chatCalled = true
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	a := newQwenAdapterForServer(t, srv, map[string]string{"HL3_QWEN_API_KEY": "placeholder"})

	_, err := a.Generate(context.Background(), qwenTestRequest("hl3", "qwen2.5-7b"))
	if err == nil {
		t.Fatalf("expected model-presence error")
	}
	if !strings.Contains(err.Error(), "not served") {
		t.Fatalf("error %q should mention the model is not served", err)
	}
	if chatCalled {
		t.Fatalf("chat endpoint must not be called when the model is not served")
	}
}

func TestQwenAdapterModelListUnparseableFails(t *testing.T) {
	// The endpoint contract is OpenAI-compatible; a 200 probe with a
	// non-JSON body is a failure, not a pass.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("plain text, not a model list"))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	a := newQwenAdapterForServer(t, srv, map[string]string{"HL3_QWEN_API_KEY": "placeholder"})

	if _, err := a.Generate(context.Background(), qwenTestRequest("hl3", "qwen2.5-7b")); err == nil {
		t.Fatalf("expected error for unparseable model list")
	}
}

func TestQwenAdapterTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen2.5-7b"}]}`))
			return
		}
		// Block on chat completions until the request context is cancelled by the
		// client timeout, with a server-side ceiling so Close() never hangs.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	a := newQwenAdapterForServer(t, srv, map[string]string{"HL3_QWEN_API_KEY": "placeholder"})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := a.Generate(ctx, qwenTestRequest("hl3", "qwen2.5-7b"))
	if err == nil {
		t.Fatalf("expected timeout error")
	}
}

func TestQwenAdapterErrorMapping(t *testing.T) {
	t.Run("http 500 maps to error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"data":[{"id":"qwen2.5-7b"}]}`))
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		a := newQwenAdapterForServer(t, srv, map[string]string{"HL3_QWEN_API_KEY": "placeholder"})
		if _, err := a.Generate(context.Background(), qwenTestRequest("hl3", "qwen2.5-7b")); err == nil {
			t.Fatalf("expected error for HTTP 500")
		}
	})

	t.Run("429 maps to error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"data":[{"id":"qwen2.5-7b"}]}`))
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()
		a := newQwenAdapterForServer(t, srv, map[string]string{"HL3_QWEN_API_KEY": "placeholder"})
		if _, err := a.Generate(context.Background(), qwenTestRequest("hl3", "qwen2.5-7b")); err == nil {
			t.Fatalf("expected error for HTTP 429 rate refusal")
		}
	})

	t.Run("content_filter maps to refusal not error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"data":[{"id":"qwen2.5-7b"}]}`))
				return
			}
			resp := map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"content": ""}, "finish_reason": "content_filter"}},
				"usage":   map[string]any{"total_tokens": 10},
			}
			_ = json.NewEncoder(w).Encode(resp)
		}))
		defer srv.Close()
		a := newQwenAdapterForServer(t, srv, map[string]string{"HL3_QWEN_API_KEY": "placeholder"})
		out, err := a.Generate(context.Background(), qwenTestRequest("hl3", "qwen2.5-7b"))
		if err != nil {
			t.Fatalf("refusal should not be a Go error: %v", err)
		}
		if out.Refusal == "" {
			t.Fatalf("expected refusal to be surfaced")
		}
		if len(out.RawResponse) != 0 {
			t.Fatalf("refusal must carry no parseable output")
		}
	})
}

func TestQwenAdapterDataBoundaryMismatch(t *testing.T) {
	cfg := QwenConfig{Endpoints: []QwenEndpoint{{EndpointID: "hl3", BaseURL: "http://x", Models: []string{"m"}}}}
	a, err := NewQwenAdapter(cfg, http.DefaultClient, qwenTestEnv(nil))
	if err != nil {
		t.Fatalf("NewQwenAdapter: %v", err)
	}
	if err := a.CheckDataBoundary("external"); err == nil {
		t.Fatalf("expected error for local-qwen declaring external boundary")
	}
	if err := a.CheckDataBoundary("local"); err != nil {
		t.Fatalf("local boundary should be accepted: %v", err)
	}

	// A request whose provider is not local-qwen is refused by Generate.
	srv := httptest.NewServer(qwenChatHandler(t, ""))
	defer srv.Close()
	a2 := newQwenAdapterForServer(t, srv, map[string]string{"HL3_QWEN_API_KEY": "placeholder"})
	req := qwenTestRequest("hl3", "qwen2.5-7b")
	req.Provider = "openai"
	if _, err := a2.Generate(context.Background(), req); err == nil {
		t.Fatalf("expected error when request provider != local-qwen")
	}
}

func TestQwenAdapterRequiresConfiguredSecret(t *testing.T) {
	// Endpoint configured to need HL3_QWEN_API_KEY but env lookup returns unset:
	// the adapter must fail loudly instead of calling unauthenticated.
	srv := httptest.NewServer(qwenChatHandler(t, ""))
	defer srv.Close()
	a := newQwenAdapterForServer(t, srv, map[string]string{}) // env empty
	if _, err := a.Generate(context.Background(), qwenTestRequest("hl3", "qwen2.5-7b")); err == nil {
		t.Fatalf("expected error when required secret env is unset")
	} else if !strings.Contains(err.Error(), "HL3_QWEN_API_KEY") {
		t.Fatalf("error %q should name the missing env var", err)
	}
}

func TestQwenAdapterSuccessProducesParseableOutput(t *testing.T) {
	// Drives the adapter end to end against an HTTP fake and confirms the raw
	// model content round-trips through the strict parser into a proposed
	// observation. No real network call.
	modelJSON := fmt.Sprintf(`{"schema_version":"1","observations":[{"kind":"decision","text":"use Go for the API service","confidence":0.86,"source_event_ids":[%d],"rationale":"explicit"}]}`, qwenTestEvents()[0].ID)
	srv := httptest.NewServer(qwenChatHandler(t, modelJSON))
	defer srv.Close()
	a := newQwenAdapterForServer(t, srv, map[string]string{"HL3_QWEN_API_KEY": "placeholder"})

	req := qwenTestRequest("hl3", "qwen2.5-7b")
	out, err := a.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out.Usage.TotalTokens != 150 {
		t.Fatalf("usage.total_tokens=%d want 150", out.Usage.TotalTokens)
	}
	parsed, err := ParseResponse(out.RawResponse, req)
	if err != nil {
		t.Fatalf("ParseResponse on adapter output: %v", err)
	}
	if len(parsed.Observations) != 1 {
		t.Fatalf("expected 1 proposed observation, got %d", len(parsed.Observations))
	}
	if parsed.Observations[0].Kind != "decision" {
		t.Fatalf("kind=%q want decision", parsed.Observations[0].Kind)
	}
}

func TestQwenAdapterOrchestratorIntegration(t *testing.T) {
	// The adapter satisfies Provider and drives the real orchestrator end to end:
	// availability probe + generate via HTTP fake -> proposed observation written
	// with data_boundary=local. This proves the adapter maps cleanly onto the
	// orchestrator's failure/success states (Slice 4 contract).
	_, store := openTestStore(t)
	sessionID := "qwen-orch-1"
	eventID := seedPrompt(t, store, sessionID, "qwen-src-1", 1_700_000_100, "use Go for the API service")

	modelJSON := fmt.Sprintf(`{"schema_version":"1","observations":[{"kind":"decision","text":"use Go for the API service","confidence":0.86,"source_event_ids":[%d],"rationale":"explicit"}]}`, eventID)
	srv := httptest.NewServer(qwenChatHandler(t, modelJSON))
	defer srv.Close()
	a := newQwenAdapterForServer(t, srv, map[string]string{"HL3_QWEN_API_KEY": "placeholder"})

	orch := newOrchestrator(store)
	req := orchRequest(sessionID)
	out, err := orch.Run(context.Background(), a, req)
	if err != nil {
		t.Fatalf("orchestrator Run: %v", err)
	}
	if out.Status != StatusCompleted {
		t.Fatalf("status=%q want completed", out.Status)
	}
	if out.WrittenCount != 1 {
		t.Fatalf("written=%d want 1", out.WrittenCount)
	}

	runs, err := store.ListDerivationRuns(context.Background(), db.DerivationRunFilter{Project: "memoire-test"})
	if err != nil {
		t.Fatalf("ListDerivationRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 derivation run, got %d", len(runs))
	}
	if runs[0].DataBoundary != "local" {
		t.Fatalf("data_boundary=%q want local", runs[0].DataBoundary)
	}
	if runs[0].EndpointID != "hl3" {
		t.Fatalf("endpoint_id=%q want hl3", runs[0].EndpointID)
	}
}

// TestQwenAdapterSendsMaxTokensCap pins that every generation request carries
// a max_tokens cap — the guard against a small local model rambling unboundedly
// on degenerate input (inbox 2026-07-10, the a7312b73 hang).
func TestQwenAdapterSendsMaxTokensCap(t *testing.T) {
	captured := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen2.5-7b"}]}`))
		case "/v1/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			captured <- body
			qwenChatHandler(t, `{"schema_version":"1","observations":[]}`)(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Run("default cap applies when unconfigured", func(t *testing.T) {
		a := newQwenAdapterForServer(t, srv, map[string]string{"HL3_QWEN_API_KEY": "placeholder"})
		if _, err := a.Generate(context.Background(), qwenTestRequest("hl3", "qwen2.5-7b")); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		body := <-captured
		if got := body["max_tokens"]; got != float64(defaultMaxCompletionTokens) {
			t.Fatalf("max_tokens = %v, want default %d", got, defaultMaxCompletionTokens)
		}
		// Pin the ruled value (2026-07-11): a thinking model that burns a low
		// default entirely on reasoning returns empty content — a silent
		// yield-killer for unattended campaigns.
		if defaultMaxCompletionTokens != 16384 {
			t.Fatalf("defaultMaxCompletionTokens = %d, want the ruled 16384", defaultMaxCompletionTokens)
		}
	})

	t.Run("configured cap wins", func(t *testing.T) {
		cfg := QwenConfig{
			Endpoints: []QwenEndpoint{
				{EndpointID: "hl3", BaseURL: srv.URL, APIKeyEnv: "HL3_QWEN_API_KEY", Models: []string{"qwen2.5-7b"}},
			},
			Timeout:             2 * time.Second,
			MaxCompletionTokens: 1234,
		}
		a, err := NewQwenAdapter(cfg, srv.Client(), qwenTestEnv(map[string]string{"HL3_QWEN_API_KEY": "placeholder"}))
		if err != nil {
			t.Fatalf("NewQwenAdapter: %v", err)
		}
		if _, err := a.Generate(context.Background(), qwenTestRequest("hl3", "qwen2.5-7b")); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		body := <-captured
		if got := body["max_tokens"]; got != float64(1234) {
			t.Fatalf("max_tokens = %v, want 1234", got)
		}
	})
}
