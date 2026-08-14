// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Package redact is the defense-at-source secret filter (GOVERNANCE-DESIGN.md
// ruling 7, 2026-07-02). It runs at the queue-writer boundary — before any
// pending file, raw/ custody copy, or SQLite row exists — so secret-shaped
// content captured by harness hooks (changed .env files, key material in tool
// responses, bearer headers in prompts) never enters the store.
//
// Rules-only by contract: no LLM on the write path, no entropy guessing.
// Detection is limited to well-known secret shapes plus env-style secret
// assignments, with an explicit exemption list for reference/placeholder
// shapes (op:// handles, ${VAR} templates, <placeholders>) that are safe by
// design and useful to keep readable.
//
// A match is replaced by "[REDACTED:<rule>:<hash8>]" where hash8 is the first
// 8 hex chars of the secret's SHA-256 — occurrences of the same secret
// correlate across events without revealing the value.
package redact

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// RulesVersion identifies the rule set for audit metadata. Bump on any rule
// change so stored redaction markers stay interpretable.
const RulesVersion = "redact-v1"

// Hit reports one rule's matches within a redacted document.
type Hit struct {
	Rule  string `json:"rule"`
	Count int    `json:"count"`
}

type rule struct {
	id string
	re *regexp.Regexp
	// group is the capture group holding the secret; 0 redacts the whole match.
	group int
}

// Rules are ordered: structural shapes first (PEM, JWT), then provider token
// formats, then the generic assignment rules so specific markers win. The env
// key match is case-insensitive (lowercase .env names are common), and quoted
// values may contain spaces (Codex review, 2026-07-02).
var rules = []rule{
	{id: "pem-block", re: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), group: 0},
	{id: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}\b`), group: 0},
	{id: "aws-access-key", re: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), group: 0},
	{id: "openai-key", re: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`), group: 0},
	{id: "github-token", re: regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b`), group: 0},
	{id: "github-fine-grained-pat", re: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`), group: 0},
	{id: "gitlab-token", re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`), group: 0},
	{id: "slack-token", re: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), group: 0},
	{id: "bearer-header", re: regexp.MustCompile(`(?i)\b(authorization\s*[:=]\s*"?bearer\s+)([A-Za-z0-9._~+/-]{8,}=*)`), group: 2},
	{id: "env-secret-assignment", re: regexp.MustCompile(`(?im)^\s*(?:export\s+)?[A-Za-z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|API_KEY|APIKEY|PRIVATE_KEY|ACCESS_KEY|CREDENTIALS?)[A-Za-z0-9_]*\s*[=:]\s*"([^"]{8,})"`), group: 1},
	{id: "env-secret-assignment", re: regexp.MustCompile(`(?im)^\s*(?:export\s+)?[A-Za-z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|API_KEY|APIKEY|PRIVATE_KEY|ACCESS_KEY|CREDENTIALS?)[A-Za-z0-9_]*\s*[=:]\s*'([^']{8,})'`), group: 1},
	{id: "env-secret-assignment", re: regexp.MustCompile(`(?im)^\s*(?:export\s+)?[A-Za-z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|API_KEY|APIKEY|PRIVATE_KEY|ACCESS_KEY|CREDENTIALS?)[A-Za-z0-9_]*\s*[=:]\s*([^\s'"]{8,})`), group: 1},
}

// exempt reports reference/placeholder shapes that look like secret values but
// are safe by design; redacting them destroys information for no protection.
func exempt(value string) bool {
	v := strings.TrimSpace(value)
	switch {
	case strings.HasPrefix(v, "op://"):
		return true // 1Password reference — a pointer, not a value
	case strings.HasPrefix(v, "${") || strings.HasPrefix(v, "$("):
		return true // shell/template expansion
	case strings.HasPrefix(v, "<") && strings.HasSuffix(v, ">"):
		return true // <placeholder>
	case strings.HasPrefix(v, "[REDACTED:"):
		return true // already redacted — idempotency
	}
	lower := strings.ToLower(v)
	for _, p := range []string{"changeme", "change-me", "placeholder", "example", "xxxxx", "dummy", "your-", "your_"} {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// Redactor applies the rule set with an optional HMAC key for markers. A keyed
// Redactor produces markers an offline attacker cannot dictionary-attack
// (Codex review 2026-07-02: unsalted SHA-256 prefixes of low-entropy passwords
// are recoverable); the key lives locally (LoadOrCreateKey) and never enters
// the store. A nil key falls back to plain SHA-256 — correlation still works,
// used only when the key file is unavailable.
type Redactor struct {
	hmacKey []byte
}

// New returns a Redactor. key may be nil (plain-hash fallback).
func New(key []byte) *Redactor {
	return &Redactor{hmacKey: key}
}

// LoadOrCreateKey reads the local redaction key, creating it (32 random bytes,
// mode 0600) on first use. The key is machine-local secret material: losing it
// only breaks cross-event marker correlation, never data.
func LoadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 16 {
		return b, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func (r *Redactor) marker(ruleID, secret string) string {
	var digest []byte
	if len(r.hmacKey) > 0 {
		mac := hmac.New(sha256.New, r.hmacKey)
		mac.Write([]byte(secret))
		digest = mac.Sum(nil)
	} else {
		sum := sha256.Sum256([]byte(secret))
		digest = sum[:]
	}
	return fmt.Sprintf("[REDACTED:%s:%s]", ruleID, hex.EncodeToString(digest)[:8])
}

// String redacts one plain-text document and reports per-rule hit counts.
func (r *Redactor) String(s string) (string, []Hit) {
	if s == "" {
		return s, nil
	}
	counts := map[string]int{}
	for _, ru := range rules {
		s = ru.re.ReplaceAllStringFunc(s, func(match string) string {
			secret := match
			prefixEnd, secretEnd := 0, len(match)
			if ru.group > 0 {
				idx := ru.re.FindStringSubmatchIndex(match)
				if idx == nil || idx[2*ru.group] < 0 {
					return match
				}
				prefixEnd, secretEnd = idx[2*ru.group], idx[2*ru.group+1]
				secret = match[prefixEnd:secretEnd]
			}
			if exempt(secret) {
				return match
			}
			counts[ru.id]++
			return match[:prefixEnd] + r.marker(ru.id, secret) + match[secretEnd:]
		})
	}
	return s, hitsFromCounts(counts)
}

// JSON redacts every string value AND every string key (recursively) in a JSON
// document — a secret used as a map key must not reach the store either (Codex
// review 2026-07-02). Numbers are decoded with UseNumber so large integers
// survive the round-trip bit-exact. A document that fails to parse is treated
// as one opaque string.
func (r *Redactor) JSON(raw json.RawMessage) (json.RawMessage, []Hit) {
	if len(raw) == 0 {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		s, hits := r.String(string(raw))
		return json.RawMessage(s), hits
	}
	// Exactly one JSON value, nothing trailing: Decode stops after the first
	// value, so `{...}JUNK` with a redaction hit would otherwise be
	// re-marshaled into a REPAIRED valid document — turning input the queue
	// drainer must reject loudly into an ingestable envelope (slice-3 round-2
	// finding 1). Redact such input as a plain string instead: secrets still
	// die at source, malformedness survives to fail at drain.
	if _, terr := dec.Token(); terr != io.EOF {
		s, hits := r.String(string(raw))
		return json.RawMessage(s), hits
	}
	counts := map[string]int{}
	v = r.walk(v, counts)
	if len(counts) == 0 {
		return raw, nil // untouched input stays byte-identical
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw, nil // never fail the write path over redaction
	}
	return out, hitsFromCounts(counts)
}

func (r *Redactor) walk(v any, counts map[string]int) any {
	switch t := v.(type) {
	case string:
		s, hits := r.String(t)
		for _, h := range hits {
			counts[h.Rule] += h.Count
		}
		return s
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			nk, keyHits := r.String(k)
			for _, h := range keyHits {
				counts[h.Rule] += h.Count
			}
			out[nk] = r.walk(val, counts)
		}
		return out
	case []any:
		for i, val := range t {
			t[i] = r.walk(val, counts)
		}
		return t
	default:
		return v // includes json.Number — passes through bit-exact
	}
}

func hitsFromCounts(counts map[string]int) []Hit {
	if len(counts) == 0 {
		return nil
	}
	hits := make([]Hit, 0, len(counts))
	for id, n := range counts {
		hits = append(hits, Hit{Rule: id, Count: n})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Rule < hits[j].Rule })
	return hits
}

// MergeHits sums per-rule counts across several redaction passes (text +
// payload + metadata of one envelope).
func MergeHits(groups ...[]Hit) []Hit {
	counts := map[string]int{}
	for _, g := range groups {
		for _, h := range g {
			counts[h.Rule] += h.Count
		}
	}
	return hitsFromCounts(counts)
}
