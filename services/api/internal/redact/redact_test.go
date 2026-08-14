// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package redact

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// Token-shaped fixtures are concatenation-split so no source blob carries a
// contiguous secret shape: GitHub push protection blocked the v0.7.0 public
// push on the Slack fixture, and downstream scanners (trufflehog, forks' CI)
// match the same shapes. Compile-time folding hands the redactor the exact
// same runtime strings. AKIAIOSFODNN7EXAMPLE stays literal — it is AWS's
// canonical documentation key, allowlisted by scanners.
const fakePEM = "-----BEGIN RSA PRIVATE " + "KEY-----\nMIIEfakefakefakefake\nfakefakefake==\n-----END RSA PRIVATE " + "KEY-----"

func plain() *Redactor { return New(nil) }

func TestStringRedactsKnownShapes(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		rule   string
		secret string
	}{
		{"pem", "before\n" + fakePEM + "\nafter", "pem-block", fakePEM},
		{"jwt", "token eyJhbGciOiJIUzI1NiJ9." + "eyJzdWIiOiIxMjM0NTY3ODkwIn0." + "dozjgNryP4J3jVmNHl0w5N_XgL0n3I9P", "jwt", "eyJhbGci"},
		{"aws", "key AKIAIOSFODNN7EXAMPLE here", "aws-access-key", "AKIAIOSFODNN7EXAMPLE"},
		{"openai", "use sk-" + "abcdefghijklmnopqrstuvwxyz123456", "openai-key", "sk-" + "abcdefghijklmnop"},
		{"github", "push ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789", "github-token", "ghp_"},
		{"github-pat", "pat github_pat_" + "11ABCDEFG0abcdefghijklmnop", "github-fine-grained-pat", "github_pat_" + "11ABCDEFG0"},
		{"slack", "hook xoxb-" + "123456789012-abcdefghijklmnop", "slack-token", "xoxb-"},
		{"bearer", `curl -H "Authorization: Bearer supersecrettoken123"`, "bearer-header", "supersecrettoken123"},
		{"env", "DATABASE_PASSWORD=hunter2hunter2", "env-secret-assignment", "hunter2hunter2"},
		{"env-export", "export HL3_QWEN_API_KEY=verysecretvalue99", "env-secret-assignment", "verysecretvalue99"},
		{"env-lowercase", "db_password=lowersecret42x", "env-secret-assignment", "lowersecret42x"},
		{"env-quoted-spaces", `APP_SECRET="my secret with spaces"`, "env-secret-assignment", "my secret with spaces"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, hits := plain().String(tc.in)
			if strings.Contains(out, tc.secret) {
				t.Fatalf("secret survived redaction: %s", out)
			}
			if !strings.Contains(out, "[REDACTED:"+tc.rule+":") {
				t.Fatalf("missing %s marker in: %s", tc.rule, out)
			}
			if len(hits) == 0 {
				t.Fatalf("no hits reported")
			}
		})
	}
}

func TestStringKeepsBearerPrefixReadable(t *testing.T) {
	out, _ := plain().String("Authorization: Bearer topsecretbearer99")
	if !strings.Contains(out, "Authorization: Bearer [REDACTED:bearer-header:") {
		t.Fatalf("bearer prefix should survive: %s", out)
	}
}

func TestExemptionsSurvive(t *testing.T) {
	for _, in := range []string{
		"DATABASE_PASSWORD=op://Vault/db/password",
		"API_TOKEN=${SOME_TEMPLATE_VAR}",
		"MY_SECRET=<your-secret-here>",
		"AWS_SECRET_ACCESS_KEY=changeme-later",
	} {
		out, hits := plain().String(in)
		if out != in {
			t.Fatalf("exempt value was redacted: %q -> %q", in, out)
		}
		if len(hits) != 0 {
			t.Fatalf("exempt value reported hits: %v", hits)
		}
	}
}

func TestRedactionIsIdempotent(t *testing.T) {
	r := plain()
	once, _ := r.String("PASSWORD=verysecret123")
	twice, hits := r.String(once)
	if twice != once {
		t.Fatalf("second pass changed output: %q -> %q", once, twice)
	}
	if len(hits) != 0 {
		t.Fatalf("second pass reported hits: %v", hits)
	}
}

func TestMarkerIsStablePerSecret(t *testing.T) {
	r := plain()
	a, _ := r.String("TOKEN_A=samesecretvalue1")
	b, _ := r.String("TOKEN_B=samesecretvalue1")
	suffix := func(s string) string {
		i := strings.LastIndex(s, ":")
		return s[i:]
	}
	if suffix(a) != suffix(b) {
		t.Fatalf("same secret produced different markers: %q vs %q", a, b)
	}
}

func TestKeyedMarkersResistDictionaryCorrelation(t *testing.T) {
	// The same secret must produce DIFFERENT markers under different keys
	// (unsalted SHA-256 prefixes of weak passwords are dictionary-recoverable),
	// and stable markers under the same key.
	k1, k2 := New([]byte("key-one-key-one-key-one-key-one!")), New([]byte("key-two-key-two-key-two-key-two!"))
	a, _ := k1.String("PASSWORD=weakpassword1")
	b, _ := k2.String("PASSWORD=weakpassword1")
	c, _ := k1.String("PASSWORD=weakpassword1")
	if a == b {
		t.Fatalf("different keys must produce different markers")
	}
	if a != c {
		t.Fatalf("same key must produce stable markers")
	}
	p, _ := plain().String("PASSWORD=weakpassword1")
	if p == a {
		t.Fatalf("keyed marker must differ from plain-hash marker")
	}
}

func TestLoadOrCreateKeyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "redaction-key")
	k1, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(k1) != 32 {
		t.Fatalf("key length = %d, want 32", len(k1))
	}
	k2, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if string(k1) != string(k2) {
		t.Fatalf("key not stable across loads")
	}
}

func TestJSONWalksNestedValues(t *testing.T) {
	raw := json.RawMessage(`{"tool_input":{"file_path":"/x/.env","content":"API_KEY=deepnestedsecret1"},"list":["AKIAIOSFODNN7EXAMPLE"],"n":3}`)
	out, hits := plain().JSON(raw)
	s := string(out)
	if strings.Contains(s, "deepnestedsecret1") || strings.Contains(s, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("secret survived JSON redaction: %s", s)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 rules hit, got %v", hits)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if v["n"].(float64) != 3 {
		t.Fatalf("non-string values must be untouched")
	}
}

func TestJSONRedactsSecretShapedKeys(t *testing.T) {
	// A secret used as a map KEY must not reach the store (Codex review).
	raw := json.RawMessage(`{"payload":{"AKIAIOSFODNN7EXAMPLE":"seen at 3pm"}}`)
	out, hits := plain().JSON(raw)
	if strings.Contains(string(out), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("secret key survived: %s", string(out))
	}
	if !strings.Contains(string(out), "[REDACTED:aws-access-key:") {
		t.Fatalf("key marker missing: %s", string(out))
	}
	if len(hits) != 1 {
		t.Fatalf("expected key hit, got %v", hits)
	}
}

func TestJSONPreservesLargeIntegers(t *testing.T) {
	// float64 round-trips corrupt integers beyond 2^53 (Codex review): the
	// walk must decode with UseNumber so untouched numbers stay bit-exact
	// even when a sibling string is redacted.
	raw := json.RawMessage(`{"big":9007199254740993,"secret":"PASSWORD=verysecret123"}`)
	out, _ := plain().JSON(raw)
	if !strings.Contains(string(out), "9007199254740993") {
		t.Fatalf("large integer corrupted: %s", string(out))
	}
	if strings.Contains(string(out), "verysecret123") {
		t.Fatalf("secret survived: %s", string(out))
	}
}

func TestJSONCleanInputStaysByteIdentical(t *testing.T) {
	raw := json.RawMessage(`{"message":  "nothing secret here",   "n": 1}`)
	out, hits := plain().JSON(raw)
	if string(out) != string(raw) {
		t.Fatalf("clean input was rewritten: %q -> %q", string(raw), string(out))
	}
	if hits != nil {
		t.Fatalf("clean input reported hits: %v", hits)
	}
}

func TestMergeHits(t *testing.T) {
	merged := MergeHits([]Hit{{Rule: "jwt", Count: 1}}, []Hit{{Rule: "jwt", Count: 2}, {Rule: "pem-block", Count: 1}})
	if len(merged) != 2 || merged[0].Rule != "jwt" || merged[0].Count != 3 {
		t.Fatalf("unexpected merge: %v", merged)
	}
}
