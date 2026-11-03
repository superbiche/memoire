// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

// Operator ruling a65: IsNoOpDuplicate is the drainer's skip-raw predicate.
// It must say true exactly when Ingest would write nothing — key stored AND
// session known — on both engines (the name keeps it inside `make test-pg`'s
// TestEnginePairConformance filter).
func TestEnginePairConformanceNoOpDuplicate(t *testing.T) {
	engines := map[string]func(t *testing.T) *Store{
		"sqlite": func(t *testing.T) *Store {
			store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), schemaDirForTest(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
		"postgres": func(t *testing.T) *Store {
			dsn, _ := conformancePGURL(t)
			store, err := Open(dsn, schemaDirForTest(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
	}
	for name, open := range engines {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			ctx := context.Background()
			ev := func(session, id string) EventInput {
				return EventInput{
					SchemaVersion: 1, Project: "p", SessionID: session, SourceAdapter: "codex-cli",
					SourceEventID: id, EventType: "codex_item", Payload: json.RawMessage(`{"x":1}`), Text: "t",
				}
			}
			probe := func(in EventInput) bool {
				t.Helper()
				ok, err := store.IsNoOpDuplicate(ctx, in)
				if err != nil {
					t.Fatal(err)
				}
				return ok
			}
			if probe(ev("s1", "e1")) {
				t.Fatal("unknown key reported as no-op duplicate")
			}
			if _, err := store.Ingest(ctx, ev("s1", "e1")); err != nil {
				t.Fatal(err)
			}
			if !probe(ev("s1", "e1")) {
				t.Fatal("stored key + known session must be a no-op duplicate")
			}
			if probe(ev("s2", "e1")) {
				t.Fatal("stored key under an UNKNOWN session is not a no-op (Ingest inserts the session)")
			}
			res, err := store.Ingest(ctx, ev("s1", "e1"))
			if err != nil || !res.Duplicate {
				t.Fatalf("ingest of the probed duplicate: %+v %v", res, err)
			}
		})
	}
}
