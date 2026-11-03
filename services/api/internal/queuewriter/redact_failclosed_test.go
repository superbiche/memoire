// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package queuewriter

// Fail-closed refusals of the redact-v5 port at the queue-writer boundary
// (review round 1, F7: the refusal branches must execute in tests): a
// nesting error from the scrub, or an unloadable redaction key, fails the
// write on both transports and the batch — nothing reaches queue/pending.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"memoire/services/api/internal/db"
	"memoire/services/api/internal/redact"
)

func deepJSON(levels int) string {
	return strings.Repeat("[", levels) + strings.Repeat("]", levels)
}

// deepText is a text document with more than MaxNesting enclosing
// secret-named objects (the text P2 walk fails closed).
func deepText() string {
	return strings.Repeat(`{"secret":`, redact.MaxNesting+1) + `{}` + strings.Repeat(`}`, redact.MaxNesting+1)
}

func pendingCount(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return len(ents)
}

func TestWriteRefusesDepthErrorsOnEveryField(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = true
	base := db.EventInput{Project: "memoire-test", SessionID: "deep", SourceAdapter: "manual", EventType: "note", Text: "ok"}
	cases := []struct {
		name string
		mut  func(in *db.EventInput)
	}{
		{"text", func(in *db.EventInput) { in.Text = deepText() }},
		{"payload", func(in *db.EventInput) { in.Payload = json.RawMessage(`{"d":` + deepJSON(redact.MaxNesting+1) + `}`) }},
		{"metadata", func(in *db.EventInput) { in.Metadata = json.RawMessage(`{"d":` + deepJSON(redact.MaxNesting+1) + `}`) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mut(&in)
			_, err := Write(context.Background(), cfg, in)
			if !errors.Is(err, redact.ErrDepth) {
				t.Fatalf("%s: want ErrDepth, got %v", tc.name, err)
			}
			if n := pendingCount(t, cfg.Queue.Pending); n != 0 {
				t.Fatalf("%s: %d pending file(s) written despite the refusal", tc.name, n)
			}
		})
	}
	// Control: the same envelope at the limit is written.
	in := base
	in.Payload = json.RawMessage(`{"d":` + deepJSON(redact.MaxNesting-1) + `}`)
	if _, err := Write(context.Background(), cfg, in); err != nil {
		t.Fatalf("depth at the limit must pass: %v", err)
	}
}

func TestWriteBytesRefusesDepthError(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = true
	body := []byte(`{"schema_version":1,"project":"memoire-test","session_id":"deep-http","source_adapter":"curl","event_type":"note","payload":{"d":` + deepJSON(redact.MaxNesting+1) + `}}`)
	_, err := WriteBytes(context.Background(), cfg, "http", body)
	if !errors.Is(err, redact.ErrDepth) {
		t.Fatalf("want ErrDepth, got %v", err)
	}
	if n := pendingCount(t, cfg.Queue.Pending); n != 0 {
		t.Fatalf("%d pending file(s) written despite the refusal", n)
	}
}

func TestBatchRefusesDepthError(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = true
	b := NewBatch(cfg)
	err := b.Add(db.EventInput{Project: "memoire-test", SessionID: "deep-batch", SourceAdapter: "manual", EventType: "note", Text: deepText()})
	if !errors.Is(err, redact.ErrDepth) {
		t.Fatalf("want ErrDepth, got %v", err)
	}
	if items, bytes := b.Pending(); items != 0 || bytes != 0 {
		t.Fatalf("refused item must not be staged: %d items, %d bytes", items, bytes)
	}
	if n := pendingCount(t, cfg.Queue.Pending); n != 0 {
		t.Fatalf("%d pending file(s) written despite the refusal", n)
	}
}

// TestUnloadableRedactionKeyFailsTheWrite: ruling Q8 — the key is loaded
// fail-closed; a key that can neither be read nor created never downgrades
// the envelope to plain-hash markers (review round 1, F4).
func TestUnloadableRedactionKeyFailsTheWrite(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = true
	// A directory where the key file should be: ReadFile fails, WriteFile fails.
	if err := os.MkdirAll(filepath.Join(cfg.Home, "redaction-key"), 0o700); err != nil {
		t.Fatal(err)
	}
	in := db.EventInput{Project: "memoire-test", SessionID: "nokey", SourceAdapter: "manual", EventType: "note", Text: "PASSWORD=FAKEnokeyvalue01"}
	if _, err := Write(context.Background(), cfg, in); err == nil || !strings.Contains(err.Error(), "redaction key") {
		t.Fatalf("CLI path: want a redaction-key error, got %v", err)
	}
	body := []byte(`{"schema_version":1,"project":"memoire-test","session_id":"nokey-http","source_adapter":"curl","event_type":"note","payload":{"m":"PASSWORD=FAKEnokeyvalue02"}}`)
	if _, err := WriteBytes(context.Background(), cfg, "http", body); err == nil || !strings.Contains(err.Error(), "redaction key") {
		t.Fatalf("HTTP path: want a redaction-key error, got %v", err)
	}
	if err := NewBatch(cfg).Add(in); err == nil || !strings.Contains(err.Error(), "redaction key") {
		t.Fatalf("batch path: want a redaction-key error, got %v", err)
	}
	if n := pendingCount(t, cfg.Queue.Pending); n != 0 {
		t.Fatalf("%d pending file(s) written without a usable key", n)
	}
}
