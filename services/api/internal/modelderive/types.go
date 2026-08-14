// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Package modelderive implements deterministic preparation and strict
// validation for model-backed observation derivation runs.
//
// Slice 2 scope: prompt construction (no provider call) and strict response
// parsing/validation (no DB writes). Provider adapters, the registry, run
// orchestration, and store writes are out of scope and arrive in later slices.
//
// The sealed observation kind set and validation rules here follow the sealed
// contract docs docs/DERIVATION.md and docs/MODEL-PROVIDER.md, which win over
// the implementation brief if they disagree.
package modelderive

import (
	"encoding/json"

	"memoire/services/api/internal/db"
)

// SchemaVersion is the response schema version this parser supports. A
// behavior-changing response shape change increments this and requires a
// derivation version bump per docs/MODEL-PROVIDER.md.
const SchemaVersion = "1"

// MinConfidence is the inclusive lower bound below which model observations are
// dropped before any store insertion (docs/MODEL-PROVIDER.md "Review And
// Confidence").
const MinConfidence = 0.4

// UnknownKind is the reserved audit channel. It is never a write target; the
// store must reject it as an observation kind.
const UnknownKind = "unknown"

// SealedKind is one of the seven sealed v1 observation kinds.
type SealedKind struct {
	Name       string
	Definition string
}

// SealedKinds returns the seven sealed v1 observation kinds with their verbatim
// definitions, ordered for deterministic prompt output. Source of truth:
// docs/DERIVATION.md "Kind Ontology" and docs/MODEL-PROVIDER.md "Prompt
// Construction".
//
// gotcha is a pre-run v1 amendment (Human ruling 2026-06-13): adding it before
// any model observations exist needs no derivation_version bump and no schema
// migration (kind is unconstrained TEXT).
func SealedKinds() []SealedKind {
	return []SealedKind{
		{Name: "decision", Definition: "a settled technical, product, or sequencing decision."},
		{Name: "preference", Definition: "durable human/project preference."},
		{Name: "constraint", Definition: "rule, safety boundary, non-goal, or policy limit."},
		{Name: "task", Definition: "actionable follow-up or re-entry point."},
		{Name: "fact", Definition: "stable project/runtime fact."},
		{Name: "open_question", Definition: "unresolved question, design fork, uncertainty, or decision that still needs human/operator review."},
		{Name: "gotcha", Definition: "an expectation-violating, evidence-backed causal surprise with a concrete cost or failure mode (what bit us, and why)."},
	}
}

// IsSealedKind reports whether kind is one of the seven sealed v1 kinds. The
// reserved "unknown" audit channel is intentionally not a sealed kind.
func IsSealedKind(kind string) bool {
	for _, k := range SealedKinds() {
		if k.Name == kind {
			return true
		}
	}
	return false
}

// Window is the bounded source-event window a derivation run consumes.
type Window struct {
	Project    string
	SessionID  string
	StartEpoch int64
	EndEpoch   int64
}

// Limits are the hard cost/safety gates enforced before any provider call.
type Limits struct {
	MaxInputEvents        int
	MaxPromptBytes        int
	MaxOutputObservations int
}

// Request carries everything needed to build a model prompt and to validate the
// response against the input window.
//
// EndpointID is Slice 4 orchestration metadata: required for local providers
// (e.g. local-qwen on hl3) and empty for external providers. It is audit and
// inspection metadata only and is excluded from run idempotency per
// docs/MODEL-PROVIDER.md "Model Identity And Idempotency".
type Request struct {
	Provider          string
	Model             string
	EndpointID        string
	// Dialect is the byok wire protocol (openai-v1 | anthropic), recorded in
	// run metadata for audit. Like EndpointID it is transport identity, not
	// model identity, and is excluded from run idempotency
	// (docs/BYOK-DERIVE-DESIGN.md §2). Empty for every other provider.
	Dialect           string
	DerivationVersion string
	PromptVersion     string
	OutputSchema      string
	Window            Window
	Events            []db.EventRecord
	Limits            Limits
	// ForceRerun mints a new derivation run for a window that would otherwise
	// dedup (prompt/model A/B reruns; operator flag -force-rerun). Default
	// idempotency is unchanged when false.
	ForceRerun bool
	// AlreadyCaptured is the budgeted list of the session's existing
	// observations rendered into the prompt as do-not-re-emit context
	// (prompt v2.4). Advisory only: it is excluded from the canonical input
	// hash — run idempotency stays a function of the source events.
	AlreadyCaptured []db.SessionObservationSummary
}

// CandidateObservation is a validated, write-eligible observation candidate
// parsed from a model response. It is not yet a store row.
//
// Extra carries any unknown JSON object fields the model emitted on this
// candidate. It is provenance only and never validation input; it is persisted
// in observation meta_json under "model_response_extra".
type CandidateObservation struct {
	Kind           string                     `json:"kind"`
	Text           string                     `json:"text"`
	Confidence     float64                    `json:"confidence"`
	SourceEventIDs []int64                    `json:"source_event_ids"`
	Rationale      string                     `json:"rationale,omitempty"`
	Provenance     string                     `json:"provenance,omitempty"`
	NoiseCandidate bool                       `json:"noise_candidate,omitempty"`
	Extra          map[string]json.RawMessage `json:"extra,omitempty"`
	// ProvenanceDowngraded marks a candidate whose model-claimed "observed"
	// was rewritten to "inferred" (memoire:D136 R-1). Not serialized: it only
	// lets the output-cap enforcement keep ProvenanceDowngrades an honest
	// stored-row count when downgraded candidates overflow the cap.
	ProvenanceDowngraded bool `json:"-"`
}

// TriedKind records one sealed kind the model considered and rejected when
// proposing an unknown candidate.
type TriedKind struct {
	Kind   string `json:"kind"`
	WhyNot string `json:"why_not"`
}

// UnknownCandidate is a validated audit-only candidate for a genuine ontology
// gap. It is preserved for run metadata and never becomes an observation row.
type UnknownCandidate struct {
	Text                   string                     `json:"text"`
	SourceEventIDs         []int64                    `json:"source_event_ids"`
	TriedKinds             []TriedKind                `json:"tried_kinds"`
	ProposedKindHypothesis string                     `json:"proposed_kind_hypothesis"`
	Extra                  map[string]json.RawMessage `json:"extra,omitempty"`
}

// RejectedCandidate records a candidate (observation or unknown) that failed
// validation, with the per-candidate reason. Rejected candidates are preserved
// for run metadata; nothing is written. Text, SourceEventIDs, and Extra are
// captured for audit usefulness so a rejection is more than an index/reason.
// Index is the candidate's position in the observations array, or -1 for a
// candidate dropped after parsing (e.g. an output-cap overflow).
type RejectedCandidate struct {
	Index          int                        `json:"index"`
	Kind           string                     `json:"kind,omitempty"`
	Text           string                     `json:"text,omitempty"`
	SourceEventIDs []int64                    `json:"source_event_ids,omitempty"`
	Class          string                     `json:"class,omitempty"`
	Reason         string                     `json:"reason"`
	Extra          map[string]json.RawMessage `json:"extra,omitempty"`
}

// Rejection classes: machine-readable counterparts to RejectedCandidate.Reason.
// They are aggregated into run meta_json under candidate_reason_counts so a
// partial run with written=0 is explainable from run metadata alone.
const (
	// RejectClassMalformed: the candidate was not a JSON object or a known
	// field had the wrong JSON type.
	RejectClassMalformed = "malformed_candidate"
	// RejectClassValidation: semantic validation failed (bad kind, empty text,
	// missing/out-of-range confidence, disallowed provenance).
	RejectClassValidation = "validation"
	// RejectClassUngrounded: source_event_ids empty or referencing ids outside
	// the input window.
	RejectClassUngrounded = "ungrounded_source_ids"
	// RejectClassBelowConfidence: structurally valid but below MinConfidence.
	RejectClassBelowConfidence = "below_min_confidence"
	// RejectClassOutputCap: dropped by the MaxOutputObservations hard cap.
	RejectClassOutputCap = "output_cap"
	// RejectClassInvalidUnknown: an unknown-kind candidate whose audit
	// structure (tried_kinds, hypothesis) failed validation.
	RejectClassInvalidUnknown = "invalid_unknown"
	// RejectClassUnknownKind counts accepted unknown-kind audit candidates
	// (never written) in candidate_reason_counts.
	RejectClassUnknownKind = "unknown_kind"
	// RejectClassEmptyResponse marks a syntactically valid response that
	// carried zero candidates.
	RejectClassEmptyResponse = "empty_observations"
	// RejectClassNearDuplicate: a valid candidate rejected by the write-time
	// near-dup gate — a paraphrase of an existing session observation or of
	// an earlier candidate in the same response (capture-don't-discard: the
	// candidate text and its match land in run metadata, nothing is written).
	RejectClassNearDuplicate = "near_duplicate"
)

// ParsedResponse is the strict-parser result: write-eligible observations,
// audit-only unknown candidates, and rejected candidates with reasons. Extra
// carries unknown top-level envelope fields, persisted in run meta_json under
// "response_extra".
type ParsedResponse struct {
	Observations       []CandidateObservation     `json:"observations"`
	UnknownCandidates  []UnknownCandidate         `json:"unknown_candidates"`
	RejectedCandidates []RejectedCandidate        `json:"rejected_candidates"`
	Extra              map[string]json.RawMessage `json:"extra,omitempty"`
	// ProvenanceDowngrades counts stored candidates whose model-claimed
	// "observed" provenance was downgraded to "inferred" (memoire:D136 R-1).
	ProvenanceDowngrades int `json:"provenance_downgrades,omitempty"`
}
