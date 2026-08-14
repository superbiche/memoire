// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"memoire/services/api/internal/sweep"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "memoire-sweep:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("memoire-sweep", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var opts sweep.Options
	fs.StringVar(&opts.RawDir, "raw-dir", "", "flat directory of raw event JSON files")
	fs.StringVar(&opts.OutputDir, "output-dir", "", "empty directory where redacted raw generation is written")
	fs.StringVar(&opts.QuarantineOut, "quarantine-out", "", "age-encrypted tar archive path for byte-exact originals")
	fs.StringVar(&opts.AgeRecipient, "age-recipient", "", "age recipient used to encrypt the quarantine archive")
	fs.StringVar(&opts.ReportPath, "report", "", "machine-readable JSON sweep report path")
	fs.StringVar(&opts.RedactionKeyPath, "redaction-key", "", "redaction HMAC key path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts.EvidencePath = sweep.DefaultEvidencePath
	if err := sweep.ValidateOptions(opts); err != nil {
		return err
	}

	report, err := sweep.Run(context.Background(), opts)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}
