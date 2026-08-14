// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import "testing"

// Round-1 finding 4: the query transforms must keep operator-shaped tokens
// inert. SanitizeFTS emits '"tok" OR "tok"'; the fallback passes that
// VERBATIM to websearch_to_tsquery (quotes are phrase syntax there, which
// neutralizes OR/-rf as terms), and ftsToMatchText must strip exactly the
// generated joiners — never a literal OR token.
func TestFTSToMatchText(t *testing.T) {
	cases := []struct{ in, want string }{
		{SanitizeFTS("alpha beta"), "alpha beta"},
		{SanitizeFTS("OR"), "OR"},
		{SanitizeFTS("alpha OR beta"), "alpha OR beta"}, // OR is a real token from the user
		{SanitizeFTS("rm -rf /tmp"), "rm -rf /tmp"},
		{SanitizeFTS("ext_log_entries pm.max_children"), "ext_log_entries pm.max_children"},
		{SanitizeFTS(""), ""},
	}
	for _, c := range cases {
		if got := ftsToMatchText(c.in); got != c.want {
			t.Errorf("ftsToMatchText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeFTSIsValidWebsearchInput(t *testing.T) {
	// The fallback contract: SanitizeFTS output goes to websearch_to_tsquery
	// unchanged, so every token must stay double-quoted (phrase-neutralized)
	// and joiners must be bare OR. Tokens cannot contain quotes or spaces
	// (tokenRE), so no escaping cases exist.
	if got := SanitizeFTS(`alpha "quoted" OR -rf`); got != `"alpha" OR "quoted" OR "OR" OR "-rf"` {
		t.Errorf("SanitizeFTS shape drifted: %q", got)
	}
}
