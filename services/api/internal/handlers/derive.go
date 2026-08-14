// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"memoire/services/api/internal/modelderive"
)

// Derive intake surface (memoire:D165, R-10.1).
//
// Token-scope contract, stated explicitly (ratified R-10.1 addition):
// POST /v1/derive/notify is a WRITE reachable with a client token — an
// intent only marks work, it never triggers derivation (Q1 sweep-only
// ruling) and never reads back other projects' data.
// GET /v1/derive/outstanding is ADMIN — it aggregates cross-project
// coverage metadata and exists for hub-side operators and the D163 trigger.

// NotifyDerive records a derive intent for a closed session. Idempotent:
// repeat notifies for the same open (project, session_id) collapse into one
// intent (hooks re-fire; the surface is safe under that).
func (h Handler) NotifyDerive(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Project   string `json:"project"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.Project == "" || body.SessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "project and session_id are required"})
		return
	}
	intent, err := h.store.NotifyDeriveIntent(r.Context(), body.Project, body.SessionID, time.Now().Unix())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recorded": true, "intent": intent})
}

// OutstandingDerive reports live canonical events not covered by any
// completed derivation run window, per (project, session) — computed from
// events x derivation_runs on every call (Q2 ruling: no coverage table).
// since_hours bounds the scan (default 24, max 720).
func (h Handler) OutstandingDerive(w http.ResponseWriter, r *http.Request) {
	sinceHours := 24
	if v := r.URL.Query().Get("since_hours"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 720 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "since_hours must be an integer in [1, 720]"})
			return
		}
		sinceHours = n
	}
	since := time.Now().Add(-time.Duration(sinceHours) * time.Hour).Unix()
	work, err := h.store.OutstandingDeriveWork(r.Context(), since, modelderive.CanonicalInputEventTypes())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"since_hours": sinceHours,
		"outstanding": work,
		"total":       len(work),
	})
}
