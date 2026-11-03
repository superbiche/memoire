// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package triage

import (
	"context"
	"errors"
	"fmt"

	"memoire/services/api/internal/db"
)

// Judge is the model seam. QwenAdapter.Chat satisfies it, which is what
// binds the lane to the `data_boundary=local` endpoint registry
// (memoire:D137) and to the name-not-value secret seam (memoire:D55/D58)
// without a second HTTP client. Tests substitute a stub.
type Judge interface {
	Chat(ctx context.Context, endpointID, model, prompt string) (rawResponse []byte, err error)
}

// Store is the narrow store surface the sweep needs — narrow on purpose
// (memoire:D49), and notice what is NOT in it: no verdict method, no
// elevation, no provenance write. The sweep structurally cannot rule.
type Store interface {
	ListTriageScope(ctx context.Context, f db.TriageScopeFilter) ([]db.TriageRow, error)
	ListTriageScopeSessions(ctx context.Context, f db.TriageScopeFilter) ([]db.TriageScopeSession, error)
	LoadRawEvents(ctx context.Context, rows []db.TriageRow, perRow int) error
	InsertTriageSuggestion(ctx context.Context, in db.TriageSuggestionInput) (bool, error)
}

// SweepOptions configures one pass.
type SweepOptions struct {
	Project    string
	SessionID  string
	EndpointID string
	Model      string
	Stage      int
	Limit      int
	// MaxPromptBytes and MaxRowsPerChunk bound the judging unit. Zero takes
	// the conservative defaults.
	MaxPromptBytes  int
	MaxRowsPerChunk int
	// Budget caps how many rows ONE pass may judge across all sessions.
	// Zero means no cap: walk every session to exhaustion. The cap exists
	// so a nightly pass can be bounded deliberately rather than by the
	// accident of a page limit.
	Budget int
	// DryRun builds and validates everything, calls the judge, and writes
	// NOTHING. The rehearsal a lane this cheap should always have.
	DryRun bool
}

// SweepOutcome is the honest report of a pass.
type SweepOutcome struct {
	Stage      int `json:"stage"`
	Sessions   int `json:"sessions"`
	Chunks     int `json:"chunks"`
	RowsJudged int `json:"rows_judged"`
	// RowsAttempted is what the BUDGET bounds: every row handed to the
	// judge, whether or not the chunk came back. A budget that counted only
	// successes would let a persistently failing judge spin through the
	// whole backlog for free, which is the opposite of a cost bound.
	RowsAttempted int `json:"rows_attempted"`
	// Suggestions counts rows actually INSERTED. SuggestionsSkipped counts
	// verdicts the idempotency guard ignored because that row already had a
	// suggestion at this prompt version. Folding the two would let a pass
	// that wrote nothing report a full night's work.
	Suggestions int `json:"suggestions"`
	// Covered counts COVERAGE rows written: rows the judge read and
	// nominated nothing for. They are bookkeeping, not suggestions, so
	// folding them into Suggestions would report a night of judging as a
	// night of suggesting — the same dishonesty the skipped-insert split
	// exists to prevent.
	Covered            int            `json:"covered"`
	SuggestionsSkipped int            `json:"suggestions_skipped"`
	Rejected           int            `json:"rejected"`
	ChunkFailed        int            `json:"chunk_failed"`
	ByVerdict          map[string]int `json:"by_verdict"`
	DryRun             bool           `json:"dry_run"`
	// BudgetReached says the pass stopped on its row budget rather than on
	// an exhausted scope — the difference between "nothing left to do" and
	// "more waiting", which the operator must not have to guess.
	BudgetReached bool   `json:"budget_reached"`
	FirstFailure  string `json:"first_failure,omitempty"`
}

// Sweep judges a stage's scope and appends suggestions. BOTH stages ride
// this one loop — stage 1 over the R-A reviewable scope, stage 2 over the
// human-validated set — so paging, the budget and the handled-row exclusion
// cannot drift apart between them.
//
// Failure posture mirrors the derivation lane (memoire:D48): a judge that
// times out or answers garbage is DATA, not an error — the chunk is counted
// failed, the pass continues, and the outcome says so. A store or setup
// failure is a real error. A pass that judged nothing because every chunk
// failed returns an error, because a silent zero would read as "nothing to
// triage".
func Sweep(ctx context.Context, store Store, judge Judge, opts SweepOptions) (SweepOutcome, error) {
	if opts.Stage != 1 && opts.Stage != 2 {
		return SweepOutcome{}, fmt.Errorf("triage: stage must be 1 or 2, got %d", opts.Stage)
	}
	if opts.EndpointID == "" || opts.Model == "" {
		return SweepOutcome{}, errors.New("triage: endpoint-id and model are required (the judge is registry-bound, memoire:D137)")
	}
	out := SweepOutcome{Stage: opts.Stage, DryRun: opts.DryRun, ByVerdict: map[string]int{}}

	// The scope read the sweep uses skips rows already judged at THIS prompt
	// version, so a re-run spends the judge's seat on new work instead of
	// re-deriving verdicts it already holds. The UI queue does not set this:
	// a suggested row is exactly what the operator came to rule on.
	base := db.TriageScopeFilter{
		Stage:                     opts.Stage,
		Project:                   opts.Project,
		SessionID:                 opts.SessionID,
		ExcludeSuggestedAtVersion: promptVersionFor(opts.Stage),
	}
	sessions, err := store.ListTriageScopeSessions(ctx, base)
	if err != nil {
		return out, err
	}
	pageSize := opts.Limit
	if pageSize <= 0 {
		pageSize = 200
	}

	for _, sess := range sessions {
		out.Sessions++
		var afterID int64
		for {
			if opts.Budget > 0 && out.RowsAttempted >= opts.Budget {
				out.BudgetReached = true
				return out, sweepErr(out)
			}
			page := base
			page.Project = sess.Project
			page.SessionID = sess.SessionID
			page.Limit = pageSize
			page.AfterID = afterID
			rows, err := store.ListTriageScope(ctx, page)
			if err != nil {
				return out, err
			}
			if len(rows) == 0 {
				break
			}
			// The page is one session by construction, so no prompt can
			// ever mix sessions.
			if err := judgeRows(ctx, store, judge, opts, rows, &out); err != nil {
				return out, err
			}
			if out.BudgetReached {
				return out, sweepErr(out)
			}
			next := rows[len(rows)-1].Observation.ID
			if next <= afterID {
				break // defensive: never loop on a non-advancing cursor
			}
			afterID = next
			if len(rows) < pageSize {
				break
			}
		}
	}
	return out, sweepErr(out)
}

// sweepErr turns an all-chunks-failed pass into an error. A silent zero
// would read as "nothing to triage" when the truth is "the judge is down".
func sweepErr(out SweepOutcome) error {
	if out.Chunks > 0 && out.ChunkFailed == out.Chunks {
		return fmt.Errorf("triage: every chunk failed (%d/%d), first failure: %s",
			out.ChunkFailed, out.Chunks, out.FirstFailure)
	}
	return nil
}

// SweepRows is Sweep over a caller-supplied row set — the stage-2 entry
// point, where the validated set comes from the operator's accepts rather
// than from the reviewable scope.
func SweepRows(ctx context.Context, store Store, judge Judge, opts SweepOptions, rows []db.TriageRow) (SweepOutcome, error) {
	out := SweepOutcome{Stage: opts.Stage, DryRun: opts.DryRun, ByVerdict: map[string]int{}}
	if len(rows) == 0 {
		return out, nil
	}
	if err := judgeRows(ctx, store, judge, opts, rows, &out); err != nil {
		return out, err
	}
	return out, sweepErr(out)
}

func judgeRows(ctx context.Context, store Store, judge Judge, opts SweepOptions, rows []db.TriageRow, out *SweepOutcome) error {
	build := func(chunk []db.TriageRow) (string, error) {
		if opts.Stage == 2 {
			// The arm is a property of the CHUNK, not of the pass: a chunk
			// is homogeneous by construction below, so arm A and arm B rows
			// never share a prompt.
			return BuildStage2Prompt(chunk, db.TriageStage2Arm(chunk[0].Observation.ID))
		}
		return BuildStage1Prompt(chunk)
	}

	groups := [][]db.TriageRow{rows}
	if opts.Stage == 2 {
		// Split by arm BEFORE chunking so the A/B stays clean: an arm-A row
		// judged inside an arm-B prompt would be measured under the wrong
		// treatment and quietly ruin the comparison.
		var armA, armB []db.TriageRow
		for _, r := range rows {
			if db.TriageStage2Arm(r.Observation.ID) == db.TriageArmA {
				armA = append(armA, r)
			} else {
				armB = append(armB, r)
			}
		}
		if err := store.LoadRawEvents(ctx, armA, 0); err != nil {
			return err
		}
		groups = nil
		for _, g := range [][]db.TriageRow{armA, armB} {
			if len(g) > 0 {
				groups = append(groups, g)
			}
		}
	}

	for _, group := range groups {
		for _, chunk := range Chunk(group, opts.MaxPromptBytes, opts.MaxRowsPerChunk, build) {
			// The budget bounds rows JUDGED, checked before each chunk and
			// enforced by trimming the chunk to what is left — a budget of
			// 1 must cost one row, not one page.
			if opts.Budget > 0 {
				remaining := opts.Budget - out.RowsAttempted
				if remaining <= 0 {
					out.BudgetReached = true
					return nil
				}
				if len(chunk) > remaining {
					chunk = chunk[:remaining]
				}
			}
			out.Chunks++
			out.RowsAttempted += len(chunk)
			prompt, err := build(chunk)
			if err != nil {
				return err
			}
			raw, err := judge.Chat(ctx, opts.EndpointID, opts.Model, prompt)
			if err != nil {
				out.ChunkFailed++
				if out.FirstFailure == "" {
					out.FirstFailure = err.Error()
				}
				continue
			}
			scope := map[int64]bool{}
			for _, r := range chunk {
				scope[r.Observation.ID] = true
			}
			parsed, err := ParseResponse(raw, opts.Stage, scope)
			if err != nil {
				out.ChunkFailed++
				if out.FirstFailure == "" {
					out.FirstFailure = err.Error()
				}
				continue
			}
			out.RowsJudged += len(chunk)
			out.Rejected += len(parsed.Rejected)
			// Every row in the chunk was EVALUATED. The ones the judge
			// nominated get a suggestion; the rest get a coverage row, so
			// "evaluated, nothing to suggest" stops being indistinguishable
			// from "never evaluated" — without it a re-sweep re-judges the
			// same rows forever and paging can never exhaust the scope.
			nominated := map[int64]bool{}
			for _, v := range parsed.Verdicts {
				nominated[v.ObservationID] = true
			}
			verdicts := append([]Verdict{}, parsed.Verdicts...)
			for _, r := range chunk {
				if !nominated[r.Observation.ID] {
					verdicts = append(verdicts, Verdict{ObservationID: r.Observation.ID, Verdict: db.TriageVerdictNone})
				}
			}
			for _, v := range verdicts {
				out.ByVerdict[v.Verdict]++
				if opts.DryRun {
					continue
				}
				in := db.TriageSuggestionInput{
					ObservationID:    v.ObservationID,
					Stage:            opts.Stage,
					PromptVersion:    promptVersionFor(opts.Stage),
					Model:            opts.Model,
					SuggestedVerdict: v.Verdict,
					ReasonGenre:      v.Genre,
					ReasonText:       v.Reason,
					Arm:              armProbe(opts.Stage, v.ObservationID),
					Confidence:       v.Confidence,
				}
				if db.IsCoverageVerdict(v.Verdict) {
					// A coverage row carries no reason, no arm and no
					// confidence: nothing was suggested, so there is nothing
					// to explain, to measure under a treatment, or to be
					// confident about.
					in.ReasonGenre, in.ReasonText, in.Arm = "", "", ""
					in.Confidence = nil
				}
				inserted, err := store.InsertTriageSuggestion(ctx, in)
				if err != nil {
					return err
				}
				switch {
				case !inserted:
					out.SuggestionsSkipped++
				case db.IsCoverageVerdict(v.Verdict):
					out.Covered++
				default:
					out.Suggestions++
				}
			}
		}
	}
	return nil
}
