// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The derive bench aligns its comparison packs with a Python port of this
// gate (bench/derive/benchlib.py), so "these two say the same thing" means in
// a pack exactly what it means here. bench/derive/tests/neardup-golden.json is
// the shared oracle: this test and bench/derive/tests/neardup-test.py assert
// the same vectors, so a change on either side that moves a score or flips a
// policy hit fails `make test` instead of silently drifting the bench.
//
// Regenerating the golden expectations to make a test pass is a recalibration
// of the D212 policy, which is an operator ruling — not a test fix.

const goldenPath = "../../../../bench/derive/tests/neardup-golden.json"

// Both implementations do float arithmetic over the same sets; compare within
// a tolerance far tighter than any gap between the two thresholds.
const goldenTolerance = 1e-9

type neardupGolden struct {
	Thresholds struct {
		Lo float64 `json:"lo"`
		Hi float64 `json:"hi"`
	} `json:"thresholds"`
	Normalize []struct {
		Name        string   `json:"name"`
		In          string   `json:"in"`
		Normalized  string   `json:"normalized"`
		DigitTokens []string `json:"digit_tokens"`
	} `json:"normalize"`
	Pairs []struct {
		Name  string  `json:"name"`
		A     string  `json:"a"`
		B     string  `json:"b"`
		Score float64 `json:"score"`
		Hit   bool    `json:"hit"`
	} `json:"pairs"`
}

func loadNearDupGolden(t *testing.T) neardupGolden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(goldenPath))
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var golden neardupGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parse golden vectors: %v", err)
	}
	if len(golden.Normalize) == 0 || len(golden.Pairs) == 0 {
		t.Fatal("golden vectors carry no cases")
	}
	return golden
}

func TestNearDupGoldenThresholds(t *testing.T) {
	golden := loadNearDupGolden(t)
	if golden.Thresholds.Lo != DefaultNearDupLoThreshold {
		t.Fatalf("golden lo threshold %v != %v", golden.Thresholds.Lo, DefaultNearDupLoThreshold)
	}
	if golden.Thresholds.Hi != DefaultNearDupHiThreshold {
		t.Fatalf("golden hi threshold %v != %v", golden.Thresholds.Hi, DefaultNearDupHiThreshold)
	}
}

func TestNearDupGoldenNormalization(t *testing.T) {
	for _, c := range loadNearDupGolden(t).Normalize {
		norm := normalizeForNearDup(c.In)
		if norm != c.Normalized {
			t.Errorf("normalize[%s]: got %q, want %q", c.Name, norm, c.Normalized)
		}
		var tokens []string
		for token := range digitTokens(norm) {
			tokens = append(tokens, token)
		}
		sort.Strings(tokens)
		want := c.DigitTokens
		if len(tokens) != len(want) {
			t.Errorf("digit_tokens[%s]: got %v, want %v", c.Name, tokens, want)
			continue
		}
		for i := range tokens {
			if tokens[i] != want[i] {
				t.Errorf("digit_tokens[%s]: got %v, want %v", c.Name, tokens, want)
				break
			}
		}
	}
}

func TestNearDupGoldenPairs(t *testing.T) {
	for _, c := range loadNearDupGolden(t).Pairs {
		score, hit := nearDupHit(buildNearDupProfile(c.A), buildNearDupProfile(c.B))
		if math.Abs(score-c.Score) > goldenTolerance {
			t.Errorf("pair[%s]: score %.9f, want %.9f", c.Name, score, c.Score)
		}
		if hit != c.Hit {
			t.Errorf("pair[%s]: hit %v, want %v", c.Name, hit, c.Hit)
		}
	}
}
