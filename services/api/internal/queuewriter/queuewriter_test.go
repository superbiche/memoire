// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package queuewriter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
)

func TestWriteCreatesAtomicPendingEvent(t *testing.T) {
	cfg := testConfig(t)
	res, err := Write(context.Background(), cfg, db.EventInput{
		Project:       "memoire-test",
		SessionID:     "queue-writer-session",
		SourceAdapter: "codex hook",
		EventType:     "note",
		Payload:       PayloadFromText("queue writer needle"),
		Text:          "queue writer needle",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Name, "1") || !strings.Contains(res.Name, "-codex-hook-") || !strings.HasSuffix(res.Name, ".json") {
		t.Fatalf("unexpected queue file name: %s", res.Name)
	}
	if strings.HasPrefix(filepath.Base(res.Path), ".") {
		t.Fatalf("final path still looks temporary: %s", res.Path)
	}
	entries, err := os.ReadDir(cfg.Queue.Pending)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one pending file, got %d", len(entries))
	}
	b, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	var event db.EventInput
	if err := json.Unmarshal(b, &event); err != nil {
		t.Fatal(err)
	}
	if event.SourceEventID == "" {
		t.Fatal("expected default source_event_id")
	}
	if event.SchemaVersion != 1 {
		t.Fatalf("expected default schema_version 1, got %d", event.SchemaVersion)
	}
	if event.OccurredAtEpoch == 0 {
		t.Fatal("expected default occurred_at_epoch")
	}
	if event.ReceivedAtEpoch == 0 {
		t.Fatal("expected default received_at_epoch")
	}
	if event.Text != "queue writer needle" {
		t.Fatalf("text mismatch: %q", event.Text)
	}
}

func TestParseJSONObjectRejectsNonObject(t *testing.T) {
	if _, err := ParseJSONObject(`[]`, "payload"); err == nil {
		t.Fatal("expected non-object JSON to fail")
	}
	if _, err := ParseJSONObject(`{"ok":true}`, "payload"); err != nil {
		t.Fatal(err)
	}
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{
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
	return cfg
}

func TestWriteRedactsSecretsWhenEnabled(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = true
	payload, _ := json.Marshal(map[string]any{
		"tool_name":  "Write",
		"tool_input": map[string]any{"file_path": "/x/.env", "content": "DATABASE_PASSWORD=hunter2hunter2\nSAFE_REF=op://Vault/db/pass"},
	})
	res, err := Write(context.Background(), cfg, db.EventInput{
		Project:       "memoire-test",
		SessionID:     "redact-session",
		SourceAdapter: "claude-code",
		EventType:     "claude_PostToolUse",
		Payload:       payload,
		Text:          "changed .env with Authorization: Bearer topsecretbearer42",
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, secret := range []string{"hunter2hunter2", "topsecretbearer42"} {
		if strings.Contains(s, secret) {
			t.Fatalf("secret %q survived into the pending file:\n%s", secret, s)
		}
	}
	if !strings.Contains(s, "op://Vault/db/pass") {
		t.Fatalf("op:// reference must survive redaction")
	}
	if !strings.Contains(s, `"redactions"`) || !strings.Contains(s, "redact-v1") {
		t.Fatalf("redaction metadata missing from envelope:\n%s", s)
	}
}

func TestWriteSkipsRedactionWhenDisabled(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = false
	res, err := Write(context.Background(), cfg, db.EventInput{
		Project:       "memoire-test",
		SessionID:     "redact-off",
		SourceAdapter: "manual",
		EventType:     "note",
		Text:          "PASSWORD=verysecret123",
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	if !strings.Contains(string(b), "verysecret123") {
		t.Fatalf("MEMOIRE_REDACT=0 must disable redaction")
	}
}

func TestWriteBytesRedactsHTTPEnvelopes(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = true
	body := []byte(`{"schema_version":1,"project":"memoire-test","session_id":"http-redact","source_adapter":"curl","event_type":"note","payload":{"message":"key AKIAIOSFODNN7EXAMPLE leaked"}}`)
	res, err := WriteBytes(context.Background(), cfg, "http", body)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	s := string(b)
	if strings.Contains(s, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("secret survived HTTP backstop:\n%s", s)
	}
	if !strings.Contains(s, `"redactions"`) {
		t.Fatalf("redaction metadata missing:\n%s", s)
	}
}

func TestWriteStampsLiaisonEvidenceContractViolation(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = true
	// A backstop hit on the liaison-evidence path is a contract violation
	// (memoire:D136 R-2): liaison pre-redacts to redact-v1, so nothing
	// secret-shaped should ever reach this writer.
	res, err := Write(context.Background(), cfg, db.EventInput{
		Project:       "liaison",
		SessionID:     "echo-violation",
		SourceAdapter: "liaison-evidence",
		EventType:     "liaison_evidence",
		Text:          `{"liaison_evidence_observation":{"task_ref":"key AKIAIOSFODNN7EXAMPLE leaked"},"schema_version":1}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	s := string(b)
	if strings.Contains(s, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("secret survived the backstop:\n%s", s)
	}
	if !strings.Contains(s, `"redaction_contract_violation": true`) {
		t.Fatalf("liaison-evidence backstop rewrite must stamp the violation marker:\n%s", s)
	}

	// Same rewrite on any other adapter stays a plain annotated redaction.
	res2, err := Write(context.Background(), cfg, db.EventInput{
		Project:       "memoire-test",
		SessionID:     "no-violation",
		SourceAdapter: "claude-code",
		EventType:     "claude_PostToolUse",
		Text:          "key AKIAIOSFODNN7EXAMPLE leaked",
	})
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(res2.Path)
	if strings.Contains(string(b2), "redaction_contract_violation") {
		t.Fatalf("non-liaison-evidence adapters must not carry the violation stamp:\n%s", string(b2))
	}
}

func TestWriteBytesStampsViolationFromEnvelopeAdapter(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = true
	// The HTTP transport parameter is "http"; the stamp must key off the
	// envelope's own source_adapter field.
	body := []byte(`{"schema_version":1,"project":"liaison","session_id":"http-echo",` +
		`"source_adapter":"liaison-evidence","event_type":"liaison_evidence",` +
		`"payload":{},"text":"key AKIAIOSFODNN7EXAMPLE leaked"}`)
	res, err := WriteBytes(context.Background(), cfg, "http", body)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	s := string(b)
	if strings.Contains(s, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("secret survived HTTP backstop:\n%s", s)
	}
	if !strings.Contains(s, `"redaction_contract_violation":true`) {
		t.Fatalf("HTTP liaison-evidence rewrite must stamp the violation marker:\n%s", s)
	}

	other := []byte(`{"schema_version":1,"project":"x","session_id":"http-other",` +
		`"source_adapter":"curl","event_type":"note",` +
		`"payload":{},"text":"key AKIAIOSFODNN7EXAMPLE leaked"}`)
	res2, err := WriteBytes(context.Background(), cfg, "http", other)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(res2.Path)
	if strings.Contains(string(b2), "redaction_contract_violation") {
		t.Fatalf("non-liaison-evidence HTTP envelopes must not carry the stamp:\n%s", string(b2))
	}
}

func TestForgedViolationStampIsStripped(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = true
	// CLI path: a producer-supplied stamp with NO secret content must not
	// survive — the key is queuewriter-owned (round-1 review finding 4).
	res, err := Write(context.Background(), cfg, db.EventInput{
		Project:       "liaison",
		SessionID:     "forged-cli",
		SourceAdapter: "liaison-evidence",
		EventType:     "liaison_evidence",
		Text:          `{"liaison_evidence_observation":{"task_ref":"clean"},"schema_version":1}`,
		Metadata:      json.RawMessage(`{"redaction_contract_violation":true,"other":"kept"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	if strings.Contains(string(b), "redaction_contract_violation") {
		t.Fatalf("forged CLI stamp must be stripped:\n%s", string(b))
	}
	if !strings.Contains(string(b), `"other"`) {
		t.Fatalf("stripping must preserve other metadata keys:\n%s", string(b))
	}

	// HTTP path: same forgery in a POSTed envelope body.
	body := []byte(`{"schema_version":1,"project":"liaison","session_id":"forged-http",` +
		`"source_adapter":"liaison-evidence","event_type":"liaison_evidence",` +
		`"payload":{},"text":"clean text",` +
		`"metadata":{"redaction_contract_violation":true,"other":"kept"}}`)
	res2, err := WriteBytes(context.Background(), cfg, "http", body)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(res2.Path)
	if strings.Contains(string(b2), "redaction_contract_violation") {
		t.Fatalf("forged HTTP stamp must be stripped:\n%s", string(b2))
	}
	if !strings.Contains(string(b2), `"other"`) {
		t.Fatalf("stripping must preserve other metadata keys:\n%s", string(b2))
	}

	// Even with redaction disabled the forged CLI stamp must not survive.
	cfg.RedactionEnabled = false
	res3, err := Write(context.Background(), cfg, db.EventInput{
		Project:       "liaison",
		SessionID:     "forged-noredact",
		SourceAdapter: "liaison-evidence",
		EventType:     "liaison_evidence",
		Text:          "clean",
		Metadata:      json.RawMessage(`{"redaction_contract_violation":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	b3, _ := os.ReadFile(res3.Path)
	if strings.Contains(string(b3), "redaction_contract_violation") {
		t.Fatalf("forged stamp must be stripped with redaction off:\n%s", string(b3))
	}
}

func TestEscapedForgedViolationStampIsStripped(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = true
	// Round-2 finding 4: a \u-escaped key evades raw-byte probes while
	// SQLite json_extract still resolves it. The strip must be JSON-aware.
	res, err := Write(context.Background(), cfg, db.EventInput{
		Project:       "liaison",
		SessionID:     "escaped-cli",
		SourceAdapter: "liaison-evidence",
		EventType:     "liaison_evidence",
		Text:          "clean",
		Metadata:      json.RawMessage(`{"\u0072edaction_contract_violation":true,"other":"kept"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	if strings.Contains(string(b), "edaction_contract_violation") {
		t.Fatalf("escaped forged CLI stamp must be stripped:\n%s", string(b))
	}

	for _, adapter := range []string{"http", "http-batch"} {
		body := []byte(`{"schema_version":1,"project":"liaison","session_id":"escaped-` + adapter + `",` +
			`"source_adapter":"liaison-evidence","event_type":"liaison_evidence",` +
			`"payload":{},"text":"clean",` +
			`"metadata":{"\u0072edaction_contract_violation":true,"other":"kept"}}`)
		res2, err := WriteBytes(context.Background(), cfg, adapter, body)
		if err != nil {
			t.Fatal(err)
		}
		b2, _ := os.ReadFile(res2.Path)
		if strings.Contains(string(b2), "edaction_contract_violation") {
			t.Fatalf("escaped forged %s stamp must be stripped:\n%s", adapter, string(b2))
		}
		if !strings.Contains(string(b2), `"other"`) {
			t.Fatalf("stripping must preserve other metadata keys (%s):\n%s", adapter, string(b2))
		}
	}
}
