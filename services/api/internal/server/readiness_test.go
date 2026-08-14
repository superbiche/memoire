// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"memoire/services/api/internal/buildinfo"
	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
)

func newReadinessEnv(t *testing.T) (config.Config, *db.Store) {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{
		Addr:      "127.0.0.1:0",
		Home:      home,
		DBPath:    filepath.Join(home, "memoire.db"),
		SchemaDir: "../../../../schema",
		TokenPath: filepath.Join(home, "token"),
		Raw:       filepath.Join(home, "raw"),
		Queue: config.QueuePaths{
			Root:     filepath.Join(home, "queue"),
			Pending:  filepath.Join(home, "queue", "pending"),
			Inflight: filepath.Join(home, "queue", "inflight"),
			Failed:   filepath.Join(home, "queue", "failed"),
			Log:      filepath.Join(home, "log"),
		},
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(cfg.DBPath, cfg.SchemaDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return cfg, store
}

type readinessBody struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func getReadiness(t *testing.T, handler http.Handler) (int, readinessBody, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/readiness", nil))
	var body readinessBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("readiness body is not JSON: %v\nbody: %s", err, rec.Body.String())
	}
	return rec.Code, body, rec.Body.String()
}

// /v1/readiness is a real probe (MISSING #8): 200 only when the store
// answers a round-trip AND the pending queue directory takes a write; 503
// with a per-check body otherwise. The endpoint is auth-exempt, so the body
// must stay closed-vocabulary (D224 invariant): check values are "ok" or
// "failed", never an error string that could carry a DSN or a path.
func TestReadinessProbesDependencies(t *testing.T) {
	cfg, store := newReadinessEnv(t)
	handler := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))

	code, body, _ := getReadiness(t, handler)
	if code != http.StatusOK || body.Status != "ready" {
		t.Fatalf("healthy readiness = %d %+v", code, body)
	}
	if body.Checks["store"] != "ok" || body.Checks["queue"] != "ok" {
		t.Fatalf("healthy checks = %+v", body.Checks)
	}
	// The queue probe must leave zero residue: a leaked probe file in
	// pending/ would sit next to real capture items forever.
	entries, err := os.ReadDir(cfg.Queue.Pending)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("queue probe left residue in pending/: %v", entries)
	}

	// Unwritable pending dir → 503, queue failed, store still reported
	// independently — and the raw body never leaks the directory path.
	if err := os.Chmod(cfg.Queue.Pending, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg.Queue.Pending, 0o700) })
	code, body, raw := getReadiness(t, handler)
	if code != http.StatusServiceUnavailable || body.Status != "not_ready" {
		t.Fatalf("queue-broken readiness = %d %+v", code, body)
	}
	if body.Checks["queue"] != "failed" || body.Checks["store"] != "ok" {
		t.Fatalf("queue-broken checks = %+v", body.Checks)
	}
	if strings.Contains(raw, cfg.Queue.Pending) {
		t.Fatalf("readiness body leaks the queue path: %s", raw)
	}
	if err := os.Chmod(cfg.Queue.Pending, 0o700); err != nil {
		t.Fatal(err)
	}

	// Closed store → 503, store failed, queue probed independently — and
	// the driver's error text never reaches the unauthenticated body.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	code, body, raw = getReadiness(t, handler)
	if code != http.StatusServiceUnavailable || body.Status != "not_ready" {
		t.Fatalf("store-broken readiness = %d %+v", code, body)
	}
	if body.Checks["store"] != "failed" || body.Checks["queue"] != "ok" {
		t.Fatalf("store-broken checks = %+v", body.Checks)
	}
	if strings.Contains(raw, "closed") || strings.Contains(raw, "sql") {
		t.Fatalf("readiness body leaks driver error text: %s", raw)
	}
	for _, v := range body.Checks {
		if v != "ok" && v != "failed" {
			t.Fatalf("check value outside the closed vocabulary: %+v", body.Checks)
		}
	}
}

// /v1/health reports the build-stamped version (MISSING #7): the handler
// must echo buildinfo.Version — never a literal. Unstamped builds (this
// test binary) report "dev"; the stamped-binary-equals-VERSION leg lives in
// tests/e2e/standalone-sqlite-smoke.sh, which builds with the ldflags stamp
// and compares against the VERSION file.
func TestHealthVersionIsBuildStamped(t *testing.T) {
	cfg, store := newReadinessEnv(t)
	handler := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))

	getVersion := func() string {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("health status = %d", rec.Code)
		}
		var body struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Version
	}

	if v := getVersion(); v != "dev" {
		t.Fatalf("unstamped health version = %q, want \"dev\" (a literal semver here means the hardcoded lie is back)", v)
	}
	old := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = old })
	buildinfo.Version = "9.9.9-stamp-test"
	if v := getVersion(); v != "9.9.9-stamp-test" {
		t.Fatalf("stamped health version = %q, want the buildinfo value", v)
	}
	// A stamp is a build input, not a trusted value: anything outside the
	// closed version-token shape must collapse to "dev" before it reaches
	// the unauthenticated body (D224; review F1).
	buildinfo.Version = "postgres://probe.invalid/db?token=review-secret"
	if v := getVersion(); v != "dev" {
		t.Fatalf("hostile stamp reached the wire: version = %q", v)
	}
}
