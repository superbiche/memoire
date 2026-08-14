// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package derive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"memoire/services/api/internal/contenthash"
	"memoire/services/api/internal/db"

	"golang.org/x/text/unicode/norm"
)

const (
	ProviderRules       = "rules"
	DataBoundaryLocal   = "local"
	DerivationVersionV1 = "observation-derive-v1"
	// DerivationVersionV2 (2026-07-11, turn-ingest campaign Phase 1): the model
	// path admits assistant-turn events (claude/codex/qwen_assistant_turn) as
	// canonical input. Input composition — and therefore every window's input
	// hash — changed, so the version bump documents the re-keying instead of
	// silently changing hashes. The rules strategy keeps V1: its input set is
	// unchanged.
	//
	// 2026-07-18 (memoire:D138, R005): the RULES input set expands with
	// `liaison_evidence`. The strategy stays observation-derive-v1 because no
	// pre-existing window's composition changes: the event type did not exist
	// before this landing (v1 echoes arrived as claude_UserPromptSubmit and
	// keep matching their historical windows), so every historical input hash
	// is byte-identical; only new liaison-evidence sessions produce windows
	// containing the new type. The model path is unaffected — it selects via
	// modelderive.CanonicalInputEventTypes(), which excludes liaison_evidence.
	DerivationVersionV2       = "observation-derive-v2"
	PromptVersionEmpty        = ""
	ConfidenceSourceRuleFixed = "rule_fixed"
)

type Window struct {
	Project    string
	SessionID  string
	StartEpoch int64
	EndEpoch   int64
}

type Rule struct {
	ID              string
	Version         string
	Name            string
	InputEventTypes []string
	OutputKind      string
	DefaultState    string
	Confidence      float64
	Apply           func(WindowEvent) []CandidateObservation
}

type WindowEvent struct {
	Event  db.EventRecord
	Prompt string
}

type CandidateObservation struct {
	RuleID      string
	RuleVersion string
	Kind        string
	Text        string
	ReviewState string
	Confidence  float64
}

type AppliedObservation struct {
	ObservationID          int64
	RuleID                 string
	RuleVersion            string
	Kind                   string
	Text                   string
	ReviewState            string
	Confidence             float64
	DowngradedFromAccepted bool
	ContentHash            string
	SourceEventIDs         []int64
}

type Result struct {
	Run              db.DerivationRunResult
	Window           Window
	SourceEventCount int
	CandidateCount   int
	// ReplayedCount counts candidates SKIPPED because a prior failed
	// attempt of this run already committed them (round-6 MED-NEW-1
	// replay idempotency).
	ReplayedCount int
	Applied       []AppliedObservation
	// FullyCovered marks a clean no-op: every canonical event in the
	// window already sits in a completed run's input manifest. No run row
	// is written and all other fields are zero.
	FullyCovered bool
}

type rulesStore interface {
	ListEventsForDerivation(context.Context, db.DerivationEventFilter) ([]db.EventRecord, error)
	ListEventsForDerivationCoverage(context.Context, db.DerivationEventFilter) (int, []db.EventRecord, error)
	EnsureDerivationRun(context.Context, db.DerivationRunInput) (db.DerivationRunResult, error)
	RecordRunInputEvents(context.Context, string, []int64) error
	RunObservationContentHashes(context.Context, string) (map[string]int, error)
	InsertDerivedObservation(context.Context, db.DerivedObservationInput) (db.DerivedObservationResult, error)
	UpdateDerivationRunStatus(context.Context, db.DerivationRunStatusUpdate) error
}

type inputHashTuple struct {
	EventID         int64           `json:"event_id"`
	IdempotencyKey  string          `json:"idempotency_key"`
	SourceAdapter   string          `json:"source_adapter"`
	SourceEventID   string          `json:"source_event_id,omitempty"`
	EventType       string          `json:"event_type"`
	OccurredAtEpoch int64           `json:"occurred_at_epoch"`
	Payload         json.RawMessage `json:"payload"`
}

type sourceWindow struct {
	Project    string   `json:"project"`
	SessionID  string   `json:"session_id,omitempty"`
	StartEpoch int64    `json:"start_epoch"`
	EndEpoch   int64    `json:"end_epoch"`
	EventTypes []string `json:"event_types"`
}

type derivationMetadata struct {
	Version          string `json:"version"`
	Provider         string `json:"provider"`
	DataBoundary     string `json:"data_boundary"`
	PromptVersion    string `json:"prompt_version"`
	ConfidenceSource string `json:"confidence_source"`
	InputHash        string `json:"input_hash"`
}

type provenanceMetadata struct {
	Rule struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	} `json:"rule"`
	ConfidenceSource string             `json:"confidence_source"`
	SourceEventIDs   []int64            `json:"source_event_ids"`
	SourceEventKeys  []string           `json:"source_event_keys,omitempty"`
	SourceWindow     sourceWindow       `json:"source_window"`
	Derivation       derivationMetadata `json:"derivation"`
}

func RunRules(ctx context.Context, store rulesStore, window Window) (Result, error) {
	if err := validateWindow(window); err != nil {
		return Result{}, err
	}
	fetchFilter := db.DerivationEventFilter{
		Project:          window.Project,
		SessionID:        window.SessionID,
		WindowStartEpoch: window.StartEpoch,
		WindowEndEpoch:   window.EndEpoch,
		EventTypes:       inputEventTypes(),
		// Rules runs complete and record manifests like model runs, so
		// they claim and must honor the same per-event coverage.
		ExcludeCovered: true,
	}
	events, err := store.ListEventsForDerivation(ctx, fetchFilter)
	if err != nil {
		return Result{}, err
	}
	if len(events) == 0 {
		// A window whose every event sits in a completed run's manifest is
		// a clean no-op, not an error (mirrors the model path's
		// fully_covered skip). The classification is ONE statement
		// (rounds 1-2, F1/R2-F2): serial probe + re-fetch pairs are
		// separate snapshots, so a late commit or soft-delete between
		// them yields a false skip. A raced commit is real work — derive
		// it.
		total, uncovered, cerr := store.ListEventsForDerivationCoverage(ctx, fetchFilter)
		if cerr != nil {
			return Result{}, cerr
		}
		if total == 0 {
			return Result{}, errors.New("no source events matched derivation window")
		}
		if len(uncovered) == 0 {
			return Result{Window: window, FullyCovered: true}, nil
		}
		events = uncovered
	}

	sort.SliceStable(events, func(i, j int) bool {
		if events[i].OccurredAtEpoch == events[j].OccurredAtEpoch {
			return events[i].ID < events[j].ID
		}
		return events[i].OccurredAtEpoch < events[j].OccurredAtEpoch
	})

	actualWindow := window
	if actualWindow.StartEpoch == 0 {
		actualWindow.StartEpoch = events[0].OccurredAtEpoch
	}
	if actualWindow.EndEpoch == 0 {
		actualWindow.EndEpoch = events[len(events)-1].OccurredAtEpoch
	}

	inputHash, err := CanonicalInputHash(events)
	if err != nil {
		return Result{}, err
	}
	runMeta, err := marshalCanonicalJSON(map[string]any{
		"source_window": buildSourceWindow(actualWindow),
		"derivation": derivationMetadata{
			Version:          DerivationVersionV1,
			Provider:         ProviderRules,
			DataBoundary:     DataBoundaryLocal,
			PromptVersion:    PromptVersionEmpty,
			ConfidenceSource: ConfidenceSourceRuleFixed,
			InputHash:        inputHash,
		},
	})
	if err != nil {
		return Result{}, err
	}
	// Non-terminal -> terminal lifecycle, mirroring the model path
	// (memoire:D165 round-5 HIGH-NEW-1: minting `completed` up front let a
	// transient failure leave a terminal row whose later dedup+manifest
	// upgrade would permanently false-cover inputs that were never
	// derived). The run is claimed `running`, the manifest recorded, ALL
	// candidate/observation work performed, and only then the terminal
	// completed update applied; any error marks the row failed (retryable
	// via Ensure's failed-retry claim).
	run, err := store.EnsureDerivationRun(ctx, db.DerivationRunInput{
		Project:           actualWindow.Project,
		SessionID:         actualWindow.SessionID,
		Provider:          ProviderRules,
		DataBoundary:      DataBoundaryLocal,
		DerivationVersion: DerivationVersionV1,
		PromptVersion:     PromptVersionEmpty,
		InputHash:         inputHash,
		WindowStartEpoch:  actualWindow.StartEpoch,
		WindowEndEpoch:    actualWindow.EndEpoch,
		Status:            "running",
		Metadata:          runMeta,
	})
	if err != nil {
		return Result{}, err
	}
	eventIDs := make([]int64, 0, len(events))
	for _, ev := range events {
		eventIDs = append(eventIDs, ev.ID)
	}
	result := Result{
		Run:              run,
		Window:           actualWindow,
		SourceEventCount: len(events),
	}
	if run.Duplicate {
		// Only a genuinely TERMINAL completed duplicate short-circuits —
		// and upgrades a pre-0015 row with its manifest best-effort (the
		// identical input_hash proves set equality). A non-terminal
		// duplicate (concurrent worker, not-yet-stale crash residue) is
		// left to Ensure's failed/stale retry machinery.
		if run.Status == "completed" {
			_ = store.RecordRunInputEvents(ctx, run.ID, eventIDs)
		}
		return result, nil
	}

	failRun := func(cause error) (Result, error) {
		_ = store.UpdateDerivationRunStatus(ctx, db.DerivationRunStatusUpdate{
			ID: run.ID, Status: "failed", CompletedAtEpoch: time.Now().Unix(),
		})
		return Result{}, cause
	}
	// EVERY completed-run writer records its exact input BEFORE going
	// terminal (round-4 HIGH-1); a failure here leaves a retryable failed
	// row, never a covered-but-underived one.
	if err := store.RecordRunInputEvents(ctx, run.ID, eventIDs); err != nil {
		return failRun(fmt.Errorf("record run input manifest: %w", err))
	}

	candidates, err := deriveCandidates(events)
	if err != nil {
		return failRun(err)
	}
	result.CandidateCount = len(candidates)
	// Replay idempotency (rounds 6-7): observations commit one at a time,
	// so a failed attempt may have landed a prefix of this exact candidate
	// set (same run id — the run key is idempotent over the same input).
	// The committed MULTISET is consumed one occurrence per candidate in
	// the loop's deterministic order: same-content candidates from
	// distinct source events are separate rows by contract, so a boolean
	// set would silently drop the uncommitted twin.
	existingHashes, err := store.RunObservationContentHashes(ctx, run.ID)
	if err != nil {
		return failRun(fmt.Errorf("list prior attempt observations: %w", err))
	}
	for _, candidate := range candidates {
		contentHash, err := CanonicalContentHash(actualWindow.Project, candidate.Kind, candidate.Text)
		if err != nil {
			return failRun(err)
		}
		if existingHashes[contentHash] > 0 {
			existingHashes[contentHash]--
			result.ReplayedCount++
			continue
		}
		meta, err := buildProvenance(candidate, actualWindow, inputHash)
		if err != nil {
			return failRun(err)
		}
		inserted, err := store.InsertDerivedObservation(ctx, db.DerivedObservationInput{
			Project:         actualWindow.Project,
			SessionID:       actualWindow.SessionID,
			Kind:            candidate.Kind,
			Text:            candidate.Text,
			OccurredAtEpoch: candidate.Event.OccurredAtEpoch,
			Confidence:      ptr(candidate.Confidence),
			SourceAdapter:   candidate.Event.SourceAdapter,
			EventType:       candidate.Event.EventType,
			ReviewState:     candidate.ReviewState,
			DerivationRunID: run.ID,
			ContentHash:     contentHash,
			SourceEventIDs:  []int64{candidate.Event.ID},
			Metadata:        meta,
		})
		if err != nil {
			return failRun(err)
		}
		result.Applied = append(result.Applied, AppliedObservation{
			ObservationID:          inserted.ID,
			RuleID:                 candidate.RuleID,
			RuleVersion:            candidate.RuleVersion,
			Kind:                   candidate.Kind,
			Text:                   candidate.Text,
			ReviewState:            inserted.ReviewState,
			Confidence:             candidate.Confidence,
			DowngradedFromAccepted: inserted.DowngradedFromAccepted,
			ContentHash:            contentHash,
			SourceEventIDs:         []int64{candidate.Event.ID},
		})
	}
	// Terminal update LAST: coverage may trust this run only now that the
	// manifest and every observation write landed.
	if err := store.UpdateDerivationRunStatus(ctx, db.DerivationRunStatusUpdate{
		ID: run.ID, Status: "completed", CompletedAtEpoch: events[len(events)-1].OccurredAtEpoch,
	}); err != nil {
		return failRun(fmt.Errorf("complete rules run: %w", err))
	}
	return result, nil
}

func CanonicalInputHash(events []db.EventRecord) (string, error) {
	tuples := make([]inputHashTuple, 0, len(events))
	for _, event := range events {
		payload, err := canonicalizeRawJSON(event.Payload)
		if err != nil {
			return "", fmt.Errorf("canonicalize payload for event %d: %w", event.ID, err)
		}
		tuples = append(tuples, inputHashTuple{
			EventID:         event.ID,
			IdempotencyKey:  event.IdempotencyKey,
			SourceAdapter:   event.SourceAdapter,
			SourceEventID:   event.SourceEventID,
			EventType:       event.EventType,
			OccurredAtEpoch: event.OccurredAtEpoch,
			Payload:         payload,
		})
	}
	sort.SliceStable(tuples, func(i, j int) bool {
		if tuples[i].OccurredAtEpoch == tuples[j].OccurredAtEpoch {
			return tuples[i].EventID < tuples[j].EventID
		}
		return tuples[i].OccurredAtEpoch < tuples[j].OccurredAtEpoch
	})
	b, err := marshalCanonicalJSON(tuples)
	if err != nil {
		return "", err
	}
	return "sha256:" + hashBytes(b), nil
}

// CanonicalContentHash delegates to internal/contenthash (extracted in
// slice E, memoire:D146, so the store can hash rewrite observations without
// an import cycle). The hash definition is unchanged.
func CanonicalContentHash(project, kind, text string) (string, error) {
	return contenthash.Canonical(project, kind, text)
}

func RulesV1() []Rule {
	return []Rule{
		{
			ID:              "R001",
			Version:         "1",
			Name:            "Explicit Typed Memoire Marker",
			InputEventTypes: []string{"claude_UserPromptSubmit"},
			OutputKind:      "typed",
			DefaultState:    "accepted",
			Confidence:      1.0,
			Apply:           applyTypedMarkerRule,
		},
		{
			ID:              "R002",
			Version:         "1",
			Name:            "Explicit Task Marker",
			InputEventTypes: []string{"claude_UserPromptSubmit"},
			OutputKind:      "task",
			DefaultState:    "accepted",
			Confidence:      1.0,
			Apply:           applyTaskMarkerRule,
		},
		{
			ID:              "R003",
			Version:         "1",
			Name:            "Explicit Open Question Marker",
			InputEventTypes: []string{"claude_UserPromptSubmit"},
			OutputKind:      "open_question",
			DefaultState:    "proposed",
			Confidence:      1.0,
			Apply:           applyOpenQuestionRule,
		},
		{
			ID:              "R004",
			Version:         "1",
			Name:            "Session Re-Entry Prompt",
			InputEventTypes: []string{"claude_UserPromptSubmit"},
			OutputKind:      "task",
			DefaultState:    "proposed",
			Confidence:      0.75,
			Apply:           applyResumePromptRule,
		},
		{
			ID:              "R005",
			Version:         "1",
			Name:            "Liaison Evidence Ingest",
			InputEventTypes: []string{"liaison_evidence"},
			OutputKind:      "fact",
			DefaultState:    "accepted",
			Confidence:      1.0,
			Apply:           applyLiaisonEvidenceRule,
		},
	}
}

type candidateWithEvent struct {
	Event db.EventRecord
	CandidateObservation
}

func deriveCandidates(events []db.EventRecord) ([]candidateWithEvent, error) {
	var out []candidateWithEvent
	for _, event := range events {
		// R005 events carry the evidence envelope as event text — no prompt
		// semantics, so they never enter the marker-rule loop below.
		if cands := applyLiaisonEvidenceRule(WindowEvent{Event: event}); len(cands) > 0 {
			for _, c := range cands {
				out = append(out, candidateWithEvent{Event: event, CandidateObservation: c})
			}
			continue
		}
		prompt, ok := claudeUserPromptText(event)
		if !ok {
			continue
		}
		windowEvent := WindowEvent{
			Event:  event,
			Prompt: prompt,
		}
		rules := RulesV1()
		explicitTask := false
		var resumeCandidate *CandidateObservation
		for _, rule := range rules {
			candidates := rule.Apply(windowEvent)
			for _, candidate := range candidates {
				if candidate.Kind == "task" && (candidate.RuleID == "R001" || candidate.RuleID == "R002") {
					explicitTask = true
				}
				if candidate.RuleID == "R004" {
					c := candidate
					resumeCandidate = &c
					continue
				}
				out = append(out, candidateWithEvent{Event: event, CandidateObservation: candidate})
			}
		}
		if resumeCandidate != nil && !explicitTask {
			out = append(out, candidateWithEvent{Event: event, CandidateObservation: *resumeCandidate})
		}
	}
	return out, nil
}

func applyTypedMarkerRule(event WindowEvent) []CandidateObservation {
	type marker struct {
		prefix string
		kind   string
		state  string
	}
	markers := []marker{
		{prefix: "Memoire decision:", kind: "decision", state: "accepted"},
		{prefix: "Memoire preference:", kind: "preference", state: "accepted"},
		{prefix: "Memoire constraint:", kind: "constraint", state: "accepted"},
		{prefix: "Memoire task:", kind: "task", state: "accepted"},
		{prefix: "Memoire fact:", kind: "fact", state: "accepted"},
		{prefix: "Memoire gotcha:", kind: "gotcha", state: "accepted"},
		{prefix: "Memoire open_question:", kind: "open_question", state: "proposed"},
	}
	lines := visiblePromptLines(event.Prompt)
	var out []CandidateObservation
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		for _, marker := range markers {
			if !strings.HasPrefix(trimmed, marker.prefix) {
				continue
			}
			body := strings.TrimSpace(strings.TrimPrefix(trimmed, marker.prefix))
			if body == "" {
				continue
			}
			out = append(out, CandidateObservation{
				RuleID:      "R001",
				RuleVersion: "1",
				Kind:        marker.kind,
				Text:        body,
				ReviewState: marker.state,
				Confidence:  1.0,
			})
			break
		}
	}
	return out
}

func applyTaskMarkerRule(event WindowEvent) []CandidateObservation {
	markers := []string{"TODO:", "Next:", "Follow up:"}
	lines := visiblePromptLines(event.Prompt)
	var out []CandidateObservation
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		for _, marker := range markers {
			if !strings.HasPrefix(trimmed, marker) {
				continue
			}
			body := strings.TrimSpace(strings.TrimPrefix(trimmed, marker))
			if body == "" {
				continue
			}
			out = append(out, CandidateObservation{
				RuleID:      "R002",
				RuleVersion: "1",
				Kind:        "task",
				Text:        body,
				ReviewState: "accepted",
				Confidence:  1.0,
			})
			break
		}
	}
	return out
}

func applyOpenQuestionRule(event WindowEvent) []CandidateObservation {
	markers := []string{"Open question:", "Unresolved:"}
	lines := visiblePromptLines(event.Prompt)
	var out []CandidateObservation
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		for _, marker := range markers {
			if !strings.HasPrefix(trimmed, marker) {
				continue
			}
			body := strings.TrimSpace(strings.TrimPrefix(trimmed, marker))
			if body == "" {
				continue
			}
			out = append(out, CandidateObservation{
				RuleID:      "R003",
				RuleVersion: "1",
				Kind:        "open_question",
				Text:        body,
				ReviewState: "proposed",
				Confidence:  1.0,
			})
			break
		}
	}
	return out
}

func applyResumePromptRule(event WindowEvent) []CandidateObservation {
	prompt := strings.TrimSpace(event.Prompt)
	if !strings.HasPrefix(prompt, "Resume Memoire in ") {
		return nil
	}
	return []CandidateObservation{{
		RuleID:      "R004",
		RuleVersion: "1",
		Kind:        "task",
		Text:        prompt,
		ReviewState: "proposed",
		Confidence:  0.75,
	}}
}

func visiblePromptLines(prompt string) []string {
	var lines []string
	inFence := false
	for _, rawLine := range strings.Split(prompt, "\n") {
		trimmed := strings.TrimLeft(rawLine, " \t")
		if isFenceLine(trimmed) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		lines = append(lines, rawLine)
	}
	return lines
}

func isFenceLine(line string) bool {
	return strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")
}

func claudeUserPromptText(event db.EventRecord) (string, bool) {
	if event.SourceAdapter != "claude-code" || !strings.EqualFold(event.EventType, "claude_UserPromptSubmit") {
		return "", false
	}
	prompt := payloadFieldString(event.Payload, "prompt", "user_prompt", "userPrompt", "message", "text")
	if prompt == "" {
		return "", true
	}
	return strings.TrimSpace(prompt), true
}

func payloadFieldString(payload json.RawMessage, keys ...string) string {
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

func inputEventTypes() []string {
	return []string{"claude_UserPromptSubmit", "claude_PostToolUse", "liaison_evidence"}
}

// applyLiaisonEvidenceRule (R005) ingests first-class liaison-evidence echo
// events (docs/LIAISON-EVIDENCE-CONTRACT.md): the D41 evidence envelope
// arrives as the event text, no prompt marker involved. Mirrors the retired
// R001 "Memoire fact:" echo behavior: kind=fact, accepted, confidence 1.0.
// A gated-out or malformed envelope yields NO candidate — the Liaison
// evidence store is authoritative (liaison:D218); a memory-side skip never
// affects graduation.
func applyLiaisonEvidenceRule(event WindowEvent) []CandidateObservation {
	if event.Event.SourceAdapter != "liaison-evidence" {
		return nil
	}
	if !strings.EqualFold(event.Event.EventType, "liaison_evidence") {
		return nil
	}
	text := strings.TrimSpace(event.Event.Text)
	if text == "" {
		return nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		return nil
	}
	if _, ok := envelope["liaison_evidence_observation"]; !ok {
		return nil
	}
	return []CandidateObservation{{
		RuleID:      "R005",
		RuleVersion: "1",
		Kind:        "fact",
		Text:        text,
		ReviewState: "accepted",
		Confidence:  1.0,
	}}
}

func buildProvenance(candidate candidateWithEvent, window Window, inputHash string) (json.RawMessage, error) {
	meta := provenanceMetadata{
		ConfidenceSource: ConfidenceSourceRuleFixed,
		SourceEventIDs:   []int64{candidate.Event.ID},
		SourceWindow:     buildSourceWindow(window),
		Derivation: derivationMetadata{
			Version:          DerivationVersionV1,
			Provider:         ProviderRules,
			DataBoundary:     DataBoundaryLocal,
			PromptVersion:    PromptVersionEmpty,
			ConfidenceSource: ConfidenceSourceRuleFixed,
			InputHash:        inputHash,
		},
	}
	meta.Rule.ID = candidate.RuleID
	meta.Rule.Version = candidate.RuleVersion
	if sourceKey := sourceEventKey(candidate.Event); sourceKey != "" {
		meta.SourceEventKeys = []string{sourceKey}
	}
	return marshalCanonicalJSON(meta)
}

func buildSourceWindow(window Window) sourceWindow {
	return sourceWindow{
		Project:    window.Project,
		SessionID:  window.SessionID,
		StartEpoch: window.StartEpoch,
		EndEpoch:   window.EndEpoch,
		EventTypes: inputEventTypes(),
	}
}

func sourceEventKey(event db.EventRecord) string {
	if event.SourceAdapter == "" || event.SourceEventID == "" {
		return ""
	}
	return event.SourceAdapter + ":" + event.SourceEventID
}

func normalizeContentText(text string) string {
	text = norm.NFC.String(text)
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(text))
	lastWasSpace := false
	for _, r := range text {
		if isASCIIWhitespace(r) {
			if lastWasSpace {
				continue
			}
			b.WriteByte(' ')
			lastWasSpace = true
			continue
		}
		lastWasSpace = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func isASCIIWhitespace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}

func canonicalizeRawJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var decoded any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		return nil, err
	}
	b, err := json.Marshal(decoded)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

func marshalCanonicalJSON(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func validateWindow(window Window) error {
	if strings.TrimSpace(window.Project) == "" {
		return errors.New("project is required")
	}
	if window.StartEpoch > 0 && window.EndEpoch > 0 && window.EndEpoch < window.StartEpoch {
		return errors.New("end_epoch must be greater than or equal to start_epoch")
	}
	return nil
}

func ptr(v float64) *float64 {
	return &v
}
