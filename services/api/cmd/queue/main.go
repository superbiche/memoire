// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
	"memoire/services/api/internal/queuewriter"
)

// parseError marks a flag-parsing failure: the flag package already printed
// the message and usage, and the historical exit code is 2 (ExitOnError).
type parseError struct{ err error }

func (e parseError) Error() string { return e.err.Error() }
func (e parseError) Unwrap() error { return e.err }

func main() {
	err := run(context.Background(), os.Args[1:], os.Stdout)
	if err == nil {
		return
	}
	// Same exit codes as flag.ExitOnError: -h is 0, a bad flag is 2.
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	var pe parseError
	if errors.As(err, &pe) {
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "memoire-queue:", err.Error())
	os.Exit(1)
}

func run(_ context.Context, args []string, stdout io.Writer) error {
	var in db.EventInput
	var payloadJSON, payloadFile, metadataJSON, metadataFile string
	var textFile, batchFile string
	fs := flag.NewFlagSet("memoire-queue", flag.ContinueOnError)
	fs.StringVar(&in.Project, "project", "", "project name")
	fs.StringVar(&in.SessionID, "session-id", "", "session identifier")
	fs.StringVar(&in.SourceAdapter, "source-adapter", "manual", "producer or harness name")
	fs.StringVar(&in.SourceEventID, "source-event-id", "", "source event identifier; generated when omitted")
	fs.StringVar(&in.EventType, "event-type", "note", "event type")
	fs.StringVar(&in.CWD, "cwd", "", "working directory")
	fs.StringVar(&in.Text, "text", "", "search-visible text")
	// -text-file carries the same text as -text but through a file: a turn
	// larger than the argv limit cannot ride the command line, so capture
	// adapters use it for turn events.
	fs.StringVar(&textFile, "text-file", "", "path to a file holding the search-visible text")
	fs.StringVar(&payloadJSON, "payload-json", "", "payload JSON object")
	fs.StringVar(&payloadFile, "payload-file", "", "path to payload JSON object")
	fs.StringVar(&metadataJSON, "metadata-json", "", "metadata JSON object")
	fs.StringVar(&metadataFile, "metadata-file", "", "path to metadata JSON object")
	fs.StringVar(&in.PrincipalID, "principal-id", "", "principal id")
	fs.StringVar(&in.OccurredAt, "occurred-at", "", "occurred-at timestamp (RFC3339); defaults to now")
	fs.Int64Var(&in.OccurredAtEpoch, "occurred-at-epoch", 0, "occurred-at unix epoch seconds; defaults to now")
	// -batch-file writes many events in one process: one JSON object per line
	// (fields below), one queue item per line, in file order, exactly as if
	// the CLI had been called once per line. The per-call flags above act as
	// defaults for the fields a line omits. Stops at the first failing line.
	fs.StringVar(&batchFile, "batch-file", "", "path to a JSONL file of events (project, session_id, source_adapter, event_type, source_event_id, cwd, occurred_at, text, payload, project_root, project_source); one queue item per line")
	var projectRoot, projectSource string
	fs.StringVar(&projectRoot, "project-root", "", "resolved project root dir (identity conflict detection; merged into payload)")
	fs.StringVar(&projectSource, "project-source", "", "project resolution source: env|memoire-file|registry|basename (merged into payload)")
	if err := fs.Parse(args); err != nil {
		return parseError{err}
	}
	textSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "text" {
			textSet = true
		}
	})

	if batchFile != "" {
		if textFile != "" || textSet || payloadJSON != "" || payloadFile != "" ||
			metadataJSON != "" || metadataFile != "" || in.SourceEventID != "" {
			return fmt.Errorf("-batch-file takes text, payload and source-event-id per line; do not combine it with -text, -text-file, -payload-json, -payload-file, -metadata-* or -source-event-id")
		}
		return runBatch(batchFile, in, projectRoot, projectSource, stdout)
	}

	if in.Project == "" {
		return fmt.Errorf("missing required -project")
	}
	if in.OccurredAt != "" && in.OccurredAtEpoch != 0 {
		return fmt.Errorf("use only one of -occurred-at or -occurred-at-epoch")
	}
	if in.OccurredAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, in.OccurredAt); err != nil {
			return fmt.Errorf("invalid -occurred-at, expected RFC3339: %w", err)
		}
	}
	if in.OccurredAtEpoch < 0 {
		return fmt.Errorf("-occurred-at-epoch must be positive")
	}
	if in.SessionID == "" {
		in.SessionID = fmt.Sprintf("%s-%d", in.SourceAdapter, time.Now().Unix())
	}
	if payloadJSON != "" && payloadFile != "" {
		return fmt.Errorf("use only one of -payload-json or -payload-file")
	}
	if metadataJSON != "" && metadataFile != "" {
		return fmt.Errorf("use only one of -metadata-json or -metadata-file")
	}
	if textSet && textFile != "" {
		return fmt.Errorf("use only one of -text or -text-file")
	}
	if textFile != "" {
		b, err := os.ReadFile(textFile)
		if err != nil {
			return fmt.Errorf("read -text-file: %w", err)
		}
		in.Text = string(b)
	}
	payload, err := payloadFromFlags(payloadJSON, payloadFile, in.Text)
	if err != nil {
		return err
	}
	// Identity-conflict detection (liaison:D239 item 4): the resolved project root and
	// resolution source ride the payload — client-specific detail, never a
	// top-level schema column. Merged here so every adapter gets it with two
	// flags instead of editing each payload construction site.
	if projectRoot != "" {
		payload, err = mergePayloadIdentity(payload, projectRoot, projectSource)
		if err != nil {
			return err
		}
	}
	meta, err := metadataFromFlags(metadataJSON, metadataFile)
	if err != nil {
		return err
	}
	in.Payload = payload
	in.Metadata = meta

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}
	res, err := queuewriter.Write(context.Background(), cfg, in)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, res.Path)
	return nil
}

// A batch is prepared, fsynced and renamed in chunks.
const (
	batchChunkItems = 256
	batchChunkBytes = 64 << 20
)

// batchLine is one event of a -batch-file. Absent fields keep the value the
// command-line flags gave (project, session id, adapter, cwd, ...).
type batchLine struct {
	Project       string          `json:"project"`
	SessionID     string          `json:"session_id"`
	SourceAdapter string          `json:"source_adapter"`
	SourceEventID string          `json:"source_event_id"`
	EventType     string          `json:"event_type"`
	CWD           string          `json:"cwd"`
	OccurredAt    string          `json:"occurred_at"`
	Text          string          `json:"text"`
	Payload       json.RawMessage `json:"payload"`
	ProjectRoot   string          `json:"project_root"`
	ProjectSource string          `json:"project_source"`
}

// buildBatchInput turns one line into the event a per-event call would give.
func buildBatchInput(raw []byte, defaults db.EventInput, projectRoot, projectSource string) (db.EventInput, error) {
	line := batchLine{
		Project:       defaults.Project,
		SessionID:     defaults.SessionID,
		SourceAdapter: defaults.SourceAdapter,
		EventType:     defaults.EventType,
		CWD:           defaults.CWD,
		OccurredAt:    defaults.OccurredAt,
		ProjectRoot:   projectRoot,
		ProjectSource: projectSource,
	}
	if err := json.Unmarshal(raw, &line); err != nil {
		return db.EventInput{}, err
	}
	in := defaults
	in.Project, in.SessionID, in.SourceAdapter = line.Project, line.SessionID, line.SourceAdapter
	in.SourceEventID, in.EventType, in.CWD = line.SourceEventID, line.EventType, line.CWD
	in.OccurredAt, in.Text = line.OccurredAt, line.Text
	if in.Project == "" {
		return in, fmt.Errorf("missing project")
	}
	if in.OccurredAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, in.OccurredAt); err != nil {
			return in, fmt.Errorf("invalid occurred_at, expected RFC3339: %w", err)
		}
	}
	if in.SessionID == "" {
		in.SessionID = fmt.Sprintf("%s-%d", in.SourceAdapter, time.Now().Unix())
	}
	payload := line.Payload
	var err error
	if len(payload) == 0 || string(payload) == "null" {
		payload = queuewriter.PayloadFromText(in.Text)
	} else if payload, err = queuewriter.ParseJSONObject(string(payload), "payload"); err != nil {
		return in, err
	}
	if line.ProjectRoot != "" {
		if payload, err = mergePayloadIdentity(payload, line.ProjectRoot, line.ProjectSource); err != nil {
			return in, err
		}
	}
	in.Payload = payload
	in.Metadata = json.RawMessage(`{}`)
	return in, nil
}

// runBatch writes every line of the JSONL file as one queue item, in order,
// through queuewriter.Batch (same envelope bytes as a single call, same
// redaction, one pending file per event; the fsync cost is amortised over
// chunks and the redaction work is spread over the cores). One path is
// printed per DURABLE item, so a caller that sees a failure knows exactly how
// many lines are durable: the first N lines when N paths were printed. The
// reader has no line-length limit: a single event may carry many megabytes.
func runBatch(path string, defaults db.EventInput, projectRoot, projectSource string, stdout io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read -batch-file: %w", err)
	}
	defer f.Close()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}
	// Temp files of earlier batches that a SIGKILL or a power cut left behind
	// hold conversation content under the queue directory: remove them before
	// staging new ones.
	// Only the temp files of a writer that no longer exists go (see
	// SweepStaleTemps). MEMOIRE_QUEUE_STALE_TEMP_S shortens the age bound; it
	// exists for the tests.
	staleAge := queuewriter.StaleTempAge
	if v, err := strconv.Atoi(os.Getenv("MEMOIRE_QUEUE_STALE_TEMP_S")); err == nil && v > 0 {
		staleAge = time.Duration(v) * time.Second
	}
	_, _ = queuewriter.SweepStaleTemps(cfg.Queue.Pending, staleAge)
	batch := queuewriter.NewBatch(cfg)
	// A signal removes every temp file of the batch, staged or still being
	// prepared, instead of leaking them; what a chunk had already made durable
	// stays durable and printed.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		s := <-sig
		batch.Abort()
		switch s {
		case syscall.SIGINT:
			os.Exit(130)
		case syscall.SIGHUP:
			os.Exit(129)
		}
		os.Exit(143)
	}()
	defer signal.Stop(sig)

	r := bufio.NewReaderSize(f, 1<<20)
	n := 0 // lines consumed so far
	for {
		var raws [][]byte
		var size int
		eof := false
		for len(raws) < batchChunkItems && size < batchChunkBytes {
			raw, rerr := r.ReadBytes('\n')
			if len(raw) > 0 {
				raws = append(raws, raw)
				size += len(raw)
			}
			if rerr == io.EOF {
				eof = true
				break
			}
			if rerr != nil {
				return fmt.Errorf("read -batch-file: %w", rerr)
			}
		}
		added, aerr := batch.AddAll(len(raws), func(i int) (db.EventInput, error) {
			in, err := buildBatchInput(raws[i], defaults, projectRoot, projectSource)
			if err != nil {
				return in, fmt.Errorf("batch line %d: %w", n+i+1, err)
			}
			return in, nil
		})
		res, cerr := batch.Commit()
		for _, r := range res {
			fmt.Fprintln(stdout, r.Path)
		}
		if aerr != nil {
			return aerr
		}
		if cerr != nil {
			return cerr
		}
		if len(res) != added {
			return fmt.Errorf("batch: %d of %d staged items durable", len(res), added)
		}
		n += len(raws)
		if eof {
			return nil
		}
	}
}

func payloadFromFlags(raw, file, text string) (json.RawMessage, error) {
	switch {
	case file != "":
		return queuewriter.ReadJSONObjectFile(file, "payload")
	case strings.TrimSpace(raw) != "":
		return queuewriter.ParseJSONObject(raw, "payload")
	default:
		return queuewriter.PayloadFromText(text), nil
	}
}

func mergePayloadIdentity(payload json.RawMessage, root, source string) (json.RawMessage, error) {
	obj := map[string]any{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &obj); err != nil {
			return nil, fmt.Errorf("payload must be a JSON object to merge -project-root: %w", err)
		}
	}
	obj["memoire_project_root"] = root
	if source != "" {
		obj["memoire_project_source"] = source
	}
	return json.Marshal(obj)
}

func metadataFromFlags(raw, file string) (json.RawMessage, error) {
	switch {
	case file != "":
		return queuewriter.ReadJSONObjectFile(file, "metadata")
	default:
		return queuewriter.ParseJSONObject(raw, "metadata")
	}
}
