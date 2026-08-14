// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package handlers

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"memoire/services/api/internal/db"
	"memoire/services/api/internal/queuefile"
)

// The /metrics surface is unauthenticated, so EVERYTHING it costs is
// attacker-purchasable on the LAN-published port. Two cache layers bound it
// (review 2026-08-03 F3/F4):
//
//   - metricsBodyTTL: the whole rendered exposition is cached; a scrape
//     flood buys at most one render per window. Alert lines are measured in
//     minutes-to-hours, so 15s staleness is invisible.
//   - metricsHeavyTTL: the embeddings-freshness anti-join (hashes every
//     derived text) and the derive-run ledger aggregate recompute at most
//     once per window. Failures are cached for the same window — a broken
//     or timing-out query must not be retried per render.
//
// Recomputes run under an OWNER-bounded context, never the requesting
// client's: a canceled unauthenticated scrape must not be able to poison
// the shared cache or blind later scrapes (F3).
const (
	metricsBodyTTL        = 15 * time.Second
	metricsHeavyTTL       = 5 * time.Minute
	metricsComputeTimeout = 20 * time.Second
)

type metricsCache struct {
	now func() time.Time
	// computeTimeout is metricsComputeTimeout in production; injectable so
	// the budget-isolation test does not wait out the real deadline.
	computeTimeout time.Duration

	bodyMu      sync.Mutex
	bodyExpires time.Time
	body        string

	heavyMu      sync.Mutex
	heavyExpires time.Time
	fresh        db.EmbeddingFreshness
	freshOK      bool
	runs         []db.DeriveRunStatusCount
	runsOK       bool

	computeFresh func(context.Context) (db.EmbeddingFreshness, error)
	computeRuns  func(context.Context) ([]db.DeriveRunStatusCount, error)
}

func newMetricsCache(store *db.Store) *metricsCache {
	return &metricsCache{
		now:            time.Now,
		computeTimeout: metricsComputeTimeout,
		computeFresh: func(ctx context.Context) (db.EmbeddingFreshness, error) {
			return store.EmbeddingFreshnessStats(ctx, db.DefaultEmbedModel)
		},
		computeRuns: func(ctx context.Context) ([]db.DeriveRunStatusCount, error) {
			return store.CountDerivationRunsByStatus(ctx)
		},
	}
}

// renderedBody serves the cached exposition, re-rendering when the body TTL
// has passed. The mutex is held across the render on purpose: concurrent
// scrapes serialize instead of stampeding the store.
func (c *metricsCache) renderedBody(render func() string) string {
	c.bodyMu.Lock()
	defer c.bodyMu.Unlock()
	if !c.now().Before(c.bodyExpires) {
		c.body = render()
		c.bodyExpires = c.now().Add(metricsBodyTTL)
	}
	return c.body
}

// snapshot serves the cached heavy values, recomputing both when the heavy
// TTL has passed — under its OWN bounded context, never a caller's. A
// failed compute (including ErrNoVectorLane on a store without the lane)
// yields ok=false and the corresponding series are simply absent from the
// exposition — never a stale-forever value or a fabricated zero.
func (c *metricsCache) snapshot() (db.EmbeddingFreshness, bool, []db.DeriveRunStatusCount, bool) {
	c.heavyMu.Lock()
	defer c.heavyMu.Unlock()
	if !c.now().Before(c.heavyExpires) {
		// Each family gets its OWN budget (round-2 N1): the freshness
		// anti-join exhausting its deadline must not hand the run query a
		// dead context and blank the second family for the whole window.
		fctx, fcancel := context.WithTimeout(context.Background(), c.computeTimeout)
		f, err := c.computeFresh(fctx)
		fcancel()
		c.fresh, c.freshOK = f, err == nil
		rctx, rcancel := context.WithTimeout(context.Background(), c.computeTimeout)
		l, lerr := c.computeRuns(rctx)
		rcancel()
		c.runs, c.runsOK = l, lerr == nil
		c.heavyExpires = c.now().Add(metricsHeavyTTL)
	}
	return c.fresh, c.freshOK, c.runs, c.runsOK
}

// Metrics serves the unauthenticated Prometheus scrape surface (infra seam
// ruling (b), inbox 2026-08-03): counters and gauges only. No stored string
// ever crosses this boundary — label values are compiled-in enums, the
// embed-model constant, or ledger statuses normalized through the closed
// deriveStatusClass vocabulary (review 2026-08-03 F1). Everything
// content-bearing stays behind bearer auth.
func (h Handler) Metrics(w http.ResponseWriter, r *http.Request) {
	body := h.metrics.renderedBody(h.renderMetrics)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

func (h Handler) renderMetrics() string {
	// Owner-bounded context for the live-path store counts: the render must
	// never inherit a scrape's cancellation (F3) and must not hang past the
	// compute budget.
	ctx, cancel := context.WithTimeout(context.Background(), metricsComputeTimeout)
	defer cancel()

	var b strings.Builder
	gauge := func(name, help string, v int64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, v)
	}
	counter := func(name, help string, v int64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	boolGauge := func(name, help string, on bool) {
		v := int64(0)
		if on {
			v = 1
		}
		gauge(name, help, v)
	}

	depth, oldestAge := pendingQueueStats(h.cfg.Queue.Pending)
	gauge("memoire_queue_depth", "Capture-queue items pending drain.", depth)
	gauge("memoire_queue_failed_depth", "Poison items in the failed queue (memoire:D136 R-3).", int64(failedQueueDepth(h.cfg.Queue.Failed)))
	gauge("memoire_queue_oldest_age_seconds", "Age of the oldest pending queue item; 0 when empty.", oldestAge)

	if n, err := h.store.CountRedactionContractViolations(ctx); err == nil {
		counter("memoire_redaction_contract_violations_total", "Events stamped by the liaison-evidence redaction backstop (memoire:D136 R-2).", n)
	}
	if h.cfg.SkewAlertSeconds > 0 {
		if n, err := h.store.CountFutureSkewEvents(ctx, h.cfg.SkewAlertSeconds); err == nil {
			gauge("memoire_clock_skew_future_events", "Events whose occurred_at leads received_at beyond the alert threshold (memoire:D152).", n)
		}
	}

	fmt.Fprintf(&b, "# HELP memoire_search_engine_info Active lexical search engine as a one-hot label (fts5 | bm25 | tsvector).\n# TYPE memoire_search_engine_info gauge\nmemoire_search_engine_info{engine=\"%s\"} 1\n",
		escapeLabel(h.store.SearchEngine()))
	boolGauge("memoire_vector_lane_on", "1 when the pgvector lane is available (D218).", h.store.VectorLane() == "pgvector")
	boolGauge("memoire_hybrid_search_on", "1 when hybrid search is the deployment default (D218 session B).", h.store.HybridState() == "on")
	counter("memoire_hybrid_fail_open_total", "Hybrid searches that fell open to FTS-only because the embed lane failed (in-process; resets on restart).", h.store.HybridFailOpenTotal())
	counter("memoire_hybrid_literal_skips_total", "Hybrid searches routed FTS-only by the exact-id guard (policy, not failure; in-process; resets on restart).", h.store.HybridLiteralSkipsTotal())

	fresh, freshOK, runs, runsOK := h.metrics.snapshot()
	if freshOK {
		model := escapeLabel(fresh.Model)
		fmt.Fprintf(&b, "# HELP memoire_embeddings_missing Derived observations lacking a current embedding (TTL-cached, %ds).\n# TYPE memoire_embeddings_missing gauge\nmemoire_embeddings_missing{model=\"%s\"} %d\n",
			int(metricsHeavyTTL.Seconds()), model, fresh.Missing)
		fmt.Fprintf(&b, "# HELP memoire_embeddings_oldest_missing_epoch Creation epoch of the oldest observation lacking a current embedding; 0 when none missing (TTL-cached, %ds).\n# TYPE memoire_embeddings_oldest_missing_epoch gauge\nmemoire_embeddings_oldest_missing_epoch{model=\"%s\"} %d\n",
			int(metricsHeavyTTL.Seconds()), model, fresh.OldestMissingEpoch)
	}
	if runsOK && len(runs) > 0 {
		// Current-state cells of a mutable ledger: a gauge, never a counter
		// — rows move between statuses within one process lifetime (F2).
		classes := map[string]int64{}
		order := []string{}
		for _, c := range runs {
			cls := deriveStatusClass(c.Status)
			if _, seen := classes[cls]; !seen {
				order = append(order, cls)
			}
			classes[cls] += c.Count
		}
		fmt.Fprintf(&b, "# HELP memoire_derive_runs Derivation runs currently in each status class (TTL-cached, %ds).\n# TYPE memoire_derive_runs gauge\n", int(metricsHeavyTTL.Seconds()))
		for _, cls := range order {
			fmt.Fprintf(&b, "memoire_derive_runs{status=\"%s\"} %d\n", cls, classes[cls])
		}
	}
	return b.String()
}

// deriveStatusClass folds a stored derivation-run status into the CLOSED
// public vocabulary the unauthenticated surface may emit. The stored value
// is unconstrained ledger data (review 2026-08-03 F1): only the class
// prefix is inspected and only known classes pass; anything else — however
// it got there — becomes "other" so no stored string ever reaches a label.
func deriveStatusClass(s string) string {
	cls, _, _ := strings.Cut(s, ":")
	switch cls {
	case "pending", "running", "completed", "partial", "failed", "skipped", "expired":
		return cls
	}
	return "other"
}

// pendingQueueStats walks the pending queue ONCE for both depth and
// oldest-item age (review 2026-08-03 F4 — the previous shape scanned the
// directory twice per probe).
func pendingQueueStats(dir string) (depth, oldestAgeSeconds int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	var oldest time.Time
	for _, entry := range entries {
		// Same predicate as the consumers: only what the drainer would
		// pick is backlog (review 2026-08-03 F3 + round-2 N2).
		if entry.IsDir() || !queuefile.IsItem(entry.Name()) {
			continue
		}
		depth++
		info, err := os.Stat(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		if oldest.IsZero() || info.ModTime().Before(oldest) {
			oldest = info.ModTime()
		}
	}
	if oldest.IsZero() {
		return depth, 0
	}
	return depth, int64(time.Since(oldest).Seconds())
}

// escapeLabel escapes a Prometheus label value per the text exposition
// format: backslash, double-quote, and newline.
func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(v)
}
