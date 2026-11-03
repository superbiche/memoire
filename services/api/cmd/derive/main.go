// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
	"memoire/services/api/internal/derive"
	"memoire/services/api/internal/modelderive"
)

// promptVersion is the model derivation prompt version label recorded in run
// and dry-run metadata. There is no operator flag for it; the prompt builder
// is deterministic for a given input window.
// v2 (2026-07-10): assistant turns declared first-class evidence (transcript
// backfill landed) and outcome-extraction instruction added — v1 explicitly
// told the model assistant turns were absent, which suppressed verdict/result
// observations once backfilled turns existed.
// v2.1 (2026-07-11, operator-unfenced mechanical clause): source_event_ids
// must be the numeric event ids, never the source_event_id strings — the
// per-event block shows both namespaces and hl5 grounded on the strings,
// zeroing yield. Ruled semantics untouched; prompt_version is excluded from
// run idempotency, so no window re-keys.
// v2.2 (2026-07-18, memoire:D136 R-1): `observed` removed from the allowed
// provenance values and moved to the never-emit list — a derivation model
// infers, it does not observe. The parser downgrades a claimed `observed` to
// `inferred` fail-open and counts it (run meta `provenance_downgrades`).
// v2.3 (2026-08-01): noise_candidate gains explicit always-noise genres
// (orchestration/coordination chatter, run-status reports, transient session
// mechanics) — the one-line definition left the classifier inert (10 flags
// across 57k proposed rows; 1,264 unflagged orchestration-genre rows at
// audit time). Prompt_version stays excluded from run idempotency.
// v2.4 (2026-08-02, D211 step 3): "Already Captured (this session)" section —
// the session's existing observations rendered as do-not-re-emit context,
// budgeted (100 entries / 16 KiB, newest first) under the local-qwen token
// ceiling. Post-D211 the remaining paraphrase-dup vector is NEW events
// restating earlier session content, which coverage exclusion cannot see.
// Context is advisory: excluded from the canonical input hash, so run
// idempotency stays a function of the source events.
// v2.5 (2026-09-11, Fibery #365): content-less event runs render as one
// compact `### events A..B` block, and a content-bearing event's project and
// session_id lines are omitted when they repeat the Source Window header.
// Every claude_PostToolUse event carries a tool name and no result summary —
// no adapter in this repo produces one — so it rendered ~336 bytes of
// envelope and zero content, stranding byte-heavy, content-light rhythm
// segments above the prompt ceiling AND under the min-content floor: both
// median halves were skipped as degenerate_window, wrote no run row, and were
// re-cut identically on every later tick. Run meta gains
// collapsed_event_count and collapsed_block_count so the saving is measurable
// per run. The input manifest is unchanged (every event, collapsed ones
// included), so coverage and the canonical input hash are untouched and the
// derivation version does NOT bump — as in v2.3/v2.4, prompt_version stays
// excluded from run idempotency, so no window re-keys.
const promptVersionBase = "derive-observations-v2.5"

// promptVersion is the base label plus the graduated-genre suffix (triage
// lane v1, R-B). A graduation CHANGES the prompt, so it must change the
// version too, or two materially different prompts would share one name in
// the run ledger. With no genre graduated the suffix is empty and the label
// is byte-identical to what it has always been.
func promptVersion() string { return promptVersionBase + modelderive.GraduatedPromptSuffix() }

// promptPreviewBytes caps how much of the built prompt is echoed in -json or
// human output. The full prompt is only written when -prompt-out is given.
const promptPreviewBytes = 800

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "memoire-derive:", err)
		os.Exit(1)
	}
}

type options struct {
	strategy              string
	project               string
	sessionID             string
	startEpoch            int64
	endEpoch              int64
	jsonOut               bool
	dryRun                bool
	provider              string
	model                 string
	endpointID            string
	maxInputEvents        int
	maxPromptBytes        int
	maxOutputObservations int
	promptOut             string
	parseResponse         string
	execute               bool
	executeTimeout        time.Duration
	maxCompletionTokens   int
	reasoningEffort       string
	ignoreDenylist        bool
	noMinContentGuard     bool
	forceRerun            bool
	noSupersede           bool
	provisional           bool
	listTargets           bool
	medianEpoch           bool
	cutPoints             bool
	cutGapSeconds         int64
	cutMaxEvents          int
	cutMaxSegments        int
	listOpenIntents       bool
	listOutstanding       bool
	settleIntents         bool
	intentTTLHours        int
	sinceHours            int
	byokBaseURL           string
	byokDialect           string
	byokDataBoundary      string
	byokAPIKeyEnv         string
	byokAPIKeyFile        string
	allowExternal         bool
	// promptBytesFromFlag / completionTokensFromFlag record whether the
	// operator passed the limit EXPLICITLY (fs.Visit), which is what lets an
	// explicit -max-prompt-bytes=0 mean "no cap" against a profile that would
	// otherwise supply one.
	promptBytesFromFlag      bool
	completionTokensFromFlag bool
}

// resolvedLimits is the outcome of the profile precedence: explicit flag >
// endpoint profile > built-in default (docs/DERIVE-PROFILE-DESIGN.md §3). The
// numbers are also written back into options so every existing consumer — the
// prompt builder's Limits, the adapter config, the run provenance — inherits
// one resolution instead of re-deriving it per site.
type resolvedLimits struct {
	promptBytesSource      string
	completionTokensSource string
	thinkingPolicy         string
	thinkingBudgetTokens   int
}

// resolveLimits applies profile precedence to opts in place and reports where
// each limit came from. A nil profile leaves today's behavior untouched, so it
// is safe to call on paths that never see a registry entry.
func resolveLimits(opts *options, profile *config.DeriveProfile) resolvedLimits {
	source := func(fromFlag bool) string {
		if fromFlag {
			return "flag"
		}
		return "default"
	}
	res := resolvedLimits{
		promptBytesSource:      source(opts.promptBytesFromFlag),
		completionTokensSource: source(opts.completionTokensFromFlag),
	}
	if profile == nil {
		return res
	}
	if !opts.promptBytesFromFlag {
		opts.maxPromptBytes = profile.EffectiveMaxPromptBytes()
		res.promptBytesSource = "profile"
	}
	if !opts.completionTokensFromFlag {
		opts.maxCompletionTokens = profile.MaxCompletionTokens
		res.completionTokensSource = "profile"
	}
	// The thinking declaration is orthogonal to the numeric precedence: it
	// describes how the endpoint is SERVED, so it is recorded whenever the
	// endpoint declares it, flags or no flags.
	if profile.Thinking != nil {
		res.thinkingPolicy = profile.Thinking.Policy
		res.thinkingBudgetTokens = profile.Thinking.BudgetTokens
	}
	return res
}

// effectiveLimitsJSON is the additive dry-run field reporting the resolved
// ceilings, so the sweep and the bench can read the numbers a run would use
// instead of hardcoding them. One "source" when both limits agree; per-limit
// sources when they do not.
func (r resolvedLimits) effectiveLimitsJSON(opts options) map[string]any {
	out := map[string]any{
		"max_prompt_bytes":      opts.maxPromptBytes,
		"max_completion_tokens": opts.maxCompletionTokens,
	}
	if r.promptBytesSource == r.completionTokensSource {
		out["source"] = r.promptBytesSource
		return out
	}
	out["max_prompt_bytes_source"] = r.promptBytesSource
	out["max_completion_tokens_source"] = r.completionTokensSource
	return out
}

// resolveLimitsBestEffort applies the endpoint's profile on a path that has no
// registry requirement of its own — the dry-run. Best-effort by design: a
// dry-run that works today must not gain a new failure mode, so a missing,
// unreadable, or profile-less registry (and an endpoint absent from it) leaves
// the flags and defaults exactly as they were. A malformed profile still fails
// loudly where it matters, on -execute.
func resolveLimitsBestEffort(opts *options) resolvedLimits {
	var profile *config.DeriveProfile
	if cfg, err := config.Load(); err == nil {
		if endpoints, err := config.LoadModelEndpoints(cfg.ModelEndpointsPath); err == nil {
			if ep, ok := findEndpoint(endpoints, opts.endpointID); ok {
				profile = ep.Profile
			}
		}
	}
	return resolveLimits(opts, profile)
}

// defaultExecuteTimeout bounds the live provider call on the -execute path. It
// governs both the orchestrator's per-call context timeout and the adapter HTTP
// client timeout (availability probe + generation). Local 7-9B derivation over
// a session window can take well over the adapter's 30s default, so the execute
// path uses a generous default ceiling, overridable via -execute-timeout (D78).
const defaultExecuteTimeout = 5 * time.Minute

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("memoire-derive", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var opts options
	fs.StringVar(&opts.strategy, "strategy", "rules", "derivation strategy: rules|model")
	fs.StringVar(&opts.project, "project", "", "project name")
	fs.StringVar(&opts.sessionID, "session-id", "", "session identifier")
	fs.Int64Var(&opts.startEpoch, "start-epoch", 0, "inclusive lower bound for occurred_at_epoch")
	fs.Int64Var(&opts.endEpoch, "end-epoch", 0, "inclusive upper bound for occurred_at_epoch")
	fs.BoolVar(&opts.jsonOut, "json", false, "emit JSON result")
	fs.BoolVar(&opts.dryRun, "dry-run", false, "model strategy: build prompt without persisting a derivation run (required for -strategy=model in v1)")
	fs.StringVar(&opts.provider, "provider", "", "model provider, e.g. local-qwen (required for -strategy=model)")
	fs.StringVar(&opts.model, "model", "", "model name (required for -strategy=model)")
	fs.StringVar(&opts.endpointID, "endpoint-id", "", "local provider host identity, e.g. hl3 (required for local providers such as local-qwen)")
	fs.IntVar(&opts.maxInputEvents, "max-input-events", 0, "optional cap on input events enforced before prompt build")
	fs.IntVar(&opts.maxPromptBytes, "max-prompt-bytes", 0, "optional cap on prompt bytes enforced before any provider call")
	fs.IntVar(&opts.maxOutputObservations, "max-output-observations", 0, "optional cap on model output observations")
	fs.StringVar(&opts.promptOut, "prompt-out", "", "model strategy: write the full prompt to this local file")
	fs.StringVar(&opts.parseResponse, "parse-response", "", "model strategy: run the strict parser against a saved provider response file (no provider call)")
	fs.BoolVar(&opts.execute, "execute", false, "model strategy: perform a real, persisted derivation run against the configured provider endpoint (mutually exclusive with -dry-run)")
	fs.DurationVar(&opts.executeTimeout, "execute-timeout", defaultExecuteTimeout, "ceiling for the live provider call on the -execute path (e.g. 90s, 10m)")
	fs.IntVar(&opts.maxCompletionTokens, "max-completion-tokens", 0, "cap on model completion tokens sent as max_tokens (0 = adapter default)")
	fs.StringVar(&opts.reasoningEffort, "reasoning-effort", "", "model strategy: chat-template reasoning effort hint (none|low|medium|high|xhigh); empty = server default")
	fs.BoolVar(&opts.ignoreDenylist, "ignore-denylist", false, "manual override: run even when -project matches the derive denylist")
	fs.BoolVar(&opts.noMinContentGuard, "no-min-content-guard", false, "manual override: derive even a degenerate (near-empty) window")
	fs.BoolVar(&opts.forceRerun, "force-rerun", false, "model strategy with -execute: mint a new derivation run even when the window would dedup (prompt/model A/B reruns; forced provenance recorded in run meta_json)")
	fs.BoolVar(&opts.noSupersede, "no-supersede", false, "model strategy with -execute: kill switch for derivation-time supersede — a restatement of an earlier cut's proposed observation is rejected as a near-duplicate instead of superseding it (Fibery #344)")
	fs.BoolVar(&opts.provisional, "provisional", false, "model strategy with -execute: mark the run and its observations provisional — the window's tail is still open, so its observations are \"so far\" statements a later cut may supersede (Fibery #344)")
	fs.BoolVar(&opts.listTargets, "list-targets", false, "sweep helper: print project<US>session pairs with events in the last -since-hours, one per line, and exit (engine-agnostic via MEMOIRE_DB)")
	fs.BoolVar(&opts.medianEpoch, "median-epoch", false, "sweep helper: print the median canonical-input event epoch for -project/-session-id inside [-start-epoch, -end-epoch] and exit (empty output when the window has none)")
	fs.BoolVar(&opts.cutPoints, "cut-points", false, "sweep helper: print the session's rhythm cut epochs (long pause, event-count cap) for -project/-session-id, one per line, and exit (empty output when the window needs no cut; Fibery #343)")
	fs.Int64Var(&opts.cutGapSeconds, "cut-gap-seconds", defaultCutGapSeconds, "with -cut-points: long-pause threshold in seconds between consecutive canonical events (0 disables the class)")
	fs.IntVar(&opts.cutMaxEvents, "cut-max-events", defaultCutMaxEvents, "with -cut-points: cut after this many canonical events since the last cut (0 disables the class)")
	fs.IntVar(&opts.cutMaxSegments, "cut-max-segments", defaultCutMaxSegments, "with -cut-points: maximum rhythm segments per pass; overflow merges into the last segment (0 = unbounded)")
	fs.IntVar(&opts.sinceHours, "since-hours", 24, "with -list-targets or -list-outstanding: recency window in hours")
	fs.BoolVar(&opts.listOpenIntents, "list-open-intents", false, "sweep helper: print project<US>session pairs with an open derive intent, oldest first, and exit (memoire:D165 R-10.2)")
	fs.BoolVar(&opts.listOutstanding, "list-outstanding", false, "trigger helper: print project<US>session pairs with live canonical events not covered by any completed derivation run within -since-hours, and exit (memoire:D165 R-10.3; same store query as GET /v1/derive/outstanding)")
	fs.BoolVar(&opts.settleIntents, "settle-intents", false, "sweep helper: close open intents older than -intent-ttl-hours whose session has no live canonical events; prints the settled count")
	fs.IntVar(&opts.intentTTLHours, "intent-ttl-hours", 48, "with -settle-intents: minimum intent age before a no-work expiry")
	fs.StringVar(&opts.byokBaseURL, "byok-base-url", "", "byok: endpoint base URL (default env MEMOIRE_BYOK_BASE_URL)")
	fs.StringVar(&opts.byokDialect, "byok-dialect", "", "byok: wire dialect, openai-v1|anthropic — explicit, never auto-detected (default env MEMOIRE_BYOK_DIALECT)")
	fs.StringVar(&opts.byokDataBoundary, "byok-data-boundary", "", "byok: where the endpoint lives, local|external (default env MEMOIRE_BYOK_DATA_BOUNDARY; external also requires -allow-external)")
	fs.StringVar(&opts.byokAPIKeyEnv, "byok-api-key-env", "", "byok: NAME of the env var holding the API key (default env MEMOIRE_BYOK_API_KEY_ENV; the value is never read into config or logged)")
	fs.StringVar(&opts.byokAPIKeyFile, "byok-api-key-file", "", "byok: path to a file whose first non-comment line is the API key (default env MEMOIRE_BYOK_API_KEY_FILE)")
	fs.BoolVar(&opts.allowExternal, "allow-external", false, "explicit per-run opt-in for an external data boundary (docs/MODEL-PROVIDER.md \"Data Boundary Discipline\"): required when the byok endpoint is declared external")

	if err := fs.Parse(args); err != nil {
		return err
	}
	// Which limits the operator set EXPLICITLY. A passed flag outranks an
	// endpoint profile even at 0, where 0 keeps its existing meaning (no
	// prompt cap / adapter default completion cap).
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "max-prompt-bytes":
			opts.promptBytesFromFlag = true
		case "max-completion-tokens":
			opts.completionTokensFromFlag = true
		}
	})
	if opts.executeTimeout <= 0 {
		return errors.New("-execute-timeout must be positive")
	}
	if !isReasoningEffort(opts.reasoningEffort) {
		return fmt.Errorf("unknown -reasoning-effort %q (want none|low|medium|high|xhigh, or empty for the server default)", opts.reasoningEffort)
	}
	modes := 0
	for _, m := range []bool{opts.listTargets, opts.medianEpoch, opts.cutPoints, opts.listOpenIntents, opts.listOutstanding, opts.settleIntents} {
		if m {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("-list-targets, -median-epoch, -cut-points, -list-open-intents, -list-outstanding and -settle-intents are mutually exclusive")
	}
	if opts.listTargets {
		return runListTargets(stdout, opts)
	}
	if opts.medianEpoch {
		return runMedianEpoch(stdout, opts)
	}
	if opts.cutPoints {
		return runCutPoints(stdout, opts)
	}
	if opts.listOpenIntents {
		return runListOpenIntents(stdout)
	}
	if opts.listOutstanding {
		return runListOutstanding(stdout, opts)
	}
	if opts.settleIntents {
		return runSettleIntents(stdout, opts)
	}

	switch opts.strategy {
	case "rules":
		return runRulesStrategy(stdout, opts)
	case "model":
		return runModelStrategy(stdout, opts)
	default:
		return fmt.Errorf("unknown -strategy %q (want rules|model)", opts.strategy)
	}
}

func runRulesStrategy(stdout io.Writer, opts options) error {
	window := derive.Window{
		Project:    opts.project,
		SessionID:  opts.sessionID,
		StartEpoch: opts.startEpoch,
		EndEpoch:   opts.endEpoch,
	}
	if window.Project == "" {
		return errors.New("missing required -project")
	}
	if window.SessionID == "" && (window.StartEpoch == 0 || window.EndEpoch == 0) {
		return errors.New("provide -session-id or both -start-epoch and -end-epoch")
	}

	store, closeStore, err := openStore()
	if err != nil {
		return err
	}
	defer closeStore()

	result, err := derive.RunRules(context.Background(), store, window)
	if err != nil {
		return err
	}
	if result.FullyCovered {
		if opts.jsonOut {
			return writeJSON(stdout, map[string]any{
				"strategy":   "rules",
				"skipped":    "fully_covered",
				"project":    result.Window.Project,
				"session_id": result.Window.SessionID,
			})
		}
		fmt.Fprintf(stdout, "strategy=rules skipped=fully_covered project=%s session_id=%s\n",
			result.Window.Project, result.Window.SessionID)
		return nil
	}

	// A duplicate of a NON-terminal run is a deferral, not a success
	// (round-6 LOW-NEW-1): another attempt holds the row (or a crash
	// residue awaits the stale ceiling). Expose the status so no caller
	// mistakes it for a completed dedup.
	deferred := result.Run.Duplicate && result.Run.Status != "completed"
	if opts.jsonOut {
		out := map[string]any{
			"strategy":           "rules",
			"run_id":             result.Run.ID,
			"duplicate":          result.Run.Duplicate,
			"project":            result.Window.Project,
			"session_id":         result.Window.SessionID,
			"window_start_epoch": result.Window.StartEpoch,
			"window_end_epoch":   result.Window.EndEpoch,
			"source_event_count": result.SourceEventCount,
			"candidate_count":    result.CandidateCount,
			"applied_count":      len(result.Applied),
			"replayed_count":     result.ReplayedCount,
		}
		if result.Run.Duplicate {
			out["run_status"] = result.Run.Status
			out["deferred"] = deferred
		}
		return writeJSON(stdout, out)
	}

	dupMark := ""
	if result.Run.Duplicate {
		dupMark = fmt.Sprintf(" run_status=%s deferred=%t", result.Run.Status, deferred)
	}
	fmt.Fprintf(stdout, "strategy=rules run_id=%s duplicate=%t source_events=%d candidates=%d applied=%d replayed=%d window=%d..%d%s\n",
		result.Run.ID,
		result.Run.Duplicate,
		result.SourceEventCount,
		result.CandidateCount,
		len(result.Applied),
		result.ReplayedCount,
		result.Window.StartEpoch,
		result.Window.EndEpoch,
		dupMark,
	)
	return nil
}

func runModelStrategy(stdout io.Writer, opts options) error {
	// Flag validation runs before any store access so misuse fails fast and
	// without touching the live store. The model path has exactly two modes:
	// -dry-run (non-persistent, no provider call) and -execute (persistent, real
	// provider call). Exactly one is required.
	if opts.dryRun && opts.execute {
		return errors.New("-strategy=model: choose either -dry-run or -execute, not both")
	}
	if !opts.dryRun && !opts.execute {
		return errors.New("-strategy=model requires -dry-run or -execute")
	}
	if opts.provider == "" {
		return errors.New("-strategy=model requires -provider")
	}
	// Provider-sovereignty allowlist: the model path accepts only contracted model
	// providers (docs/MODEL-PROVIDER.md "Provider Selection": openai, local-qwen,
	// byok). "manual" is a valid operator-marker source (docs/DERIVATION.md) but
	// NOT a model provider; rejecting it here (dry-run included) keeps the model
	// path from producing dry-run metadata for an unimplemented or bogus provider.
	if !isModelProvider(opts.provider) {
		return fmt.Errorf("-strategy=model provider %q is not a model provider (want openai, local-qwen, or byok)", opts.provider)
	}
	// byok config is env-first with flags overriding (docs/BYOK-DERIVE-DESIGN.md
	// §3): fill the generic -model / -endpoint-id from the byok env surface
	// before the generic requirements below judge them, and normalize the
	// resolved dialect back into opts so both modes report it.
	if opts.provider == modelderive.ProviderBYOK {
		settings := resolveBYOK(opts)
		opts.model = settings.model
		opts.endpointID = settings.endpointID
		opts.byokDialect = settings.dialect
	}
	if opts.model == "" {
		return errors.New("-strategy=model requires -model")
	}
	// Local providers require an explicit endpoint id (host identity). The CLI
	// must not hide it behind a default (docs/MODEL-PROVIDER.md "Provider
	// Naming"). External providers (later) leave it empty.
	if isLocalProvider(opts.provider) && opts.endpointID == "" {
		return fmt.Errorf("-strategy=model with local provider %q requires -endpoint-id", opts.provider)
	}
	if opts.project == "" {
		return errors.New("missing required -project")
	}
	if opts.sessionID == "" && (opts.startEpoch == 0 || opts.endEpoch == 0) {
		return errors.New("provide -session-id or both -start-epoch and -end-epoch")
	}

	// -force-rerun only makes sense where a run is persisted; a dry-run never
	// dedups, so accepting the flag there would silently mean nothing.
	if opts.forceRerun && !opts.execute {
		return errors.New("-force-rerun requires -execute")
	}
	// The write-time gate and the run metadata exist only where a run is
	// persisted, so on a dry-run both #344 flags would silently mean nothing.
	if opts.noSupersede && !opts.execute {
		return errors.New("-no-supersede requires -execute")
	}
	if opts.provisional && !opts.execute {
		return errors.New("-provisional requires -execute")
	}

	if opts.execute {
		return runModelExecute(stdout, opts)
	}
	return runModelDryRun(stdout, opts)
}

func runModelDryRun(stdout io.Writer, opts options) error {
	// Resolve the endpoint's declared geometry before sizing anything: the
	// dry-run's whole job is to report the prompt a run WOULD send, which
	// means reporting it against the ceiling that run would enforce.
	limits := resolveLimitsBestEffort(&opts)

	store, closeStore, err := openStore()
	if err != nil {
		return err
	}
	defer closeStore()

	ctx := context.Background()
	fetchFilter := db.DerivationEventFilter{
		Project:          opts.project,
		SessionID:        opts.sessionID,
		WindowStartEpoch: opts.startEpoch,
		WindowEndEpoch:   opts.endEpoch,
		EventTypes:       modelderive.CanonicalInputEventTypes(),
		// Mirror the execute path (orchestrate.go): the dry-run sizes and
		// windows exactly what a run would feed the model, so it must
		// apply the same covered-event exclusion.
		ExcludeCovered: true,
	}
	events, err := store.ListEventsForDerivation(ctx, fetchFilter)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		// Same skip contract as execute (round-1 F3): a fully covered
		// window is a clean no-op. The classification is ONE statement
		// (rounds 1-2): a racing commit is sized rather than skipped, and
		// a soft-delete cannot fake coverage of an empty window.
		total, uncovered, cerr := store.ListEventsForDerivationCoverage(ctx, fetchFilter)
		if cerr != nil {
			return cerr
		}
		if total == 0 {
			return errors.New("no source events matched derivation window")
		}
		events = uncovered
		if len(events) == 0 {
			if opts.jsonOut {
				return writeJSON(stdout, map[string]any{
					"strategy":   "model",
					"dry_run":    true,
					"skipped":    "fully_covered",
					"project":    opts.project,
					"session_id": opts.sessionID,
					// The resolved ceilings are reported on EVERY dry-run
					// JSON, skip included (review r1 F2): a caller reading
					// the endpoint's geometry must not have to hit a
					// non-skipped window to learn it.
					"effective_limits": limits.effectiveLimitsJSON(opts),
				})
			}
			fmt.Fprintf(stdout, "strategy=model dry_run=true skipped=fully_covered project=%s session_id=%s\n",
				opts.project, opts.sessionID)
			return nil
		}
	}

	// The store already returns events ordered by (occurred_at_epoch, id). The
	// actual window bounds fall back to the event range when not pinned, mirroring
	// the rules path so the dry-run inspects the same window a model run will use.
	startEpoch := opts.startEpoch
	endEpoch := opts.endEpoch
	if startEpoch == 0 {
		startEpoch = events[0].OccurredAtEpoch
	}
	if endEpoch == 0 {
		endEpoch = events[len(events)-1].OccurredAtEpoch
	}

	req := modelderive.Request{
		Provider:          opts.provider,
		Model:             opts.model,
		EndpointID:        opts.endpointID,
		DerivationVersion: derive.DerivationVersionV2,
		PromptVersion:     promptVersion(),
		OutputSchema:      modelderive.SchemaVersion,
		Window: modelderive.Window{
			Project:    opts.project,
			SessionID:  opts.sessionID,
			StartEpoch: startEpoch,
			EndEpoch:   endEpoch,
		},
		Events: events,
		Limits: modelderive.Limits{
			MaxInputEvents:        opts.maxInputEvents,
			MaxPromptBytes:        opts.maxPromptBytes,
			MaxOutputObservations: opts.maxOutputObservations,
			MaxCompletionTokens:   opts.maxCompletionTokens,
		},
		ThinkingPolicy:       limits.thinkingPolicy,
		ThinkingBudgetTokens: limits.thinkingBudgetTokens,
	}

	// Mirror the execute path's already-captured context (prompt v2.4) so the
	// dry-run sizes the exact prompt a run would send. BuildPrompt budgets
	// the section internally (yielding to -max-prompt-bytes headroom) and
	// only ever renders the newest DefaultAlreadyCapturedMaxEntries, so
	// fetching exactly that many yields the identical prompt without the
	// gate-sized read. (-force-rerun requires -execute, so the dry-run never
	// sees the ForceRerun carve-out.)
	if opts.sessionID != "" {
		sessionObs, err := store.ListSessionObservationsForDerivation(ctx, opts.project, opts.sessionID, modelderive.DefaultAlreadyCapturedMaxEntries)
		if err != nil {
			return err
		}
		req.AlreadyCaptured = sessionObs
	}

	prompt, meta, err := modelderive.BuildPrompt(req)
	promptOverCap := false
	if errors.Is(err, modelderive.ErrPromptExceedsMaxBytes) {
		// The WINDOW alone exceeds -max-prompt-bytes (the advisory context
		// has already yielded to zero). The dry-run's job is sizing, not
		// gating: report the base size so the sweep's split loop can halve
		// the window — erroring here would break splitting entirely.
		capless := req
		capless.Limits.MaxPromptBytes = 0
		capless.AlreadyCaptured = nil
		prompt, meta, err = modelderive.BuildPrompt(capless)
		promptOverCap = true
	}
	if err != nil {
		return err
	}

	// Same canonical input hash the model run will compute (reuses the shared
	// derive hash so dry-run output matches future persistence).
	inputHash, err := derive.CanonicalInputHash(events)
	if err != nil {
		return err
	}

	if opts.promptOut != "" {
		if err := os.WriteFile(opts.promptOut, []byte(prompt), 0o600); err != nil {
			return fmt.Errorf("write -prompt-out file: %w", err)
		}
	}

	out := map[string]any{
		"strategy":           "model",
		"dry_run":            true,
		"persisted":          false,
		"provider":           opts.provider,
		"model":              opts.model,
		"endpoint_id":        opts.endpointID,
		"project":            opts.project,
		"session_id":         opts.sessionID,
		"window_start_epoch": startEpoch,
		"window_end_epoch":   endEpoch,
		"source_event_count": len(events),
		"input_hash":         inputHash,
		"prompt_bytes":       len(prompt),
		"prompt_preview":     previewString(prompt),
		"prompt_meta":        meta,
		"effective_limits":   limits.effectiveLimitsJSON(opts),
	}
	if promptOverCap {
		// Additive transparency marker: prompt_bytes is the capless BASE
		// size (context omitted) because the window alone exceeds
		// -max-prompt-bytes; an execute of this exact window would refuse.
		out["prompt_over_cap"] = true
	}
	if opts.byokDialect != "" {
		out["dialect"] = opts.byokDialect
	}
	if opts.promptOut != "" {
		out["prompt_out"] = opts.promptOut
	}

	if opts.parseResponse != "" {
		parsed, err := runParseResponse(opts.parseResponse, req)
		if err != nil {
			return err
		}
		out["parsed"] = map[string]any{
			"proposed_observation_count": len(parsed.Observations),
			"unknown_candidate_count":    len(parsed.UnknownCandidates),
			"rejected_candidate_count":   len(parsed.RejectedCandidates),
			"proposed_observations":      parsed.Observations,
			"unknown_candidates":         parsed.UnknownCandidates,
			"rejected_candidates":        parsed.RejectedCandidates,
		}
	}

	if opts.jsonOut {
		return writeJSON(stdout, out)
	}

	fmt.Fprintf(stdout, "strategy=model dry_run=true persisted=false provider=%s model=%s source_events=%d window=%d..%d prompt_bytes=%d input_hash=%s\n",
		opts.provider, opts.model, len(events), startEpoch, endEpoch, len(prompt), inputHash)
	if opts.promptOut != "" {
		fmt.Fprintf(stdout, "prompt_out=%s\n", opts.promptOut)
	}
	if opts.parseResponse != "" {
		parsed := out["parsed"].(map[string]any)
		fmt.Fprintf(stdout, "parsed proposed=%d unknown=%d rejected=%d\n",
			parsed["proposed_observation_count"], parsed["unknown_candidate_count"], parsed["rejected_candidate_count"])
	}
	return nil
}

// runModelExecute performs a real, persisted model-backed derivation run. It
// constructs the provider adapter from the on-disk endpoint registry plus an
// injected EnvLookup (os env, resolved by name), builds the request, drives the
// orchestrator, and surfaces the RunOutcome to the operator.
//
// Error policy (docs/MODEL-PROVIDER.md "Failure Modes"): a Go error here is an
// operator/setup/store failure (exit 1). A recorded failed/partial run is NOT a
// Go error — the run id and status are printed and the command exits 0.
func runModelExecute(stdout io.Writer, opts options) error {
	// Runnable providers: the local family (local-qwen) and byok (the
	// user-configured standalone endpoint, memoire:D232). The contracted
	// openai provider remains deferred; refuse before any config/store access.
	if !isLocalProvider(opts.provider) && opts.provider != modelderive.ProviderBYOK {
		return fmt.Errorf("-execute supports local-qwen and byok, got %q", opts.provider)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Sweep targeting hygiene (operator brief 2026-07-10): a denylisted project
	// is skipped before any endpoint/store access. The marker output keeps the
	// sweep log honest — the project was seen and deliberately not derived.
	if !opts.ignoreDenylist {
		denylist, err := config.LoadDeriveDenylist(cfg.DeriveDenylistPath)
		if err != nil {
			return err
		}
		if denylist.Denies(opts.project) {
			// A denial is a TERMINAL verdict for any open notify intent
			// (round-2 MED-4: without this, a denylisted project's intent
			// sits in OpenDeriveIntents forever — its canonical events make
			// the no-work expiry unreachable). BOUNDED best-effort
			// (round-3 MED-NEW-1): db.Open pings unbounded on PG, so the
			// whole open+serve runs in a goroutine raced against a 15s
			// timer — a blackholed store must never hang the sweep's clean
			// denial skip. On timeout the goroutine is abandoned (the CLI
			// exits right after; the intent settles on a later tick).
			denyServed := settleDeniedIntent(opts.project, opts.sessionID)
			if opts.jsonOut {
				return writeJSON(stdout, map[string]any{
					"strategy":      "model",
					"execute":       true,
					"persisted":     false,
					"skipped":       "project_denied",
					"project":       opts.project,
					"session_id":    opts.sessionID,
					"denylist":      cfg.DeriveDenylistPath,
					"intent_served": denyServed,
				})
			}
			fmt.Fprintf(stdout, "strategy=model execute=true skipped=project_denied project=%s denylist=%s intent_served=%t\n",
				opts.project, cfg.DeriveDenylistPath, denyServed)
			return nil
		}
	}

	// Limits resolution starts flag-or-default; the local-qwen branch below
	// re-resolves it against the selected endpoint's profile. byok has no
	// registry entry, so it keeps flags and defaults (D233 gets profiles later).
	limits := resolveLimits(&opts, nil)

	var adapter modelderive.Provider
	if opts.provider == modelderive.ProviderBYOK {
		byok, err := buildBYOKAdapter(opts)
		if err != nil {
			return err
		}
		adapter = byok
	} else {
		// Config is the home of endpoint data: load the registry rather than
		// hardcoding any host. A missing/invalid registry is an operator error.
		endpoints, err := config.LoadModelEndpoints(cfg.ModelEndpointsPath)
		if err != nil {
			return err
		}
		ep, ok := findEndpoint(endpoints, opts.endpointID)
		if !ok {
			return fmt.Errorf("endpoint-id %q is not present in model endpoints config %s", opts.endpointID, cfg.ModelEndpointsPath)
		}

		// The endpoint's declared geometry resolves ONCE, here, before the
		// adapter and the request are built from opts — the composition root
		// is where config and modelderive meet (D75), so it is where the
		// profile becomes numbers.
		limits = resolveLimits(&opts, ep.Profile)

		// Secret seam (D73 / Finding 5): the bearer token is resolved by env-var NAME
		// at call time via the injected EnvLookup. The value is never read into config
		// and never printed. Pre-flight the required env var here so a missing secret
		// is a loud operator error naming only the variable, instead of being buried
		// in a recorded "failed" run by the adapter's call-time auth check.
		envLookup := modelderive.EnvLookup(os.LookupEnv)
		if err := requireEndpointSecret(ep, envLookup); err != nil {
			return err
		}

		qwen, err := modelderive.NewQwenAdapter(modelderive.QwenConfig{
			Endpoints:             toQwenEndpoints(endpoints),
			Timeout:               opts.executeTimeout,
			MaxPromptBytes:        opts.maxPromptBytes,
			MaxOutputObservations: opts.maxOutputObservations,
			MaxCompletionTokens:   opts.maxCompletionTokens,
			ReasoningEffort:       opts.reasoningEffort,
		}, nil, envLookup)
		if err != nil {
			return err
		}
		adapter = qwen
	}

	store, closeStore, err := openStore()
	if err != nil {
		return err
	}
	defer closeStore()

	orch := &modelderive.Orchestrator{
		Store:   store,
		Timeout: opts.executeTimeout,
		Guard:   modelderive.WindowGuard{Disabled: opts.noMinContentGuard},
	}
	req := modelderive.Request{
		Provider:          opts.provider,
		Model:             opts.model,
		EndpointID:        opts.endpointID,
		Dialect:           opts.byokDialect,
		DerivationVersion: derive.DerivationVersionV2,
		PromptVersion:     promptVersion(),
		OutputSchema:      modelderive.SchemaVersion,
		Window: modelderive.Window{
			Project:    opts.project,
			SessionID:  opts.sessionID,
			StartEpoch: opts.startEpoch,
			EndEpoch:   opts.endEpoch,
		},
		// MaxOutputObservations defaults to 0 = uncapped during the calibration
		// window (D72). The orchestrator fetches events and resolves window bounds.
		Limits: modelderive.Limits{
			MaxInputEvents:        opts.maxInputEvents,
			MaxPromptBytes:        opts.maxPromptBytes,
			MaxOutputObservations: opts.maxOutputObservations,
			MaxCompletionTokens:   opts.maxCompletionTokens,
		},
		ThinkingPolicy:       limits.thinkingPolicy,
		ThinkingBudgetTokens: limits.thinkingBudgetTokens,
		ForceRerun:           opts.forceRerun,
		NoSupersede:          opts.noSupersede,
		Provisional:          opts.provisional,
	}

	outcome, err := orch.Run(context.Background(), adapter, req)
	if err != nil {
		// Setup/config/store error: operator-facing, exit 1.
		return err
	}

	// Min-content guard skip: no run record, no provider call. The marker plus
	// the measured numbers make the sweep log line self-explanatory. A skip
	// still SERVES an open notify intent — there is nothing to derive, and an
	// intent left open would sit in OpenDeriveIntents forever.
	if outcome.Skipped != "" {
		// A guard skip serves through the attempt's own fetch stamp — the
		// verdict is exactly as fresh as that fetch.
		intentLatency, intentServed := serveDeriveIntent(store, opts.project, opts.sessionID, "skipped:"+outcome.Skipped, outcome.FetchedAtEpoch)
		if opts.jsonOut {
			out := map[string]any{
				"strategy":               "model",
				"execute":                true,
				"persisted":              false,
				"skipped":                outcome.Skipped,
				"project":                opts.project,
				"session_id":             opts.sessionID,
				"non_trivial_events":     outcome.Guard.NonTrivialEvents,
				"content_chars":          outcome.Guard.ContentChars,
				"min_non_trivial_events": outcome.Guard.MinNonTrivialEvents,
				"min_content_chars":      outcome.Guard.MinContentChars,
			}
			if intentServed {
				out["intent_latency_s"] = intentLatency
			}
			return writeJSON(stdout, out)
		}
		intentMark := ""
		if intentServed {
			intentMark = fmt.Sprintf(" intent_latency_s=%d", intentLatency)
		}
		fmt.Fprintf(stdout, "strategy=model execute=true skipped=%s project=%s non_trivial_events=%d content_chars=%d%s\n",
			outcome.Skipped, opts.project, outcome.Guard.NonTrivialEvents, outcome.Guard.ContentChars, intentMark)
		return nil
	}

	// Serve the notify intent (memoire:D165 R-10.2, Q1 instrumentation).
	// Semantics of record: the FIRST completed run for the session closes the
	// intent, so latency measures notify -> first coverage, not full-session
	// sweep completion (oversized sessions keep deriving their remaining
	// split legs with the intent already served). Failed runs leave the
	// intent open for the next sweep tick.
	var intentLatency int64
	intentServed := false
	if outcome.Status == "completed" {
		intentLatency, intentServed = serveDeriveIntent(store, opts.project, opts.sessionID, outcome.RunID, outcome.FetchedAtEpoch)
	}

	if opts.jsonOut {
		out := map[string]any{
			"strategy":         "model",
			"execute":          true,
			"persisted":        true,
			"provider":         opts.provider,
			"model":            opts.model,
			"endpoint_id":      opts.endpointID,
			"project":          opts.project,
			"session_id":       opts.sessionID,
			"run_id":           outcome.RunID,
			"status":           outcome.Status,
			"duplicate":        outcome.Duplicate,
			"written_count":    outcome.WrittenCount,
			"rejected_count":   outcome.RejectedCount,
			"unknown_count":    outcome.UnknownCount,
			"superseded_count": outcome.SupersededCount,
			"refused":          outcome.Refused,
			"usage": map[string]any{
				"prompt_tokens":     outcome.Usage.PromptTokens,
				"completion_tokens": outcome.Usage.CompletionTokens,
				"total_tokens":      outcome.Usage.TotalTokens,
			},
		}
		if opts.byokDialect != "" {
			out["dialect"] = opts.byokDialect
		}
		if outcome.Forced {
			out["forced"] = true
			if outcome.SupersededRunID != "" {
				out["superseded_run_id"] = outcome.SupersededRunID
			}
		}
		if intentServed {
			out["intent_latency_s"] = intentLatency
		}
		return writeJSON(stdout, out)
	}

	forced := ""
	if outcome.Forced {
		forced = fmt.Sprintf(" forced=true superseded_run_id=%s", outcome.SupersededRunID)
	}
	intentMark := ""
	if intentServed {
		intentMark = fmt.Sprintf(" intent_latency_s=%d", intentLatency)
	}
	fmt.Fprintf(stdout, "strategy=model execute=true run_id=%s status=%s duplicate=%t written=%d superseded=%d rejected=%d unknown=%d refused=%t%s%s\n",
		outcome.RunID, outcome.Status, outcome.Duplicate,
		outcome.WrittenCount, outcome.SupersededCount, outcome.RejectedCount, outcome.UnknownCount, outcome.Refused, forced, intentMark)
	return nil
}

// settleDeniedOpenStore is the denial path's store opener — a package var
// so the harness can substitute a blocking opener and falsify the bound
// (round-4 LOW-NEW-2).
var settleDeniedOpenStore = openStore

// settleDeniedTimeout bounds the denial-branch bookkeeping, store open
// included; test-overridable for the same falsifier.
var settleDeniedTimeout = 15 * time.Second

// settleDeniedIntent runs the denial-branch intent bookkeeping — store
// open included — under a hard bound (round-3 MED-NEW-1). The denial
// verdict is evaluated live, so eligibility uses the current time.
func settleDeniedIntent(project, sessionID string) bool {
	done := make(chan bool, 1)
	go func() {
		served := false
		if store, closeStore, err := settleDeniedOpenStore(); err == nil {
			_, served = serveDeriveIntent(store, project, sessionID, "skipped:project_denied", time.Now().Unix())
			closeStore()
		}
		done <- served
	}()
	select {
	case served := <-done:
		return served
	case <-time.After(settleDeniedTimeout):
		fmt.Fprintf(os.Stderr, "derive: denial intent bookkeeping for project=%s session=%s timed out; will settle on a later tick\n", project, sessionID)
		return false
	}
}

// serveDeriveIntent closes the open notify intent for (project, session)
// (memoire:D165 R-10.2). Best-effort by contract — and honestly so
// (round-1 MED-3): the bookkeeping is BOUNDED (10s) so a blackholed store
// cannot delay the derive result, and a real error emits a non-secret
// stderr marker instead of silently degrading to "not served".
// servedAtEpoch is the eligibility timestamp (rounds 2-4): the CURRENT
// attempt's own fetch/evaluation time — never a run row's original
// created_at — compared strictly against the intent's LATEST notify.
func serveDeriveIntent(store *db.Store, project, sessionID, ref string, servedAtEpoch int64) (int64, bool) {
	if project == "" || sessionID == "" {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done, ok, err := store.CompleteDeriveIntent(ctx, project, sessionID, ref, servedAtEpoch, time.Now().Unix())
	if err != nil {
		fmt.Fprintf(os.Stderr, "derive: intent completion for project=%s session=%s could not be recorded: %v\n", project, sessionID, err)
		return 0, false
	}
	if !ok {
		return 0, false
	}
	return done.LatencySeconds, true
}

// byokSettings is the resolved byok config surface: env-first, flags
// override (docs/BYOK-DERIVE-DESIGN.md §3).
type byokSettings struct {
	baseURL    string
	dialect    string
	boundary   string
	keyEnv     string
	keyFile    string
	endpointID string
	model      string
}

// resolveBYOK merges byok flags over the MEMOIRE_BYOK_* env surface. The
// endpoint label defaults to "byok"; everything else stays empty when
// unset so the adapter constructor names exactly what is missing.
func resolveBYOK(opts options) byokSettings {
	pick := func(flagVal, envKey string) string {
		if flagVal != "" {
			return flagVal
		}
		return strings.TrimSpace(os.Getenv(envKey))
	}
	s := byokSettings{
		baseURL:    pick(opts.byokBaseURL, "MEMOIRE_BYOK_BASE_URL"),
		dialect:    pick(opts.byokDialect, "MEMOIRE_BYOK_DIALECT"),
		boundary:   pick(opts.byokDataBoundary, "MEMOIRE_BYOK_DATA_BOUNDARY"),
		keyEnv:     pick(opts.byokAPIKeyEnv, "MEMOIRE_BYOK_API_KEY_ENV"),
		keyFile:    pick(opts.byokAPIKeyFile, "MEMOIRE_BYOK_API_KEY_FILE"),
		endpointID: pick(opts.endpointID, "MEMOIRE_BYOK_ENDPOINT_ID"),
		model:      pick(opts.model, "MEMOIRE_BYOK_MODEL"),
	}
	if s.endpointID == "" {
		s.endpointID = modelderive.ProviderBYOK
	}
	return s
}

// buildBYOKAdapter resolves the byok config, enforces the per-run external
// opt-in, pre-flights the key source, and constructs the adapter. Every
// refusal is a loud operator error naming a field or variable — never a
// key value (D73 posture).
func buildBYOKAdapter(opts options) (*modelderive.BYOKAdapter, error) {
	settings := resolveBYOK(opts)

	// Explicit per-run opt-in for external endpoints (docs/MODEL-PROVIDER.md
	// "Data Boundary Discipline"; BYOK design §4). The declaration lives in
	// config; the consent lives on the invocation.
	if settings.boundary == "external" && !opts.allowExternal {
		return nil, errors.New("byok endpoint is declared external — pass -allow-external to opt in to sending this window's events off-machine for this run")
	}

	// Key pre-flight: loud operator errors before any store access. The
	// adapter re-resolves at call time; this only fronts the failure.
	if settings.keyEnv != "" {
		if v, ok := os.LookupEnv(settings.keyEnv); !ok || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("byok requires environment variable %s but it is unset (export the env var; its value is never read into config)", settings.keyEnv)
		}
	}
	if settings.keyFile != "" {
		info, err := os.Stat(settings.keyFile)
		if err != nil {
			return nil, fmt.Errorf("byok key file %s: %w", settings.keyFile, err)
		}
		// A world-readable key file defeats the point of file custody
		// (BYOK design §3): refuse loudly rather than use it.
		if info.Mode().Perm()&0o004 != 0 {
			return nil, fmt.Errorf("byok key file %s is world-readable (mode %o) — chmod it to 0600", settings.keyFile, info.Mode().Perm())
		}
	}

	return modelderive.NewBYOKAdapter(modelderive.BYOKConfig{
		BaseURL:               settings.baseURL,
		Model:                 settings.model,
		Dialect:               settings.dialect,
		DataBoundary:          settings.boundary,
		APIKeyEnv:             settings.keyEnv,
		APIKeyFile:            settings.keyFile,
		EndpointID:            settings.endpointID,
		Timeout:               opts.executeTimeout,
		MaxPromptBytes:        opts.maxPromptBytes,
		MaxOutputObservations: opts.maxOutputObservations,
		MaxCompletionTokens:   opts.maxCompletionTokens,
	}, nil, modelderive.EnvLookup(os.LookupEnv))
}

// findEndpoint resolves a configured endpoint by id.
func findEndpoint(endpoints []config.ModelEndpoint, id string) (config.ModelEndpoint, bool) {
	for _, ep := range endpoints {
		if ep.EndpointID == id {
			return ep, true
		}
	}
	return config.ModelEndpoint{}, false
}

// toQwenEndpoints maps the config registry onto the adapter's endpoint type.
func toQwenEndpoints(endpoints []config.ModelEndpoint) []modelderive.QwenEndpoint {
	out := make([]modelderive.QwenEndpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		out = append(out, modelderive.QwenEndpoint{
			EndpointID: ep.EndpointID,
			BaseURL:    ep.BaseURL,
			APIKeyEnv:  ep.APIKeyEnv,
			Models:     ep.Models,
		})
	}
	return out
}

// requireEndpointSecret pre-flights the endpoint's configured bearer-token env
// var. It names ONLY the variable, never its value. An endpoint with no
// configured APIKeyEnv needs no auth and passes.
//
// env-var-by-name, FOR NOW (D73): this is the ratified interim secret source.
// When the HL API keys are rotated/encrypted, this lookup breaks — and that
// breakage is the deliberate trigger to migrate to the bw/vault track
// (MISSING.md #17). No vault/bw abstraction is added here.
func requireEndpointSecret(ep config.ModelEndpoint, envLookup modelderive.EnvLookup) error {
	if ep.APIKeyEnv == "" {
		return nil
	}
	if v, ok := envLookup(ep.APIKeyEnv); !ok || strings.TrimSpace(v) == "" {
		return fmt.Errorf("endpoint %q requires environment variable %s but it is unset (export the env var; its value is never read into config)", ep.EndpointID, ep.APIKeyEnv)
	}
	return nil
}

func runParseResponse(path string, req modelderive.Request) (modelderive.ParsedResponse, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return modelderive.ParsedResponse{}, fmt.Errorf("read -parse-response file: %w", err)
	}
	parsed, err := modelderive.ParseResponse(raw, req)
	if err != nil {
		return modelderive.ParsedResponse{}, fmt.Errorf("parse saved response: %w", err)
	}
	return parsed, nil
}

// sweepFieldSep is the unit separator the sweep script splits on: unlike a
// tab it cannot plausibly appear inside a project or session id.
const sweepFieldSep = "\x1f"

// runListTargets prints the sweep's enumeration — one project<US>session
// pair per line — through the dialect seam, replacing the sweep script's
// raw sqlite3 query (which silently no-oped once MEMOIRE_DB became a
// postgres:// DSN at the D159 cutover).
func runListTargets(stdout io.Writer, opts options) error {
	if opts.sinceHours <= 0 {
		return errors.New("-since-hours must be positive")
	}
	store, closeStore, err := openStore()
	if err != nil {
		return err
	}
	defer closeStore()
	since := time.Now().Add(-time.Duration(opts.sinceHours) * time.Hour).Unix()
	targets, err := store.RecentDeriveTargets(context.Background(), since)
	if err != nil {
		return err
	}
	for _, t := range targets {
		fmt.Fprintf(stdout, "%s%s%s\n", t.Project, sweepFieldSep, t.SessionID)
	}
	return nil
}

// runListOpenIntents prints open notify intents oldest-first in the same
// project<US>session framing as -list-targets: the sweep merges these AHEAD
// of its own enumeration (priority work) and dedups the union
// (memoire:D165 R-10.2, round-1 MED-2 — intents are scheduling input, not
// telemetry).
func runListOpenIntents(stdout io.Writer) error {
	store, closeStore, err := openStore()
	if err != nil {
		return err
	}
	defer closeStore()
	intents, err := store.OpenDeriveIntents(context.Background())
	if err != nil {
		return err
	}
	for _, it := range intents {
		fmt.Fprintf(stdout, "%s%s%s\n", it.Project, sweepFieldSep, it.SessionID)
	}
	return nil
}

// runListOutstanding prints the (project, session) pairs the store still
// considers uncovered — the same events x derivation_runs computation the
// admin HTTP surface exposes (Q2 ruling: no coverage table). The HL5
// early-fire trigger intersects its sweep-log candidates with this list so
// stale markers for already-derived legs can never fire a wake (memoire:D163
// MED-2 residual, closed by memoire:D165 R-10.3).
func runListOutstanding(stdout io.Writer, opts options) error {
	if opts.sinceHours < 1 || opts.sinceHours > 720 {
		return errors.New("-since-hours must be in [1, 720]")
	}
	store, closeStore, err := openStore()
	if err != nil {
		return err
	}
	defer closeStore()
	since := time.Now().Add(-time.Duration(opts.sinceHours) * time.Hour).Unix()
	work, err := store.OutstandingDeriveWork(context.Background(), since, modelderive.CanonicalInputEventTypes())
	if err != nil {
		return err
	}
	for _, w := range work {
		fmt.Fprintf(stdout, "%s%s%s\n", w.Project, sweepFieldSep, w.SessionID)
	}
	return nil
}

// runSettleIntents applies the no-work expiry (round-1 MED-4 settlement
// policy): open intents older than the TTL whose session holds no live
// canonical events close as 'expired:no_events'. Prints the settled count.
func runSettleIntents(stdout io.Writer, opts options) error {
	if opts.intentTTLHours <= 0 {
		return errors.New("-intent-ttl-hours must be positive")
	}
	store, closeStore, err := openStore()
	if err != nil {
		return err
	}
	defer closeStore()
	now := time.Now().Unix()
	before := now - int64(opts.intentTTLHours)*3600
	n, err := store.SettleNoWorkIntents(context.Background(), before, now, modelderive.CanonicalInputEventTypes())
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "settled=%d\n", n)
	return nil
}

// runMedianEpoch prints the adaptive-split point for an oversized window:
// the median canonical-input event epoch inside [-start-epoch, -end-epoch].
// Empty output (exit 0) means the window holds no canonical events — the
// sweep treats that as "unsplittable". The canonical type list comes from
// the prompt builder itself, so the split always sees exactly the events
// the builder admits (this used to be a keep-in-sync SQL literal in the
// sweep script).
func runMedianEpoch(stdout io.Writer, opts options) error {
	if opts.project == "" || opts.sessionID == "" {
		return errors.New("-median-epoch requires -project and -session-id")
	}
	if opts.startEpoch <= 0 || opts.endEpoch <= 0 || opts.endEpoch < opts.startEpoch {
		return errors.New("-median-epoch requires a valid [-start-epoch, -end-epoch] window")
	}
	store, closeStore, err := openStore()
	if err != nil {
		return err
	}
	defer closeStore()
	epoch, ok, err := store.MedianCanonicalEpoch(context.Background(),
		opts.project, opts.sessionID, opts.startEpoch, opts.endEpoch,
		modelderive.CanonicalInputEventTypes())
	if err != nil {
		return err
	}
	if ok {
		fmt.Fprintf(stdout, "%d\n", epoch)
	}
	return nil
}

// runCutPoints prints the session's RHYTHM cut epochs, one per line — the
// sweep's first splitter, ahead of the byte-ceiling halving that -median-epoch
// serves (Fibery #343). Empty output (exit 0) means the window needs no cut and
// derives whole, exactly the contract -median-epoch already has.
//
// The detector lives here rather than in the sweep script because memoire:D162
// put every derive-lane store read behind this binary's helper flags, and
// because the population it must walk is the one the prompt builder admits:
// the window's live, UNCOVERED, canonical-input events, fetched with the very
// same filter the dry-run builds (memoire:D211 ExcludeCovered — on a live
// session the stream the model sees is the uncovered stream, so that is the
// stream whose rhythm matters). Reusing ListEventsForDerivation is also what
// lets the floor pass consult the real min-content guard: the guard measures
// per-event extracted TEXT, which no epochs-only query could express, and a
// second implementation of that floor would drift.
//
// -no-min-content-guard is honored so the detector's floor and the
// orchestrator's guard are never in disagreement.
func runCutPoints(stdout io.Writer, opts options) error {
	if opts.project == "" || opts.sessionID == "" {
		return errors.New("-cut-points requires -project and -session-id")
	}
	if opts.cutGapSeconds < 0 || opts.cutMaxEvents < 0 || opts.cutMaxSegments < 0 {
		return errors.New("-cut-gap-seconds, -cut-max-events and -cut-max-segments must not be negative")
	}
	if opts.endEpoch > 0 && opts.startEpoch > 0 && opts.endEpoch < opts.startEpoch {
		return errors.New("-cut-points requires -end-epoch >= -start-epoch when both are pinned")
	}
	policy := cutPolicy{
		GapSeconds:  opts.cutGapSeconds,
		MaxEvents:   opts.cutMaxEvents,
		MaxSegments: opts.cutMaxSegments,
		Guard:       modelderive.WindowGuard{Disabled: opts.noMinContentGuard},
	}
	// Both classes disabled is a legal "rhythm cuts off" configuration and must
	// not cost a store open.
	if policy.GapSeconds <= 0 && policy.MaxEvents <= 0 {
		return nil
	}
	store, closeStore, err := openStore()
	if err != nil {
		return err
	}
	defer closeStore()
	events, err := store.ListEventsForDerivation(context.Background(), db.DerivationEventFilter{
		Project:          opts.project,
		SessionID:        opts.sessionID,
		WindowStartEpoch: opts.startEpoch,
		WindowEndEpoch:   opts.endEpoch,
		EventTypes:       modelderive.CanonicalInputEventTypes(),
		ExcludeCovered:   true,
	})
	if err != nil {
		return err
	}
	for _, epoch := range computeCutPoints(events, policy) {
		fmt.Fprintf(stdout, "%d\n", epoch)
	}
	return nil
}

func openStore() (*db.Store, func(), error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return nil, nil, err
	}
	store, err := db.Open(cfg.DBPath, cfg.SchemaDir)
	if err != nil {
		return nil, nil, err
	}
	return store, func() { store.Close() }, nil
}

func writeJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(w, string(b))
	return nil
}

// isModelProvider reports whether a provider id is a contracted model provider
// for -strategy=model (docs/MODEL-PROVIDER.md "Provider Selection"). "rules" and
// "manual" are derivation sources but not model providers, so they are rejected
// on the model path.
func isModelProvider(provider string) bool {
	switch provider {
	case "local-qwen", "openai", modelderive.ProviderBYOK:
		return true
	default:
		return false
	}
}

// isLocalProvider reports whether a model provider id is a local model family
// that requires an explicit endpoint id. Kept in sync with the modelderive
// provider boundary discipline; external providers (e.g. openai) return false.
// "manual" is intentionally absent: it is not a model provider and is already
// rejected by isModelProvider before this gate runs.
func isLocalProvider(provider string) bool {
	switch provider {
	case "local-qwen":
		return true
	default:
		return false
	}
}

// isReasoningEffort reports whether a -reasoning-effort value is one the Qwen3.8
// chat template accepts. Empty is valid and means "send no kwarg", leaving the
// server's own default in force. The value is rejected at preflight rather than
// forwarded blindly: an unknown level reaches the template as a silent no-op, so
// a typo would run at the wrong effort for a whole unattended sweep.
func isReasoningEffort(effort string) bool {
	switch effort {
	case "", "none", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

func previewString(s string) string {
	if len(s) <= promptPreviewBytes {
		return s
	}
	return s[:promptPreviewBytes]
}
