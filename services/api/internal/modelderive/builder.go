// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"memoire/services/api/internal/db"
)

// Canonical input event types. Anything outside this set is excluded from the
// prompt: derived observation text and audit rows are never primary input
// (docs/MODEL-PROVIDER.md "Prompt Construction").
//
// Assistant turns joined the set on 2026-07-11 (turn-ingest campaign Phase 1):
// admitting them changes every window's input composition — and therefore its
// canonical input hash — which is why the derivation version bumped to
// observation-derive-v2 at the same time.
var canonicalInputEventTypes = map[string]bool{
	"claude_UserPromptSubmit": true,
	"claude_PostToolUse":      true,
	"claude_assistant_turn":   true,
	"codex_assistant_turn":    true,
	"qwen_assistant_turn":     true,
	"opencode_assistant_turn": true,
}

// CanonicalInputEventTypes returns the canonical input event types as a sorted
// slice. It is the single source for the store fetch filters (orchestrator and
// dry-run) and run metadata, so the fetch filter can never drift from the
// prompt builder's admission set.
func CanonicalInputEventTypes() []string {
	out := make([]string, 0, len(canonicalInputEventTypes))
	for t := range canonicalInputEventTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// AssistantTurnLimitation is the v1 limitation statement included verbatim in
// the prompt (docs/MODEL-PROVIDER.md "Input Windows").
const AssistantTurnLimitation = "Assistant turns ARE included when present: events with event_type `claude_assistant_turn` carry the assistant's visible prose (live-captured or transcript-backfilled) and are first-class evidence — often the richest source of conclusions, verdicts, and results. Windows may still be prompt+tool-only when no assistant capture exists. Never invent content beyond what the events state; hidden chain-of-thought is never included."

// SecretHandlingGuidance is the secret-handling guidance included verbatim in
// the prompt. It is best-effort guidance, not a hard redaction claim
// (docs/MODEL-PROVIDER.md "Prompt Construction").
const SecretHandlingGuidance = "Treat any credential-shaped or secret-shaped content as opaque. Do not reproduce, infer, or reconstruct it in observations. Secret handling is enforced by the raw custody policy upstream, not by the model. Do not claim deletion/purge semantics."

// Already-captured context budget (prompt v2.4). The section must never let
// a long session's history crowd out the source events under the local-qwen
// 32,768-token ceiling: entries are admitted newest-first until either cap,
// and each entry's text is clipped to alreadyCapturedEntryBytes. The byte
// budget additionally YIELDS to the request's MaxPromptBytes headroom
// (review round-1 finding 2): a fixed allocation on top of a near-ceiling
// window turned a formerly derivable single-event window into a permanent
// unsplittable sweep loop — the context is advisory, so it shrinks to
// whatever the source window leaves, down to omitting itself entirely.
const (
	// DefaultAlreadyCapturedMaxEntries caps how many session observations
	// enter the prompt context.
	DefaultAlreadyCapturedMaxEntries = 100
	// DefaultAlreadyCapturedMaxBytes caps the RENDERED section bytes
	// (framing and instructions included).
	DefaultAlreadyCapturedMaxBytes = 16384
	// alreadyCapturedEntryBytes clips a single entry's text.
	alreadyCapturedEntryBytes = 300
)

// ErrPromptExceedsMaxBytes marks a BuildPrompt failure where the SOURCE
// WINDOW alone exceeds MaxPromptBytes (the advisory context has already
// yielded to zero by then). Callers that size windows (the CLI dry-run, and
// through it the sweep's split loop) match on this to report an honest base
// size instead of erroring.
var ErrPromptExceedsMaxBytes = errors.New("prompt exceeds MaxPromptBytes")

// alreadyCapturedIntro is the section preamble, a fixed part of the rendered
// budget.
const alreadyCapturedIntro = "## Already Captured (this session)\n\n" +
	"The observations below were already derived for this session — every " +
	"review state, including reviewed-away content. Do not re-emit them or " +
	"restatements/paraphrases of them; emit only genuinely NEW content from " +
	"the source events. An update that changes a result or status (a fix " +
	"landing, a verdict changing, a new concrete value) is new content; a " +
	"rewording is not.\n\n"

// alreadyCapturedBudget resolves the section's byte allowance: the package
// default, shrunk to the prompt-byte headroom the base prompt leaves. A
// non-positive result means the section is omitted.
func alreadyCapturedBudget(maxPromptBytes, basePromptLen int) int {
	budget := DefaultAlreadyCapturedMaxBytes
	if maxPromptBytes > 0 {
		if headroom := maxPromptBytes - basePromptLen; headroom < budget {
			budget = headroom
		}
	}
	return budget
}

// renderAlreadyCaptured renders the section within byteBudget, counting the
// FULL rendered bytes (intro + entry lines + trailing separator) so the
// yielded budget is exact against a hard MaxPromptBytes. Returns the section
// text (empty when nothing fits), how many entries it carries, and whether
// anything was dropped or clipped — callers record truncation in run
// metadata so a shrunken context is visible, never silent.
func renderAlreadyCaptured(list []db.SessionObservationSummary, byteBudget int) (section string, entries int, truncated bool) {
	if len(list) == 0 {
		return "", 0, false
	}
	const trailer = "\n"
	if byteBudget < len(alreadyCapturedIntro)+len(trailer) {
		return "", 0, true
	}
	var b strings.Builder
	b.WriteString(alreadyCapturedIntro)
	used := b.Len() + len(trailer)
	for _, obs := range list {
		if entries >= DefaultAlreadyCapturedMaxEntries {
			truncated = true
			break
		}
		text := obs.Text
		if len(text) > alreadyCapturedEntryBytes {
			text = clipUTF8(text, alreadyCapturedEntryBytes) + "…"
			truncated = true
		}
		line := fmt.Sprintf("- [%s/%s] %s\n", obs.Kind, obs.ReviewState, text)
		if used+len(line) > byteBudget {
			truncated = true
			break
		}
		b.WriteString(line)
		used += len(line)
		entries++
	}
	if entries == 0 {
		return "", 0, true
	}
	b.WriteString(trailer)
	return b.String(), entries, truncated
}

// clipUTF8 truncates s to at most n bytes without splitting a rune.
func clipUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// BuildPrompt constructs the deterministic model prompt and prompt metadata for
// derivation_runs.meta_json. It enforces the input-event and prompt-byte limits
// fail-closed before any provider call. It performs no provider call and no DB
// write.
func BuildPrompt(req Request) (string, map[string]any, error) {
	// Select canonical input events only; exclude derived observation text and
	// audit rows.
	events := make([]db.EventRecord, 0, len(req.Events))
	for _, e := range req.Events {
		if canonicalInputEventTypes[e.EventType] {
			events = append(events, e)
		}
	}

	// Order by (occurred_at_epoch, id).
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].OccurredAtEpoch == events[j].OccurredAtEpoch {
			return events[i].ID < events[j].ID
		}
		return events[i].OccurredAtEpoch < events[j].OccurredAtEpoch
	})

	// Fail closed on MaxInputEvents before building anything.
	if req.Limits.MaxInputEvents > 0 && len(events) > req.Limits.MaxInputEvents {
		return "", nil, fmt.Errorf("modelderive: input events %d exceed MaxInputEvents %d", len(events), req.Limits.MaxInputEvents)
	}

	// Two-pass render: the base prompt (no context section) fixes the
	// headroom, then the advisory section takes at most what remains. The
	// section counts its exact rendered bytes, so it can never push a
	// window that fits on its own past MaxPromptBytes.
	base := renderPrompt(req, events, "")
	section, sectionEntries, sectionTruncated := renderAlreadyCaptured(
		req.AlreadyCaptured, alreadyCapturedBudget(req.Limits.MaxPromptBytes, len(base)))
	prompt := base
	if section != "" {
		prompt = renderPrompt(req, events, section)
	}

	// Fail closed on MaxPromptBytes before returning the prompt. With the
	// yielding budget above, only the source window itself can trip this.
	// Typed so the dry-run can distinguish "the WINDOW is too big" (report
	// the base size for splitting) from other build failures.
	if req.Limits.MaxPromptBytes > 0 && len(prompt) > req.Limits.MaxPromptBytes {
		return "", nil, fmt.Errorf("modelderive: prompt %d bytes exceed MaxPromptBytes %d: %w", len(prompt), req.Limits.MaxPromptBytes, ErrPromptExceedsMaxBytes)
	}

	eventIDs := make([]int64, 0, len(events))
	for _, e := range events {
		eventIDs = append(eventIDs, e.ID)
	}

	meta := map[string]any{
		"provider":                req.Provider,
		"model":                   req.Model,
		"derivation_version":      req.DerivationVersion,
		"prompt_version":          req.PromptVersion,
		"output_schema":           req.OutputSchema,
		"response_schema_version": SchemaVersion,
		"window": map[string]any{
			"project":     req.Window.Project,
			"session_id":  req.Window.SessionID,
			"start_epoch": req.Window.StartEpoch,
			"end_epoch":   req.Window.EndEpoch,
			"event_types": CanonicalInputEventTypes(),
		},
		"input_event_count": len(events),
		"input_event_ids":   eventIDs,
		// The context the prompt actually carries, post-budget: available =
		// what the caller fetched; count = entries rendered; truncated =
		// anything dropped, clipped, or the whole section yielded away.
		"already_captured_count":     sectionEntries,
		"already_captured_available": len(req.AlreadyCaptured),
		"already_captured_truncated": sectionTruncated,
		"prompt_bytes":               len(prompt),
		"limits": map[string]any{
			"max_input_events":        req.Limits.MaxInputEvents,
			"max_prompt_bytes":        req.Limits.MaxPromptBytes,
			"max_output_observations": req.Limits.MaxOutputObservations,
		},
	}

	return prompt, meta, nil
}

func renderPrompt(req Request, events []db.EventRecord, alreadyCapturedSection string) string {
	var b strings.Builder

	b.WriteString("# Memoire Observation Derivation\n\n")
	b.WriteString("You derive durable memory observations from captured source events.\n\n")

	b.WriteString("## Assistant Turns\n\n")
	b.WriteString(AssistantTurnLimitation)
	b.WriteString("\n\n")

	b.WriteString("## Sealed Observation Kinds\n\n")
	b.WriteString("Pick kind only from the sealed set. Do not invent kinds.\n\n")
	for _, k := range SealedKinds() {
		b.WriteString("- `")
		b.WriteString(k.Name)
		b.WriteString("`: ")
		b.WriteString(k.Definition)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString("Prefer the best-fit kind from the seven. Disambiguation:\n")
	b.WriteString("- `gotcha`: a resolved, evidence-backed expectation violation with a concrete ")
	b.WriteString("cost or failure mode (what bit us, and why). Use it for surprise/cost shapes.\n")
	b.WriteString("- `fact`: a neutral, stable truth with no surprise or cost attached.\n")
	b.WriteString("- `constraint`: a prescriptive rule, boundary, or non-goal to obey going forward.\n")
	b.WriteString("- `open_question`: an unresolved question or fork, not a settled finding.\n\n")
	b.WriteString("Outcome extraction comes first: when the window contains final results — ")
	b.WriteString("judge verdicts, test/smoke outcomes, merges, run completions, approvals, ")
	b.WriteString("failures — state each as a `fact` (or `gotcha` when it violated expectations) ")
	b.WriteString("INCLUDING the concrete result value (e.g. \"J1 verdict PASS 8/8\", ")
	b.WriteString("\"4/4 smoke legs green\", \"merged as <sha>\"). Outcomes outrank process notes: ")
	b.WriteString("never emit a process observation (task created, tool invoked, monitoring set up) ")
	b.WriteString("when the same events yield an outcome observation.\n\n")
	b.WriteString("Consider `gotcha` before `unknown` whenever the candidate describes a ")
	b.WriteString("costly or unexpected outcome. Use `unknown` only when no sealed kind ")
	b.WriteString("plausibly applies, and only with full structural analysis: choosing `unknown` ")
	b.WriteString("requires explicit rejection rationale for at least three sealed kinds plus a ")
	b.WriteString("hypothesis for what kind would fit. Prefer no output over weak inference. ")
	b.WriteString("Do not summarize for its own sake.\n\n")

	b.WriteString("## Secret Handling\n\n")
	b.WriteString(SecretHandlingGuidance)
	b.WriteString("\n\n")

	b.WriteString("## Confidence Calibration\n\n")
	b.WriteString("- 0.9-1.0: directly supported by explicit source evidence.\n")
	b.WriteString("- 0.7-0.9: strong inference from clear source evidence.\n")
	b.WriteString("- 0.4-0.7: plausible but ambiguous; emit only if useful as proposed.\n")
	b.WriteString("- <0.4: do not emit.\n\n")

	b.WriteString("## Governance Fields\n\n")
	b.WriteString("Each observation may include governance metadata. The model must emit\n")
	b.WriteString("the fields below when they apply; omit fields that do not apply.\n\n")
	b.WriteString("### provenance\n\n")
	b.WriteString("Indicate how the observation was derived. Allowed values:\n")
	b.WriteString("- `inferred`: conclusions drawn from source evidence by the model.\n")
	b.WriteString("- `generated`: synthetic observations created by the model to fill gaps.\n")
	b.WriteString("Reserved values (never emit these): `observed` (a derivation model\n")
	b.WriteString("infers, it does not observe — an emitted `observed` is recorded as\n")
	b.WriteString("`inferred`), `user_confirmed`, `imported`.\n\n")
	// Noise genres spelled out (prompt v2.3, 2026-08-01): the one-line
	// "speculative or partial" definition left the classifier inert in
	// practice (10 flags across 57k proposed rows) and missed the
	// orchestration genre entirely — 1,264 unflagged "await completion of
	// lanes…"-shaped rows at audit time.
	b.WriteString("### noise_candidate\n\n")
	b.WriteString("Boolean flag. Set to `true` when the observation is speculative, partial,\n")
	b.WriteString("or likely noise (e.g., a weak inference that may need re-evaluation).\n")
	b.WriteString("Default is `false` (omit the field when false).\n\n")
	b.WriteString("Noise genres are STATUS-ONLY narration — they never include final\n")
	b.WriteString("results. A final result with its concrete value (a test/smoke verdict,\n")
	b.WriteString("a merge, a run completion or failure with what it produced) is an\n")
	b.WriteString("OUTCOME under \"Outcome extraction\" above: a durable `fact`/`gotcha`,\n")
	b.WriteString("never noise. The genres below cover the bookkeeping AROUND such\n")
	b.WriteString("outcomes — prefer not emitting them at all; any that still carries\n")
	b.WriteString("something worth recording must carry `\"noise_candidate\": true`:\n")
	b.WriteString("- orchestration and coordination chatter: lane/worker bookkeeping,\n")
	b.WriteString("  \"await/monitor completion of X\", subagent dispatch and status notes;\n")
	b.WriteString("- status-only run narration: progress updates and bare completion or\n")
	b.WriteString("  exit-code notices whose only content is that a step ran, carrying no\n")
	b.WriteString("  durable result value;\n")
	b.WriteString("- transient session mechanics: statements true only while the session\n")
	b.WriteString("  runs (what is currently open, running, or awaited), worthless as\n")
	b.WriteString("  durable memory once it ends.\n\n")

	b.WriteString("## Output Format\n\n")
	b.WriteString("Return strict JSON only, no prose wrapper, shaped as:\n\n")
	b.WriteString("```json\n")
	b.WriteString("{\n")
	b.WriteString("  \"schema_version\": \"" + SchemaVersion + "\",\n")
	b.WriteString("  \"observations\": [\n")
	b.WriteString("    {\n")
	b.WriteString("      \"kind\": \"decision\",\n")
	b.WriteString("      \"text\": \"...\",\n")
	b.WriteString("      \"confidence\": 0.86,\n")
	b.WriteString("      \"source_event_ids\": [123],\n")
	b.WriteString("      \"rationale\": \"...\",\n")
	b.WriteString("      \"provenance\": \"inferred\",\n")
	b.WriteString("      \"noise_candidate\": false\n")
	b.WriteString("    }\n")
	b.WriteString("  ]\n")
	b.WriteString("}\n")
	b.WriteString("```\n\n")
	b.WriteString("Emit only observations grounded in source event ids. `source_event_ids` must ")
	b.WriteString("be non-empty and every id must come from the source events below. ")
	// Mechanical disambiguation (prompt v2.1, 2026-07-11): each event block
	// shows two id namespaces (numeric header id + source_event_id string) and
	// local models grounded on the strings; the parser now maps exact string
	// matches, this clause reduces how often it has to.
	b.WriteString("`source_event_ids` entries must be the numeric event ids (e.g. 12345), ")
	b.WriteString("never the `source_event_id` strings.\n\n")

	// Already-captured context (prompt v2.4, 2026-08-02): post-D211 the
	// remaining paraphrase-dup vector is NEW events restating earlier session
	// content (recaps, progress notes) — coverage exclusion cannot see it, so
	// the model is told what already exists. The section arrives pre-rendered
	// and pre-budgeted (renderAlreadyCaptured) so its bytes are exact.
	b.WriteString(alreadyCapturedSection)

	b.WriteString("## Source Window\n\n")
	b.WriteString(fmt.Sprintf("project=%s session_id=%s start_epoch=%d end_epoch=%d\n\n",
		req.Window.Project, req.Window.SessionID, req.Window.StartEpoch, req.Window.EndEpoch))

	b.WriteString("## Source Events\n\n")
	if len(events) == 0 {
		b.WriteString("(no source events in window)\n")
	}
	for _, e := range events {
		writeEvent(&b, e)
	}

	return b.String()
}

func writeEvent(b *strings.Builder, e db.EventRecord) {
	b.WriteString(fmt.Sprintf("### event %d\n", e.ID))
	b.WriteString(fmt.Sprintf("- occurred_at_epoch: %d\n", e.OccurredAtEpoch))
	b.WriteString(fmt.Sprintf("- event_type: %s\n", e.EventType))
	b.WriteString(fmt.Sprintf("- source_adapter: %s\n", e.SourceAdapter))
	if e.SourceEventID != "" {
		b.WriteString(fmt.Sprintf("- source_event_id: %s\n", e.SourceEventID))
	}
	b.WriteString(fmt.Sprintf("- project: %s\n", e.Project))
	if e.SessionID != "" {
		b.WriteString(fmt.Sprintf("- session_id: %s\n", e.SessionID))
	}
	switch e.EventType {
	case "claude_UserPromptSubmit":
		if text := payloadString(e.Payload, "prompt", "user_prompt", "userPrompt", "message", "text"); text != "" {
			b.WriteString("- prompt: ")
			b.WriteString(strings.TrimSpace(text))
			b.WriteString("\n")
		}
	case "claude_PostToolUse":
		if tool := payloadString(e.Payload, "tool_name", "toolName", "tool"); tool != "" {
			b.WriteString("- tool: ")
			b.WriteString(tool)
			b.WriteString("\n")
		}
		if summary := payloadString(e.Payload, "result_summary", "resultSummary", "summary", "result"); summary != "" {
			b.WriteString("- result_summary: ")
			b.WriteString(strings.TrimSpace(summary))
			b.WriteString("\n")
		}
	case "claude_assistant_turn", "codex_assistant_turn", "qwen_assistant_turn", "opencode_assistant_turn":
		// Assistant turns carry their visible prose in the event text column
		// (live-captured or transcript-backfilled), not in the payload.
		if text := strings.TrimSpace(e.Text); text != "" {
			b.WriteString("- assistant_text: ")
			b.WriteString(text)
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
}

func payloadString(payload json.RawMessage, keys ...string) string {
	if len(payload) == 0 {
		return ""
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return ""
	}
	for _, key := range keys {
		if s, ok := decoded[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}
