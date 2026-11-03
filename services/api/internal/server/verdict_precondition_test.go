// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package server

// Preconditioned verdicts (memoire:D207 residual 1, lean cut): optional
// `expected` snapshot on verdict bodies, checked in-transaction; mismatch =
// unified 409 {code: state_conflict, current: METADATA-ONLY} with NOTHING
// written. The 409 doubles as the reconciliation read for a client whose
// POST lost its response — including states the read surface hides.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func postJSON(t *testing.T, handler http.Handler, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeConflict(t *testing.T, rec *httptest.ResponseRecorder) (code string, current map[string]any, raw map[string]any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("409 body is not JSON: %v — %s", err, rec.Body.String())
	}
	code, _ = raw["code"].(string)
	current, _ = raw["current"].(map[string]any)
	return code, current, raw
}

func reviewLogCount(t *testing.T, handler http.Handler, id, token string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/observations/"+id+"/review-log", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(rec, req)
	var body struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("review-log body: %v", err)
	}
	return body.Total
}

func TestVerdictPreconditionMatchAndMismatch(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	ids := seedSliceE(t, store, "pre-basic", "row A", "row B")

	// Matching precondition behaves exactly like the legacy call.
	rec := postJSON(t, handler, "/v1/observations/"+ids[0]+"/accept", "secret-token",
		`{"expected":{"review_state":"proposed"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("preconditioned accept: got %d — %s", rec.Code, rec.Body.String())
	}

	// A duplicate delivery of the same gesture conflicts and writes NOTHING:
	// same 1 review-log row before and after.
	before := reviewLogCount(t, handler, ids[0], "secret-token")
	rec = postJSON(t, handler, "/v1/observations/"+ids[0]+"/accept", "secret-token",
		`{"expected":{"review_state":"proposed"}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate preconditioned accept: got %d — %s", rec.Code, rec.Body.String())
	}
	code, current, raw := decodeConflict(t, rec)
	if code != "state_conflict" {
		t.Fatalf("conflict code = %q", code)
	}
	if current["review_state"] != "accepted" {
		t.Fatalf("conflict current.review_state = %v", current["review_state"])
	}
	// Metadata only — never the observation text or payload.
	if _, has := current["text"]; has {
		t.Fatal("conflict current leaks text")
	}
	if strings.Contains(rec.Body.String(), "row A") {
		t.Fatalf("conflict body leaks observation content: %s", rec.Body.String())
	}
	_ = raw
	if after := reviewLogCount(t, handler, ids[0], "secret-token"); after != before {
		t.Fatalf("conflict wrote a review-log row: %d -> %d", before, after)
	}

	// Absent precondition keeps legacy behavior (unconditional).
	rec = postJSON(t, handler, "/v1/observations/"+ids[1]+"/accept", "secret-token", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy accept without expected: got %d — %s", rec.Code, rec.Body.String())
	}
}

func TestConfirmPreconditionKillsDoubleWrite(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	ids := seedSliceE(t, store, "pre-confirm", "confirm target")

	rec := postJSON(t, handler, "/v1/observations/"+ids[0]+"/confirm", "secret-token",
		`{"expected":{"review_state":"proposed"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first confirm: got %d — %s", rec.Code, rec.Body.String())
	}
	logs := reviewLogCount(t, handler, ids[0], "secret-token")

	// The duplicate that used to write a second accepted->accepted row
	// (confirm-on-accepted is deliberately legal WITHOUT a precondition,
	// memoire:D192) now conflicts instead.
	rec = postJSON(t, handler, "/v1/observations/"+ids[0]+"/confirm", "secret-token",
		`{"expected":{"review_state":"proposed"}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate confirm: got %d — %s", rec.Code, rec.Body.String())
	}
	_, current, _ := decodeConflict(t, rec)
	if current["review_state"] != "accepted" || current["provenance"] != "user_confirmed" {
		t.Fatalf("conflict current = %v", current)
	}
	if after := reviewLogCount(t, handler, ids[0], "secret-token"); after != logs {
		t.Fatalf("duplicate confirm wrote a review-log row: %d -> %d", logs, after)
	}

	// A confirm on an ALREADY-VOUCHED row is a pure no-op whose duplicate
	// delivery would log a second confirm row (round-2 review NEW BLOCKER
	// 1): it conflicts — and for a lost-response retry that 409 IS the
	// landed signal.
	rec = postJSON(t, handler, "/v1/observations/"+ids[0]+"/confirm", "secret-token",
		`{"expected":{"review_state":"accepted","provenance":"user_confirmed"}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("no-op confirm on vouched row: got %d — %s", rec.Code, rec.Body.String())
	}
	_, current, _ = decodeConflict(t, rec)
	if current["review_state"] != "accepted" || current["provenance"] != "user_confirmed" {
		t.Fatalf("no-op confirm conflict current = %v", current)
	}
	// The D192 pure-provenance promotion (accepted + NOT yet vouched)
	// stays reachable: plain accept leaves provenance inferred, then a
	// truthful confirm-on-accepted promotes it.
	ids2 := seedSliceE(t, store, "pre-confirm-promote", "promotion target")
	rec = postJSON(t, handler, "/v1/observations/"+ids2[0]+"/accept", "secret-token",
		`{"expected":{"review_state":"proposed"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("plain accept: got %d — %s", rec.Code, rec.Body.String())
	}
	rec = postJSON(t, handler, "/v1/observations/"+ids2[0]+"/confirm", "secret-token",
		`{"expected":{"review_state":"accepted","provenance":"inferred"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("D192 promotion confirm: got %d — %s", rec.Code, rec.Body.String())
	}
}

func TestRejectLandedIsReadableThroughConflict(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	ids := seedSliceE(t, store, "pre-reject", "reject target")

	rec := postJSON(t, handler, "/v1/observations/"+ids[0]+"/reject", "secret-token",
		`{"expected":{"review_state":"proposed"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reject: got %d — %s", rec.Code, rec.Body.String())
	}
	// The read surface hides rejected rows (404) — but the retried
	// preconditioned reject reports the landed state in its conflict, which
	// is exactly the verification a lost-response client needs.
	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/v1/observations/"+ids[0]+"?include_derived=true&include_proposed=true&include_noise=true", nil)
	getReq.Header.Set("Authorization", "Bearer secret-token")
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusNotFound {
		t.Fatalf("rejected row visible via GET: %d", getRec.Code)
	}
	rec = postJSON(t, handler, "/v1/observations/"+ids[0]+"/reject", "secret-token",
		`{"expected":{"review_state":"proposed"}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("retried reject: got %d — %s", rec.Code, rec.Body.String())
	}
	code, current, _ := decodeConflict(t, rec)
	if code != "state_conflict" || current["review_state"] != "rejected" {
		t.Fatalf("retried reject conflict = %q %v", code, current)
	}
}

func TestElevationConflictCarriesCurrentMetadata(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	ids := seedSliceE(t, store, "pre-elevate", "elevate target")

	if rec := postJSON(t, handler, "/v1/observations/"+ids[0]+"/confirm", "secret-token", ""); rec.Code != http.StatusOK {
		t.Fatalf("confirm: %d — %s", rec.Code, rec.Body.String())
	}
	if rec := postJSON(t, handler, "/v1/observations/"+ids[0]+"/elevate", "secret-token", ""); rec.Code != http.StatusOK {
		t.Fatalf("elevate: %d — %s", rec.Code, rec.Body.String())
	}
	// "Already elevated" is the landed signal for a lost-response client:
	// the unified conflict shape must carry machine-readable current state.
	rec := postJSON(t, handler, "/v1/observations/"+ids[0]+"/elevate", "secret-token", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("double elevate: got %d — %s", rec.Code, rec.Body.String())
	}
	code, current, _ := decodeConflict(t, rec)
	if code != "state_conflict" {
		t.Fatalf("elevation conflict code = %q", code)
	}
	if current["can_instruct"] != float64(1) || current["review_state"] != "accepted" {
		t.Fatalf("elevation conflict current = %v", current)
	}
}
