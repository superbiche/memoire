// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package handlers

import (
	"os"
	"path/filepath"
	"time"

	"memoire/services/api/internal/queuefile"
)

// All queue accounting gates on queuefile.IsItem — the SAME predicate the
// drainer and forwarder pick by — so depth/age report exactly the backlog
// a consumer would process: temp artifacts (writer temps, readiness
// probes) are invisible, and anything a consumer would take is counted
// (review 2026-08-03 F3 + round-2 N2).

// failedQueueDepth counts poison items in failed/, excluding the
// .error.json sidecar the drainer writes beside each one — one poison item
// must report as depth 1 (round-1 review finding 2).
func failedQueueDepth(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, entry := range entries {
		if entry.IsDir() || !queuefile.IsItem(entry.Name()) || queuefile.IsErrorSidecar(entry.Name()) {
			continue
		}
		n++
	}
	return n
}

func queueDepth(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, entry := range entries {
		if !entry.IsDir() && queuefile.IsItem(entry.Name()) {
			n++
		}
	}
	return n
}

func oldestQueueAgeSeconds(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var oldest time.Time
	for _, entry := range entries {
		if entry.IsDir() || !queuefile.IsItem(entry.Name()) {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		if oldest.IsZero() || info.ModTime().Before(oldest) {
			oldest = info.ModTime()
		}
	}
	if oldest.IsZero() {
		return 0
	}
	return int64(time.Since(oldest).Seconds())
}
