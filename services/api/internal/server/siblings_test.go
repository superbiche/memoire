// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"memoire/services/api/internal/db"
)

// Lineage at retrieval (Fibery #335): every retrieval response carries a
// hit's run-siblings, and the prompt-ready context string renders them as a
// continuation line. The four routes go through two store reads, so the
// test walks all four rather than trusting one.
func TestRunSiblingsOnRetrievalRoutes(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	ctx := t.Context()

	project, session := "siblings-http", "sess-siblings-http"
	evt, err := store.Ingest(ctx, db.EventInput{
		Project: project, SessionID: session, SourceAdapter: "http-test",
		SourceEventID: "siblings-evt-1", EventType: "note",
		OccurredAtEpoch: 1800000000,
		Payload:         json.RawMessage(`{"message":"sibling source"}`),
		Text:            "sibling source",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.EnsureDerivationRun(ctx, db.DerivationRunInput{
		Project: project, SessionID: session, Provider: "manual", DataBoundary: "local",
		DerivationVersion: "observation-derive-v1", InputHash: "sha256:siblings-http",
		WindowStartEpoch: 1799999990, WindowEndEpoch: 1800000010, Status: "completed",
	})
	if err != nil {
		t.Fatal(err)
	}

	insert := func(kind, text, hash string, occurred int64) int64 {
		rec, err := store.InsertDerivedObservation(ctx, db.DerivedObservationInput{
			Project: project, SessionID: session, Kind: kind, Text: text,
			DerivationRunID: run.ID, ContentHash: hash, SourceEventIDs: []int64{evt.ID},
			OccurredAtEpoch: occurred,
		})
		if err != nil {
			t.Fatal(err)
		}
		return rec.ID
	}
	decisionID := insert("decision", "rename splendiferous to human", "sha256:siblings-decision", 1800000001)
	taskID := insert("task", "carry out the splendiferous rename", "sha256:siblings-task", 1800000002)
	sweepID := insert("task", "unrelated inbox sweep", "sha256:siblings-sweep", 1800000003)

	wantRun := fmt.Sprintf(`"derivation_run_id":%q`, run.ID)
	// The kinds are id-ordered inside the continuation line, so the shape
	// is pinned byte-for-byte, not just probed for a substring.
	wantLine := fmt.Sprintf("  ↳ same run: [%d] task, [%d] task", taskID, sweepID)

	// GET /v1/search
	body := requestBody(t, handler, http.MethodGet,
		"/v1/search?q=splendiferous&project="+project+"&include_derived=true", "secret-token", "")
	assertHitSiblings(t, body, decisionID, []int64{taskID, sweepID})
	if !strings.Contains(body, wantRun) {
		t.Fatalf("GET /v1/search hit missing %s: %s", wantRun, body)
	}

	// POST /v1/search
	body = requestBody(t, handler, http.MethodPost, "/v1/search", "secret-token",
		fmt.Sprintf(`{"q":"splendiferous","project":%q,"include_derived":true}`, project))
	assertHitSiblings(t, body, decisionID, []int64{taskID, sweepID})

	// GET /v1/timeline
	body = requestBody(t, handler, http.MethodGet,
		"/v1/timeline?project="+project+"&exclude_kind=raw&include_derived=true", "secret-token", "")
	assertHitSiblings(t, body, decisionID, []int64{taskID, sweepID})

	// POST /v1/context: both the hits array and the prompt-ready string.
	body = requestBody(t, handler, http.MethodPost, "/v1/context", "secret-token",
		fmt.Sprintf(`{"q":"splendiferous","project":%q,"include_derived":true}`, project))
	assertHitSiblings(t, body, decisionID, []int64{taskID, sweepID})
	var ctxResp struct {
		Context string `json:"context"`
	}
	if err := json.Unmarshal([]byte(body), &ctxResp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ctxResp.Context, wantLine) {
		t.Fatalf("context string missing %q:\n%s", wantLine, ctxResp.Context)
	}
	// The continuation line sits directly under its hit's line.
	lines := strings.Split(ctxResp.Context, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "  ↳ ") {
			continue
		}
		if i == 0 || strings.HasPrefix(lines[i-1], "  ↳ ") {
			t.Fatalf("continuation line %d is not under a hit line:\n%s", i, ctxResp.Context)
		}
	}

	// GET /v1/observations/{id}: siblings sit next to the record.
	body = requestBody(t, handler, http.MethodGet,
		fmt.Sprintf("/v1/observations/%d?include_derived=true", decisionID), "secret-token", "")
	var byID struct {
		Observation struct {
			ID int64 `json:"id"`
		} `json:"observation"`
		Siblings []db.ObservationSibling `json:"siblings"`
	}
	if err := json.Unmarshal([]byte(body), &byID); err != nil {
		t.Fatal(err)
	}
	if byID.Observation.ID != decisionID {
		t.Fatalf("get-by-id returned observation %d, want %d", byID.Observation.ID, decisionID)
	}
	assertSiblingIDsJSON(t, byID.Siblings, []int64{taskID, sweepID})
}

// A hit with no run-siblings renders exactly as before: no `siblings` key in
// the JSON, no continuation line in the context string. Raw capture
// projections have no derivation run at all, which is the cheap case the
// early return serves.
func TestNoRunSiblingsLeavesRetrievalUnchanged(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	ctx := t.Context()

	project := "siblings-http-none"
	if _, err := store.Ingest(ctx, db.EventInput{
		Project: project, SessionID: "sess-none", SourceAdapter: "http-test",
		SourceEventID: "siblings-none-1", EventType: "note",
		OccurredAtEpoch: 1800000000,
		Payload:         json.RawMessage(`{"message":"lonesome needle"}`),
		Text:            "lonesome needle",
	}); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{
		"/v1/search?q=lonesome&project=" + project,
		"/v1/timeline?project=" + project,
		"/v1/observations/1",
	} {
		body := requestBody(t, handler, http.MethodGet, target, "secret-token", "")
		if strings.Contains(body, `"siblings"`) {
			t.Fatalf("%s must not serve a siblings key when there are none: %s", target, body)
		}
		if strings.Contains(body, `"derivation_run_id"`) {
			t.Fatalf("%s must omit an empty derivation_run_id: %s", target, body)
		}
	}

	body := requestBody(t, handler, http.MethodPost, "/v1/context", "secret-token",
		fmt.Sprintf(`{"q":"lonesome","project":%q}`, project))
	var ctxResp struct {
		Context string `json:"context"`
	}
	if err := json.Unmarshal([]byte(body), &ctxResp); err != nil {
		t.Fatal(err)
	}
	if ctxResp.Context == "" {
		t.Fatalf("context string is empty: %s", body)
	}
	if strings.Contains(ctxResp.Context, "↳") {
		t.Fatalf("context string must carry no continuation line:\n%s", ctxResp.Context)
	}
}

// The fetchability invariant (operator ruling, 2026-09-12): a sibling id a
// caller receives must be an id that same caller can fetch with the same
// filter. A sibling is a pointer, and a pointer that 404s is worse than no
// pointer. This walks every sibling of every hit under a spread of filter
// combinations and asserts GET /v1/observations/{id} answers 200 under the
// same query string.
func TestEverySiblingIsFetchableUnderTheSameFilter(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	ctx := t.Context()

	project, session := "siblings-fetchable", "sess-siblings-fetchable"
	evt, err := store.Ingest(ctx, db.EventInput{
		Project: project, SessionID: session, SourceAdapter: "http-test",
		SourceEventID: "fetchable-evt-1", EventType: "note",
		OccurredAtEpoch: 1800000000,
		Payload:         json.RawMessage(`{"message":"fetchable source"}`),
		Text:            "fetchable source",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.EnsureDerivationRun(ctx, db.DerivationRunInput{
		Project: project, SessionID: session, Provider: "manual", DataBoundary: "local",
		DerivationVersion: "observation-derive-v1", InputHash: "sha256:siblings-fetchable",
		WindowStartEpoch: 1799999990, WindowEndEpoch: 1800000010, Status: "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	insert := func(kind, text, hash, state string, noise bool) int64 {
		rec, err := store.InsertDerivedObservation(ctx, db.DerivedObservationInput{
			Project: project, SessionID: session, Kind: kind, Text: text,
			DerivationRunID: run.ID, ContentHash: hash, SourceEventIDs: []int64{evt.ID},
			ReviewState: state, NoiseCandidate: noise, OccurredAtEpoch: 1800000001,
		})
		if err != nil {
			t.Fatal(err)
		}
		return rec.ID
	}

	// A run holding one of every awkward shape: accepted, unreviewed and
	// noise-flagged, so every filter combination below has something to
	// include or exclude.
	insert("decision", "quinquagenarian rename ruling", "sha256:fetchable-decision", "accepted", false)
	insert("task", "carry out the quinquagenarian rename", "sha256:fetchable-task", "accepted", false)
	insert("fact", "quinquagenarian noise", "sha256:fetchable-noisy", "accepted", true)
	insert("fact", "quinquagenarian unreviewed", "sha256:fetchable-proposed", "proposed", false)

	// Each query string is applied to BOTH the search and the by-id read,
	// which is what "the same filter" means on this surface. L0010: the
	// hits and siblings below are derived, so every combination carries the
	// include_derived opt-in — without it the raw-only default would hide
	// the hits themselves.
	for _, qs := range []string{
		"&include_derived=true",
		"&include_derived=true&include_proposed=true",
		"&include_derived=true&include_noise=true",
		"&include_derived=true&include_proposed=true&include_noise=true",
		"&include_derived=true&include_agent_accepted=true",
		"&include_derived=true&include_proposed=true&include_noise=true&include_agent_accepted=true",
		"&include_derived=true&review_state=accepted",
		"&include_derived=true&exclude_kind=raw",
	} {
		t.Run("filter"+qs, func(t *testing.T) {
			body := requestBody(t, handler, http.MethodGet,
				"/v1/search?q=quinquagenarian&project="+project+qs, "secret-token", "")
			var resp struct {
				Hits []struct {
					ID       int64                   `json:"id"`
					Siblings []db.ObservationSibling `json:"siblings"`
				} `json:"hits"`
			}
			if err := json.Unmarshal([]byte(body), &resp); err != nil {
				t.Fatal(err)
			}
			seen := 0
			for _, hit := range resp.Hits {
				for _, sib := range hit.Siblings {
					seen++
					target := fmt.Sprintf("/v1/observations/%d?%s", sib.ID, strings.TrimPrefix(qs, "&"))
					rec := httptest.NewRecorder()
					req := httptest.NewRequest(http.MethodGet, target, nil)
					req.Header.Set("Authorization", "Bearer secret-token")
					handler.ServeHTTP(rec, req)
					if rec.Code != http.StatusOK {
						t.Fatalf("sibling %d of hit %d is not fetchable under the same filter: GET %s = %d %s",
							sib.ID, hit.ID, target, rec.Code, rec.Body.String())
					}
				}
			}
			// A vacuous pass would prove nothing, so the walk must have
			// walked something on every filter combination.
			if seen == 0 {
				t.Fatalf("no siblings surfaced under %q, so the invariant went unchecked: %s", qs, body)
			}
		})
	}
}

func requestBody(t *testing.T, handler http.Handler, method, target, token, body string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s status = %d body=%s", method, target, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func assertHitSiblings(t *testing.T, body string, hitID int64, want []int64) {
	t.Helper()
	var resp struct {
		Hits []struct {
			ID       int64                   `json:"id"`
			Text     string                  `json:"text"`
			Siblings []db.ObservationSibling `json:"siblings"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	for _, hit := range resp.Hits {
		if hit.ID != hitID {
			continue
		}
		assertSiblingIDsJSON(t, hit.Siblings, want)
		for _, sib := range hit.Siblings {
			if sib.Kind == "" || sib.ReviewState == "" {
				t.Fatalf("sibling %d must carry kind and review_state: %+v", sib.ID, sib)
			}
		}
		return
	}
	t.Fatalf("hit %d not in response: %s", hitID, body)
}

func assertSiblingIDsJSON(t *testing.T, siblings []db.ObservationSibling, want []int64) {
	t.Helper()
	if len(siblings) != len(want) {
		t.Fatalf("siblings = %+v, want ids %v", siblings, want)
	}
	for i, id := range want {
		if siblings[i].ID != id {
			t.Fatalf("siblings[%d].ID = %d, want %d (full %+v)", i, siblings[i].ID, id, siblings)
		}
	}
}

// An empty query is the store's own validation: it stays 400 with the message
// on every search route (the 500 classification is covered in handlers).
func TestSearchEmptyQueryStays400(t *testing.T) {
	handler, _ := testServer(t, "secret-token")
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/search?q=%20", ""},
		{http.MethodPost, "/v1/search", `{"q":"  "}`},
		{http.MethodPost, "/v1/context", `{"q":"  "}`},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer secret-token")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "empty search query") {
			t.Fatalf("%s %s = %d %s, want 400 with the store message", tc.method, tc.path, rec.Code, rec.Body)
		}
	}
}
