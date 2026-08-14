// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Package forwarder is the client half of remote-capable capture
// (memoire:D147 slice 3, memoire:D153). It drains the LOCAL file queue by
// POSTing each pending envelope to the hub's POST /v1/events and deletes the
// local file only on the hub's queue-accept ack (202 + "queued":true — the
// memoire:D138 echo). The ack means the bytes are durably in the hub's own
// pending queue (fsynced tmp+rename), so deleting the local copy transfers
// custody without a loss window.
//
// Delivery is at-least-once: a crash between the ack and the local delete
// re-forwards the envelope, and hub ingest collapses the duplicate through
// the UNIQUE idempotency_key column (hash of source_adapter:source_event_id;
// envelopes arriving without a source_event_id get a retry-stable
// original-bytes stamp at the hub's HTTP surface — see queuewriter.WriteBytes).
// The local queue remains the buffer — off-LAN or hub-down degrades to
// capture-and-forward-later, never to loss.
//
// Failure taxonomy:
//   - transport errors, 5xx, 401/403, and malformed acks are SYSTEMIC: the
//     item returns to pending/, the cycle stops (preserving order), and the
//     next cycle waits out an exponential backoff. Auth failures are config
//     faults; retrying preserves the data while the operator fixes the token.
//   - other 4xx are PER-ITEM POISON: the hub (or a proxy) rejected this body.
//     The item moves to failed/ with an .error.json sidecar — same shape the
//     drainer writes, so queue_failed_depth, the TTL reaper, and the operator
//     triage story all apply unchanged.
//
// The forwarder must be the ONLY consumer of the queue it drains: running it
// against a MEMOIRE_HOME that a local memoire-api is also draining splits
// custody between the local store and the hub (each file goes to exactly one,
// but the corpus divides). See docs/RUNBOOK.md.
package forwarder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/drainer"
	"memoire/services/api/internal/queuefile"
)

const (
	requestTimeout = 10 * time.Second
	pollInterval   = 2 * time.Second
	backoffBase    = 2 * time.Second
	backoffCap     = 5 * time.Minute
)

// Status is the forwarder's client-visible state, persisted atomically to
// $MEMOIRE_HOME/forward-status.json after every cycle that did work or
// failed. The inject header and `memoire-forward -status` read it fail-open.
type Status struct {
	ForwardURL          string `json:"forward_url"`
	LastSuccessEpoch    int64  `json:"last_success_epoch"`
	LastErrorEpoch      int64  `json:"last_error_epoch,omitempty"`
	LastError           string `json:"last_error,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	ForwardedTotal      int64  `json:"forwarded_total"`
}

type Forwarder struct {
	cfg    config.Config
	client *http.Client
	logger *slog.Logger
	token  string

	status Status
}

// New builds a Forwarder. cfg.ForwardURL must be set; the hub bearer token is
// read once from cfg.ForwardTokenPath.
func New(cfg config.Config, logger *slog.Logger) (*Forwarder, error) {
	if strings.TrimSpace(cfg.ForwardURL) == "" {
		return nil, errors.New("forward URL is required (MEMOIRE_FORWARD_URL, e.g. http://hub:37778/v1)")
	}
	tokenBytes, err := os.ReadFile(cfg.ForwardTokenPath)
	if err != nil {
		return nil, fmt.Errorf("read forward token %s: %w (provision the hub token there)", cfg.ForwardTokenPath, err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return nil, fmt.Errorf("forward token %s is empty", cfg.ForwardTokenPath)
	}
	return &Forwarder{
		cfg:    cfg,
		client: &http.Client{Timeout: requestTimeout},
		logger: logger,
		token:  token,
		status: Status{ForwardURL: cfg.ForwardURL},
	}, nil
}

// Run drives the forward loop until ctx is done: reconcile once, then drain
// on a fixed poll interval, stretching to an exponential backoff while the
// hub is unreachable. The failed-queue TTL reaper runs hourly here too: on a
// client machine the forwarder is the only queue consumer, so poison items
// it sets aside would otherwise outlive the liaison:D97 decision-8 window
// (slice-3 round-1 finding 4).
func (f *Forwarder) Run(ctx context.Context) {
	f.Reconcile(ctx)
	var lastReap time.Time
	for {
		f.ForwardOnce(ctx)
		if f.cfg.FailedTTLDays > 0 && time.Since(lastReap) >= time.Hour {
			drainer.ReapFailedDir(f.logger, f.cfg.Queue.Failed, f.cfg.FailedTTLDays)
			lastReap = time.Now()
		}
		delay := pollInterval
		if f.status.ConsecutiveFailures > 0 {
			delay = backoffDelay(f.status.ConsecutiveFailures)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func backoffDelay(failures int) time.Duration {
	d := backoffBase
	for i := 1; i < failures && d < backoffCap; i++ {
		d *= 2
	}
	if d > backoffCap {
		d = backoffCap
	}
	return d
}

// Reconcile returns crashed inflight items to pending, same discipline as the
// drainer: inflight files belong to a dead process by definition at startup.
func (f *Forwarder) Reconcile(ctx context.Context) {
	entries, err := os.ReadDir(f.cfg.Queue.Inflight)
	if err != nil {
		f.logger.Warn("read inflight queue", "err", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		src := filepath.Join(f.cfg.Queue.Inflight, entry.Name())
		dst := filepath.Join(f.cfg.Queue.Pending, entry.Name())
		if err := os.Rename(src, dst); err != nil {
			f.logger.Warn("requeue inflight item", "file", entry.Name(), "err", err)
		}
	}
}

// ForwardOnce drains the pending queue once, in name order (timestamp
// prefixes make that arrival order). A systemic failure stops the cycle so
// order is preserved; poison items are set aside and the cycle continues.
func (f *Forwarder) ForwardOnce(ctx context.Context) {
	entries, err := os.ReadDir(f.cfg.Queue.Pending)
	if err != nil {
		f.logger.Warn("read pending queue", "err", err)
		return
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		// Shared pick/accounting predicate (review 2026-08-03 round-2 N2).
		if !entry.IsDir() && queuefile.IsItem(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	worked := false
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		worked = true
		if !f.forwardItem(ctx, name) {
			break
		}
	}
	if worked {
		f.writeStatus()
	}
}

// forwardItem pushes one queue file to the hub. Returns false when the cycle
// must stop (systemic failure). The item is claimed by renaming into
// inflight/ first so a crash mid-POST leaves it recoverable by Reconcile.
func (f *Forwarder) forwardItem(ctx context.Context, name string) bool {
	pending := filepath.Join(f.cfg.Queue.Pending, name)
	inflight := filepath.Join(f.cfg.Queue.Inflight, name)
	if err := os.Rename(pending, inflight); err != nil {
		// Lost the claim (another process, or already gone) — not a failure.
		return true
	}
	requeue := func() {
		if err := os.Rename(inflight, pending); err != nil {
			f.logger.Warn("requeue item after failure", "file", name, "err", err)
		}
	}

	body, err := os.ReadFile(inflight)
	if err != nil {
		requeue()
		f.systemicFailure(name, fmt.Errorf("read: %w", err))
		return false
	}

	status, respBody, err := f.post(ctx, body)
	if err != nil {
		requeue()
		f.systemicFailure(name, err)
		return false
	}
	if acked(status, respBody) {
		if err := os.Remove(inflight); err != nil {
			f.logger.Warn("remove forwarded queue file", "file", name, "err", err)
		}
		f.status.ForwardedTotal++
		f.status.LastSuccessEpoch = time.Now().Unix()
		f.status.ConsecutiveFailures = 0
		f.status.LastError = ""
		f.status.LastErrorEpoch = 0
		return true
	}
	if status >= 400 && status < 500 && status != http.StatusUnauthorized && status != http.StatusForbidden {
		// Per-item poison: the hub refused this body. Same failed/ + sidecar
		// shape as the drainer, so depth counters and the TTL reaper apply.
		f.poison(inflight, name, fmt.Sprintf("hub rejected: status=%d body=%.200s", status, respBody))
		return true
	}
	requeue()
	f.systemicFailure(name, fmt.Errorf("hub ack missing: status=%d body=%.200s", status, respBody))
	return false
}

func (f *Forwarder) post(ctx context.Context, body []byte) (int, string, error) {
	url := strings.TrimRight(f.cfg.ForwardURL, "/") + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	respBody := make([]byte, 0, 512)
	buf := make([]byte, 512)
	for len(respBody) < 4096 {
		n, readErr := resp.Body.Read(buf)
		respBody = append(respBody, buf[:n]...)
		if readErr != nil {
			break
		}
	}
	return resp.StatusCode, string(respBody), nil
}

// acked reports whether the hub response is the queue-accept ack: 202 with a
// JSON body carrying "queued": true (memoire:D138 echo). Anything less —
// wrong status, unparseable body, queued absent or false — is NOT custody
// transfer and the local copy must survive.
func acked(status int, body string) bool {
	if status != http.StatusAccepted {
		return false
	}
	var ack struct {
		Queued bool `json:"queued"`
	}
	if err := json.Unmarshal([]byte(body), &ack); err != nil {
		return false
	}
	return ack.Queued
}

func (f *Forwarder) systemicFailure(name string, err error) {
	f.status.ConsecutiveFailures++
	f.status.LastError = err.Error()
	f.status.LastErrorEpoch = time.Now().Unix()
	f.logger.Warn("forward failed; item kept in pending",
		"file", name, "err", err, "consecutive_failures", f.status.ConsecutiveFailures)
}

func (f *Forwarder) poison(path, name, reason string) {
	failed := filepath.Join(f.cfg.Queue.Failed, name)
	if err := os.Rename(path, failed); err != nil {
		f.logger.Warn("move rejected item to failed", "file", name, "reason", reason, "err", err)
		return
	}
	body, _ := json.MarshalIndent(map[string]any{
		"reason":          reason,
		"failed_at_epoch": time.Now().Unix(),
	}, "", "  ")
	body = append(body, '\n')
	_ = os.WriteFile(failed+".error.json", body, 0o600)
	f.logger.Warn("queue item rejected by hub", "file", name, "reason", reason)
}

// StatusPath is where the forwarder persists Status for client-side readers.
func StatusPath(cfg config.Config) string {
	return filepath.Join(cfg.Home, "forward-status.json")
}

func (f *Forwarder) writeStatus() {
	body, err := json.MarshalIndent(f.status, "", "  ")
	if err != nil {
		return
	}
	body = append(body, '\n')
	path := StatusPath(f.cfg)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		f.logger.Warn("write forward status", "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		f.logger.Warn("rename forward status", "err", err)
	}
}

// Depths reports the client-visible queue counters, mirroring the /v1/health
// queue_depth family (memoire:D136): failed depth excludes .error.json
// sidecars so one poison item reports as depth 1.
type Depths struct {
	PendingDepth            int   `json:"pending_depth"`
	InflightDepth           int   `json:"inflight_depth"`
	FailedDepth             int   `json:"failed_depth"`
	OldestPendingAgeSeconds int64 `json:"oldest_pending_age_seconds"`
}

func MeasureDepths(cfg config.Config) Depths {
	return Depths{
		PendingDepth:            countFiles(cfg.Queue.Pending, false),
		InflightDepth:           countFiles(cfg.Queue.Inflight, false),
		FailedDepth:             countFiles(cfg.Queue.Failed, true),
		OldestPendingAgeSeconds: oldestAgeSeconds(cfg.Queue.Pending),
	}
}

func countFiles(dir string, excludeSidecars bool) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, entry := range entries {
		// queuefile.IsItem is the shared pick/accounting predicate: status
		// counts exactly what a consumer would process, so a crash-leaked
		// temp is invisible and a pickable item never is (review
		// 2026-08-03 F3 + round-2 N2).
		if entry.IsDir() || !queuefile.IsItem(entry.Name()) {
			continue
		}
		if excludeSidecars && queuefile.IsErrorSidecar(entry.Name()) {
			continue
		}
		n++
	}
	return n
}

func oldestAgeSeconds(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var oldest time.Time
	for _, entry := range entries {
		if entry.IsDir() || !queuefile.IsItem(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if oldest.IsZero() || info.ModTime().Before(oldest) {
			oldest = info.ModTime()
		}
	}
	if oldest.IsZero() {
		return 0
	}
	return int64(time.Since(oldest).Seconds())
}
