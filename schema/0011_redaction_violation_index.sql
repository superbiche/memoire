-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

BEGIN;

-- memoire:D136 R-2: a queue-writer backstop rewrite on the liaison-evidence
-- adapter path is a detectable contract violation. The writer stamps
-- metadata.redaction_contract_violation = true; this partial index serves the
-- /v1/health violation count without scanning events. The predicate must
-- textually match the literal WHERE clause in
-- CountRedactionContractViolations (bound parameters cannot prove a partial
-- index predicate; 0008 lesson).
CREATE INDEX IF NOT EXISTS idx_events_redaction_violation
ON events(id)
WHERE json_extract(meta_json, '$.redaction_contract_violation') = 1;

INSERT OR IGNORE INTO schema_version(version, applied_at, note)
VALUES (11, datetime('now'), 'partial index for redaction contract violation count');

COMMIT;
