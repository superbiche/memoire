// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package embedclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func embedServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embed" || r.Method != http.MethodPost {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEmbedQuery(t *testing.T) {
	srv := embedServer(t, `{"model":"m1","dim":3,"vector":[0.1,0.2,0.3]}`)
	vec, err := New(srv.URL, "tok", "m1").EmbedQuery(context.Background(), "hello")
	if err != nil || len(vec) != 3 {
		t.Fatalf("EmbedQuery: vec=%v err=%v", vec, err)
	}
}

// Round-1 F2: a response from a different model is an error, never a
// vector — a wrong-model embedding would rank against stored vectors as
// silent garbage instead of triggering the fail-open path.
func TestEmbedQueryRejectsWrongModel(t *testing.T) {
	srv := embedServer(t, `{"model":"other-384","dim":3,"vector":[0.1,0.2,0.3]}`)
	_, err := New(srv.URL, "tok", "m1").EmbedQuery(context.Background(), "hello")
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("want model-identity error, got %v", err)
	}
}

func TestEmbedQueryRejectsDimMismatch(t *testing.T) {
	srv := embedServer(t, `{"model":"m1","dim":4,"vector":[0.1,0.2,0.3]}`)
	_, err := New(srv.URL, "tok", "m1").EmbedQuery(context.Background(), "hello")
	if err == nil || !strings.Contains(err.Error(), "dim") {
		t.Fatalf("want dim-mismatch error, got %v", err)
	}
}

func TestEmbedQueryRejectsHTTPError(t *testing.T) {
	srv := embedServer(t, `{}`)
	_, err := New(srv.URL, "wrong-token", "m1").EmbedQuery(context.Background(), "hello")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want HTTP 401 error, got %v", err)
	}
}
