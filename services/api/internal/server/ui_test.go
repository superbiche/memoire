// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The embedded review SPA shell is auth-exempt: the app itself attaches the
// bearer token to every /v1 call, so serving the static shell must never
// require one. Whether /ui/ yields the app (200) or 404 depends on whether
// services/ui has been built; the auth contract holds either way.
func TestUIShellIsAuthExempt(t *testing.T) {
	handler, _ := testServer(t, "secret-token")

	for _, path := range []string{"/ui/", "/ui/deep/link"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Fatalf("GET %s = %d, want auth-exempt", path, rec.Code)
		}
		if rec.Code == http.StatusOK && !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("GET %s Content-Type = %q, want text/html", path, rec.Header().Get("Content-Type"))
		}
	}

	// The API surface itself must stay locked.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/observations", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/observations without token = %d, want 401", rec.Code)
	}
}
