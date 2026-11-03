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
	if !strings.Contains(s, `"redactions"`) || !strings.Contains(s, "redact-v7") {
		t.Fatalf("redaction metadata missing from envelope:\n%s", s)
	}
}

// TestWriteStampsRulesVersionOnCleanEvent pins Fibery #616 decision 3 (review
// round-1 F1): the rule-set version is stamped on every valid envelope, hit
// or no hit, so "filtered by this version and clean" is distinguishable from
// "never filtered". A producer-supplied version is dropped first; an upstream
// hit list (forwarded pending file) is kept and merged.
func TestWriteStampsRulesVersionOnCleanEvent(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = true
	res, err := Write(context.Background(), cfg, db.EventInput{
		Project:       "memoire-test",
		SessionID:     "clean-session",
		SourceAdapter: "manual",
		EventType:     "note",
		Text:          "nothing secret here",
		Metadata:      json.RawMessage(`{"redaction_rules_version":"redact-v0","keep":9007199254740993}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	s := string(b)
	if !strings.Contains(s, "redact-v7") || strings.Contains(s, "redact-v0") {
		t.Fatalf("clean event must carry the current rule-set version and drop the producer's:\n%s", s)
	}
	if strings.Contains(s, `"redactions"`) {
		t.Fatalf("a clean event has no hit list:\n%s", s)
	}
	if !strings.Contains(s, "9007199254740993") {
		t.Fatalf("large integer must survive the stamp bit-exact:\n%s", s)
	}
}

// TestForwardedHitListSurvivesHubWalk: a pending file redacted on a capture
// host (markers in the body, local hit list + version in metadata) is
// forwarded to the hub, whose walk sees only exempt markers and finds no new
// hit. The hub must keep the local hit list (evidence of the markers), merge
// any new hits, and re-stamp the version with its own filter.
func TestForwardedHitListSurvivesHubWalk(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = true
	body := []byte(`{"schema_version":1,"project":"memoire-test","session_id":"fwd","source_adapter":"claude-code","event_type":"note","payload":{"m":"DATABASE_PASSWORD=[REDACTED:env-secret-assignment:0123abcd] and AKIA` +
		`IOSFODNN7EXAMPLE"},"metadata":{"redactions":[{"rule":"env-secret-assignment","count":1}],"redaction_rules_version":"redact-v1"}}`)
	res, err := WriteBytes(context.Background(), cfg, "http", body)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	s := string(b)
	// Key order differs between a struct marshal and a map re-marshal; assert
	// on rule ids and counts, not on byte layout.
	if !strings.Contains(s, `"rule":"aws-access-key"`) || !strings.Contains(s, `"rule":"env-secret-assignment"`) || strings.Count(s, `"count":1`) != 2 {
		t.Fatalf("hub must merge its new hit with the forwarded local hit list:\n%s", s)
	}
	if !strings.Contains(s, `"redaction_rules_version":"redact-v7"`) || strings.Contains(s, "redact-v1") {
		t.Fatalf("hub re-stamps the version with the filter that ran last:\n%s", s)
	}
	// Clean hub walk: markers only, no new hit — the local list still survives.
	clean := []byte(`{"schema_version":1,"project":"memoire-test","session_id":"fwd2","source_adapter":"claude-code","event_type":"note","payload":{"m":"[REDACTED:openai-key:0123abcd]"},"metadata":{"redactions":[{"rule":"openai-key","count":1}],"redaction_rules_version":"redact-v1"}}`)
	res2, err := WriteBytes(context.Background(), cfg, "http", clean)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(res2.Path)
	s2 := string(b2)
	if !strings.Contains(s2, `"rule":"openai-key"`) || !strings.Contains(s2, `"count":1`) || !strings.Contains(s2, `"redaction_rules_version":"redact-v7"`) {
		t.Fatalf("clean hub walk must keep the forwarded hit list and re-stamp the version:\n%s", s2)
	}
}

// TestWriteBytesStampsRulesVersionOnCleanEnvelope: same universal stamp on
// the HTTP path, with and without producer metadata, producer value dropped.
func TestWriteBytesStampsRulesVersionOnCleanEnvelope(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = true
	for name, body := range map[string]string{
		"no-metadata": `{"schema_version":1,"project":"memoire-test","session_id":"http-clean","source_adapter":"curl","event_type":"note","payload":{"message":"clean"}}`,
		"stale-meta":  `{"schema_version":1,"project":"memoire-test","session_id":"http-clean","source_adapter":"curl","event_type":"note","payload":{"message":"clean"},"metadata":{"redaction_rules_version":"redact-v0","big":9007199254740993}}`,
	} {
		res, err := WriteBytes(context.Background(), cfg, "http", []byte(body))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := os.ReadFile(res.Path)
		s := string(b)
		if !strings.Contains(s, `"redaction_rules_version":"redact-v7"`) || strings.Contains(s, "redact-v0") {
			t.Fatalf("%s: clean HTTP envelope must carry the current version and drop the producer's:\n%s", name, s)
		}
		if strings.Contains(s, `"redactions"`) {
			t.Fatalf("%s: a clean envelope has no hit list:\n%s", name, s)
		}
		if name == "stale-meta" && !strings.Contains(s, "9007199254740993") {
			t.Fatalf("%s: large integer must survive bit-exact:\n%s", name, s)
		}
	}
}

// TestDisabledRedactionStampsNoVersion: with MEMOIRE_REDACT=0 no filter ran,
// so no version is attested — and a producer cannot supply one either.
func TestDisabledRedactionStampsNoVersion(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = false
	res, err := Write(context.Background(), cfg, db.EventInput{
		Project:       "memoire-test",
		SessionID:     "redact-off-stamp",
		SourceAdapter: "manual",
		EventType:     "note",
		Text:          "plain",
		Metadata:      json.RawMessage(`{"redaction_rules_version":"redact-v2"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	if strings.Contains(string(b), "redaction_rules_version") {
		t.Fatalf("no filter ran, so no version may be attested:\n%s", b)
	}
	body := []byte(`{"schema_version":1,"project":"memoire-test","session_id":"http-off","source_adapter":"curl","event_type":"note","payload":{"m":"x"},"metadata":{"redaction_rules_version":"redact-v2"}}`)
	res2, err := WriteBytes(context.Background(), cfg, "http", body)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(res2.Path)
	if strings.Contains(string(b2), "redaction_rules_version") {
		t.Fatalf("HTTP: no filter ran, so no version may be attested:\n%s", b2)
	}
}

// TestNonObjectMetadataIsNeverReplaced pins round-2 F6: the writer stamps
// only absent/null/object metadata. Array or string metadata — accepted by
// Validate and the drainer, never produced by in-repo producers — is left
// byte-identical on both transports, clean or hit-bearing; the body is still
// redacted and the markers are the record.
func TestNonObjectMetadataIsNeverReplaced(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = true
	res, err := Write(context.Background(), cfg, db.EventInput{
		Project:       "memoire-test",
		SessionID:     "array-meta",
		SourceAdapter: "manual",
		EventType:     "note",
		Text:          "clean text",
		Metadata:      json.RawMessage(`["a",9007199254740993]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	s := string(b)
	if !strings.Contains(s, `"a"`) || !strings.Contains(s, "9007199254740993") || strings.Contains(s, "redaction_rules_version") {
		t.Fatalf("array metadata must stay byte-identical and unstamped:\n%s", s)
	}
	res2, err := Write(context.Background(), cfg, db.EventInput{
		Project:       "memoire-test",
		SessionID:     "array-meta-hit",
		SourceAdapter: "manual",
		EventType:     "note",
		Text:          "key AKIA" + "IOSFODNN7EXAMPLE leaked",
		Metadata:      json.RawMessage(`"opaque"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(res2.Path)
	s2 := string(b2)
	if strings.Contains(s2, "AKIA" + "IOSFODNN7EXAMPLE") || !strings.Contains(s2, `"metadata": "opaque"`) || strings.Contains(s2, `"redactions"`) {
		t.Fatalf("hit-bearing event with string metadata: body redacted, metadata untouched, no annotation:\n%s", s2)
	}
	body := []byte(`{"schema_version":1,"project":"memoire-test","session_id":"http-array","source_adapter":"curl","event_type":"note","payload":{"m":"key AKIA` +
		`IOSFODNN7EXAMPLE"},"metadata":[1,2]}`)
	res3, err := WriteBytes(context.Background(), cfg, "http", body)
	if err != nil {
		t.Fatal(err)
	}
	b3, _ := os.ReadFile(res3.Path)
	s3 := string(b3)
	if strings.Contains(s3, "AKIA" + "IOSFODNN7EXAMPLE") || !strings.Contains(s3, `"metadata":[1,2]`) || strings.Contains(s3, "redaction_rules_version") {
		t.Fatalf("HTTP: array metadata must stay byte-identical and unstamped while the body is redacted:\n%s", s3)
	}
}

// TestCarriedHitListNeverAuthorsViolation pins round-3 F7: a liaison-evidence
// envelope arriving with an upstream `redactions` list and no new hit keeps
// the list but gets NO contract-violation stamp on either transport; a new
// hit on this walk still stamps it.
func TestCarriedHitListNeverAuthorsViolation(t *testing.T) {
	cfg := testConfig(t)
	cfg.RedactionEnabled = true
	carried := json.RawMessage(`{"redactions":[{"rule":"openai-key","count":1}],"liaison_redaction_rules_version":"redact-v1"}`)
	res, err := Write(context.Background(), cfg, db.EventInput{
		Project: "liaison", SessionID: "echo-clean", SourceAdapter: "liaison-evidence", EventType: "liaison_evidence",
		Text: `{"liaison_evidence_observation":{"artifact_ref":"[REDACTED:openai-key:0123abcd]"},"schema_version":1}`, Metadata: carried,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	s := string(b)
	if strings.Contains(s, "redaction_contract_violation") || !strings.Contains(s, `"rule": "openai-key"`) || !strings.Contains(s, "redact-v7") {
		t.Fatalf("CLI: carried list must survive without authoring a violation:\n%s", s)
	}
	res2, err := Write(context.Background(), cfg, db.EventInput{
		Project: "liaison", SessionID: "echo-hit", SourceAdapter: "liaison-evidence", EventType: "liaison_evidence",
		Text: "key AKIA" + "IOSFODNN7EXAMPLE leaked", Metadata: carried,
	})
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(res2.Path)
	s2 := string(b2)
	if !strings.Contains(s2, `"redaction_contract_violation": true`) || !strings.Contains(s2, `"rule": "aws-access-key"`) || !strings.Contains(s2, `"rule": "openai-key"`) {
		t.Fatalf("CLI: a hit on this walk stamps the violation and merges the lists:\n%s", s2)
	}
	body := []byte(`{"schema_version":1,"project":"liaison","session_id":"echo-http","source_adapter":"liaison-evidence","event_type":"liaison_evidence","payload":{"m":"[REDACTED:openai-key:0123abcd]"},"metadata":{"redactions":[{"rule":"openai-key","count":1}]}}`)
	res3, err := WriteBytes(context.Background(), cfg, "http", body)
	if err != nil {
		t.Fatal(err)
	}
	b3, _ := os.ReadFile(res3.Path)
	s3 := string(b3)
	if strings.Contains(s3, "redaction_contract_violation") || !strings.Contains(s3, `"rule":"openai-key"`) {
		t.Fatalf("HTTP: carried list must survive without authoring a violation:\n%s", s3)
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
	body := []byte(`{"schema_version":1,"project":"memoire-test","session_id":"http-redact","source_adapter":"curl","event_type":"note","payload":{"message":"key AKIA` +
		`IOSFODNN7EXAMPLE leaked"}}`)
	res, err := WriteBytes(context.Background(), cfg, "http", body)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	s := string(b)
	if strings.Contains(s, "AKIA" + "IOSFODNN7EXAMPLE") {
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
		Text:          `{"liaison_evidence_observation":{"task_ref":"key AKIA` +
		`IOSFODNN7EXAMPLE leaked"},"schema_version":1}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	s := string(b)
	if strings.Contains(s, "AKIA" + "IOSFODNN7EXAMPLE") {
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
		Text:          "key AKIA" + "IOSFODNN7EXAMPLE leaked",
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
		`"payload":{},"text":"key AKIA` +
			`IOSFODNN7EXAMPLE leaked"}`)
	res, err := WriteBytes(context.Background(), cfg, "http", body)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	s := string(b)
	if strings.Contains(s, "AKIA" + "IOSFODNN7EXAMPLE") {
		t.Fatalf("secret survived HTTP backstop:\n%s", s)
	}
	if !strings.Contains(s, `"redaction_contract_violation":true`) {
		t.Fatalf("HTTP liaison-evidence rewrite must stamp the violation marker:\n%s", s)
	}

	other := []byte(`{"schema_version":1,"project":"x","session_id":"http-other",` +
		`"source_adapter":"curl","event_type":"note",` +
		`"payload":{},"text":"key AKIA` +
			`IOSFODNN7EXAMPLE leaked"}`)
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
