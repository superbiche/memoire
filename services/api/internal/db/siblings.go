// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"strings"
)

// SiblingsPerHit caps how many run-siblings ride one hit. Since Fibery #343
// a derivation run is one rhythm segment, so a run holds a handful of
// observations — measured run sizes on recent sessions were 3 to 11. The cap
// bounds the pathological run without truncating the ordinary story.
const SiblingsPerHit = 8

// siblingVisibility reduces a request's filter to the visibility flags the
// by-id route honors, which is what makes a sibling pointer safe to hand out:
// a sibling a caller can see must be a sibling that same caller can FETCH.
//
// GetObservation reads only include_proposed and include_noise from the
// query, so those two ride through and everything else is dropped. Project,
// session, kind and run scoping are deliberately absent — a sibling is
// defined by its run, not by the hit's scope. ReviewState is dropped so the
// band falls back to the include_proposed band the by-id route computes.
// IncludeAgentAccepted is dropped fail-closed: lifting the feed actor-gate
// is an opt-in the by-id route cannot express, so a sibling never rides it.
func siblingVisibility(filter ObservationFilter) ObservationFilter {
	return ObservationFilter{
		IncludeProposed: filter.IncludeProposed,
		IncludeNoise:    filter.IncludeNoise,
	}
}

// AttachRunSiblings fills Siblings on every hit that names a derivation run.
// Siblings are the run's other observations: the story the hit was derived
// with (Fibery #335). One grouped statement serves the whole slice, so a
// search response costs one extra query no matter how many hits it carries.
//
// The visibility clauses come from observationWhere itself, over
// siblingVisibility(filter), so the sibling predicate cannot drift from the
// by-id predicate: soft-delete, the review-state band (D146 R-6), the
// default noise exclusion and the feed actor-gate (triage lane v1, R-D) are
// all the same code. On top of that sit the sibling-specific exclusions —
// raw projections, rejected and superseded rows, and the hit itself.
//
// Handlers call this after the search lanes return. The lanes themselves
// never fill Siblings — attaching inside a lane would run per lane and
// break fusion's whole-hit copy.
func (s *Store) AttachRunSiblings(ctx context.Context, hits []SearchHit, filter ObservationFilter, perHit int) error {
	if perHit <= 0 {
		perHit = SiblingsPerHit
	}
	runIDs := make([]string, 0, len(hits))
	seen := make(map[string]bool, len(hits))
	for _, h := range hits {
		if h.DerivationRunID == "" || seen[h.DerivationRunID] {
			continue
		}
		seen[h.DerivationRunID] = true
		runIDs = append(runIDs, h.DerivationRunID)
	}
	if len(runIDs) == 0 {
		return nil
	}

	// Run ids bind first, then whatever observationWhere binds, so the
	// placeholder order matches the arg order whichever clauses it emits.
	args := make([]any, 0, len(runIDs))
	for _, id := range runIDs {
		args = append(args, id)
	}
	visibility, visibilityArgs := observationWhere(siblingVisibility(filter), "o")
	args = append(args, visibilityArgs...)
	// The rejected/superseded exclusion is redundant with today's bands,
	// which admit only accepted, proposed and lapsed. It is written anyway:
	// a sibling is a pointer a caller may follow, and a future band widening
	// must not silently start handing out buried rows.
	//
	// The store's dialect seam rebinds '?' to $1..$n on Postgres, so the
	// placeholder list is written once. idx_observations_derivation_run
	// (schema/0004) covers the lookup.
	query := `SELECT o.derivation_run_id, o.id, o.kind, o.review_state, o.can_instruct
		FROM observations o
		WHERE o.derivation_run_id IN (` + placeholders(len(runIDs)) + `)
		  AND o.kind <> 'raw'
		  AND o.review_state NOT IN ('rejected', 'superseded')
		  AND ` + visibility + `
		ORDER BY o.id`
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byRun := make(map[string][]ObservationSibling, len(runIDs))
	for rows.Next() {
		var runID string
		var sib ObservationSibling
		if err := rows.Scan(&runID, &sib.ID, &sib.Kind, &sib.ReviewState, &sib.CanInstruct); err != nil {
			return err
		}
		byRun[runID] = append(byRun[runID], sib)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for i := range hits {
		run := hits[i].DerivationRunID
		if run == "" {
			continue
		}
		var sibs []ObservationSibling
		for _, sib := range byRun[run] {
			if sib.ID == hits[i].ID {
				continue
			}
			sibs = append(sibs, sib)
			if len(sibs) == perHit {
				break
			}
		}
		hits[i].Siblings = sibs
	}
	return nil
}

// placeholders renders an n-wide '?' list for an IN clause. The package has
// no helper for it and the dialect seam rebinds the result per engine.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
