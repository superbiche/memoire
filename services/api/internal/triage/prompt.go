// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Package triage implements the triage lane v1 judge: prompt construction,
// strict response validation, the chunked sweep that writes suggestions,
// and the evidence-gated genre graduation.
//
// The lane SUGGESTS and never rules. Nothing here writes review_state,
// provenance, or can_instruct; the only store mutation it performs is an
// append to observation_triage_suggestions (memoire's triage lane v1, R-C).
package triage

import (
	"fmt"
	"strings"

	"memoire/services/api/internal/db"
)

// Prompt versions. They carry the stage so the ruled idempotency key
// (observation_id, prompt_version) stays literally that pair while both
// stages write into one table, and so a prompt change shows up as a NEW
// row in the override-rate table rather than as drift inside one cell.
const (
	PromptVersionStage1 = db.TriagePromptStage1
	PromptVersionStage2 = db.TriagePromptStage2
)

// ResponseSchemaVersion is the judge response envelope version. A
// behavior-changing response shape increments it, and a mismatch is a hard
// parse failure rather than a best-effort read.
const ResponseSchemaVersion = "1"

// DefaultMaxPromptBytes is the conservative chunk ceiling. The judge lane's
// seat is the HL5 conductor at a 32k context, and a prompt is not the only
// thing in that window — the completion has to fit too. 24 KiB of prompt
// leaves room for both with margin, and the ceiling is deliberately
// CONSERVATIVE rather than tuned: a chunk that overflows silently truncates
// the rows at the end, which would look exactly like a judge that declined
// to opine on them.
const DefaultMaxPromptBytes = 24 << 10

// DefaultMaxRowsPerChunk bounds a chunk even when the rows are short. A
// judge asked about two hundred rows at once degrades to pattern-matching
// on position; the unit of work stays "session, chunked".
const DefaultMaxRowsPerChunk = 25

// observationTextBudget truncates one row's text inside the prompt. Triage
// judges shape, not detail: a row whose first 1200 characters do not say
// what it is is itself evidence about the row.
const observationTextBudget = 1200

// BuildStage1Prompt renders the stage-1 judging prompt. Stage-1 input is
// OBSERVATIONS-ONLY (R-C): the judge rules on what the memory layer would
// actually serve, not on the events behind it.
func BuildStage1Prompt(rows []db.TriageRow) (string, error) {
	if len(rows) == 0 {
		return "", fmt.Errorf("triage: stage-1 prompt needs at least one row")
	}
	var b strings.Builder
	b.WriteString(`You are triaging proposed entries in a project memory store.

Each entry below is an observation a derivation run proposed. Your job is to
suggest ONE verdict per entry. You are not deciding: a human applies or
overrides every suggestion, and your reasons are measured against what they
do.

## The discriminator

An observation earns storage by adding STRUCTURE THE RAW EVENTS DO NOT
CARRY. A pure single-event echo — a restatement of one thing that happened,
with no synthesis — is noise. A synthesized outcome across events is not.

Do NOT filter on "this mirrors another system of record". A synthesis like
"the advisory-lock change went review -> merge -> deploy and then ran three
clean nights" mirrors git and deploy logs, and is exactly the kind of thing
this store exists to hold.

## Verdicts

- accept       — durable, synthesized, worth keeping.
- noise-lapse  — noise by the discriminator above: orchestration and
                 coordination chatter, status-only run narration, transient
                 session mechanics. A final result carrying concrete values
                 is an OUTCOME, never noise.
- lapse        — not noise, but not worth keeping either: too thin, too
                 local, or superseded by something obvious.
- reject       — the entry is FALSE: fabricated content, wrong project, or a
                 duplicate of another entry. Reject is strong — a rejected
                 row disappears from the read surface — so use it only when
                 the entry states something untrue, never merely unhelpful.

## Reasons

Every noise-lapse and lapse carries a short genre slug in "genre":
lowercase, hyphenated, reusable across entries (e.g. status-narration,
orchestration-chatter, session-mechanics, thin-restatement). Reuse an
existing slug rather than inventing a synonym — recurring slugs are the
evidence a genre is eventually killed at derivation.

Every reject carries "genre" from this CLOSED set:
  fabrication | wrong-project | duplicate-of-id | other-explicit
"duplicate-of-id" and "other-explicit" both REQUIRE a non-empty "reason";
for duplicate-of-id the reason names the duplicated entry id.

## Confidence

Every verdict carries "confidence": a number from 0 to 1 saying how sure you
are of THAT verdict. Be honest rather than generous — a low number is useful
and costs nothing, because a verdict you are unsure of is read first by a
human instead of riding a batch. Reserve high confidence for entries whose
verdict you would defend.

## Output

Emit STRICT JSON, no prose, no markdown fence:

{"schema_version":"1","verdicts":[
  {"observation_id":123,"verdict":"accept","confidence":0.9},
  {"observation_id":124,"verdict":"noise-lapse","genre":"status-narration","confidence":0.95},
  {"observation_id":125,"verdict":"reject","genre":"fabrication","reason":"dates an event to 1970","confidence":0.7}
]}

Emit exactly one verdict per entry below, and no entry that is not below.

## Entries

`)
	writeRows(&b, rows, false)
	return b.String(), nil
}

// BuildStage2Prompt renders the stage-2 prompt over the VALIDATED set: rows
// the operator accepted. Stage 2 suggests confirm/elevation candidates and
// never touches provenance or can_instruct itself — confirm and elevation
// stay structurally human (memoire:D146/D191's double gesture).
//
// arm selects the measured input treatment (R-C): arm A renders the raw
// event window beside each observation, arm B is observations-only.
func BuildStage2Prompt(rows []db.TriageRow, arm string) (string, error) {
	if len(rows) == 0 {
		return "", fmt.Errorf("triage: stage-2 prompt needs at least one row")
	}
	if arm != db.TriageArmA && arm != db.TriageArmB {
		return "", fmt.Errorf("triage: stage-2 arm must be %q or %q, got %q", db.TriageArmA, db.TriageArmB, arm)
	}
	var b strings.Builder
	b.WriteString(`You are reviewing entries a human has already validated in a project
memory store. Suggest which ones deserve a stronger standing. A human
applies every suggestion; nothing you say changes any entry by itself.

## The two stronger standings

- confirm  — "I vouch for this." The entry is a fact the operator would
             stand behind. Suggest it for durable, specific, verified
             content.
- elevate  — a STANDING ORDER: "constrain every session in this project,
             regardless of subject." Caveats, gotchas, invariants. An
             important FACT is confirmed, not elevated. If an entry is only
             true-and-useful, it is not a standing order.

Elevation is a subscription, not a truth tier. Be sparing: every elevated
entry competes for a small, capped standing brief, and one that does not
constrain future work is taking a seat from one that does.

## Output

Emit STRICT JSON, no prose, no markdown fence:

{"schema_version":"1","verdicts":[
  {"observation_id":123,"verdict":"confirm"},
  {"observation_id":124,"verdict":"elevate","reason":"a constraint every session must respect"}
]}

Emit a verdict ONLY for entries that deserve one. SAY NOTHING about an
entry that deserves neither — omitting it is the answer. Never invent an
entry that is not below.

## Entries

`)
	writeRows(&b, rows, arm == db.TriageArmA)
	return b.String(), nil
}

func writeRows(b *strings.Builder, rows []db.TriageRow, withEvents bool) {
	for _, r := range rows {
		o := r.Observation
		fmt.Fprintf(b, "### id=%d kind=%s project=%s state=%s%s\n",
			o.ID, o.Kind, o.Project, o.ReviewState, noiseMarker(o))
		b.WriteString(truncate(o.Text, observationTextBudget))
		b.WriteString("\n")
		if withEvents && len(r.RawEvents) > 0 {
			b.WriteString("\nSource events:\n")
			for _, e := range r.RawEvents {
				b.WriteString("- ")
				b.WriteString(truncate(collapse(e), observationTextBudget/4))
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
	}
}

// noiseMarker surfaces an EXISTING noise flag to the judge. Derivation
// already flags what it suspects (memoire:D211's prompt v2.3 genres); the
// judge should know that, both to agree cheaply and to disagree explicitly.
func noiseMarker(o db.ObservationRecord) string {
	if o.NoiseCandidate == 1 {
		return " flagged=noise_candidate"
	}
	return ""
}

func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + " …[truncated]"
}

// Chunk splits rows into judging units under BOTH ceilings — rendered
// prompt bytes and row count. It is deliberately conservative: a chunk that
// overflows the judge's window loses its tail silently, and a silently
// unjudged row is indistinguishable from a row the judge declined.
func Chunk(rows []db.TriageRow, maxBytes, maxRows int, build func([]db.TriageRow) (string, error)) [][]db.TriageRow {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxPromptBytes
	}
	if maxRows <= 0 {
		maxRows = DefaultMaxRowsPerChunk
	}
	var out [][]db.TriageRow
	var cur []db.TriageRow
	flush := func() {
		if len(cur) > 0 {
			out = append(out, cur)
			cur = nil
		}
	}
	for _, r := range rows {
		next := append(append([]db.TriageRow{}, cur...), r)
		p, err := build(next)
		// A single row that alone exceeds the ceiling still gets judged: it
		// is emitted as its own chunk rather than dropped, because dropping
		// it would silently remove a row from the operator's queue.
		if err == nil && len(p) > maxBytes && len(cur) > 0 {
			flush()
			cur = []db.TriageRow{r}
			continue
		}
		cur = next
		if len(cur) >= maxRows {
			flush()
		}
	}
	flush()
	return out
}
