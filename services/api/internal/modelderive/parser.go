// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// rawCandidate is a single candidate as emitted by the model, after typed
// extraction from its raw JSON object. Both observation and unknown candidates
// share this wire shape; the kind selects validation.
//
// SourceEventIDsRaw holds the wire entries before id resolution: each may be a
// numeric event id or a source_event_id string (two canonical names for the
// same event — operator ruling 2026-07-11); resolveSourceEventIDs maps them
// onto SourceEventIDs.
type rawCandidate struct {
	Kind              string
	Text              string
	Confidence        *float64
	SourceEventIDs    []int64
	SourceEventIDsRaw []json.RawMessage
	Rationale         string
	Provenance        string
	NoiseCandidate    bool
	UnknownAnalysis   *rawUnknownAnalysis
}

type rawUnknownAnalysis struct {
	TriedKinds             []rawTriedKind `json:"tried_kinds"`
	ProposedKindHypothesis string         `json:"proposed_kind_hypothesis"`
}

type rawTriedKind struct {
	Kind   string `json:"kind"`
	WhyNot string `json:"why_not"`
}

// knownCandidateFields are the typed fields the parser understands. Any other
// object field on a candidate is captured into Extra rather than rejected
// (Human ruling 2026-06-13: lenient on structure, strict on semantics).
var knownCandidateFields = map[string]bool{
	"kind":             true,
	"text":             true,
	"confidence":       true,
	"source_event_ids": true,
	"rationale":        true,
	"provenance":       true,
	"noise_candidate":  true,
	"unknown_analysis": true,
}

// ParseResponse parses and validates a model response against the input window
// in req. It is lenient on structure and strict on semantics:
//
//   - Unknown JSON object fields (top-level envelope or per-candidate) are
//     captured into Extra, never rejected — a capture we can study beats a
//     thrown-away 200-with-no-value.
//   - Semantic fitness still gates whether a candidate becomes a stored
//     observation. A candidate that fails validation (bad/missing kind, empty
//     text, absent/non-numeric confidence, below-threshold confidence,
//     out-of-window or empty source ids, malformed unknown audit) is neither
//     silently kept nor vanished: it lands in RejectedCandidates with its Extra,
//     Text, and SourceEventIDs captured.
//   - A candidate reaches Observations only via validateObservation.
//
// A returned error means the envelope or observations array is truly
// unsalvageable (malformed top-level JSON, trailing data, missing/mismatched
// schema_version, missing observations array, or an observations value that is
// not a JSON array). Salvageable candidate-level failures never abort the parse;
// they become RejectedCandidates and the run is recorded as partial.
func ParseResponse(raw []byte, req Request) (ParsedResponse, error) {
	var out ParsedResponse

	// Local models often wrap the JSON envelope in a Markdown code fence despite the
	// no-prose-wrapper instruction (first-light 2026-06-13: HL3 qwen3.5 emits
	// ```json ... ```). Strip a single wrapping fence — lenient on structure; the
	// inner bytes are then parsed exactly as strictly as before.
	raw = stripCodeFence(raw)

	dec := json.NewDecoder(bytes.NewReader(raw))
	var env map[string]json.RawMessage
	if err := dec.Decode(&env); err != nil {
		return ParsedResponse{}, fmt.Errorf("modelderive: malformed response JSON: %w", err)
	}

	// Robust trailing-data rejection: a clean single-value document leaves the
	// decoder at EOF. Any further decodable value (or non-EOF error) is trailing
	// data. This replaces the weaker Decoder.More() check, which only reports
	// whether the current array/object has another element.
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return ParsedResponse{}, fmt.Errorf("modelderive: trailing data after response JSON")
		}
		return ParsedResponse{}, fmt.Errorf("modelderive: trailing data after response JSON: %w", err)
	}

	svRaw, ok := env["schema_version"]
	if !ok {
		return ParsedResponse{}, fmt.Errorf("modelderive: missing schema_version")
	}
	var sv string
	if err := json.Unmarshal(svRaw, &sv); err != nil {
		return ParsedResponse{}, fmt.Errorf("modelderive: schema_version not a string: %w", err)
	}
	if sv != SchemaVersion {
		return ParsedResponse{}, fmt.Errorf("modelderive: schema_version %q, want %q", sv, SchemaVersion)
	}

	obsRaw, ok := env["observations"]
	if !ok || len(obsRaw) == 0 {
		return ParsedResponse{}, fmt.Errorf("modelderive: missing observations array")
	}
	var candidates []json.RawMessage
	if err := json.Unmarshal(obsRaw, &candidates); err != nil {
		return ParsedResponse{}, fmt.Errorf("modelderive: malformed observations array: %w", err)
	}

	// Capture unknown top-level envelope fields as provenance.
	for key, val := range env {
		if key == "schema_version" || key == "observations" {
			continue
		}
		if out.Extra == nil {
			out.Extra = map[string]json.RawMessage{}
		}
		out.Extra[key] = val
	}

	// Allowed source event ids: only ids present in the request window. The
	// string map recognizes an event's source_event_id as its second canonical
	// name (operator ruling 2026-07-11): local models ground on the distinctive
	// string ids the prompt displays per event, and an exact match names the
	// same event as its numeric id does. A string shared by several in-window
	// events is ambiguous and never resolves.
	allowed := make(map[int64]bool, len(req.Events))
	bySourceEventID := make(map[string]int64, len(req.Events))
	ambiguousSourceEventID := map[string]bool{}
	for _, e := range req.Events {
		allowed[e.ID] = true
		if e.SourceEventID == "" {
			continue
		}
		if prev, ok := bySourceEventID[e.SourceEventID]; ok && prev != e.ID {
			ambiguousSourceEventID[e.SourceEventID] = true
			continue
		}
		bySourceEventID[e.SourceEventID] = e.ID
	}

	for i, msg := range candidates {
		c, extra, err := decodeCandidate(msg)
		if err != nil {
			// Type/shape failure on a known field: capture and reject, never abort.
			out.RejectedCandidates = append(out.RejectedCandidates, rejected(i, c, extra, RejectClassMalformed, err.Error()))
			continue
		}

		ids, class, err := resolveSourceEventIDs(c.SourceEventIDsRaw, bySourceEventID, ambiguousSourceEventID)
		c.SourceEventIDs = ids
		if err != nil {
			out.RejectedCandidates = append(out.RejectedCandidates, rejected(i, c, extra, class, err.Error()))
			continue
		}

		if c.Kind == UnknownKind {
			uc, class, err := validateUnknown(c, allowed)
			if err != nil {
				out.RejectedCandidates = append(out.RejectedCandidates, rejected(i, c, extra, class, err.Error()))
				continue
			}
			uc.Extra = extra
			out.UnknownCandidates = append(out.UnknownCandidates, uc)
			continue
		}

		// memoire:D136 R-1: model-derived "observed" is illegitimate — the
		// model never observes, it infers. Downgrade fail-open (never reject
		// on this) and count only candidates that actually get stored, so the
		// counter reads as "rows saved from mislabeling".
		downgraded := c.Provenance == "observed"
		if downgraded {
			c.Provenance = "inferred"
		}
		obs, drop, class, err := validateObservation(c, allowed)
		if err != nil {
			out.RejectedCandidates = append(out.RejectedCandidates, rejected(i, c, extra, class, err.Error()))
			continue
		}
		if drop {
			// Below-threshold confidence is not stored; it is recorded as rejected
			// for run-metadata audit, with extras preserved.
			out.RejectedCandidates = append(out.RejectedCandidates, rejected(i, c, extra, RejectClassBelowConfidence,
				fmt.Sprintf("confidence %.4f below MinConfidence %.2f", *c.Confidence, MinConfidence)))
			continue
		}
		if downgraded {
			out.ProvenanceDowngrades++
			obs.ProvenanceDowngraded = true
		}
		obs.Extra = extra
		out.Observations = append(out.Observations, obs)
	}

	return out, nil
}

// stripCodeFence removes a single wrapping Markdown code fence (```json ... ``` or
// ``` ... ```) that local models emit despite the no-prose-wrapper instruction.
// Conservative: only strips when the trimmed input opens with a fence; on any
// mismatch the original bytes flow through and the strict parser still rejects
// (and captures) them — no silent corruption.
func stripCodeFence(raw []byte) []byte {
	t := bytes.TrimSpace(raw)
	if !bytes.HasPrefix(t, []byte("```")) {
		return raw
	}
	nl := bytes.IndexByte(t, '\n')
	if nl < 0 {
		return raw
	}
	body := bytes.TrimSpace(t[nl+1:])
	if i := bytes.LastIndex(body, []byte("```")); i >= 0 {
		body = bytes.TrimSpace(body[:i])
	}
	return body
}

// resolveSourceEventIDs maps the wire-format source_event_ids entries onto
// numeric event ids (operator ruling 2026-07-11). Each entry is either a JSON
// number (used as-is; window membership is validated downstream) or a string
// that EXACTLY matches one in-window event's source_event_id (mapped to that
// event's numeric id). Anything else still rejects — an unmatched string is a
// grounding failure, an ambiguous string (shared by several in-window events)
// never resolves, and a non-number-non-string entry is malformed. This is not
// laundering imprecision: a source_event_id is the event's second canonical
// name, and exact-match recognition preserves the strict-parser philosophy.
// Successfully resolved prefix entries are returned even on failure so the
// rejection record keeps them for audit.
func resolveSourceEventIDs(raws []json.RawMessage, bySourceEventID map[string]int64, ambiguous map[string]bool) ([]int64, string, error) {
	ids := make([]int64, 0, len(raws))
	for _, raw := range raws {
		// JSON null unmarshals into a non-pointer int64 as a NO-OP (zero value),
		// which would misclassify it as an ungrounded id 0; it is neither a
		// number nor a string, so it is malformed (review repair 2026-07-11).
		if string(bytes.TrimSpace(raw)) == "null" {
			return ids, RejectClassMalformed, fmt.Errorf("source_event_ids entry null is neither a numeric event id nor a source_event_id string")
		}
		var id int64
		if err := json.Unmarshal(raw, &id); err == nil {
			ids = append(ids, id)
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ids, RejectClassMalformed, fmt.Errorf("source_event_ids entry %s is neither a numeric event id nor a source_event_id string", string(raw))
		}
		if ambiguous[s] {
			return ids, RejectClassUngrounded, fmt.Errorf("source_event_id string %q is ambiguous in the input window", s)
		}
		mapped, ok := bySourceEventID[s]
		if !ok {
			return ids, RejectClassUngrounded, fmt.Errorf("source_event_id string %q does not match any input-window event", s)
		}
		ids = append(ids, mapped)
	}
	return ids, "", nil
}

// rejected builds a RejectedCandidate, capturing whatever was parseable for
// audit (kind, text, source ids, unknown extras) alongside the machine-readable
// class and the human-readable reason.
func rejected(index int, c rawCandidate, extra map[string]json.RawMessage, class, reason string) RejectedCandidate {
	return RejectedCandidate{
		Index:          index,
		Kind:           c.Kind,
		Text:           c.Text,
		SourceEventIDs: c.SourceEventIDs,
		Class:          class,
		Reason:         reason,
		Extra:          extra,
	}
}

// decodeCandidate decodes one candidate raw object into a typed rawCandidate,
// capturing any non-known object fields into extra. Extras are captured in a
// first pass so they survive even when a known field fails type validation. A
// returned error means a known field had the wrong JSON type (a semantic/type
// failure routed to RejectedCandidate) or the candidate was not a JSON object.
func decodeCandidate(msg json.RawMessage) (rawCandidate, map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(msg, &fields); err != nil {
		return rawCandidate{}, nil, fmt.Errorf("candidate is not a JSON object: %w", err)
	}

	var extra map[string]json.RawMessage
	for key, val := range fields {
		if knownCandidateFields[key] {
			continue
		}
		if extra == nil {
			extra = map[string]json.RawMessage{}
		}
		extra[key] = val
	}

	var c rawCandidate
	typed := []struct {
		key    string
		assign func(json.RawMessage) error
	}{
		{"kind", func(v json.RawMessage) error { return json.Unmarshal(v, &c.Kind) }},
		{"text", func(v json.RawMessage) error { return json.Unmarshal(v, &c.Text) }},
		{"confidence", func(v json.RawMessage) error { return json.Unmarshal(v, &c.Confidence) }},
		{"source_event_ids", func(v json.RawMessage) error { return json.Unmarshal(v, &c.SourceEventIDsRaw) }},
		{"rationale", func(v json.RawMessage) error { return json.Unmarshal(v, &c.Rationale) }},
		{"provenance", func(v json.RawMessage) error { return json.Unmarshal(v, &c.Provenance) }},
		{"noise_candidate", func(v json.RawMessage) error { return json.Unmarshal(v, &c.NoiseCandidate) }},
		{"unknown_analysis", func(v json.RawMessage) error { return json.Unmarshal(v, &c.UnknownAnalysis) }},
	}
	for _, f := range typed {
		v, ok := fields[f.key]
		if !ok {
			continue
		}
		if err := f.assign(v); err != nil {
			return c, extra, fmt.Errorf("field %s: %w", f.key, err)
		}
	}
	return c, extra, nil
}

// allowedProvenanceValues are the provenance values the model may emit for
// model-derived observations. "user_confirmed" and "imported" are reserved
// for store/hook paths and must never appear in a model parse. "observed" is
// NOT here: a model never observes, it infers (memoire:D136 R-1) — the parse
// loop downgrades it to "inferred" fail-open before validation, so it can
// never reach a reject.
var allowedProvenanceValues = map[string]bool{
	"inferred":  true,
	"generated": true,
}

// validateObservation validates one sealed-kind candidate. On failure the
// returned class is the machine-readable rejection class for
// candidate_reason_counts; it is empty on success and on a confidence drop
// (the caller assigns RejectClassBelowConfidence there).
func validateObservation(c rawCandidate, allowed map[int64]bool) (CandidateObservation, bool, string, error) {
	if !IsSealedKind(c.Kind) {
		return CandidateObservation{}, false, RejectClassValidation, fmt.Errorf("unrecognized kind %q (not a sealed kind or unknown)", c.Kind)
	}
	if c.Text == "" {
		return CandidateObservation{}, false, RejectClassValidation, fmt.Errorf("empty text")
	}
	if c.Confidence == nil {
		return CandidateObservation{}, false, RejectClassValidation, fmt.Errorf("missing confidence")
	}
	conf := *c.Confidence
	if conf < 0.0 || conf > 1.0 {
		return CandidateObservation{}, false, RejectClassValidation, fmt.Errorf("confidence %.4f outside [0.0, 1.0]", conf)
	}
	if len(c.SourceEventIDs) == 0 {
		return CandidateObservation{}, false, RejectClassUngrounded, fmt.Errorf("empty source_event_ids")
	}
	for _, id := range c.SourceEventIDs {
		if !allowed[id] {
			return CandidateObservation{}, false, RejectClassUngrounded, fmt.Errorf("source event id %d not in input window", id)
		}
	}
	// Validate provenance: model-derived observations may only emit "inferred"
	// or "generated". Reserved values (user_confirmed, imported) are rejected
	// — they belong to store/hook paths. "observed" never reaches this check:
	// the parse loop already downgraded it to "inferred" (memoire:D136 R-1,
	// fail-open — derivation must not break on a provenance claim).
	if c.Provenance != "" && !allowedProvenanceValues[c.Provenance] {
		return CandidateObservation{}, false, RejectClassValidation, fmt.Errorf("provenance %q not allowed for model-derived observations (only inferred/generated)", c.Provenance)
	}
	// Valid; decide drop after structural validity so out-of-range/structural
	// errors are reported, not silently dropped.
	if conf < MinConfidence {
		return CandidateObservation{}, true, "", nil
	}
	return CandidateObservation{
		Kind:           c.Kind,
		Text:           c.Text,
		Confidence:     conf,
		SourceEventIDs: c.SourceEventIDs,
		Rationale:      c.Rationale,
		Provenance:     c.Provenance,
		NoiseCandidate: c.NoiseCandidate,
	}, false, "", nil
}

// validateUnknown validates one unknown-kind audit candidate. On failure the
// returned class distinguishes grounding failures from audit-structure
// failures; it is empty on success.
func validateUnknown(c rawCandidate, allowed map[int64]bool) (UnknownCandidate, string, error) {
	if c.Text == "" {
		return UnknownCandidate{}, RejectClassInvalidUnknown, fmt.Errorf("unknown candidate empty text")
	}
	if len(c.SourceEventIDs) == 0 {
		return UnknownCandidate{}, RejectClassUngrounded, fmt.Errorf("unknown candidate empty source_event_ids")
	}
	for _, id := range c.SourceEventIDs {
		if !allowed[id] {
			return UnknownCandidate{}, RejectClassUngrounded, fmt.Errorf("unknown candidate source event id %d not in input window", id)
		}
	}
	if c.UnknownAnalysis == nil {
		return UnknownCandidate{}, RejectClassInvalidUnknown, fmt.Errorf("unknown candidate missing unknown_analysis")
	}
	tried := c.UnknownAnalysis.TriedKinds
	if len(tried) < 3 {
		return UnknownCandidate{}, RejectClassInvalidUnknown, fmt.Errorf("unknown candidate tried_kinds has %d entries, need at least 3", len(tried))
	}
	for _, t := range tried {
		if !IsSealedKind(t.Kind) {
			return UnknownCandidate{}, RejectClassInvalidUnknown, fmt.Errorf("unknown candidate tried_kinds kind %q is not sealed", t.Kind)
		}
		if t.WhyNot == "" {
			return UnknownCandidate{}, RejectClassInvalidUnknown, fmt.Errorf("unknown candidate tried_kinds why_not empty for kind %q", t.Kind)
		}
	}
	if len([]rune(c.UnknownAnalysis.ProposedKindHypothesis)) < 40 {
		return UnknownCandidate{}, RejectClassInvalidUnknown, fmt.Errorf("unknown candidate proposed_kind_hypothesis under 40 characters")
	}

	outTried := make([]TriedKind, 0, len(tried))
	for _, t := range tried {
		outTried = append(outTried, TriedKind{Kind: t.Kind, WhyNot: t.WhyNot})
	}
	return UnknownCandidate{
		Text:                   c.Text,
		SourceEventIDs:         c.SourceEventIDs,
		TriedKinds:             outTried,
		ProposedKindHypothesis: c.UnknownAnalysis.ProposedKindHypothesis,
	}, "", nil
}
