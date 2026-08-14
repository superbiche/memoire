// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// memoire-mint-token — hub-side, operator-run credential mint
// (memoire:D165, laptop onboarding prep; closes D153 §7 "provisioning is
// manual for now" with a real surface instead of hand-inserted hashes).
//
// Mints a random bearer token, registers its HASH in the store's
// credentials table (the plaintext is never persisted server-side), and
// prints the token ONCE on stdout. The operator moves it to the client
// (scp, 0600) — e.g. a thin client's forward token
// ($MEMOIRE_HOME/forward-token) or its read+write client token.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("memoire-mint-token", flag.ContinueOnError)
	label := fs.String("label", "", "credential label (required, e.g. laptop-forward)")
	scopes := fs.String("scopes", "read,write", "comma-separated scopes (read, write, admin)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *label == "" {
		return fmt.Errorf("-label is required")
	}
	var scopeList []string
	for _, s := range strings.Split(*scopes, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		switch s {
		case "read", "write", "admin":
			scopeList = append(scopeList, s)
		default:
			return fmt.Errorf("unknown scope %q (valid: read, write, admin)", s)
		}
	}
	if len(scopeList) == 0 {
		return fmt.Errorf("at least one scope is required")
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	token := "mem_" + hex.EncodeToString(raw[:])

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := db.Open(cfg.DBPath, cfg.SchemaDir)
	if err != nil {
		return err
	}
	defer store.Close()
	cred, err := store.EnsureTokenCredential(context.Background(), token, *label, scopeList)
	if err != nil {
		return err
	}
	// The token goes to STDOUT alone (pipe it straight into the destination
	// file); everything descriptive goes to stderr so redirection stays clean.
	fmt.Fprintf(os.Stderr, "minted credential id=%s label=%s scopes=%s prefix=%s\n",
		cred.ID, *label, strings.Join(scopeList, ","), cred.Prefix)
	fmt.Println(token)
	return nil
}
