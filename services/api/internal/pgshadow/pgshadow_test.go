// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package pgshadow

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// The multiset hash is the parity verdict's foundation: it must be
// order-independent (collation differences between engines must not
// register) while still counting duplicates (XOR would cancel pairs).
func TestMultisetHashOrderIndependentButDuplicateSensitive(t *testing.T) {
	digest := func(rows ...string) string {
		var sum [sha256.Size]byte
		for _, r := range rows {
			h := sha256.Sum256([]byte(r))
			addDigest(&sum, h[:])
		}
		return hex.EncodeToString(sum[:])
	}
	if digest("a", "b", "c") != digest("c", "a", "b") {
		t.Fatal("multiset hash must be order-independent")
	}
	if digest("a", "b") == digest("a", "b", "b") {
		t.Fatal("multiset hash must count duplicates")
	}
	if digest("a", "a") == digest() {
		t.Fatal("identical pairs must not cancel (the XOR failure mode)")
	}
	if digest("a") == digest("b") {
		t.Fatal("distinct rows must hash differently")
	}
}

func TestAddDigestCarries(t *testing.T) {
	var sum [sha256.Size]byte
	all := make([]byte, sha256.Size)
	for i := range all {
		all[i] = 0xff
	}
	addDigest(&sum, all)
	one := make([]byte, sha256.Size)
	one[sha256.Size-1] = 1
	addDigest(&sum, one)
	// 0xff... + 1 == 0 mod 2^256: full carry propagation.
	for i, b := range sum {
		if b != 0 {
			t.Fatalf("expected zero after wraparound, byte %d = %#x", i, b)
		}
	}
}

func TestSetsEqualIsMultisetEquality(t *testing.T) {
	if !setsEqual([]string{"a", "b", "b"}, []string{"b", "a", "b"}) {
		t.Fatal("order must not matter")
	}
	if setsEqual([]string{"a", "b"}, []string{"a", "a"}) {
		t.Fatal("multiplicity must matter")
	}
	if setsEqual([]string{"a"}, []string{"a", "a"}) {
		t.Fatal("length must matter")
	}
}

func TestFormatValDistinguishesNullFromEmpty(t *testing.T) {
	if formatVal(nil) == formatVal("") {
		t.Fatal("NULL and empty string must hash differently")
	}
	if formatVal(int64(1)) != "1" || formatVal(true) != "1" {
		t.Fatal("integer/bool formatting must agree across drivers (sqlite int64 vs pg bool)")
	}
}
