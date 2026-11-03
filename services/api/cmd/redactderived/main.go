// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// memoire-redact-derived — apply the current redaction ruleset to the derived stores
// (memoire:D144 residual; see internal/redactderived). DRY-RUN by default.
// Apply is double-gated: --apply AND MEMOIRE_REDACT_DERIVED_APPLY=yes, and
// refuses while a memoire-derive process is alive
// (MEMOIRE_REDACT_DERIVED_SKIP_QUIESCE=yes for hermetic fixtures only).
// Operator runbook: stop memoire.service around --apply so the drainer's
// writes cannot queue behind the big IMMEDIATE transaction and poison.
//
// Postgres hub (redact-v5 S2a): --db postgres://… (or a postgres DSN in
// MEMOIRE_DB) selects the Postgres engine — the store is opened raw, no
// migrations or DDL, and every run rides ONE REPEATABLE READ transaction
// (apply: all-or-nothing; dry-run: READ ONLY). The process check is
// meaningless there (the API runs in another pod), so a Postgres apply
// instead refuses without the explicit operator attestation
// --i-have-quiesced-the-hub (RUNBOOK "Re-sweep on the Postgres hub"). The
// report names the store by its password-redacted DSN; the DSN password
// never reaches stdout, stderr or the report file.
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

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/mattn/go-sqlite3"

	memdb "memoire/services/api/internal/db"
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
	var hubQuiesced bool
	fs.StringVar(&opts.DBPath, "db", "", "store: SQLite path or postgres:// DSN (default: a postgres DSN in MEMOIRE_DB, else <home>/memoire.db)")
	fs.StringVar(&opts.RedactionKeyPath, "redaction-key", home+"/.memoire/redaction-key", "redaction HMAC key path")
	fs.BoolVar(&opts.Apply, "apply", false, "apply changes (also requires MEMOIRE_REDACT_DERIVED_APPLY=yes)")
	fs.BoolVar(&hubQuiesced, "i-have-quiesced-the-hub", false, "Postgres only: attest the hub is quiesced (no API/derive work against the store; required for --apply)")
	fs.StringVar(&reportPath, "report", "", "optional machine-readable JSON report path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// MEMOIRE_DB is consulted only for a Postgres DSN (the hub pods carry
	// it from the memoire-pg-app secret): an explicit --db wins, and the
	// default otherwise stays <home>/memoire.db (a non-postgres MEMOIRE_DB
	// is ignored).
	switch {
	case opts.DBPath != "":
	case memdb.IsPostgresDSN(os.Getenv("MEMOIRE_DB")):
		opts.DBPath = os.Getenv("MEMOIRE_DB")
	default:
		opts.DBPath = filepath.Join(home, ".memoire", "memoire.db")
	}
	isPG := memdb.IsPostgresDSN(opts.DBPath)
	if hubQuiesced && !isPG {
		return errors.New("--i-have-quiesced-the-hub applies only to a Postgres store; the SQLite engine checks for a live memoire-derive process itself")
	}

	if !isPG {
		if _, err := os.Stat(opts.DBPath); err != nil {
			return fmt.Errorf("database not found: %s", opts.DBPath)
		}
	}

	if opts.Apply {
		if os.Getenv("MEMOIRE_REDACT_DERIVED_APPLY") != "yes" {
			return errors.New("REFUSING --apply: MEMOIRE_REDACT_DERIVED_APPLY=yes not set (double gate)")
		}
		if isPG {
			// No process check across pods: explicit operator attestation
			// (same model as memoire-purge's Postgres gate).
			if !hubQuiesced {
				return errors.New("REFUSING --apply on Postgres: pass --i-have-quiesced-the-hub after quiescing the hub — no API or derive work against the store (RUNBOOK \"Re-sweep on the Postgres hub\")")
			}
		} else if os.Getenv("MEMOIRE_REDACT_DERIVED_SKIP_QUIESCE") != "yes" {
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
