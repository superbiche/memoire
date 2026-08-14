// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
	"memoire/services/api/internal/queuewriter"
)

func main() {
	var in db.EventInput
	var payloadJSON, payloadFile, metadataJSON, metadataFile string
	flag.StringVar(&in.Project, "project", "", "project name")
	flag.StringVar(&in.SessionID, "session-id", "", "session identifier")
	flag.StringVar(&in.SourceAdapter, "source-adapter", "manual", "producer or harness name")
	flag.StringVar(&in.SourceEventID, "source-event-id", "", "source event identifier; generated when omitted")
	flag.StringVar(&in.EventType, "event-type", "note", "event type")
	flag.StringVar(&in.CWD, "cwd", "", "working directory")
	flag.StringVar(&in.Text, "text", "", "search-visible text")
	flag.StringVar(&payloadJSON, "payload-json", "", "payload JSON object")
	flag.StringVar(&payloadFile, "payload-file", "", "path to payload JSON object")
	flag.StringVar(&metadataJSON, "metadata-json", "", "metadata JSON object")
	flag.StringVar(&metadataFile, "metadata-file", "", "path to metadata JSON object")
	flag.StringVar(&in.PrincipalID, "principal-id", "", "principal id")
	flag.StringVar(&in.OccurredAt, "occurred-at", "", "occurred-at timestamp (RFC3339); defaults to now")
	flag.Int64Var(&in.OccurredAtEpoch, "occurred-at-epoch", 0, "occurred-at unix epoch seconds; defaults to now")
	var projectRoot, projectSource string
	flag.StringVar(&projectRoot, "project-root", "", "resolved project root dir (identity conflict detection; merged into payload)")
	flag.StringVar(&projectSource, "project-source", "", "project resolution source: env|memoire-file|registry|basename (merged into payload)")
	flag.Parse()

	if in.Project == "" {
		die("missing required -project")
	}
	if in.OccurredAt != "" && in.OccurredAtEpoch != 0 {
		die("use only one of -occurred-at or -occurred-at-epoch")
	}
	if in.OccurredAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, in.OccurredAt); err != nil {
			die("invalid -occurred-at, expected RFC3339: " + err.Error())
		}
	}
	if in.OccurredAtEpoch < 0 {
		die("-occurred-at-epoch must be positive")
	}
	if in.SessionID == "" {
		in.SessionID = fmt.Sprintf("%s-%d", in.SourceAdapter, time.Now().Unix())
	}
	if payloadJSON != "" && payloadFile != "" {
		die("use only one of -payload-json or -payload-file")
	}
	if metadataJSON != "" && metadataFile != "" {
		die("use only one of -metadata-json or -metadata-file")
	}
	payload, err := payloadFromFlags(payloadJSON, payloadFile, in.Text)
	if err != nil {
		die(err.Error())
	}
	// Identity-conflict detection (liaison:D239 item 4): the resolved project root and
	// resolution source ride the payload — client-specific detail, never a
	// top-level schema column. Merged here so every adapter gets it with two
	// flags instead of editing each payload construction site.
	if projectRoot != "" {
		payload, err = mergePayloadIdentity(payload, projectRoot, projectSource)
		if err != nil {
			die(err.Error())
		}
	}
	meta, err := metadataFromFlags(metadataJSON, metadataFile)
	if err != nil {
		die(err.Error())
	}
	in.Payload = payload
	in.Metadata = meta

	cfg, err := config.Load()
	if err != nil {
		die(err.Error())
	}
	if err := cfg.EnsureDirs(); err != nil {
		die(err.Error())
	}
	res, err := queuewriter.Write(context.Background(), cfg, in)
	if err != nil {
		die(err.Error())
	}
	fmt.Println(res.Path)
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

func die(msg string) {
	fmt.Fprintln(os.Stderr, "memoire-queue:", msg)
	os.Exit(1)
}
