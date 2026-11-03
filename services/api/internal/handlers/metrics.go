// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package handlers

import (
	"context"
	"encoding/json"
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
	failures     deriveFailureHealth
	failuresOK   bool
	verdicts     []db.ReviewTransitionCount
	verdictsOK   bool

	computeFresh    func(context.Context) (db.EmbeddingFreshness, error)
	computeRuns     func(context.Context) ([]db.DeriveRunStatusCount, error)
	computeFailures func(context.Context) (deriveFailureHealth, error)
	computeVerdicts func(context.Context) ([]db.ReviewTransitionCount, error)
}

// metricsRecentRunWindow bounds the failure-genre read: the newest N runs of
// the ledger. A window, not a full-table scan — failed rows retain their raw
// provider response (memoire:D44), so an unbounded read is unbounded bytes.
const metricsRecentRunWindow = 200

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
		// The fold happens HERE, under the heavy-window owner budget (DEEP
		// review F1): only the bounded numeric summary is cached — the raw
		// outcomes (whose meta_json can retain full provider responses,
		// memoire:D44) live only for the duration of this refresh, and a
		// body-TTL re-render never re-parses them.
		computeFailures: func(ctx context.Context) (deriveFailureHealth, error) {
			o, err := store.RecentDeriveRunOutcomes(ctx, metricsRecentRunWindow)
			if err != nil {
				return deriveFailureHealth{}, err
			}
			return summarizeDeriveFailures(o), nil
		},
		// The R-D metrics actor split. It is a heavy family — a grouped
		// scan of the whole review log — so it rides the 5-minute window
		// beside the other aggregates rather than the per-scrape path.
		computeVerdicts: func(ctx context.Context) ([]db.ReviewTransitionCount, error) {
			return store.CountReviewTransitionsByActorClass(ctx)
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
func (c *metricsCache) snapshot() (db.EmbeddingFreshness, bool, []db.DeriveRunStatusCount, bool, deriveFailureHealth, bool, []db.ReviewTransitionCount, bool) {
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
		octx, ocancel := context.WithTimeout(context.Background(), c.computeTimeout)
		o, oerr := c.computeFailures(octx)
		ocancel()
		c.failures, c.failuresOK = o, oerr == nil
		vctx, vcancel := context.WithTimeout(context.Background(), c.computeTimeout)
		v, verr := c.computeVerdicts(vctx)
		vcancel()
		c.verdicts, c.verdictsOK = v, verr == nil
		c.heavyExpires = c.now().Add(metricsHeavyTTL)
	}
	return c.fresh, c.freshOK, c.runs, c.runsOK, c.failures, c.failuresOK, c.verdicts, c.verdictsOK
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

	h.reads.render(&b)

	fresh, freshOK, runs, runsOK, failures, failuresOK, verdicts, verdictsOK := h.metrics.snapshot()
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
	if failuresOK {
		// The memoire:D222 detection gap: a lane that fails the same way
		// every night. The genre histogram is the durable signal — it
		// survives a healthy lane's successes interleaving with a dead
		// lane's failures, which the streak below does not. Zeros here are
		// computed, not fabricated: the window was read. On a failed read
		// the whole family is absent, like the other cached ones. The fold
		// itself happened at heavy-refresh time (DEEP review F1) — this
		// render only prints cached numbers.
		fmt.Fprintf(&b, "# HELP memoire_derive_failed_runs_recent Failed derivation runs among the last %d ledger runs, by closed-vocabulary failure genre (TTL-cached, %ds).\n# TYPE memoire_derive_failed_runs_recent gauge\n",
			metricsRecentRunWindow, int(metricsHeavyTTL.Seconds()))
		for _, genre := range deriveFailureGenres {
			fmt.Fprintf(&b, "memoire_derive_failed_runs_recent{genre=\"%s\"} %d\n", genre, failures.ByGenre[genre])
		}
		gauge("memoire_derive_consecutive_failed_runs",
			fmt.Sprintf("Failed derivation runs in an unbroken newest-first streak, skipping non-terminal pending/running rows (a stale crashed run is skipped, not counted); saturates at the %d-run window.", metricsRecentRunWindow),
			failures.Consecutive)
	}
	if verdictsOK && len(verdicts) > 0 {
		// The R-D metrics actor split, on the surface where a review-log
		// number is read without a human in the loop. Both labels are
		// compiled-in closed vocabularies (the review states, and
		// db.ActorClass's three-value fold) — the actor STRING never
		// crosses this boundary, per the memoire:D224 invariant. A gauge,
		// not a counter: the log is append-only but this renders a
		// current-state census of it.
		fmt.Fprintf(&b, "# HELP memoire_review_transitions Review-log transitions by target state and actor class — human, system:*, or agent:* (TTL-cached, %ds).\n# TYPE memoire_review_transitions gauge\n", int(metricsHeavyTTL.Seconds()))
		for _, v := range verdicts {
			fmt.Fprintf(&b, "memoire_review_transitions{to_state=\"%s\",actor_class=\"%s\"} %d\n", v.ToState, v.ActorClass, v.Count)
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

// The CLOSED failure-genre vocabulary. Every value here is compiled in; no
// stored or provider-supplied string is ever emitted as a genre, so these
// are legal /metrics label values under the D224 invariant, exactly like
// deriveStatusClass above.
//
// The observable exists because of the memoire:D222 incident class: the
// nightly derive lane failed four consecutive nights on a CLIENT-SIDE
// registry-allowlist rejection ("model ... is not allowlisted for endpoint
// ..."), and all-fail=red marked each sweep failed without ever saying WHAT
// KIND of failure — so "same class, night after night" was invisible until a
// human read the logs. genreClientConfig is that incident's genre.
const (
	genreClientConfig      = "client_config"
	genreProviderHTTP      = "provider_http"
	genreProviderTransport = "provider_transport"
	genreProviderResponse  = "provider_response"
	genreParse             = "parse"
	genreRefusal           = "refusal"
	genreUnknown           = "unknown"
)

// deriveFailureGenres is the emission order for the gauge: a fixed series
// set, so an alert expression never has to cope with a genre appearing and
// disappearing between scrapes.
var deriveFailureGenres = []string{
	genreClientConfig,
	genreProviderHTTP,
	genreProviderTransport,
	genreProviderResponse,
	genreParse,
	genreRefusal,
	genreUnknown,
}

// providerErrorGenres maps compiled-in fragments of the orchestrator's own
// provider-call error text onto genres. FIRST MATCH WINS and the order is
// load-bearing: the HTTP shapes are checked first because those are exactly
// the error strings that interpolate a provider-supplied tail (the byok
// adapter appends the response's error type), so a provider echoing another
// genre's wording cannot re-tag its own HTTP failure. Nothing here is
// emitted — only the genre constant is.
var providerErrorGenres = []struct {
	fragment string
	genre    string
}{
	{"availability probe returned HTTP", genreProviderHTTP},
	{"answered a redirect (HTTP", genreProviderHTTP},
	{"rate/availability refusal: HTTP", genreProviderHTTP},
	{"returned HTTP ", genreProviderHTTP},

	{"is not an OpenAI-shaped model list", genreProviderResponse},
	{"response envelope not JSON", genreProviderResponse},
	{"had no choices", genreProviderResponse},

	{"is not allowlisted", genreClientConfig},
	{"is not in the local-qwen allowlist", genreClientConfig},
	{"not served by endpoint", genreClientConfig},
	{"requires an explicit", genreClientConfig},
	{"but it is unset", genreClientConfig},
	{"key file", genreClientConfig},
	{"does not match the configured", genreClientConfig},
	{"does not match the byok endpoint label", genreClientConfig},
	{"is not in the closed dialect set", genreClientConfig},
	{"unsupported output schema", genreClientConfig},
	{"adapter received request for provider", genreClientConfig},
	{"exceeds adapter max_prompt_bytes", genreClientConfig},

	{"generate call failed", genreProviderTransport},
	{"availability probe body read", genreProviderTransport},
	{"unavailable:", genreProviderTransport},
}

// deriveFailureGenre classifies ONE failed run's stored metadata at read
// time. The orchestrator records `failure.stage` (a compiled-in two-value
// vocabulary) and `failure.error` (its own error text, sometimes wrapping a
// provider tail); a refusal with nothing salvaged records `refusal` and no
// failure block. Anything else — a run row from the rules path, which
// records no failure detail at all, malformed metadata, an unrecognized
// error shape — is genreUnknown. Never guessed: unknown is the honest cell.
func deriveFailureGenre(meta []byte) string {
	if len(meta) == 0 {
		return genreUnknown
	}
	var m struct {
		Failure *struct {
			Stage string `json:"stage"`
			Error string `json:"error"`
		} `json:"failure"`
		Refusal string `json:"refusal"`
	}
	if err := json.Unmarshal(meta, &m); err != nil {
		return genreUnknown
	}
	if m.Failure == nil {
		if m.Refusal != "" {
			return genreRefusal
		}
		return genreUnknown
	}
	switch m.Failure.Stage {
	case "parse":
		return genreParse
	case "provider_call":
		// Boundary-aware scan (DEEP review F2 + its round-2 sibling): owned
		// error shapes interpolate text we do not control — provider model
		// IDs after "(serving: ", and whole net/http errors after the
		// %w-wrapping markers below (a hostile redirect Location header is
		// interpolated verbatim by net/http, so "is not allowlisted" inside
		// such a tail would re-tag a transport failure as client_config).
		// Everything after a boundary is dropped BEFORE fragment matching;
		// the wrapper markers themselves are kept so their own transport
		// fragments still match. The one conditional boundary is
		// "unavailable: %w": its inner error is often one of OUR shapes
		// (probe/model-presence) that deserves its specific genre — an
		// owned inner is recognizable by its compiled-in PREFIX, which a
		// provider cannot control, and its own tails are truncated by the
		// earlier boundaries. The byok errType tail after "HTTP %d (" needs
		// no boundary: the HTTP fragments sit first in the table and match
		// on the owned prefix before any tail is reached.
		scan := m.Failure.Error
		if i := strings.Index(scan, "(serving: "); i >= 0 {
			scan = scan[:i]
		}
		for _, b := range []string{"generate call failed", "availability probe body read", "unavailable: "} {
			i := strings.Index(scan, b)
			if i < 0 {
				continue
			}
			tail := scan[i+len(b):]
			if b == "unavailable: " &&
				(strings.HasPrefix(tail, "availability probe") || strings.HasPrefix(tail, `model "`)) {
				continue
			}
			scan = scan[:i+len(b)]
		}
		for _, c := range providerErrorGenres {
			if strings.Contains(scan, c.fragment) {
				return c.genre
			}
		}
	}
	return genreUnknown
}

// deriveFailureHealth is the read-time summary of the recent-run window.
type deriveFailureHealth struct {
	// ByGenre counts the FAILED runs in the window per genre.
	ByGenre map[string]int64
	// Consecutive is the streak of failed runs at the head of the ledger,
	// saturating at the window size.
	Consecutive int64
}

// summarizeDeriveFailures folds the recent-run window into the two gauges.
// The streak walks newest-first and SKIPS non-terminal rows (a `running`
// sweep in flight has no verdict yet and must not read as an all-clear),
// stopping at the first other terminal class. Statuses go through the same
// deriveStatusClass fold as the run-state gauge — one status vocabulary.
func summarizeDeriveFailures(runs []db.DeriveRunOutcome) deriveFailureHealth {
	h := deriveFailureHealth{ByGenre: map[string]int64{}}
	for _, g := range deriveFailureGenres {
		h.ByGenre[g] = 0
	}
	streakOpen := true
	for _, run := range runs {
		cls := deriveStatusClass(run.Status)
		if cls == "failed" {
			h.ByGenre[deriveFailureGenre(run.Metadata)]++
			if streakOpen {
				h.Consecutive++
			}
			continue
		}
		if cls == "running" || cls == "pending" {
			continue
		}
		streakOpen = false
	}
	return h
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
