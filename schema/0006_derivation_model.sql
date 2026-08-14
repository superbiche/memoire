-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

BEGIN;

ALTER TABLE derivation_runs ADD COLUMN model TEXT;
ALTER TABLE derivation_runs ADD COLUMN endpoint_id TEXT;

DROP INDEX IF EXISTS idx_derivation_runs_idempotency;
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
VALUES (6, datetime('now'), 'derivation run model and endpoint identity');

COMMIT;
