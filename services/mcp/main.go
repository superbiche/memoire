// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// memoire-mcp — thin stdio MCP adapter over Memoire's HTTP read surface.
//
// Per docs/ARCHITECTURE.md, MCP arrives as "a thin adapter over HTTP, not a
// second implementation": every tool delegates to the local API at
// MEMOIRE_ADDR with the bearer token from MEMOIRE_TOKEN_FILE. Read-only —
// no tool writes to the store.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/server"
)

const serverName = "memoire"

// serverVersion is stamped at build time via
// -ldflags "-X main.serverVersion=<version>" from the repo's VERSION file
// (same MISSING #7 honesty rule as the API's /v1/health). Unstamped builds
// report "dev".
var serverVersion = "dev"

func main() {
	// MEMOIRE_BASE_URL is the ratified single knob for every API consumer
	// (memoire:D165, R-10 §3.2); MEMOIRE_ADDR stays as the host:port fallback.
	baseURL := os.Getenv("MEMOIRE_BASE_URL")
	if baseURL == "" {
		addr := os.Getenv("MEMOIRE_ADDR")
		if addr == "" {
			addr = "127.0.0.1:37778"
		}
		baseURL = fmt.Sprintf("http://%s", addr)
	}
	baseURL = strings.TrimRight(baseURL, "/")

	// An auth/config problem must NOT abort startup or touch the token file
	// before the JSON-RPC handshake: a harness reads a server that dies (or
	// hangs on a FIFO/stalled mount) pre-handshake as "no tools", hiding the
	// problem as a missing capability (inbox 2026-07-30). NO token I/O
	// happens here — not even a warm-up read; the token loads lazily inside
	// bearer() on the first tool call, and every failed call answers with
	// the path tried and the MEMOIRE_TOKEN_FILE override.
	tokenFile := os.Getenv("MEMOIRE_TOKEN_FILE")
	tokenFromEnv := tokenFile != ""
	tokenPathErr := ""
	if tokenFile == "" {
		home := os.Getenv("MEMOIRE_HOME")
		if home == "" {
			if userHome, err := os.UserHomeDir(); err != nil {
				tokenPathErr = fmt.Sprintf("cannot resolve home dir for the default token path: %v (set MEMOIRE_TOKEN_FILE or MEMOIRE_HOME)", err)
			} else {
				home = filepath.Join(userHome, ".memoire")
			}
		}
		if home != "" {
			tokenFile = filepath.Join(home, "token")
		}
	}

	client := newAPIClient(baseURL, tokenFile, tokenFromEnv, tokenPathErr)
	s := server.NewMCPServer(serverName, serverVersion,
		server.WithToolCapabilities(false),
	)
	registerTools(s, client)

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("memoire-mcp: %v", err)
	}
}
