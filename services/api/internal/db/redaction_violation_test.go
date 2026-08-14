// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

// Also proves schema/0011 applied: the INDEXED BY in the count query errors
// out if idx_events_redaction_violation is missing.
func TestCountRedactionContractViolations(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	n, err := store.CountRedactionContractViolations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 violations on empty store, got %d", n)
	}

	if _, err := store.Ingest(ctx, EventInput{
		Project:       "liaison",
		SessionID:     "echo-1",
		SourceAdapter: "liaison-evidence",
		SourceEventID: "liaison-evidence-echo:v1",
		EventType:     "liaison_evidence",
		Payload:       json.RawMessage(`{}`),
		Metadata:      json.RawMessage(`{"redaction_contract_violation":true,"redaction_rules_version":"redact-v1"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(ctx, EventInput{
		Project:       "liaison",
		SessionID:     "echo-2",
		SourceAdapter: "liaison-evidence",
		SourceEventID: "liaison-evidence-echo:v2",
		EventType:     "liaison_evidence",
		Payload:       json.RawMessage(`{}`),
		Metadata:      json.RawMessage(`{"liaison_redaction_rules_version":"redact-v1"}`),
	}); err != nil {
		t.Fatal(err)
	}

	n, err = store.CountRedactionContractViolations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 stamped violation, got %d", n)
	}
}
