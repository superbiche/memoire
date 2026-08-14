// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type apiClient struct {
	base         string
	tokenFile    string
	tokenFromEnv bool
	// tokenPathErr is a startup-time path-resolution failure (no home dir);
	// carried here so it surfaces per-call instead of killing startup.
	tokenPathErr string

	mu     sync.Mutex
	token  string
	warned bool

	http *http.Client
}

func newAPIClient(base, tokenFile string, tokenFromEnv bool, tokenPathErr string) *apiClient {
	return &apiClient{
		base:         base,
		tokenFile:    tokenFile,
		tokenFromEnv: tokenFromEnv,
		tokenPathErr: tokenPathErr,
		http:         &http.Client{Timeout: 5 * time.Second},
	}
}

// bearer returns the API token, reading tokenFile on first use and retrying
// on every call until a read succeeds — so a token rendered after launch
// (TPM unlock, secret mount) heals without a restart. No caller may touch
// the token file before the JSON-RPC handshake: the first read happens here,
// inside a tool call. Failures name the path tried and how it was chosen, so
// the error a harness shows is the diagnosis; the first failure is also
// logged to stderr so launcher logs carry it.
func (c *apiClient) bearer() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" {
		return c.token, nil
	}
	err := c.loadTokenLocked()
	if err != nil && !c.warned {
		c.warned = true
		log.Printf("memoire-mcp: %v — tools stay served; calls fail until the token is readable", err)
	}
	if err != nil {
		return "", err
	}
	return c.token, nil
}

func (c *apiClient) loadTokenLocked() error {
	if c.tokenPathErr != "" {
		return errors.New(c.tokenPathErr)
	}
	source := "default; set MEMOIRE_TOKEN_FILE to override"
	if c.tokenFromEnv {
		source = "from MEMOIRE_TOKEN_FILE"
	}
	raw, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return fmt.Errorf("cannot read token file %s (%s): %v", c.tokenFile, source, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return fmt.Errorf("empty token in %s (%s)", c.tokenFile, source)
	}
	c.token = token
	return nil
}

// searchHit mirrors the API's search/timeline hit shape (db.SearchHit).
type searchHit struct {
	ID              int64  `json:"id"`
	SessionID       string `json:"session_id,omitempty"`
	Project         string `json:"project"`
	Kind            string `json:"kind"`
	ReviewState     string `json:"review_state"`
	Text    string `json:"text,omitempty"`
	Snippet string `json:"snippet"`
	// CapturedAtEpoch is the legacy alias the server has always served for
	// the observation's occurred_at; OccurredAtEpoch is the honest name it
	// serves alongside it. An older server sends only the alias, so the
	// renderer falls back to it rather than dating every line 1970.
	CapturedAtEpoch int64  `json:"captured_at_epoch"`
	OccurredAtEpoch int64  `json:"occurred_at_epoch"`
	Provenance      string `json:"provenance,omitempty"`
	CanInstruct     int    `json:"can_instruct"`
}

type hitsResponse struct {
	Hits []searchHit `json:"hits"`
	// IdentityAlerts are liaison:D239 identity-conflict lines the server attaches to
	// project-filtered reads; tools surface them ahead of their results.
	IdentityAlerts []string `json:"identity_alerts,omitempty"`
}

func (c *apiClient) get(path string, params url.Values, out any) error {
	u := c.base + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *apiClient) post(path string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

// httpStatusError carries the API status code so handlers can classify a
// rejected-bearer response (401/403) apart from ordinary per-item failures.
type httpStatusError struct {
	status int
	msg    string
}

func (e *httpStatusError) Error() string { return e.msg }

// isAuthRejection reports whether err is the API refusing the bearer token —
// an auth failure that dooms every request, never a per-item condition.
func isAuthRejection(err error) bool {
	var he *httpStatusError
	return errors.As(err, &he) &&
		(he.status == http.StatusUnauthorized || he.status == http.StatusForbidden)
}

func (c *apiClient) do(req *http.Request, out any) error {
	token, err := c.bearer()
	if err != nil {
		return fmt.Errorf("memoire auth: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// The status is known before the body: classify non-200 responses even
	// when reading their body fails (truncated 401s must stay typed auth
	// rejections, not degrade to an untyped read error — review round 3).
	data, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		body := truncate(string(data), 200)
		if readErr != nil {
			body = fmt.Sprintf("%s (body read: %v)", body, readErr)
		}
		return &httpStatusError{
			status: resp.StatusCode,
			msg:    fmt.Sprintf("memoire API %s: HTTP %d: %s", req.URL.Path, resp.StatusCode, body),
		}
	}
	if readErr != nil {
		return readErr
	}
	return json.Unmarshal(data, out)
}

// compactLine renders one hit as:
//
//	[id] kind~ YYYY-MM-DD project provenance/review_state[!]: snippet
//
// `~` after kind marks unreviewed (proposed or lapsed — memoire:D146 R-6);
// `!` after review_state marks can_instruct=1 (an elevated operating note).
// Provenance renders as `-` when the server sends none, so the field count
// is stable and a consumer can split on it.
//
// This shape deliberately DIVERGES from the injection block's line, which is
// unchanged: an MCP caller reads results without a per-id fetch round and
// needs the governance value inline, where the injection block spends its
// budget on body text.
func compactLine(h searchHit) string {
	marker := ""
	if h.ReviewState == "proposed" || h.ReviewState == "lapsed" {
		marker = "~"
	}
	elevated := ""
	if h.CanInstruct == 1 {
		elevated = "!"
	}
	provenance := h.Provenance
	if provenance == "" {
		provenance = "-"
	}
	body := h.Snippet
	if body == "" {
		body = h.Text
	}
	body = strings.Join(strings.Fields(body), " ")
	body = truncate(body, 240)
	// occurred_at_epoch is the honest field; captured_at_epoch is its legacy
	// alias and the only one an older server sends.
	when := h.OccurredAtEpoch
	if when == 0 {
		when = h.CapturedAtEpoch
	}
	day := time.Unix(when, 0).UTC().Format("2006-01-02")
	return fmt.Sprintf("[%d] %s%s %s %s %s/%s%s: %s",
		h.ID, h.Kind, marker, day, h.Project, provenance, h.ReviewState, elevated, body)
}

// withIdentityAlerts prepends identity-conflict lines (loud, operator-facing)
// to a tool result body when the server attached any.
func withIdentityAlerts(alerts []string, body string) string {
	if len(alerts) == 0 {
		return body
	}
	var b strings.Builder
	b.WriteString("IDENTITY ALERT — surface this to the operator:\n")
	for _, a := range alerts {
		b.WriteString("! ")
		b.WriteString(a)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(body)
	return b.String()
}

func compactLines(hits []searchHit) string {
	if len(hits) == 0 {
		return "no observations matched"
	}
	lines := make([]string, 0, len(hits))
	for _, h := range hits {
		lines = append(lines, compactLine(h))
	}
	return strings.Join(lines, "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func intParam(params url.Values, key string, value int) {
	if value > 0 {
		params.Set(key, strconv.Itoa(value))
	}
}
