// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// memoire-redact-derived — apply redact-v1 to the derived stores
// (memoire:D144 residual; see internal/redactderived). DRY-RUN by default.
// Apply is double-gated: --apply AND MEMOIRE_REDACT_DERIVED_APPLY=yes, and
// refuses while a memoire-derive process is alive
// (MEMOIRE_REDACT_DERIVED_SKIP_QUIESCE=yes for hermetic fixtures only).
// Operator runbook: stop memoire.service around --apply so the drainer's
// writes cannot queue behind the big IMMEDIATE transaction and poison.
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
	"strings"

	_ "github.com/mattn/go-sqlite3"

	"memoire/services/api/internal/redactderived"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "memoire-redact-derived:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("memoire-redact-derived", flag.ContinueOnError)
	fs.SetOutput(stderr)

	home, _ := os.UserHomeDir()
	var opts redactderived.Options
	var reportPath string
	fs.StringVar(&opts.DBPath, "db", home+"/.memoire/memoire.db", "SQLite database path")
	fs.StringVar(&opts.RedactionKeyPath, "redaction-key", home+"/.memoire/redaction-key", "redaction HMAC key path")
	fs.BoolVar(&opts.Apply, "apply", false, "apply changes (also requires MEMOIRE_REDACT_DERIVED_APPLY=yes)")
	fs.StringVar(&reportPath, "report", "", "optional machine-readable JSON report path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.HasPrefix(opts.DBPath, "postgres://") || strings.HasPrefix(opts.DBPath, "postgresql://") {
		// Dialect-seam residual (slice-5 execution round-1 finding 4): the
		// derived-redaction path is SQLite-file-only today; its Postgres port
		// is post-cutover work. Reject the DSN loudly instead of failing an
		// os.Stat on a URL.
		return errors.New("memoire-redact-derived is SQLite-only: -db must be a database FILE, not a postgres:// DSN (PG port tracked as a post-cutover residual)")
	}
	if _, err := os.Stat(opts.DBPath); err != nil {
		return fmt.Errorf("database not found: %s", opts.DBPath)
	}

	if opts.Apply {
		if os.Getenv("MEMOIRE_REDACT_DERIVED_APPLY") != "yes" {
			return errors.New("REFUSING --apply: MEMOIRE_REDACT_DERIVED_APPLY=yes not set (double gate)")
		}
		if os.Getenv("MEMOIRE_REDACT_DERIVED_SKIP_QUIESCE") != "yes" {
			out, err := exec.Command("pgrep", "-af", "memoire-derive").Output()
			if err == nil && len(out) > 0 {
				fmt.Fprintf(stderr, "%s", out)
				return errors.New("REFUSING --apply: a memoire-derive process is running — quiesce derivation first")
			}
		}
	}

	report, err := redactderived.Run(context.Background(), opts)
	if err != nil {
		return err
	}
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
	if !opts.Apply {
		fmt.Fprintln(stderr, "dry-run: no changes made (use --apply with MEMOIRE_REDACT_DERIVED_APPLY=yes)")
	}
	return nil
}
