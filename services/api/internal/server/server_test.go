// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
	"memoire/services/api/internal/drainer"
)

func TestAuthAndErrorBehavior(t *testing.T) {
	handler, _ := testServer(t, "secret-token")

	assertStatus(t, handler, http.MethodGet, "/v1/health", "", "", http.StatusOK)
	assertStatus(t, handler, http.MethodGet, "/v1/stats", "", "", http.StatusUnauthorized)
	assertStatus(t, handler, http.MethodGet, "/v1/stats", "wrong-token", "", http.StatusUnauthorized)
	assertStatus(t, handler, http.MethodGet, "/v1/stats", "secret-token", "", http.StatusOK)
	assertStatus(t, handler, http.MethodGet, "/v1/stats?token=secret-token", "", "", http.StatusOK)

	assertStatus(t, handler, http.MethodPost, "/v1/events", "secret-token", `{"project":"missing-fields"}`, http.StatusAccepted)
	// JSON null must queue-and-reject-at-drain like any non-object body,
	// never panic into a 500 the forwarder would treat as systemic and
	// retry forever (slice-3 round-3 finding 1).
	assertStatus(t, handler, http.MethodPost, "/v1/events", "secret-token", `null`, http.StatusAccepted)
}

func TestEventLifecycleThroughHTTP(t *testing.T) {
	handler, store, cfg := testServerWithConfig(t, "secret-token")
	body := `{
		"schema_version":1,
		"project":"memoire-test",
		"session_id":"http-session",
		"source_adapter":"http-test",
		"source_event_id":"http-event-1",
		"event_type":"note",
		"payload":{"message":"http needle"},
		"text":"http needle"
	}`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-token")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	drainer.New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil))).RunOnce(t.Context())

	assertBodyContains(t, handler, http.MethodGet, "/v1/search?q=http+needle&project=memoire-test", "secret-token", "", http.StatusOK, "http")
	assertBodyContains(t, handler, http.MethodGet, "/v1/timeline?project=memoire-test", "secret-token", "", http.StatusOK, "http needle")
	assertBodyContains(t, handler, http.MethodGet, "/v1/observations/1", "secret-token", "", http.StatusOK, "http needle")
	assertBodyContains(t, handler, http.MethodGet, "/v1/sessions/http-session", "secret-token", "", http.StatusOK, `"observation_count":1`)
	assertBodyContains(t, handler, http.MethodPost, "/v1/context?token=secret-token", "", `{"q":"http needle","project":"memoire-test"}`, http.StatusOK, "http-test/note")
	assertStatus(t, handler, http.MethodDelete, "/v1/events/1", "secret-token", "", http.StatusOK)
	assertBodyContains(t, handler, http.MethodGet, "/v1/search?q=http+needle&project=memoire-test", "secret-token", "", http.StatusOK, `"total":0`)

	if count, err := store.AuditCount(t.Context(), "event.ingest"); err != nil || count != 1 {
		t.Fatalf("event.ingest audit count = %d, err=%v", count, err)
	}
	if count, err := store.AuditCount(t.Context(), "event.delete"); err != nil || count != 1 {
		t.Fatalf("event.delete audit count = %d, err=%v", count, err)
	}
}

// The observations list is capped at 200 rows, so sort direction decides
// WHICH rows a caller sees, not just their presentation order. This checks
// the order query param actually wires through the HTTP layer down to the
// store, not just that the store-level unit test passes.
func TestObservationListOrderQueryParam(t *testing.T) {
	handler, store, cfg := testServerWithConfig(t, "secret-token")

	// Ingest the newer-occurring event first so it gets the lower id; the
	// older-occurring event is ingested second and gets the higher id.
	// That decouples "id order" from "occurred_at order" so the assertion
	// below can only pass if order=asc is actually honored.
	newerBody := `{
		"schema_version":1,
		"project":"order-test",
		"session_id":"order-session",
		"source_adapter":"http-test",
		"source_event_id":"order-event-newer",
		"event_type":"note",
		"occurred_at_epoch":200,
		"payload":{"message":"newer"},
		"text":"order needle newer"
	}`
	olderBody := `{
		"schema_version":1,
		"project":"order-test",
		"session_id":"order-session",
		"source_adapter":"http-test",
		"source_event_id":"order-event-older",
		"event_type":"note",
		"occurred_at_epoch":100,
		"payload":{"message":"older"},
		"text":"order needle older"
	}`
	// Drain between the two POSTs: same-millisecond queue files sort by
	// content hash, so a single drain would ingest them in hash order, not
	// POST order — the id assignment this test depends on must not hinge on
	// hash luck.
	assertStatus(t, handler, http.MethodPost, "/v1/events", "secret-token", newerBody, http.StatusAccepted)
	drainer.New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil))).RunOnce(t.Context())
	assertStatus(t, handler, http.MethodPost, "/v1/events", "secret-token", olderBody, http.StatusAccepted)
	drainer.New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil))).RunOnce(t.Context())

	seeded, _, err := store.ListObservations(t.Context(), db.ObservationFilter{Project: "order-test", SessionID: "order-session", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var newerID, olderID int64
	for _, rec := range seeded {
		switch rec.Text {
		case "order needle newer":
			newerID = rec.ID
		case "order needle older":
			olderID = rec.ID
		}
	}
	if newerID == 0 || olderID == 0 {
		t.Fatalf("expected both seeded observations, got %#v", seeded)
	}
	if newerID >= olderID {
		t.Fatalf("expected newer-occurring event to get the lower id, got newerID=%d olderID=%d", newerID, olderID)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/observations?project=order-test&session_id=order-session&order=asc", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	olderIdx := strings.Index(body, strconv.FormatInt(olderID, 10))
	newerIdx := strings.Index(body, strconv.FormatInt(newerID, 10))
	if olderIdx < 0 || newerIdx < 0 {
		t.Fatalf("expected both ids in body, got %s", body)
	}
	if olderIdx >= newerIdx {
		t.Fatalf("expected order=asc to list the older-occurring observation (id=%d) before the newer one (id=%d), body=%s", olderID, newerID, body)
	}

	// A junk order value must fall back to the default descending order at
	// the HTTP surface, not only in the store (public-contract pin).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/observations?project=order-test&session_id=order-session&order=banana", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("junk order status = %d body=%s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	olderIdx = strings.Index(body, strconv.FormatInt(olderID, 10))
	newerIdx = strings.Index(body, strconv.FormatInt(newerID, 10))
	if olderIdx < 0 || newerIdx < 0 {
		t.Fatalf("expected both ids in junk-order body, got %s", body)
	}
	if newerIdx >= olderIdx {
		t.Fatalf("expected junk order value to fall back to descending (newer id=%d before older id=%d), body=%s", newerID, olderID, body)
	}
}

// TestObservationListCursorPagination wires the next_cursor continuation
// token through the HTTP layer: a limit=1 page's next_cursor, fed back in as
// the cursor query param, must walk to the next distinct page rather than
// repeating or skipping, and the final page's next_cursor must serialize as
// JSON null (not an absent key, not an empty string).
func TestObservationListCursorPagination(t *testing.T) {
	handler, store, cfg := testServerWithConfig(t, "secret-token")

	firstBody := `{
		"schema_version":1,
		"project":"cursor-test",
		"session_id":"cursor-session",
		"source_adapter":"http-test",
		"source_event_id":"cursor-event-1",
		"event_type":"note",
		"occurred_at_epoch":100,
		"payload":{"message":"one"},
		"text":"cursor needle one"
	}`
	secondBody := `{
		"schema_version":1,
		"project":"cursor-test",
		"session_id":"cursor-session",
		"source_adapter":"http-test",
		"source_event_id":"cursor-event-2",
		"event_type":"note",
		"occurred_at_epoch":200,
		"payload":{"message":"two"},
		"text":"cursor needle two"
	}`
	assertStatus(t, handler, http.MethodPost, "/v1/events", "secret-token", firstBody, http.StatusAccepted)
	assertStatus(t, handler, http.MethodPost, "/v1/events", "secret-token", secondBody, http.StatusAccepted)
	drainer.New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil))).RunOnce(t.Context())

	// Queue filenames key on a millisecond timestamp plus a content hash
	// (queuewriter.queueFileName), so two POSTs issued within the same
	// millisecond can drain — and get assigned database ids — in either
	// order. Resolve newerID/olderID by content, not by assuming POST order
	// determines id order (same gotcha TestObservationListOrderQueryParam
	// works around above).
	seeded, _, err := store.ListObservations(t.Context(), db.ObservationFilter{Project: "cursor-test", SessionID: "cursor-session", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var newerID, olderID int64
	for _, rec := range seeded {
		switch rec.Text {
		case "cursor needle two":
			newerID = rec.ID
		case "cursor needle one":
			olderID = rec.ID
		}
	}
	if newerID == 0 || olderID == 0 {
		t.Fatalf("expected both seeded observations, got %#v", seeded)
	}

	page1 := getObservationsPage(t, handler, "/v1/observations?project=cursor-test&session_id=cursor-session&limit=1")
	if len(page1.Observations) != 1 {
		t.Fatalf("page1: expected 1 observation, got %#v", page1)
	}
	if page1.NextCursor == nil || *page1.NextCursor == "" {
		t.Fatalf("page1: expected non-empty next_cursor, got %#v", page1)
	}
	if page1.Observations[0].ID != newerID {
		t.Fatalf("page1: expected the newer-occurring observation (id=%d) first in descending order, got id=%d", newerID, page1.Observations[0].ID)
	}

	// page2 is the final page (only 2 observations total, limit=1): its
	// next_cursor must serialize as JSON null, not an empty string or an
	// absent key.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/v1/observations?project=cursor-test&session_id=cursor-session&limit=1&cursor="+*page1.NextCursor, nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("page2 status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"next_cursor":null`) {
		t.Fatalf(`expected literal "next_cursor":null in final page body, got %s`, rec.Body.String())
	}

	page2 := getObservationsPage(t, handler,
		"/v1/observations?project=cursor-test&session_id=cursor-session&limit=1&cursor="+*page1.NextCursor)
	if len(page2.Observations) != 1 {
		t.Fatalf("page2: expected 1 observation, got %#v", page2)
	}
	if page2.Observations[0].ID != olderID {
		t.Fatalf("page2: expected the older-occurring observation (id=%d) to follow page1, got id=%d", olderID, page2.Observations[0].ID)
	}
	if page2.NextCursor != nil {
		t.Fatalf("page2: expected nil next_cursor on the final page, got %q", *page2.NextCursor)
	}

	// A malformed cursor is a 400, not a silently-empty page.
	assertStatus(t, handler, http.MethodGet,
		"/v1/observations?project=cursor-test&session_id=cursor-session&cursor=banana", "secret-token", "", http.StatusBadRequest)
}

type observationsPageResponse struct {
	Observations []struct {
		ID int64 `json:"id"`
	} `json:"observations"`
	Total      int     `json:"total"`
	NextCursor *string `json:"next_cursor"`
}

func getObservationsPage(t *testing.T, handler http.Handler, target string) observationsPageResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d body=%s", target, rec.Code, rec.Body.String())
	}
	var out observationsPageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET %s: decode body: %v, body=%s", target, err, rec.Body.String())
	}
	return out
}

func TestHTTPEventQueuesMalformedEnvelopeForAsyncFailure(t *testing.T) {
	handler, store, cfg := testServerWithConfig(t, "secret-token")

	assertStatus(t, handler, http.MethodPost, "/v1/events", "secret-token", `{`, http.StatusAccepted)
	drainer.New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil))).RunOnce(t.Context())

	if entries, err := filepath.Glob(filepath.Join(cfg.Raw, "*.json")); err != nil {
		t.Fatal(err)
	} else if len(entries) != 1 {
		t.Fatalf("expected raw custody copy for malformed envelope, got %d", len(entries))
	}
	if entries, err := filepath.Glob(filepath.Join(cfg.Queue.Failed, "*.error.json")); err != nil {
		t.Fatal(err)
	} else if len(entries) != 1 {
		t.Fatalf("expected failed error sidecar, got %d", len(entries))
	}
}

func TestCredentialBootstrapLastUsedAndRevocation(t *testing.T) {
	handler, store := testServer(t, "secret-token")

	credID := "local-" + db.TokenHash("secret-token")[:16]
	cred, err := store.CredentialByID(t.Context(), credID)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Prefix != "secret-token" || strings.Join(cred.Scopes, ",") != "read,write,admin" {
		t.Fatalf("credential bootstrap mismatch: %#v", cred)
	}

	assertStatus(t, handler, http.MethodGet, "/v1/stats?token=secret-token", "", "", http.StatusOK)
	cred, err = store.CredentialByID(t.Context(), credID)
	if err != nil {
		t.Fatal(err)
	}
	if cred.LastUsedAtEpoch == 0 {
		t.Fatal("credential last_used_at_epoch was not updated")
	}

	if err := store.RevokeCredential(t.Context(), credID); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, handler, http.MethodGet, "/v1/stats", "secret-token", "", http.StatusUnauthorized)
}

func TestUnauthorizedAudit(t *testing.T) {
	handler, store := testServer(t, "secret-token")

	assertStatus(t, handler, http.MethodGet, "/v1/stats", "wrong-token", "", http.StatusUnauthorized)
	if count, err := store.AuditCount(t.Context(), "auth.reject"); err != nil || count != 1 {
		t.Fatalf("auth.reject audit count = %d, err=%v", count, err)
	}
}

func TestReadOnlyCredentialCannotWrite(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	if _, err := store.EnsureTokenCredential(t.Context(), "read-token", "read-only", []string{"read"}); err != nil {
		t.Fatal(err)
	}

	assertStatus(t, handler, http.MethodGet, "/v1/stats", "read-token", "", http.StatusOK)
	assertStatus(t, handler, http.MethodPost, "/v1/events", "read-token", `{"project":"blocked"}`, http.StatusForbidden)
	if count, err := store.AuditCount(t.Context(), "auth.forbidden"); err != nil || count != 1 {
		t.Fatalf("auth.forbidden audit count = %d, err=%v", count, err)
	}
}

func TestCredentialAdminEndpoints(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	readCred, err := store.EnsureTokenCredential(t.Context(), "read-token", "read-only", []string{"read"})
	if err != nil {
		t.Fatal(err)
	}

	assertBodyContains(t, handler, http.MethodGet, "/v1/credentials", "secret-token", "", http.StatusOK, readCred.ID)
	assertStatus(t, handler, http.MethodGet, "/v1/credentials", "read-token", "", http.StatusForbidden)
	assertStatus(t, handler, http.MethodPost, "/v1/credentials/"+readCred.ID+"/revoke", "secret-token", "", http.StatusOK)
	assertStatus(t, handler, http.MethodGet, "/v1/stats", "read-token", "", http.StatusUnauthorized)

	if count, err := store.AuditCount(t.Context(), "credential.revoke"); err != nil || count != 1 {
		t.Fatalf("credential.revoke audit count = %d, err=%v", count, err)
	}
}

func TestCredentialAdminRefusesSelfRevocation(t *testing.T) {
	handler, _ := testServer(t, "secret-token")
	credID := "local-" + db.TokenHash("secret-token")[:16]

	assertStatus(t, handler, http.MethodPost, "/v1/credentials/"+credID+"/revoke", "secret-token", "", http.StatusBadRequest)
	assertStatus(t, handler, http.MethodGet, "/v1/stats", "secret-token", "", http.StatusOK)
}

func TestObservationReadParityAndAdminReviewTransitions(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	if _, err := store.EnsureTokenCredential(t.Context(), "read-token", "read-only", []string{"read"}); err != nil {
		t.Fatal(err)
	}

	seed := seedReviewHTTPObservations(t, store)

	assertBodyContains(t, handler, http.MethodGet, "/v1/observations?project=memoire-test&session_id=http-review-session", "read-token", "", http.StatusOK, "http accepted decision")
	assertBodyNotContains(t, handler, http.MethodGet, "/v1/observations?project=memoire-test&session_id=http-review-session", "read-token", "", http.StatusOK, "http proposed open question")
	assertBodyContains(t, handler, http.MethodGet, "/v1/observations?project=memoire-test&session_id=http-review-session&kind=open_question", "read-token", "", http.StatusOK, "http proposed open question")
	assertBodyContains(t, handler, http.MethodGet, "/v1/search?q=http+proposed+open+question&project=memoire-test&session_id=http-review-session&kind=open_question", "read-token", "", http.StatusOK, `"review_state":"proposed"`)
	assertBodyContains(t, handler, http.MethodGet, "/v1/timeline?project=memoire-test&session_id=http-review-session&kind=open_question", "read-token", "", http.StatusOK, "http proposed open question")
	assertBodyContains(t, handler, http.MethodPost, "/v1/context?token=read-token", "", `{"q":"http proposed open question","project":"memoire-test","session_id":"http-review-session","kind":"open_question"}`, http.StatusOK, "open_question/proposed")
	// Wire names of the hit governance fields: search and timeline both
	// serve provenance and occurred_at_epoch alongside the legacy
	// captured_at_epoch alias, so a consumer can weigh and date a result
	// line without a per-id fetch.
	for _, path := range []string{
		"/v1/search?q=http+proposed+open+question&project=memoire-test&session_id=http-review-session&kind=open_question",
		"/v1/timeline?project=memoire-test&session_id=http-review-session&kind=open_question",
	} {
		assertBodyContains(t, handler, http.MethodGet, path, "read-token", "", http.StatusOK, `"provenance":"inferred"`)
		assertBodyContains(t, handler, http.MethodGet, path, "read-token", "", http.StatusOK, `"occurred_at_epoch":`)
		assertBodyContains(t, handler, http.MethodGet, path, "read-token", "", http.StatusOK, `"captured_at_epoch":`)
	}

	assertStatus(t, handler, http.MethodPost, "/v1/observations/"+seed.ProposedOpenQuestionPathID+"/review", "read-token", `{"to_state":"accepted","reason_text":"read token blocked"}`, http.StatusForbidden)
	assertBodyContains(t, handler, http.MethodPost, "/v1/observations/"+seed.ProposedOpenQuestionPathID+"/review", "secret-token", `{"to_state":"accepted","reason_text":"looks durable"}`, http.StatusOK, `"review_state":"accepted"`)
	assertBodyContains(t, handler, http.MethodGet, "/v1/observations/"+seed.ProposedOpenQuestionPathID, "read-token", "", http.StatusOK, `"review_state":"accepted"`)
	assertStatus(t, handler, http.MethodGet, "/v1/observations/"+seed.ProposedOpenQuestionPathID+"/review-log", "read-token", "", http.StatusForbidden)
	assertBodyContains(t, handler, http.MethodGet, "/v1/observations/"+seed.ProposedOpenQuestionPathID+"/review-log", "secret-token", "", http.StatusOK, `"observation_id":`+seed.ProposedOpenQuestionPathID)
	assertBodyContains(t, handler, http.MethodGet, "/v1/observations/"+seed.ProposedOpenQuestionPathID+"/review-log", "secret-token", "", http.StatusOK, `"actor":"local-`)
	assertBodyContains(t, handler, http.MethodGet, "/v1/observations/"+seed.ProposedOpenQuestionPathID+"/review-log", "secret-token", "", http.StatusOK, `"to_state":"accepted"`)
	assertBodyContains(t, handler, http.MethodGet, "/v1/observations/"+seed.ProposedOpenQuestionPathID+"/review-log", "secret-token", "", http.StatusOK, `"reason_text":"looks durable"`)
}

func TestObservationReviewEndpointRejectsRawTransitions(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	seed := seedReviewHTTPObservations(t, store)

	assertBodyContains(t, handler, http.MethodPost, "/v1/observations/"+seed.RawPathID+"/review", "secret-token", `{"to_state":"superseded","reason_text":"should fail"}`, http.StatusBadRequest, "raw observations are locked at accepted")
}

func TestDerivationRunInspectionEndpoints(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	if _, err := store.EnsureTokenCredential(t.Context(), "read-token", "read-only", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	seed := seedReviewHTTPObservations(t, store)
	modelRun, err := store.EnsureDerivationRun(t.Context(), db.DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "http-model-session",
		Provider:          "local-qwen",
		Model:             "qwen-14b",
		EndpointID:        "hl3",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:http-model",
		WindowStartEpoch:  700,
		WindowEndEpoch:    710,
		Status:            "completed",
	})
	if err != nil {
		t.Fatal(err)
	}

	assertStatus(t, handler, http.MethodGet, "/v1/derivation-runs?project=memoire-test", "read-token", "", http.StatusForbidden)
	assertBodyContains(t, handler, http.MethodGet, "/v1/derivation-runs?project=memoire-test&session_id=http-review-session&provider=manual&status=completed", "secret-token", "", http.StatusOK, seed.DerivationRunID)
	assertBodyContains(t, handler, http.MethodGet, "/v1/derivation-runs/"+seed.DerivationRunID, "secret-token", "", http.StatusOK, `"observation_count":3`)
	assertBodyContains(t, handler, http.MethodGet, "/v1/derivation-runs/"+seed.DerivationRunID, "secret-token", "", http.StatusOK, `"proposed_observation_count":2`)
	assertBodyContains(t, handler, http.MethodGet, "/v1/derivation-runs/"+seed.DerivationRunID, "secret-token", "", http.StatusOK, `"provider":"manual"`)
	assertBodyContains(t, handler, http.MethodGet, "/v1/derivation-runs?project=memoire-test&provider=local-qwen&model=qwen-14b&endpoint_id=hl3", "secret-token", "", http.StatusOK, modelRun.ID)
	assertBodyContains(t, handler, http.MethodGet, "/v1/derivation-runs/"+modelRun.ID, "secret-token", "", http.StatusOK, `"endpoint_id":"hl3"`)

	assertBodyContains(t, handler, http.MethodGet, "/v1/observations?derivation_run_id="+seed.DerivationRunID+"&review_state=proposed", "read-token", "", http.StatusOK, "http proposed task")
	assertBodyContains(t, handler, http.MethodGet, "/v1/observations?derivation_run_id="+seed.DerivationRunID+"&review_state=proposed", "read-token", "", http.StatusOK, `"review_state":"proposed"`)
}

func TestObservationAcceptRejectEndpoints(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	if _, err := store.EnsureTokenCredential(t.Context(), "read-token", "read-only", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	seed := seedReviewHTTPObservations(t, store)
	wantSourceEvents := `"source_event_ids":[` + strconv.FormatInt(seed.EventID, 10) + `]`

	// Proposed-queue listing surfaces provenance for triage: kind,
	// derivation_run_id, and source_event_ids.
	assertBodyContains(t, handler, http.MethodGet, "/v1/observations?review_state=proposed&project=memoire-test&session_id=http-review-session", "read-token", "", http.StatusOK, `"derivation_run_id":"`+seed.DerivationRunID+`"`)
	assertBodyContains(t, handler, http.MethodGet, "/v1/observations?review_state=proposed&project=memoire-test&session_id=http-review-session", "read-token", "", http.StatusOK, wantSourceEvents)

	// Auth: accept/reject require admin scope; a read token is forbidden.
	assertStatus(t, handler, http.MethodPost, "/v1/observations/"+seed.ProposedOpenQuestionPathID+"/accept", "read-token", `{"reason_text":"blocked"}`, http.StatusForbidden)
	assertStatus(t, handler, http.MethodPost, "/v1/observations/"+seed.ProposedTaskPathID+"/reject", "read-token", `{"reason_text":"blocked"}`, http.StatusForbidden)

	// Accept moves a proposed observation into trusted memory; the
	// response carries the updated state and provenance.
	assertBodyContains(t, handler, http.MethodPost, "/v1/observations/"+seed.ProposedOpenQuestionPathID+"/accept", "secret-token", `{"reason_text":"durable"}`, http.StatusOK, `"review_state":"accepted"`)
	assertBodyContains(t, handler, http.MethodGet, "/v1/observations/"+seed.ProposedOpenQuestionPathID, "read-token", "", http.StatusOK, `"review_state":"accepted"`)
	assertBodyContains(t, handler, http.MethodGet, "/v1/observations/"+seed.ProposedOpenQuestionPathID, "read-token", "", http.StatusOK, wantSourceEvents)

	// Reject removes a proposed observation from the trusted set without losing the record.
	assertBodyContains(t, handler, http.MethodPost, "/v1/observations/"+seed.ProposedTaskPathID+"/reject", "secret-token", `{"reason_text":"noise"}`, http.StatusOK, `"review_state":"rejected"`)

	// Illegal transition: re-accepting an already-accepted observation is a 400, not a silent no-op.
	assertBodyContains(t, handler, http.MethodPost, "/v1/observations/"+seed.ProposedOpenQuestionPathID+"/accept", "secret-token", `{"reason_text":"again"}`, http.StatusBadRequest, "unsupported review transition accepted")
	// Rejecting a raw observation is locked.
	assertBodyContains(t, handler, http.MethodPost, "/v1/observations/"+seed.RawPathID+"/reject", "secret-token", `{"reason_text":"nope"}`, http.StatusBadRequest, "raw observations are locked at accepted")
	// Unknown id is a 404.
	assertStatus(t, handler, http.MethodPost, "/v1/observations/99999999/accept", "secret-token", `{"reason_text":"missing"}`, http.StatusNotFound)

	// Audit trail records exactly the two successful verdicts.
	if count, err := store.AuditCount(t.Context(), "observation.review"); err != nil || count != 2 {
		t.Fatalf("expected 2 observation.review audit rows, got %d err=%v", count, err)
	}
}

// Elevate/demote are operator-surface writes like accept/reject: admin-only
// intent-revealing routes. Elevate requires review_state=accepted (409
// otherwise); demote requires the observation to be currently elevated.
// can_instruct=true is the additive read filter behind the operating-notes
// injection block.
func TestObservationElevateDemoteEndpoints(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	if _, err := store.EnsureTokenCredential(t.Context(), "read-token", "read-only", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	seed := seedReviewHTTPObservations(t, store)
	target := "/v1/observations/" + seed.ProposedOpenQuestionPathID

	// Auth: elevate/demote require admin scope; a read token is forbidden.
	assertStatus(t, handler, http.MethodPost, target+"/elevate", "read-token", `{"reason_text":"blocked"}`, http.StatusForbidden)
	assertStatus(t, handler, http.MethodPost, target+"/demote", "read-token", `{"reason_text":"blocked"}`, http.StatusForbidden)

	// Elevation is a second gate above accept, never a shortcut past it.
	assertBodyContains(t, handler, http.MethodPost, target+"/elevate", "secret-token", `{"reason_text":"too early"}`, http.StatusConflict, "requires review_state=accepted")

	// Two deliberate acts (memoire:D146 R-5): CONFIRM (provenance promotion),
	// then elevate. A plain accept is not enough — elevate never promotes.
	assertBodyContains(t, handler, http.MethodPost, target+"/confirm", "secret-token", `{"reason_text":"durable"}`, http.StatusOK, `"provenance":"user_confirmed"`)
	assertBodyContains(t, handler, http.MethodPost, target+"/elevate", "secret-token", `{"reason_text":"rides every session"}`, http.StatusOK, `"can_instruct":1`)
	assertBodyContains(t, handler, http.MethodGet, target, "read-token", "", http.StatusOK, `"provenance":"user_confirmed"`)

	// The operating-notes read: can_instruct=true narrows list and timeline
	// to elevated observations only.
	assertBodyContains(t, handler, http.MethodGet, "/v1/observations?project=memoire-test&can_instruct=true", "read-token", "", http.StatusOK, "http proposed open question")
	assertBodyNotContains(t, handler, http.MethodGet, "/v1/observations?project=memoire-test&can_instruct=true", "read-token", "", http.StatusOK, "http accepted decision")
	assertBodyContains(t, handler, http.MethodGet, "/v1/timeline?project=memoire-test&exclude_kind=raw&can_instruct=true", "read-token", "", http.StatusOK, "http proposed open question")

	// Double elevate is a 409, not a silent no-op.
	assertBodyContains(t, handler, http.MethodPost, target+"/elevate", "secret-token", `{"reason_text":"again"}`, http.StatusConflict, "already elevated")

	// Demote reverses can_instruct only; provenance stays user_confirmed.
	assertBodyContains(t, handler, http.MethodPost, target+"/demote", "secret-token", `{"reason_text":"stand down"}`, http.StatusOK, `"can_instruct":0`)
	assertBodyContains(t, handler, http.MethodGet, target, "read-token", "", http.StatusOK, `"provenance":"user_confirmed"`)
	assertBodyNotContains(t, handler, http.MethodGet, "/v1/timeline?project=memoire-test&exclude_kind=raw&can_instruct=true", "read-token", "", http.StatusOK, "http proposed open question")

	// Demoting a non-elevated observation is a 409.
	assertBodyContains(t, handler, http.MethodPost, target+"/demote", "secret-token", `{"reason_text":"again"}`, http.StatusConflict, "not elevated")
	// Unknown id is a 404; malformed JSON is a 400.
	assertStatus(t, handler, http.MethodPost, "/v1/observations/99999999/elevate", "secret-token", `{"reason_text":"missing"}`, http.StatusNotFound)
	assertStatus(t, handler, http.MethodPost, target+"/elevate", "secret-token", `{"reason_text":`, http.StatusBadRequest)

	// Audit trail: one elevate, one demote.
	if count, err := store.AuditCount(t.Context(), "observation.elevate"); err != nil || count != 1 {
		t.Fatalf("expected 1 observation.elevate audit row, got %d err=%v", count, err)
	}
	if count, err := store.AuditCount(t.Context(), "observation.demote"); err != nil || count != 1 {
		t.Fatalf("expected 1 observation.demote audit row, got %d err=%v", count, err)
	}
}

// Confirm-on-accepted (memoire:D192): a plain-accepted observation must not
// be a provenance dead end — confirm on an accepted row is a pure provenance
// promotion (state unchanged), which then unlocks elevate. Plain accept on
// an accepted row stays invalid (asserted in the review test above).
func TestConfirmOnAcceptedPromotesProvenance(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	if _, err := store.EnsureTokenCredential(t.Context(), "read-token", "read-only", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	seed := seedReviewHTTPObservations(t, store)
	target := "/v1/observations/" + seed.ProposedOpenQuestionPathID

	// Fast queue pass: plain accept — provenance untouched (R-5 split).
	assertBodyContains(t, handler, http.MethodPost, target+"/accept", "secret-token", `{"reason_text":"seen"}`, http.StatusOK, `"review_state":"accepted"`)
	assertBodyNotContains(t, handler, http.MethodGet, target, "read-token", "", http.StatusOK, `"provenance":"user_confirmed"`)

	// Elevate on accepted-but-unconfirmed is still a 409 (double-gesture
	// invariant holds).
	assertBodyContains(t, handler, http.MethodPost, target+"/elevate", "secret-token", `{"reason_text":"too early"}`, http.StatusConflict, "confirm the observation first")

	// Confirm on the ACCEPTED row: provenance promoted, state unchanged,
	// review-log row written.
	assertBodyContains(t, handler, http.MethodPost, target+"/confirm", "secret-token", `{"reason_text":"I vouch"}`, http.StatusOK, `"provenance":"user_confirmed"`)
	assertBodyContains(t, handler, http.MethodGet, target, "read-token", "", http.StatusOK, `"review_state":"accepted"`)
	assertBodyContains(t, handler, http.MethodGet, target+"/review-log", "secret-token", "", http.StatusOK, `"from_state":"accepted"`)

	// ...and elevate now succeeds: the accepted->confirmed->elevated chain
	// is complete without leaving the observation.
	assertBodyContains(t, handler, http.MethodPost, target+"/elevate", "secret-token", `{"reason_text":"standing order"}`, http.StatusOK, `"can_instruct":1`)
}

func testServer(t *testing.T, token string) (http.Handler, *db.Store) {
	t.Helper()
	handler, store, _ := testServerWithConfig(t, token)
	return handler, store
}

func testServerWithConfig(t *testing.T, token string) (http.Handler, *db.Store, config.Config) {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{
		Addr:      "127.0.0.1:0",
		Home:      home,
		DBPath:    filepath.Join(home, "memoire.db"),
		SchemaDir: "../../../../schema",
		TokenPath: filepath.Join(home, "token"),
		Raw:       filepath.Join(home, "raw"),
		Queue: config.QueuePaths{
			Root:     filepath.Join(home, "queue"),
			Pending:  filepath.Join(home, "queue", "pending"),
			Inflight: filepath.Join(home, "queue", "inflight"),
			Failed:   filepath.Join(home, "queue", "failed"),
			Log:      filepath.Join(home, "log"),
		},
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(cfg.DBPath, cfg.SchemaDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureLocalTokenCredential(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil))), store, cfg
}

func assertStatus(t *testing.T, handler http.Handler, method, target, token, body string, want int) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	handler.ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("%s %s status = %d, want %d body=%s", method, target, rec.Code, want, rec.Body.String())
	}
}

func assertBodyContains(t *testing.T, handler http.Handler, method, target, token, body string, want int, contains string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	handler.ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("%s %s status = %d, want %d body=%s", method, target, rec.Code, want, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), contains) {
		t.Fatalf("%s %s body does not contain %q: %s", method, target, contains, rec.Body.String())
	}
}

func assertBodyNotContains(t *testing.T, handler http.Handler, method, target, token, body string, want int, contains string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	handler.ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("%s %s status = %d, want %d body=%s", method, target, rec.Code, want, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), contains) {
		t.Fatalf("%s %s body unexpectedly contains %q: %s", method, target, contains, rec.Body.String())
	}
}

type reviewHTTPSeed struct {
	RawPathID                  string
	ProposedOpenQuestionPathID string
	ProposedTaskPathID         string
	EventID                    int64
	DerivationRunID            string
}

func seedReviewHTTPObservations(t *testing.T, store *db.Store) reviewHTTPSeed {
	t.Helper()
	eventInput := db.EventInput{
		Project:         "memoire-test",
		SessionID:       "http-review-session",
		SourceAdapter:   "http-test",
		SourceEventID:   "http-review-event",
		EventType:       "note",
		OccurredAtEpoch: 600,
		Payload:         json.RawMessage(`{"message":"http raw visibility needle"}`),
		Text:            "http raw visibility needle",
	}
	eventRes, err := store.Ingest(t.Context(), eventInput)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.EnsureDerivationRun(t.Context(), db.DerivationRunInput{
		Project:           "memoire-test",
		SessionID:         "http-review-session",
		Provider:          "manual",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:http-review",
		WindowStartEpoch:  590,
		WindowEndEpoch:    610,
		Status:            "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertDerivedObservation(t.Context(), db.DerivedObservationInput{
		Project:         "memoire-test",
		SessionID:       "http-review-session",
		Kind:            "decision",
		Text:            "http accepted decision",
		ReviewState:     "accepted",
		DerivationRunID: run.ID,
		ContentHash:     "sha256:http-accepted-decision",
		SourceEventIDs:  []int64{eventRes.ID},
	}); err != nil {
		t.Fatal(err)
	}
	proposed, err := store.InsertDerivedObservation(t.Context(), db.DerivedObservationInput{
		Project:         "memoire-test",
		SessionID:       "http-review-session",
		Kind:            "open_question",
		Text:            "http proposed open question",
		ReviewState:     "proposed",
		DerivationRunID: run.ID,
		ContentHash:     "sha256:http-proposed-open-question",
		SourceEventIDs:  []int64{eventRes.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	proposedTask, err := store.InsertDerivedObservation(t.Context(), db.DerivedObservationInput{
		Project:         "memoire-test",
		SessionID:       "http-review-session",
		Kind:            "task",
		Text:            "http proposed task",
		ReviewState:     "proposed",
		DerivationRunID: run.ID,
		ContentHash:     "sha256:http-proposed-task",
		SourceEventIDs:  []int64{eventRes.ID},
	})
	if err != nil {
		t.Fatal(err)
	}

	visible, _, err := store.ListObservations(t.Context(), db.ObservationFilter{
		Project:   "memoire-test",
		SessionID: "http-review-session",
		Limit:     20,
	})
	if err != nil {
		t.Fatal(err)
	}
	var rawID int64
	for _, rec := range visible {
		if rec.Kind == "raw" {
			rawID = rec.ID
			break
		}
	}
	if rawID == 0 {
		t.Fatal("expected seeded raw observation id")
	}

	return reviewHTTPSeed{
		RawPathID:                  strconv.FormatInt(rawID, 10),
		ProposedOpenQuestionPathID: strconv.FormatInt(proposed.ID, 10),
		ProposedTaskPathID:         strconv.FormatInt(proposedTask.ID, 10),
		EventID:                    eventRes.ID,
		DerivationRunID:            run.ID,
	}
}
