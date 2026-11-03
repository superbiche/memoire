// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// memoire-raw-pack — raw/ cold-archive compaction (liaison:D97 decision 2;
// internal/rawstore). Packs the flat envelopes of closed UTC days into new,
// immutable parts raw/pack-YYYY-MM-DD.NNN.tar.zst, verifies each part, then
// deletes the flat files it provably holds. DRY-RUN by default; --apply
// writes. --verify walks the whole store (flat and packed) under the custody
// lock and prints a digest, for restore and migration checks. --unpack PART
// is the rollback of one part: it advances the custody generation first (so
// the snapshot re-baselines before the part name can be reused), restores
// the members as flat files and removes the part.
//
// Concurrency: purge, Replay, the retro-sweep and the snapshot script take
// the same raw/ custody lock. The drainer publishes complete raw files only
// (staged in IncomingDir, then linked into raw/), so a file the packer reads
// is never still being written. The packer refuses while a purge intent
// journal exists (--purge-intent), so it never relocates a file a crashed
// purge still has to delete.
//
// Output: one JSON report on stdout (counts and day names, never content).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"memoire/services/api/internal/rawstore"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, time.Now); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "memoire-raw-pack:", err)
		os.Exit(1)
	}
}

type verifyReport struct {
	Envelopes int    `json:"envelopes"`
	Packed    int    `json:"packed"`
	Flat      int    `json:"flat"`
	Digest    string `json:"digest"` // sha256 over "name\0sha256(body)\n" in walk order
}

func run(args []string, stdout, stderr io.Writer, now func() time.Time) error {
	fs := flag.NewFlagSet("memoire-raw-pack", flag.ContinueOnError)
	fs.SetOutput(stderr)

	defHome := os.Getenv("MEMOIRE_HOME")
	if defHome == "" {
		userHome, _ := os.UserHomeDir()
		defHome = filepath.Join(userHome, ".memoire")
	}
	var (
		home        string
		rawDir      string
		purgeIntent string
		manifest    string
		unpack      string
		verify      bool
		opts        rawstore.PackOptions
	)
	fs.StringVar(&home, "home", defHome, "Memoire home (raw/ and purge-intent.json derived from it)")
	fs.StringVar(&rawDir, "raw-dir", "", "raw custody dir (default <home>/raw)")
	fs.StringVar(&purgeIntent, "purge-intent", "", "purge intent journal that blocks packing while present (default <home>/purge-intent.json)")
	fs.IntVar(&opts.GraceDays, "grace-days", 2, "UTC days before today that stay flat (minimum 1)")
	fs.IntVar(&opts.MaxParts, "max-parts", 0, "write at most N parts this run, oldest days first (0 = all)")
	fs.IntVar(&opts.MaxSeconds, "max-seconds", 0, "stop between parts after N seconds (0 = no bound)")
	fs.IntVar(&opts.MaxFilesPerPart, "max-files-per-part", rawstore.DefaultMaxFilesPerPart, "members per part")
	fs.BoolVar(&opts.Apply, "apply", false, "write parts and delete the packed flat files")
	fs.BoolVar(&verify, "verify", false, "walk the whole store under the custody lock and print a content digest; writes nothing")
	fs.StringVar(&unpack, "unpack", "", "rollback: restore part NAME (pack-YYYY-MM-DD.NNN.tar.zst) as flat files and remove it")
	fs.StringVar(&manifest, "manifest", "", "custody manifest whose generation --unpack advances (default <home>/custody/MANIFEST.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if rawDir == "" {
		rawDir = filepath.Join(home, "raw")
	}
	if purgeIntent == "" {
		purgeIntent = filepath.Join(home, "purge-intent.json")
	}
	if manifest == "" {
		manifest = filepath.Join(home, "custody", "MANIFEST.json")
	}
	if _, err := os.Stat(rawDir); err != nil {
		return fmt.Errorf("raw dir: %w", err)
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")

	if unpack != "" {
		if verify || opts.Apply {
			return errors.New("--unpack is exclusive with --verify and --apply")
		}
		rep, err := rawstore.Unpack(rawDir, unpack, manifest, []string{purgeIntent})
		if encErr := enc.Encode(rep); encErr != nil && err == nil {
			err = encErr
		}
		return err
	}

	if verify {
		if opts.Apply {
			return errors.New("--verify and --apply are exclusive")
		}
		// Review N2: the digest must describe one inventory, so no packer or
		// purge may move files under the walk.
		sess, err := rawstore.Custody{RawDir: rawDir, Intents: []string{purgeIntent}}.Enter(rawstore.ModeRead)
		if err != nil {
			return err
		}
		defer sess.Release()
		var rep verifyReport
		h := sha256.New()
		err = rawstore.Walk(rawDir, func(e rawstore.Entry, body []byte) error {
			rep.Envelopes++
			if e.Pack != "" {
				rep.Packed++
			} else {
				rep.Flat++
			}
			sum := sha256.Sum256(body)
			fmt.Fprintf(h, "%s\x00%x\n", e.Name, sum)
			return nil
		})
		if err != nil {
			return err
		}
		rep.Digest = hex.EncodeToString(h.Sum(nil))
		return enc.Encode(rep)
	}

	opts.Now = now()
	opts.BlockingFiles = []string{purgeIntent}
	report, err := rawstore.PackClosedDays(rawDir, opts)
	if encErr := enc.Encode(report); encErr != nil && err == nil {
		err = encErr
	}
	return err
}
