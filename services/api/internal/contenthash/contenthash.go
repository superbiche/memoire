// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Package contenthash computes the canonical observation content hash — the
// dedupe/overlap key over (project, kind, normalized text). Extracted from
// internal/derive (slice E, memoire:D146) so the store can hash
// operator-authored rewrite observations without an import cycle
// (derive imports db). derive.CanonicalContentHash delegates here; the hash
// definition is unchanged and MUST stay byte-compatible with every stored
// observations.content_hash value.
package contenthash

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"golang.org/x/text/unicode/norm"
)

func Canonical(project, kind, text string) (string, error) {
	b, err := json.Marshal(map[string]string{
		"project":         project,
		"kind":            kind,
		"normalized_text": Normalize(text),
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Normalize collapses whitespace runs to single spaces after NFC
// normalization — identical to the derive-side normalization it was
// extracted from.
func Normalize(text string) string {
	text = norm.NFC.String(text)
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(text))
	lastWasSpace := false
	for _, r := range text {
		if isASCIIWhitespace(r) {
			if lastWasSpace {
				continue
			}
			b.WriteByte(' ')
			lastWasSpace = true
			continue
		}
		lastWasSpace = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func isASCIIWhitespace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}
