// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"memoire/services/api/internal/db"
)

// L0010 R1 = A: Hindsight owns memory reads. Memoire's default read surfaces
// serve raw capture projections only; derived observations stay in the store
// and are reachable through an explicit include_derived=true opt-in. A
// derived-kind filter without the opt-in is a 400 that names the opt-in, so an
// old client learns why it gets nothing instead of reading an empty answer.
// occurred_after bounds reads to a recent window by occurred time.

type l0010Seed struct {
	DerivedID int64
}

func seedL0010(t *testing.T, store *db.Store) l0010Seed {
	t.Helper()
	now := time.Now()
	ingest := func(id, text string, occurred time.Time) db.IngestResult {
		t.Helper()
		res, err := store.Ingest(t.Context(), db.EventInput{
			Project:         "l0010-test",
			SessionID:       "l0010-session",
			SourceAdapter:   "http-test",
			SourceEventID:   id,
			EventType:       "note",
			OccurredAtEpoch: occurred.Unix(),
			ReceivedAtEpoch: occurred.Unix(),
			Payload:         json.RawMessage(fmt.Sprintf(`{"message":%q}`, text)),
			Text:            text,
		})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	recent := ingest("l0010-recent", "lzeroten recentraw marker", now.Add(-24*time.Hour))
	ingest("l0010-old", "lzeroten oldraw marker", now.Add(-60*24*time.Hour))
	run, err := store.EnsureDerivationRun(t.Context(), db.DerivationRunInput{
		Project:           "l0010-test",
		SessionID:         "l0010-session",
		Provider:          "manual",
		DataBoundary:      "local",
		DerivationVersion: "observation-derive-v1",
		InputHash:         "sha256:l0010",
		WindowStartEpoch:  now.Add(-48 * time.Hour).Unix(),
		WindowEndEpoch:    now.Unix(),
		Status:            "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	derived, err := store.InsertDerivedObservation(t.Context(), db.DerivedObservationInput{
		Project:         "l0010-test",
		SessionID:       "l0010-session",
		Kind:            "decision",
		Text:            "lzeroten deriveddecision marker",
		ReviewState:     "accepted",
		DerivationRunID: run.ID,
		ContentHash:     "sha256:l0010-derived",
		SourceEventIDs:  []int64{recent.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return l0010Seed{DerivedID: derived.ID}
}

func TestDefaultReadsServeRawOnly(t *testing.T) {
	handler, store := testServer(t, "tok")
	seed := seedL0010(t, store)

	type read struct{ method, target, body string }
	defaults := []read{
		{http.MethodGet, "/v1/search?q=lzeroten", ""},
		{http.MethodPost, "/v1/search", `{"q":"lzeroten"}`},
		{http.MethodPost, "/v1/context", `{"q":"lzeroten"}`},
		{http.MethodGet, "/v1/timeline?project=l0010-test", ""},
		{http.MethodGet, "/v1/observations?project=l0010-test", ""},
	}
	for _, rd := range defaults {
		assertBodyContains(t, handler, rd.method, rd.target, "tok", rd.body, http.StatusOK, "recentraw")
		assertBodyNotContains(t, handler, rd.method, rd.target, "tok", rd.body, http.StatusOK, "deriveddecision")
	}

	optIn := []read{
		{http.MethodGet, "/v1/search?q=lzeroten&include_derived=true", ""},
		{http.MethodPost, "/v1/search", `{"q":"lzeroten","include_derived":true}`},
		{http.MethodPost, "/v1/context", `{"q":"lzeroten","include_derived":true}`},
		{http.MethodGet, "/v1/timeline?project=l0010-test&include_derived=true", ""},
		{http.MethodGet, "/v1/observations?project=l0010-test&include_derived=true", ""},
	}
	for _, rd := range optIn {
		assertBodyContains(t, handler, rd.method, rd.target, "tok", rd.body, http.StatusOK, "deriveddecision")
	}

	byID := fmt.Sprintf("/v1/observations/%d", seed.DerivedID)
	assertStatus(t, handler, http.MethodGet, byID, "tok", "", http.StatusNotFound)
	assertBodyContains(t, handler, http.MethodGet, byID+"?include_derived=true", "tok", "", http.StatusOK, "deriveddecision")
}

func TestDerivedKindFilterWithoutOptInIsRejected(t *testing.T) {
	handler, store := testServer(t, "tok")
	seedL0010(t, store)

	for _, rd := range []struct{ method, target, body string }{
		{http.MethodGet, "/v1/search?q=lzeroten&kind=decision", ""},
		{http.MethodGet, "/v1/search?q=lzeroten&exclude_kind=raw", ""},
		{http.MethodPost, "/v1/search", `{"q":"lzeroten","kind":"decision"}`},
		{http.MethodPost, "/v1/context", `{"q":"lzeroten","exclude_kind":"raw"}`},
		{http.MethodGet, "/v1/timeline?exclude_kind=raw", ""},
		{http.MethodGet, "/v1/observations?kind=decision", ""},
	} {
		assertBodyContains(t, handler, rd.method, rd.target, "tok", rd.body, http.StatusBadRequest, "include_derived")
	}
	// Explicit kind=raw is the default made explicit, not an error.
	assertBodyContains(t, handler, http.MethodGet, "/v1/search?q=lzeroten&kind=raw", "tok", "", http.StatusOK, "recentraw")
	// With the opt-in, derived-kind filters behave as before.
	assertBodyContains(t, handler, http.MethodGet, "/v1/search?q=lzeroten&kind=decision&include_derived=true", "tok", "", http.StatusOK, "deriveddecision")
	assertBodyNotContains(t, handler, http.MethodGet, "/v1/search?q=lzeroten&exclude_kind=raw&include_derived=true", "tok", "", http.StatusOK, "recentraw")
}

func TestOccurredAfterBoundsReads(t *testing.T) {
	handler, store := testServer(t, "tok")
	seedL0010(t, store)
	cut := time.Now().Add(-30 * 24 * time.Hour).Unix()

	for _, rd := range []struct{ method, target, body string }{
		{http.MethodGet, fmt.Sprintf("/v1/search?q=lzeroten&occurred_after=%d", cut), ""},
		{http.MethodPost, "/v1/search", fmt.Sprintf(`{"q":"lzeroten","occurred_after":%d}`, cut)},
		{http.MethodPost, "/v1/context", fmt.Sprintf(`{"q":"lzeroten","occurred_after":%d}`, cut)},
		{http.MethodGet, fmt.Sprintf("/v1/timeline?project=l0010-test&occurred_after=%d", cut), ""},
	} {
		assertBodyContains(t, handler, rd.method, rd.target, "tok", rd.body, http.StatusOK, "recentraw")
		assertBodyNotContains(t, handler, rd.method, rd.target, "tok", rd.body, http.StatusOK, "oldraw")
	}
	// Context lines carry the occurrence date (L0010: dates on every hit).
	recentDay := time.Now().Add(-24 * time.Hour).UTC().Format("2006-01-02")
	assertBodyContains(t, handler, http.MethodPost, "/v1/context", "tok", `{"q":"recentraw"}`, http.StatusOK, "] "+recentDay+" l0010-test ")
	// Without the bound, old raw rows stay readable (custody is keep-all).
	assertBodyContains(t, handler, http.MethodGet, "/v1/search?q=lzeroten", "tok", "", http.StatusOK, "oldraw")
	// A malformed bound is a 400, not a silent full-history read.
	assertStatus(t, handler, http.MethodGet, "/v1/search?q=lzeroten&occurred_after=yesterday", "tok", "", http.StatusBadRequest)
}
