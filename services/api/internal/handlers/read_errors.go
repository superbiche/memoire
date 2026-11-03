// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"

	"memoire/services/api/internal/authctx"
	"memoire/services/api/internal/db"
)

// statusClientClosedRequest is the nginx convention for a client that
// disconnected before the response.
const statusClientClosedRequest = 499

const maxLoggedErrorLen = 200

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

func (h Handler) searchStore(ctx context.Context, q string, filter db.ObservationFilter) ([]db.SearchHit, error) {
	if h.searchFn != nil {
		return h.searchFn(ctx, q, filter)
	}
	return h.store.SearchWithFilter(ctx, q, filter)
}

// writeReadError maps a read-handler store error. A query the store rejects
// by its own validation (db.ErrInvalidQuery, db.ErrBadCursor, db.ErrInvalidFilter) stays 400 with
// its message. Anything else is a store fault: 500, a generic body carrying
// the request id, and one server log line with the request id, route,
// SQLSTATE, message, query length and filter. The query text is operator
// content and is never logged.
func (h Handler) writeReadError(w http.ResponseWriter, r *http.Request, err error, queryLen int, filter db.ObservationFilter) {
	if errors.Is(err, db.ErrInvalidQuery) || errors.Is(err, db.ErrBadCursor) || errors.Is(err, db.ErrInvalidFilter) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	requestID := w.Header().Get("X-Request-ID")
	if auth, ok := authctx.From(r.Context()); ok && auth.RequestID != "" {
		requestID = auth.RequestID
	}
	sqlstate := ""
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		sqlstate = pgErr.Code
	}
	level := slog.LevelError
	if errors.Is(err, context.Canceled) {
		// The client went away: not a store fault. Warn keeps it from
		// drowning the SQLSTATE signal.
		level = slog.LevelWarn
	}
	h.logger.Log(r.Context(), level, "read request failed",
		"request_id", requestID,
		"route", r.Method+" "+r.URL.Path,
		"sqlstate", sqlstate,
		"error", truncate(err.Error(), maxLoggedErrorLen),
		"query_len", queryLen,
		"project", filter.Project,
		"kind", filter.Kind,
		"limit", filter.Limit,
	)
	status := http.StatusInternalServerError
	if errors.Is(err, context.Canceled) {
		status = statusClientClosedRequest
	}
	writeJSON(w, status, map[string]any{
		"error":      "internal error while reading the store; quote the request id when reporting",
		"request_id": requestID,
	})
}
