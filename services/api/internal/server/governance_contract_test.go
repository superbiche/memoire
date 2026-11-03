// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package server

import (
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"memoire/services/api/internal/db"

	_ "github.com/mattn/go-sqlite3"
)

func TestGovernanceDisputeEndpointIsThinAdminWrapper(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	if _, err := store.EnsureTokenCredential(t.Context(), "read-token", "read-only", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureTokenCredential(t.Context(), "write-token", "write-only", []string{"write"}); err != nil {
		t.Fatal(err)
	}
	seed := seedReviewHTTPObservations(t, store)

	acceptedID := httpObservationIDByText(t, store, "http accepted decision")
	assertStatus(t, handler, http.MethodPost, "/v1/observations/"+acceptedID+"/dispute", "read-token", `{"reason_text":"blocked"}`, http.StatusForbidden)
	assertStatus(t, handler, http.MethodPost, "/v1/observations/"+acceptedID+"/dispute", "write-token", `{"reason_text":"write token blocked"}`, http.StatusForbidden)
	assertBodyContains(t, handler, http.MethodPost, "/v1/observations/"+acceptedID+"/dispute", "secret-token", `{"reason_text":"human challenge"}`, http.StatusOK, `"review_state":"disputed"`)

	assertBodyContains(t, handler, http.MethodPost, "/v1/observations/"+seed.ProposedTaskPathID+"/dispute", "secret-token", `{"reason_text":"too early"}`, http.StatusBadRequest, "unsupported review transition proposed -> disputed")
}

func TestGovernanceHTTPNoiseFilterDefaultAndOptIn(t *testing.T) {
	handler, store, cfg := testServerWithConfig(t, "secret-token")
	if _, err := store.EnsureTokenCredential(t.Context(), "read-token", "read-only", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	seed := seedReviewHTTPObservations(t, store)
	runID := seed.DerivationRunID
	if _, err := store.InsertDerivedObservation(t.Context(), db.DerivedObservationInput{
		Project:         "memoire-test",
		SessionID:       "http-review-session",
		Kind:            "fact",
		Text:            "http clean governance noise peer",
		ReviewState:     "accepted",
		DerivationRunID: runID,
		ContentHash:     "sha256:http-clean-governance-noise-peer",
		SourceEventIDs:  []int64{seed.EventID},
	}); err != nil {
		t.Fatal(err)
	}

	noisyID := httpObservationIDByText(t, store, "http accepted decision")
	conn, err := sql.Open("sqlite3", cfg.DBPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`UPDATE observations SET noise_candidate = 1 WHERE id = ?`, noisyID); err != nil {
		t.Fatal(err)
	}

	assertBodyNotContains(t, handler, http.MethodGet, "/v1/observations?project=memoire-test&session_id=http-review-session&include_derived=true", "read-token", "", http.StatusOK, "http accepted decision")
	assertBodyContains(t, handler, http.MethodGet, "/v1/observations?project=memoire-test&session_id=http-review-session&include_derived=true", "read-token", "", http.StatusOK, "http clean governance noise peer")
	assertBodyContains(t, handler, http.MethodGet, "/v1/observations?project=memoire-test&session_id=http-review-session&include_noise=true&include_derived=true", "read-token", "", http.StatusOK, "http accepted decision")

	assertBodyNotContains(t, handler, http.MethodGet, "/v1/search?q=http+accepted+decision&project=memoire-test&session_id=http-review-session&include_derived=true", "read-token", "", http.StatusOK, "http accepted decision")
	assertBodyContains(t, handler, http.MethodGet, "/v1/search?q=http+accepted+decision&project=memoire-test&session_id=http-review-session&include_noise=true&include_derived=true", "read-token", "", http.StatusOK, "http accepted decision")

	assertBodyNotContains(t, handler, http.MethodGet, "/v1/timeline?project=memoire-test&session_id=http-review-session&include_derived=true", "read-token", "", http.StatusOK, "http accepted decision")
	assertBodyContains(t, handler, http.MethodGet, "/v1/timeline?project=memoire-test&session_id=http-review-session&include_noise=true&include_derived=true", "read-token", "", http.StatusOK, "http accepted decision")

	contextBody := `{"q":"http accepted decision","project":"memoire-test","session_id":"http-review-session","include_derived":true}`
	assertBodyNotContains(t, handler, http.MethodPost, "/v1/context?token=read-token", "", contextBody, http.StatusOK, "http accepted decision")
	assertBodyContains(t, handler, http.MethodPost, "/v1/context?token=read-token&include_noise=true", "", contextBody, http.StatusOK, "http accepted decision")
}

func TestGovernanceEventCaptureDoesNotGateNoiseCandidatePayload(t *testing.T) {
	handler, _, cfg := testServerWithConfig(t, "secret-token")
	body, err := os.ReadFile("testdata/governance-v1/noisy-event.json")
	if err != nil {
		t.Fatal(err)
	}
	assertBodyContains(t, handler, http.MethodPost, "/v1/events", "secret-token", string(body), http.StatusAccepted, `"queued":true`)

	entries, err := os.ReadDir(cfg.Queue.Pending)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected noisy event payload to be queued, got %d entries in %s", len(entries), cfg.Queue.Pending)
	}
	queued, err := os.ReadFile(filepath.Join(cfg.Queue.Pending, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(queued), "scratch unsourced broad claim that must still be captured") {
		t.Fatalf("queued event lost capture payload: %s", string(queued))
	}
}

func httpObservationIDByText(t *testing.T, store *db.Store, text string) string {
	t.Helper()
	recs, _, err := store.ListObservations(t.Context(), db.ObservationFilter{
		Project:   "memoire-test",
		SessionID: "http-review-session",
		Limit:     50,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if rec.Text == text {
			return strconv.FormatInt(rec.ID, 10)
		}
	}
	t.Fatalf("observation with text %q not found in %#v", text, recs)
	return ""
}
