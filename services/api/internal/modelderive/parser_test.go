// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"strings"
	"testing"

	"memoire/services/api/internal/db"
)

func windowRequest(ids ...int64) Request {
	events := make([]db.EventRecord, 0, len(ids))
	for _, id := range ids {
		events = append(events, db.EventRecord{ID: id, Project: "memoire", EventType: "claude_UserPromptSubmit"})
	}
	return Request{
		Window: Window{Project: "memoire", SessionID: "session-1"},
		Events: events,
	}
}

func TestParserSchemaVersionMismatch(t *testing.T) {
	raw := []byte(`{"schema_version":"2","observations":[]}`)
	_, err := ParseResponse(raw, windowRequest(1))
	if err == nil {
		t.Fatalf("expected schema-version mismatch error")
	}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParserMalformedJSON(t *testing.T) {
	raw := []byte(`{"schema_version":"1", "observations": [`)
	_, err := ParseResponse(raw, windowRequest(1))
	if err == nil {
		t.Fatalf("expected malformed JSON error")
	}
}

func TestParserStripsMarkdownCodeFence(t *testing.T) {
	// First-light 2026-06-13: HL3 qwen3.5 wraps a valid envelope in a ```json fence
	// despite the no-prose-wrapper instruction. The fence must not discard the run.
	raw := []byte("```json\n{\"schema_version\":\"1\",\"observations\":[{\"kind\":\"fact\",\"text\":\"fenced but valid\",\"confidence\":0.9,\"source_event_ids\":[1]}]}\n```")
	out, err := ParseResponse(raw, windowRequest(1))
	if err != nil {
		t.Fatalf("fenced valid envelope should parse, got: %v", err)
	}
	if len(out.Observations) != 1 || out.Observations[0].Text != "fenced but valid" {
		t.Fatalf("expected 1 observation through the fence, got %+v", out.Observations)
	}
}

func TestStripCodeFence(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"json-fence", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"bare-fence", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"no-fence", `{"a":1}`, `{"a":1}`},
		{"no-close", "```json\n{\"a\":1}", `{"a":1}`},
	}
	for _, c := range cases {
		if got := string(stripCodeFence([]byte(c.in))); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestParserAcceptsEachSealedKind(t *testing.T) {
	for _, k := range SealedKinds() {
		raw := []byte(`{"schema_version":"1","observations":[{"kind":"` + k.Name + `","text":"a settled thing","confidence":0.9,"source_event_ids":[1]}]}`)
		got, err := ParseResponse(raw, windowRequest(1))
		if err != nil {
			t.Fatalf("kind %q: unexpected error: %v", k.Name, err)
		}
		if len(got.Observations) != 1 {
			t.Fatalf("kind %q: expected 1 observation, got %d (rejected=%v)", k.Name, len(got.Observations), got.RejectedCandidates)
		}
		if got.Observations[0].Kind != k.Name {
			t.Fatalf("kind %q: parsed kind %q", k.Name, got.Observations[0].Kind)
		}
	}
}

func TestParserRejectsUnknownAsWriteTarget(t *testing.T) {
	// A valid unknown candidate is preserved as audit data, never as a write
	// target observation.
	raw := []byte(`{"schema_version":"1","observations":[{"kind":"unknown","text":"genuine gap","source_event_ids":[1],"unknown_analysis":{"tried_kinds":[{"kind":"decision","why_not":"no choice made"},{"kind":"fact","why_not":"not stable"},{"kind":"task","why_not":"not actionable"}],"proposed_kind_hypothesis":"timeline - a chronological narrative that none of the sealed kinds capture"}}]}`)
	got, err := ParseResponse(raw, windowRequest(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Observations) != 0 {
		t.Fatalf("unknown must never be a write-target observation, got %d", len(got.Observations))
	}
	if len(got.UnknownCandidates) != 1 {
		t.Fatalf("expected unknown preserved as audit data, got %d", len(got.UnknownCandidates))
	}
}

func TestParserUnknownStructuralFriction(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"too few tried_kinds", `{"schema_version":"1","observations":[{"kind":"unknown","text":"gap","source_event_ids":[1],"unknown_analysis":{"tried_kinds":[{"kind":"decision","why_not":"no"},{"kind":"fact","why_not":"no"}],"proposed_kind_hypothesis":"timeline - a chronological narrative that sealed kinds miss"}}]}`},
		{"tried kind not sealed", `{"schema_version":"1","observations":[{"kind":"unknown","text":"gap","source_event_ids":[1],"unknown_analysis":{"tried_kinds":[{"kind":"summary","why_not":"no"},{"kind":"fact","why_not":"no"},{"kind":"task","why_not":"no"}],"proposed_kind_hypothesis":"timeline - a chronological narrative that sealed kinds miss"}}]}`},
		{"empty why_not", `{"schema_version":"1","observations":[{"kind":"unknown","text":"gap","source_event_ids":[1],"unknown_analysis":{"tried_kinds":[{"kind":"decision","why_not":""},{"kind":"fact","why_not":"no"},{"kind":"task","why_not":"no"}],"proposed_kind_hypothesis":"timeline - a chronological narrative that sealed kinds miss"}}]}`},
		{"hypothesis under 40 chars", `{"schema_version":"1","observations":[{"kind":"unknown","text":"gap","source_event_ids":[1],"unknown_analysis":{"tried_kinds":[{"kind":"decision","why_not":"no"},{"kind":"fact","why_not":"no"},{"kind":"task","why_not":"no"}],"proposed_kind_hypothesis":"too short"}}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseResponse([]byte(c.raw), windowRequest(1))
			if err != nil {
				t.Fatalf("unexpected top-level error: %v", err)
			}
			if len(got.UnknownCandidates) != 0 {
				t.Fatalf("malformed unknown must not be preserved as valid audit data")
			}
			if len(got.RejectedCandidates) != 1 {
				t.Fatalf("expected malformed unknown to be rejected, got %d rejected", len(got.RejectedCandidates))
			}
		})
	}
}

func TestParserPreservesValidUnknowns(t *testing.T) {
	raw := []byte(`{"schema_version":"1","observations":[{"kind":"unknown","text":"genuine ontology gap","source_event_ids":[1],"unknown_analysis":{"tried_kinds":[{"kind":"decision","why_not":"no decision was settled"},{"kind":"fact","why_not":"not a stable fact"},{"kind":"open_question","why_not":"not phrased as a question"}],"proposed_kind_hypothesis":"timeline - a chronological narrative shape that none of the six sealed kinds capture"}}]}`)
	got, err := ParseResponse(raw, windowRequest(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.UnknownCandidates) != 1 {
		t.Fatalf("expected 1 unknown candidate preserved, got %d", len(got.UnknownCandidates))
	}
	uc := got.UnknownCandidates[0]
	if len(uc.TriedKinds) != 3 {
		t.Fatalf("expected 3 tried_kinds preserved, got %d", len(uc.TriedKinds))
	}
	if len(got.RejectedCandidates) != 0 {
		t.Fatalf("valid unknown should not be rejected")
	}
}

func TestParserRejectsMalformedUnknowns(t *testing.T) {
	// Missing unknown_analysis entirely is malformed, not an audit signal.
	raw := []byte(`{"schema_version":"1","observations":[{"kind":"unknown","text":"gap","source_event_ids":[1]}]}`)
	got, err := ParseResponse(raw, windowRequest(1))
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if len(got.UnknownCandidates) != 0 {
		t.Fatalf("malformed unknown must not become audit data")
	}
	if len(got.RejectedCandidates) != 1 {
		t.Fatalf("expected malformed unknown rejected, got %d", len(got.RejectedCandidates))
	}
}

func TestParserDropsLowConfidence(t *testing.T) {
	raw := []byte(`{"schema_version":"1","observations":[{"kind":"fact","text":"weak","confidence":0.3,"source_event_ids":[1]},{"kind":"fact","text":"strong","confidence":0.8,"source_event_ids":[1]}]}`)
	got, err := ParseResponse(raw, windowRequest(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Observations) != 1 {
		t.Fatalf("expected 1 observation after dropping <0.4, got %d", len(got.Observations))
	}
	if got.Observations[0].Text != "strong" {
		t.Fatalf("kept wrong observation: %q", got.Observations[0].Text)
	}
	// Boundary: exactly 0.4 is kept (MinConfidence is inclusive lower bound).
	rawBoundary := []byte(`{"schema_version":"1","observations":[{"kind":"fact","text":"boundary","confidence":0.4,"source_event_ids":[1]}]}`)
	gotB, err := ParseResponse(rawBoundary, windowRequest(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotB.Observations) != 1 {
		t.Fatalf("confidence exactly 0.4 should be kept, got %d", len(gotB.Observations))
	}
}

func TestParserConfidenceRange(t *testing.T) {
	for _, conf := range []string{"1.5", "-0.2"} {
		raw := []byte(`{"schema_version":"1","observations":[{"kind":"fact","text":"x","confidence":` + conf + `,"source_event_ids":[1]}]}`)
		got, err := ParseResponse(raw, windowRequest(1))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.Observations) != 0 {
			t.Fatalf("confidence %s outside [0,1] must be rejected", conf)
		}
		if len(got.RejectedCandidates) != 1 {
			t.Fatalf("expected out-of-range confidence rejected, got %d", len(got.RejectedCandidates))
		}
	}
}

func TestParserRejectsOutOfWindowEventIDs(t *testing.T) {
	raw := []byte(`{"schema_version":"1","observations":[{"kind":"fact","text":"x","confidence":0.9,"source_event_ids":[1,999]}]}`)
	got, err := ParseResponse(raw, windowRequest(1, 2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Observations) != 0 {
		t.Fatalf("expected rejection for out-of-window event id")
	}
	if len(got.RejectedCandidates) != 1 || !strings.Contains(got.RejectedCandidates[0].Reason, "999") {
		t.Fatalf("expected out-of-window rejection mentioning 999, got %v", got.RejectedCandidates)
	}
}

func TestParserRejectsUnrecognizedKind(t *testing.T) {
	raw := []byte(`{"schema_version":"1","observations":[{"kind":"summary","text":"x","confidence":0.9,"source_event_ids":[1]}]}`)
	got, err := ParseResponse(raw, windowRequest(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Observations) != 0 {
		t.Fatalf("unrecognized kind must not be written")
	}
	if len(got.RejectedCandidates) != 1 {
		t.Fatalf("expected unrecognized kind rejected, got %d", len(got.RejectedCandidates))
	}
}

func TestParserCapturesTopLevelExtra(t *testing.T) {
	// Lenient on structure: an unknown top-level envelope field must NOT fail the
	// parse (D24 correction). It is captured into ParsedResponse.Extra.
	raw := []byte(`{"schema_version":"1","model_note":"ran hot","observations":[{"kind":"fact","text":"x","confidence":0.9,"source_event_ids":[1]}]}`)
	got, err := ParseResponse(raw, windowRequest(1))
	if err != nil {
		t.Fatalf("unknown top-level field must not fail the parse: %v", err)
	}
	if len(got.Observations) != 1 {
		t.Fatalf("expected the valid observation kept, got %d", len(got.Observations))
	}
	if _, ok := got.Extra["model_note"]; !ok {
		t.Fatalf("expected top-level extra captured, got %v", got.Extra)
	}
	if _, ok := got.Extra["schema_version"]; ok {
		t.Fatalf("known fields must not leak into Extra")
	}
}

func TestParserCapturesCandidateExtraOnObservation(t *testing.T) {
	raw := []byte(`{"schema_version":"1","observations":[{"kind":"fact","text":"x","confidence":0.9,"source_event_ids":[1],"model_score":0.42}]}`)
	got, err := ParseResponse(raw, windowRequest(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Observations) != 1 {
		t.Fatalf("expected 1 observation, got %d (rejected=%v)", len(got.Observations), got.RejectedCandidates)
	}
	if _, ok := got.Observations[0].Extra["model_score"]; !ok {
		t.Fatalf("expected candidate extra captured on observation, got %v", got.Observations[0].Extra)
	}
}

func TestParserCapturesCandidateExtraOnUnknown(t *testing.T) {
	raw := []byte(`{"schema_version":"1","observations":[{"kind":"unknown","text":"genuine ontology gap","source_event_ids":[1],"vendor_hint":"weird","unknown_analysis":{"tried_kinds":[{"kind":"decision","why_not":"no decision settled"},{"kind":"fact","why_not":"not stable"},{"kind":"open_question","why_not":"not a question"}],"proposed_kind_hypothesis":"timeline - a chronological narrative that none of the sealed kinds capture"}}]}`)
	got, err := ParseResponse(raw, windowRequest(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.UnknownCandidates) != 1 {
		t.Fatalf("expected 1 unknown candidate, got %d", len(got.UnknownCandidates))
	}
	if _, ok := got.UnknownCandidates[0].Extra["vendor_hint"]; !ok {
		t.Fatalf("expected candidate extra captured on unknown, got %v", got.UnknownCandidates[0].Extra)
	}
}

func TestParserSemanticFailureCapturesExtraNotDiscarded(t *testing.T) {
	// A semantically-invalid candidate (non-sealed kind) carrying an unknown field
	// must NOT be silently kept and must NOT vanish: it becomes a RejectedCandidate
	// with its extra, text, and source ids preserved (lenient/strict ruling).
	raw := []byte(`{"schema_version":"1","observations":[{"kind":"summary","text":"bad kind","confidence":0.9,"source_event_ids":[1],"weird_field":123}]}`)
	got, err := ParseResponse(raw, windowRequest(1))
	if err != nil {
		t.Fatalf("salvageable candidate must not abort the parse: %v", err)
	}
	if len(got.Observations) != 0 {
		t.Fatalf("semantically invalid candidate must not be stored")
	}
	if len(got.RejectedCandidates) != 1 {
		t.Fatalf("expected 1 rejected candidate, got %d", len(got.RejectedCandidates))
	}
	rc := got.RejectedCandidates[0]
	if rc.Text != "bad kind" || len(rc.SourceEventIDs) != 1 {
		t.Fatalf("rejected candidate must preserve text/source ids for audit: %+v", rc)
	}
	if _, ok := rc.Extra["weird_field"]; !ok {
		t.Fatalf("rejected candidate must preserve captured extra, got %v", rc.Extra)
	}
}

func TestParserWrongTypedFieldBecomesRejected(t *testing.T) {
	// A string confidence is a per-candidate type failure (not a whole-parse
	// abort) and lands in RejectedCandidates with its extra preserved.
	raw := []byte(`{"schema_version":"1","observations":[{"kind":"fact","text":"x","confidence":"high","source_event_ids":[1],"extra_meta":"keep me"}]}`)
	got, err := ParseResponse(raw, windowRequest(1))
	if err != nil {
		t.Fatalf("type failure on a candidate must not abort the parse: %v", err)
	}
	if len(got.Observations) != 0 {
		t.Fatalf("string-confidence candidate must not be stored")
	}
	if len(got.RejectedCandidates) != 1 {
		t.Fatalf("expected 1 rejected candidate, got %d: %v", len(got.RejectedCandidates), got.RejectedCandidates)
	}
	if !strings.Contains(got.RejectedCandidates[0].Reason, "confidence") {
		t.Fatalf("expected confidence type-failure reason, got %q", got.RejectedCandidates[0].Reason)
	}
	if _, ok := got.RejectedCandidates[0].Extra["extra_meta"]; !ok {
		t.Fatalf("expected extra preserved on type-failed candidate, got %v", got.RejectedCandidates[0].Extra)
	}
}

func TestParserRejectsTrailingData(t *testing.T) {
	// Robust trailing-data detection: appended top-level JSON after a complete
	// envelope is an unsalvageable error (replaces the weak Decoder.More() check).
	raw := []byte(`{"schema_version":"1","observations":[{"kind":"fact","text":"x","confidence":0.9,"source_event_ids":[1]}]}{"schema_version":"1","observations":[]}`)
	_, err := ParseResponse(raw, windowRequest(1))
	if err == nil {
		t.Fatalf("expected trailing-data error")
	}
	if !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParserGotchaBecomesObservationNotUnknown(t *testing.T) {
	// gotcha is the seventh sealed kind: a gotcha candidate is a write-eligible
	// observation and needs no unknown_analysis.
	raw := []byte(`{"schema_version":"1","observations":[{"kind":"gotcha","text":"expected method A, but listener B caused costly side effect Y","confidence":0.9,"source_event_ids":[1]}]}`)
	got, err := ParseResponse(raw, windowRequest(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Observations) != 1 || got.Observations[0].Kind != "gotcha" {
		t.Fatalf("expected 1 gotcha observation, got %d (%v)", len(got.Observations), got.Observations)
	}
	if len(got.UnknownCandidates) != 0 {
		t.Fatalf("gotcha must not be routed to unknown candidates")
	}
}

func TestParserUnknownMayListGotchaInTriedKinds(t *testing.T) {
	// A valid unknown may include gotcha among the sealed kinds it rejected.
	raw := []byte(`{"schema_version":"1","observations":[{"kind":"unknown","text":"genuine ontology gap","source_event_ids":[1],"unknown_analysis":{"tried_kinds":[{"kind":"gotcha","why_not":"no cost or failure mode"},{"kind":"fact","why_not":"not stable"},{"kind":"open_question","why_not":"not a question"}],"proposed_kind_hypothesis":"timeline - a chronological narrative that none of the sealed kinds capture"}}]}`)
	got, err := ParseResponse(raw, windowRequest(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.UnknownCandidates) != 1 {
		t.Fatalf("expected 1 unknown candidate, got %d (rejected=%v)", len(got.UnknownCandidates), got.RejectedCandidates)
	}
	found := false
	for _, tk := range got.UnknownCandidates[0].TriedKinds {
		if tk.Kind == "gotcha" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected gotcha preserved in tried_kinds: %+v", got.UnknownCandidates[0].TriedKinds)
	}
}

func TestParserReturnsPerCandidateErrorsNoWrites(t *testing.T) {
	// Mixed: one valid, one out-of-window, one bad kind, one low confidence.
	raw := []byte(`{"schema_version":"1","observations":[
		{"kind":"fact","text":"good","confidence":0.9,"source_event_ids":[1]},
		{"kind":"fact","text":"oow","confidence":0.9,"source_event_ids":[42]},
		{"kind":"summary","text":"bad","confidence":0.9,"source_event_ids":[1]},
		{"kind":"task","text":"weak","confidence":0.1,"source_event_ids":[1]}
	]}`)
	got, err := ParseResponse(raw, windowRequest(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Observations) != 1 || got.Observations[0].Text != "good" {
		t.Fatalf("expected only the valid observation, got %v", got.Observations)
	}
	if len(got.RejectedCandidates) != 3 {
		t.Fatalf("expected 3 rejected candidates, got %d: %v", len(got.RejectedCandidates), got.RejectedCandidates)
	}
}

// TestParserAssignsRejectionClasses pins the machine-readable class on each
// rejection shape so candidate_reason_counts in run metadata stays meaningful.
func TestParserAssignsRejectionClasses(t *testing.T) {
	req := Request{Events: []db.EventRecord{{ID: 1}, {ID: 2}}}
	raw := []byte(`{
	  "schema_version": "1",
	  "observations": [
	    {"kind": "decision", "text": "grounded elsewhere", "confidence": 0.9, "source_event_ids": [999]},
	    {"kind": "decision", "text": "no sources", "confidence": 0.9, "source_event_ids": []},
	    {"kind": "not-a-kind", "text": "bad kind", "confidence": 0.9, "source_event_ids": [1]},
	    {"kind": "decision", "text": "weak", "confidence": 0.2, "source_event_ids": [1]},
	    {"kind": "decision", "text": 42, "confidence": 0.9, "source_event_ids": [1]},
	    {"kind": "unknown", "text": "gap with no audit", "source_event_ids": [1]}
	  ]
	}`)
	parsed, err := ParseResponse(raw, req)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if len(parsed.Observations) != 0 || len(parsed.RejectedCandidates) != 6 {
		t.Fatalf("want 0 observations / 6 rejections, got %d/%d", len(parsed.Observations), len(parsed.RejectedCandidates))
	}
	wantClasses := map[int]string{
		0: RejectClassUngrounded,
		1: RejectClassUngrounded,
		2: RejectClassValidation,
		3: RejectClassBelowConfidence,
		4: RejectClassMalformed,
		5: RejectClassInvalidUnknown,
	}
	for _, rc := range parsed.RejectedCandidates {
		if want := wantClasses[rc.Index]; rc.Class != want {
			t.Fatalf("candidate %d class = %q, want %q (reason: %s)", rc.Index, rc.Class, want, rc.Reason)
		}
	}
}

// TestParserMapsSourceEventIDStrings pins the 2026-07-11 operator ruling: a
// source_event_ids entry that EXACTLY matches an in-window event's
// source_event_id string is the event's second canonical name and maps to its
// numeric id; everything else still rejects (unmatched = ungrounded,
// ambiguous = never resolves, other types = malformed).
func TestParserMapsSourceEventIDStrings(t *testing.T) {
	req := Request{
		Window: Window{Project: "memoire", SessionID: "session-1"},
		Events: []db.EventRecord{
			{ID: 101, SourceEventID: "cc-turn:sess:uuid-a", EventType: "claude_assistant_turn"},
			{ID: 102, SourceEventID: "claude-userpromptsubmit-abc", EventType: "claude_UserPromptSubmit"},
			{ID: 103, SourceEventID: "dup-key", EventType: "claude_PostToolUse"},
			{ID: 104, SourceEventID: "dup-key", EventType: "claude_PostToolUse"},
		},
	}

	t.Run("exact string matches map to numeric ids", func(t *testing.T) {
		raw := []byte(`{"schema_version":"1","observations":[
		  {"kind":"fact","text":"mixed ids","confidence":0.9,
		   "source_event_ids":["cc-turn:sess:uuid-a",102,"claude-userpromptsubmit-abc"]}
		]}`)
		got, err := ParseResponse(raw, req)
		if err != nil {
			t.Fatalf("ParseResponse: %v", err)
		}
		if len(got.Observations) != 1 || len(got.RejectedCandidates) != 0 {
			t.Fatalf("want 1 observation / 0 rejections, got %d/%d (%v)", len(got.Observations), len(got.RejectedCandidates), got.RejectedCandidates)
		}
		ids := got.Observations[0].SourceEventIDs
		if len(ids) != 3 || ids[0] != 101 || ids[1] != 102 || ids[2] != 102 {
			t.Fatalf("mapped ids = %v, want [101 102 102]", ids)
		}
	})

	t.Run("unknown-kind candidates map too", func(t *testing.T) {
		raw := []byte(`{"schema_version":"1","observations":[
		  {"kind":"unknown","text":"a genuine gap","source_event_ids":["cc-turn:sess:uuid-a"],
		   "unknown_analysis":{"tried_kinds":[{"kind":"fact","why_not":"not stable"},{"kind":"decision","why_not":"nothing settled"},{"kind":"gotcha","why_not":"no cost"}],
		   "proposed_kind_hypothesis":"a hypothesis easily longer than forty characters total"}}
		]}`)
		got, err := ParseResponse(raw, req)
		if err != nil {
			t.Fatalf("ParseResponse: %v", err)
		}
		if len(got.UnknownCandidates) != 1 {
			t.Fatalf("want 1 unknown candidate, got %d (%v)", len(got.UnknownCandidates), got.RejectedCandidates)
		}
		if ids := got.UnknownCandidates[0].SourceEventIDs; len(ids) != 1 || ids[0] != 101 {
			t.Fatalf("unknown candidate ids = %v, want [101]", ids)
		}
	})

	t.Run("unmatched string is ungrounded", func(t *testing.T) {
		raw := []byte(`{"schema_version":"1","observations":[
		  {"kind":"fact","text":"phantom","confidence":0.9,"source_event_ids":["cc-turn:sess:NOPE"]}
		]}`)
		got, err := ParseResponse(raw, req)
		if err != nil {
			t.Fatalf("ParseResponse: %v", err)
		}
		if len(got.RejectedCandidates) != 1 || got.RejectedCandidates[0].Class != RejectClassUngrounded {
			t.Fatalf("want 1 ungrounded rejection, got %v", got.RejectedCandidates)
		}
	})

	t.Run("ambiguous string never resolves", func(t *testing.T) {
		raw := []byte(`{"schema_version":"1","observations":[
		  {"kind":"fact","text":"ambiguous","confidence":0.9,"source_event_ids":["dup-key"]}
		]}`)
		got, err := ParseResponse(raw, req)
		if err != nil {
			t.Fatalf("ParseResponse: %v", err)
		}
		if len(got.RejectedCandidates) != 1 || got.RejectedCandidates[0].Class != RejectClassUngrounded {
			t.Fatalf("want 1 ungrounded rejection, got %v", got.RejectedCandidates)
		}
		if !strings.Contains(got.RejectedCandidates[0].Reason, "ambiguous") {
			t.Fatalf("reason should name ambiguity, got %q", got.RejectedCandidates[0].Reason)
		}
	})

	t.Run("non-number-non-string entry is malformed", func(t *testing.T) {
		raw := []byte(`{"schema_version":"1","observations":[
		  {"kind":"fact","text":"objecty","confidence":0.9,"source_event_ids":[{"id":101}]}
		]}`)
		got, err := ParseResponse(raw, req)
		if err != nil {
			t.Fatalf("ParseResponse: %v", err)
		}
		if len(got.RejectedCandidates) != 1 || got.RejectedCandidates[0].Class != RejectClassMalformed {
			t.Fatalf("want 1 malformed rejection, got %v", got.RejectedCandidates)
		}
	})

	t.Run("null entry is malformed, not a zero id", func(t *testing.T) {
		// json.Unmarshal(null, &int64) is a silent no-op; without the explicit
		// check it would resolve to an ungrounded id 0 (review 2026-07-11).
		raw := []byte(`{"schema_version":"1","observations":[
		  {"kind":"fact","text":"nully","confidence":0.9,"source_event_ids":[null]}
		]}`)
		got, err := ParseResponse(raw, req)
		if err != nil {
			t.Fatalf("ParseResponse: %v", err)
		}
		if len(got.RejectedCandidates) != 1 || got.RejectedCandidates[0].Class != RejectClassMalformed {
			t.Fatalf("want 1 malformed rejection for null entry, got %v", got.RejectedCandidates)
		}
	})

	t.Run("rejection record keeps the resolved prefix for audit", func(t *testing.T) {
		raw := []byte(`{"schema_version":"1","observations":[
		  {"kind":"fact","text":"partial","confidence":0.9,
		   "source_event_ids":["cc-turn:sess:uuid-a",{"bad":true}]}
		]}`)
		got, err := ParseResponse(raw, req)
		if err != nil {
			t.Fatalf("ParseResponse: %v", err)
		}
		if len(got.RejectedCandidates) != 1 || got.RejectedCandidates[0].Class != RejectClassMalformed {
			t.Fatalf("want 1 malformed rejection, got %v", got.RejectedCandidates)
		}
		if ids := got.RejectedCandidates[0].SourceEventIDs; len(ids) != 1 || ids[0] != 101 {
			t.Fatalf("rejection must keep the resolved prefix [101], got %v", ids)
		}
	})

	t.Run("numeric ids still validate window membership", func(t *testing.T) {
		raw := []byte(`{"schema_version":"1","observations":[
		  {"kind":"fact","text":"outside","confidence":0.9,"source_event_ids":[999]}
		]}`)
		got, err := ParseResponse(raw, req)
		if err != nil {
			t.Fatalf("ParseResponse: %v", err)
		}
		if len(got.RejectedCandidates) != 1 || got.RejectedCandidates[0].Class != RejectClassUngrounded {
			t.Fatalf("want 1 ungrounded rejection, got %v", got.RejectedCandidates)
		}
	})
}

// TestPromptDisambiguatesSourceIDNamespace pins the prompt v2.1 clause.
func TestPromptDisambiguatesSourceIDNamespace(t *testing.T) {
	prompt, _, err := BuildPrompt(baseRequest([]db.EventRecord{userPromptEvent(1, 100, "hi")}))
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if !strings.Contains(prompt, "must be the numeric event ids (e.g. 12345), never the `source_event_id` strings") {
		t.Fatalf("prompt missing the source-id namespace disambiguation clause")
	}
}
