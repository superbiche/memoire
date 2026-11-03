// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package triage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"memoire/services/api/internal/db"
)

// Verdict is one judged row as the model emitted it.
type Verdict struct {
	ObservationID int64  `json:"observation_id"`
	Verdict       string `json:"verdict"`
	Genre         string `json:"genre,omitempty"`
	Reason        string `json:"reason,omitempty"`
	// Confidence is the judge's own confidence in this verdict, in [0,1].
	// A POINTER so "said nothing" stays distinct from "said zero": a judge
	// that omits the field has made no confidence claim, while an explicit
	// 0 is a verdict it is disclaiming. The de-noise doubt block reads only
	// present-and-low values, so collapsing the two would pin every silent
	// verdict into the doubt block.
	Confidence *float64 `json:"confidence,omitempty"`
}

type responseEnvelope struct {
	SchemaVersion string    `json:"schema_version"`
	Verdicts      []Verdict `json:"verdicts"`
}

// ParseResult carries what survived validation and what did not. Same
// posture as the derivation parser (memoire:D57): lenient on structure,
// strict on semantics, and a rejected item is CAPTURED with its reason
// rather than vanishing — a judge that keeps emitting one malformed shape
// is data about the prompt.
type ParseResult struct {
	Verdicts []Verdict
	Rejected []RejectedVerdict
}

// RejectedVerdict is a verdict that failed validation, kept for inspection.
type RejectedVerdict struct {
	Verdict Verdict
	Reason  string
}

// ParseResponse validates a judge response against the rows it was asked
// about. A malformed envelope is a hard error (nothing is salvageable); a
// malformed individual verdict is captured and skipped.
//
// scope is the set of observation ids the prompt carried. A verdict naming
// anything else is REJECTED, not stored: the judge answering about a row it
// was never shown is the hallucination shape that matters most here, since
// the suggestion would otherwise attach to a real row nobody judged.
func ParseResponse(raw []byte, stage int, scope map[int64]bool) (ParseResult, error) {
	dec := json.NewDecoder(bytes.NewReader(stripCodeFence(raw)))
	var env responseEnvelope
	if err := dec.Decode(&env); err != nil {
		return ParseResult{}, fmt.Errorf("triage: response envelope is not JSON: %w", err)
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ParseResult{}, errors.New("triage: response carries trailing data after the envelope")
	}
	if env.SchemaVersion != ResponseSchemaVersion {
		return ParseResult{}, fmt.Errorf("triage: response schema_version %q, want %q", env.SchemaVersion, ResponseSchemaVersion)
	}

	var out ParseResult
	seen := map[int64]bool{}
	for _, v := range env.Verdicts {
		v.Genre = strings.TrimSpace(v.Genre)
		v.Reason = strings.TrimSpace(v.Reason)
		// The genre is MODEL OUTPUT and travels into a rendered shell
		// command and a records/ filename. A non-conforming value is
		// DEGRADED to free text rather than rejected outright — the
		// judge's reasoning is still worth keeping, it just does not earn
		// promotion to a structured slug that later drives a gesture. A
		// reject's genre is the closed taxonomy and is validated by its
		// own rule below.
		if v.Genre != "" && v.Verdict != db.TriageVerdictReject && !db.ValidTriageGenre(v.Genre) {
			v.Reason = strings.TrimSpace(v.Genre + " " + v.Reason)
			v.Genre = ""
		}
		switch {
		case !scope[v.ObservationID]:
			out.Rejected = append(out.Rejected, RejectedVerdict{v, "observation_id was not in the prompt"})
			continue
		case seen[v.ObservationID]:
			out.Rejected = append(out.Rejected, RejectedVerdict{v, "duplicate verdict for one observation"})
			continue
		}
		// An out-of-range confidence is DROPPED, not rejected: the verdict
		// and its reason are still worth keeping, and a judge that cannot
		// keep a number in [0,1] has told us nothing about its certainty.
		// Dropping it lands the row in the doubt block's flag-vs-judge
		// branch rather than silently asserting a certainty nobody stated.
		if v.Confidence != nil && (*v.Confidence < 0 || *v.Confidence > 1) {
			v.Confidence = nil
		}
		probe := db.TriageSuggestionInput{
			ObservationID: v.ObservationID, Stage: stage, PromptVersion: promptVersionFor(stage),
			Model: "probe", SuggestedVerdict: v.Verdict, ReasonGenre: v.Genre, ReasonText: v.Reason,
			Arm: armProbe(stage, v.ObservationID), Confidence: v.Confidence,
		}
		if err := probe.Validate(); err != nil {
			out.Rejected = append(out.Rejected, RejectedVerdict{v, err.Error()})
			continue
		}
		seen[v.ObservationID] = true
		out.Verdicts = append(out.Verdicts, v)
	}
	return out, nil
}

func promptVersionFor(stage int) string {
	if stage == 2 {
		return PromptVersionStage2
	}
	return PromptVersionStage1
}

func armProbe(stage int, observationID int64) string {
	if stage == 2 {
		return db.TriageStage2Arm(observationID)
	}
	return ""
}

// stripCodeFence unwraps a single surrounding markdown fence, the same
// conservative allowance the derivation parser makes (memoire:D57 as
// extended by memoire:D80): on any mismatch the original bytes flow
// through and the strict path still rejects them.
func stripCodeFence(raw []byte) []byte {
	s := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(s, "```") {
		return raw
	}
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return raw
	}
	body := s[nl+1:]
	end := strings.LastIndex(body, "```")
	if end < 0 {
		return raw
	}
	return []byte(strings.TrimSpace(body[:end]))
}
