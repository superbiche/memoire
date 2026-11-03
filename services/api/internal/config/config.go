// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Addr      string
	Home      string
	DBPath    string
	SchemaDir string
	TokenPath string
	Raw       string
	Queue     QueuePaths
	// ModelEndpointsPath is the on-disk model endpoint registry consumed by the
	// `-execute` derivation path. Config is the home of provider/endpoint data:
	// no HL hostnames are compiled into the binary.
	ModelEndpointsPath string
	// DeriveDenylistPath is the project denylist consumed by the `-execute`
	// derivation path (sweep targeting hygiene, operator brief 2026-07-10).
	DeriveDenylistPath string
	// RedactionEnabled gates the defense-at-source secret filter at the queue
	// writer (GOVERNANCE-DESIGN.md ruling 7). Default on; MEMOIRE_REDACT=0 is
	// the explicit operator escape hatch.
	RedactionEnabled bool
	// FailedTTLDays is the queue/failed/ retention window (liaison:D97
	// decision 8: 30 days). The drainer reaps older items + their error
	// sidecars. MEMOIRE_FAILED_TTL_DAYS overrides; 0 disables the reaper.
	FailedTTLDays int
	// ForwardURL enables forward mode (memoire:D147 slice 3): the base API URL
	// of the hub this client's queue forwards to, e.g. http://hub:37778/v1.
	// Empty means this machine drains its own queue locally (hub topology).
	ForwardURL string
	// ForwardTokenPath holds the HUB's bearer token for forward mode. Distinct
	// from TokenPath (this machine's own token) — the two stores must never
	// share credentials implicitly.
	ForwardTokenPath string
	// ForwardPriorityAdapters lists the source adapters whose queue items the
	// forwarder sends before any other item (MEMOIRE_FORWARD_PRIORITY_ADAPTERS,
	// comma-separated, default "claude-code", the hook-fed live adapter). Empty
	// disables priority.
	ForwardPriorityAdapters []string
	// EmbedURL is the query-embedding service base URL (hybrid lane,
	// D218), e.g. http://memoire-embed:37779. Empty leaves the hybrid
	// lane unwired — search stays FTS-only regardless of any flag.
	EmbedURL string
	// HybridSearchDefault is the deployment default for hybrid fusion
	// (MEMOIRE_HYBRID_SEARCH=1). Per-request `hybrid` overrides win.
	// Stays off until the D218 acceptance gate passes.
	HybridSearchDefault bool
	// EmbedBudgetMS bounds the in-search query-embed call; over budget
	// the search fails open to FTS-only. 0 uses the store default (150).
	EmbedBudgetMS int
	// EmbedTokenFile names the file holding the dedicated /embed bearer
	// (least-privilege split, D218 review F4). Empty falls back to this
	// deployment's own custody token — the local-first single-token
	// topology. Distinct from TokenPath and ForwardTokenPath: leaking
	// this value grants only query embedding, never API scopes.
	EmbedTokenFile string
	// SkewAlertSeconds is the one-sided future-skew alert threshold
	// (memoire:D152): events whose occurred_at_epoch is AHEAD of
	// received_at_epoch by more than this are counted as producer clock
	// faults on /v1/health. The lag direction is never alerted (it is
	// indistinguishable from legitimate queue buffering). 0 disables.
	SkewAlertSeconds int64
}

// ModelEndpoint is one configured model backend host in the endpoint registry.
//
// Secret seam (Slice 6 / D73): APIKeyEnv names the environment variable that
// carries the bearer token. The token VALUE is never read into config and never
// stored here — it is resolved by name at call time via the injected EnvLookup
// (env-var-by-name). This is the ratified interim. When the HL API keys are
// rotated and encrypted on the HLs, this name-based resolution will break, and
// that breakage is the deliberate trigger to migrate Memoire's secret source to
// the bw/vault track (MISSING.md #17). No vault/bw abstraction is added here.
type ModelEndpoint struct {
	// EndpointID is the audit/inspection identity, e.g. "hl3" or "hl5".
	EndpointID string `json:"endpoint_id"`
	// BaseURL is the OpenAI-compatible server base, e.g. http://host:8000.
	BaseURL string `json:"base_url"`
	// APIKeyEnv names the env var holding the bearer token. Empty means no auth.
	// This is a NAME, never a value.
	APIKeyEnv string `json:"api_key_env"`
	// Models is the per-endpoint model allowlist. A run must name a listed model.
	Models []string `json:"models"`
	// Profile is the endpoint's declared context geometry. Optional and
	// additive: an entry without one behaves exactly as it did before
	// profiles existed (docs/DERIVE-PROFILE-DESIGN.md).
	Profile *DeriveProfile `json:"profile,omitempty"`
}

// DeriveThinking declares how the SERVING side is configured for the model's
// thinking phase (llama-server --reasoning / --reasoning-budget). v1 is a
// declaration, not an instruction: memoire records it in run provenance and
// never sends it to the provider (docs/DERIVE-PROFILE-DESIGN.md §4).
type DeriveThinking struct {
	// Policy is the closed set on|off|budget.
	Policy string `json:"policy"`
	// BudgetTokens is the served reasoning budget. Required when Policy is
	// "budget", rejected otherwise.
	BudgetTokens int `json:"budget_tokens"`
}

// DeriveProfile is one endpoint's declared context geometry. It exists because
// a byte-denominated prompt ceiling shared across models with different
// tokenizers, contexts, and thinking behavior is a latent bug class: it let the
// first derive-bench run send prompts that left the model no room to finish
// (tmp/derive-bench/2026-08-24/autopsy-empty-content.md). The numbers are
// per-endpoint DATA; the byte mechanism itself is unchanged.
//
// No tokenizer enters memoire: BytesPerToken is measured or declared, and bytes
// stay the transport unit.
type DeriveProfile struct {
	// CtxTokens is the serving context window in tokens.
	CtxTokens int `json:"ctx_tokens"`
	// MaxCompletionTokens is the completion cap this endpoint is served with.
	MaxCompletionTokens int `json:"max_completion_tokens"`
	// BytesPerToken is the measured prompt-content ratio for this model's
	// tokenizer. Declare it conservatively: it converts the token budget into
	// the byte ceiling the prompt builder actually enforces.
	BytesPerToken float64 `json:"bytes_per_token"`
	// MarginTokens is the safety slack held back from the context.
	MarginTokens int `json:"margin_tokens"`
	// Thinking is the serving-side reasoning declaration, recorded as run
	// provenance. Optional.
	Thinking *DeriveThinking `json:"thinking,omitempty"`
}

// EffectiveMaxPromptBytes derives the byte ceiling from the declared token
// geometry: floor((ctx − max_completion − margin) × bytes_per_token). The
// invariant it enforces by construction is
// prompt_tokens_est + max_completion_tokens + margin <= ctx_tokens.
//
// The subtraction happens first and the product is floored once: the bench's
// Python port (bench/derive/benchlib.py) must compute the identical integer,
// and a differently shaped expression over the same doubles need not.
func (p *DeriveProfile) EffectiveMaxPromptBytes() int {
	if p == nil {
		return 0
	}
	room := p.CtxTokens - p.MaxCompletionTokens - p.MarginTokens
	if room <= 0 {
		return 0
	}
	return int(math.Floor(float64(room) * p.BytesPerToken))
}

// validateDeriveProfile fails closed on a profile that cannot describe a
// runnable geometry. Every message names the endpoint and the field so the
// operator can fix the registry without reading this code.
func validateDeriveProfile(endpointID string, p DeriveProfile) error {
	if p.CtxTokens <= 0 {
		return fmt.Errorf("model endpoint %q profile: ctx_tokens must be > 0", endpointID)
	}
	if p.MaxCompletionTokens <= 0 {
		return fmt.Errorf("model endpoint %q profile: max_completion_tokens must be > 0", endpointID)
	}
	if p.MarginTokens < 0 {
		return fmt.Errorf("model endpoint %q profile: margin_tokens must be >= 0", endpointID)
	}
	if p.BytesPerToken <= 0 {
		return fmt.Errorf("model endpoint %q profile: bytes_per_token must be > 0", endpointID)
	}
	if p.MaxCompletionTokens+p.MarginTokens >= p.CtxTokens {
		return fmt.Errorf("model endpoint %q profile: max_completion_tokens (%d) + margin_tokens (%d) must be < ctx_tokens (%d) — the prompt needs room too",
			endpointID, p.MaxCompletionTokens, p.MarginTokens, p.CtxTokens)
	}
	if p.Thinking == nil {
		return nil
	}
	switch p.Thinking.Policy {
	case "on", "off":
		if p.Thinking.BudgetTokens != 0 {
			return fmt.Errorf("model endpoint %q profile: thinking.budget_tokens is only valid with policy \"budget\", got policy %q", endpointID, p.Thinking.Policy)
		}
	case "budget":
		if p.Thinking.BudgetTokens <= 0 {
			return fmt.Errorf("model endpoint %q profile: thinking.policy \"budget\" requires budget_tokens > 0", endpointID)
		}
	default:
		return fmt.Errorf("model endpoint %q profile: thinking.policy %q is not one of on|off|budget", endpointID, p.Thinking.Policy)
	}
	return nil
}

type modelEndpointsFile struct {
	Endpoints []ModelEndpoint `json:"endpoints"`
}

type QueuePaths struct {
	Root     string
	Pending  string
	Inflight string
	Failed   string
	Log      string
}

func Load() (Config, error) {
	home, err := defaultHome()
	if err != nil {
		return Config{}, err
	}
	home = env("MEMOIRE_HOME", home)

	cfg := Config{
		Addr:      env("MEMOIRE_ADDR", "127.0.0.1:37778"),
		Home:      home,
		DBPath:    env("MEMOIRE_DB", filepath.Join(home, "memoire.db")),
		SchemaDir: env("MEMOIRE_SCHEMA_DIR", defaultSchemaDir()),
		TokenPath: filepath.Join(home, "token"),
		Raw:       filepath.Join(home, "raw"),
		// Default registry path lives under MEMOIRE_HOME; override with
		// MEMOIRE_MODEL_ENDPOINTS to point at a shared/managed location.
		ModelEndpointsPath: env("MEMOIRE_MODEL_ENDPOINTS", filepath.Join(home, "model-endpoints.json")),
		RedactionEnabled:   env("MEMOIRE_REDACT", "1") != "0",
	}
	// Denylist lives next to the endpoint registry (data, not code) — including
	// when the registry path itself is overridden — so a shared/managed registry
	// dir carries its denylist along. MEMOIRE_DERIVE_DENYLIST still wins.
	cfg.DeriveDenylistPath = env("MEMOIRE_DERIVE_DENYLIST",
		filepath.Join(filepath.Dir(cfg.ModelEndpointsPath), "derive-denylist.json"))
	cfg.Queue = QueuePaths{
		Root:     filepath.Join(home, "queue"),
		Pending:  filepath.Join(home, "queue", "pending"),
		Inflight: filepath.Join(home, "queue", "inflight"),
		Failed:   filepath.Join(home, "queue", "failed"),
		Log:      filepath.Join(home, "log"),
	}
	cfg.FailedTTLDays = 30
	if v := os.Getenv("MEMOIRE_FAILED_TTL_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Config{}, fmt.Errorf("MEMOIRE_FAILED_TTL_DAYS must be a non-negative integer, got %q", v)
		}
		cfg.FailedTTLDays = n
	}
	cfg.EmbedURL = os.Getenv("MEMOIRE_EMBED_URL")
	cfg.HybridSearchDefault = os.Getenv("MEMOIRE_HYBRID_SEARCH") == "1"
	if v := os.Getenv("MEMOIRE_EMBED_BUDGET_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Config{}, fmt.Errorf("MEMOIRE_EMBED_BUDGET_MS must be a non-negative integer, got %q", v)
		}
		cfg.EmbedBudgetMS = n
	}
	cfg.EmbedTokenFile = os.Getenv("MEMOIRE_EMBED_TOKEN_FILE")
	cfg.ForwardURL = os.Getenv("MEMOIRE_FORWARD_URL")
	cfg.ForwardTokenPath = env("MEMOIRE_FORWARD_TOKEN_FILE", filepath.Join(home, "forward-token"))
	cfg.ForwardPriorityAdapters = []string{"claude-code"}
	if v, ok := os.LookupEnv("MEMOIRE_FORWARD_PRIORITY_ADAPTERS"); ok {
		cfg.ForwardPriorityAdapters = nil
		for _, a := range strings.Split(v, ",") {
			if a = strings.TrimSpace(a); a != "" {
				cfg.ForwardPriorityAdapters = append(cfg.ForwardPriorityAdapters, a)
			}
		}
	}
	cfg.SkewAlertSeconds = 60
	if v := os.Getenv("MEMOIRE_SKEW_ALERT_SECONDS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return Config{}, fmt.Errorf("MEMOIRE_SKEW_ALERT_SECONDS must be a non-negative integer, got %q", v)
		}
		cfg.SkewAlertSeconds = n
	}
	return cfg, nil
}

func (c Config) EnsureDirs() error {
	dirs := []string{c.Home, c.Raw, c.Queue.Pending, c.Queue.Inflight, c.Queue.Failed, c.Queue.Log}
	// A postgres:// DSN in MEMOIRE_DB (dual-engine seam, memoire:D157) has no
	// parent directory to create.
	if !strings.HasPrefix(c.DBPath, "postgres://") && !strings.HasPrefix(c.DBPath, "postgresql://") {
		dirs = append(dirs, filepath.Dir(c.DBPath))
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (c Config) EnsureToken() (string, error) {
	if b, err := os.ReadFile(c.TokenPath); err == nil {
		return string(trimSpace(b)), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	if err := os.WriteFile(c.TokenPath, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// EmbedToken resolves the bearer sent to the /embed service. When
// EmbedTokenFile is unset the caller's fallback (the custody token)
// stands; a configured-but-unreadable or empty file is a loud error,
// never a silent downgrade to the admin bearer (memoire:D55 posture).
// A configured token EQUAL to the custody token is refused outright
// (F4 review round-1 F1, operator-ruled): a dedicated credential that
// is the admin bearer — e.g. a Secret accidentally seeded from
// $MEMOIRE_HOME/token — silently voids the privilege split; the
// single-token topology is expressed by leaving the file unset, never
// by duplicating the value. Neither value is printed.
func (c Config) EmbedToken(fallback string) (string, error) {
	if c.EmbedTokenFile == "" {
		return fallback, nil
	}
	b, err := os.ReadFile(c.EmbedTokenFile)
	if err != nil {
		return "", fmt.Errorf("read embed token %s: %w (provision the embed credential there)", c.EmbedTokenFile, err)
	}
	token := string(trimSpace(b))
	if token == "" {
		return "", fmt.Errorf("embed token %s is empty", c.EmbedTokenFile)
	}
	if token == fallback {
		return "", fmt.Errorf("embed token %s equals this deployment's custody token — the dedicated credential must be distinct (mint one per the RUNBOOK, or unset MEMOIRE_EMBED_TOKEN_FILE for the single-token topology)", c.EmbedTokenFile)
	}
	return token, nil
}

// LoadModelEndpoints reads the model endpoint registry from path. The registry
// is the single home of provider/endpoint configuration for the `-execute`
// path; the binary hardcodes no hostnames. A missing file is an operator error
// (the path is named so the operator knows where to create it), as is malformed
// JSON or an empty endpoint list.
func LoadModelEndpoints(path string) ([]ModelEndpoint, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("model endpoints config not found at %s (create it to enable -execute; see model-endpoints.example.json)", path)
		}
		return nil, err
	}
	var f modelEndpointsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("model endpoints config %s is not valid JSON: %w", path, err)
	}
	if len(f.Endpoints) == 0 {
		return nil, fmt.Errorf("model endpoints config %s has no endpoints", path)
	}
	// A profile is optional; a present one must be coherent. Absent profiles
	// keep the registry parsing exactly as it did before they existed.
	for _, ep := range f.Endpoints {
		if ep.Profile == nil {
			continue
		}
		if err := validateDeriveProfile(ep.EndpointID, *ep.Profile); err != nil {
			return nil, fmt.Errorf("model endpoints config %s: %w", path, err)
		}
	}
	return f.Endpoints, nil
}

// DeriveDenylist is the sweep-targeting project filter: projects that can
// never yield memory (tooling noise, smoke-test sessions, subagent transcript
// captures) are denied a model derivation run. Patterns are exact project
// names or path.Match globs such as "agent-*". The operator can flip the file
// to an allowlist later if noise keeps finding new names; only the denylist
// shape ships for now.
type DeriveDenylist struct {
	DenyProjects []string `json:"deny_projects"`
}

// LoadDeriveDenylist reads the project denylist from path. A missing file is
// an empty denylist (the filter is opt-in data), while an unreadable or
// malformed file is a loud operator error — a typo must not silently disable
// targeting hygiene.
func LoadDeriveDenylist(path string) (DeriveDenylist, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DeriveDenylist{}, nil
		}
		return DeriveDenylist{}, err
	}
	var d DeriveDenylist
	if err := json.Unmarshal(b, &d); err != nil {
		return DeriveDenylist{}, fmt.Errorf("derive denylist %s is not valid JSON: %w", path, err)
	}
	return d, nil
}

// Denies reports whether project matches any denylist entry, by exact name or
// path.Match glob. An invalid glob pattern falls back to exact matching only.
func (d DeriveDenylist) Denies(project string) bool {
	for _, pat := range d.DenyProjects {
		if pat == "" {
			continue
		}
		if pat == project {
			return true
		}
		if ok, err := path.Match(pat, project); err == nil && ok {
			return true
		}
	}
	return false
}

// defaultSchemaDir resolves the SQLite migration directory for runs that do
// not set MEMOIRE_SCHEMA_DIR: the cwd-relative "schema" when present (repo
// flow), else a "schema" directory found relative to the running binary
// (dist layouts: bin/ and schema/ as tarball siblings, or flat). The store's
// migrate() carries schema-pg along as a sibling of whatever resolves here.
// Falls back to the bare relative path so the refusing-to-open error keeps
// naming it.
func defaultSchemaDir() string {
	if fi, err := os.Stat("schema"); err == nil && fi.IsDir() {
		return "schema"
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		for _, cand := range []string{
			filepath.Join(exeDir, "..", "schema"),
			filepath.Join(exeDir, "schema"),
		} {
			if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
				return cand
			}
		}
	}
	return "schema"
}

func defaultHome() (string, error) {
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".memoire"), nil
	}
	return "", errors.New("HOME is not set and MEMOIRE_HOME was not provided")
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}
