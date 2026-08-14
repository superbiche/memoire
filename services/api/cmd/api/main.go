// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
	"memoire/services/api/internal/drainer"
	"memoire/services/api/internal/embedclient"
	"memoire/services/api/internal/server"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}
	if err := cfg.EnsureDirs(); err != nil {
		logger.Error("ensure directories", "err", err)
		os.Exit(1)
	}

	token, err := cfg.EnsureToken()
	if err != nil {
		logger.Error("ensure token", "err", err)
		os.Exit(1)
	}

	store, err := db.Open(cfg.DBPath, cfg.SchemaDir)
	if err != nil {
		logger.Error("open database", "err", err)
		os.Exit(1)
	}
	defer store.Close()
	if residue, err := store.BroadReviewQueueIndexResidue(); err != nil {
		logger.Warn("broad review-queue index residue check failed", "err", err)
	} else if len(residue) > 0 {
		logger.Warn("residual broad review-queue index detected — hand-swap to the schema/0008 proposed-only partial shape (drop+create in one transaction; memoire:D142)",
			"indexes", residue)
	}
	if _, err := store.EnsureLocalTokenCredential(context.Background(), token); err != nil {
		logger.Error("ensure token credential", "err", err)
		os.Exit(1)
	}

	if cfg.EmbedURL != "" {
		// Hybrid lane (D218). The bearer sent to /embed is the dedicated
		// embed credential when MEMOIRE_EMBED_TOKEN_FILE is set (F4
		// least-privilege split: the embed pod validates a secret that
		// grants nothing on this API), else this deployment's own custody
		// token (local-first single-token topology). The store's own
		// preconditions (PG engine + provisioned pgvector) still gate
		// activation; wiring on SQLite is a harmless no-op.
		embedToken, err := cfg.EmbedToken(token)
		if err != nil {
			logger.Error("resolve embed token", "err", err)
			os.Exit(1)
		}
		store.EnableHybrid(embedclient.New(cfg.EmbedURL, embedToken, db.DefaultEmbedModel),
			cfg.HybridSearchDefault, time.Duration(cfg.EmbedBudgetMS)*time.Millisecond)
		logger.Info("hybrid search lane wired",
			"embed_url", cfg.EmbedURL, "default_on", cfg.HybridSearchDefault,
			"state", store.HybridState())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	drain := drainer.New(cfg, store, logger.With("component", "drainer"))
	go drain.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(cfg, store, logger),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("memoire api listening", "addr", cfg.Addr, "home", cfg.Home)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
