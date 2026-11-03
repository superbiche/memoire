// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"

	"memoire/services/api/internal/authctx"
	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
	"memoire/services/api/internal/handlers"
	"memoire/services/api/internal/ui"
)

// route is one registered surface entry. The table below is DATA on purpose
// (memoire:D172): the HTTP endpoints are the versioned API surface, so the
// contract test derives the served surface from this table and diffs it
// against docs/API-SURFACE.json — a route added or removed here without the
// matching inventory edit fails `make test`.
type route struct {
	method  string
	path    string
	handler func(handlers.Handler) http.HandlerFunc
}

var routes = []route{
	{http.MethodGet, "/v1/health", func(h handlers.Handler) http.HandlerFunc { return h.Health }},
	{http.MethodGet, "/metrics", func(h handlers.Handler) http.HandlerFunc { return h.Metrics }},
	{http.MethodGet, "/v1/readiness", func(h handlers.Handler) http.HandlerFunc { return h.Readiness }},
	{http.MethodGet, "/v1/stats", func(h handlers.Handler) http.HandlerFunc { return h.Stats }},
	{http.MethodGet, "/v1/credentials", func(h handlers.Handler) http.HandlerFunc { return h.ListCredentials }},
	{http.MethodPost, "/v1/credentials/{id}/revoke", func(h handlers.Handler) http.HandlerFunc { return h.RevokeCredential }},
	{http.MethodPost, "/v1/derive/notify", func(h handlers.Handler) http.HandlerFunc { return h.NotifyDerive }},
	{http.MethodGet, "/v1/derive/outstanding", func(h handlers.Handler) http.HandlerFunc { return h.OutstandingDerive }},
	{http.MethodGet, "/v1/derivation-runs", func(h handlers.Handler) http.HandlerFunc { return h.ListDerivationRuns }},
	{http.MethodGet, "/v1/derivation-runs/{id}", func(h handlers.Handler) http.HandlerFunc { return h.GetDerivationRun }},
	{http.MethodPost, "/v1/events", func(h handlers.Handler) http.HandlerFunc { return h.CreateEvent }},
	{http.MethodPost, "/v1/events/batch", func(h handlers.Handler) http.HandlerFunc { return h.CreateEventBatch }},
	{http.MethodGet, "/v1/events/tool-window", func(h handlers.Handler) http.HandlerFunc { return h.ListToolEventsInWindow }},
	{http.MethodGet, "/v1/events/{id}", func(h handlers.Handler) http.HandlerFunc { return h.GetEvent }},
	{http.MethodDelete, "/v1/events/{id}", func(h handlers.Handler) http.HandlerFunc { return h.DeleteEvent }},
	{http.MethodGet, "/v1/observations", func(h handlers.Handler) http.HandlerFunc { return h.ListObservations }},
	{http.MethodGet, "/v1/observations/{id}", func(h handlers.Handler) http.HandlerFunc { return h.GetObservation }},
	{http.MethodGet, "/v1/observations/{id}/review-log", func(h handlers.Handler) http.HandlerFunc { return h.ListObservationReviewLog }},
	{http.MethodGet, "/v1/observations/{id}/usage", func(h handlers.Handler) http.HandlerFunc { return h.ListObservationUsage }},
	{http.MethodPost, "/v1/observations/{id}/review", func(h handlers.Handler) http.HandlerFunc { return h.ReviewObservation }},
	{http.MethodPost, "/v1/observations/{id}/accept", func(h handlers.Handler) http.HandlerFunc { return h.AcceptObservation }},
	{http.MethodPost, "/v1/observations/{id}/reject", func(h handlers.Handler) http.HandlerFunc { return h.RejectObservation }},
	{http.MethodPost, "/v1/observations/{id}/dispute", func(h handlers.Handler) http.HandlerFunc { return h.DisputeObservation }},
	{http.MethodPost, "/v1/observations/{id}/elevate", func(h handlers.Handler) http.HandlerFunc { return h.ElevateObservation }},
	{http.MethodPost, "/v1/observations/{id}/demote", func(h handlers.Handler) http.HandlerFunc { return h.DemoteObservation }},
	{http.MethodPost, "/v1/observations/{id}/confirm", func(h handlers.Handler) http.HandlerFunc { return h.ConfirmObservation }},
	{http.MethodPost, "/v1/observations/{id}/lapse", func(h handlers.Handler) http.HandlerFunc { return h.LapseObservation }},
	{http.MethodPost, "/v1/observations/{id}/rewrite", func(h handlers.Handler) http.HandlerFunc { return h.RewriteObservation }},
	{http.MethodPost, "/v1/observations/{id}/action", func(h handlers.Handler) http.HandlerFunc { return h.ApplyReviewAction }},
	{http.MethodGet, "/v1/sessions", func(h handlers.Handler) http.HandlerFunc { return h.ListSessions }},
	{http.MethodGet, "/v1/sessions/{id}", func(h handlers.Handler) http.HandlerFunc { return h.GetSession }},
	{http.MethodPost, "/v1/sessions/{id}/lapse-remaining", func(h handlers.Handler) http.HandlerFunc { return h.LapseSessionRemaining }},
	{http.MethodPost, "/v1/admin/lapse-expired", func(h handlers.Handler) http.HandlerFunc { return h.LapseExpired }},
	{http.MethodGet, "/v1/triage/queue", func(h handlers.Handler) http.HandlerFunc { return h.ListTriageQueue }},
	{http.MethodGet, "/v1/triage/validated", func(h handlers.Handler) http.HandlerFunc { return h.ListTriageValidated }},
	{http.MethodGet, "/v1/triage/genres", func(h handlers.Handler) http.HandlerFunc { return h.TriageGenres }},
	{http.MethodGet, "/v1/triage/graduation-candidates", func(h handlers.Handler) http.HandlerFunc { return h.TriageGraduationCandidates }},
	{http.MethodGet, "/v1/triage/override-rates", func(h handlers.Handler) http.HandlerFunc { return h.TriageOverrideRates }},
	{http.MethodGet, "/v1/embeddings/missing", func(h handlers.Handler) http.HandlerFunc { return h.EmbeddingsMissing }},
	{http.MethodPut, "/v1/embeddings", func(h handlers.Handler) http.HandlerFunc { return h.EmbeddingsUpsert }},
	{http.MethodGet, "/v1/embeddings/freshness", func(h handlers.Handler) http.HandlerFunc { return h.EmbeddingsFreshness }},
	{http.MethodGet, "/v1/search", func(h handlers.Handler) http.HandlerFunc { return h.SearchGET }},
	{http.MethodPost, "/v1/search", func(h handlers.Handler) http.HandlerFunc { return h.SearchPOST }},
	{http.MethodPost, "/v1/context", func(h handlers.Handler) http.HandlerFunc { return h.Context }},
	{http.MethodGet, "/v1/timeline", func(h handlers.Handler) http.HandlerFunc { return h.Timeline }},

	// Embedded review SPA. Static shell only — every /v1 call it makes still
	// carries the bearer token, so serving the shell is auth-exempt (see auth).
	{http.MethodGet, "/ui/", func(handlers.Handler) http.HandlerFunc { return ui.Handler().ServeHTTP }},
}

func New(cfg config.Config, store *db.Store, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	h := handlers.New(cfg, store, logger)

	for _, rt := range routes {
		mux.HandleFunc(rt.method+" "+rt.path, rt.handler(h))
	}

	return recoverer(logger, auth(store, mux))
}

// authExempt is the auth-exemption predicate, extracted so the D172 contract
// test derives each route's auth class from the REAL rule.
// /metrics is deliberately unauthenticated (infra seam ruling (b),
// inbox 2026-08-03): Prometheus counters/gauges only, no stored
// content — VMAgent scrapes it in-cluster and cannot carry a bearer.
func authExempt(path string) bool {
	return path == "/v1/health" || path == "/v1/readiness" ||
		path == "/metrics" ||
		path == "/ui" || strings.HasPrefix(path, "/ui/")
}

func auth(store *db.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = newRequestID()
		}
		w.Header().Set("X-Request-ID", requestID)
		if authExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		scope := requiredScope(r)
		got, source := bearer(r.Header.Get("Authorization")), "authorization"
		if got == "" && scope == "read" {
			got = r.URL.Query().Get("token")
			source = "query"
		}
		auth, ok, err := store.AuthenticateToken(r.Context(), got, source, requestID)
		if err != nil {
			http.Error(w, "authentication failed", http.StatusInternalServerError)
			return
		}
		if !ok {
			ctx := authctx.With(r.Context(), authctx.Context{RequestID: requestID, Source: source})
			_ = store.Audit(ctx, "auth.reject", "http_request", r.Method+" "+r.URL.Path, map[string]any{"source": source})
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if scope != "" && !auth.HasScope(scope) {
			ctx := authctx.With(r.Context(), auth)
			_ = store.Audit(ctx, "auth.forbidden", "http_request", r.Method+" "+r.URL.Path, map[string]any{"required_scope": scope})
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(authctx.With(r.Context(), auth)))
	})
}

func bearer(header string) string {
	const prefix = "Bearer "
	if strings.HasPrefix(header, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(header, prefix))
	}
	return ""
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-unavailable"
	}
	return hex.EncodeToString(b[:])
}

func requiredScope(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/v1/credentials") {
		return "admin"
	}
	if strings.HasPrefix(r.URL.Path, "/v1/derivation-runs") {
		return "admin"
	}
	// Outstanding-work aggregates cross-project coverage metadata: operator/
	// trigger surface, admin like /v1/derivation-runs. Notify deliberately
	// falls through to the "write" default — the R-10.1 contract states a
	// client token may mark intent (it only marks work; Q1 sweep-only).
	if r.URL.Path == "/v1/derive/outstanding" {
		return "admin"
	}
	if strings.HasPrefix(r.URL.Path, "/v1/observations/") && strings.HasSuffix(r.URL.Path, "/review-log") {
		return "admin"
	}
	if strings.HasPrefix(r.URL.Path, "/v1/observations/") && strings.HasSuffix(r.URL.Path, "/review") {
		return "admin"
	}
	if strings.HasPrefix(r.URL.Path, "/v1/observations/") &&
		(strings.HasSuffix(r.URL.Path, "/accept") || strings.HasSuffix(r.URL.Path, "/reject") || strings.HasSuffix(r.URL.Path, "/dispute") ||
			strings.HasSuffix(r.URL.Path, "/elevate") || strings.HasSuffix(r.URL.Path, "/demote") ||
			strings.HasSuffix(r.URL.Path, "/confirm") || strings.HasSuffix(r.URL.Path, "/lapse") ||
			strings.HasSuffix(r.URL.Path, "/rewrite") || strings.HasSuffix(r.URL.Path, "/action")) {
		return "admin"
	}
	if strings.HasPrefix(r.URL.Path, "/v1/sessions/") && strings.HasSuffix(r.URL.Path, "/lapse-remaining") {
		return "admin"
	}
	if r.URL.Path == "/v1/admin/lapse-expired" {
		return "admin"
	}
	// Event soft-delete is a cross-project visibility gesture (Fibery #616
	// slice C, operator ruling 2026-10-07): every capture hook token carries
	// write, so leaving DELETE on the write default let any hook token — or
	// any hook token leaked into a transcript — hide any event. Admin, like
	// the observation review verbs. The purge path (memoire-purge) never
	// used this route.
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/events/") {
		return "admin"
	}
	// Triage reads carry full observation text across projects plus the
	// judge's reasoning about it — the same operator/triage posture as the
	// review log, admin on every verb including the GETs.
	if strings.HasPrefix(r.URL.Path, "/v1/triage/") {
		return "admin"
	}
	// Embeddings seam (D218): worker-only surface. The work list leaks
	// full observation text and the upsert can poison the vector lane —
	// admin for all three, GETs included.
	if strings.HasPrefix(r.URL.Path, "/v1/embeddings") {
		return "admin"
	}
	if r.Method == http.MethodGet || r.URL.Path == "/v1/context" || r.URL.Path == "/v1/search" || r.URL.Path == "/v1/stats" {
		return "read"
	}
	return "write"
}

func recoverer(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				logger.Error("panic", "value", v)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
