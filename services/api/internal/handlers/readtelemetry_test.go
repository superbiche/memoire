// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package handlers

import (
	"testing"

	"memoire/services/api/internal/db"
)

func TestFilterRawScope(t *testing.T) {
	cases := []struct {
		f    db.ObservationFilter
		want int
	}{
		{db.ObservationFilter{}, rawIncluded},
		{db.ObservationFilter{ExcludeKind: "raw"}, rawExcluded},
		{db.ObservationFilter{ExcludeKind: "decision"}, rawIncluded},
		{db.ObservationFilter{Kind: "raw"}, rawIncluded},
		{db.ObservationFilter{Kind: "fact"}, rawExcluded},
		{db.ObservationFilter{Kind: "fact", ExcludeKind: "decision"}, rawExcluded},
		// Both predicates apply: kind=raw AND kind!=raw admits nothing (the MCP
		// client defaults exclude_kind=raw even when kind=raw is passed).
		{db.ObservationFilter{Kind: "raw", ExcludeKind: "raw"}, rawExcluded},
		{db.ObservationFilter{Kind: "raw", ExcludeKind: "decision"}, rawIncluded},
	}
	for _, c := range cases {
		if got := filterRawScope(c.f); got != c.want {
			t.Errorf("filterRawScope(%+v) = %d, want %d", c.f, got, c.want)
		}
	}
}

func TestAgeBucketBoundaries(t *testing.T) {
	const day = 86400
	now := int64(10_000 * day)
	cases := []struct {
		age  int64
		want int
	}{
		{-60, ageLT1d}, // future-skewed item: counted as fresh
		{0, ageLT1d},
		{day - 1, ageLT1d},
		{day, age1to7d},
		{7*day - 1, age1to7d},
		{7 * day, age7to30d},
		{30 * day, age30to90d},
		{90*day - 1, age30to90d},
		{90 * day, ageGT90d},
	}
	for _, c := range cases {
		if got := ageBucket(now, now-c.age); got != c.want {
			t.Errorf("ageBucket(age=%d) = %s, want %s", c.age, ageBucketNames[got], ageBucketNames[c.want])
		}
	}
}

func TestReadTelemetryNilSafe(t *testing.T) {
	var tel *readTelemetry
	tel.request(readSearch, rawIncluded)
	tel.hits(readSearch, db.ObservationFilter{}, []db.SearchHit{{Kind: "raw"}})
	tel.records(readObservationGet, db.ObservationFilter{}, []db.ObservationRecord{{Kind: "fact"}})
}
