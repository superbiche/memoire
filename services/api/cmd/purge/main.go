// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// memoire-purge — hard purge (liaison:D97 decision 4, memoire:D147 slice 1;
// see internal/purge). DRY-RUN by default. Apply is double-gated: --apply
// AND MEMOIRE_PURGE_APPLY=yes, and refuses while a memoire-derive OR
// memoire-api process is alive (the API's drainer copies queue files into
// raw/ and its writer would queue behind the purge transaction —
// MEMOIRE_PURGE_SKIP_QUIESCE=yes is for hermetic test fixtures only, never
// the live store). Operator runbook: stop memoire.service around --apply.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"memoire/services/api/internal/purge"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "memoire-purge:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("memoire-purge", flag.ContinueOnError)
	fs.SetOutput(stderr)

	defHome := os.Getenv("MEMOIRE_HOME")
	if defHome == "" {
		userHome, _ := os.UserHomeDir()
		defHome = filepath.Join(userHome, ".memoire")
	}

	var (
		home           string
		project        string
		eventIDs       string
		observationIDs string
		resumeIntent   bool
		reportPath     string
		opts           purge.Options
	)
	fs.StringVar(&home, "home", defHome, "Memoire home (db, raw/, queue/ derived from it)")
	fs.StringVar(&project, "project", "", "purge everything captured under this project key")
	fs.StringVar(&eventIDs, "event-ids", "", "comma-separated event ids to purge (with linked observations)")
	fs.StringVar(&observationIDs, "observation-ids", "", "comma-separated observation ids to purge (DB-only)")
	fs.BoolVar(&opts.Apply, "apply", false, "apply the purge (also requires MEMOIRE_PURGE_APPLY=yes)")
	fs.BoolVar(&opts.NoQuarantine, "no-quarantine", false, "skip the age-encrypted quarantine archive (direct destruction)")
	fs.StringVar(&opts.AgeRecipient, "age-recipient", "", "age recipient for the quarantine archive")
	fs.StringVar(&opts.QuarantineOut, "quarantine-out", "", "quarantine archive path (default <home>/quarantine-purge-<epoch>.tar.age)")
	fs.BoolVar(&opts.Vacuum, "vacuum", true, "VACUUM after deletion so freed pages leave the file")
	fs.BoolVar(&resumeIntent, "resume-intent", false, "finish file deletions from a leftover intent journal")
	fs.StringVar(&reportPath, "report", "", "optional machine-readable JSON report path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	opts.DBPath = filepath.Join(home, "memoire.db")
	opts.RawDir = filepath.Join(home, "raw")
	opts.PendingDir = filepath.Join(home, "queue", "pending")
	opts.InflightDir = filepath.Join(home, "queue", "inflight")
	opts.FailedDir = filepath.Join(home, "queue", "failed")
	opts.IntentPath = filepath.Join(home, "purge-intent.json")
	opts.ManifestPath = filepath.Join(home, "custody", "MANIFEST.json")

	// Resume IS destructive (it deletes the intent's file list) — it rides
	// the identical gate as apply: --apply AND the env var (review finding
	// 5, round 1).
	if resumeIntent && !opts.Apply {
		return errors.New("REFUSING --resume-intent without --apply: resume deletes files and is gated identically to apply (double gate)")
	}
	if opts.Apply {
		if os.Getenv("MEMOIRE_PURGE_APPLY") != "yes" {
			return errors.New("REFUSING destructive mode: MEMOIRE_PURGE_APPLY=yes not set (double gate)")
		}
		if os.Getenv("MEMOIRE_PURGE_SKIP_QUIESCE") == "yes" {
			// Fixture escape hatch (same pattern as memoire-redact-derived,
			// D145) — loud so it can never pass silently in operator use.
			fmt.Fprintln(stderr, "WARNING: MEMOIRE_PURGE_SKIP_QUIESCE=yes — quiesce guard BYPASSED. This is for hermetic test fixtures ONLY; never run this against the live store.")
		} else {
			for _, proc := range []string{"memoire-derive", "memoire-api"} {
				out, err := exec.Command("pgrep", "-af", proc).Output()
				if err == nil && len(out) > 0 {
					fmt.Fprintf(stderr, "%s", out)
					return fmt.Errorf("REFUSING destructive mode: a %s process is running — quiesce first", proc)
				}
			}
		}
	}

	if resumeIntent {
		report, err := purge.ResumeIntent(context.Background(), opts)
		if err != nil {
			return err
		}
		return emit(report, stdout, reportPath)
	}

	var err error
	opts.Selector.EventIDs, err = parseIDs(eventIDs)
	if err != nil {
		return fmt.Errorf("--event-ids: %w", err)
	}
	opts.Selector.ObservationIDs, err = parseIDs(observationIDs)
	if err != nil {
		return fmt.Errorf("--observation-ids: %w", err)
	}
	opts.Selector.Project = project

	if opts.Apply && !opts.NoQuarantine && opts.QuarantineOut == "" {
		opts.QuarantineOut = filepath.Join(home, fmt.Sprintf("quarantine-purge-%d.tar.age", time.Now().Unix()))
	}
	if _, err := os.Stat(opts.DBPath); err != nil {
		return fmt.Errorf("database not found: %s", opts.DBPath)
	}

	report, err := purge.Run(context.Background(), opts)
	if err != nil {
		return err
	}
	if err := emit(report, stdout, reportPath); err != nil {
		return err
	}
	if !opts.Apply {
		fmt.Fprintln(stderr, "dry-run: no changes made (use --apply with MEMOIRE_PURGE_APPLY=yes)")
	}
	return nil
}

func parseIDs(s string) ([]int64, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad id %q", p)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func emit(report purge.Report, stdout io.Writer, reportPath string) error {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return err
	}
	if reportPath != "" {
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(reportPath, append(b, '\n'), 0o600); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
	}
	return nil
}
