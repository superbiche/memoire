// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package buildinfo

import (
	"strings"
	"testing"
)

// The public-version grammar is a closed shape (D224; review rounds 1-2
// F1): version tokens pass, everything else — DSNs, paths, and crucially
// BARE secret-shaped alphanumeric tokens — collapses to "dev".
func TestPublicVersionGrammar(t *testing.T) {
	accepted := []string{
		"dev",
		"0.6.0",
		"0.7.0",
		"10.20.30",
		"0.6.0-dev.gb3398a1",
		"0.7.0-rc1",
		"1.0.0+build.5",
	}
	rejected := []string{
		"",
		"0.6",
		"0.6.0 ",
		" 0.6.0",
		"v0.6.0",
		"1.2.3-",
		"-1.2.3",
		"AKIA" + "IOSFODNN7EXAMPLE",
		"ghp_" + "16C7e42F292c6912E7710c838347Ae178B4a",
		"postgres://probe.invalid/db?token=review-secret",
		"/etc/passwd",
		"0.6.0-" + strings.Repeat("a", 60),
		"123456.0.0",
		"1.2.3.4",
	}
	old := Version
	t.Cleanup(func() { Version = old })
	for _, v := range accepted {
		Version = v
		if got := Public(); got != v {
			t.Errorf("Public() rejected legitimate version %q (got %q)", v, got)
		}
	}
	for _, v := range rejected {
		Version = v
		if got := Public(); got != "dev" {
			t.Errorf("Public() let %q reach the wire as %q", v, got)
		}
	}
}
