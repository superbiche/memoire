// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// memoire-pgshadow — slice-5 groundwork harness (memoire:D157,
// docs/SLICE5-GROUNDWORK-DESIGN.md): apply the schema-pg/ DDL to a shadow
// Postgres, populate it (copy lane from SQLite, replay lane from raw/),
// and produce the parity report that is the standing-shadow evidence.
//
//	memoire-pgshadow -pg <url> apply  [-schema-pg DIR]
//	memoire-pgshadow -pg <url> copy   [-db PATH]
//	memoire-pgshadow -pg <url> replay [-raw DIR] [-delta]
//	memoire-pgshadow -pg <url> diff   [-db PATH] [-project P] [-probes FILE]
//	                                  [-replay-lane] [-out REPORT.md]
//
// The Postgres URL may also come from MEMOIRE_PG_URL. diff exits 1 on
// parity FAIL so make targets and rehearsal scripts can gate on it.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/mattn/go-sqlite3"

	"memoire/services/api/internal/pgshadow"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "memoire-pgshadow:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout *os.File) error {
	fs := flag.NewFlagSet("memoire-pgshadow", flag.ContinueOnError)
	pgURL := fs.String("pg", os.Getenv("MEMOIRE_PG_URL"), "postgres URL (or MEMOIRE_PG_URL)")
	home := fs.String("home", defaultHome(), "memoire home (for -db/-raw defaults)")
	dbPath := fs.String("db", "", "sqlite store path (default <home>/memoire.db)")
	rawDir := fs.String("raw", "", "raw custody dir (default <home>/raw)")
	schemaPG := fs.String("schema-pg", "schema-pg", "postgres schema dir")
	project := fs.String("project", "", "diff probe project (default: busiest)")
	probesFile := fs.String("probes", "", "file with one search probe per line")
	replayLane := fs.Bool("replay-lane", false, "diff only replay-covered surfaces")
	delta := fs.Bool("delta", false, "replay: preload shadow keys, skip already-present files")
	outPath := fs.String("out", "", "diff report path (default stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Go's flag parser stops at the first non-flag token, so flags may
	// legally appear on either side of the subcommand: parse, take the
	// subcommand, then parse the rest.
	if fs.NArg() == 0 {
		return fmt.Errorf("subcommand required: apply | copy | replay | diff")
	}
	sub := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments after %s: %v", sub, fs.Args())
	}
	if *pgURL == "" {
		return fmt.Errorf("-pg or MEMOIRE_PG_URL is required")
	}
	if *dbPath == "" {
		*dbPath = filepath.Join(*home, "memoire.db")
	}
	if *rawDir == "" {
		*rawDir = filepath.Join(*home, "raw")
	}

	pg, err := sql.Open("pgx", *pgURL)
	if err != nil {
		return err
	}
	defer pg.Close()
	if err := pg.PingContext(ctx); err != nil {
		return fmt.Errorf("postgres unreachable: %w", err)
	}

	switch sub {
	case "apply":
		if err := pgshadow.Apply(ctx, pg, *schemaPG); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "applied", *schemaPG)
		return nil
	case "copy":
		lite, err := openSQLiteRO(*dbPath)
		if err != nil {
			return err
		}
		defer lite.Close()
		counts, err := pgshadow.Copy(ctx, pg, lite)
		if err != nil {
			return err
		}
		for name, n := range counts {
			fmt.Fprintf(stdout, "copied %s: %d\n", name, n)
		}
		return nil
	case "replay":
		stats, err := pgshadow.Replay(ctx, pg, *rawDir, *delta)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "replayed files=%d skipped=%d ingested=%d duplicates=%d failed=%d\n",
			stats.Files, stats.Skipped, stats.Ingested, stats.Duplicates, stats.Failed)
		return nil
	case "diff":
		lite, err := openSQLiteRO(*dbPath)
		if err != nil {
			return err
		}
		defer lite.Close()
		out := stdout
		if *outPath != "" {
			f, err := os.Create(*outPath)
			if err != nil {
				return err
			}
			defer f.Close()
			out = f
		}
		opts := pgshadow.DiffOptions{Project: *project, ReplayLane: *replayLane}
		if *probesFile != "" {
			body, err := os.ReadFile(*probesFile)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(string(body), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					opts.Probes = append(opts.Probes, line)
				}
			}
		}
		pass, err := pgshadow.Diff(ctx, pg, lite, opts, out)
		if err != nil {
			return err
		}
		if !pass {
			return fmt.Errorf("parity FAIL (see report)")
		}
		fmt.Fprintln(os.Stderr, "parity PASS")
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q", sub)
	}
}

func openSQLiteRO(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return sql.Open("sqlite3", "file:"+path+"?mode=ro&_busy_timeout=5000&_query_only=true")
}

func defaultHome() string {
	if h := os.Getenv("MEMOIRE_HOME"); h != "" {
		return h
	}
	return filepath.Join(os.Getenv("HOME"), ".memoire")
}
