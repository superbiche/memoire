// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func fixtureHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MEMOIRE_HOME", home)
	t.Setenv("MEMOIRE_FORWARD_URL", "")
	t.Setenv("MEMOIRE_FORWARD_TOKEN_FILE", "")
	return home
}

func TestForwardRefusedWithoutURL(t *testing.T) {
	fixtureHome(t)
	var out bytes.Buffer
	err := run(context.Background(), []string{"-once"}, &out, discard())
	if err == nil || !strings.Contains(err.Error(), "MEMOIRE_FORWARD_URL") {
		t.Fatalf("want forward-URL-required error, got: %v", err)
	}
}

func TestStatusWorksWithoutURLAndReportsDepths(t *testing.T) {
	home := fixtureHome(t)
	if err := os.MkdirAll(filepath.Join(home, "queue", "pending"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "queue", "pending", "001-a.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-status"}, &out, discard()); err != nil {
		t.Fatalf("status must not require a forward URL: %v", err)
	}
	var body struct {
		Depths struct {
			PendingDepth int `json:"pending_depth"`
		} `json:"depths"`
	}
	if err := json.Unmarshal(out.Bytes(), &body); err != nil {
		t.Fatalf("status output not JSON: %v\n%s", err, out.String())
	}
	if body.Depths.PendingDepth != 1 {
		t.Fatalf("pending_depth = %d, want 1 (output: %s)", body.Depths.PendingDepth, out.String())
	}
}

func TestOnceForwardsToHub(t *testing.T) {
	home := fixtureHome(t)
	var got [][]byte
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, b)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"queued": true})
	}))
	defer hub.Close()
	t.Setenv("MEMOIRE_FORWARD_URL", hub.URL+"/v1")
	tokenPath := filepath.Join(home, "forward-token")
	if err := os.WriteFile(tokenPath, []byte("hub-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "queue", "pending"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "queue", "pending", "001-a.json"), []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := run(context.Background(), []string{"-once"}, &out, discard()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got[0]) != `{"a":1}` {
		t.Fatalf("hub saw %d bodies: %v", len(got), got)
	}
	entries, err := os.ReadDir(filepath.Join(home, "queue", "pending"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected pending drained after -once, found %d entries", len(entries))
	}
}
