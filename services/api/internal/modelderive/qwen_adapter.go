// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProviderLocalQwen is the provider id for the first runnable local model
// backend. It is the provider family name, never a host-specific id; the host is
// recorded separately as endpoint_id (docs/MODEL-PROVIDER.md "Provider
// Selection").
const ProviderLocalQwen = "local-qwen"

// DataBoundaryLocal is the data boundary local-qwen always declares
// (docs/MODEL-PROVIDER.md "Data Boundary Discipline").
const DataBoundaryLocal = "local"

// QwenEndpoint is one allowlisted local-Qwen host identity. BaseURL is the
// OpenAI-compatible base (the adapter appends /v1/chat/completions and
// /v1/models). APIKeyEnv names the environment variable carrying the bearer
// token; the value is never logged and never stored in run metadata.
type QwenEndpoint struct {
	// EndpointID is the audit/inspection identity such as "hl3" or "hl5".
	EndpointID string
	// BaseURL is the OpenAI-compatible server base, e.g. http://gpu-host.internal:8000.
	BaseURL string
	// APIKeyEnv names the env var holding the bearer token. Empty means the
	// endpoint needs no auth.
	APIKeyEnv string
	// Models is the per-endpoint model allowlist. A run must name a model in this
	// list (docs/MODEL-PROVIDER.md "Cost Discipline": configured endpoint/model
	// allowlist).
	Models []string
}

// QwenConfig configures a QwenAdapter: the allowlisted endpoints, the per-call
// timeout, and the prompt/output limits enforced before any network call.
type QwenConfig struct {
	// Endpoints is the endpoint allowlist keyed by EndpointID.
	Endpoints []QwenEndpoint
	// Timeout bounds availability check and generation HTTP calls. Zero falls
	// back to defaultQwenTimeout. The orchestrator also wraps Generate in its own
	// timeout; this is the adapter-internal HTTP client timeout.
	Timeout time.Duration
	// MaxPromptBytes is the hard prompt-size gate. Zero means rely on the
	// per-request Limits only.
	MaxPromptBytes int
	// MaxOutputObservations is recorded as an instruction-side cap; the parser
	// enforces the schema, this is a request-shaping hint and a guard against a
	// runaway local job.
	MaxOutputObservations int
	// MaxCompletionTokens caps the model's completion via the request's
	// max_tokens field. Zero falls back to defaultMaxCompletionTokens; the cap
	// is always sent — degenerate input can make a small local model generate
	// unboundedly until the call deadline (inbox 2026-07-10: a ~270-char window
	// hung HL3 to context-deadline twice).
	MaxCompletionTokens int
	// ReasoningEffort steers a thinking model's reasoning budget through the
	// server's chat template (chat_template_kwargs.reasoning_effort). Empty
	// omits the field entirely, leaving the body byte-identical to a request
	// that never knew the knob — the endpoint's own default then governs. A
	// template that does not know the kwarg ignores it.
	ReasoningEffort string
}

// defaultQwenTimeout bounds adapter HTTP calls when QwenConfig.Timeout is zero.
const defaultQwenTimeout = 30 * time.Second

// defaultMaxCompletionTokens bounds the completion when
// QwenConfig.MaxCompletionTokens is zero. Raised 4096 → 16384 (operator
// ruling 2026-07-11): a thinking model can spend its entire budget on
// reasoning_content and return empty content — a silent yield-killer for
// unattended runs — and 16384 is empirically sufficient on the exact
// workload (the turn-ingest e2e window finished in ~6.5k) while still
// cutting off a runaway ramble. -max-completion-tokens still overrides.
const defaultMaxCompletionTokens = 16384

// HTTPDoer is the minimal HTTP surface the adapter needs. *http.Client satisfies
// it; tests substitute a fake driven by httptest so no real network call is made.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// EnvLookup reads an environment variable. Injected so tests never depend on the
// process environment and never have to set a real secret.
type EnvLookup func(key string) (string, bool)

// QwenAdapter is the local-qwen Provider implementation. It calls an
// OpenAI-compatible chat-completions endpoint, performs an availability check
// before the run, enforces an endpoint/model allowlist, and maps transport,
// HTTP, and content errors into orchestrator failure states.
//
// QwenAdapter never logs or stores the bearer token. It records endpoint_id and
// model in run metadata via the orchestrator; the host URL is not leaked into
// observation rows.
type QwenAdapter struct {
	cfg    QwenConfig
	client HTTPDoer
	env    EnvLookup
}

// NewQwenAdapter builds a local-qwen adapter. It validates configuration up
// front so misconfiguration fails before any run, not mid-call. At least one
// endpoint with a non-empty EndpointID, BaseURL, and a non-empty model allowlist
// is required.
func NewQwenAdapter(cfg QwenConfig, client HTTPDoer, env EnvLookup) (*QwenAdapter, error) {
	if len(cfg.Endpoints) == 0 {
		return nil, errors.New("modelderive: local-qwen adapter requires at least one configured endpoint")
	}
	seen := map[string]bool{}
	for _, ep := range cfg.Endpoints {
		if strings.TrimSpace(ep.EndpointID) == "" {
			return nil, errors.New("modelderive: local-qwen endpoint requires a non-empty endpoint_id")
		}
		if seen[ep.EndpointID] {
			return nil, fmt.Errorf("modelderive: duplicate local-qwen endpoint_id %q", ep.EndpointID)
		}
		seen[ep.EndpointID] = true
		if strings.TrimSpace(ep.BaseURL) == "" {
			return nil, fmt.Errorf("modelderive: local-qwen endpoint %q requires a base_url", ep.EndpointID)
		}
		if len(ep.Models) == 0 {
			return nil, fmt.Errorf("modelderive: local-qwen endpoint %q requires a non-empty model allowlist", ep.EndpointID)
		}
	}
	if client == nil {
		client = &http.Client{Timeout: cfgTimeout(cfg)}
	}
	if env == nil {
		env = func(string) (string, bool) { return "", false }
	}
	return &QwenAdapter{cfg: cfg, client: client, env: env}, nil
}

func cfgTimeout(cfg QwenConfig) time.Duration {
	if cfg.Timeout > 0 {
		return cfg.Timeout
	}
	return defaultQwenTimeout
}

// Name implements Provider.
func (a *QwenAdapter) Name() string { return ProviderLocalQwen }

// DataBoundary implements Provider.
func (a *QwenAdapter) DataBoundary() string { return DataBoundaryLocal }

// lookupEndpoint resolves an allowlisted endpoint by id.
func (a *QwenAdapter) lookupEndpoint(id string) (QwenEndpoint, bool) {
	for _, ep := range a.cfg.Endpoints {
		if ep.EndpointID == id {
			return ep, true
		}
	}
	return QwenEndpoint{}, false
}

func endpointAllowsModel(ep QwenEndpoint, model string) bool {
	for _, m := range ep.Models {
		if m == model {
			return true
		}
	}
	return false
}

// chatCompletionRequest is the OpenAI-compatible request body. Temperature is
// pinned low for deterministic structured extraction; MaxTokens is always set
// (see QwenConfig.MaxCompletionTokens).
type chatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`
	MaxTokens   int           `json:"max_tokens"`
	// ChatTemplateKwargs is the template-level steering channel (llama.cpp /
	// vLLM pass it to the chat template). Nil is absent from the wire.
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// Generate implements Provider. It validates request configuration against the
// adapter allowlist and limits, performs an availability check, builds the
// prompt, calls the chat-completions endpoint, and returns the model's raw JSON
// content for the orchestrator to parse.
//
// Generate returns a non-nil error for any failure the orchestrator must record
// as a failed/partial run: missing config, allowlist violation, data-boundary
// mismatch, unavailability, transport error, non-2xx HTTP, or empty content.
// Refusals are surfaced via ProviderResponse.Refusal, not as an error.
func (a *QwenAdapter) Generate(ctx context.Context, req Request) (ProviderResponse, error) {
	ep, err := a.validateRequest(req)
	if err != nil {
		return ProviderResponse{}, err
	}

	// Availability check before the run (docs/MODEL-PROVIDER.md "Cost
	// Discipline": require local endpoint/model availability check; model
	// presence enforced per D69/D79).
	if err := a.availabilityCheck(ctx, ep, req.Model); err != nil {
		return ProviderResponse{}, fmt.Errorf("modelderive: local-qwen endpoint %q unavailable: %w", ep.EndpointID, err)
	}

	prompt, _, err := BuildPrompt(req)
	if err != nil {
		return ProviderResponse{}, err
	}
	if a.cfg.MaxPromptBytes > 0 && len(prompt) > a.cfg.MaxPromptBytes {
		return ProviderResponse{}, fmt.Errorf("modelderive: prompt %d bytes exceeds adapter max_prompt_bytes %d", len(prompt), a.cfg.MaxPromptBytes)
	}

	return a.send(ctx, ep, req.Model, prompt)
}

// Chat is the adapter's PROMPT-IN surface: the same endpoint allowlist,
// availability probe, bearer-by-name auth, and HTTP/refusal mapping as
// Generate, for a caller that builds its own prompt. The triage judge
// (triage lane v1, R-C) is that caller — a judge is not a derivation, so it
// has no derivation Request to hand Generate, but memoire:D137 binds it to
// the same `data_boundary=local` registry and memoire:D55/D58 to the same
// name-not-value secret seam, which is exactly what reusing this adapter
// buys instead of a second HTTP client with a second key path.
func (a *QwenAdapter) Chat(ctx context.Context, endpointID, model, prompt string) (ProviderResponse, error) {
	if strings.TrimSpace(endpointID) == "" {
		return ProviderResponse{}, errors.New("modelderive: chat requires an explicit endpoint_id")
	}
	if strings.TrimSpace(model) == "" {
		return ProviderResponse{}, errors.New("modelderive: chat requires an explicit model")
	}
	ep, ok := a.lookupEndpoint(endpointID)
	if !ok {
		return ProviderResponse{}, fmt.Errorf("modelderive: endpoint_id %q is not in the local-qwen allowlist", endpointID)
	}
	if !endpointAllowsModel(ep, model) {
		return ProviderResponse{}, fmt.Errorf("modelderive: model %q is not allowlisted for endpoint %q", model, ep.EndpointID)
	}
	if err := a.availabilityCheck(ctx, ep, model); err != nil {
		return ProviderResponse{}, fmt.Errorf("modelderive: local-qwen endpoint %q unavailable: %w", ep.EndpointID, err)
	}
	if a.cfg.MaxPromptBytes > 0 && len(prompt) > a.cfg.MaxPromptBytes {
		return ProviderResponse{}, fmt.Errorf("modelderive: prompt %d bytes exceeds adapter max_prompt_bytes %d", len(prompt), a.cfg.MaxPromptBytes)
	}
	return a.send(ctx, ep, model, prompt)
}

// send is the shared wire half of Generate and Chat: body, auth, transport,
// and the HTTP/refusal mapping that memoire:D57 pins.
func (a *QwenAdapter) send(ctx context.Context, ep QwenEndpoint, model, prompt string) (ProviderResponse, error) {
	maxTokens := a.cfg.MaxCompletionTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxCompletionTokens
	}
	var templateKwargs map[string]any
	if a.cfg.ReasoningEffort != "" {
		templateKwargs = map[string]any{"reasoning_effort": a.cfg.ReasoningEffort}
	}
	body, err := json.Marshal(chatCompletionRequest{
		Model:              model,
		Messages:           []chatMessage{{Role: "user", Content: prompt}},
		Temperature:        0,
		Stream:             false,
		MaxTokens:          maxTokens,
		ChatTemplateKwargs: templateKwargs,
	})
	if err != nil {
		return ProviderResponse{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL(ep.BaseURL, "/v1/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return ProviderResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := a.setAuth(httpReq, ep); err != nil {
		return ProviderResponse{}, err
	}

	resp, err := a.client.Do(httpReq)
	if err != nil {
		// Transport error (timeout, connection refused) -> failed run.
		return ProviderResponse{}, fmt.Errorf("modelderive: local-qwen generate call failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		return ProviderResponse{}, fmt.Errorf("modelderive: local-qwen endpoint %q rate/availability refusal: HTTP %d", ep.EndpointID, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ProviderResponse{}, fmt.Errorf("modelderive: local-qwen generate returned HTTP %d", resp.StatusCode)
	}

	var parsed chatCompletionResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return ProviderResponse{}, fmt.Errorf("modelderive: local-qwen response envelope not JSON: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return ProviderResponse{}, errors.New("modelderive: local-qwen response had no choices")
	}

	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	usage := Usage{
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
		TotalTokens:      parsed.Usage.TotalTokens,
	}

	// A finish_reason of content_filter (or empty content) is a model refusal:
	// surface it as Refusal so the orchestrator records failed/partial rather
	// than treating it as a transport failure.
	if parsed.Choices[0].FinishReason == "content_filter" {
		return ProviderResponse{Usage: usage, Refusal: "content_filter"}, nil
	}
	if content == "" {
		return ProviderResponse{Usage: usage, Refusal: "empty model content"}, nil
	}

	return ProviderResponse{
		RawResponse: json.RawMessage(content),
		Usage:       usage,
	}, nil
}

// validateRequest enforces required configuration, the endpoint/model allowlist,
// and provider/data-boundary discipline. It returns the resolved endpoint.
func (a *QwenAdapter) validateRequest(req Request) (QwenEndpoint, error) {
	if req.Provider != ProviderLocalQwen {
		return QwenEndpoint{}, fmt.Errorf("modelderive: local-qwen adapter received request for provider %q", req.Provider)
	}
	// Data-boundary discipline: a local provider must never run an external
	// boundary request (docs/MODEL-PROVIDER.md "Data Boundary Discipline").
	if req.OutputSchema != "" && req.OutputSchema != SchemaVersion {
		return QwenEndpoint{}, fmt.Errorf("modelderive: unsupported output schema %q", req.OutputSchema)
	}
	if strings.TrimSpace(req.EndpointID) == "" {
		return QwenEndpoint{}, errors.New("modelderive: local-qwen requires an explicit endpoint_id")
	}
	if strings.TrimSpace(req.Model) == "" {
		return QwenEndpoint{}, errors.New("modelderive: local-qwen requires an explicit model")
	}
	ep, ok := a.lookupEndpoint(req.EndpointID)
	if !ok {
		return QwenEndpoint{}, fmt.Errorf("modelderive: endpoint_id %q is not in the local-qwen allowlist", req.EndpointID)
	}
	if !endpointAllowsModel(ep, req.Model) {
		return QwenEndpoint{}, fmt.Errorf("modelderive: model %q is not allowlisted for endpoint %q", req.Model, ep.EndpointID)
	}
	return ep, nil
}

// CheckDataBoundary lets a caller (orchestrator/CLI) fail early on a
// provider/data-boundary mismatch before constructing a request. It mirrors the
// store-side discipline: local-qwen must declare a local boundary.
func (a *QwenAdapter) CheckDataBoundary(boundary string) error {
	return validateDataBoundary(ProviderLocalQwen, boundary)
}

// availabilityCheck probes the endpoint's /v1/models before the run. A non-2xx
// or transport error means the endpoint is unavailable, and a served-model list
// that does not include the requested model fails the probe too (D69/D79:
// model-presence enforcement, deferred from Slices 5/6). The body must be
// OpenAI-shaped ({"data":[{"id":...}]}); an unparseable body is a failure
// because the endpoint contract is OpenAI-compatible. The orchestrator records
// any probe failure as a failed run rather than calling Generate's body.
func (a *QwenAdapter) availabilityCheck(ctx context.Context, ep QwenEndpoint, model string) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(ep.BaseURL, "/v1/models"), nil)
	if err != nil {
		return err
	}
	if err := a.setAuth(httpReq, ep); err != nil {
		return err
	}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("availability probe body read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("availability probe returned HTTP %d", resp.StatusCode)
	}
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return fmt.Errorf("availability probe body is not an OpenAI-shaped model list: %w", err)
	}
	served := make([]string, 0, len(listing.Data))
	for _, m := range listing.Data {
		if m.ID == model {
			return nil
		}
		served = append(served, m.ID)
	}
	return fmt.Errorf("model %q not served by endpoint (serving: %s)", model, strings.Join(served, ", "))
}

// setAuth attaches the bearer token from the configured env var, if any. The
// token value is never logged. A configured-but-unset env var is a hard
// configuration error so a run fails loudly instead of calling unauthenticated.
func (a *QwenAdapter) setAuth(req *http.Request, ep QwenEndpoint) error {
	if ep.APIKeyEnv == "" {
		return nil
	}
	key, ok := a.env(ep.APIKeyEnv)
	if !ok || strings.TrimSpace(key) == "" {
		return fmt.Errorf("modelderive: local-qwen endpoint %q requires env %s but it is unset", ep.EndpointID, ep.APIKeyEnv)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return nil
}

// joinURL concatenates a base URL and a path without doubling the separator.
func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + path
}
