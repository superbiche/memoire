-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

-- schema-pg/0007_triage_suggestion_confidence.sql — PostgreSQL twin of
-- schema/0018_triage_suggestion_confidence.sql. See that file for the design
-- comment; the column type is the only translation.
BEGIN;

ALTER TABLE observation_triage_suggestions ADD COLUMN IF NOT EXISTS confidence DOUBLE PRECISION;

INSERT INTO schema_version(version, applied_at, note)
VALUES (7, now()::text, 'triage suggestion confidence (additive, nullable)')
ON CONFLICT (version) DO NOTHING;

COMMIT;
