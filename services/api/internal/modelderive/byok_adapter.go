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
	"os"
	"strings"
	"time"
)

// ProviderBYOK is the bring-your-own-endpoint provider for standalone
// derivation (memoire:D232 lane-matrix cell; docs/BYOK-DERIVE-DESIGN.md).
// The adapter knows one URL, one key, one model — config, nothing else:
// no endpoint registry, no prewarm, no scheduling (those stay Reveille
// Conductor's; only the DIALECT knowledge is ported, provenance below).
const ProviderBYOK = "byok"

// The closed dialect set (operator-ratified 2026-08-04, handoff §3; same
// family as the D175 closed gesture set): exactly these two, extensible by
// operator ruling only, never by PR.
const (
	// DialectOpenAIV1 speaks POST {base}/v1/chat/completions with a bearer
	// key — the same wire shape the in-repo local-qwen adapter has spoken
	// in production since Slice 6 (the request/response structs are shared;
	// see qwen_adapter.go).
	DialectOpenAIV1 = "openai-v1"
	// DialectAnthropic speaks POST {base}/v1/messages with x-api-key +
	// anthropic-version headers. Shapes and error taxonomy ported from
	// Reveille Conductor's production Anthropic surface (internal
	// port-not-adopt; quarried files: reveille server/conductor/http.ts
	// [anthropicError envelope, header discipline], adapters.ts [upstream
	// request shaping], routes.ts [error-type mapping]).
	DialectAnthropic = "anthropic"
)

// anthropicVersion is the API version header value sent on every Anthropic
// dialect call. Pinned, not configurable: the parser below understands
// exactly this version's response envelope.
const anthropicVersion = "2023-06-01"

// BYOKConfig configures a BYOKAdapter. Exactly one endpoint; the key
// arrives by env-var NAME or by file path, never by value (D73 posture:
// never in argv, never in repo-riding config, never logged).
type BYOKConfig struct {
	// BaseURL is the endpoint base, e.g. https://api.anthropic.com or
	// http://localhost:8080. The adapter appends the dialect's path.
	BaseURL string
	// Model is the single configured model name. A request must name it.
	Model string
	// Dialect selects the wire protocol: DialectOpenAIV1 or
	// DialectAnthropic. Required and EXPLICIT — there is no auto-detection
	// (operator ruling 2026-08-05 Q1: nothing about a bare URL is
	// unambiguous; explicit beats clever).
	Dialect string
	// DataBoundary is the user's declaration of where the endpoint lives:
	// "local" or "external". Required, no default (operator ruling
	// 2026-08-05 Q2): the same dialect reaches localhost and a SaaS API,
	// so the adapter cannot infer it. External inherits the per-run
	// opt-in discipline (docs/MODEL-PROVIDER.md "Data Boundary
	// Discipline"), enforced by the CLI before the adapter is built.
	DataBoundary string
	// APIKeyEnv names the env var holding the key. APIKeyFile points at a
	// file whose first non-empty line is the key. At most one may be set;
	// both empty means a keyless endpoint (local inference servers).
	APIKeyEnv  string
	APIKeyFile string
	// EndpointID is the audit label recorded on runs. Empty defaults to
	// "byok". Audit and inspection metadata only, excluded from run
	// idempotency (docs/MODEL-PROVIDER.md "Model Identity And Idempotency").
	EndpointID string
	// Timeout bounds the HTTP call when the adapter builds its own client.
	// Zero falls back to defaultBYOKTimeout. The orchestrator also wraps
	// Generate in its own timeout.
	Timeout time.Duration
	// MaxPromptBytes / MaxOutputObservations / MaxCompletionTokens mirror
	// QwenConfig: hard prompt gate, instruction-side cap, and the
	// always-sent completion-token ceiling.
	MaxPromptBytes        int
	MaxOutputObservations int
	MaxCompletionTokens   int
}

// defaultBYOKTimeout bounds adapter HTTP calls when BYOKConfig.Timeout is
// zero. Same rationale as defaultQwenTimeout; BYOK endpoints include SaaS
// APIs whose long-tail latency the -execute-timeout flag governs anyway.
const defaultBYOKTimeout = 60 * time.Second

// BYOKAdapter is the byok Provider implementation: one user-configured
// endpoint, one model, one of two wire dialects. It never logs or stores
// the key; error strings name only the env var or file path.
type BYOKAdapter struct {
	cfg    BYOKConfig
	client HTTPDoer
	env    EnvLookup
	// readFile is the key-file reader, injected for tests. Defaults to
	// os.ReadFile.
	readFile func(string) ([]byte, error)
}

// NewBYOKAdapter validates configuration up front so misconfiguration
// fails before any run. Every violation is an operator-facing error that
// names the field, never a key value.
func NewBYOKAdapter(cfg BYOKConfig, client HTTPDoer, env EnvLookup) (*BYOKAdapter, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("modelderive: byok requires a base URL (MEMOIRE_BYOK_BASE_URL)")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("modelderive: byok requires a model name (MEMOIRE_BYOK_MODEL)")
	}
	switch cfg.Dialect {
	case DialectOpenAIV1, DialectAnthropic:
	default:
		return nil, fmt.Errorf("modelderive: byok dialect %q is not in the closed dialect set (%s, %s — extended by operator ruling only, docs/BYOK-DERIVE-DESIGN.md)",
			cfg.Dialect, DialectOpenAIV1, DialectAnthropic)
	}
	switch cfg.DataBoundary {
	case "local", "external":
	default:
		return nil, errors.New("modelderive: byok requires an explicit data boundary, local or external (MEMOIRE_BYOK_DATA_BOUNDARY — the adapter never guesses where your endpoint lives)")
	}
	if cfg.APIKeyEnv != "" && cfg.APIKeyFile != "" {
		return nil, errors.New("modelderive: byok accepts an API key env var OR a key file, not both")
	}
	// The keyless carve-out is for LOCAL endpoints only (design §3; review
	// r1 F3): an external declaration with no key source would ship a raw
	// event window to an unauthenticated endpoint.
	if cfg.DataBoundary == "external" && cfg.APIKeyEnv == "" && cfg.APIKeyFile == "" {
		return nil, errors.New("modelderive: byok with an external data boundary requires an API key source (MEMOIRE_BYOK_API_KEY_ENV or MEMOIRE_BYOK_API_KEY_FILE); keyless is a local-endpoint carve-out only")
	}
	if client == nil {
		t := cfg.Timeout
		if t <= 0 {
			t = defaultBYOKTimeout
		}
		// Never follow redirects (review r1 F2): a 307/308 would resend the
		// full prompt body — and, on the anthropic dialect, the x-api-key
		// header, which Go's cross-origin header stripping does not cover —
		// to whatever origin the endpoint names. The adapter surfaces the
		// 3xx as a refusal instead.
		client = &http.Client{
			Timeout: t,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if env == nil {
		env = func(string) (string, bool) { return "", false }
	}
	return &BYOKAdapter{cfg: cfg, client: client, env: env, readFile: os.ReadFile}, nil
}

// Name implements Provider.
func (a *BYOKAdapter) Name() string { return ProviderBYOK }

// DataBoundary implements Provider: the user's declared boundary.
func (a *BYOKAdapter) DataBoundary() string { return a.cfg.DataBoundary }

// EndpointID is the audit label recorded on runs.
func (a *BYOKAdapter) EndpointID() string {
	if a.cfg.EndpointID != "" {
		return a.cfg.EndpointID
	}
	return ProviderBYOK
}

// apiKey resolves the configured key at call time. The value is returned
// to the caller only for header injection and never appears in any error.
func (a *BYOKAdapter) apiKey() (string, error) {
	switch {
	case a.cfg.APIKeyEnv != "":
		v, ok := a.env(a.cfg.APIKeyEnv)
		if !ok || strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("modelderive: byok requires env %s but it is unset", a.cfg.APIKeyEnv)
		}
		return strings.TrimSpace(v), nil
	case a.cfg.APIKeyFile != "":
		b, err := a.readFile(a.cfg.APIKeyFile)
		if err != nil {
			return "", fmt.Errorf("modelderive: byok key file %s unreadable: %w", a.cfg.APIKeyFile, err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				return line, nil
			}
		}
		return "", fmt.Errorf("modelderive: byok key file %s holds no key", a.cfg.APIKeyFile)
	default:
		return "", nil // keyless endpoint
	}
}

// Generate implements Provider. It validates the request against the
// configured endpoint, builds the prompt, and dispatches on the dialect.
// Error policy mirrors the qwen adapter: transport/HTTP/content failures
// are Go errors the orchestrator records as failed runs; model refusals
// surface via ProviderResponse.Refusal.
func (a *BYOKAdapter) Generate(ctx context.Context, req Request) (ProviderResponse, error) {
	if err := a.validateRequest(req); err != nil {
		return ProviderResponse{}, err
	}
	prompt, _, err := BuildPrompt(req)
	if err != nil {
		return ProviderResponse{}, err
	}
	if a.cfg.MaxPromptBytes > 0 && len(prompt) > a.cfg.MaxPromptBytes {
		return ProviderResponse{}, fmt.Errorf("modelderive: prompt %d bytes exceeds adapter max_prompt_bytes %d", len(prompt), a.cfg.MaxPromptBytes)
	}
	maxTokens := a.cfg.MaxCompletionTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxCompletionTokens
	}
	key, err := a.apiKey()
	if err != nil {
		return ProviderResponse{}, err
	}
	switch a.cfg.Dialect {
	case DialectOpenAIV1:
		return a.generateOpenAI(ctx, prompt, key, maxTokens)
	case DialectAnthropic:
		return a.generateAnthropic(ctx, prompt, key, maxTokens)
	default:
		// Unreachable: the constructor enforces the closed set.
		return ProviderResponse{}, fmt.Errorf("modelderive: byok dialect %q is not in the closed dialect set", a.cfg.Dialect)
	}
}

// validateRequest enforces provider identity, schema, and the single
// configured model. There is no allowlist to consult — one endpoint, one
// model is the ruled shape.
func (a *BYOKAdapter) validateRequest(req Request) error {
	if req.Provider != ProviderBYOK {
		return fmt.Errorf("modelderive: byok adapter received request for provider %q", req.Provider)
	}
	if req.OutputSchema != "" && req.OutputSchema != SchemaVersion {
		return fmt.Errorf("modelderive: unsupported output schema %q", req.OutputSchema)
	}
	if strings.TrimSpace(req.Model) == "" {
		return errors.New("modelderive: byok requires an explicit model")
	}
	if req.Model != a.cfg.Model {
		return fmt.Errorf("modelderive: model %q does not match the configured byok model %q", req.Model, a.cfg.Model)
	}
	// Transport identity binding (review r1 F4): the request's dialect and
	// endpoint label are what the orchestrator PERSISTS — they must equal
	// what this adapter actually does, or run provenance lies.
	if req.Dialect != a.cfg.Dialect {
		return fmt.Errorf("modelderive: request dialect %q does not match the configured byok dialect %q", req.Dialect, a.cfg.Dialect)
	}
	if req.EndpointID != a.EndpointID() {
		return fmt.Errorf("modelderive: request endpoint label %q does not match the byok endpoint label %q", req.EndpointID, a.EndpointID())
	}
	return nil
}

// generateOpenAI speaks the OpenAI-compatible chat-completions dialect.
// The request/response structs are the package-shared shapes the
// local-qwen adapter has used in production since Slice 6; refusal and
// error mapping mirror it deliberately so both OpenAI-dialect paths in
// this package answer failures the same way.
func (a *BYOKAdapter) generateOpenAI(ctx context.Context, prompt, key string, maxTokens int) (ProviderResponse, error) {
	body, err := json.Marshal(chatCompletionRequest{
		Model:       a.cfg.Model,
		Messages:    []chatMessage{{Role: "user", Content: prompt}},
		Temperature: 0,
		Stream:      false,
		MaxTokens:   maxTokens,
	})
	if err != nil {
		return ProviderResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL(a.cfg.BaseURL, "/v1/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return ProviderResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return ProviderResponse{}, fmt.Errorf("modelderive: byok generate call failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return ProviderResponse{}, fmt.Errorf("modelderive: byok endpoint answered a redirect (HTTP %d) — refusing to follow; configure the final endpoint URL", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		return ProviderResponse{}, fmt.Errorf("modelderive: byok endpoint rate/availability refusal: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ProviderResponse{}, fmt.Errorf("modelderive: byok generate returned HTTP %d", resp.StatusCode)
	}
	var parsed chatCompletionResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return ProviderResponse{}, fmt.Errorf("modelderive: byok response envelope not JSON: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return ProviderResponse{}, errors.New("modelderive: byok response had no choices")
	}
	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	usage := Usage{
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
		TotalTokens:      parsed.Usage.TotalTokens,
	}
	if parsed.Choices[0].FinishReason == "content_filter" {
		return ProviderResponse{Usage: usage, Refusal: "content_filter"}, nil
	}
	if content == "" {
		return ProviderResponse{Usage: usage, Refusal: "empty model content"}, nil
	}
	return ProviderResponse{RawResponse: json.RawMessage(content), Usage: usage}, nil
}

// anthropicMessagesRequest is the Anthropic Messages API request body.
// max_tokens is REQUIRED by that API (unlike chat-completions where it is
// merely always sent here). Shape ported from Reveille Conductor's
// production Anthropic surface (adapters.ts upstream shaping).
type anthropicMessagesRequest struct {
	Model       string        `json:"model"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
	Messages    []chatMessage `json:"messages"`
}

// anthropicMessagesResponse is the Messages API success envelope: content
// blocks, a stop reason, and input/output token usage.
type anthropicMessagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// anthropicErrorEnvelope is the Anthropic error body:
// {"type":"error","error":{"type":...,"message":...}} (ported from
// Conductor http.ts anthropicError). ONLY the error type is consumed, and
// only through the closed taxonomy below — the endpoint controls both
// fields and has already seen the key, so a reflected message (or an
// arbitrary type string) must never enter a Go error the orchestrator
// persists into run meta_json (review r1 F1).
type anthropicErrorEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// anthropicKnownErrorTypes is the closed taxonomy an error string may name
// (Conductor routes.ts mapping plus the documented API error set). An
// unknown type is reported as unrecognized, never echoed.
var anthropicKnownErrorTypes = map[string]bool{
	"invalid_request_error": true,
	"authentication_error":  true,
	"permission_error":      true,
	"not_found_error":       true,
	"request_too_large":     true,
	"rate_limit_error":      true,
	"api_error":             true,
	"overloaded_error":      true,
}

// generateAnthropic speaks the Anthropic Messages dialect. Error taxonomy
// (ported from Conductor routes.ts mapping): rate_limit_error and
// overloaded_error are the rate/availability class the orchestrator
// records as a failed run to retry later; other error types fail with the
// type named. A stop_reason of "refusal" (and empty content) is a model
// refusal, not a transport failure.
func (a *BYOKAdapter) generateAnthropic(ctx context.Context, prompt, key string, maxTokens int) (ProviderResponse, error) {
	body, err := json.Marshal(anthropicMessagesRequest{
		Model:       a.cfg.Model,
		MaxTokens:   maxTokens,
		Temperature: 0,
		Messages:    []chatMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return ProviderResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL(a.cfg.BaseURL, "/v1/messages"), bytes.NewReader(body))
	if err != nil {
		return ProviderResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	if key != "" {
		httpReq.Header.Set("x-api-key", key)
	}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return ProviderResponse{}, fmt.Errorf("modelderive: byok generate call failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return ProviderResponse{}, fmt.Errorf("modelderive: byok endpoint answered a redirect (HTTP %d) — refusing to follow; configure the final endpoint URL", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var env anthropicErrorEnvelope
		errType := ""
		if json.Unmarshal(respBody, &env) == nil && env.Type == "error" {
			errType = env.Error.Type
		}
		// Closed taxonomy only (r1 F1): the message field is NEVER used, and
		// a type outside the known set is reported as unrecognized — both are
		// endpoint-controlled strings on a wire that carried the key.
		if !anthropicKnownErrorTypes[errType] {
			errType = ""
		}
		if resp.StatusCode == http.StatusTooManyRequests || errType == "rate_limit_error" || errType == "overloaded_error" {
			return ProviderResponse{}, fmt.Errorf("modelderive: byok endpoint rate/availability refusal: HTTP %d (%s)", resp.StatusCode, errType)
		}
		if errType != "" {
			return ProviderResponse{}, fmt.Errorf("modelderive: byok generate returned HTTP %d (%s)", resp.StatusCode, errType)
		}
		return ProviderResponse{}, fmt.Errorf("modelderive: byok generate returned HTTP %d", resp.StatusCode)
	}

	var parsed anthropicMessagesResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return ProviderResponse{}, fmt.Errorf("modelderive: byok response envelope not JSON: %w", err)
	}
	usage := Usage{
		PromptTokens:     parsed.Usage.InputTokens,
		CompletionTokens: parsed.Usage.OutputTokens,
		TotalTokens:      parsed.Usage.InputTokens + parsed.Usage.OutputTokens,
	}
	if parsed.StopReason == "refusal" {
		return ProviderResponse{Usage: usage, Refusal: "refusal"}, nil
	}
	var text strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	content := strings.TrimSpace(text.String())
	if content == "" {
		return ProviderResponse{Usage: usage, Refusal: "empty model content"}, nil
	}
	return ProviderResponse{RawResponse: json.RawMessage(content), Usage: usage}, nil
}
