// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"memoire/services/api/internal/authctx"
	"memoire/services/api/internal/buildinfo"
	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
	"memoire/services/api/internal/queuewriter"
)

type Handler struct {
	cfg    config.Config
	store  *db.Store
	logger *slog.Logger
	// metrics is the /metrics TTL cache for the two expensive series; a
	// pointer so every by-value Handler copy shares the one cache.
	metrics *metricsCache
	// reads counts read routes and the age of what they return (L0009);
	// a pointer for the same reason as metrics.
	reads *readTelemetry
	// searchFn is a test seam: nil in production, where searchStore calls
	// the store directly.
	searchFn func(ctx context.Context, q string, filter db.ObservationFilter) ([]db.SearchHit, error)
}

func New(cfg config.Config, store *db.Store, logger *slog.Logger) Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return Handler{cfg: cfg, store: store, logger: logger, metrics: newMetricsCache(store), reads: newReadTelemetry()}
}

// healthStoreTimeout bounds EACH store read behind /v1/health. The liveness
// probe calls this endpoint, so it must answer well inside the probe timeout
// even when the database is slow. Each read has its own budget, so one slow
// read never expires the next.
const healthStoreTimeout = 3 * time.Second

// healthRead runs one store read under its own healthStoreTimeout budget.
func healthRead[T any](parent context.Context, read func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(parent, healthStoreTimeout)
	defer cancel()
	return read(ctx)
}

// healthCount is a lifetime counter for the health body: null when the read
// failed, so a consumer never mistakes "unknown" for a count of 0.
func healthCount(n int64, err error) any {
	if err != nil {
		return nil
	}
	return n
}

func (h Handler) Health(w http.ResponseWriter, r *http.Request) {
	// HealthStats, not Stats: the exact counts are full table scans (11 s on
	// the hub's 26 GB observations table, which crash-looped the hub under
	// liveness on 2026-10-29), and this body reports none of them. Health
	// stays best-effort: a failed read keeps what was read.
	st, _ := healthRead(r.Context(), func(ctx context.Context) (db.Stats, error) {
		return h.store.HealthStats(ctx, h.cfg.DBPath)
	})
	var lastWrite any
	if st.LastWriteKnown {
		lastWrite = st.LastWriteEpoch
	}
	// Both additions are memoire:D136: queue_failed_depth makes the poison
	// residual visible (R-3), redaction_contract_violations surfaces
	// liaison-evidence backstop rewrites (R-2). Both counters below are
	// null when their read fails or overruns its budget.
	violations := healthCount(healthRead(r.Context(), h.store.CountRedactionContractViolations))
	// clock_skew_future_events is the memoire:D152 one-sided future-skew
	// alert (occurred_at ahead of received_at beyond the threshold = producer
	// clock fault; the lag direction is queue buffering and never alerted).
	// 0 threshold disables (reported as 0, not null).
	var futureSkew any = int64(0)
	if h.cfg.SkewAlertSeconds > 0 {
		futureSkew = healthCount(healthRead(r.Context(), func(ctx context.Context) (int64, error) {
			return h.store.CountFutureSkewEvents(ctx, h.cfg.SkewAlertSeconds)
		}))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		// Stamped from the repo's VERSION file at build time (MISSING #7);
		// "dev" on unstamped builds. Public() bounds the stamp to a version
		// token — this body is unauthenticated (D224; review F1).
		"version":                  buildinfo.Public(),
		"queue_depth":              queueDepth(h.cfg.Queue.Pending),
		"queue_failed_depth":       failedQueueDepth(h.cfg.Queue.Failed),
		"oldest_queue_age_seconds": oldestQueueAgeSeconds(h.cfg.Queue.Pending),
		// null until the exact seed behind HealthStats has landed: a lower
		// bound must not read as the last write (write-stall checks).
		"last_write_epoch":              lastWrite,
		"db_bytes":                      st.DBBytes,
		"wal_bytes":                     st.WALBytes,
		"redaction_contract_violations": violations,
		"clock_skew_future_events":      futureSkew,
		// search_engine makes an accidental tsvector fallback VISIBLE when a
		// pg_search deployment loses its extension/index (D160 round-1
		// finding 1): fts5 | bm25 | tsvector.
		"search_engine": h.store.SearchEngine(),
		// vector_lane (D218): pgvector | absent. Freshness (the RUNBOOK 2h
		// look-line) lives on GET /v1/embeddings/freshness — its anti-join
		// hashes every derived text, far too heavy for probe-frequency
		// health calls.
		"vector_lane": h.store.VectorLane(),
		// hybrid_search (D218 session B): absent | off | on — the
		// deployment default, not any per-request override. The fail-open
		// counter makes a dead embed service visible: it climbs while
		// searches silently degrade to FTS-only.
		"hybrid_search":          h.store.HybridState(),
		"hybrid_fail_open_total": h.store.HybridFailOpenTotal(),
		// Exact-id guard routings (policy, not failure): literal-shaped
		// queries take the FTS-only path even with hybrid active.
		"hybrid_literal_skips_total": h.store.HybridLiteralSkipsTotal(),
	})
}

// readinessProbeTimeout caps the store round-trip so a hung database turns
// into a bounded 503, never a probe that outlives the kubelet's own timeout
// window by minutes.
const readinessProbeTimeout = 5 * time.Second

// Readiness answers the orchestrator's routing question honestly (MISSING
// #8): it probes the two things the service cannot function without — store
// reachability (one trivial round-trip on the READ pool; see Store.Ping
// for why the write handle is deliberately not probed) and pending-queue
// writability — and returns 503 with a per-check body when either fails.
// Capture-first holds: capture is fail-open at the queue writer, and
// shedding traffic from a broken pod protects capture.
//
// The endpoint is auth-exempt, so the body follows the D224 closed-
// vocabulary invariant: each check is "ok" or "failed", never an error
// string (a driver error can carry DSN hosts or paths). The real error goes
// to the server log.
func (h Handler) Readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessProbeTimeout)
	defer cancel()
	checks := map[string]string{"store": "ok", "queue": "ok"}
	ready := true
	if err := h.store.Ping(ctx); err != nil {
		checks["store"] = "failed"
		ready = false
		h.logger.Error("readiness: store probe failed", "error", err)
	}
	if err := probeQueueWritable(h.cfg.Queue.Pending); err != nil {
		checks["queue"] = "failed"
		ready = false
		h.logger.Error("readiness: queue probe failed", "error", err)
	}
	body := map[string]any{"status": "ready", "checks": checks}
	if !ready {
		body["status"] = "not_ready"
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// probeQueueWritable proves the pending queue directory can take a new file
// right now: create, write, remove. The probe name is dot-prefixed and ends
// in .tmp — the drainer only picks pending entries with a ".json" suffix and
// the queue writer's own temp files use the same dot/.tmp shape, so a probe
// (even one leaked by a crash mid-probe) can never be ingested or poison
// queue/failed.
func probeQueueWritable(pendingDir string) error {
	f, err := os.CreateTemp(pendingDir, ".readiness-probe-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	_, werr := f.WriteString("probe\n")
	cerr := f.Close()
	rerr := os.Remove(name)
	if werr != nil {
		return werr
	}
	if cerr != nil {
		return cerr
	}
	return rerr
}

func (h Handler) Stats(w http.ResponseWriter, r *http.Request) {
	st, err := h.store.Stats(r.Context(), h.cfg.DBPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h Handler) ListCredentials(w http.ResponseWriter, r *http.Request) {
	creds, err := h.store.ListCredentials(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": creds, "total": len(creds)})
}

func (h Handler) ListDerivationRuns(w http.ResponseWriter, r *http.Request) {
	filter := derivationRunFilterFromQuery(r, 50)
	runs, err := h.store.ListDerivationRuns(r.Context(), filter)
	if err != nil {
		h.writeReadError(w, r, err, 0, db.ObservationFilter{Project: filter.Project, Limit: filter.Limit})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"derivation_runs": runs, "total": len(runs), "next_cursor": nil})
}

func (h Handler) GetDerivationRun(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("derivation run id is required"))
		return
	}
	rec, err := h.store.GetDerivationRun(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"derivation_run": rec})
}

func (h Handler) RevokeCredential(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("credential id is required"))
		return
	}
	if auth, ok := authctx.From(r.Context()); ok && auth.CredentialID == id {
		writeError(w, http.StatusBadRequest, errors.New("refusing to revoke the credential used for this request"))
		return
	}
	if err := h.store.RevokeCredential(r.Context(), id); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, err)
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "revoked": true})
}

func (h Handler) CreateEvent(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	res, err := queuewriter.WriteBytes(r.Context(), h.cfg, "http", body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "queue_id": res.Name})
}

func (h Handler) CreateEventBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rejected := []map[string]any{}
	queued := []string{}
	for i, event := range body.Events {
		res, err := queuewriter.WriteBytes(r.Context(), h.cfg, "http-batch", event)
		if err != nil {
			rejected = append(rejected, map[string]any{"index": i, "reason": err.Error()})
			continue
		}
		queued = append(queued, res.Name)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": queued, "rejected": rejected})
}

// ListToolEventsInWindow serves GET /v1/events/tool-window: a window-scoped,
// READ-scope query over RAW tool events. The response carries raw events only
// (tool_name, tool_input, tool_response, cwd, occurred_at_epoch, agent_id) plus
// a best-effort paths_touched convenience. It NEVER emits a task-shape tuple, a
// verdict, or a score — Liaison does the artifact match and interpretation.
// session_id is required; start/end are inclusive epoch bounds. See
// DECISIONS D110-D116.
func (h Handler) ListToolEventsInWindow(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sessionID := strings.TrimSpace(q.Get("session_id"))
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, errors.New("session_id is required"))
		return
	}
	start, err := parseEpochParam(q.Get("start"))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid start: %w", err))
		return
	}
	end, err := parseEpochParam(q.Get("end"))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid end: %w", err))
		return
	}
	events, err := h.store.ListToolEventsInWindow(r.Context(), db.ToolEventFilter{
		SessionID:  sessionID,
		StartEpoch: start,
		EndEpoch:   end,
		AgentID:    q.Get("agent_id"),
		Project:    q.Get("project"),
		EventType:  q.Get("event_type"),
		Limit:      parseLimit(q.Get("limit"), 100),
	})
	if err != nil {
		h.writeReadError(w, r, err, 0, db.ObservationFilter{Project: q.Get("project"), Limit: parseLimit(q.Get("limit"), 100)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "total": len(events), "next_cursor": nil})
}

func (h Handler) GetEvent(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err := h.store.GetEvent(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": rec})
}

func (h Handler) GetObservation(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var filter db.ObservationFilter
	if r.URL.Query().Get("include_proposed") == "true" {
		filter.IncludeProposed = true
	}
	if r.URL.Query().Get("include_noise") == "true" {
		filter.IncludeNoise = true
	}
	// L0010 R1 = A: the default by-id read serves raw projections only. No
	// 400 here — this route takes no kind or exclude_kind, so there is
	// nothing to reject; the forced kind simply leaves a derived row
	// unfound, and it 404s until include_derived=true.
	// occurred_after does not apply: a by-id fetch is exact, not a window.
	if r.URL.Query().Get("include_derived") != "true" {
		filter.Kind = "raw"
	}
	rec, err := h.store.GetObservationWithFilter(r.Context(), id, filter)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// The single-observation route serves siblings next to the record, from
	// the same store method over a one-element slice (Fibery #335).
	probe := []db.SearchHit{{ID: rec.ID, DerivationRunID: rec.DerivationRunID}}
	if err := h.store.AttachRunSiblings(r.Context(), probe, filter, db.SiblingsPerHit); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	one := []db.ObservationRecord{rec}
	if err := h.store.AttachUsage(r.Context(), one); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	h.reads.records(readObservationGet, filter, one)
	resp := map[string]any{"observation": one[0]}
	if len(probe[0].Siblings) > 0 {
		resp["siblings"] = probe[0].Siblings
	}
	writeJSON(w, http.StatusOK, resp)
}

// ListObservationUsage serves the attribution lines of one observation
// (attribution at Stop): the verdict and reason each consuming turn gave the
// hit, newest first, with the aggregate the list routes attach. Read scope,
// like the observation itself; proposed and noise rows resolve, since the
// review UI is the consumer.
func (h Handler) ListObservationUsage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err := h.store.GetObservationWithFilter(r.Context(), id, db.ObservationFilter{IncludeProposed: true, IncludeNoise: true})
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	one := []db.ObservationRecord{rec}
	if err := h.store.AttachUsage(r.Context(), one); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	lines, err := h.store.ListObservationUsage(r.Context(), id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"observation_id": id, "usage": one[0].Usage, "lines": lines, "total": len(lines)})
}

func (h Handler) ListObservations(w http.ResponseWriter, r *http.Request) {
	filter, err := observationFilterFromQuery(r, 50)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := gateDefaultRawReads(r.URL.Query().Get("include_derived") == "true", &filter); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	recs, nextCursor, err := h.store.ListObservations(r.Context(), filter)
	if errors.Is(err, db.ErrBadCursor) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err != nil {
		h.writeReadError(w, r, err, 0, filter)
		return
	}
	if err := h.store.AttachUsage(r.Context(), recs); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	h.reads.records(readObservationList, filter, recs)
	var nextCursorJSON any
	if nextCursor != "" {
		nextCursorJSON = nextCursor
	}
	writeJSON(w, http.StatusOK, map[string]any{"observations": recs, "total": len(recs), "next_cursor": nextCursorJSON})
}

func (h Handler) GetSession(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("session id is required"))
		return
	}
	rec, err := h.store.GetSession(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	h.reads.request(readSessionGet, rawNA)
	writeJSON(w, http.StatusOK, map[string]any{"session": rec})
}

func (h Handler) DeleteEvent(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := h.store.DeleteEvent(r.Context(), id); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, err)
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (h Handler) SearchGET(w http.ResponseWriter, r *http.Request) {
	filter, err := observationFilterFromQuery(r, 20)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := gateDefaultRawReads(r.URL.Query().Get("include_derived") == "true", &filter); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	h.search(w, r, r.URL.Query().Get("q"), filter)
}

func (h Handler) SearchPOST(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Q                  string `json:"q"`
		Project            string `json:"project"`
		SessionID          string `json:"session_id"`
		Kind               string `json:"kind"`
		ExcludeKind        string `json:"exclude_kind"`
		ReviewState        string `json:"review_state"`
		DerivationRunID    string `json:"derivation_run_id"`
		Limit              int    `json:"limit"`
		IncludeNoise       bool   `json:"include_noise"`
		IncludeProposed    bool   `json:"include_proposed"`
		IncludeDerived     bool   `json:"include_derived"`
		CanInstruct        bool   `json:"can_instruct"`
		Hybrid             *bool  `json:"hybrid"`
		OccurredAfterEpoch int64  `json:"occurred_after"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.OccurredAfterEpoch < 0 {
		writeError(w, http.StatusBadRequest, errors.New("occurred_after must be a unix epoch seconds bound"))
		return
	}
	filter := db.ObservationFilter{
		Project:            body.Project,
		SessionID:          body.SessionID,
		Kind:               body.Kind,
		ExcludeKind:        body.ExcludeKind,
		ReviewState:        body.ReviewState,
		DerivationRunID:    body.DerivationRunID,
		Limit:              body.Limit,
		IncludeNoise:       body.IncludeNoise,
		IncludeProposed:    body.IncludeProposed,
		CanInstruct:        body.CanInstruct,
		Hybrid:             body.Hybrid,
		OccurredAfterEpoch: body.OccurredAfterEpoch,
	}
	if err := gateDefaultRawReads(body.IncludeDerived, &filter); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	h.search(w, r, body.Q, filter)
}

func (h Handler) Context(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Q                  string `json:"q"`
		Project            string `json:"project"`
		SessionID          string `json:"session_id"`
		Kind               string `json:"kind"`
		ExcludeKind        string `json:"exclude_kind"`
		ReviewState        string `json:"review_state"`
		DerivationRunID    string `json:"derivation_run_id"`
		Limit              int    `json:"limit"`
		IncludeNoise       bool   `json:"include_noise"`
		IncludeProposed    bool   `json:"include_proposed"`
		IncludeDerived     bool   `json:"include_derived"`
		CanInstruct        bool   `json:"can_instruct"`
		Hybrid             *bool  `json:"hybrid"`
		OccurredAfterEpoch int64  `json:"occurred_after"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// Also allow include_noise from query string (e.g. /v1/context?token=...&include_noise=true)
	if !body.IncludeNoise && r.URL.Query().Get("include_noise") == "true" {
		body.IncludeNoise = true
	}
	if body.OccurredAfterEpoch < 0 {
		writeError(w, http.StatusBadRequest, errors.New("occurred_after must be a unix epoch seconds bound"))
		return
	}
	filter := db.ObservationFilter{
		Project:            body.Project,
		SessionID:          body.SessionID,
		Kind:               body.Kind,
		ExcludeKind:        body.ExcludeKind,
		ReviewState:        body.ReviewState,
		DerivationRunID:    body.DerivationRunID,
		Limit:              body.Limit,
		IncludeNoise:       body.IncludeNoise,
		IncludeProposed:    body.IncludeProposed,
		CanInstruct:        body.CanInstruct,
		Hybrid:             body.Hybrid,
		OccurredAfterEpoch: body.OccurredAfterEpoch,
	}
	if err := gateDefaultRawReads(body.IncludeDerived, &filter); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	hits, err := h.searchStore(r.Context(), body.Q, filter)
	if err != nil {
		h.writeReadError(w, r, err, len(body.Q), filter)
		return
	}
	// One attach serves both the hits array and the prompt-ready string.
	if err := h.store.AttachRunSiblings(r.Context(), hits, filter, db.SiblingsPerHit); err != nil {
		h.writeReadError(w, r, err, len(body.Q), filter)
		return
	}
	var parts []string
	for _, hit := range hits {
		parts = append(parts, fmt.Sprintf("[%d] %s %s %s: %s", hit.ID, time.Unix(hit.OccurredAtEpoch, 0).UTC().Format("2006-01-02"), hit.Project, searchHitLabel(hit), hit.Snippet))
		if line := siblingContinuationLine(hit.Siblings); line != "" {
			parts = append(parts, line)
		}
	}
	h.reads.hits(readContext, filter, hits)
	resp := map[string]any{"hits": hits, "context": strings.Join(parts, "\n")}
	h.attachIdentityAlerts(r, body.Project, resp)
	writeJSON(w, http.StatusOK, resp)
}

func (h Handler) Timeline(w http.ResponseWriter, r *http.Request) {
	filter, err := observationFilterFromQuery(r, 50)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := gateDefaultRawReads(r.URL.Query().Get("include_derived") == "true", &filter); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	hits, err := h.store.TimelineWithFilter(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := h.store.AttachRunSiblings(r.Context(), hits, filter, db.SiblingsPerHit); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	h.reads.hits(readTimeline, filter, hits)
	resp := map[string]any{"hits": hits, "total": len(hits), "next_cursor": nil}
	h.attachIdentityAlerts(r, filter.Project, resp)
	writeJSON(w, http.StatusOK, resp)
}

// attachIdentityAlerts adds identity_alerts to a project-filtered read
// response (liaison:D239 item 4 surfacing: the injection header and MCP reads both
// ride timeline/search). Advisory: an alert-read failure never fails the
// read itself.
func (h Handler) attachIdentityAlerts(r *http.Request, project string, resp map[string]any) {
	if project == "" {
		return
	}
	alerts, err := h.store.ListProjectIdentityAlerts(r.Context(), project)
	if err != nil || len(alerts) == 0 {
		return
	}
	lines := make([]string, 0, len(alerts))
	for _, a := range alerts {
		lines = append(lines, a.Text())
	}
	resp["identity_alerts"] = lines
}

func (h Handler) ReviewObservation(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var body struct {
		ToState    string `json:"to_state"`
		ReasonText string `json:"reason_text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err := h.store.TransitionObservationReview(r.Context(), id, body.ToState, body.ReasonText)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"observation": rec})
}

// AcceptObservation moves a proposed observation into trusted memory
// (review_state=accepted). It is a thin, intent-revealing wrapper over
// TransitionObservationReview: the review queue's two human verdicts —
// accept and reject — get dedicated, self-documenting routes so a triaging
// human or agent never has to know the internal to_state vocabulary. The
// generic POST /v1/observations/{id}/review remains for the non-verdict
// transitions (superseded, soft_deleted). See DECISIONS D90.
func (h Handler) AcceptObservation(w http.ResponseWriter, r *http.Request) {
	h.verdict(w, r, "accepted")
}

// RejectObservation removes a proposed observation from the trusted set
// without losing the record: it is a review_state change to "rejected", not a
// hard delete, so the row and its review-log/audit trail are preserved.
func (h Handler) RejectObservation(w http.ResponseWriter, r *http.Request) {
	h.verdict(w, r, "rejected")
}

// ConfirmObservation is the deliberate accept-and-promote verdict (slice E,
// memoire:D146, R-5): accepted + provenance=user_confirmed in one gesture —
// what plain accept used to do implicitly. Plain accept now means "seen, not
// contested, usable as memory" and leaves provenance untouched.
func (h Handler) ConfirmObservation(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var body struct {
		ReasonText string          `json:"reason_text"`
		Expected   *reviewExpected `json:"expected"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	rec, err := h.store.ConfirmObservationPre(r.Context(), id, body.ReasonText, body.Expected.pre())
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var sce *db.StateConflictError
	if errors.As(err, &sce) {
		writeStateConflict(w, sce)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"observation": rec})
}

// LapseObservation parks a proposed observation as lapsed (slice E,
// memoire:D146, R-6): searchable, ~-marked at injection, never
// feed-eligible, out of the queue counters. Reversible on-encounter.
func (h Handler) LapseObservation(w http.ResponseWriter, r *http.Request) {
	h.verdict(w, r, "lapsed")
}

// RewriteObservation replaces a proposed/lapsed/disputed observation with an
// operator-authored one (slice E, memoire:D146, R-5): a NEW accepted,
// user_confirmed observation carrying the operator's text supersedes the
// original (filiation via supersedes_id; source-event links copied).
func (h Handler) RewriteObservation(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var body struct {
		Text       string `json:"text"`
		ReasonText string `json:"reason_text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err := h.store.RewriteObservation(r.Context(), id, body.Text, body.ReasonText)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var sce *db.StateConflictError
	if errors.As(err, &sce) {
		writeStateConflict(w, sce)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"observation": rec})
}

// ListSessions serves the session review surface (slice E, E3): sessions
// newest-first with their review workload, so catchup mode can order the
// deposit to skim. ?project= filters, ?proposed_only=true keeps sessions
// with outstanding work, ?limit= caps (default 50, max 200).
func (h Handler) ListSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("limit must be an integer"))
			return
		}
		limit = n
	}
	sessions, err := h.store.ListSessions(r.Context(), q.Get("project"), q.Get("proposed_only") == "true", limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if sessions == nil {
		sessions = []db.SessionSummary{}
	}
	h.reads.request(readSessionList, rawNA)
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// LapseSessionRemaining is the per-session bulk verdict (slice E, R-6):
// every still-proposed observation of the session lapses in one transaction.
func (h Handler) LapseSessionRemaining(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	var body struct {
		ReasonText string `json:"reason_text"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	n, err := h.store.LapseSessionRemaining(r.Context(), sessionID, body.ReasonText)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sessionID, "lapsed_count": n})
}

// Embeddings seam (D218): the embed worker is a dumb loop over these three
// endpoints — work list, batch upsert, freshness. All admin-scoped: a
// client write token must not be able to poison the vector lane. Every
// handler degrades to 409 when the store has no vector lane so a
// misconfigured worker fails loud, not silent.

// EmbeddingsMissing returns the oldest-first work list of derived
// observations lacking a CURRENT embedding for the given model.
func (h Handler) EmbeddingsMissing(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	if model == "" {
		model = db.DefaultEmbedModel
	}
	limit := parseLimit(r.URL.Query().Get("limit"), 1000)
	missing, err := h.store.ListObservationsMissingEmbedding(r.Context(), model, limit)
	if errors.Is(err, db.ErrNoVectorLane) {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": model, "missing": missing, "total": len(missing)})
}

// EmbeddingsUpsert writes a worker batch.
func (h Handler) EmbeddingsUpsert(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Embeddings []db.EmbeddingUpsert `json:"embeddings"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(body.Embeddings) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("embeddings is required"))
		return
	}
	now := time.Now().Unix()
	for i := range body.Embeddings {
		if body.Embeddings[i].ObservationID <= 0 || body.Embeddings[i].Model == "" || body.Embeddings[i].TextHash == "" {
			writeError(w, http.StatusBadRequest, errors.New("each embedding needs observation_id, model, vector, text_hash"))
			return
		}
		body.Embeddings[i].EpochNow = now
	}
	err := h.store.UpsertObservationEmbeddings(r.Context(), body.Embeddings)
	if errors.Is(err, db.ErrNoVectorLane) {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"written": len(body.Embeddings)})
}

// EmbeddingsFreshness serves the RUNBOOK 2h look-line: missing count and
// oldest-missing capture epoch for the model.
func (h Handler) EmbeddingsFreshness(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	if model == "" {
		model = db.DefaultEmbedModel
	}
	f, err := h.store.EmbeddingFreshnessStats(r.Context(), model)
	if errors.Is(err, db.ErrNoVectorLane) {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// LapseExpired runs the review-TTL sweep (slice E, R-6): proposed rows never
// opened within the TTL lapse under the attributable system actor. Invoked
// by the derive-sweep timer; ttl_days optional (default 21).
func (h Handler) LapseExpired(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TTLDays int `json:"ttl_days"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	n, err := h.store.LapseExpiredProposed(r.Context(), body.TTLDays)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	ttl := body.TTLDays
	if ttl <= 0 {
		ttl = db.DefaultLapseTTLDays
	}
	writeJSON(w, http.StatusOK, map[string]any{"ttl_days": ttl, "lapsed_count": n})
}

// ElevateObservation sets can_instruct=1 on an accepted observation — the
// rare, operator-pulled act that makes it ride the operating-notes block of
// every future session for its project. Like accept/reject it gets a
// dedicated intent-revealing route; unlike them it is never queued or badged
// (pull, not push — docs/ELEVATION-DESIGN.md). Not accepted yet → 409.
func (h Handler) ElevateObservation(w http.ResponseWriter, r *http.Request) {
	h.elevation(w, r, h.store.ElevateObservation)
}

// DemoteObservation is the inverse of ElevateObservation: can_instruct back
// to 0, provenance untouched. Elevation at operating-notes blast radius must
// be reversible. Not currently elevated → 409.
func (h Handler) DemoteObservation(w http.ResponseWriter, r *http.Request) {
	h.elevation(w, r, h.store.DemoteObservation)
}

func (h Handler) elevation(w http.ResponseWriter, r *http.Request, act func(context.Context, int64, string) (db.ObservationRecord, error)) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var body struct {
		ReasonText string `json:"reason_text"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	rec, err := act(r.Context(), id, body.ReasonText)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	// Elevation conflicts carry current-state metadata in the unified 409
	// shape (memoire:D207 residual 1); the bare ErrElevationConflict branch
	// stays as a fallback for any unwrapped path.
	var sce *db.StateConflictError
	if errors.As(err, &sce) {
		writeStateConflict(w, sce)
		return
	}
	if errors.Is(err, db.ErrElevationConflict) {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"observation": rec})
}

// reviewExpected is the optional client precondition on a verdict body
// (memoire:D207 residual 1): the caller's snapshot of the row, checked
// inside the mutation's transaction. Any non-nil field that mismatches the
// current row conflicts with HTTP 409 and writes nothing.
type reviewExpected struct {
	ReviewState *string `json:"review_state"`
	Provenance  *string `json:"provenance"`
	CanInstruct *int    `json:"can_instruct"`
}

func (e *reviewExpected) pre() *db.ReviewPrecondition {
	if e == nil {
		return nil
	}
	return &db.ReviewPrecondition{ReviewState: e.ReviewState, Provenance: e.Provenance, CanInstruct: e.CanInstruct}
}

// writeStateConflict is the ONE 409 shape for verdict/elevation conflicts:
// machine-readable code plus the row's current METADATA (never text or
// payload — safe for states the read surface hides). A client whose POST
// lost its response retries the same preconditioned call and reads the
// outcome here instead of a blind truth-read.
func writeStateConflict(w http.ResponseWriter, sce *db.StateConflictError) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": sce.Error(),
		"code":  "state_conflict",
		"current": map[string]any{
			"id":           sce.ID,
			"review_state": sce.ReviewState,
			"provenance":   sce.Provenance,
			"can_instruct": sce.CanInstruct,
		},
	})
}

func (h Handler) verdict(w http.ResponseWriter, r *http.Request, toState string) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var body struct {
		ReasonText string          `json:"reason_text"`
		Expected   *reviewExpected `json:"expected"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	rec, err := h.store.TransitionObservationReviewPre(r.Context(), id, toState, body.ReasonText, body.Expected.pre())
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var sce *db.StateConflictError
	if errors.As(err, &sce) {
		writeStateConflict(w, sce)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"observation": rec})
}

func (h Handler) ListObservationReviewLog(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	recs, err := h.store.ListObservationReviewLog(r.Context(), db.ObservationReviewLogFilter{
		ObservationID: id,
		Limit:         parseLimit(r.URL.Query().Get("limit"), 50),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"review_log": recs, "total": len(recs), "next_cursor": nil})
}

// DisputeObservation transitions an observation to "disputed" review state.
// It is a thin admin-only wrapper over TransitionObservationReview.
func (h Handler) DisputeObservation(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var body struct {
		ReasonText string `json:"reason_text"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	rec, err := h.store.TransitionObservationReview(r.Context(), id, "disputed", body.ReasonText)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"observation": rec})
}

func (h Handler) search(w http.ResponseWriter, r *http.Request, q string, filter db.ObservationFilter) {
	hits, err := h.searchStore(r.Context(), q, filter)
	if err != nil {
		h.writeReadError(w, r, err, len(q), filter)
		return
	}
	if err := h.store.AttachRunSiblings(r.Context(), hits, filter, db.SiblingsPerHit); err != nil {
		h.writeReadError(w, r, err, len(q), filter)
		return
	}
	h.reads.hits(readSearch, filter, hits)
	resp := map[string]any{"hits": hits, "total": len(hits), "next_cursor": nil}
	h.attachIdentityAlerts(r, filter.Project, resp)
	writeJSON(w, http.StatusOK, resp)
}

func boolPtr(b bool) *bool { return &b }

// L0010 R1 = A: Hindsight owns memory reads, so the five default read
// routes (search, timeline, context, observation list, observation by-id)
// serve raw capture projections only. Without include_derived the filter is
// forced to kind='raw' — the store's existing kind clause does the rest —
// and a derived-kind ask (a non-raw kind, or exclude_kind='raw') is a 400
// that names the opt-in, so a stale client learns why it gets nothing
// instead of reading an empty answer. The gate sits on the routes: store-
// internal callers (purge, redact-derived, triage, review routes) keep the
// full kind surface and never pass through it. HTTP clients that read
// derived rows on purpose (the review UI, the opt-in SessionStart
// injector) must send include_derived=true.
func gateDefaultRawReads(includeDerived bool, filter *db.ObservationFilter) error {
	if includeDerived {
		return nil
	}
	if (filter.Kind != "" && filter.Kind != "raw") || filter.ExcludeKind == "raw" {
		return errors.New("derived observations are not served by default; pass include_derived=true")
	}
	filter.Kind = "raw"
	return nil
}

func observationFilterFromQuery(r *http.Request, fallbackLimit int) (db.ObservationFilter, error) {
	filter := db.ObservationFilter{
		Project:         r.URL.Query().Get("project"),
		SessionID:       r.URL.Query().Get("session_id"),
		Kind:            r.URL.Query().Get("kind"),
		ExcludeKind:     r.URL.Query().Get("exclude_kind"),
		ReviewState:     r.URL.Query().Get("review_state"),
		DerivationRunID: r.URL.Query().Get("derivation_run_id"),
		Limit:           parseLimit(r.URL.Query().Get("limit"), fallbackLimit),
	}
	if v := r.URL.Query().Get("include_noise"); v == "true" {
		filter.IncludeNoise = true
	}
	if v := r.URL.Query().Get("include_proposed"); v == "true" {
		filter.IncludeProposed = true
	}
	if v := r.URL.Query().Get("can_instruct"); v == "true" {
		filter.CanInstruct = true
	}
	// The feed actor-gate (R-D) is fail-closed, so lifting it is an
	// explicit opt-in the operator surfaces send: the review and browse
	// screens must show every accepted row, including one a machine
	// verdict would have accepted, or the gate would hide its own subject.
	if v := r.URL.Query().Get("include_agent_accepted"); v == "true" {
		filter.IncludeAgentAccepted = true
	}
	// hybrid is Search-only (D218): explicit "1"/"true" or "0"/"false"
	// overrides the deployment default; anything else follows it.
	switch r.URL.Query().Get("hybrid") {
	case "1", "true":
		filter.Hybrid = boolPtr(true)
	case "0", "false":
		filter.Hybrid = boolPtr(false)
	}
	// Order and Cursor are only consumed by ListObservations (GET
	// /v1/observations); Search and Timeline ignore db.ObservationFilter's
	// Order/Cursor entirely, so populating them here has no effect on their
	// behavior.
	filter.Order = r.URL.Query().Get("order")
	filter.Cursor = r.URL.Query().Get("cursor")
	// L0010 dated reads: occurred_after bounds the read to a recent window
	// by occurred time. A malformed bound is a 400, not a silent
	// full-history read; a negative epoch is likewise refused (there is no
	// window before 1970, and 0 already means no bound).
	if v := r.URL.Query().Get("occurred_after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return filter, errors.New("occurred_after must be a unix epoch seconds bound")
		}
		filter.OccurredAfterEpoch = n
	}
	return filter, nil
}

func derivationRunFilterFromQuery(r *http.Request, fallbackLimit int) db.DerivationRunFilter {
	return db.DerivationRunFilter{
		Project:    r.URL.Query().Get("project"),
		SessionID:  r.URL.Query().Get("session_id"),
		Provider:   r.URL.Query().Get("provider"),
		Model:      r.URL.Query().Get("model"),
		EndpointID: r.URL.Query().Get("endpoint_id"),
		Status:     r.URL.Query().Get("status"),
		Limit:      parseLimit(r.URL.Query().Get("limit"), fallbackLimit),
	}
}

func searchHitLabel(hit db.SearchHit) string {
	// Unreviewed states must stay visible in the compact context string no
	// matter which label branch wins (memoire:D146 R-6: lapsed and proposed
	// are low-trust everywhere they surface).
	unreviewed := ""
	if hit.ReviewState == "proposed" || hit.ReviewState == "lapsed" {
		unreviewed = "~" + hit.ReviewState
	}
	if hit.SourceAdapter != "" || hit.EventType != "" {
		return strings.Trim(strings.Trim(hit.SourceAdapter+"/"+hit.EventType, "/"), " ") + unreviewed
	}
	if hit.Kind != "" {
		if hit.ReviewState != "" {
			return hit.Kind + "/" + hit.ReviewState
		}
		return hit.Kind
	}
	return "observation"
}

// siblingContinuationLine renders a hit's run-siblings as one indented
// continuation line under the hit, indented by two spaces:
//
//	↳ same run: [204165] task~, [204164] task~!
//
// `~` after the kind marks unreviewed (proposed or lapsed), `!` marks
// can_instruct=1 — the same two markers the compact line uses. No text
// travels, so the line is a pointer set a consumer can fetch by id.
// Returns "" when the hit has no siblings, so a hit without any renders
// exactly as before (Fibery #335).
//
// The MCP server renders the identical shape from the fields the API sent.
// The two live in separate Go modules, so the shape is written twice and
// docs/INJECTION-AND-SEARCH.md is its single source.
func siblingContinuationLine(siblings []db.ObservationSibling) string {
	if len(siblings) == 0 {
		return ""
	}
	parts := make([]string, 0, len(siblings))
	for _, sib := range siblings {
		marker := ""
		if sib.ReviewState == "proposed" || sib.ReviewState == "lapsed" {
			marker = "~"
		}
		if sib.CanInstruct == 1 {
			marker += "!"
		}
		parts = append(parts, fmt.Sprintf("[%d] %s%s", sib.ID, sib.Kind, marker))
	}
	return "  ↳ same run: " + strings.Join(parts, ", ")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// parseEpochParam parses an optional epoch-seconds query parameter. An empty
// value means "unbounded on this side of the window" and returns 0, nil. A
// non-numeric value is a 400-worthy error.
func parseEpochParam(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	return strconv.ParseInt(v, 10, 64)
}

func parseLimit(v string, fallback int) int {
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
