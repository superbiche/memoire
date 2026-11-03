// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package queuefile

import (
	"strings"
	"testing"
)

func TestAdapter(t *testing.T) {
	h := strings.Repeat("ab", 32)
	cases := []struct {
		name string
		want string
		ok   bool
	}{
		{"1790813648369-claude-code-" + h + ".json", "claude-code", true},
		{"1-codex-cli-" + h + ".json", "codex-cli", true},
		{"1-http-" + h + ".json", "http", true},
		{"1-unknown-" + h + ".json", "unknown", true},
		{"001-a.json", "", false},
		{"1-claude-code-" + h + ".json.tmp", "", false},
		{"1-claude-code-" + h[:63] + ".json", "", false},
		{"1-claude-code-" + strings.Repeat("g", 64) + ".json", "", false},
		{"x-claude-code-" + h + ".json", "", false},
		{"1--" + h + ".json", "", false},
	}
	for _, c := range cases {
		got, ok := Adapter(c.name)
		if got != c.want || ok != c.ok {
			t.Errorf("Adapter(%q) = %q,%v want %q,%v", c.name, got, ok, c.want, c.ok)
		}
	}
}
