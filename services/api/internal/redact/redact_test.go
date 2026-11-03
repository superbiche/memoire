// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package redact

import "testing"

// The pin is the contract Memoire declares in .agents/contracts.json; a core
// upgrade that changes it must change that pin in the same commit.
func TestRulesVersionPin(t *testing.T) {
	if RulesVersion != "redact-v7" {
		t.Fatalf("RulesVersion = %q, want redact-v7", RulesVersion)
	}
}
