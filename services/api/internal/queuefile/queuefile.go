// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Package queuefile is the single naming contract for queue-directory
// entries. What consumers process and what accounting reports must be the
// SAME predicate (review 2026-08-03 N2): a name the drainer or forwarder
// would pick up is backlog, and nothing else is — a per-surface temp
// heuristic forks the semantics and either hides real work or reports
// phantom work.
package queuefile

import "strings"

// hashHexLen is the length of the full sha256 hex content hash that ends every
// queue item name (queuewriter.queueFileNameAt).
const hashHexLen = 64

// IsItem reports whether a queue-directory entry is a processable item.
// The drainer's pending pick and the forwarder's pick select exactly
// these; every accounting surface (/v1/health depths and age, /metrics,
// the forwarder's -status) counts exactly these. Temp artifacts — the
// queue writer's ".<name>.json.tmp", the readiness probe's
// ".readiness-probe-*.tmp" — do not match, so a crash-leaked temp can
// neither be ingested nor become permanent fake backlog (review F3).
func IsItem(name string) bool {
	return strings.HasSuffix(name, ".json")
}

// IsErrorSidecar reports the drainer's ".error.json" failure sidecar. A
// sidecar rides its base item (they live and die as a unit) and is
// excluded from failed-depth counts: one poison item reports as depth 1.
func IsErrorSidecar(name string) bool {
	return strings.HasSuffix(name, ".error.json")
}

// Adapter extracts the adapter field of a queue item name,
// "<ms>-<adapter>-<hash>.json" (queuewriter.queueFileNameAt). The adapter may
// itself contain dashes ("claude-code"), so the name is parsed from both ends:
// a decimal millisecond stamp up to the first dash, a 64-character hex hash
// after the last one. A name that does not have that shape reports false.
func Adapter(name string) (string, bool) {
	base, ok := strings.CutSuffix(name, ".json")
	if !ok {
		return "", false
	}
	first := strings.IndexByte(base, '-')
	last := strings.LastIndexByte(base, '-')
	if first <= 0 || last <= first+1 || len(base)-last-1 != hashHexLen {
		return "", false
	}
	for _, c := range base[:first] {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	for _, c := range base[last+1:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return base[first+1 : last], true
}
