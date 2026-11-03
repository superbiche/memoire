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
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	body, err := prepareBody(cfg, in)
	if err != nil {
		return Result{}, err
	}
	return writePendingBytes(cfg, in.SourceAdapter, body)
}

// prepareBody turns an event input into the queue item bytes: defaults,
// contract-stamp stripping, redaction, validation, indented JSON. Shared by
// Write and Batch so both produce byte-identical items.
func prepareBody(cfg config.Config, in db.EventInput) ([]byte, error) {
	return prepareBodyWith(cfg, in, nil)
}

// prepareBodyWith is prepareBody with a redactor built by the caller: a Batch
// builds it once, before its workers start, so the redaction key is created
// exactly once and every item of the batch carries markers of the same key. A
// nil redactor loads (or creates) the key for this one call, as Write does.
func prepareBodyWith(cfg config.Config, in db.EventInput, r *redact.Redactor) ([]byte, error) {
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
	in.Metadata = stripSystemRedactionKeys(in.Metadata)
	// Defense-at-source (GOVERNANCE-DESIGN.md ruling 7): redact secret-shaped
	// content BEFORE the pending file exists, so neither queue/, raw/ custody
	// copies, nor SQLite ever hold the value. Runs client-side in hook
	// producers via the memoire-queue binary.
	if cfg.RedactionEnabled {
		if r == nil {
			var rerr error
			if r, rerr = redactorFor(cfg); rerr != nil {
				return nil, rerr
			}
		}
		// Fail-closed (redact-v5 §Errors): a nesting error from the scrub
		// fails the write — the envelope is never written unredacted.
		var textHits, payloadHits, metaHits []redact.Hit
		var err error
		if in.Text, textHits, err = r.String(in.Text); err != nil {
			return nil, fmt.Errorf("redact text: %w", err)
		}
		if in.Payload, payloadHits, err = r.JSON(in.Payload); err != nil {
			return nil, fmt.Errorf("redact payload: %w", err)
		}
		if in.Metadata, metaHits, err = r.JSON(in.Metadata); err != nil {
			return nil, fmt.Errorf("redact metadata: %w", err)
		}
		// The rule-set version is Memoire's attestation of WHICH filter ran,
		// hit or no hit (Fibery #616 decision 3, review round-1 F1): every
		// valid envelope carries it, so a later reader can tell "filtered by
		// redact-v2 and clean" from "never filtered by v2".
		in.Metadata = annotateRulesVersion(in.Metadata)
		// Two predicates, kept apart (review round-3 F7): the hit list is
		// CUMULATIVE evidence (carried upstream accounting merged with this
		// walk's hits); the contract-violation stamp is THIS writer's
		// attestation and follows this walk's hits only — a carried list
		// never authors a violation.
		newHits := redact.MergeHits(textHits, payloadHits, metaHits)
		if hits := redact.MergeHits(existingHits(in.Metadata), newHits); len(hits) > 0 {
			in.Metadata = annotateRedactions(in.Metadata, hits)
		}
		// memoire:D136 R-2: liaison pre-redacts by contract
		// (docs/LIAISON-EVIDENCE-CONTRACT.md), so a backstop hit on this
		// adapter's path is a contract violation — stamped, never silent.
		if len(newHits) > 0 && in.SourceAdapter == liaisonEvidenceAdapter {
			in.Metadata = annotateContractViolation(in.Metadata)
		}
	}
	if err := in.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.Queue.Pending, 0o700); err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
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
			// Same shape rule as mutateMetaObject: the writer stamps only
			// absent/null/object metadata and never replaces non-object
			// metadata it did not author (round-2 F6).
			metaStampable := meta != nil || envelope["metadata"] == nil
			if meta != nil {
				stripSystemRedactionKeysMap(meta)
			}
			// Universal rule-set stamp, same rule as the CLI path (hit or no
			// hit); the hit branch below re-sets it with the hit list.
			if cfg.RedactionEnabled && metaStampable {
				if meta == nil {
					meta = map[string]any{}
				}
				meta["redaction_rules_version"] = redact.RulesVersion
			}
			// received_at is stamped at HUB arrival (memoire:D152/D153): the
			// receiver's clock is the trusted side of the one-sided future-skew
			// comparison, and a client-stamped value would blind it — a fast
			// client clock skews occurred_at and received_at together (slice-3
			// round-1 finding 3). The producer's own stamp is preserved as
			// metadata.client_received_at_epoch so the capture-buffering signal
			// survives for forensics.
			if prev, ok := envelope["received_at_epoch"]; ok {
				if n, isNum := prev.(json.Number); isNum && n.String() != "0" && metaStampable {
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
		rd, err := redactorFor(cfg)
		if err != nil {
			return Result{}, err
		}
		redacted, hits, err := rd.JSON(json.RawMessage(body))
		if err != nil {
			return Result{}, fmt.Errorf("redact body: %w", err) // fail-closed (redact-v5 §Errors)
		}
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
				if meta == nil && envelope["metadata"] != nil {
					// Non-object metadata: redacted bytes ship unannotated
					// (never replaced); the markers in the body are the record.
					decErr = errors.New("non-object metadata")
				}
			}
			if decErr == nil {
				meta, _ := envelope["metadata"].(map[string]any)
				if meta == nil {
					meta = map[string]any{}
				}
				meta["redactions"] = redact.MergeHits(existingHitsMap(meta), hits)
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
// dictionary attacks. The key is machine-local under MEMOIRE_HOME and is
// loaded fail-closed (redact-v5 ruling Q8, review round 1 F4): a key that
// cannot be read or created fails the write instead of downgrading the
// envelope to plain-hash markers, which a forwarded pending file would carry
// out of the host.
func redactorFor(cfg config.Config) (*redact.Redactor, error) {
	key, err := redact.LoadOrCreateKey(filepath.Join(cfg.Home, "redaction-key"))
	if err != nil {
		return nil, fmt.Errorf("redaction key: %w", err)
	}
	return redact.New(key), nil
}

// liaisonEvidenceAdapter is the source_adapter whose envelopes arrive
// pre-redacted by contract (docs/LIAISON-EVIDENCE-CONTRACT.md).
const liaisonEvidenceAdapter = "liaison-evidence"

// systemRedactionKeys are the metadata keys only the queue writer may set:
// the violation stamp (round-1 finding 4) and the rule-set version (Fibery
// #616 decision 3 — the version is Memoire's attestation of which filter ran
// LAST over this body, never the producer's claim). A producer-supplied value
// is dropped before the filter runs. The hit list (`redactions`) is NOT
// system-owned in that sense: it is cumulative evidence of the markers in the
// body — a pending file redacted on a capture host and forwarded to the hub
// arrives with its local hit list, and the hub's walk (which sees only
// markers) must not erase it; new hits are merged into it. Liaison's own
// declaration lives under a different key (liaison_redaction_rules_version)
// and is untouched.
var systemRedactionKeys = []string{"redaction_contract_violation", "redaction_rules_version"}

// mutateMetaObject is the ONE primitive every metadata stamp and strip goes
// through (slice D simplification, review round 2 → 3). It decodes metadata
// once (UseNumber, so large integers survive bit-exact), applies fn only when
// metadata is absent, JSON null or a JSON object, and re-marshals once.
// Non-object metadata (array, string, number, bool) is returned
// BYTE-IDENTICAL: the writer never replaces content it did not author
// (round-2 F6). Invalid JSON is likewise returned untouched and fails at
// Validate / drain as before. fn reports whether it changed anything so an
// untouched object also stays byte-identical.
func mutateMetaObject(meta json.RawMessage, fn func(m map[string]any) bool) json.RawMessage {
	var m map[string]any
	if len(bytes.TrimSpace(meta)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(meta))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return meta
		}
		if _, trailing := dec.Token(); trailing != io.EOF {
			return meta
		}
		switch t := v.(type) {
		case nil:
			m = map[string]any{}
		case map[string]any:
			m = t
		default:
			return meta
		}
	} else {
		m = map[string]any{}
	}
	if !fn(m) {
		return meta
	}
	out, err := json.Marshal(m)
	if err != nil {
		return meta
	}
	return out
}

// stripSystemRedactionKeys removes producer-supplied system-owned redaction
// keys from metadata. JSON-aware, never a raw-byte probe: a \u0072-escaped
// key (`"\u0072edaction_contract_violation"`) evades byte matching while
// json_extract still resolves it (round-2 finding 4).
func stripSystemRedactionKeys(meta json.RawMessage) json.RawMessage {
	return mutateMetaObject(meta, func(m map[string]any) bool {
		return stripSystemRedactionKeysMap(m)
	})
}

func stripSystemRedactionKeysMap(m map[string]any) bool {
	changed := false
	for _, k := range systemRedactionKeys {
		if _, ok := m[k]; ok {
			delete(m, k)
			changed = true
		}
	}
	return changed
}

// existingHits decodes a producer- or upstream-hop-supplied `redactions`
// list from raw metadata (forwarded pending files carry the capture host's
// hits). Anything that is not a list of {rule,count} yields nil.
func existingHits(meta json.RawMessage) []redact.Hit {
	if len(meta) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(meta, &m); err != nil || m == nil {
		return nil
	}
	return existingHitsMap(m)
}

func existingHitsMap(m map[string]any) []redact.Hit {
	raw, ok := m["redactions"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var hits []redact.Hit
	if err := json.Unmarshal(b, &hits); err != nil {
		return nil
	}
	out := hits[:0]
	for _, h := range hits {
		if h.Rule != "" && h.Count > 0 {
			out = append(out, h)
		}
	}
	return out
}

// annotateRulesVersion stamps the rule-set version that filtered the
// envelope, hit or no hit.
func annotateRulesVersion(meta json.RawMessage) json.RawMessage {
	return mutateMetaObject(meta, func(m map[string]any) bool {
		m["redaction_rules_version"] = redact.RulesVersion
		return true
	})
}

// annotateContractViolation stamps the liaison-evidence contract-violation
// marker (memoire:D136 R-2). The /v1/health violation count reads this via
// the schema/0011 partial index.
func annotateContractViolation(meta json.RawMessage) json.RawMessage {
	return mutateMetaObject(meta, func(m map[string]any) bool {
		m["redaction_contract_violation"] = true
		return true
	})
}

// annotateRedactions records THAT redaction happened (rules + counts +
// rule-set version), never the values, in the envelope metadata.
func annotateRedactions(meta json.RawMessage, hits []redact.Hit) json.RawMessage {
	return mutateMetaObject(meta, func(m map[string]any) bool {
		m["redactions"] = hits
		m["redaction_rules_version"] = redact.RulesVersion
		return true
	})
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
	return queueFileNameAt(time.Now().UnixMilli(), sourceAdapter, body)
}

func queueFileNameAt(ms int64, sourceAdapter string, body []byte) string {
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
	return fmt.Sprintf("%d-%s-%s.json", ms, adapter, hex.EncodeToString(sum[:]))
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

// Batch stages queue items and makes them visible together, with the same
// guarantees as Write per item (byte-identical item, complete file before it
// appears under its final name, file and directory fsynced before the caller
// is told the item is durable) at a fraction of the cost. Two costs cap a
// per-item writer at tens of events per second: the per-item fsync, and the
// redaction walk over every string of the envelope. A batch prepares its items
// concurrently, writes every temp file first, fsyncs them concurrently,
// renames them in order and syncs the directory once.
//
// The redaction key is loaded (or created) once, when the batch is built,
// before any worker starts; every item is redacted with that one key.
//
// Item names carry a millisecond stamp taken in Add order, strictly
// increasing inside a batch, so the name order of the items of a batch is
// their add order (the forwarder sends in name order). Two items with the
// same bytes in one batch therefore get two names, where two Write calls in
// the same millisecond would have collapsed onto one.
//
// Temp names are unique per item (".<name>.tmp.<pid>.<seq>"). Every temp path
// is registered under the mutex before its file exists, so Abort removes the
// temp files of items still being prepared as well as the staged ones. Once
// aborted, a batch creates no further temp file. Not safe for concurrent use;
// Abort may be called from a signal handler goroutine.
type Batch struct {
	cfg      config.Config
	redactor *redact.Redactor
	mu       sync.Mutex
	staged   []stagedItem
	bytes    int64
	seq      atomic.Uint64
	temps    map[string]struct{} // every temp file this batch created and did not yet rename or remove
	aborted  bool
	lastMS   int64
}

type stagedItem struct{ tmp, final, name string }

func NewBatch(cfg config.Config) *Batch {
	b := &Batch{cfg: cfg, temps: map[string]struct{}{}}
	if cfg.RedactionEnabled {
		// A key-load failure is not swallowed: prepareBodyWith retries on
		// every Add and fails the write (fail-closed, ruling Q8).
		if r, err := redactorFor(cfg); err == nil {
			b.redactor = r
		}
	}
	return b
}

// Pending reports how many items and how many bytes wait for Commit.
func (b *Batch) Pending() (items int, bytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.staged), b.bytes
}

// Add stages one item. On error nothing of this item remains staged.
func (b *Batch) Add(in db.EventInput) error {
	n, err := b.AddAll(1, func(int) (db.EventInput, error) { return in, nil })
	if err == nil && n != 1 {
		err = errors.New("batch: item not staged")
	}
	return err
}

type preparedItem struct {
	stagedItem
	size int
	err  error
}

var errBatchAborted = errors.New("batch aborted")

// AddAll stages n items built by build(i), preparing them concurrently. It
// stages the longest prefix that succeeded and returns its length with the
// error of the first item that did not; later items leave nothing behind.
func (b *Batch) AddAll(n int, build func(i int) (db.EventInput, error)) (int, error) {
	res := make([]preparedItem, n)
	stamps := b.nextStamps(n)
	workers := runtime.NumCPU()
	if workers > 16 {
		workers = 16
	}
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				res[i] = b.prepareOne(build, i, stamps[i])
			}
		}()
	}
	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	wg.Wait()

	b.mu.Lock()
	defer b.mu.Unlock()
	added := 0
	var firstErr error
	for i := range res {
		if res[i].err != nil {
			firstErr = res[i].err
			break
		}
		b.staged = append(b.staged, res[i].stagedItem)
		b.bytes += int64(res[i].size)
		added++
	}
	// Items after the cut left a temp file when they were written.
	for i := added; i < n; i++ {
		if res[i].err == nil {
			b.removeTempLocked(res[i].tmp)
		}
	}
	return added, firstErr
}

// nextStamps returns n millisecond stamps, strictly increasing across the
// whole batch and never behind the clock.
func (b *Batch) nextStamps(n int) []int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]int64, n)
	for i := range out {
		ms := time.Now().UnixMilli()
		if ms <= b.lastMS {
			ms = b.lastMS + 1
		}
		b.lastMS = ms
		out[i] = ms
	}
	return out
}

// createTemp registers the temp path and creates the file under the mutex, so
// an Abort either sees the path or has already forbidden the creation.
func (b *Batch) createTemp(path string) (*os.File, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.aborted {
		return nil, errBatchAborted
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	b.temps[path] = struct{}{}
	return f, nil
}

func (b *Batch) removeTempLocked(path string) {
	_ = os.Remove(path)
	delete(b.temps, path)
}

func (b *Batch) removeTemp(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removeTempLocked(path)
}

func (b *Batch) prepareOne(build func(i int) (db.EventInput, error), i int, ms int64) preparedItem {
	in, err := build(i)
	if err != nil {
		return preparedItem{err: err}
	}
	body, err := prepareBodyWith(b.cfg, in, b.redactor)
	if err != nil {
		return preparedItem{err: err}
	}
	name := queueFileNameAt(ms, in.SourceAdapter, body)
	r := preparedItem{size: len(body), stagedItem: stagedItem{
		name:  name,
		final: filepath.Join(b.cfg.Queue.Pending, name),
		tmp:   filepath.Join(b.cfg.Queue.Pending, fmt.Sprintf(".%s.tmp.%d.%d", name, os.Getpid(), b.seq.Add(1))),
	}}
	f, err := b.createTemp(r.tmp)
	if err != nil {
		return preparedItem{err: err}
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		b.removeTemp(r.tmp)
		return preparedItem{err: err}
	}
	if err := f.Close(); err != nil {
		b.removeTemp(r.tmp)
		return preparedItem{err: err}
	}
	return r
}

// Abort removes every temp file the batch created, staged or still being
// prepared (a signal handler's clean exit), and forbids any further one.
func (b *Batch) Abort() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.aborted = true
	for p := range b.temps {
		_ = os.Remove(p)
	}
	b.temps = map[string]struct{}{}
	b.staged, b.bytes = nil, 0
}

// Commit makes the staged items visible. It returns the results of the
// longest durable prefix, in Add order, and the error that stopped the rest
// (whose temp files are removed). The staging area is empty afterwards.
func (b *Batch) Commit() ([]Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	staged := b.staged
	b.staged, b.bytes = nil, 0
	if len(staged) == 0 {
		return nil, nil
	}
	// Phase 1: fsync every temp file, concurrently.
	syncErr := make([]error, len(staged))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i := range staged {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			f, err := os.Open(staged[i].tmp)
			if err != nil {
				syncErr[i] = err
				return
			}
			syncErr[i] = f.Sync()
			if cerr := f.Close(); syncErr[i] == nil {
				syncErr[i] = cerr
			}
		}(i)
	}
	wg.Wait()
	ok := len(staged)
	var firstErr error
	for i, e := range syncErr {
		if e != nil {
			ok, firstErr = i, e
			break
		}
	}
	// Phase 2: rename the durable prefix, in order.
	renamed := 0
	for ; renamed < ok; renamed++ {
		if err := os.Rename(staged[renamed].tmp, staged[renamed].final); err != nil {
			firstErr = err
			break
		}
		delete(b.temps, staged[renamed].tmp)
	}
	// Whatever did not become visible must not leave temp files behind.
	for i := renamed; i < len(staged); i++ {
		b.removeTempLocked(staged[i].tmp)
	}
	if renamed > 0 {
		if err := syncDir(b.cfg.Queue.Pending); err != nil {
			// The renames above already happened: the items are visible in
			// the queue, but the directory entry is not known to be durable,
			// so none is reported. The caller sees no durable item and
			// re-queues the same events; each gets a new name and the hub
			// keeps one per source_event_id. The failure direction is a
			// duplicate file, never a lost event.
			return nil, err
		}
	}
	results := make([]Result, renamed)
	for i := 0; i < renamed; i++ {
		results[i] = Result{Path: staged[i].final, Name: staged[i].name}
	}
	return results, firstErr
}

// StaleTempAge is the age past which SweepStaleTemps may treat a batch temp
// file as the leftover of a dead writer. Age alone never decides: the writer
// named in the file must also be gone (a writer frozen for hours, a laptop
// asleep in the middle of a write, still owns its temp files).
const StaleTempAge = 10 * time.Minute

// StaleSingleTempAge is the age past which SweepStaleTemps removes a
// single-item temp file (".<name>.tmp"). That form carries no pid, so a
// live writer cannot be told from a dead one: only a very old file goes.
const StaleSingleTempAge = 24 * time.Hour

// tempNameRE matches exactly the temp names this package creates: the batch
// form ".<ms>-<adapter>-<sha256>.json.tmp.<pid>.<seq>" (group 1: the pid of
// the writer) and the single-item form ".<ms>-<adapter>-<sha256>.json.tmp".
// A real queue item never starts with a dot.
var tempNameRE = regexp.MustCompile(`^\.[0-9]+-[a-zA-Z0-9._-]+-[0-9a-f]{64}\.json\.tmp(?:\.([0-9]+)\.[0-9]+)?$`)

// processAlive reports whether a process with this pid exists. EPERM means it
// exists (another user's); a pid that does not fit an int32 cannot be one.
func processAlive(pid int) bool {
	if pid <= 0 || pid > math.MaxInt32 {
		return true // not a pid this package wrote: keep the file
	}
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

// SweepStaleTemps removes temp files of earlier writers (killed by SIGKILL, a
// power cut, ...) from the pending directory. A file goes only if its name is
// one of this package's temp names and:
//   - batch form: it was last modified more than olderThan ago AND the process
//     whose pid the name carries no longer exists;
//   - single-item form: it was last modified more than
//     max(olderThan, StaleSingleTempAge) ago.
//
// It never touches a queue item or any other file. It returns how many files
// it removed.
func SweepStaleTemps(pending string, olderThan time.Duration) (int, error) {
	entries, err := os.ReadDir(pending)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	singleAge := StaleSingleTempAge
	if olderThan > singleAge {
		singleAge = olderThan
	}
	now := time.Now()
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := tempNameRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if m[1] == "" {
			if !info.ModTime().Before(now.Add(-singleAge)) {
				continue
			}
		} else {
			pid, perr := strconv.Atoi(m[1])
			if perr != nil || !info.ModTime().Before(now.Add(-olderThan)) || processAlive(pid) {
				continue
			}
		}
		if err := os.Remove(filepath.Join(pending, e.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}
