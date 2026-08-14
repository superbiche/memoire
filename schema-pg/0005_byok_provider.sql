-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

-- schema-pg/0005_byok_provider.sql — PostgreSQL twin of
-- schema/0016_byok_provider.sql (BYOK provider, memoire:D232;
-- docs/BYOK-DERIVE-DESIGN.md). Strict-widening carve-out (operator-ruled
-- 2026-08-05): the replacement CHECK accepts a strict superset, so every
-- existing row passes and ADD CONSTRAINT validates instantly. PG can
-- replace a constraint in place — no table rebuild here.
--
-- The inline column CHECK from 0001_baseline.sql carries the
-- auto-generated name derivation_runs_provider_check; the DO block drops
-- whichever check constraint actually guards provider membership, so a
-- differently-named twin (restored dump, hand-applied baseline) cannot
-- silently survive and reject 'byok' at runtime.

BEGIN;

DO $$
DECLARE c RECORD;
BEGIN
  FOR c IN
    SELECT conname FROM pg_constraint
    WHERE conrelid = 'derivation_runs'::regclass
      AND contype = 'c'
      AND (pg_get_constraintdef(oid) LIKE '%provider = ANY%'
           OR pg_get_constraintdef(oid) LIKE '%provider IN%')
  LOOP
    EXECUTE format('ALTER TABLE derivation_runs DROP CONSTRAINT %I', c.conname);
  END LOOP;
END $$;

ALTER TABLE derivation_runs ADD CONSTRAINT derivation_runs_provider_check
  CHECK (provider IN ('rules', 'local-qwen', 'openai', 'manual', 'byok'));

INSERT INTO schema_version(version, applied_at, note)
VALUES (5, now()::text, 'widen derivation_runs.provider CHECK for byok (strict-widening carve-out, BYOK slice)')
ON CONFLICT (version) DO NOTHING;

COMMIT;
