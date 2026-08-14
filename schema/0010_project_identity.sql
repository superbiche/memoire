-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

BEGIN;

-- liaison:D239 item 4: identity-conflict detection at the ingest chokepoint.
-- Adapters resolve a project key and forward the root it was anchored at
-- (payload.memoire_project_root/_source; mechanic ratified 2026-07-18:
-- adapter-passed roots, chosen over a server-side cwd heuristic to keep the
-- alert channel free of false positives from legitimate multi-home projects).
--
-- project_roots: the roots a key is known under. First root learned wins as
-- canonical; registry-sourced roots are operator-curated and always join
-- silently (the registry is the sanctioned multi-home mechanism).
CREATE TABLE IF NOT EXISTS project_roots (
  project          TEXT NOT NULL,
  root             TEXT NOT NULL,
  source           TEXT NOT NULL,
  first_seen_epoch INTEGER NOT NULL,
  last_seen_epoch  INTEGER NOT NULL,
  PRIMARY KEY (project, root)
);

-- project_identity_alerts: a key arriving from a root it is NOT known under,
-- via a non-curated source (basename collision, or a duplicated .memoire
-- key). Loud, never blocking: surfaced in the injection header and MCP
-- reads; cleared when the root becomes known (operator adds it to the
-- registry, or re-keys via project-move so events stop arriving).
CREATE TABLE IF NOT EXISTS project_identity_alerts (
  project          TEXT NOT NULL,
  root             TEXT NOT NULL,
  known_root       TEXT NOT NULL,
  source           TEXT NOT NULL,
  first_seen_epoch INTEGER NOT NULL,
  last_seen_epoch  INTEGER NOT NULL,
  event_count      INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (project, root)
);

INSERT OR IGNORE INTO schema_version(version, applied_at, note)
VALUES (10, datetime('now'), 'project identity roots and conflict alerts');

COMMIT;
