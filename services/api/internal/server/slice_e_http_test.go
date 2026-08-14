// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package server

// Slice E (memoire:D146) HTTP contract: confirm/lapse/rewrite verdicts,
// session listing, per-session bulk lapse, TTL sweep — all admin-scoped.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"memoire/services/api/internal/db"

	_ "github.com/mattn/go-sqlite3"
)

func seedSliceE(t *testing.T, store *db.Store, session string, texts ...string) []string {
	t.Helper()
	evRes, err := store.Ingest(t.Context(), db.EventInput{
		Project: "memoire-test", SessionID: session, SourceAdapter: "http-test",
		SourceEventID: "slice-e-" + session, EventType: "note",
		OccurredAtEpoch: 600, Payload: json.RawMessage(`{}`), Text: "seed",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.EnsureDerivationRun(t.Context(), db.DerivationRunInput{
		Project: "memoire-test", SessionID: session, Provider: "manual",
		DataBoundary: "local", DerivationVersion: "observation-derive-v1",
		InputHash: "sha256:" + session, WindowStartEpoch: 590, WindowEndEpoch: 610,
		Status: "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(texts))
	for i, text := range texts {
		res, err := store.InsertDerivedObservation(t.Context(), db.DerivedObservationInput{
			Project: "memoire-test", SessionID: session, Kind: "fact", Text: text,
			ReviewState: "proposed", DerivationRunID: run.ID,
			ContentHash: "sha256:" + session + "-" + texts[i], Provenance: "inferred",
			SourceEventIDs: []int64{evRes.ID},
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, intToStr(res.ID))
	}
	return ids
}

func intToStr(id int64) string {
	return json.Number(jsonInt(id)).String()
}

func jsonInt(id int64) string {
	b, _ := json.Marshal(id)
	return string(b)
}

func TestSliceEVerdictRoutes(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	if _, err := store.EnsureTokenCredential(t.Context(), "read-token", "read-only", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	ids := seedSliceE(t, store, "slice-e-verdicts",
		"plain accept keeps provenance", "confirm promotes", "lapse parks", "rewrite supersedes")

	// Plain accept: provenance stays inferred (R-5 split).
	assertBodyContains(t, handler, http.MethodPost, "/v1/observations/"+ids[0]+"/accept", "secret-token", "", http.StatusOK, `"provenance":"inferred"`)
	// Elevate after a PLAIN accept must 409: elevation never promotes
	// provenance — confirm is the only promotion gesture (review finding 1).
	assertBodyContains(t, handler, http.MethodPost, "/v1/observations/"+ids[0]+"/elevate", "secret-token", "", http.StatusConflict, "confirm")
	// Confirm: accepted + user_confirmed.
	assertBodyContains(t, handler, http.MethodPost, "/v1/observations/"+ids[1]+"/confirm", "secret-token", `{"reason_text":"checked"}`, http.StatusOK, `"provenance":"user_confirmed"`)
	// Lapse, then revive on-encounter via accept.
	assertBodyContains(t, handler, http.MethodPost, "/v1/observations/"+ids[2]+"/lapse", "secret-token", "", http.StatusOK, `"review_state":"lapsed"`)
	assertBodyContains(t, handler, http.MethodPost, "/v1/observations/"+ids[2]+"/accept", "secret-token", "", http.StatusOK, `"review_state":"accepted"`)
	// Rewrite: new accepted user_confirmed row supersedes the original.
	assertBodyContains(t, handler, http.MethodPost, "/v1/observations/"+ids[3]+"/rewrite", "secret-token", `{"text":"the corrected statement","reason_text":"wrong port"}`, http.StatusOK, `"provenance":"user_confirmed"`)
	// The original leaves default visibility (superseded); its review log
	// records the supersede.
	assertBodyContains(t, handler, http.MethodGet, "/v1/observations/"+ids[3]+"/review-log", "secret-token", "", http.StatusOK, `"to_state":"superseded"`)

	// Read scope is refused on every verdict.
	assertStatus(t, handler, http.MethodPost, "/v1/observations/"+ids[0]+"/confirm", "read-token", "", http.StatusForbidden)
	assertStatus(t, handler, http.MethodPost, "/v1/observations/"+ids[0]+"/lapse", "read-token", "", http.StatusForbidden)
	assertStatus(t, handler, http.MethodPost, "/v1/observations/"+ids[0]+"/rewrite", "read-token", `{"text":"x"}`, http.StatusForbidden)
}

func TestSliceESessionSurface(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	if _, err := store.EnsureTokenCredential(t.Context(), "read-token", "read-only", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	seedSliceE(t, store, "slice-e-sessions", "one", "two", "three")

	// Session listing carries the review workload (read scope suffices).
	assertBodyContains(t, handler, http.MethodGet, "/v1/sessions?project=memoire-test", "read-token", "", http.StatusOK, `"proposed_count":3`)
	assertBodyContains(t, handler, http.MethodGet, "/v1/sessions?project=memoire-test&proposed_only=true", "read-token", "", http.StatusOK, "slice-e-sessions")

	// Bulk lapse drains the session; read scope refused.
	assertStatus(t, handler, http.MethodPost, "/v1/sessions/slice-e-sessions/lapse-remaining", "read-token", "", http.StatusForbidden)
	assertBodyContains(t, handler, http.MethodPost, "/v1/sessions/slice-e-sessions/lapse-remaining", "secret-token", `{"reason_text":"skimmed"}`, http.StatusOK, `"lapsed_count":3`)
	assertBodyContains(t, handler, http.MethodGet, "/v1/sessions?project=memoire-test&proposed_only=true", "read-token", "", http.StatusOK, `"sessions":[]`)
}

func TestSliceETTLSweepRoute(t *testing.T) {
	handler, store, cfg := testServerWithConfig(t, "secret-token")
	if _, err := store.EnsureTokenCredential(t.Context(), "read-token", "read-only", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	seedSliceE(t, store, "slice-e-ttl", "aged untouched row")
	conn, err := sql.Open("sqlite3", cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`UPDATE observations SET created_at_epoch = created_at_epoch - 40*86400 WHERE session_id = 'slice-e-ttl'`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	assertStatus(t, handler, http.MethodPost, "/v1/admin/lapse-expired", "read-token", "", http.StatusForbidden)
	assertBodyContains(t, handler, http.MethodPost, "/v1/admin/lapse-expired", "secret-token", `{"ttl_days":21}`, http.StatusOK, `"lapsed_count":1`)
	// Idempotent: nothing left to lapse.
	assertBodyContains(t, handler, http.MethodPost, "/v1/admin/lapse-expired", "secret-token", "", http.StatusOK, `"lapsed_count":0`)
}
