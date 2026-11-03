// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Package redact is the defense-at-source secret filter (GOVERNANCE-DESIGN.md
// ruling 7, 2026-07-02). It runs at the queue-writer boundary — before any
// pending file, raw/ custody copy, or store row exists — so secret-shaped
// content captured by harness hooks never enters the store.
//
// Memoire no longer carries its own implementation. This package forwards to
// the central Go core, github.com/superbiche/caviar/redact (Caviar v0.3.0,
// the public module; operator rulings a18/a23, a81/a82), which implements the
// shared contract redact-v7 (~/.agents/shared/contracts/redact-v7.md) and owns
// the rules, the port tests and the hash-pinned corpus. Nothing here may add logic: a rule change is a
// Caviar release and a version bump in go.mod, never an edit in this tree.
package redact

import core "github.com/superbiche/caviar/redact"

// RulesVersion names the contract version the forwarded core implements.
const RulesVersion = core.RulesVersion

// MaxNesting bounds the text P2 walk and the structured walk; beyond it the
// scrub fails closed with ErrDepth.
const MaxNesting = core.MaxNesting

// ErrDepth is the fail-closed nesting error: callers fail the write, never
// fall back to writing unredacted.
var ErrDepth = core.ErrDepth

// Hit reports one rule's matches within a redacted document.
type Hit = core.Hit

// Redactor applies the rule set with an optional HMAC key for markers.
type Redactor = core.Redactor

// New returns a Redactor. key may be nil (plain-hash fallback); a supplied key
// shorter than the core's MinKeyBytes panics.
func New(key []byte) *Redactor { return core.New(key) }

// LoadOrCreateKey reads the local redaction key, creating it (32 random
// bytes, mode 0600) on first use.
func LoadOrCreateKey(path string) ([]byte, error) { return core.LoadOrCreateKey(path) }

// MergeHits sums per-rule counts across several redaction passes.
func MergeHits(groups ...[]Hit) []Hit { return core.MergeHits(groups...) }
