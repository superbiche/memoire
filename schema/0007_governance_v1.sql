-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

BEGIN;

ALTER TABLE observations ADD COLUMN provenance TEXT;
ALTER TABLE observations ADD COLUMN can_instruct INTEGER NOT NULL DEFAULT 0;
ALTER TABLE observations ADD COLUMN can_evidence INTEGER NOT NULL DEFAULT 1;
ALTER TABLE observations ADD COLUMN needs_confirmation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE observations ADD COLUMN noise_candidate INTEGER NOT NULL DEFAULT 0;

ALTER TABLE observation_review_log ADD COLUMN action TEXT;

CREATE INDEX idx_observations_provenance ON observations(provenance);
CREATE INDEX idx_observations_noise_candidate ON observations(noise_candidate);

UPDATE observations SET provenance = 'observed' WHERE kind = 'raw';
UPDATE observations SET provenance = 'inferred' WHERE kind != 'raw';

CREATE TRIGGER governance_observations_can_instruct_insert
AFTER INSERT ON observations
WHEN NEW.can_instruct = 1 AND (NEW.provenance IS NULL OR NEW.provenance NOT IN ('user_confirmed','imported'))
BEGIN
    SELECT RAISE(ABORT, 'can_instruct requires provenance user_confirmed or imported');
END;

CREATE TRIGGER governance_observations_can_instruct_update
AFTER UPDATE ON observations
WHEN NEW.can_instruct = 1 AND (NEW.provenance IS NULL OR NEW.provenance NOT IN ('user_confirmed','imported'))
BEGIN
    SELECT RAISE(ABORT, 'can_instruct requires provenance user_confirmed or imported');
END;

INSERT OR IGNORE INTO schema_version(version, applied_at, note) VALUES (7, datetime('now'), 'governance v1 provenance flags and review actions');

COMMIT;
