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
	// pi_assistant_turn (2026-10-07, Pi live capture): a type that did not
	// exist before, so no historical window changes composition or hash and
	// the derivation version stays (the memoire:D138 precedent).
	"pi_assistant_turn": true,
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

// Collapsed content-less runs (prompt v2.5, 2026-09-11, Fibery #365).
//
// A canonical event whose eventContentText is empty renders as envelope and
// nothing else — roughly 336 bytes of ids, epoch, type and adapter carrying no
// text the model can ground on. No adapter in this repo produces a
// claude_PostToolUse result summary, so EVERY tool event is that shape, and
// the min-content guard (guard.go) measures it as zero content too: renderer
// and guard already agree, the renderer just spent the bytes anyway. A
// subagent tool burst could therefore push a rhythm segment (memoire:D261)
// past the byte ceiling while leaving both halves of the median split under
// the content floor — skipped as degenerate_window, no run row, no coverage,
// re-cut identically on every later tick.
//
// So a MAXIMAL RUN of consecutive content-less events renders as ONE compact
// block: the run's id and epoch span, its event-type counts, its tool-name
// counts. Nothing leaves the input: the collapsed ids stay in
// input_event_ids, so memoire:D211 coverage still covers them, and they stay
// citable through the parser's id maps (parser.go), which resolve against the
// request's events and never against the rendered prompt.
const (
	// collapsedToolNameLimit bounds how many tool names a collapsed block
	// names individually. The overflow folds into a single `other xN` entry
	// (N counts the folded EVENTS, not the folded names), so a pathological
	// run cannot grow the block without bound.
	collapsedToolNameLimit = 12
)

// collapsedIntro is the one-sentence Source Events preamble that tells the
// model what a collapsed block is. Fixed framing: it renders on every prompt,
// so the section shape does not depend on whether a window happens to hold a
// content-less run.
const collapsedIntro = "A `### events A..B` block summarizes a run of consecutive events with no captured content: counts only, nothing to cite.\n\n"

// ErrPromptExceedsMaxBytes marks a BuildPrompt failure where the SOURCE
// WINDOW alone exceeds MaxPromptBytes (the advisory context has already
// yielded to zero by then). Callers that size windows (the CLI dry-run, and
// through it the sweep's split loop) match on this to report an honest base
// size instead of erroring.
var ErrPromptExceedsMaxBytes = errors.New("prompt exceeds MaxPromptBytes")

// alreadyCapturedIntro is the section preamble, a fixed part of the rendered
// budget.
const alreadyCapturedIntro = "## Already Captured (this session)\n\n" +
	"The observations below were already derived for this session — accepted, " +
	"proposed and reviewed-away alike. Do not re-emit them or " +
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

// eventGroup is one unit of the Source Events section: either a single
// content-bearing event rendered in full, or a maximal run of consecutive
// content-less events rendered as one collapsed block.
type eventGroup struct {
	Collapsed bool
	Events    []db.EventRecord
}

// groupEvents walks the ordered events and folds every MAXIMAL run of
// consecutive content-less events into one collapsed group. A run of length 1
// collapses too — the rule is uniform, a content-less event never gets a
// per-event block. Content is measured with the guard's eventContentText
// (guard.go), never a second definition, so the renderer and the min-content
// guard can never disagree on what content is.
//
// The grouping is computed ONCE per BuildPrompt and handed to both render
// passes (the base pass and the already-captured pass), so the two passes
// cannot diverge and the prompt meta counts exactly what the prompt shows.
func groupEvents(events []db.EventRecord) []eventGroup {
	groups := make([]eventGroup, 0, len(events))
	for i := 0; i < len(events); i++ {
		if eventContentText(events[i]) != "" {
			groups = append(groups, eventGroup{Events: events[i : i+1]})
			continue
		}
		j := i + 1
		for j < len(events) && eventContentText(events[j]) == "" {
			j++
		}
		groups = append(groups, eventGroup{Collapsed: true, Events: events[i:j]})
		i = j - 1
	}
	return groups
}

// collapsedCounts reports how many events sit inside collapsed blocks and how
// many blocks there are, for the prompt meta.
func collapsedCounts(groups []eventGroup) (events, blocks int) {
	for _, g := range groups {
		if !g.Collapsed {
			continue
		}
		blocks++
		events += len(g.Events)
	}
	return events, blocks
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
	groups := groupEvents(events)
	collapsedEvents, collapsedBlocks := collapsedCounts(groups)

	base := renderPrompt(req, groups, "")
	section, sectionEntries, sectionTruncated := renderAlreadyCaptured(
		req.AlreadyCaptured, alreadyCapturedBudget(req.Limits.MaxPromptBytes, len(base)))
	prompt := base
	if section != "" {
		prompt = renderPrompt(req, groups, section)
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
		// The input manifest lists EVERY admitted canonical event, collapsed
		// ones included: coverage (memoire:D211) is a property of the input
		// set, not of how many bytes the prompt spent on it.
		"input_event_count": len(events),
		"input_event_ids":   eventIDs,
		// Collapse effect (prompt v2.5, Fibery #365): how many events the
		// prompt summarized instead of rendering per-event, in how many
		// blocks. Per-run numbers so the ceiling calibration can measure the
		// saving on real sweep legs.
		"collapsed_event_count": collapsedEvents,
		"collapsed_block_count": collapsedBlocks,
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

func renderPrompt(req Request, groups []eventGroup, alreadyCapturedSection string) string {
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
	// Graduated genres (triage lane v1, R-B): the narrow, evidence-gated
	// second death point. Renders nothing until a genre graduates, so the
	// prompt is byte-identical to the pre-triage one on a deployment that
	// has graduated none.
	renderGraduatedGenres(&b)

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
	b.WriteString(collapsedIntro)
	if len(groups) == 0 {
		b.WriteString("(no source events in window)\n")
	}
	for _, g := range groups {
		if g.Collapsed {
			writeCollapsedBlock(&b, g.Events)
			continue
		}
		writeEvent(&b, g.Events[0], req.Window)
	}

	return b.String()
}

// writeCollapsedBlock renders one maximal run of content-less events as a
// single block (prompt v2.5, Fibery #365). Deterministic by construction: the
// span comes from the run's ends in prompt order and both count lines are
// ordered by count desc then name asc.
func writeCollapsedBlock(b *strings.Builder, run []db.EventRecord) {
	first, last := run[0], run[len(run)-1]
	noun := "events"
	if len(run) == 1 {
		noun = "event"
	}
	b.WriteString(fmt.Sprintf("### events %d..%d (%d %s, no captured content)\n", first.ID, last.ID, len(run), noun))
	b.WriteString(fmt.Sprintf("- occurred_at_epoch: %d..%d\n", first.OccurredAtEpoch, last.OccurredAtEpoch))

	types := map[string]int{}
	tools := map[string]int{}
	for _, e := range run {
		types[e.EventType]++
		// Same key lookup writeEvent uses for the per-event `- tool:` line.
		if tool := payloadString(e.Payload, "tool_name", "toolName", "tool"); tool != "" {
			tools[tool]++
		}
	}
	// event_types is unbounded on purpose: the canonical set is closed and
	// small (builder.go canonicalInputEventTypes), so the line is short and
	// it tells the model what the run was.
	b.WriteString("- event_types: " + renderNameCounts(types, 0) + "\n")
	if len(tools) > 0 {
		b.WriteString("- tools: " + renderNameCounts(tools, collapsedToolNameLimit) + "\n")
	}
	b.WriteString("\n")
}

// renderNameCounts renders `name xN` pairs ordered by count desc then name
// asc — a total order, so the same run always renders the same bytes. A
// positive limit names at most that many and folds the rest into a single
// `other xN`, where N is the number of EVENTS folded, not the number of names.
func renderNameCounts(counts map[string]int, limit int) string {
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		return names[i] < names[j]
	})

	parts := make([]string, 0, len(names)+1)
	other := 0
	for i, name := range names {
		if limit > 0 && i >= limit {
			other += counts[name]
			continue
		}
		parts = append(parts, fmt.Sprintf("%s x%d", name, counts[name]))
	}
	if other > 0 {
		parts = append(parts, fmt.Sprintf("other x%d", other))
	}
	return strings.Join(parts, ", ")
}

// writeEvent renders one content-bearing event. The project and session_id
// lines are omitted when they repeat the window (prompt v2.5, Fibery #365):
// the `## Source Window` header already states both, and in time-window mode
// (Window.SessionID == "") the session line stays on every event because the
// header cannot state it. source_adapter stays, and source_event_id ALWAYS
// stays: the parser maps string citations through it (parser.go).
func writeEvent(b *strings.Builder, e db.EventRecord, w Window) {
	b.WriteString(fmt.Sprintf("### event %d\n", e.ID))
	b.WriteString(fmt.Sprintf("- occurred_at_epoch: %d\n", e.OccurredAtEpoch))
	b.WriteString(fmt.Sprintf("- event_type: %s\n", e.EventType))
	b.WriteString(fmt.Sprintf("- source_adapter: %s\n", e.SourceAdapter))
	if e.SourceEventID != "" {
		b.WriteString(fmt.Sprintf("- source_event_id: %s\n", e.SourceEventID))
	}
	if e.Project != w.Project {
		b.WriteString(fmt.Sprintf("- project: %s\n", e.Project))
	}
	if e.SessionID != "" && e.SessionID != w.SessionID {
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
	case "claude_assistant_turn", "codex_assistant_turn", "qwen_assistant_turn", "opencode_assistant_turn", "pi_assistant_turn":
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
