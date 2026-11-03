// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package pgshadow

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"memoire/services/api/internal/db"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Sibling leg of the 2026-08-27 deadlock fix (its two other legs live in
// package db's pg_migrate_lock_test.go — they cannot live together, since a
// db-package test importing pgshadow would close an import cycle). Apply is
// the SECOND schema applier: it must ride the SAME advisory lock as the
// store's own migrate, or the serialization only covers store-vs-store.
// Holding the lock through db.AcquirePGMigrateLock — rather than a literal
// key — is what makes "same key" the thing under test.
func TestApplyRidesTheSchemaApplierLock(t *testing.T) {
	admin := os.Getenv("MEMOIRE_TEST_PG_URL")
	if admin == "" {
		t.Skip("MEMOIRE_TEST_PG_URL not set; postgres applier-lock leg skipped")
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("MEMOIRE_TEST_PG_URL: %v", err)
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	dbName := "memoire_applylock_" + hex.EncodeToString(suffix[:])
	adminConn, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminConn.Exec("CREATE DATABASE " + dbName); err != nil {
		_ = adminConn.Close()
		t.Fatalf("create fixture database (role needs CREATEDB): %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminConn.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
		_ = adminConn.Close()
	})
	u.Path = "/" + dbName

	ctx := context.Background()
	holder, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	release, err := db.AcquirePGMigrateLock(ctx, holder)
	if err != nil {
		t.Fatalf("hold applier lock: %v", err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	pg, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })

	done := make(chan error, 1)
	go func() { done <- Apply(ctx, pg, "../../../../schema-pg") }()

	select {
	case err := <-done:
		t.Fatalf("Apply completed while the applier lock was held (it does not ride the same key; err=%v)", err)
	case <-time.After(500 * time.Millisecond):
	}

	released = true
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Apply failed after the applier lock was released: %v", err)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("Apply did not complete within 120s of the applier lock being released")
	}
}
