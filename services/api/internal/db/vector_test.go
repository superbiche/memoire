// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// The SQLite engine never grows a vector lane (D218): every lane method
// must refuse with ErrNoVectorLane and the health surface must say
// "absent" — a misconfigured worker pointed at a standalone store fails
// loud, not silent.
func TestVectorLaneAbsentOnSQLite(t *testing.T) {
	t.Setenv("MEMOIRE_HOME", t.TempDir())
	store, err := Open(filepath.Join(t.TempDir(), "test.db"), "../../../../schema")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	if store.HasPGVector() {
		t.Fatal("SQLite store reports a vector lane")
	}
	if got := store.VectorLane(); got != "absent" {
		t.Fatalf("VectorLane() = %q, want absent", got)
	}
	ctx := context.Background()
	if _, err := store.ListObservationsMissingEmbedding(ctx, DefaultEmbedModel, 10); !errors.Is(err, ErrNoVectorLane) {
		t.Fatalf("ListObservationsMissingEmbedding err = %v, want ErrNoVectorLane", err)
	}
	if _, err := store.EmbeddingFreshnessStats(ctx, DefaultEmbedModel); !errors.Is(err, ErrNoVectorLane) {
		t.Fatalf("EmbeddingFreshnessStats err = %v, want ErrNoVectorLane", err)
	}
	if err := store.UpsertObservationEmbeddings(ctx, []EmbeddingUpsert{{ObservationID: 1}}); !errors.Is(err, ErrNoVectorLane) {
		t.Fatalf("UpsertObservationEmbeddings err = %v, want ErrNoVectorLane", err)
	}
}

func TestVectorLiteral(t *testing.T) {
	got := vectorLiteral([]float32{0.5, -1, 0.25})
	if got != "[0.5,-1,0.25]" {
		t.Fatalf("vectorLiteral = %q", got)
	}
}
