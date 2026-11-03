// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package handlers

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"memoire/services/api/internal/db"
)

// Read telemetry (records/history/L0009, ruling a2 = A1): which read routes
// are called, whether they reach raw projections, and how old the items
// they return are. Retention rulings rest on this instead of client-side
// proxies. Counters only, in-process (reset on restart; VMAgent keeps the
// series), and every label is a compiled-in closed vocabulary: no query,
// project, session or stored string reaches /metrics (memoire:D224).

type readRoute int

const (
	readSearch readRoute = iota
	readContext
	readTimeline
	readObservationGet
	readObservationList
	readSessionGet
	readSessionList
	readRouteCount
)

var readRouteNames = [readRouteCount]string{
	"search", "context", "timeline", "observation_get", "observation_list", "session_get", "session_list",
}

// rawScope is whether the request's filter lets kind=raw projections through;
// rawNA for routes that return no observations (sessions).
const (
	rawExcluded = iota
	rawIncluded
	rawNA
	rawScopeCount
)

var rawScopeNames = [rawScopeCount]string{"excluded", "included", "n_a"}

const (
	kindDerived = iota
	kindRaw
	kindClassCount
)

var kindClassNames = [kindClassCount]string{"derived", "raw"}

// Item age buckets, by the item's occurred_at relative to the read.
const (
	ageLT1d = iota
	age1to7d
	age7to30d
	age30to90d
	ageGT90d
	ageBucketCount
)

var ageBucketNames = [ageBucketCount]string{"lt_1d", "1d_7d", "7d_30d", "30d_90d", "gt_90d"}

type readTelemetry struct {
	now      func() time.Time
	requests [readRouteCount][rawScopeCount]atomic.Int64
	items    [readRouteCount][kindClassCount][ageBucketCount]atomic.Int64
}

func newReadTelemetry() *readTelemetry {
	return &readTelemetry{now: time.Now}
}

// filterRawScope mirrors the store filter, which applies Kind and ExcludeKind
// conjunctively (observationWhere): raw rows pass only when Kind is empty or
// "raw" AND ExcludeKind is not "raw".
func filterRawScope(f db.ObservationFilter) int {
	if (f.Kind == "" || f.Kind == "raw") && f.ExcludeKind != "raw" {
		return rawIncluded
	}
	return rawExcluded
}

func ageBucket(now, occurred int64) int {
	d := now - occurred
	switch {
	case d < 86400:
		return ageLT1d
	case d < 7*86400:
		return age1to7d
	case d < 30*86400:
		return age7to30d
	case d < 90*86400:
		return age30to90d
	default:
		return ageGT90d
	}
}

func kindClass(kind string) int {
	if kind == "raw" {
		return kindRaw
	}
	return kindDerived
}

// request counts one successful read.
func (t *readTelemetry) request(route readRoute, scope int) {
	if t == nil {
		return
	}
	t.requests[route][scope].Add(1)
}

// item counts one returned item of the given kind and occurred_at.
func (t *readTelemetry) item(route readRoute, kind string, occurredAt int64, now int64) {
	if t == nil {
		return
	}
	t.items[route][kindClass(kind)][ageBucket(now, occurredAt)].Add(1)
}

func (t *readTelemetry) hits(route readRoute, filter db.ObservationFilter, hits []db.SearchHit) {
	if t == nil {
		return
	}
	t.request(route, filterRawScope(filter))
	now := t.now().Unix()
	for _, h := range hits {
		t.item(route, h.Kind, h.OccurredAtEpoch, now)
	}
}

func (t *readTelemetry) records(route readRoute, filter db.ObservationFilter, recs []db.ObservationRecord) {
	if t == nil {
		return
	}
	t.request(route, filterRawScope(filter))
	now := t.now().Unix()
	for _, r := range recs {
		t.item(route, r.Kind, r.OccurredAtEpoch, now)
	}
}

// render appends the two counter families. Zero cells are emitted, so a
// route nobody calls reads as a measured 0, not as an absent series.
func (t *readTelemetry) render(b *strings.Builder) {
	if t == nil {
		return
	}
	b.WriteString("# HELP memoire_read_requests_total Successful read requests by route and whether the filter admits raw projections (L0009; in-process; resets on restart).\n# TYPE memoire_read_requests_total counter\n")
	for r := readRoute(0); r < readRouteCount; r++ {
		for s := 0; s < rawScopeCount; s++ {
			sessions := r == readSessionGet || r == readSessionList
			if sessions != (s == rawNA) {
				continue
			}
			fmt.Fprintf(b, "memoire_read_requests_total{route=\"%s\",raw=\"%s\"} %d\n", readRouteNames[r], rawScopeNames[s], t.requests[r][s].Load())
		}
	}
	b.WriteString("# HELP memoire_read_items_total Items returned by read routes, by kind class and age of the item's occurred_at at read time (L0009; in-process; resets on restart).\n# TYPE memoire_read_items_total counter\n")
	for r := readRoute(0); r < readRouteCount; r++ {
		if r == readSessionGet || r == readSessionList {
			continue // sessions are counted as requests only
		}
		for k := 0; k < kindClassCount; k++ {
			for a := 0; a < ageBucketCount; a++ {
				fmt.Fprintf(b, "memoire_read_items_total{route=\"%s\",kind=\"%s\",age=\"%s\"} %d\n", readRouteNames[r], kindClassNames[k], ageBucketNames[a], t.items[r][k][a].Load())
			}
		}
	}
}
