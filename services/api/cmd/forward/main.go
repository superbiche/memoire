// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// memoire-forward is the client half of remote-capable capture (memoire:D147
// slice 3): it drains the LOCAL file queue by forwarding each envelope to the
// hub's POST /v1/events, deleting the local copy only on the queue-accept ack
// (202 + "queued":true). See services/api/internal/forwarder for semantics.
//
// Modes:
//
//	memoire-forward            run the forward loop until signalled
//	memoire-forward -once      one drain cycle, then exit
//	memoire-forward -status    print client-visible queue depths + last
//	                           forwarder status as JSON, then exit (works
//	                           without MEMOIRE_FORWARD_URL — it never talks
//	                           to the hub)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/forwarder"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, logger); err != nil {
		logger.Error("memoire-forward", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer, logger *slog.Logger) error {
	fs := flag.NewFlagSet("memoire-forward", flag.ContinueOnError)
	once := fs.Bool("once", false, "run one drain cycle and exit")
	statusOnly := fs.Bool("status", false, "print queue depths + forward status as JSON and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		return fmt.Errorf("ensure directories: %w", err)
	}

	if *statusOnly {
		out := map[string]any{
			"depths":      forwarder.MeasureDepths(cfg),
			"forward_url": cfg.ForwardURL,
		}
		if b, err := os.ReadFile(forwarder.StatusPath(cfg)); err == nil {
			var st forwarder.Status
			if json.Unmarshal(b, &st) == nil {
				out["forward_status"] = st
			}
		}
		body, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(body))
		return nil
	}

	fwd, err := forwarder.New(cfg, logger.With("component", "forwarder"))
	if err != nil {
		return fmt.Errorf("init forwarder: %w", err)
	}

	if *once {
		fwd.Reconcile(ctx)
		fwd.ForwardOnce(ctx)
		return nil
	}
	logger.Info("memoire forward loop starting", "home", cfg.Home, "forward_url", cfg.ForwardURL)
	fwd.Run(ctx)
	return nil
}
