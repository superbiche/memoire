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
	}

	fresh, freshOK, runs, runsOK := c.snapshot()
	if !freshOK || !runsOK || fresh.Missing != 7 || len(runs) != 1 {
		t.Fatalf("first snapshot = %+v %v %+v %v", fresh, freshOK, runs, runsOK)
	}
	if freshCalls != 1 || runCalls != 1 {
		t.Fatalf("first snapshot computed %d/%d times, want 1/1", freshCalls, runCalls)
	}

	// Inside the TTL: served from cache, zero store work.
	now = now.Add(metricsHeavyTTL - time.Second)
	if _, _, _, _ = c.snapshot(); freshCalls != 1 || runCalls != 1 {
		t.Fatalf("in-window snapshot recomputed (%d/%d)", freshCalls, runCalls)
	}

	// Past the TTL: recomputes; a compute failure is served as ok=false...
	freshErr = errors.New("boom")
	now = now.Add(2 * time.Second)
	if _, freshOK, _, _ = c.snapshot(); freshOK || freshCalls != 2 {
		t.Fatalf("post-TTL snapshot freshOK=%v calls=%d, want false/2", freshOK, freshCalls)
	}
	// ...and the failure itself is cached for the window — no per-scrape retry.
	now = now.Add(time.Second)
	if _, _, _, _ = c.snapshot(); freshCalls != 2 {
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
	}
	_, freshOK, runs, runsOK := c.snapshot()
	if freshOK {
		t.Fatal("exhausted freshness compute reported ok")
	}
	if !runsOK || len(runs) != 1 {
		t.Fatalf("run family starved by the freshness budget: runsOK=%v runs=%+v", runsOK, runs)
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

func TestEscapeLabel(t *testing.T) {
	if got := escapeLabel("a\\b\"c\nd"); got != `a\\b\"c\nd` {
		t.Fatalf("escapeLabel = %q", got)
	}
}
