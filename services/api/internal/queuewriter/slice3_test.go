// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package queuewriter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"memoire/services/api/internal/config"
)

func slice3Config(t *testing.T) config.Config {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{
		Home:   home,
		DBPath: filepath.Join(home, "memoire.db"),
		Raw:    filepath.Join(home, "raw"),
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

func readOnlyPending(t *testing.T, cfg config.Config) map[string]any {
	t.Helper()
	entries, err := os.ReadDir(cfg.Queue.Pending)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 pending file, got %d", len(entries))
	}
	b, err := os.ReadFile(filepath.Join(cfg.Queue.Pending, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(b, &envelope); err != nil {
		t.Fatalf("pending file not JSON: %v", err)
	}
	return envelope
}

// memoire:D152/D153 (slice-3 round-1 findings 3+5): the HTTP surface stamps
// hub-arrival received_at (preserving the client stamp in metadata) and a
// retry-stable original-bytes source_event_id when the envelope has none.
func TestWriteBytesStampsHubArrivalAndStableIdentity(t *testing.T) {
	cfg := slice3Config(t)
	body := []byte(`{"schema_version":1,"project":"p","session_id":"s","source_adapter":"a","event_type":"note","received_at_epoch":123,"payload":{"message":"x"}}`)
	sum := sha256.Sum256(body)
	wantID := "http-body-" + hex.EncodeToString(sum[:])

	if _, err := WriteBytes(context.Background(), cfg, "http", body); err != nil {
		t.Fatal(err)
	}
	envelope := readOnlyPending(t, cfg)

	received, _ := envelope["received_at_epoch"].(float64)
	now := float64(time.Now().Unix())
	if received < now-10 || received > now+10 {
		t.Fatalf("received_at_epoch must be hub-arrival time, got %v", envelope["received_at_epoch"])
	}
	meta, _ := envelope["metadata"].(map[string]any)
	if meta == nil {
		t.Fatalf("expected metadata with client stamp, envelope: %v", envelope)
	}
	if got, _ := meta["client_received_at_epoch"].(float64); got != 123 {
		t.Fatalf("client_received_at_epoch = %v, want 123", meta["client_received_at_epoch"])
	}
	if got, _ := envelope["source_event_id"].(string); got != wantID {
		t.Fatalf("source_event_id = %q, want original-bytes stamp %q", got, wantID)
	}
}

func TestWriteBytesStampIsRetryStable(t *testing.T) {
	cfg := slice3Config(t)
	body := []byte(`{"schema_version":1,"project":"p","session_id":"s","source_adapter":"a","event_type":"note","payload":{"m":1}}`)

	ids := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		if _, err := WriteBytes(context.Background(), cfg, "http", body); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(cfg.Queue.Pending)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(cfg.Queue.Pending, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var envelope map[string]any
			if err := json.Unmarshal(b, &envelope); err != nil {
				t.Fatal(err)
			}
			id, _ := envelope["source_event_id"].(string)
			ids = append(ids, id)
			if err := os.Remove(filepath.Join(cfg.Queue.Pending, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(ids) != 2 || ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("re-POSTed identical bytes must stamp identical source_event_id, got %v", ids)
	}
}

func TestWriteBytesLeavesExistingAndMalformedIdentityAlone(t *testing.T) {
	cfg := slice3Config(t)
	if _, err := WriteBytes(context.Background(), cfg, "http",
		[]byte(`{"source_event_id":"producer-id-1","payload":{}}`)); err != nil {
		t.Fatal(err)
	}
	envelope := readOnlyPending(t, cfg)
	if got, _ := envelope["source_event_id"].(string); got != "producer-id-1" {
		t.Fatalf("producer-supplied source_event_id must survive, got %q", got)
	}
	if err := os.Remove(filepath.Join(cfg.Queue.Pending, mustOne(t, cfg))); err != nil {
		t.Fatal(err)
	}

	// A non-string source_event_id is a malformed envelope: it must stay
	// malformed (and fail loudly at drain), not be silently repaired.
	if _, err := WriteBytes(context.Background(), cfg, "http",
		[]byte(`{"source_event_id":42,"payload":{}}`)); err != nil {
		t.Fatal(err)
	}
	envelope = readOnlyPending(t, cfg)
	if _, isString := envelope["source_event_id"].(string); isString {
		t.Fatalf("non-string source_event_id must not be repaired, got %v", envelope["source_event_id"])
	}
}

// Every file queuewriter.Write produces ends with a newline, and that is
// exactly what the forwarder re-POSTs — trailing WHITESPACE must not trip
// the exactly-one-JSON-value guard or forwarded envelopes silently lose
// their hub-arrival stamps.
func TestWriteBytesStampsNewlineTerminatedBodies(t *testing.T) {
	cfg := slice3Config(t)
	if _, err := WriteBytes(context.Background(), cfg, "http",
		[]byte("{\"schema_version\":1,\"project\":\"p\",\"session_id\":\"s\",\"source_adapter\":\"a\",\"event_type\":\"note\",\"payload\":{}}  \n")); err != nil {
		t.Fatal(err)
	}
	envelope := readOnlyPending(t, cfg)
	if _, ok := envelope["received_at_epoch"].(float64); !ok {
		t.Fatalf("newline-terminated body must still be stamped, got %v", envelope)
	}
	if id, _ := envelope["source_event_id"].(string); !strings.HasPrefix(id, "http-body-") {
		t.Fatalf("expected original-bytes id stamp, got %q", id)
	}
}

// Slice-3 round-2 finding 1: a body that is not EXACTLY one JSON object must
// pass through byte-identical — never be "repaired" by the stamp/re-marshal
// path into a valid envelope. The drainer owns rejecting it loudly.
func TestWriteBytesLeavesTrailingJunkUntouched(t *testing.T) {
	cfg := slice3Config(t)
	for name, body := range map[string]string{
		"trailing-junk":   `{"schema_version":1,"project":"p","session_id":"s","source_adapter":"a","event_type":"note","payload":{}}JUNK`,
		"second-value":    `{"payload":{}} {"payload":{}}`,
		"not-json-at-all": `hello`,
		"array-body":      `[{"payload":{}}]`,
		// JSON null decodes into a nil map WITHOUT error — the shape that
		// panicked the stamp path (round-3 finding 1). Must pass through
		// byte-identical, never 500.
		"null-body": `null`,
		"bool-body": `true`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := WriteBytes(context.Background(), cfg, "http", []byte(body)); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(cfg.Queue.Pending, mustOne(t, cfg)))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != body {
				t.Fatalf("malformed body must be queued byte-identical (drain rejects it), got:\n%s\nwant:\n%s", got, body)
			}
			if err := os.Remove(filepath.Join(cfg.Queue.Pending, mustOne(t, cfg))); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The same guarantee must survive redaction: a trailing-junk body carrying a
// secret gets the secret killed at source (defense-at-source is
// non-negotiable) but must NOT come out the other side as a repaired valid
// envelope — the junk survives so the drainer still rejects it.
func TestWriteBytesRedactsButNeverRepairsTrailingJunk(t *testing.T) {
	cfg := slice3Config(t)
	cfg.RedactionEnabled = true
	body := `{"schema_version":1,"project":"p","session_id":"s","source_adapter":"a","event_type":"note","payload":{"k":"AKIA` +
`IOSFODNN7EXAMPLE"}}JUNK`
	if _, err := WriteBytes(context.Background(), cfg, "http", []byte(body)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(cfg.Queue.Pending, mustOne(t, cfg)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "AKIA" + "IOSFODNN7EXAMPLE") {
		t.Fatal("secret survived the write path")
	}
	if !strings.Contains(string(got), "JUNK") {
		t.Fatalf("trailing junk must survive redaction (drain rejects the item), got: %s", got)
	}
	var repaired map[string]any
	if json.Unmarshal(got, &repaired) == nil {
		t.Fatalf("malformed body must stay malformed, but parses as JSON: %s", got)
	}
}

func TestQueueFileNameCarriesFullContentHash(t *testing.T) {
	body := []byte(`{"x":1}`)
	sum := sha256.Sum256(body)
	name := queueFileName("http", body)
	if !strings.HasSuffix(name, "-"+hex.EncodeToString(sum[:])+".json") {
		t.Fatalf("queue file name must carry the FULL content hash (collision => identical bytes), got %q", name)
	}
}

func mustOne(t *testing.T, cfg config.Config) string {
	t.Helper()
	entries, err := os.ReadDir(cfg.Queue.Pending)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 pending file, got %d", len(entries))
	}
	return entries[0].Name()
}
