// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package queuewriter

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
	"memoire/services/api/internal/redact"
)

type Result struct {
	Path string
	Name string
}

func Write(ctx context.Context, cfg config.Config, in db.EventInput) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if in.SchemaVersion == 0 {
		in.SchemaVersion = 1
	}
	if in.OccurredAt == "" && in.OccurredAtEpoch == 0 {
		in.OccurredAtEpoch = time.Now().Unix()
	}
	if in.ReceivedAtEpoch == 0 {
		in.ReceivedAtEpoch = time.Now().Unix()
	}
	if in.SourceEventID == "" {
		in.SourceEventID = defaultSourceEventID(in.SourceAdapter)
	}
	if len(in.Payload) == 0 {
		in.Payload = json.RawMessage(`{}`)
	}
	if len(in.Metadata) == 0 {
		in.Metadata = json.RawMessage(`{}`)
	}
	// The violation stamp is queuewriter-owned: only the backstop below may
	// set it. Strip a producer-supplied value unconditionally (redaction on
	// or off) so the /v1/health violation count cannot be forged (round-1
	// review finding 4).
	in.Metadata = stripContractViolation(in.Metadata)
	// Defense-at-source (GOVERNANCE-DESIGN.md ruling 7): redact secret-shaped
	// content BEFORE the pending file exists, so neither queue/, raw/ custody
	// copies, nor SQLite ever hold the value. Runs client-side in hook
	// producers via the memoire-queue binary.
	if cfg.RedactionEnabled {
		r := redactorFor(cfg)
		var textHits, payloadHits, metaHits []redact.Hit
		in.Text, textHits = r.String(in.Text)
		in.Payload, payloadHits = r.JSON(in.Payload)
		in.Metadata, metaHits = r.JSON(in.Metadata)
		if hits := redact.MergeHits(textHits, payloadHits, metaHits); len(hits) > 0 {
			in.Metadata = annotateRedactions(in.Metadata, hits)
			// memoire:D136 R-2: liaison pre-redacts to redact-v1 by contract
			// (docs/LIAISON-EVIDENCE-CONTRACT.md), so a backstop hit on this
			// adapter's path is a contract violation — stamped, never silent.
			if in.SourceAdapter == liaisonEvidenceAdapter {
				in.Metadata = annotateContractViolation(in.Metadata)
			}
		}
	}
	if err := in.Validate(); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(cfg.Queue.Pending, 0o700); err != nil {
		return Result{}, err
	}
	body, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return Result{}, err
	}
	body = append(body, '\n')

	return writePendingBytes(cfg, in.SourceAdapter, body)
}

// WriteBytes is the HTTP-arrival write path: its only callers are the
// POST /v1/events and /v1/events/batch handlers, so "this machine received
// the envelope over the wire" is true by construction — including forwarded
// envelopes from remote capture clients (memoire:D147 slice 3).
func WriteBytes(ctx context.Context, cfg config.Config, sourceAdapter string, body []byte) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	// Hash the ORIGINAL bytes before any rewrite: this is the retry-stable
	// identity for envelopes that omit source_event_id (below). A crashed
	// forwarder re-POSTs the same bytes, so the same hash — whereas anything
	// derived after redaction can drift between attempts (marker keying is
	// machine-local and key availability can change; slice-3 round-1
	// finding 5).
	bodySum := sha256.Sum256(body)
	// Same system-key ownership as Write: a POSTed envelope must not arrive
	// pre-stamped (round-1 finding 4). JSON-aware by requirement — a raw-byte
	// probe is evadable via \u-escaped keys (round-2 finding 4), so decode
	// unconditionally (the redaction walk below decodes every body anyway).
	{
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		var envelope map[string]any
		err := dec.Decode(&envelope)
		if err == nil {
			// The body must be EXACTLY one JSON object: Decode stops after the
			// first value, so `{...}JUNK` would otherwise be silently repaired
			// into a valid envelope by the re-marshal below — turning a body the
			// drainer is supposed to reject loudly into an ingested event
			// (slice-3 round-2 finding 1). Trailing anything leaves the bytes
			// untouched to fail at drain.
			if _, trailing := dec.Token(); trailing != io.EOF {
				err = errors.New("trailing data after envelope")
			}
		}
		if err == nil && envelope == nil {
			// JSON `null` decodes into a nil map WITHOUT error and passes the
			// EOF guard; any stamp assignment below would then panic — a 500
			// the forwarder treats as systemic, wedging the whole queue behind
			// one poison item (slice-3 round-3 finding 1). Null queues
			// byte-identical and dies at drain like every other non-object.
			err = errors.New("null envelope")
		}
		if err == nil {
			meta, _ := envelope["metadata"].(map[string]any)
			if meta != nil {
				delete(meta, "redaction_contract_violation")
			}
			// received_at is stamped at HUB arrival (memoire:D152/D153): the
			// receiver's clock is the trusted side of the one-sided future-skew
			// comparison, and a client-stamped value would blind it — a fast
			// client clock skews occurred_at and received_at together (slice-3
			// round-1 finding 3). The producer's own stamp is preserved as
			// metadata.client_received_at_epoch so the capture-buffering signal
			// survives for forensics.
			if prev, ok := envelope["received_at_epoch"]; ok {
				if n, isNum := prev.(json.Number); isNum && n.String() != "0" {
					if meta == nil {
						meta = map[string]any{}
					}
					meta["client_received_at_epoch"] = n
				}
			}
			if meta != nil {
				envelope["metadata"] = meta
			}
			envelope["received_at_epoch"] = time.Now().Unix()
			// Retry-stable identity for envelopes without source_event_id: the
			// drainer's content-derived fallback hashes the REDACTED payload,
			// which is not stable across attempts (finding 5 above). Stamping
			// the original-bytes hash here makes re-forwards of the same file
			// collapse at ingest like every other envelope shape.
			switch v := envelope["source_event_id"].(type) {
			case nil:
				envelope["source_event_id"] = "http-body-" + hex.EncodeToString(bodySum[:])
			case string:
				if v == "" {
					envelope["source_event_id"] = "http-body-" + hex.EncodeToString(bodySum[:])
				}
			default:
				// Malformed (non-string): leave untouched so the envelope keeps
				// failing loudly at drain instead of being silently repaired.
			}
			if b, err := json.Marshal(envelope); err == nil {
				body = b
			}
		}
	}
	// API-side backstop of the same ruling-7 filter: HTTP producers that POST
	// envelopes directly get the identical rule set. The whole body is walked
	// (any string value can carry a secret); redaction hits are annotated into
	// the envelope's metadata when the body is a JSON object.
	if cfg.RedactionEnabled {
		redacted, hits := redactorFor(cfg).JSON(json.RawMessage(body))
		if len(hits) > 0 {
			// UseNumber: annotating metadata must not corrupt large integers
			// elsewhere in the envelope via a float64 round-trip.
			dec := json.NewDecoder(bytes.NewReader(redacted))
			dec.UseNumber()
			var envelope map[string]any
			decErr := dec.Decode(&envelope)
			if decErr == nil {
				// Same exactly-one-JSON-object rule as the stamp block above:
				// Decode alone would read the first object out of `{...}JUNK`
				// and the re-marshal below would ship it repaired (slice-3
				// round-2 finding 1). Malformed input keeps its redacted bytes
				// unannotated and dies at drain.
				if _, terr := dec.Token(); terr != io.EOF {
					decErr = errors.New("trailing data after envelope")
				}
			}
			if decErr == nil && envelope == nil {
				// Same nil-map panic guard as the stamp block (round-3
				// finding 1); unreachable for real hit-carrying bodies but a
				// panic path is never left to reachability arguments.
				decErr = errors.New("null envelope")
			}
			if decErr == nil {
				meta, _ := envelope["metadata"].(map[string]any)
				if meta == nil {
					meta = map[string]any{}
				}
				meta["redactions"] = hits
				meta["redaction_rules_version"] = redact.RulesVersion
				// memoire:D136 R-2: same violation stamp as the CLI path. The
				// adapter name comes from the envelope body here — the
				// transport-level sourceAdapter parameter is "http". A body
				// that fails to decode skips annotation entirely (existing
				// behavior) and cannot ingest anyway (normalizeEnvelope).
				if sa, _ := envelope["source_adapter"].(string); sa == liaisonEvidenceAdapter {
					meta["redaction_contract_violation"] = true
				}
				envelope["metadata"] = meta
				if b, err := json.Marshal(envelope); err == nil {
					redacted = b
				}
			}
			body = []byte(redacted)
		}
	}
	return writePendingBytes(cfg, sourceAdapter, body)
}

// redactorFor builds the keyed Redactor for markers that resist offline
// dictionary attacks. The key is machine-local under MEMOIRE_HOME; if it
// cannot be read or created, redaction still runs with plain-hash markers —
// the write path never fails over marker quality.
func redactorFor(cfg config.Config) *redact.Redactor {
	key, err := redact.LoadOrCreateKey(filepath.Join(cfg.Home, "redaction-key"))
	if err != nil {
		key = nil
	}
	return redact.New(key)
}

// liaisonEvidenceAdapter is the source_adapter whose envelopes arrive
// pre-redacted by contract (docs/LIAISON-EVIDENCE-CONTRACT.md).
const liaisonEvidenceAdapter = "liaison-evidence"

// stripContractViolation removes a producer-supplied violation stamp from
// metadata — the key is system-owned; only the backstop sets it (round-1
// finding 4). The check MUST be JSON-aware, never a raw-byte probe: a
// \u0072-escaped key (`"\u0072edaction_contract_violation"`) evades byte
// matching while json_extract still resolves it (round-2 finding 4).
// Untouched input stays byte-identical; decoding uses UseNumber so large
// integers survive a re-marshal bit-exact.
func stripContractViolation(meta json.RawMessage) json.RawMessage {
	if len(meta) == 0 {
		return meta
	}
	dec := json.NewDecoder(bytes.NewReader(meta))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return meta
	}
	if _, present := m["redaction_contract_violation"]; !present {
		return meta
	}
	delete(m, "redaction_contract_violation")
	out, err := json.Marshal(m)
	if err != nil {
		return meta
	}
	return out
}

// annotateContractViolation stamps the liaison-evidence contract-violation
// marker (memoire:D136 R-2). The /v1/health violation count reads this via
// the schema/0011 partial index.
func annotateContractViolation(meta json.RawMessage) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(meta, &m); err != nil || m == nil {
		m = map[string]any{}
	}
	m["redaction_contract_violation"] = true
	out, err := json.Marshal(m)
	if err != nil {
		return meta
	}
	return out
}

// annotateRedactions records THAT redaction happened (rules + counts +
// rule-set version), never the values, in the envelope metadata.
func annotateRedactions(meta json.RawMessage, hits []redact.Hit) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(meta, &m); err != nil || m == nil {
		m = map[string]any{}
	}
	m["redactions"] = hits
	m["redaction_rules_version"] = redact.RulesVersion
	out, err := json.Marshal(m)
	if err != nil {
		return meta
	}
	return out
}

func writePendingBytes(cfg config.Config, sourceAdapter string, body []byte) (Result, error) {
	name := queueFileName(sourceAdapter, body)
	finalPath := filepath.Join(cfg.Queue.Pending, name)
	tmpPath := filepath.Join(cfg.Queue.Pending, "."+name+".tmp")

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Result{}, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := f.Write(body); err != nil {
		return Result{}, err
	}
	if err := f.Sync(); err != nil {
		return Result{}, err
	}
	if err := f.Close(); err != nil {
		return Result{}, err
	}
	closed = true
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return Result{}, err
	}
	_ = syncDir(cfg.Queue.Pending)
	return Result{Path: finalPath, Name: name}, nil
}

func PayloadFromText(text string) json.RawMessage {
	if text == "" {
		return json.RawMessage(`{}`)
	}
	b, _ := json.Marshal(map[string]string{"message": text})
	return json.RawMessage(b)
}

func ParseJSONObject(raw string, field string) (json.RawMessage, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return json.RawMessage(`{}`), nil
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("%s must be valid JSON: %w", field, err)
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, fmt.Errorf("%s must be a JSON object", field)
	}
	return json.RawMessage(raw), nil
}

func ReadJSONObjectFile(path string, field string) (json.RawMessage, error) {
	if path == "" {
		return nil, errors.New("path is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseJSONObject(string(b), field)
}

var filenamePartRE = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func queueFileName(sourceAdapter string, body []byte) string {
	adapter := filenamePartRE.ReplaceAllString(sourceAdapter, "-")
	adapter = strings.Trim(adapter, "-._")
	if adapter == "" {
		adapter = "unknown"
	}
	if len(adapter) > 48 {
		adapter = adapter[:48]
	}
	// FULL content hash, never truncated: writePendingBytes renames over the
	// final name without an existence check, so a same-millisecond collision
	// between DIFFERENT bodies would silently replace a pending envelope its
	// producer already holds an ack for (custody loss — slice-3 round-1
	// finding 1). With the full hash a name collision implies byte-identical
	// content, and replacing a pending file with the same bytes loses nothing.
	sum := sha256.Sum256(body)
	return fmt.Sprintf("%d-%s-%s.json", time.Now().UnixMilli(), adapter, hex.EncodeToString(sum[:]))
}

func defaultSourceEventID(sourceAdapter string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%d", sourceAdapter, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-%d-%s", sourceAdapter, time.Now().UnixNano(), hex.EncodeToString(b[:]))
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
