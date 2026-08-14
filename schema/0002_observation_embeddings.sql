-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

-- Phase 2 sketch. Not applied by v0.1.
CREATE TABLE IF NOT EXISTS observation_embeddings (
  observation_id   INTEGER PRIMARY KEY,
  model            TEXT NOT NULL,
  dim              INTEGER NOT NULL,
  vector           BLOB NOT NULL,
  created_at_epoch INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_obs_emb_model
  ON observation_embeddings(model);

