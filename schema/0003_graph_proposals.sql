-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

-- Phase 3 sketch. Not applied by v0.1.
CREATE TABLE IF NOT EXISTS graph_proposals (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  src_obs_id        INTEGER NOT NULL,
  dst_obs_id        INTEGER NOT NULL,
  edge_kind         TEXT NOT NULL,
  confidence        REAL NOT NULL,
  rationale         TEXT,
  proposer          TEXT NOT NULL,
  state             TEXT NOT NULL,
  created_at_epoch  INTEGER NOT NULL,
  reviewed_at_epoch INTEGER,
  reviewer          TEXT
);
CREATE INDEX IF NOT EXISTS idx_gp_state_created
  ON graph_proposals(state, created_at_epoch DESC);
CREATE INDEX IF NOT EXISTS idx_gp_src ON graph_proposals(src_obs_id);
CREATE INDEX IF NOT EXISTS idx_gp_dst ON graph_proposals(dst_obs_id);

