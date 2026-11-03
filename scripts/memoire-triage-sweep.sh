#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-triage-sweep.sh — the DAILY triage pass (triage lane v1, R-C).
#
# It appends SUGGESTIONS and rules on nothing: every verdict still travels
# the human routes. The judging unit is "session, chunked"; the binary does
# the chunking under a conservative prompt ceiling, and this script walks
# sessions so one bad session cannot swallow the pass.
#
# Config surface mirrors the derive lane deliberately (R-C): endpoint, model
# and effort by env, resolved through the same model-endpoints registry, so
# the judge is bound to a `data_boundary=local` provider (memoire:D137) and
# to the name-not-value secret seam (memoire:D55/D58). Moving the judge to
# another seat is an env change, not a code change — the 3060 seat map will
# move it.
#
# Not wired into any scheduler by this slice: the CronJob lives in cdk8s and
# is deliberately out of scope here. Run it by hand or from a user timer
# until that lands.

MEMOIRE_HOME="${MEMOIRE_HOME:-$HOME/.memoire}"
LOG_DIR="$MEMOIRE_HOME/log"
LOG_FILE="$LOG_DIR/triage-sweep.log"
TRIAGE_BIN="${MEMOIRE_TRIAGE_BIN:-$(dirname "${BASH_SOURCE[0]}")/../bin/memoire-triage}"

# The judge DEFAULTS to the HL5 conductor lane (R-C), in-script — the
# memoire:D162 precedent, where the derive scripts default their endpoint
# rather than making an unset env a silent no-op. The model has no default:
# the endpoint's allowlist is per-endpoint, so guessing a model name would
# fail at the adapter with a worse error than saying it is unset.
ENDPOINT="${MEMOIRE_TRIAGE_ENDPOINT:-hl5}"
MODEL="${MEMOIRE_TRIAGE_MODEL:-}"
STAGE="${MEMOIRE_TRIAGE_STAGE:-1}"
LIMIT="${MEMOIRE_TRIAGE_LIMIT:-200}"
# The sweep walks every session in scope and pages to exhaustion. BUDGET
# bounds one pass deliberately (0 = no cap); LIMIT is only the page size.
BUDGET="${MEMOIRE_TRIAGE_BUDGET:-0}"
TIMEOUT="${MEMOIRE_TRIAGE_TIMEOUT:-10m}"
DRY_RUN="${MEMOIRE_TRIAGE_DRY_RUN:-0}"

# Every precondition exits 0 and says why. A triage pass is a convenience
# lane: it must never fail a host's timer chain, and a silent exit would be
# indistinguishable from a pass that found nothing.
log_line() {
  mkdir -p "$LOG_DIR" 2>/dev/null || return 0
  printf '%s triage %s\n' "$(date -Iseconds)" "$*" >> "$LOG_FILE"
}

[[ -x "$TRIAGE_BIN" ]] || { log_line "skipped=no_binary path=$TRIAGE_BIN"; exit 0; }
if [[ -z "$ENDPOINT" || -z "$MODEL" ]]; then
  log_line "skipped=unconfigured (set MEMOIRE_TRIAGE_MODEL; endpoint defaults to hl5)"
  exit 0
fi
command -v flock >/dev/null 2>&1 || { log_line "skipped=no_flock"; exit 0; }

# One pass at a time. A second pass over the same rows is harmless — the
# suggestions are idempotent per (observation, prompt version) — but it
# would burn the judge's seat for nothing.
LOCK_FILE="$MEMOIRE_HOME/triage-sweep.lock"
exec 9>"$LOCK_FILE" || { log_line "skipped=no_lock"; exit 0; }
flock -n 9 || { log_line "skipped=already_running"; exit 0; }

ARGS=(-sweep -stage "$STAGE" -endpoint-id "$ENDPOINT" -model "$MODEL"
      -limit "$LIMIT" -budget "$BUDGET" -timeout "$TIMEOUT" -json)
[[ "$DRY_RUN" == "1" ]] && ARGS+=(-dry-run)
[[ -n "${MEMOIRE_TRIAGE_PROJECT:-}" ]] && ARGS+=(-project "$MEMOIRE_TRIAGE_PROJECT")
[[ -n "${MEMOIRE_TRIAGE_SESSION:-}" ]] && ARGS+=(-session-id "$MEMOIRE_TRIAGE_SESSION")

out="$("$TRIAGE_BIN" "${ARGS[@]}" 2>&1)"
rc=$?
# Sanitized to one line, the same shape the derive sweep logs: a judge's
# reason text can be long and is content, not lane telemetry.
log_line "rc=$rc $(printf '%s' "$out" | tr -c '[:print:]' ' ' | tail -c 1500)"

if (( rc != 0 )); then
  # A pass that judged nothing because the endpoint was down is red: the
  # lane must not report success it did not have (memoire:D166/D198's
  # all-fail=red doctrine, applied to this lane).
  exit 1
fi
exit 0
