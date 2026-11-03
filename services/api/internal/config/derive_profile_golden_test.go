// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The derive bench computes its window ceiling from the same profile object
// this package reads (bench/derive/benchlib.py effective_max_prompt_bytes), so
// "the ceiling this endpoint declares" must mean the identical integer on both
// sides. bench/derive/tests/derive-profile-golden.json is the shared oracle:
// this test and bench/derive/tests/profile-test.py assert the same vectors, so
// a reordered expression or a changed rounding rule fails `make test` instead
// of silently benching a different window than production runs.
//
// The cases are exact integers on purpose — floor() admits no tolerance, and
// the rounding-boundary vector exists precisely to catch a formula that
// multiplies before subtracting.
const profileGoldenPath = "../../../../bench/derive/tests/derive-profile-golden.json"

func TestDeriveProfileGoldenVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(profileGoldenPath))
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var golden struct {
		Cases []struct {
			Name     string        `json:"name"`
			Profile  DeriveProfile `json:"profile"`
			Expected int           `json:"effective_max_prompt_bytes"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parse golden vectors: %v", err)
	}
	if len(golden.Cases) == 0 {
		t.Fatal("golden vectors carry no cases")
	}
	for _, c := range golden.Cases {
		profile := c.Profile
		if err := validateDeriveProfile(c.Name, profile); err != nil {
			t.Fatalf("%s: golden profile must be valid geometry: %v", c.Name, err)
		}
		if got := profile.EffectiveMaxPromptBytes(); got != c.Expected {
			t.Fatalf("%s: EffectiveMaxPromptBytes() = %d, golden says %d", c.Name, got, c.Expected)
		}
	}
}
