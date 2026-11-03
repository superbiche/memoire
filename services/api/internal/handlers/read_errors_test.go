// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"memoire/services/api/internal/authctx"
	"memoire/services/api/internal/db"
)

const secretQuery = "operator-private-needle"

func searchFailing(err error) func(context.Context, string, db.ObservationFilter) ([]db.SearchHit, error) {
	return func(context.Context, string, db.ObservationFilter) ([]db.SearchHit, error) { return nil, err }
}

func TestSearchStoreErrorClassification(t *testing.T) {
	pgErr := fmt.Errorf("search: %w", &pgconn.PgError{Code: "XX000", Message: "unrecognized node type: -1"})
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantLog    bool
	}{
		{"postgres XX000", pgErr, http.StatusInternalServerError, true},
		{"sqlite error", errors.New("database is locked (5)"), http.StatusInternalServerError, true},
		{"deadline", context.DeadlineExceeded, http.StatusInternalServerError, true},
		{"syntax", fmt.Errorf("%w: empty search query", db.ErrInvalidQuery), http.StatusBadRequest, false},
	}
	routes := []struct {
		name string
		call func(h Handler, w http.ResponseWriter, r *http.Request)
		req  func() *http.Request
	}{
		{"GET /v1/search", Handler.SearchGET, func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/v1/search?q="+secretQuery+"&project=p1&kind=decision&limit=7&include_derived=true", nil)
		}},
		{"POST /v1/search", Handler.SearchPOST, func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(
				`{"q":"`+secretQuery+`","project":"p1","kind":"decision","limit":7,"include_derived":true}`))
		}},
		{"POST /v1/context", Handler.Context, func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/v1/context", strings.NewReader(
				`{"q":"`+secretQuery+`","project":"p1","kind":"decision","limit":7,"include_derived":true}`))
		}},
	}
	for _, rt := range routes {
		for _, tc := range tests {
			t.Run(rt.name+"/"+tc.name, func(t *testing.T) {
				var logs bytes.Buffer
				h := Handler{
					logger:   slog.New(slog.NewTextHandler(&logs, nil)),
					searchFn: searchFailing(tc.err),
				}
				r := rt.req()
				r = r.WithContext(authctx.With(r.Context(), authctx.Context{RequestID: "req-abc123"}))
				w := httptest.NewRecorder()
				rt.call(h, w, r)

				if w.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d: %s", w.Code, tc.wantStatus, w.Body)
				}
				var body map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				out := logs.String()
				if !tc.wantLog {
					if body["error"] != tc.err.Error() {
						t.Fatalf("400 body = %v, want the store message %q", body, tc.err)
					}
					if out != "" {
						t.Fatalf("syntax error must not log, got %q", out)
					}
					return
				}
				if body["request_id"] != "req-abc123" {
					t.Fatalf("500 body lacks request id: %v", body)
				}
				if msg, _ := body["error"].(string); strings.Contains(msg, "XX000") || strings.Contains(msg, "node type") {
					t.Fatalf("500 body leaks store detail: %v", body)
				}
				if n := strings.Count(strings.TrimSpace(out), "\n"); n != 0 || out == "" {
					t.Fatalf("want exactly one log line, got %q", out)
				}
				wants := []string{"request_id=req-abc123", `route="` + strings.Fields(rt.name)[0] + " " + strings.Fields(rt.name)[1] + `"`,
					fmt.Sprintf("query_len=%d", len(secretQuery)), "project=p1", "kind=decision", "limit=7"}
				var pg *pgconn.PgError
				if errors.As(tc.err, &pg) {
					wants = append(wants, "sqlstate=XX000", "unrecognized node type: -1")
				}
				for _, want := range wants {
					if !strings.Contains(out, want) {
						t.Fatalf("log line missing %q: %s", want, out)
					}
				}
				if strings.Contains(out, secretQuery) {
					t.Fatalf("log line carries the query text: %s", out)
				}
			})
		}
	}
}

// closedStore opens a real store and closes it, so every read fails with a
// store fault (not a validation error).
func closedStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(t.TempDir()+"/memoire.db", "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return store
}

func readErrorRequest(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	return r.WithContext(authctx.With(r.Context(), authctx.Context{RequestID: "req-xyz"}))
}

// The widened routes (observations, derivation-runs, tool-window) answer 500
// with a request id and one log line on a store fault.
func TestWidenedRoutesStoreFaultIs500(t *testing.T) {
	routes := []struct {
		name string
		call func(h Handler, w http.ResponseWriter, r *http.Request)
		url  string
	}{
		{"observations", Handler.ListObservations, "/v1/observations?project=p1&limit=5"},
		{"derivation-runs", Handler.ListDerivationRuns, "/v1/derivation-runs?project=p1&limit=5"},
		{"tool-window", Handler.ListToolEventsInWindow, "/v1/events/tool-window?session_id=s&project=p1&limit=5"},
	}
	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			var logs bytes.Buffer
			h := Handler{logger: slog.New(slog.NewTextHandler(&logs, nil)), store: closedStore(t)}
			w := httptest.NewRecorder()
			rt.call(h, w, readErrorRequest(http.MethodGet, rt.url))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500: %s", w.Code, w.Body)
			}
			if !strings.Contains(w.Body.String(), `"request_id":"req-xyz"`) {
				t.Fatalf("body lacks request id: %s", w.Body)
			}
			out := logs.String()
			if strings.Count(strings.TrimSpace(out), "\n") != 0 || !strings.Contains(out, "read request failed") || !strings.Contains(out, "request_id=req-xyz") {
				t.Fatalf("want one read-failure log line, got %q", out)
			}
		})
	}
}

// AttachRunSiblings failing after a good search is a store fault: 500,
// generic body, one log line.
func TestSearchAttachSiblingsFailureIs500(t *testing.T) {
	hits := func(context.Context, string, db.ObservationFilter) ([]db.SearchHit, error) {
		return []db.SearchHit{{DerivationRunID: "run-1"}}, nil
	}
	for _, rt := range []struct {
		name string
		call func(h Handler, w http.ResponseWriter, r *http.Request)
		req  func() *http.Request
	}{
		{"GET /v1/search", Handler.SearchGET, func() *http.Request { return readErrorRequest(http.MethodGet, "/v1/search?q=abc") }},
		{"POST /v1/context", Handler.Context, func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/v1/context", strings.NewReader(`{"q":"abc"}`))
			return r.WithContext(authctx.With(r.Context(), authctx.Context{RequestID: "req-xyz"}))
		}},
	} {
		t.Run(rt.name, func(t *testing.T) {
			var logs bytes.Buffer
			h := Handler{logger: slog.New(slog.NewTextHandler(&logs, nil)), store: closedStore(t), searchFn: hits}
			w := httptest.NewRecorder()
			rt.call(h, w, rt.req())
			if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"request_id":"req-xyz"`) {
				t.Fatalf("status/body = %d %s, want 500 with request id", w.Code, w.Body)
			}
			if strings.Contains(w.Body.String(), "closed") {
				t.Fatalf("body leaks store detail: %s", w.Body)
			}
			if out := logs.String(); strings.Count(strings.TrimSpace(out), "\n") != 0 || !strings.Contains(out, "read request failed") {
				t.Fatalf("want one log line, got %q", out)
			}
		})
	}
}

// Tool-window validation errors are client errors.
func TestToolWindowValidationIs400(t *testing.T) {
	var logs bytes.Buffer
	h := Handler{logger: slog.New(slog.NewTextHandler(&logs, nil)), store: closedStore(t)}
	w := httptest.NewRecorder()
	h.ListToolEventsInWindow(w, readErrorRequest(http.MethodGet, "/v1/events/tool-window?session_id=s&start=300&end=100"))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "end must be >= start") {
		t.Fatalf("status/body = %d %s, want 400", w.Code, w.Body)
	}
	if logs.Len() != 0 {
		t.Fatalf("client error must not log: %q", logs.String())
	}
}

// A client disconnect is not a store fault: it logs below Error so it cannot
// drown the SQLSTATE signal.
func TestClientDisconnectLogsBelowError(t *testing.T) {
	var logs bytes.Buffer
	h := Handler{logger: slog.New(slog.NewTextHandler(&logs, nil)), searchFn: searchFailing(fmt.Errorf("search: %w", context.Canceled))}
	w := httptest.NewRecorder()
	h.SearchGET(w, readErrorRequest(http.MethodGet, "/v1/search?q=abc"))
	out := logs.String()
	if strings.Contains(out, "level=ERROR") || !strings.Contains(out, "level=WARN") {
		t.Fatalf("want a WARN line, got %q", out)
	}
	if w.Code != 499 {
		t.Fatalf("status = %d, want 499", w.Code)
	}
}
