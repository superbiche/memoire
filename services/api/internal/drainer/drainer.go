// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package drainer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
	"memoire/services/api/internal/queuefile"
)

type Drainer struct {
	cfg    config.Config
	store  *db.Store
	logger *slog.Logger
}

func New(cfg config.Config, store *db.Store, logger *slog.Logger) Drainer {
	return Drainer{cfg: cfg, store: store, logger: logger}
}

func (d Drainer) Run(ctx context.Context) {
	d.reconcile(ctx)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var lastReap time.Time
	for {
		d.drain(ctx)
		if d.cfg.FailedTTLDays > 0 && time.Since(lastReap) >= time.Hour {
			d.ReapFailed(ctx)
			lastReap = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ReapFailed deletes queue/failed/ items older than the configured TTL
// (liaison:D97 decision 8: 30-day window to triage, then gone). Age is
// mtime-based on the BASE item: the failure move stamps it, and operator
// touch(1) on the item under investigation extends its life — item and
// .error.json sidecar live and die as a unit keyed on the item's mtime.
// Orphan sidecars (base item gone) are reaped on their own mtime.
func (d Drainer) ReapFailed(ctx context.Context) {
	items, sidecars := ReapFailedDir(d.logger, d.cfg.Queue.Failed, d.cfg.FailedTTLDays)
	if items > 0 || sidecars > 0 {
		_ = d.store.Audit(ctx, "queue.failed_reap", "queue", "failed", map[string]any{
			"items": items, "sidecars": sidecars, "ttl_days": d.cfg.FailedTTLDays,
		})
	}
}

// ReapFailedDir is the store-free reap core, exported so the forwarder can
// run the same TTL on a client machine where no drainer (and no SQLite
// audit surface) exists (slice-3 round-1 finding 4). Reaps are logged either
// way; only the hub topology additionally audits them.
func ReapFailedDir(logger *slog.Logger, failedDir string, ttlDays int) (int, int) {
	if ttlDays <= 0 {
		return 0, 0
	}
	cutoff := time.Now().Add(-time.Duration(ttlDays) * 24 * time.Hour)
	entries, err := os.ReadDir(failedDir)
	if err != nil {
		logger.Warn("read failed queue for reap", "err", err)
		return 0, 0
	}
	baseItems := map[string]os.DirEntry{}
	var sidecarEntries []os.DirEntry
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if queuefile.IsErrorSidecar(entry.Name()) {
			sidecarEntries = append(sidecarEntries, entry)
		} else {
			baseItems[entry.Name()] = entry
		}
	}
	expired := func(entry os.DirEntry) bool {
		info, err := entry.Info()
		return err == nil && info.ModTime().Before(cutoff)
	}
	remove := func(name string) bool {
		if err := os.Remove(filepath.Join(failedDir, name)); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logger.Warn("reap failed queue item", "file", name, "err", err)
			}
			return false
		}
		return true
	}
	items, sidecars := 0, 0
	for name, entry := range baseItems {
		if !expired(entry) {
			continue
		}
		if remove(name) {
			items++
			if remove(name + ".error.json") {
				sidecars++
			}
		}
	}
	for _, entry := range sidecarEntries {
		base := strings.TrimSuffix(entry.Name(), ".error.json")
		if _, hasBase := baseItems[base]; hasBase {
			continue // unit rule above owns it, expired or not
		}
		if expired(entry) && remove(entry.Name()) {
			sidecars++
		}
	}
	if items > 0 || sidecars > 0 {
		logger.Info("reaped expired failed-queue items", "items", items, "sidecars", sidecars, "ttl_days", ttlDays)
	}
	return items, sidecars
}

func (d Drainer) RunOnce(ctx context.Context) {
	d.drain(ctx)
}

func (d Drainer) reconcile(ctx context.Context) {
	entries, err := os.ReadDir(d.cfg.Queue.Inflight)
	if err != nil {
		d.logger.Warn("read inflight queue", "err", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		src := filepath.Join(d.cfg.Queue.Inflight, entry.Name())
		dst := filepath.Join(d.cfg.Queue.Pending, entry.Name())
		if err := os.Rename(src, dst); err != nil {
			d.logger.Warn("requeue inflight item", "file", entry.Name(), "err", err)
		}
	}
}

func (d Drainer) drain(ctx context.Context) {
	entries, err := os.ReadDir(d.cfg.Queue.Pending)
	if err != nil {
		d.logger.Warn("read pending queue", "err", err)
		return
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		// queuefile.IsItem is the shared pick/accounting predicate: what
		// is drained here is exactly what health//metrics report as
		// backlog (review 2026-08-03 round-2 N2).
		if !entry.IsDir() && queuefile.IsItem(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		d.process(ctx, name)
	}
}

func (d Drainer) process(ctx context.Context, name string) {
	pending := filepath.Join(d.cfg.Queue.Pending, name)
	inflight := filepath.Join(d.cfg.Queue.Inflight, name)
	if err := os.Rename(pending, inflight); err != nil {
		return
	}

	b, err := os.ReadFile(inflight)
	if err != nil {
		d.fail(inflight, name, "read: "+err.Error())
		return
	}
	if err := d.copyRaw(inflight, name); err != nil {
		d.fail(inflight, name, "raw: "+err.Error())
		return
	}
	event, err := NormalizeEnvelope(b)
	if err != nil {
		d.fail(inflight, name, "envelope: "+err.Error())
		return
	}
	res, err := d.store.Ingest(ctx, event)
	if err != nil {
		d.fail(inflight, name, "ingest: "+err.Error())
		return
	}
	if err := os.Remove(inflight); err != nil {
		d.logger.Warn("remove ingested queue file", "file", name, "event_id", res.ID, "err", err)
	}
}

func (d Drainer) fail(path, name, reason string) {
	failed := filepath.Join(d.cfg.Queue.Failed, name)
	if err := os.Rename(path, failed); err != nil {
		d.logger.Warn("move queue item to failed", "file", name, "reason", reason, "err", err)
		return
	}
	body, _ := json.MarshalIndent(map[string]any{
		"reason":          reason,
		"failed_at_epoch": time.Now().Unix(),
	}, "", "  ")
	body = append(body, '\n')
	_ = os.WriteFile(failed+".error.json", body, 0o600)
	d.logger.Warn("queue item failed", "file", name, "reason", reason)
}

func (d Drainer) copyRaw(src, name string) error {
	if err := os.MkdirAll(d.cfg.Raw, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(d.cfg.Raw, name)
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = out.Close()
			_ = os.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	closed = true
	_ = syncDir(d.cfg.Raw)
	return nil
}

// NormalizeEnvelope maps a queue/raw envelope to the canonical EventInput —
// including the source_event_id fallback derivation. Exported so memoire-purge
// matches raw/queue files by the exact identity ingest would assign them
// (envelopes from direct HTTP producers may omit source_event_id on disk).
func NormalizeEnvelope(b []byte) (db.EventInput, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return db.EventInput{}, err
	}
	var event db.EventInput
	if err := json.Unmarshal(b, &event); err != nil {
		return db.EventInput{}, err
	}
	if event.SchemaVersion != 1 {
		return db.EventInput{}, fmt.Errorf("unsupported schema_version %d", event.SchemaVersion)
	}
	if event.SourceAdapter == "" && event.ProducerID != "" {
		event.SourceAdapter = event.ProducerID
	}
	if event.Project == "" || event.SessionID == "" || event.SourceAdapter == "" || event.EventType == "" {
		return db.EventInput{}, errors.New("project, session_id, source_adapter or producer_id, and event_type are required")
	}
	payload, ok := raw["payload"]
	if !ok {
		return db.EventInput{}, errors.New("payload is required")
	}
	if !json.Valid(payload) {
		return db.EventInput{}, errors.New("payload must be valid JSON")
	}
	event.Payload = payload
	if len(event.Metadata) > 0 && !json.Valid(event.Metadata) {
		return db.EventInput{}, errors.New("metadata must be valid JSON")
	}
	if event.ReceivedAtEpoch == 0 {
		event.ReceivedAtEpoch = time.Now().Unix()
	}
	if event.SourceEventID == "" {
		event.SourceEventID = deriveSourceEventID(event)
	}
	if event.SourceEventID == "" {
		return db.EventInput{}, errors.New("source_event_id is required or must be derivable")
	}
	return event, nil
}

func deriveSourceEventID(event db.EventInput) string {
	toolUseID := payloadString(event.Payload, "tool_use_id", "toolUseID", "tool_use.id")
	userPrompt := payloadString(event.Payload, "prompt", "user_prompt", "userPrompt", "message", "text")
	switch {
	case strings.Contains(strings.ToLower(event.EventType), "posttooluse") && toolUseID != "":
		return "derived-" + hashString(event.SessionID+":"+toolUseID)
	case strings.Contains(strings.ToLower(event.EventType), "userpromptsubmit") && userPrompt != "":
		return "derived-" + hashString(event.SessionID+":"+event.EventType+":"+userPrompt)
	case event.EventType == "session_start" || event.EventType == "session_end":
		return "derived-" + hashString(event.SessionID+":"+event.EventType)
	default:
		return "derived-" + canonicalEventHash(event)
	}
}

func payloadString(payload json.RawMessage, keys ...string) string {
	var v map[string]any
	if err := json.Unmarshal(payload, &v); err != nil {
		return ""
	}
	for _, key := range keys {
		if strings.Contains(key, ".") {
			parts := strings.Split(key, ".")
			var cur any = v
			for _, part := range parts {
				m, ok := cur.(map[string]any)
				if !ok {
					cur = nil
					break
				}
				cur = m[part]
			}
			if s, ok := cur.(string); ok && s != "" {
				return s
			}
			continue
		}
		if s, ok := v[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func canonicalEventHash(event db.EventInput) string {
	var payload any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		payload = string(event.Payload)
	}
	stable := struct {
		SchemaVersion int    `json:"schema_version"`
		Project       string `json:"project"`
		SessionID     string `json:"session_id"`
		SourceAdapter string `json:"source_adapter"`
		ProducerID    string `json:"producer_id,omitempty"`
		EventType     string `json:"event_type"`
		Payload       any    `json:"payload"`
	}{
		SchemaVersion: event.SchemaVersion,
		Project:       event.Project,
		SessionID:     event.SessionID,
		SourceAdapter: event.SourceAdapter,
		ProducerID:    event.ProducerID,
		EventType:     event.EventType,
		Payload:       payload,
	}
	b, _ := json.Marshal(stable)
	return hashBytes(b)
}

func hashString(s string) string {
	return hashBytes([]byte(s))
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
