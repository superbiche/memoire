// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// L0010 R1 = A: the MCP read tools are a dated recent raw lookup. They never
// ask for derived observations, send no exclude_kind=raw, bound reads to the
// last `days` days by occurred time (default 30; 0 lifts the bound), and
// their declarations send memory questions to Hindsight.

type recordedCall struct {
	query url.Values
	body  map[string]any
}

func recordingServer(t *testing.T, reply string) (*apiClient, *[]recordedCall) {
	t.Helper()
	calls := &[]recordedCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := recordedCall{query: r.URL.Query()}
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &rc.body)
			}
		}
		*calls = append(*calls, rc)
		w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	return newAPIClient(srv.URL, tokenFile, false, ""), calls
}

func callTool(t *testing.T, h toolHandler, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool error: %s", resultText(t, res))
	}
	return res
}

func assertRecentBound(t *testing.T, got string, days int) {
	t.Helper()
	epoch, err := strconv.ParseInt(got, 10, 64)
	if err != nil {
		t.Fatalf("occurred_after = %q, want an epoch", got)
	}
	want := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	if d := epoch - want; d < -120 || d > 120 {
		t.Fatalf("occurred_after = %d, want about now-%dd (%d)", epoch, days, want)
	}
}

func TestQueryToolsSendRawRecentReads(t *testing.T) {
	c, calls := recordingServer(t, `{"hits":[]}`)

	callTool(t, handleSearch(c), map[string]any{"q": "needle"})
	callTool(t, handleTimeline(c), map[string]any{})
	for i, rc := range *calls {
		for _, banned := range []string{"exclude_kind", "include_derived", "include_proposed"} {
			if rc.query.Has(banned) {
				t.Errorf("call %d sends %s=%q", i, banned, rc.query.Get(banned))
			}
		}
		assertRecentBound(t, rc.query.Get("occurred_after"), 30)
	}

	*calls = nil
	callTool(t, handleSearch(c), map[string]any{"q": "needle", "days": float64(7)})
	assertRecentBound(t, (*calls)[0].query.Get("occurred_after"), 7)

	*calls = nil
	callTool(t, handleSearch(c), map[string]any{"q": "needle", "days": float64(-5)})
	assertRecentBound(t, (*calls)[0].query.Get("occurred_after"), 30)

	*calls = nil
	callTool(t, handleSearch(c), map[string]any{"q": "needle", "days": float64(0)})
	if (*calls)[0].query.Has("occurred_after") {
		t.Errorf("days=0 must lift the bound, got occurred_after=%q", (*calls)[0].query.Get("occurred_after"))
	}
}

func TestContextToolSendsRawRecentRead(t *testing.T) {
	c, calls := recordingServer(t, `{"context":"[1] p raw: x"}`)
	callTool(t, handleContext(c), map[string]any{"q": "needle"})
	body := (*calls)[0].body
	for _, banned := range []string{"exclude_kind", "include_derived", "include_proposed"} {
		if _, ok := body[banned]; ok {
			t.Errorf("context body sends %s=%v", banned, body[banned])
		}
	}
	after, ok := body["occurred_after"].(float64)
	if !ok {
		t.Fatalf("context body lacks numeric occurred_after: %v", body)
	}
	assertRecentBound(t, strconv.FormatInt(int64(after), 10), 30)
}

func TestToolDeclarationsPointToHindsight(t *testing.T) {
	s := server.NewMCPServer("memoire-test", "0")
	c, _ := recordingServer(t, `{}`)
	registerTools(s, c)
	tools := servedTools(t)
	if len(tools) == 0 {
		t.Fatal("no tools served")
	}
	listed := s.ListTools()
	for _, name := range []string{"memoire_search", "memoire_timeline", "memoire_context", "memoire_get_observations"} {
		tool, ok := listed[name]
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		d := tool.Tool.Description
		if !strings.Contains(d, "Hindsight") {
			t.Errorf("%s description does not point to Hindsight: %q", name, d)
		}
		if strings.Contains(d, "shared memory of past sessions") {
			t.Errorf("%s description still claims Memoire is the shared memory", name)
		}
	}
	for _, name := range []string{"memoire_search", "memoire_timeline"} {
		for _, p := range []string{"exclude_kind", "include_proposed", "kind"} {
			if _, ok := listed[name].Tool.InputSchema.Properties[p]; ok {
				t.Errorf("%s still declares parameter %q", name, p)
			}
		}
		if _, ok := listed[name].Tool.InputSchema.Properties["days"]; !ok {
			t.Errorf("%s lacks the days parameter", name)
		}
	}
}
