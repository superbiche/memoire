// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"memoire/services/api/internal/db"
)

// The triage read surface (triage lane v1, R-C). READ-ONLY on purpose: the
// triage screen applies its verdicts through the EXISTING per-row verdict
// routes under their D208 preconditions, because a batch verdict route was
// ruled out at memoire:D146 and stays out. There is no POST here and no
// write route for suggestions either — the judge writes through the
// colocated `memoire-triage` binary, the same seam the derive lane uses,
// so no new machine-facing write authority is opened over HTTP.

// ApplyReviewAction is the memoire:D125 flag-verb surface, finally exposed.
// It carries the operator's ENCOUNTER with a row whose review_state already
// says what they want it to say — an affirmed TTL lapse, a noise genre the
// judge missed, a declined elevation — as an append-only action entry. It
// changes flags, never review_state, so the transition legality table is
// untouched and lapsed -> lapsed stays illegal.
func (h Handler) ApplyReviewAction(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var body struct {
		Action     string `json:"action"`
		ReasonText string `json:"reason_text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.Action == "" {
		writeError(w, http.StatusBadRequest, errors.New("action is required"))
		return
	}
	rec, err := h.store.ApplyObservationReviewAction(r.Context(), id, body.Action, body.ReasonText)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var sce *db.StateConflictError
	if errors.As(err, &sce) {
		writeStateConflict(w, sce)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"observation": rec})
}

// ListTriageQueue serves the rows in the R-A scope with their suggestions.
// Filters, all optional: ?project=, ?session_id= (the triage unit),
// ?review_state= (proposed | lapsed — a FILTER inside the scope, never a
// state of its own), ?pool=de-noise, ?limit=, ?after_id= for paging.
//
// The DE-NOISE pool is the fourth pool: rows carrying an unhandled stage-1
// noise signal. It is filtered SERVER-side, and that is not an optimization —
// a client-side filter over a capped page would hand the operator a page of
// mostly non-noise rows and call it a de-noise batch, while the noise it was
// meant to clear stayed unreachable behind the limit.
//
// The main pools EXCLUDE the de-noise members, so the two never present the
// same row as two different jobs. Note what this endpoint does NOT do: it
// never leaves the pool unset. Empty means the WHOLE scope in the store
// filter, which is the SWEEP's read — the judge must keep seeing
// noise-flagged rows, or the pool would starve of the verdicts that resolve
// its doubt block.
func (h Handler) ListTriageQueue(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := db.TriageScopeFilter{
		Project:     q.Get("project"),
		SessionID:   q.Get("session_id"),
		ReviewState: q.Get("review_state"),
		Pool:        db.TriagePoolMain,
	}
	switch pool := q.Get("pool"); pool {
	case "":
	case db.TriagePoolDeNoise:
		f.Pool = db.TriagePoolDeNoise
	default:
		writeError(w, http.StatusBadRequest, errors.New(`pool must be "de-noise" when present`))
		return
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("limit must be an integer"))
			return
		}
		f.Limit = n
	}
	if v := q.Get("after_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("after_id must be an integer"))
			return
		}
		f.AfterID = n
	}
	if s := f.ReviewState; s != "" && s != "proposed" && s != "lapsed" {
		writeError(w, http.StatusBadRequest, errors.New("review_state must be proposed or lapsed"))
		return
	}
	// The de-noise pool spans proposed AND TTL-lapsed by construction:
	// narrowing it by state would split one gesture in two and hide half the
	// condemned batch behind a control the surface does not show. Cleared
	// AFTER validation, so a malformed value is still refused rather than
	// silently swallowed by the pool.
	if f.Pool == db.TriagePoolDeNoise {
		f.ReviewState = ""
	}
	rows, err := h.store.ListTriageScope(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if rows == nil {
		rows = []db.TriageRow{}
	}
	// The triage surface computes its OWN counters: lapsed rows are
	// excluded from the queue counters by law (memoire:D146 R-6), so
	// reusing those would under-report the scope by exactly the TTL pool
	// this lane exists to work through.
	counts, err := h.store.TriageScopeCountsFor(r.Context(), f.Project, f.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":   rows,
		"counts": counts,
		"total":  len(rows),
	})
}

// ListTriageValidated serves the stage-2 scope: rows a HUMAN validated,
// with their stage-2 suggestions attached. Rules-born accepted rows are
// excluded by the store predicate — they were accepted at birth by a rule
// and nobody validated them, so they are not material for a vouch or an
// elevation.
func (h Handler) ListTriageValidated(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("limit must be an integer"))
			return
		}
		limit = n
	}
	rows, err := h.store.ListTriageValidatedRows(r.Context(), q.Get("project"), q.Get("session_id"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if rows == nil {
		rows = []db.TriageRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "total": len(rows)})
}

// TriageGenres serves every known genre slug for the manual noise input's
// autocomplete: the judge's genres with their evidence, and the operator's
// own named genres. Convergence happens at input time — a slug split
// across spellings crosses the graduation threshold later or never, and
// reconciling append-only logs after the fact would need alias machinery
// nothing else wants.
func (h Handler) TriageGenres(w http.ResponseWriter, r *http.Request) {
	judge, err := h.store.TriageAllGenres(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if judge == nil {
		judge = []db.TriageGenreCandidate{}
	}
	operator, err := h.store.TriageOperatorGenres(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if operator == nil {
		operator = []db.TriageOperatorGenre{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"judge_genres":    judge,
		"operator_genres": operator,
	})
}

// TriageGraduationCandidates serves the R-B graduation candidates: genres
// over the evidence threshold, each with its occurrence count and its
// zero-flip check, plus the command that lands one.
//
// The UI RENDERS the gesture; it cannot perform it. Graduation writes two
// repo files — the prompt-genre addition and the dated record — and the
// server's write authority is the store, not the repo. The honest surface
// says so instead of offering a button that cannot work.
func (h Handler) TriageGraduationCandidates(w http.ResponseWriter, r *http.Request) {
	cands, err := h.store.TriageGraduationCandidates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if cands == nil {
		cands = []db.TriageGenreCandidate{}
	}
	// Operator-named genres ride alongside, EXPLICITLY not as graduation
	// evidence: they are the judge's blind spots, which is prompt feedback,
	// not a case for killing a genre at derivation.
	operator, err := h.store.TriageOperatorGenres(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if operator == nil {
		operator = []db.TriageOperatorGenre{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"candidates":      cands,
		"operator_genres": operator,
		"threshold":       db.TriageGraduationThreshold,
		"gesture":         "memoire-triage -graduate <genre> -repo-root <code checkout> -records-dir <ledger>/records",
	})
}

// TriageOverrideRates serves suggested/applied/flipped by verdict class and
// prompt version — computed from source at read time, never stored beside
// the suggestion (memoire:D245).
func (h Handler) TriageOverrideRates(w http.ResponseWriter, r *http.Request) {
	rates, err := h.store.TriageOverrideRates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if rates == nil {
		rates = []db.TriageOverrideRate{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"override_rates": rates})
}
