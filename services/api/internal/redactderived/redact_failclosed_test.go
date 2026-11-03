// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package redactderived

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"memoire/services/api/internal/redact"
)

func deepDoc() string {
	return `{"d":` + strings.Repeat("[", redact.MaxNesting+1) + strings.Repeat("]", redact.MaxNesting+1) + `}`
}

func exec(t *testing.T, dbPath, stmt string) {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(stmt); err != nil {
		t.Fatal(err)
	}
}

// TestScanRefusesDepthError: a derived row the scrub cannot walk fails the
// run (dry-run and apply alike) instead of being skipped (review round 1, F7).
func TestScanRefusesDepthError(t *testing.T) {
	dbPath := openFixture(t)
	exec(t, dbPath, `UPDATE events SET payload_json = '`+deepDoc()+`' WHERE idempotency_key = 'k1'`)
	_, err := Run(context.Background(), Options{DBPath: dbPath, RedactionKeyPath: keyPath(t)})
	if !errors.Is(err, redact.ErrDepth) {
		t.Fatalf("want ErrDepth, got %v", err)
	}
}

// TestPreflightRefusesDepthError: the identity-path preflight propagates a
// scrub error instead of counting the column clean.
func TestPreflightRefusesDepthError(t *testing.T) {
	dbPath := openFixture(t)
	deepText := strings.Repeat(`{"secret":`, redact.MaxNesting+1) + `{}` + strings.Repeat(`}`, redact.MaxNesting+1)
	exec(t, dbPath, `INSERT INTO project_roots(project, root, source, first_seen_epoch, last_seen_epoch) VALUES ('memoire', '`+deepText+`', 'test', 1, 1)`)
	_, err := Run(context.Background(), Options{DBPath: dbPath, RedactionKeyPath: keyPath(t)})
	if !errors.Is(err, redact.ErrDepth) {
		t.Fatalf("want ErrDepth from the preflight, got %v", err)
	}
}
