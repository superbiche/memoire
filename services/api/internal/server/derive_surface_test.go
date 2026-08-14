// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package server

import (
	"net/http"
	"testing"
)

// Derive intake surface (memoire:D165, R-10.1): notify idempotency is pinned
// by contract (two notifies for the same session = one intent), and the
// token-scope contract is stated — notify is a client-token write,
// outstanding is admin.
func TestDeriveSurface(t *testing.T) {
	handler, store := testServer(t, "secret-token")
	if _, err := store.EnsureTokenCredential(t.Context(), "read-token", "read-only", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureTokenCredential(t.Context(), "client-token", "client", []string{"read", "write"}); err != nil {
		t.Fatal(err)
	}

	// Missing fields and bad JSON are 400s.
	assertStatus(t, handler, http.MethodPost, "/v1/derive/notify", "client-token", `{"project":"p"}`, http.StatusBadRequest)
	assertStatus(t, handler, http.MethodPost, "/v1/derive/notify", "client-token", `{`, http.StatusBadRequest)

	// Idempotency: two notifies for the same (project, session) = one intent
	// with notify_count 2.
	body := `{"project":"derive-surface","session_id":"ds-1"}`
	assertBodyContains(t, handler, http.MethodPost, "/v1/derive/notify", "client-token", body, http.StatusOK, `"notify_count":1`)
	assertBodyContains(t, handler, http.MethodPost, "/v1/derive/notify", "client-token", body, http.StatusOK, `"notify_count":2`)
	open, err := store.OpenDeriveIntents(t.Context())
	if err != nil || len(open) != 1 {
		t.Fatalf("want exactly 1 open intent, got %d err %v", len(open), err)
	}

	// Scope contract: read token cannot notify; client token cannot read
	// outstanding; the admin (local) token can.
	assertStatus(t, handler, http.MethodPost, "/v1/derive/notify", "read-token", body, http.StatusForbidden)
	assertStatus(t, handler, http.MethodGet, "/v1/derive/outstanding", "client-token", "", http.StatusForbidden)
	assertStatus(t, handler, http.MethodGet, "/v1/derive/outstanding", "read-token", "", http.StatusForbidden)
	assertStatus(t, handler, http.MethodGet, "/v1/derive/outstanding", "secret-token", "", http.StatusOK)
	assertStatus(t, handler, http.MethodGet, "/v1/derive/outstanding?since_hours=0", "secret-token", "", http.StatusBadRequest)
	assertStatus(t, handler, http.MethodGet, "/v1/derive/outstanding?since_hours=nope", "secret-token", "", http.StatusBadRequest)
}
