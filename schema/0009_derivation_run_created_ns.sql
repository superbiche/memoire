-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

BEGIN;

-- Deterministic "latest run" ordering for the force-mint supersede lookup:
-- created_at_epoch has second resolution and id is a content hash, so two
-- forced runs in the same second tied on time and ordered by effectively
-- random hash — nondeterministic superseded_run_id provenance. New runs
-- record insert time at nanosecond resolution; historical rows stay NULL
-- (COALESCE to 0 in ORDER BY, preserving today's behavior for old ties).
ALTER TABLE derivation_runs ADD COLUMN created_at_ns INTEGER;

INSERT OR IGNORE INTO schema_version(version, applied_at, note)
VALUES (9, datetime('now'), 'derivation_runs.created_at_ns for deterministic supersede ordering');

COMMIT;
