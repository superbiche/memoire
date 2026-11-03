// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"fmt"
	"testing"

	"memoire/services/api/internal/authctx"
)

// AttachRunSiblings is the lineage-at-retrieval read (Fibery #335): a hit
// carries the other observations of its derivation run, textlessly, so a
// consumer sees the story a hit belongs to without a second search.
func TestAttachRunSiblings(t *testing.T) {
	store, eventID, runID := governanceSeed(t, "run-siblings")
	defer store.Close()
	ctx := authctx.With(context.Background(), authctx.Context{
		PrincipalID:  "operator",
		CredentialID: "cred-run-siblings",
		RequestID:    "req-run-siblings",
		Scopes:       []string{"admin"},
	})

	decisionID := insertTestDerivedObservation(t, store, runID, eventID, "run-siblings", "decision", "rename Michel to Human", "accepted", "sha256:run-siblings-decision")
	taskID := insertTestDerivedObservation(t, store, runID, eventID, "run-siblings", "task", "do the rename across the tree", "accepted", "sha256:run-siblings-task")
	unrelatedID := insertTestDerivedObservation(t, store, runID, eventID, "run-siblings", "task", "unrelated inbox sweep", "accepted", "sha256:run-siblings-unrelated")
	proposedID := insertTestDerivedObservation(t, store, runID, eventID, "run-siblings", "fact", "still unreviewed", "proposed", "sha256:run-siblings-proposed")
	lapsedID := insertTestDerivedObservation(t, store, runID, eventID, "run-siblings", "fact", "never reviewed in time", "proposed", "sha256:run-siblings-lapsed")
	if _, err := store.TransitionObservationReview(ctx, lapsedID, "lapsed", "ttl"); err != nil {
		t.Fatal(err)
	}
	rejectedID := insertTestDerivedObservation(t, store, runID, eventID, "run-siblings", "fact", "ruled out", "proposed", "sha256:run-siblings-rejected")
	if _, err := store.TransitionObservationReview(ctx, rejectedID, "rejected", "noise"); err != nil {
		t.Fatal(err)
	}
	supersededID := insertTestDerivedObservation(t, store, runID, eventID, "run-siblings", "fact", "restated later", "accepted", "sha256:run-siblings-superseded")
	if _, err := store.TransitionObservationReview(ctx, supersededID, "superseded", "restated"); err != nil {
		t.Fatal(err)
	}
	deletedID := insertTestDerivedObservation(t, store, runID, eventID, "run-siblings", "fact", "soft deleted", "accepted", "sha256:run-siblings-deleted")
	if _, err := store.TransitionObservationReview(ctx, deletedID, "soft_deleted", "privacy"); err != nil {
		t.Fatal(err)
	}
	// The run's raw capture projection must never surface as a sibling: it
	// is the event text, not a derived observation.
	rawID := rawObservationIDForEvent(t, store, eventID)

	// Accepted-only band: the default read. The decision's siblings are the
	// run's other accepted derived rows, in id order, self excluded.
	hits := []SearchHit{{ID: decisionID, DerivationRunID: runID}}
	if err := store.AttachRunSiblings(context.Background(), hits, ObservationFilter{}, SiblingsPerHit); err != nil {
		t.Fatal(err)
	}
	assertSiblingIDs(t, hits[0].Siblings, []int64{taskID, unrelatedID})
	for _, sib := range hits[0].Siblings {
		if sib.Kind == "" || sib.ReviewState == "" {
			t.Fatalf("sibling %d must carry kind and review_state, got %+v", sib.ID, sib)
		}
	}
	for _, absent := range []int64{decisionID, proposedID, lapsedID, rejectedID, supersededID, deletedID, rawID} {
		assertNoSibling(t, hits[0].Siblings, absent)
	}

	// Unreviewed visibility follows the request's filter, so a caller who
	// excluded unreviewed hits never receives unreviewed siblings.
	hits = []SearchHit{{ID: decisionID, DerivationRunID: runID}}
	if err := store.AttachRunSiblings(context.Background(), hits, ObservationFilter{IncludeProposed: true}, SiblingsPerHit); err != nil {
		t.Fatal(err)
	}
	assertSiblingIDs(t, hits[0].Siblings, []int64{taskID, unrelatedID, proposedID, lapsedID})
	for _, absent := range []int64{decisionID, rejectedID, supersededID, deletedID, rawID} {
		assertNoSibling(t, hits[0].Siblings, absent)
	}

	// Every hit of the run gets its own view: each excludes itself only.
	hits = []SearchHit{
		{ID: decisionID, DerivationRunID: runID},
		{ID: taskID, DerivationRunID: runID},
	}
	if err := store.AttachRunSiblings(context.Background(), hits, ObservationFilter{}, SiblingsPerHit); err != nil {
		t.Fatal(err)
	}
	assertSiblingIDs(t, hits[0].Siblings, []int64{taskID, unrelatedID})
	assertSiblingIDs(t, hits[1].Siblings, []int64{decisionID, unrelatedID})

	// A hit with no run id keeps no siblings, and the elevation flag rides
	// through when the operator promoted a sibling.
	if _, err := store.ConfirmObservation(ctx, unrelatedID, "verified"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ElevateObservation(ctx, unrelatedID, "operating note"); err != nil {
		t.Fatal(err)
	}
	hits = []SearchHit{
		{ID: decisionID, DerivationRunID: runID},
		{ID: rawID},
	}
	if err := store.AttachRunSiblings(context.Background(), hits, ObservationFilter{}, SiblingsPerHit); err != nil {
		t.Fatal(err)
	}
	if len(hits[1].Siblings) != 0 {
		t.Fatalf("a hit with no derivation run must carry no siblings, got %+v", hits[1].Siblings)
	}
	elevated := findSibling(t, hits[0].Siblings, unrelatedID)
	if elevated.CanInstruct != 1 {
		t.Fatalf("elevated sibling must carry can_instruct=1, got %+v", elevated)
	}
}

// A sibling is a pointer a caller may follow, so a sibling must never be a
// row that same caller cannot fetch. Siblings therefore ride the by-id
// route's visibility clauses, which is where the noise exclusion and the
// feed actor-gate come from: both are ON by default and both lift only the
// way observationWhere lifts them.
func TestAttachRunSiblingsObeyHitVisibility(t *testing.T) {
	store, eventID, runID := governanceSeed(t, "siblings-visibility")
	defer store.Close()
	ctx := authctx.With(context.Background(), authctx.Context{
		PrincipalID:  "operator",
		CredentialID: "cred-siblings-visibility",
		RequestID:    "req-siblings-visibility",
		Scopes:       []string{"admin"},
	})

	anchorID := insertTestDerivedObservation(t, store, runID, eventID, "siblings-visibility", "decision", "the anchor decision", "accepted", "sha256:siblings-vis-anchor")
	plainID := insertTestDerivedObservation(t, store, runID, eventID, "siblings-visibility", "task", "an ordinary accepted task", "accepted", "sha256:siblings-vis-plain")

	// A noise-flagged sibling: excluded by default, visible on the opt-in.
	noisyID := insertTestDerivedObservation(t, store, runID, eventID, "siblings-visibility", "fact", "noisy row", "accepted", "sha256:siblings-vis-noisy")
	if _, err := store.db.ExecContext(context.Background(),
		`UPDATE observations SET noise_candidate = 1 WHERE id = ?`, noisyID); err != nil {
		t.Fatal(err)
	}

	// An accepted sibling whose accept transition was rendered by an
	// `agent:*` actor: feed-INELIGIBLE, so never a sibling either.
	machineID := insertTestDerivedObservation(t, store, runID, eventID, "siblings-visibility", "fact", "machine accepted row", "proposed", "sha256:siblings-vis-machine")
	agentAccept(t, store, machineID, TriageActor("qwen3-30b"))

	// A human-accepted sibling keeps its eligibility, so the gate is proved
	// to be actor-shaped and not "anything with a review-log entry".
	humanID := insertTestDerivedObservation(t, store, runID, eventID, "siblings-visibility", "fact", "human accepted row", "proposed", "sha256:siblings-vis-human")
	if _, err := store.TransitionObservationReview(ctx, humanID, "accepted", "reviewed"); err != nil {
		t.Fatal(err)
	}

	attach := func(filter ObservationFilter) []ObservationSibling {
		t.Helper()
		hits := []SearchHit{{ID: anchorID, DerivationRunID: runID}}
		if err := store.AttachRunSiblings(context.Background(), hits, filter, SiblingsPerHit); err != nil {
			t.Fatal(err)
		}
		return hits[0].Siblings
	}

	// Defaults: the noisy row and the machine-accepted row are both out.
	assertSiblingIDs(t, attach(ObservationFilter{}), []int64{plainID, humanID})

	// include_noise=true is the by-id route's own opt-in, so the noisy
	// sibling appears — and the actor-gate still holds.
	assertSiblingIDs(t, attach(ObservationFilter{IncludeNoise: true}), []int64{plainID, noisyID, humanID})

	// The actor-gate does NOT lift with include_agent_accepted: the by-id
	// route cannot express that opt-in, so a sibling never rides it. This is
	// the fail-closed half of the fetchability invariant.
	assertSiblingIDs(t, attach(ObservationFilter{IncludeAgentAccepted: true}), []int64{plainID, humanID})
	assertNoSibling(t, attach(ObservationFilter{IncludeAgentAccepted: true}), machineID)

	// A hit's own scope never narrows its siblings: a sibling is defined by
	// its run. Project, session, kind and run filters on the request leave
	// the sibling set alone.
	scoped := ObservationFilter{
		Project: "memoire-test", SessionID: "siblings-visibility",
		Kind: "decision", ExcludeKind: "raw", DerivationRunID: runID, Limit: 20,
	}
	assertSiblingIDs(t, attach(scoped), []int64{plainID, humanID})

	// An explicit review_state on the request does not widen the band
	// either: the by-id route ignores review_state, so the sibling band
	// stays the include_proposed band. Asking for the proposed queue must
	// not hand out proposed siblings the caller cannot then fetch.
	unreviewedID := insertTestDerivedObservation(t, store, runID, eventID, "siblings-visibility", "fact", "still proposed", "proposed", "sha256:siblings-vis-proposed")
	proposedOnly := attach(ObservationFilter{ReviewState: "proposed", Limit: 20})
	assertSiblingIDs(t, proposedOnly, []int64{plainID, humanID})
	assertNoSibling(t, proposedOnly, unreviewedID)

	// include_proposed=true IS the by-id opt-in, so the unreviewed sibling
	// appears — and the actor-gate still holds across the wider band.
	withProposed := attach(ObservationFilter{IncludeProposed: true})
	assertSiblingIDs(t, withProposed, []int64{plainID, humanID, unreviewedID})
	assertNoSibling(t, withProposed, machineID)
}

// The cap bounds a pathological run without truncating the ordinary story.
// It applies per hit, after the self exclusion, in id order.
func TestAttachRunSiblingsCap(t *testing.T) {
	store, eventID, runID := governanceSeed(t, "run-siblings-cap")
	defer store.Close()

	ids := make([]int64, 0, 12)
	for i := range 12 {
		ids = append(ids, insertTestDerivedObservation(t, store, runID, eventID, "run-siblings-cap", "fact",
			fmt.Sprintf("capped row %d", i), "accepted", fmt.Sprintf("sha256:run-siblings-cap-%d", i)))
	}

	hits := []SearchHit{{ID: ids[0], DerivationRunID: runID}}
	if err := store.AttachRunSiblings(context.Background(), hits, ObservationFilter{}, 3); err != nil {
		t.Fatal(err)
	}
	assertSiblingIDs(t, hits[0].Siblings, ids[1:4])

	// A non-positive perHit falls back to the package default.
	hits = []SearchHit{{ID: ids[0], DerivationRunID: runID}}
	if err := store.AttachRunSiblings(context.Background(), hits, ObservationFilter{}, 0); err != nil {
		t.Fatal(err)
	}
	assertSiblingIDs(t, hits[0].Siblings, ids[1:1+SiblingsPerHit])
}

// No run ids means no query at all: the method returns early, which is what
// makes attaching on every retrieval response free for raw-only results.
func TestAttachRunSiblingsEarlyReturnWithoutRunIDs(t *testing.T) {
	store, eventID, _ := governanceSeed(t, "run-siblings-empty")
	defer store.Close()
	rawID := rawObservationIDForEvent(t, store, eventID)

	for name, hits := range map[string][]SearchHit{
		"nil slice":   nil,
		"empty slice": {},
		"no run ids":  {{ID: rawID}, {ID: rawID + 1}},
	} {
		if err := store.AttachRunSiblings(context.Background(), hits, ObservationFilter{}, SiblingsPerHit); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, hit := range hits {
			if len(hit.Siblings) != 0 {
				t.Fatalf("%s: hit %d must carry no siblings, got %+v", name, hit.ID, hit.Siblings)
			}
		}
	}
}

func assertSiblingIDs(t *testing.T, siblings []ObservationSibling, want []int64) {
	t.Helper()
	if len(siblings) != len(want) {
		t.Fatalf("siblings = %+v, want ids %v", siblings, want)
	}
	for i, id := range want {
		if siblings[i].ID != id {
			t.Fatalf("siblings[%d].ID = %d, want %d (full: %+v)", i, siblings[i].ID, id, siblings)
		}
	}
}

func assertNoSibling(t *testing.T, siblings []ObservationSibling, id int64) {
	t.Helper()
	for _, sib := range siblings {
		if sib.ID == id {
			t.Fatalf("did not expect sibling %d in %+v", id, siblings)
		}
	}
}

func findSibling(t *testing.T, siblings []ObservationSibling, id int64) ObservationSibling {
	t.Helper()
	for _, sib := range siblings {
		if sib.ID == id {
			return sib
		}
	}
	t.Fatalf("expected sibling %d in %+v", id, siblings)
	return ObservationSibling{}
}
