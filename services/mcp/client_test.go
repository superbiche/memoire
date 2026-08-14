// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// An unreadable token must not prevent the client from being constructed
// (the server serves initialize/tools/list regardless); each call fails
// with an error naming the path tried and how it was chosen, and the
// client heals without restart once the token becomes readable.
func TestBearerLazyLoadAndSelfHeal(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sekrit" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"hits":[]}`))
	}))
	defer srv.Close()

	c := newAPIClient(srv.URL, tokenFile, false, "")

	var out hitsResponse
	err := c.get("/v1/search", nil, &out)
	if err == nil {
		t.Fatal("expected auth error while token file is missing")
	}
	// Exact source label: the default path must NOT be labeled as coming
	// from MEMOIRE_TOKEN_FILE.
	for _, want := range []string{"memoire auth:", tokenFile, "(default; set MEMOIRE_TOKEN_FILE to override)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "(from MEMOIRE_TOKEN_FILE)") {
		t.Errorf("default-path error %q mislabeled as env-sourced", err)
	}

	// Token appears after launch (TPM unlock, secret mount): next call works.
	if err := os.WriteFile(tokenFile, []byte("sekrit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.get("/v1/search", nil, &out); err != nil {
		t.Fatalf("expected self-heal after token became readable, got: %v", err)
	}
}

func TestBearerEmptyTokenNamesEnvSource(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := newAPIClient("http://127.0.0.1:0", tokenFile, true, "")
	_, err := c.bearer()
	if err == nil {
		t.Fatal("expected error for empty token")
	}
	for _, want := range []string{"empty token", tokenFile, "(from MEMOIRE_TOKEN_FILE)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// A startup-time token-path resolution failure (no home dir) surfaces
// per-call, never as a startup death.
func TestBearerTokenPathError(t *testing.T) {
	c := newAPIClient("http://127.0.0.1:0", "", false, "cannot resolve home dir for the default token path: boom (set MEMOIRE_TOKEN_FILE or MEMOIRE_HOME)")
	_, err := c.bearer()
	if err == nil || !strings.Contains(err.Error(), "MEMOIRE_TOKEN_FILE or MEMOIRE_HOME") {
		t.Fatalf("expected path-resolution error naming the overrides, got: %v", err)
	}
}

// mcp-go dispatches stdio tool calls concurrently; bearer() must be safe
// under concurrent first-load (run with -race in CI-of-one).
func TestBearerConcurrentFirstLoad(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("sekrit"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newAPIClient("http://127.0.0.1:0", tokenFile, false, "")

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := c.bearer()
			if err != nil || tok != "sekrit" {
				t.Errorf("bearer under concurrency: tok=%q err=%v", tok, err)
			}
		}()
	}
	wg.Wait()
}

// Round-2 residual (review finding 2): a READABLE token the API rejects
// (401/403) must fail the get_observations CALL with isError, exactly like
// the local bearer-load pre-flight — never aggregate into per-id text.
func TestGetObservationsAPIAuthRejectionIsError(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("readable-but-rejected"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad token", http.StatusUnauthorized)
	}))
	defer srv.Close()

	h := handleGetObservations(newAPIClient(srv.URL, tokenFile, false, ""))
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"ids": []any{float64(1), float64(2)}}
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("expected isError result for API-rejected bearer, got %+v", res)
	}
	text := resultText(t, res)
	for _, want := range []string{"memoire auth:", "HTTP 401"} {
		if !strings.Contains(text, want) {
			t.Errorf("result %q missing %q", text, want)
		}
	}

	// A 401 whose body cannot be read completely (Content-Length larger than
	// the bytes sent) must STAY a typed auth rejection — the status is known
	// before the body (round-3 residual).
	srvTrunc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("bad"))
	}))
	defer srvTrunc.Close()
	hTrunc := handleGetObservations(newAPIClient(srvTrunc.URL, tokenFile, false, ""))
	resTrunc, err := hTrunc(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !resTrunc.IsError {
		t.Fatalf("truncated-body 401 must remain a typed auth rejection (isError), got %+v", resTrunc)
	}
	if !strings.Contains(resultText(t, resTrunc), "HTTP 401") {
		t.Errorf("truncated-body 401 result %q missing status", resultText(t, resTrunc))
	}

	// Per-id failures with a WORKING token stay inline: a 404 on one id must
	// not fail the call.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such observation", http.StatusNotFound)
	}))
	defer srv2.Close()
	h2 := handleGetObservations(newAPIClient(srv2.URL, tokenFile, false, ""))
	res2, err := h2(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res2.IsError {
		t.Fatalf("per-id 404 must not fail the call, got isError: %+v", res2)
	}
	if !strings.Contains(resultText(t, res2), "ERROR:") {
		t.Errorf("per-id failure should surface as inline ERROR lines")
	}
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// compactLine is the only surface an MCP caller reads before deciding
// whether to spend a memoire_get_observations round, so its governance
// fields — provenance, review_state, the can_instruct `!` — must render
// from the hit itself, and the date must survive an older server that
// serves only the legacy captured_at_epoch alias.
func TestCompactLineRendersGovernanceFields(t *testing.T) {
	const day = 1785312000 // 2026-07-29 UTC
	cases := []struct {
		name string
		hit  searchHit
		want string
	}{
		{
			name: "unreviewed inferred hit",
			hit: searchHit{
				ID: 101, Project: "memoire", Kind: "fact", ReviewState: "proposed",
				Provenance: "inferred", Snippet: "derived and unreviewed",
				OccurredAtEpoch: day, CapturedAtEpoch: day,
			},
			want: "[101] fact~ 2026-07-29 memoire inferred/proposed: derived and unreviewed",
		},
		{
			name: "elevated operating note",
			hit: searchHit{
				ID: 102, Project: "memoire", Kind: "constraint", ReviewState: "accepted",
				Provenance: "user_confirmed", CanInstruct: 1, Snippet: "always do the thing",
				OccurredAtEpoch: day, CapturedAtEpoch: day,
			},
			want: "[102] constraint 2026-07-29 memoire user_confirmed/accepted!: always do the thing",
		},
		{
			name: "missing provenance keeps the field count stable",
			hit: searchHit{
				ID: 103, Project: "memoire", Kind: "decision", ReviewState: "accepted",
				Snippet: "no provenance served", OccurredAtEpoch: day, CapturedAtEpoch: day,
			},
			want: "[103] decision 2026-07-29 memoire -/accepted: no provenance served",
		},
		{
			name: "older server sends only the legacy alias",
			hit: searchHit{
				ID: 104, Project: "memoire", Kind: "fact", ReviewState: "accepted",
				Snippet: "legacy date", CapturedAtEpoch: day,
			},
			want: "[104] fact 2026-07-29 memoire -/accepted: legacy date",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := compactLine(tc.hit); got != tc.want {
				t.Fatalf("compactLine:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}
