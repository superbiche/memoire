-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

-- BYOK provider (memoire:D232 matrix cell, ratified handoff 2026-08-04):
-- widen the derivation_runs.provider CHECK to admit 'byok' — the
-- bring-your-own-endpoint standalone derivation provider (two dialects,
-- closed set; docs/BYOK-DERIVE-DESIGN.md).
--
-- SQLite cannot alter an inline CHECK, so this is the documented rebuild
-- pattern under the operator-ruled STRICT-WIDENING carve-out to the
-- additive-only law (2026-08-05, journaled with the BYOK slice; see
-- schema/README.md): the new constraint accepts a strict superset of the
-- old values, every existing row passes unchanged, and the copy is
-- column-complete. No FK references and no triggers touch derivation_runs
-- (verified at authoring); the write handle serializes DDL, so the rename
-- is atomic within the transaction.

BEGIN;

CREATE TABLE derivation_runs_new (
  id                  TEXT PRIMARY KEY,
  project             TEXT NOT NULL,
  session_id          TEXT,
  provider            TEXT NOT NULL CHECK (provider IN ('rules', 'local-qwen', 'openai', 'manual', 'byok')),
  data_boundary       TEXT NOT NULL CHECK (data_boundary IN ('local', 'external')),
  derivation_version  TEXT NOT NULL,
  prompt_version      TEXT,
  input_hash          TEXT NOT NULL,
  window_start_epoch  INTEGER NOT NULL,
  window_end_epoch    INTEGER NOT NULL,
  status              TEXT NOT NULL,
  created_at_epoch    INTEGER NOT NULL,
  completed_at_epoch  INTEGER,
  meta_json           TEXT,
  model               TEXT,
  endpoint_id         TEXT,
  created_at_ns       INTEGER,
  snapshot_max_event_id INTEGER
);

INSERT INTO derivation_runs_new (
  id, project, session_id, provider, data_boundary, derivation_version,
  prompt_version, input_hash, window_start_epoch, window_end_epoch,
  status, created_at_epoch, completed_at_epoch, meta_json,
  model, endpoint_id, created_at_ns, snapshot_max_event_id
)
SELECT
  id, project, session_id, provider, data_boundary, derivation_version,
  prompt_version, input_hash, window_start_epoch, window_end_epoch,
  status, created_at_epoch, completed_at_epoch, meta_json,
  model, endpoint_id, created_at_ns, snapshot_max_event_id
FROM derivation_runs;

DROP TABLE derivation_runs;
ALTER TABLE derivation_runs_new RENAME TO derivation_runs;

-- Recreate the table's full current index set (0004 + 0006 final shapes).
CREATE UNIQUE INDEX IF NOT EXISTS idx_derivation_runs_idempotency
  ON derivation_runs(
    provider,
    COALESCE(model, ''),
    derivation_version,
    project,
    COALESCE(session_id, ''),
    window_start_epoch,
    window_end_epoch,
    input_hash
  );
CREATE INDEX IF NOT EXISTS idx_derivation_runs_project_window
  ON derivation_runs(project, window_end_epoch DESC, window_start_epoch DESC);
CREATE INDEX IF NOT EXISTS idx_derivation_runs_status_created
  ON derivation_runs(status, created_at_epoch DESC);
CREATE INDEX IF NOT EXISTS idx_derivation_runs_project_provider_model
  ON derivation_runs(
    project,
    provider,
    COALESCE(model, ''),
    COALESCE(endpoint_id, ''),
    window_end_epoch DESC,
    window_start_epoch DESC
  );

INSERT OR IGNORE INTO schema_version(version, applied_at, note)
VALUES (16, datetime('now'), 'widen derivation_runs.provider CHECK for byok (strict-widening carve-out, BYOK slice)');

COMMIT;
