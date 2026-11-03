// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"memoire/services/api/internal/db"
)

// The heavy cache exists because the freshness anti-join and the run-ledger
// aggregate are too expensive for scrape frequency: inside the TTL window a
// scrape must serve the cached values without touching the store, a failed
// compute must be cached (absent series) for the same window — never retried
// per scrape — and the compute must run under an OWNER-bounded context (a
// deadline of our choosing), never a caller's cancelable one (review
// 2026-08-03 F3).
func TestMetricsCacheHeavyTTL(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	freshCalls, runCalls := 0, 0
	freshErr := error(nil)
	c := &metricsCache{
		now:            func() time.Time { return now },
		computeTimeout: metricsComputeTimeout,
		computeFresh: func(ctx context.Context) (db.EmbeddingFreshness, error) {
			freshCalls++
			if err := ctx.Err(); err != nil {
				t.Fatalf("compute context already dead: %v", err)
			}
			if _, hasDeadline := ctx.Deadline(); !hasDeadline {
				t.Fatal("compute context carries no owner deadline")
			}
			return db.EmbeddingFreshness{Model: "m", Missing: 7, OldestMissingEpoch: 42}, freshErr
		},
		computeRuns: func(context.Context) ([]db.DeriveRunStatusCount, error) {
			runCalls++
			return []db.DeriveRunStatusCount{{Status: "completed", Count: 3}}, nil
		},
		computeFailures: func(context.Context) (deriveFailureHealth, error) {
			return summarizeDeriveFailures([]db.DeriveRunOutcome{{Status: "completed"}}), nil
		},
		computeVerdicts: func(context.Context) ([]db.ReviewTransitionCount, error) {
			return []db.ReviewTransitionCount{{ToState: "accepted", ActorClass: "human", Count: 1}}, nil
		},
	}

	fresh, freshOK, runs, runsOK, _, _, _, _ := c.snapshot()
	if !freshOK || !runsOK || fresh.Missing != 7 || len(runs) != 1 {
		t.Fatalf("first snapshot = %+v %v %+v %v", fresh, freshOK, runs, runsOK)
	}
	if freshCalls != 1 || runCalls != 1 {
		t.Fatalf("first snapshot computed %d/%d times, want 1/1", freshCalls, runCalls)
	}

	// Inside the TTL: served from cache, zero store work.
	now = now.Add(metricsHeavyTTL - time.Second)
	if _, _, _, _, _, _, _, _ = c.snapshot(); freshCalls != 1 || runCalls != 1 {
		t.Fatalf("in-window snapshot recomputed (%d/%d)", freshCalls, runCalls)
	}

	// Past the TTL: recomputes; a compute failure is served as ok=false...
	freshErr = errors.New("boom")
	now = now.Add(2 * time.Second)
	if _, freshOK, _, _, _, _, _, _ = c.snapshot(); freshOK || freshCalls != 2 {
		t.Fatalf("post-TTL snapshot freshOK=%v calls=%d, want false/2", freshOK, freshCalls)
	}
	// ...and the failure itself is cached for the window — no per-scrape retry.
	now = now.Add(time.Second)
	if _, _, _, _, _, _, _, _ = c.snapshot(); freshCalls != 2 {
		t.Fatalf("failure was retried inside the TTL window (calls=%d)", freshCalls)
	}
}

// Each heavy family owns an independent compute budget (round-2 N1): a
// freshness query that eats its whole deadline must not hand the run query
// a dead context — the second family still computes and is served.
func TestMetricsCacheBudgetIsolation(t *testing.T) {
	c := &metricsCache{
		now:            time.Now,
		computeTimeout: 50 * time.Millisecond,
		computeFresh: func(ctx context.Context) (db.EmbeddingFreshness, error) {
			<-ctx.Done() // exhaust the entire freshness budget
			return db.EmbeddingFreshness{}, ctx.Err()
		},
		computeRuns: func(ctx context.Context) ([]db.DeriveRunStatusCount, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return []db.DeriveRunStatusCount{{Status: "failed", Count: 1}}, nil
		},
		computeFailures: func(ctx context.Context) (deriveFailureHealth, error) {
			if err := ctx.Err(); err != nil {
				return deriveFailureHealth{}, err
			}
			return summarizeDeriveFailures([]db.DeriveRunOutcome{{Status: "failed"}}), nil
		},
		computeVerdicts: func(ctx context.Context) ([]db.ReviewTransitionCount, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return []db.ReviewTransitionCount{{ToState: "lapsed", ActorClass: "system", Count: 2}}, nil
		},
	}
	_, freshOK, runs, runsOK, failures, failuresOK, verdicts, verdictsOK := c.snapshot()
	if freshOK {
		t.Fatal("exhausted freshness compute reported ok")
	}
	if !runsOK || len(runs) != 1 {
		t.Fatalf("run family starved by the freshness budget: runsOK=%v runs=%+v", runsOK, runs)
	}
	if !failuresOK || failures.Consecutive != 1 {
		t.Fatalf("failure-genre family starved by the freshness budget: ok=%v failures=%+v", failuresOK, failures)
	}
	if !verdictsOK || len(verdicts) != 1 {
		t.Fatalf("actor-split family starved by the freshness budget: ok=%v verdicts=%+v", verdictsOK, verdicts)
	}
}

// The body cache is the unauthenticated surface's cost bound: a scrape
// flood inside the window buys exactly one render.
func TestMetricsCacheBodyTTL(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := &metricsCache{now: func() time.Time { return now }}
	renders := 0
	render := func() string { renders++; return "body" }

	if got := c.renderedBody(render); got != "body" || renders != 1 {
		t.Fatalf("first body = %q renders=%d", got, renders)
	}
	now = now.Add(metricsBodyTTL - time.Second)
	if got := c.renderedBody(render); got != "body" || renders != 1 {
		t.Fatalf("in-window body re-rendered (renders=%d, got=%q)", renders, got)
	}
	now = now.Add(2 * time.Second)
	if _ = c.renderedBody(render); renders != 2 {
		t.Fatalf("post-TTL body served stale (renders=%d)", renders)
	}
}

// deriveStatusClass is the F1 boundary: the stored status is unconstrained
// ledger data, and ONLY the closed class vocabulary may reach a label on
// the unauthenticated surface.
func TestDeriveStatusClass(t *testing.T) {
	for in, want := range map[string]string{
		"pending":                 "pending",
		"running":                 "running",
		"completed":               "completed",
		"partial":                 "partial",
		"failed":                  "failed",
		"skipped:project_denied":  "skipped",
		"expired:no_events":       "expired",
		"operator-private-status": "other",
		"rack-private-codename":   "other",
		"":                        "other",
		"completedX":              "other",
	} {
		if got := deriveStatusClass(in); got != want {
			t.Errorf("deriveStatusClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// deriveFailureGenre is the memoire:D222 observable's whole point: the run
// metadata is stored ledger data (our own error text, sometimes wrapping a
// provider tail) and ONLY the compiled-in genre vocabulary may reach a
// label. The anchor case is D222's verbatim incident string.
func TestDeriveFailureGenre(t *testing.T) {
	fail := func(stage, msg string) []byte {
		return []byte(`{"status":"failed","failure":{"stage":"` + stage + `","error":"` + msg + `"}}`)
	}
	cases := []struct {
		name string
		meta []byte
		want string
	}{
		{
			// The D222 incident, verbatim from the nightly lane's run rows.
			name: "d222_registry_allowlist",
			meta: fail("provider_call", `modelderive: model \"qwen3.6-35b-a3b-ud-q6_k_xl-duo\" is not allowlisted for endpoint \"hl5-conductor\"`),
			want: genreClientConfig,
		},
		{name: "endpoint_not_allowlisted", meta: fail("provider_call", `modelderive: endpoint_id \"x\" is not in the local-qwen allowlist`), want: genreClientConfig},
		{name: "model_not_served", meta: fail("provider_call", `modelderive: local-qwen endpoint \"hl5\" unavailable: model \"m\" not served by endpoint (serving: a, b)`), want: genreClientConfig},
		{name: "missing_key_env", meta: fail("provider_call", "modelderive: byok requires env MEMOIRE_BYOK_API_KEY but it is unset"), want: genreClientConfig},
		{name: "model_mismatch", meta: fail("provider_call", `modelderive: model \"a\" does not match the configured byok model \"b\"`), want: genreClientConfig},
		{name: "prompt_over_adapter_cap", meta: fail("provider_call", "modelderive: prompt 9 bytes exceeds adapter max_prompt_bytes 4"), want: genreClientConfig},

		{name: "http_status", meta: fail("provider_call", "modelderive: byok generate returned HTTP 500"), want: genreProviderHTTP},
		{name: "http_rate_refusal", meta: fail("provider_call", "modelderive: byok endpoint rate/availability refusal: HTTP 429"), want: genreProviderHTTP},
		{name: "http_redirect", meta: fail("provider_call", "modelderive: byok endpoint answered a redirect (HTTP 302) — refusing to follow; configure the final endpoint URL"), want: genreProviderHTTP},
		{name: "probe_http", meta: fail("provider_call", `modelderive: local-qwen endpoint \"hl5\" unavailable: availability probe returned HTTP 404`), want: genreProviderHTTP},
		{
			// The ordering is deliberate: the HTTP shapes interpolate a
			// PROVIDER-SUPPLIED error type, so a provider echoing another
			// genre's wording must not re-tag its own HTTP failure.
			name: "hostile_provider_tail_stays_http",
			meta: fail("provider_call", "modelderive: byok generate returned HTTP 400 (is not allowlisted)"),
			want: genreProviderHTTP,
		},
		{
			// DEEP review F2's demonstrated spoof: a hostile /v1/models
			// entry named "hostile returned HTTP 503" rides the
			// "(serving: ...)" tail of a client-config failure; the
			// boundary truncation must keep the owned classification.
			name: "hostile_serving_tail_stays_client_config",
			meta: fail("provider_call", `modelderive: local-qwen endpoint \"hl5\" unavailable: model \"wanted\" not served by endpoint (serving: hostile returned HTTP 503)`),
			want: genreClientConfig,
		},
		{
			// Round-2 F2 sibling: net/http interpolates a hostile redirect
			// Location header verbatim into the %w-wrapped Do error; the
			// wrapper boundary must keep this a transport failure.
			name: "hostile_location_tail_stays_transport",
			meta: fail("provider_call", `modelderive: local-qwen endpoint \"hl5\" unavailable: Get \"http://h/v1/models\": failed to parse Location header \"is not allowlisted\": parse error`),
			want: genreProviderTransport,
		},
		{
			name: "hostile_location_in_generate_call_stays_transport",
			meta: fail("provider_call", `modelderive: byok generate call failed: Get \"http://h\": failed to parse Location header \"model x is not allowlisted for endpoint y\"`),
			want: genreProviderTransport,
		},

		{name: "transport", meta: fail("provider_call", "modelderive: byok generate call failed: dial tcp 192.0.2.1:3000: connect: connection refused"), want: genreProviderTransport},
		{name: "probe_body_read", meta: fail("provider_call", `modelderive: local-qwen endpoint \"hl5\" unavailable: availability probe body read: unexpected EOF`), want: genreProviderTransport},

		{name: "envelope_not_json", meta: fail("provider_call", "modelderive: byok response envelope not JSON: invalid character '<'"), want: genreProviderResponse},
		{name: "no_choices", meta: fail("provider_call", "modelderive: byok response had no choices"), want: genreProviderResponse},
		{name: "probe_body_shape", meta: fail("provider_call", `modelderive: local-qwen endpoint \"hl5\" unavailable: availability probe body is not an OpenAI-shaped model list: x`), want: genreProviderResponse},

		{name: "parse_stage", meta: fail("parse", "modelderive: malformed response JSON: unexpected end of JSON input"), want: genreParse},

		{name: "refusal_without_failure_block", meta: []byte(`{"refusal":"I cannot help with that"}`), want: genreRefusal},

		// The honest fallback: never guessed.
		{name: "no_metadata", meta: nil, want: genreUnknown},
		{name: "malformed_metadata", meta: []byte(`{`), want: genreUnknown},
		{name: "rules_path_no_failure_detail", meta: []byte(`{"source_window":{"project":"p"}}`), want: genreUnknown},
		{name: "unrecognized_stage", meta: fail("moon_phase", "something new"), want: genreUnknown},
		{name: "unrecognized_provider_error", meta: fail("provider_call", "modelderive: a shape this classifier has never seen"), want: genreUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveFailureGenre(tc.meta); got != tc.want {
				t.Errorf("deriveFailureGenre(%s) = %q, want %q", tc.meta, got, tc.want)
			}
		})
	}
}

// Every genre the classifier can return must be in the emission list, or a
// gauge cell would silently vanish.
func TestDeriveFailureGenreVocabularyIsClosed(t *testing.T) {
	known := map[string]bool{}
	for _, g := range deriveFailureGenres {
		known[g] = true
	}
	for _, c := range providerErrorGenres {
		if !known[c.genre] {
			t.Errorf("providerErrorGenres maps %q to genre %q, absent from deriveFailureGenres", c.fragment, c.genre)
		}
	}
	for _, g := range []string{genreClientConfig, genreProviderHTTP, genreProviderTransport, genreProviderResponse, genreParse, genreRefusal, genreUnknown} {
		if !known[g] {
			t.Errorf("genre %q is reachable but not emitted", g)
		}
	}
}

// The streak is the "lane is dead right now" half; the genre histogram is
// the "same class, night after night" half. A run still in flight carries no
// verdict and must not read as an all-clear.
func TestSummarizeDeriveFailures(t *testing.T) {
	allowlist := []byte(`{"failure":{"stage":"provider_call","error":"modelderive: model \"m\" is not allowlisted for endpoint \"hl5-conductor\""}}`)
	http500 := []byte(`{"failure":{"stage":"provider_call","error":"modelderive: byok generate returned HTTP 500"}}`)

	t.Run("streak_skips_in_flight_and_breaks_on_success", func(t *testing.T) {
		sum := summarizeDeriveFailures([]db.DeriveRunOutcome{
			{Status: "running"},
			{Status: "failed", Metadata: allowlist},
			{Status: "failed", Metadata: allowlist},
			{Status: "completed"},
			{Status: "failed", Metadata: http500},
		})
		if sum.Consecutive != 2 {
			t.Errorf("Consecutive = %d, want 2", sum.Consecutive)
		}
		// Counting spans the whole window even past the streak break.
		if sum.ByGenre[genreClientConfig] != 2 || sum.ByGenre[genreProviderHTTP] != 1 {
			t.Errorf("ByGenre = %+v", sum.ByGenre)
		}
	})

	t.Run("every_genre_cell_present_after_a_successful_read", func(t *testing.T) {
		sum := summarizeDeriveFailures(nil)
		if len(sum.ByGenre) != len(deriveFailureGenres) {
			t.Fatalf("ByGenre = %+v, want one cell per genre", sum.ByGenre)
		}
		for _, g := range deriveFailureGenres {
			if sum.ByGenre[g] != 0 {
				t.Errorf("empty window reported %s = %d", g, sum.ByGenre[g])
			}
		}
		if sum.Consecutive != 0 {
			t.Errorf("empty window Consecutive = %d", sum.Consecutive)
		}
	})

	t.Run("d222_four_nights_of_one_genre", func(t *testing.T) {
		var window []db.DeriveRunOutcome
		for i := 0; i < 4; i++ {
			window = append(window, db.DeriveRunOutcome{Status: "failed", Metadata: allowlist})
		}
		sum := summarizeDeriveFailures(window)
		if sum.Consecutive != 4 || sum.ByGenre[genreClientConfig] != 4 {
			t.Fatalf("D222 shape not visible: consecutive=%d bygenre=%+v", sum.Consecutive, sum.ByGenre)
		}
	})

	t.Run("prefixed_status_folds_through_deriveStatusClass", func(t *testing.T) {
		sum := summarizeDeriveFailures([]db.DeriveRunOutcome{
			{Status: "failed:whatever", Metadata: allowlist},
			{Status: "skipped:project_denied"},
			{Status: "failed", Metadata: allowlist},
		})
		if sum.Consecutive != 1 || sum.ByGenre[genreClientConfig] != 2 {
			t.Fatalf("consecutive=%d bygenre=%+v", sum.Consecutive, sum.ByGenre)
		}
	})
}

func TestEscapeLabel(t *testing.T) {
	if got := escapeLabel("a\\b\"c\nd"); got != `a\\b\"c\nd` {
		t.Fatalf("escapeLabel = %q", got)
	}
}
