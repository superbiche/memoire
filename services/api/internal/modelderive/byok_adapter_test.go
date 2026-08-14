// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// byokTestRequest mirrors qwenTestRequest for the byok provider: a one-event
// window so BuildPrompt succeeds; no endpoint registry to consult.
func byokTestRequest(model string) Request {
	return Request{
		Provider:          ProviderBYOK,
		Model:             model,
		EndpointID:        "byok",
		Dialect:           DialectOpenAIV1,
		DerivationVersion: "observation-derive-v1",
		PromptVersion:     "derive-observations-v1",
		OutputSchema:      SchemaVersion,
		Window:            Window{Project: "memoire-test", SessionID: "sess-1"},
		Events:            qwenTestEvents(),
		Limits:            Limits{MaxInputEvents: 100, MaxPromptBytes: 200000, MaxOutputObservations: 20},
	}
}

func byokConfig(baseURL, dialect string) BYOKConfig {
	return BYOKConfig{
		BaseURL:      baseURL,
		Model:        "byok-test-model",
		Dialect:      dialect,
		DataBoundary: "local",
	}
}

// validDeriveJSON is a minimal valid strict-parser payload grounded on the
// fixture window's event id.
const validDeriveJSON = `{"schema_version":"1","observations":[{"kind":"fact","text":"the API service is written in Go","confidence":0.9,"source_event_ids":[101]}],"unknown_candidates":[]}`

// --- constructor: closed set, required declarations ------------------------

func TestBYOKConstructorRefusesUnknownDialect(t *testing.T) {
	cfg := byokConfig("http://example.test", "grpc")
	if _, err := NewBYOKAdapter(cfg, nil, nil); err == nil || !strings.Contains(err.Error(), "closed dialect set") {
		t.Fatalf("unknown dialect must refuse naming the closed set, got %v", err)
	}
}

func TestBYOKConstructorRequiresExplicitDialect(t *testing.T) {
	cfg := byokConfig("http://example.test", "")
	if _, err := NewBYOKAdapter(cfg, nil, nil); err == nil {
		t.Fatal("empty dialect must refuse — there is no auto-detection (ruling Q1)")
	}
}

func TestBYOKConstructorRequiresDataBoundary(t *testing.T) {
	cfg := byokConfig("http://example.test", DialectOpenAIV1)
	cfg.DataBoundary = ""
	if _, err := NewBYOKAdapter(cfg, nil, nil); err == nil || !strings.Contains(err.Error(), "data boundary") {
		t.Fatalf("missing data boundary must refuse loudly (ruling Q2), got %v", err)
	}
	cfg.DataBoundary = "cloudish"
	if _, err := NewBYOKAdapter(cfg, nil, nil); err == nil {
		t.Fatal("invalid data boundary must refuse")
	}
}

func TestBYOKConstructorRefusesBothKeySources(t *testing.T) {
	cfg := byokConfig("http://example.test", DialectOpenAIV1)
	cfg.APIKeyEnv = "SOME_KEY"
	cfg.APIKeyFile = "/tmp/some-key"
	if _, err := NewBYOKAdapter(cfg, nil, nil); err == nil {
		t.Fatal("env var AND key file together must refuse")
	}
}

func TestBYOKDataBoundaryIsDeclared(t *testing.T) {
	for _, boundary := range []string{"local", "external"} {
		cfg := byokConfig("http://example.test", DialectOpenAIV1)
		cfg.DataBoundary = boundary
		cfg.APIKeyEnv = "SOME_KEY" // external requires a key source (r1 F3)
		a, err := NewBYOKAdapter(cfg, nil, nil)
		if err != nil {
			t.Fatalf("boundary %q: %v", boundary, err)
		}
		if a.DataBoundary() != boundary {
			t.Fatalf("DataBoundary() = %q, want the declared %q", a.DataBoundary(), boundary)
		}
		if err := validateDataBoundary(ProviderBYOK, boundary); err != nil {
			t.Fatalf("validateDataBoundary(byok, %q): %v", boundary, err)
		}
	}
	if err := validateDataBoundary(ProviderBYOK, "elsewhere"); err == nil {
		t.Fatal("byok with a non-legal boundary value must fail validateDataBoundary")
	}
}

// --- openai-v1 dialect ------------------------------------------------------

func TestBYOKOpenAIDialectSuccess(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "byok-test-model" {
			t.Errorf("request model = %v", body["model"])
		}
		if _, ok := body["max_tokens"].(float64); !ok {
			t.Error("max_tokens must always be sent")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": validDeriveJSON}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 120, "completion_tokens": 30, "total_tokens": 150},
		})
	}))
	defer srv.Close()

	cfg := byokConfig(srv.URL, DialectOpenAIV1)
	cfg.APIKeyEnv = "BYOK_TEST_KEY"
	a, err := NewBYOKAdapter(cfg, srv.Client(), qwenTestEnv(map[string]string{"BYOK_TEST_KEY": "placeholder-not-a-real-key"}))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Generate(context.Background(), byokTestRequest("byok-test-model"))
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("openai-v1 dialect must call /v1/chat/completions, called %s", gotPath)
	}
	if gotAuth != "Bearer placeholder-not-a-real-key" {
		t.Fatalf("openai-v1 dialect must authenticate with a bearer header, got %q", gotAuth)
	}
	if string(resp.RawResponse) != validDeriveJSON {
		t.Fatalf("raw response mismatch: %s", resp.RawResponse)
	}
	if resp.Usage.TotalTokens != 150 {
		t.Fatalf("usage not extracted: %+v", resp.Usage)
	}
	if _, err := ParseResponse(resp.RawResponse, byokTestRequest("byok-test-model")); err != nil {
		t.Fatalf("orchestrator-side parse of the returned content failed: %v", err)
	}
}

func TestBYOKOpenAIDialectRateLimitIsAvailabilityClass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	a, err := NewBYOKAdapter(byokConfig(srv.URL, DialectOpenAIV1), srv.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Generate(context.Background(), byokTestRequest("byok-test-model"))
	if err == nil || !strings.Contains(err.Error(), "rate/availability") {
		t.Fatalf("HTTP 429 must map to the rate/availability class, got %v", err)
	}
}

// --- anthropic dialect ------------------------------------------------------

func TestBYOKAnthropicDialectSuccess(t *testing.T) {
	var gotKey, gotVersion, gotBearer, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotBearer = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["max_tokens"].(float64); !ok {
			t.Error("max_tokens is REQUIRED by the messages API and must be sent")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]any{{"type": "text", "text": validDeriveJSON}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 200, "output_tokens": 40},
		})
	}))
	defer srv.Close()

	cfg := byokConfig(srv.URL, DialectAnthropic)
	cfg.APIKeyEnv = "BYOK_TEST_KEY"
	a, err := NewBYOKAdapter(cfg, srv.Client(), qwenTestEnv(map[string]string{"BYOK_TEST_KEY": "placeholder-not-a-real-key"}))
	if err != nil {
		t.Fatal(err)
	}
	req := byokTestRequest("byok-test-model")
	req.Dialect = DialectAnthropic
	resp, err := a.Generate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("anthropic dialect must call /v1/messages, called %s", gotPath)
	}
	if gotKey != "placeholder-not-a-real-key" {
		t.Fatalf("anthropic dialect must authenticate with x-api-key, got %q", gotKey)
	}
	if gotVersion != anthropicVersion {
		t.Fatalf("anthropic-version header = %q, want %q", gotVersion, anthropicVersion)
	}
	if gotBearer != "" {
		t.Fatalf("anthropic dialect must not send a bearer header, got %q", gotBearer)
	}
	if string(resp.RawResponse) != validDeriveJSON {
		t.Fatalf("raw response mismatch: %s", resp.RawResponse)
	}
	if resp.Usage.PromptTokens != 200 || resp.Usage.CompletionTokens != 40 || resp.Usage.TotalTokens != 240 {
		t.Fatalf("anthropic usage mapping wrong: %+v", resp.Usage)
	}
}

func TestBYOKAnthropicErrorTaxonomy(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		errType    string
		wantSubstr string
	}{
		{"overloaded is availability", http.StatusInternalServerError, "overloaded_error", "rate/availability"},
		{"rate limit is availability", http.StatusTooManyRequests, "rate_limit_error", "rate/availability"},
		{"authentication is named", http.StatusUnauthorized, "authentication_error", "authentication_error"},
		{"invalid request is named", http.StatusBadRequest, "invalid_request_error", "invalid_request_error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"type":  "error",
					"error": map[string]any{"type": c.errType, "message": "upstream says no"},
				})
			}))
			defer srv.Close()
			a, err := NewBYOKAdapter(byokConfig(srv.URL, DialectAnthropic), srv.Client(), nil)
			if err != nil {
				t.Fatal(err)
			}
			req := byokTestRequest("byok-test-model")
			req.Dialect = DialectAnthropic
			_, err = a.Generate(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), c.wantSubstr) {
				t.Fatalf("want error containing %q, got %v", c.wantSubstr, err)
			}
		})
	}
}

func TestBYOKAnthropicRefusalStopReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]any{},
			"stop_reason": "refusal",
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 0},
		})
	}))
	defer srv.Close()
	a, err := NewBYOKAdapter(byokConfig(srv.URL, DialectAnthropic), srv.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req := byokTestRequest("byok-test-model")
	req.Dialect = DialectAnthropic
	resp, err := a.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("a refusal is not a transport error: %v", err)
	}
	if resp.Refusal == "" {
		t.Fatal("stop_reason=refusal must surface as ProviderResponse.Refusal")
	}
}

// --- key custody ------------------------------------------------------------

func TestBYOKKeyNeverInErrors(t *testing.T) {
	// Unset env var: the error must name the VARIABLE, never a value.
	cfg := byokConfig("http://example.test", DialectOpenAIV1)
	cfg.APIKeyEnv = "BYOK_MISSING_KEY"
	a, err := NewBYOKAdapter(cfg, nil, qwenTestEnv(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Generate(context.Background(), byokTestRequest("byok-test-model"))
	if err == nil || !strings.Contains(err.Error(), "BYOK_MISSING_KEY") {
		t.Fatalf("missing key must name the env var, got %v", err)
	}

	// Transport failure with a resolved key: the key value must not leak
	// into the error string.
	const secret = "placeholder-secret-value"
	cfg2 := byokConfig("http://127.0.0.1:1", DialectOpenAIV1) // nothing listens
	cfg2.APIKeyEnv = "BYOK_TEST_KEY"
	a2, err := NewBYOKAdapter(cfg2, nil, qwenTestEnv(map[string]string{"BYOK_TEST_KEY": secret}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = a2.Generate(context.Background(), byokTestRequest("byok-test-model"))
	if err == nil {
		t.Fatal("expected transport failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("key value leaked into an error string")
	}
}

func TestBYOKKeyFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := dir + "/key"
	if err := os.WriteFile(keyPath, []byte("# comment\nplaceholder-file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": validDeriveJSON}, "finish_reason": "stop"}},
			"usage":   map[string]any{},
		})
	}))
	defer srv.Close()
	cfg := byokConfig(srv.URL, DialectOpenAIV1)
	cfg.APIKeyFile = keyPath
	a, err := NewBYOKAdapter(cfg, srv.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Generate(context.Background(), byokTestRequest("byok-test-model")); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer placeholder-file-key" {
		t.Fatalf("key file first non-comment line must be the key, got auth %q", gotAuth)
	}
}

// --- request validation -----------------------------------------------------

func TestBYOKModelMustMatchConfig(t *testing.T) {
	a, err := NewBYOKAdapter(byokConfig("http://example.test", DialectOpenAIV1), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Generate(context.Background(), byokTestRequest("some-other-model"))
	if err == nil || !strings.Contains(err.Error(), "does not match the configured byok model") {
		t.Fatalf("model mismatch must refuse, got %v", err)
	}
}

func TestBYOKTimeoutHonorsContext(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
	}))
	defer srv.Close()
	defer close(blocked)
	a, err := NewBYOKAdapter(byokConfig(srv.URL, DialectOpenAIV1), srv.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = a.Generate(ctx, byokTestRequest("byok-test-model"))
	if err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("adapter must honor ctx cancellation, got %v", err)
	}
}

// --- review r1 regressions --------------------------------------------------

// TestBYOKAnthropicErrorNeverReflectsProviderStrings pins r1 F1: the
// endpoint controls the error envelope and has already SEEN the key, so
// neither error.message nor a non-closed-set error.type may enter the Go
// error (which the orchestrator persists into run meta_json).
func TestBYOKAnthropicErrorNeverReflectsProviderStrings(t *testing.T) {
	const sentinel = "review-sentinel-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "weird_custom_type", "message": sentinel},
		})
	}))
	defer srv.Close()
	cfg := byokConfig(srv.URL, DialectAnthropic)
	cfg.APIKeyEnv = "BYOK_TEST_KEY"
	a, err := NewBYOKAdapter(cfg, srv.Client(), qwenTestEnv(map[string]string{"BYOK_TEST_KEY": sentinel}))
	if err != nil {
		t.Fatal(err)
	}
	req := byokTestRequest("byok-test-model")
	req.Dialect = DialectAnthropic
	_, err = a.Generate(context.Background(), req)
	if err == nil {
		t.Fatal("expected an error for HTTP 401")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("provider-controlled error message leaked into the Go error: %v", err)
	}
	if strings.Contains(err.Error(), "weird_custom_type") {
		t.Fatalf("non-closed-set provider error type leaked into the Go error: %v", err)
	}
}

// TestBYOKRunMetaNeverHoldsProviderErrorStrings is the end-to-end half of
// r1 F1: the failed run's PERSISTED meta_json must not carry the
// endpoint's reflected strings either.
func TestBYOKRunMetaNeverHoldsProviderErrorStrings(t *testing.T) {
	const sentinel = "review-sentinel-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "authentication_error", "message": sentinel},
		})
	}))
	defer srv.Close()
	cfg := byokConfig(srv.URL, DialectAnthropic)
	cfg.APIKeyEnv = "BYOK_TEST_KEY"
	a, err := NewBYOKAdapter(cfg, srv.Client(), qwenTestEnv(map[string]string{"BYOK_TEST_KEY": sentinel}))
	if err != nil {
		t.Fatal(err)
	}
	_, store := openTestStore(t)
	o := newOrchestrator(store)
	ctx := context.Background()
	seedPrompt(t, store, "sess-byok-meta", "e1", 1000, "Memoire decision: chose Go")

	req := byokTestRequest("byok-test-model")
	req.Dialect = DialectAnthropic
	req.Window = Window{Project: "memoire-test", SessionID: "sess-byok-meta"}
	req.Events = nil
	out, err := o.Run(ctx, a, req)
	if err != nil {
		t.Fatalf("a provider failure is a recorded run, not a Go error: %v", err)
	}
	if out.Status != "failed" {
		t.Fatalf("run status = %q, want failed", out.Status)
	}
	rec, err := store.GetDerivationRun(ctx, out.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rec.Metadata), sentinel) {
		t.Fatalf("persisted run meta_json carries the endpoint-reflected string: %s", rec.Metadata)
	}
	if !strings.Contains(string(rec.Metadata), "authentication_error") {
		t.Fatalf("closed-set error type should be preserved for diagnosis: %s", rec.Metadata)
	}
}

// TestBYOKRefusesRedirects pins r1 F2 (coverage completed per r2 F2): a
// redirecting endpoint must not be followed on EITHER dialect for EITHER
// body-preserving redirect status — a 307/308 from a declared-local
// endpoint would otherwise resend the full prompt body (and, on the
// anthropic dialect, the x-api-key header, which Go's cross-origin
// sensitive-header stripping does not cover) to an arbitrary target.
// nil client: each case must exercise the adapter's own production
// client construction.
func TestBYOKRefusesRedirects(t *testing.T) {
	for _, c := range []struct {
		dialect string
		status  int
	}{
		{DialectOpenAIV1, http.StatusTemporaryRedirect},
		{DialectOpenAIV1, http.StatusPermanentRedirect},
		{DialectAnthropic, http.StatusTemporaryRedirect},
		{DialectAnthropic, http.StatusPermanentRedirect},
	} {
		t.Run(fmt.Sprintf("%s-%d", c.dialect, c.status), func(t *testing.T) {
			var capturedHits int
			var capturedKey string
			capture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedHits++
				capturedKey = r.Header.Get("x-api-key")
				w.WriteHeader(http.StatusOK)
			}))
			defer capture.Close()
			redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, capture.URL+r.URL.Path, c.status)
			}))
			defer redirecting.Close()

			cfg := byokConfig(redirecting.URL, c.dialect)
			cfg.APIKeyEnv = "BYOK_TEST_KEY"
			a, err := NewBYOKAdapter(cfg, nil, qwenTestEnv(map[string]string{"BYOK_TEST_KEY": "placeholder-not-a-real-key"}))
			if err != nil {
				t.Fatal(err)
			}
			req := byokTestRequest("byok-test-model")
			req.Dialect = c.dialect
			_, err = a.Generate(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), "redirect") {
				t.Fatalf("a redirecting endpoint must refuse naming the redirect, got %v", err)
			}
			if capturedHits != 0 {
				t.Fatalf("the redirect target was called %d times; anthropic key crossed origins = %t", capturedHits, capturedKey != "")
			}
		})
	}
}

// TestBYOKExternalRequiresKeySource pins r1 F3: the keyless carve-out is
// for LOCAL endpoints only — an external declaration with no key source
// must refuse at construction.
func TestBYOKExternalRequiresKeySource(t *testing.T) {
	cfg := byokConfig("https://api.example.test", DialectOpenAIV1)
	cfg.DataBoundary = "external"
	if _, err := NewBYOKAdapter(cfg, nil, nil); err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("external boundary without a key source must refuse, got %v", err)
	}
	cfg.APIKeyEnv = "SOME_KEY"
	if _, err := NewBYOKAdapter(cfg, nil, nil); err != nil {
		t.Fatalf("external with a key source must construct: %v", err)
	}
}

// TestBYOKRequestTransportIdentityMustMatchConfig pins r1 F4: the request's
// recorded dialect and endpoint label are what the orchestrator persists —
// they must equal what the adapter actually does.
func TestBYOKRequestTransportIdentityMustMatchConfig(t *testing.T) {
	a, err := NewBYOKAdapter(byokConfig("http://example.test", DialectAnthropic), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := byokTestRequest("byok-test-model") // carries Dialect openai-v1
	_, err = a.Generate(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "dialect") {
		t.Fatalf("a request whose dialect differs from the adapter's must refuse, got %v", err)
	}
	req.Dialect = DialectAnthropic
	req.EndpointID = "some-other-label"
	_, err = a.Generate(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("a request whose endpoint label differs from the adapter's must refuse, got %v", err)
	}
}
