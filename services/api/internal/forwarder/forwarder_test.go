// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package forwarder

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"memoire/services/api/internal/config"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{
		Home:             home,
		DBPath:           filepath.Join(home, "memoire.db"),
		TokenPath:        filepath.Join(home, "token"),
		Raw:              filepath.Join(home, "raw"),
		ForwardTokenPath: filepath.Join(home, "forward-token"),
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
	if err := os.WriteFile(cfg.ForwardTokenPath, []byte("hub-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newTestForwarder(t *testing.T, cfg config.Config) *Forwarder {
	t.Helper()
	f, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func writePending(t *testing.T, cfg config.Config, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cfg.Queue.Pending, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func ackHub(t *testing.T, record *[][]byte, auths *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/events" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if record != nil {
			*record = append(*record, b)
		}
		if auths != nil {
			*auths = append(*auths, r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"queued": true, "queue_id": "hub-file.json"})
	}))
}

func TestForwardDeletesLocalOnlyOnAck(t *testing.T) {
	cfg := testConfig(t)
	var bodies [][]byte
	var auths []string
	hub := ackHub(t, &bodies, &auths)
	defer hub.Close()
	cfg.ForwardURL = hub.URL + "/v1"

	writePending(t, cfg, "001-a.json", `{"schema_version":1,"project":"p","payload":{}}`)
	writePending(t, cfg, "002-b.json", `{"schema_version":1,"project":"p","payload":{"n":2}}`)

	f := newTestForwarder(t, cfg)
	f.ForwardOnce(context.Background())

	if len(bodies) != 2 {
		t.Fatalf("expected 2 forwarded envelopes, got %d", len(bodies))
	}
	if string(bodies[0]) != `{"schema_version":1,"project":"p","payload":{}}` {
		t.Fatalf("first body altered in flight: %s", bodies[0])
	}
	for _, a := range auths {
		if a != "Bearer hub-token" {
			t.Fatalf("expected hub bearer token, got %q", a)
		}
	}
	for _, dir := range []string{cfg.Queue.Pending, cfg.Queue.Inflight, cfg.Queue.Failed} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected %s empty after acked forward, found %d entries", dir, len(entries))
		}
	}

	var st Status
	b, err := os.ReadFile(StatusPath(cfg))
	if err != nil {
		t.Fatalf("expected status file: %v", err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st.ForwardedTotal != 2 || st.ConsecutiveFailures != 0 || st.LastSuccessEpoch == 0 {
		t.Fatalf("unexpected status after success: %+v", st)
	}
}

func TestSystemicFailureKeepsItemPendingAndStopsCycle(t *testing.T) {
	cfg := testConfig(t)
	calls := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer hub.Close()
	cfg.ForwardURL = hub.URL + "/v1"

	writePending(t, cfg, "001-a.json", `{"a":1}`)
	writePending(t, cfg, "002-b.json", `{"b":2}`)

	f := newTestForwarder(t, cfg)
	f.ForwardOnce(context.Background())

	if calls != 1 {
		t.Fatalf("systemic failure must stop the cycle after the first item, hub saw %d calls", calls)
	}
	entries, err := os.ReadDir(cfg.Queue.Pending)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected both items back in pending, found %d", len(entries))
	}
	if f.status.ConsecutiveFailures != 1 || f.status.LastError == "" {
		t.Fatalf("expected failure recorded, got %+v", f.status)
	}
}

func TestUnreachableHubKeepsItemPending(t *testing.T) {
	cfg := testConfig(t)
	// Reserved-but-closed port: connection refused.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closed.Close()
	cfg.ForwardURL = closed.URL + "/v1"

	writePending(t, cfg, "001-a.json", `{"a":1}`)
	f := newTestForwarder(t, cfg)
	f.ForwardOnce(context.Background())

	entries, err := os.ReadDir(cfg.Queue.Pending)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("off-LAN item must stay pending (capture-and-forward-later), found %d entries", len(entries))
	}
	if f.status.ConsecutiveFailures != 1 {
		t.Fatalf("expected 1 consecutive failure, got %+v", f.status)
	}
}

func TestAuthFailureIsSystemicNotPoison(t *testing.T) {
	cfg := testConfig(t)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer hub.Close()
	cfg.ForwardURL = hub.URL + "/v1"

	writePending(t, cfg, "001-a.json", `{"a":1}`)
	f := newTestForwarder(t, cfg)
	f.ForwardOnce(context.Background())

	if n := len(mustDir(t, cfg.Queue.Pending)); n != 1 {
		t.Fatalf("401 must keep the item pending (config fault, data is good), pending=%d", n)
	}
	if n := len(mustDir(t, cfg.Queue.Failed)); n != 0 {
		t.Fatalf("401 must not poison items, failed=%d", n)
	}
}

func TestHubRejectionMovesItemToFailedWithSidecar(t *testing.T) {
	cfg := testConfig(t)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	defer hub.Close()
	cfg.ForwardURL = hub.URL + "/v1"

	writePending(t, cfg, "001-poison.json", `{"a":1}`)
	writePending(t, cfg, "002-next.json", `{"b":2}`)

	f := newTestForwarder(t, cfg)
	f.ForwardOnce(context.Background())

	if _, err := os.Stat(filepath.Join(cfg.Queue.Failed, "001-poison.json")); err != nil {
		t.Fatalf("expected rejected item in failed/: %v", err)
	}
	sidecar, err := os.ReadFile(filepath.Join(cfg.Queue.Failed, "001-poison.json.error.json"))
	if err != nil {
		t.Fatalf("expected error sidecar: %v", err)
	}
	if len(sidecar) == 0 {
		t.Fatal("expected non-empty sidecar")
	}
	// Poison is per-item: the cycle continues, so 002 was attempted (and also
	// rejected here) rather than left behind a wedged head-of-line.
	if _, err := os.Stat(filepath.Join(cfg.Queue.Failed, "002-next.json")); err != nil {
		t.Fatalf("expected cycle to continue past poison item: %v", err)
	}
	if n := len(mustDir(t, cfg.Queue.Pending)); n != 0 {
		t.Fatalf("expected pending drained, found %d", n)
	}
}

func TestAckWithoutQueuedTrueIsNotCustodyTransfer(t *testing.T) {
	cfg := testConfig(t)
	responses := []string{`{"queued":false}`, `not json`, ``}
	i := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(responses[i%len(responses)]))
		i++
	}))
	defer hub.Close()
	cfg.ForwardURL = hub.URL + "/v1"

	for range responses {
		writePending(t, cfg, "001-a.json", `{"a":1}`)
		f := newTestForwarder(t, cfg)
		f.ForwardOnce(context.Background())
		if n := len(mustDir(t, cfg.Queue.Pending)); n != 1 {
			t.Fatalf("202 without queued:true must keep the local copy, pending=%d (response %d)", n, i)
		}
		if err := os.Remove(filepath.Join(cfg.Queue.Pending, "001-a.json")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReconcileReturnsInflightToPending(t *testing.T) {
	cfg := testConfig(t)
	var bodies [][]byte
	hub := ackHub(t, &bodies, nil)
	defer hub.Close()
	cfg.ForwardURL = hub.URL + "/v1"

	if err := os.WriteFile(filepath.Join(cfg.Queue.Inflight, "001-crashed.json"), []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newTestForwarder(t, cfg)
	f.Reconcile(context.Background())
	if _, err := os.Stat(filepath.Join(cfg.Queue.Pending, "001-crashed.json")); err != nil {
		t.Fatalf("expected crashed inflight item requeued: %v", err)
	}
	f.ForwardOnce(context.Background())
	if len(bodies) != 1 {
		t.Fatalf("expected requeued item forwarded, hub saw %d", len(bodies))
	}
}

func TestNewRequiresURLAndToken(t *testing.T) {
	cfg := testConfig(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, err := New(cfg, logger); err == nil {
		t.Fatal("expected error without forward URL")
	}
	cfg.ForwardURL = "http://127.0.0.1:1/v1"
	if err := os.Remove(cfg.ForwardTokenPath); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, logger); err == nil {
		t.Fatal("expected error without forward token")
	}
	if err := os.WriteFile(cfg.ForwardTokenPath, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, logger); err == nil {
		t.Fatal("expected error for empty forward token")
	}
}

func TestMeasureDepthsExcludesSidecars(t *testing.T) {
	cfg := testConfig(t)
	writePending(t, cfg, "001-a.json", `{}`)
	writePending(t, cfg, "002-b.json", `{}`)
	for _, name := range []string{"bad.json", "bad.json.error.json"} {
		if err := os.WriteFile(filepath.Join(cfg.Queue.Failed, name), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d := MeasureDepths(cfg)
	if d.PendingDepth != 2 || d.FailedDepth != 1 || d.InflightDepth != 0 {
		t.Fatalf("unexpected depths: %+v", d)
	}
}

func TestBackoffDelayGrowsAndCaps(t *testing.T) {
	if backoffDelay(1) != backoffBase {
		t.Fatalf("first backoff must be the base, got %v", backoffDelay(1))
	}
	if backoffDelay(2) != 2*backoffBase {
		t.Fatalf("second backoff must double, got %v", backoffDelay(2))
	}
	if backoffDelay(100) != backoffCap {
		t.Fatalf("backoff must cap at %v, got %v", backoffCap, backoffDelay(100))
	}
}

func mustDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// Status counts share the consumer predicate (queuefile.IsItem): a
// crash-leaked temp is not backlog, a pickable dot-prefixed .json is
// (review 2026-08-03 F3 + round-2 N2). The age assertion mirrors the
// handler regression's shape (round-3 N3): non-items carry an OLDER mtime
// than the real items, so the reported age proves the helper filters on
// the shared predicate rather than merely not crashing.
func TestStatusCountsMatchConsumerPredicate(t *testing.T) {
	dir := t.TempDir()
	stamp := func(name string, age time.Duration) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
	stamp("1000-item.json", 10*time.Minute)
	stamp(".hand-written.json", 10*time.Minute)
	stamp(".readiness-probe-crash.tmp", 24*time.Hour)
	stamp(".1000-writer.json.tmp", 24*time.Hour)
	stamp("README", 24*time.Hour)

	if got := countFiles(dir, false); got != 2 {
		t.Fatalf("countFiles = %d, want 2 (consumer-pickable items only)", got)
	}
	age := oldestAgeSeconds(dir)
	if age < 9*60 || age > 15*60 {
		t.Fatalf("oldestAgeSeconds = %d, want the real items' ~10m range — keyed on a non-item entry", age)
	}
}
