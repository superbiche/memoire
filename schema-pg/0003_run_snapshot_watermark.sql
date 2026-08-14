-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

-- schema-pg/0003_run_snapshot_watermark.sql — PostgreSQL twin of
-- schema/0014_run_snapshot_watermark.sql (memoire:D165 round-2 HIGH-1).
-- See that file for the design comment.
BEGIN;

ALTER TABLE derivation_runs ADD COLUMN IF NOT EXISTS snapshot_max_event_id BIGINT;

INSERT INTO schema_version(version, applied_at, note)
VALUES (3, now()::text, 'derivation_runs input-snapshot watermark (R-10.1, memoire:D165)')
ON CONFLICT (version) DO NOTHING;

COMMIT;
