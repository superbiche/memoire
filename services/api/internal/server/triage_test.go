// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"memoire/services/api/internal/db"
)

// The triage read surface (triage lane v1, R-C). Admin on every verb
// including the GETs: the queue carries cross-project observation text plus
// a judge's reasoning about it, which is the same leak class as the review
// log and the derivation-run ledger.
func TestTriageSurfaceIsAdminOnly(t *testing.T) {
	handler, store := testServer(t, "admin-token")
	if _, err := store.EnsureTokenCredential(t.Context(), "read-token", "read-only", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureTokenCredential(t.Context(), "write-token", "write-only", []string{"write"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/v1/triage/queue",
		"/v1/triage/graduation-candidates",
		"/v1/triage/override-rates",
	} {
		assertStatus(t, handler, http.MethodGet, path, "", "", http.StatusUnauthorized)
		assertStatus(t, handler, http.MethodGet, path, "read-token", "", http.StatusForbidden)
		assertStatus(t, handler, http.MethodGet, path, "write-token", "", http.StatusForbidden)
		assertStatus(t, handler, http.MethodGet, path, "admin-token", "", http.StatusOK)
		// The ?token= read fallback is read-class only; an admin surface
		// must not be reachable through a URL that lands in logs.
		assertStatus(t, handler, http.MethodGet, path+"?token=admin-token", "", "", http.StatusUnauthorized)
	}
}

// The queue serves the R-A scope with its suggestions, and the TTL-lapsed
// pool is a FILTER within that scope rather than a state of its own.
func TestTriageQueueScopeAndCounters(t *testing.T) {
	handler, store := testServer(t, "admin-token")
	seed := seedReviewHTTPObservations(t, store)

	if _, err := store.InsertTriageSuggestion(t.Context(), db.TriageSuggestionInput{
		ObservationID:    mustInt64(t, seed.ProposedTaskPathID),
		Stage:            1,
		PromptVersion:    db.TriagePromptStage1,
		Model:            "judge-model",
		SuggestedVerdict: db.TriageVerdictNoiseLapse,
		ReasonGenre:      "status-narration",
	}); err != nil {
		t.Fatal(err)
	}

	body := getJSON(t, handler, "/v1/triage/queue?session_id=http-review-session", "admin-token")
	rows, _ := body["rows"].([]any)
	if len(rows) == 0 {
		t.Fatal("the triage queue must serve the reviewable scope")
	}
	counts, ok := body["counts"].(map[string]any)
	if !ok {
		t.Fatal("the surface must carry its OWN scope counters")
	}
	if counts["proposed"].(float64) < 1 {
		t.Errorf("proposed counter = %v", counts["proposed"])
	}
	if counts["suggested"].(float64) != 1 {
		t.Errorf("suggested counter = %v, want 1", counts["suggested"])
	}
	// The raw mirror is accepted and locked, so it is not reviewable and
	// must not appear in the triage scope.
	rawID := mustInt64(t, seed.RawPathID)
	for _, r := range rows {
		obs := r.(map[string]any)["observation"].(map[string]any)
		if int64(obs["id"].(float64)) == rawID {
			t.Error("an accepted raw mirror is not reviewable and must stay out of the scope")
		}
	}

	// review_state is a filter INSIDE the scope; anything else is refused
	// rather than silently ignored.
	assertStatus(t, handler, http.MethodGet, "/v1/triage/queue?review_state=proposed", "admin-token", "", http.StatusOK)
	assertStatus(t, handler, http.MethodGet, "/v1/triage/queue?review_state=lapsed", "admin-token", "", http.StatusOK)
	assertStatus(t, handler, http.MethodGet, "/v1/triage/queue?review_state=accepted", "admin-token", "", http.StatusBadRequest)
}

// Graduation is rendered, never performed: the surface hands back the
// command, because both artifacts are repo files and the server's write
// authority is the store.
func TestTriageGraduationSurfaceIsAdvisory(t *testing.T) {
	handler, _ := testServer(t, "admin-token")
	body := getJSON(t, handler, "/v1/triage/graduation-candidates", "admin-token")
	if body["threshold"].(float64) != float64(db.TriageGraduationThreshold) {
		t.Errorf("threshold = %v, want %d", body["threshold"], db.TriageGraduationThreshold)
	}
	if g, _ := body["gesture"].(string); g == "" {
		t.Error("the surface must name the gesture it cannot itself perform")
	}
	if _, ok := body["operator_genres"]; !ok {
		t.Error("operator-named genres must ride alongside, separately from graduation evidence")
	}
	// There is no write verb here — a POST must not be routed.
	assertStatus(t, handler, http.MethodPost, "/v1/triage/graduation-candidates", "admin-token", "{}", http.StatusMethodNotAllowed)
}

func mustInt64(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("path id %q is not an integer: %v", s, err)
	}
	return n
}

func getJSON(t *testing.T, handler http.Handler, target, token string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d body=%s", target, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET %s body is not JSON: %v", target, err)
	}
	return out
}
