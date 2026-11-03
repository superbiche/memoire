-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

BEGIN;

-- The de-noise pool inversion: the stage-1 judge now emits a CONFIDENCE in
-- [0,1] on its verdict, and the de-noise surface pins low-confidence noise
-- calls into a doubt block the operator reads first.
--
-- ADDITIVE ONLY: a new nullable column, no drop, no rename, no rewrite. Every
-- suggestion written before the prompt bump keeps a NULL confidence, and NULL
-- is deliberately NOT read as doubt — see the store's doubt predicate. The
-- whole v1-era backlog would otherwise land in the doubt block on day one,
-- which is the opposite of what a doubt block is for.
ALTER TABLE observation_triage_suggestions ADD COLUMN confidence REAL;

INSERT OR IGNORE INTO schema_version(version, applied_at, note)
VALUES (18, datetime('now'), 'triage suggestion confidence (additive, nullable)');

COMMIT;
