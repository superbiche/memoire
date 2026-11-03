#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-derive-sweep.sh — periodic derivation sweep over recently-active
# sessions, run by the memoire-derive-sweep systemd user timer (see
# scripts/derive-timer.sh). Complements the CC SessionEnd hook
# (memoire-derive-auto.sh): the hook covers Claude Code session ends; the
# sweep covers foreign-harness sessions and anything the hook missed.
#
# Secret sourcing (operator ruling 2026-07-10, TPM option): the systemd unit
# loads a TPM2/host-encrypted credential (LoadCredentialEncrypted) and this
# script exports it under the endpoint's api_key_env name. Ambient env still
# works for manual runs. The value is never logged or placed in argv.
#
# Server-side idempotency (run input-hash dedup + observation content-hash
# dedup) makes re-sweeping unchanged sessions a cheap no-op, so the sweep
# enumerates bluntly: every session with events in the last SWEEP_HOURS.
# Targeting hygiene (operator brief 2026-07-10) lives in memoire-derive
# itself: the project denylist and the min-content window guard both emit
# skip markers that land in this log.
#
# Adaptive window split (brief amendment 2026-07-10): a whole-session window
# above the local model's prompt ceiling fails instantly, so each session is
# dry-run first (local, no provider call) and windows over
# MEMOIRE_SWEEP_MAX_PROMPT_BYTES are recursively halved on the median
# canonical-event epoch. Each half is its own idempotent derivation run.
#
# Rhythm cuts (operator ruling 2026-09-08, Fibery #343): halving is a BYTE
# response, not a story one — it cuts arcs mid-sentence, and the model then
# summarizes fragments. Since memoire:D260 made transcript capture a stream of
# checkpoints, a session's live input is in the store while it is still being
# written, so each session is first cut at its own rhythm — a long pause, or N
# new substantive events — via the binary's -cut-points helper, and each
# segment is derived as its own run with its own input manifest. Session end is
# not a special case: the trailing segment simply ends at the end of the
# window. Rhythm cuts are the FIRST splitter and halving is the FALLBACK: a
# segment still above the ceiling goes through the same dry-run + halving path
# it always did. Cross-cut context needs nothing here — memoire:D212's "Already
# Captured (this session)" prompt section is scoped by (project, session) with
# no window predicate, so the previous cut's observations are already in the
# next cut's prompt. Knobs: MEMOIRE_SWEEP_RHYTHM_CUTS=0 disables the pass
# entirely; MEMOIRE_SWEEP_CUT_GAP_SECONDS / _CUT_MAX_EVENTS / _CUT_MAX_SEGMENTS
# override the binary's defaults (unset = the binary governs, which keeps the
# numbers in ONE place, the same discipline the ceiling probe below follows).
#
# Open tail (operator ruling 2026-09-08, Fibery #344): the trailing segment —
# and the whole-session window when there is no cut, fail-open paths included —
# ends at the last event SO FAR, so it is derived mid-arc while the session may
# still be running. Those legs carry -provisional, which marks the run and its
# observations as "so far" statements; inner segments, whose arc the detector
# already closed, never do. Halving an open tail keeps the marker on the RIGHT
# leg only. Knob: MEMOIRE_SWEEP_SUPERSEDE=0 passes the binary's -no-supersede
# kill switch on every execute leg, restoring the pre-#344 near-dup behavior.
#
# Kill switch: MEMOIRE_AUTO_DERIVE=0 short-circuits to an instant clean exit
# (one log marker, no store/network access). Same switch covers the
# SessionEnd hook (memoire-derive-auto.sh).
#
# Foreign-harness pre-pass (live turn ingest, 2026-07-12): before session
# selection each tick, scan recent codex/qwen rollouts and the opencode
# session store through their capture adapters so foreign assistant turns
# reach the store the same tick their sessions are derived, then drain-wait
# (bounded) for the file queue. The scan
# window defaults to "2 hours ago" — keep it >= 2x the tick interval so
# nothing slips between ticks. Fail-open per adapter; the kill switch above
# still short-circuits before any of this.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MEMOIRE_HOME="${MEMOIRE_HOME:-$HOME/.memoire}"
# The derive binary next to this checkout (MEMOIRE_DERIVE_BIN overrides).
DERIVE_BIN="${MEMOIRE_DERIVE_BIN:-$SCRIPT_DIR/../bin/memoire-derive}"
# D166 ruling (2): the derive binary auto-migrates via db.Open, but the
# config default schema dir is the RELATIVE "schema" — under the systemd
# timer's cwd it resolves nowhere and migrations silently no-op'd for the
# whole 42P01 window. Anchor to the repo this script lives in; explicit
# env always wins.
export MEMOIRE_SCHEMA_DIR="${MEMOIRE_SCHEMA_DIR:-$(cd "$SCRIPT_DIR/.." && pwd)/schema}"
PROVIDER="${MEMOIRE_DERIVE_PROVIDER:-local-qwen}"
# Daily derivation runs on HL3 (operator ruling 2026-07-22); HL5 is reserved
# for one-off large derivation needs via MEMOIRE_DERIVE_ENDPOINT/-MODEL/
# -KEY_ENV overrides.
ENDPOINT_ID="${MEMOIRE_DERIVE_ENDPOINT:-hl3}"
MODEL="${MEMOIRE_DERIVE_MODEL:-hl3-qwen3.6-35b-a3b-iq3_xxs}"
KEY_ENV="${MEMOIRE_DERIVE_KEY_ENV:-HL3_QWEN_API_KEY}"
# Chat-template reasoning effort (operator ruling 2026-08-26): unset sends no
# kwarg, so the endpoint's own default governs and the request body is
# unchanged. The HL5 lanes set "low"; a template that does not know the kwarg
# ignores it.
REASONING_EFFORT="${MEMOIRE_DERIVE_REASONING_EFFORT:-}"
EXECUTE_TIMEOUT="${MEMOIRE_DERIVE_TIMEOUT:-15m}"
SWEEP_HOURS="${MEMOIRE_SWEEP_HOURS:-24}"
# Split ceiling sized for the daily HL3 endpoint: n_ctx measured 32768
# (2026-07-22, hl3-qwen3.6-35b-a3b-iq3_xxs) — 110 KB prompts overflowed it
# with HTTP 400 at provider_call; 70 KB leaves completion + template
# headroom. Raise via env for endpoints with larger contexts (e.g. HL5
# one-off runs).
MAX_PROMPT_BYTES="${MEMOIRE_SWEEP_MAX_PROMPT_BYTES:-70000}"
# Ceiling precedence (review r1 F1): an operator env override outranks
# everything, exactly as before. With it UNSET, the first dry-run of the run
# omits -max-prompt-bytes so the endpoint's declared profile can govern
# (docs/DERIVE-PROFILE-DESIGN.md §3 — passing the fallback as a flag made
# fs.Visit mark it explicit, which silently outranked every profile).
#
# The 70000 above MUST survive as the no-profile fallback: with neither flag
# nor profile the CLI resolves max_prompt_bytes to its built-in default of
# 0 = NO CAP, so the dry-run would never report an oversized window and the
# adaptive split below would stop happening entirely. Production hl3 declares
# no profile (design Q4), so that is the live path, not a legacy branch.
CEILING_RESOLVED=0
[[ -n "${MEMOIRE_SWEEP_MAX_PROMPT_BYTES:-}" ]] && CEILING_RESOLVED=1
# 6 halvings = up to 64 legs per session; deeper than that means the split is
# not converging (e.g. one enormous event) and the leg is marked, not derived.
MAX_SPLIT_DEPTH="${MEMOIRE_SWEEP_MAX_SPLIT_DEPTH:-6}"
# Fail-open on a knob typo, and clamp: the value is now also shifted into a
# per-session leg budget below, so a nonnumeric or absurd depth must not turn
# into a nonsensical budget under set -u.
[[ "$MAX_SPLIT_DEPTH" =~ ^[0-9]+$ ]] || MAX_SPLIT_DEPTH=6
(( MAX_SPLIT_DEPTH <= 16 )) || MAX_SPLIT_DEPTH=16
# Rhythm cuts (Fibery #343). Empty means "the binary's default governs": the
# numbers and their justification live in services/api/cmd/derive/cutpoints.go,
# and duplicating them here would give one knob two sources of truth. A value
# of 0 for a class disables that class; MEMOIRE_SWEEP_RHYTHM_CUTS=0 disables
# the whole pass and restores the pure time-slice behavior.
RHYTHM_CUTS="${MEMOIRE_SWEEP_RHYTHM_CUTS:-1}"
CUT_GAP_SECONDS="${MEMOIRE_SWEEP_CUT_GAP_SECONDS:-}"
CUT_MAX_EVENTS="${MEMOIRE_SWEEP_CUT_MAX_EVENTS:-}"
CUT_MAX_SEGMENTS="${MEMOIRE_SWEEP_CUT_MAX_SEGMENTS:-}"
CUT_ARGS=()
[[ "$CUT_GAP_SECONDS" =~ ^[0-9]+$ ]] && CUT_ARGS+=(-cut-gap-seconds "$CUT_GAP_SECONDS")
[[ "$CUT_MAX_EVENTS" =~ ^[0-9]+$ ]] && CUT_ARGS+=(-cut-max-events "$CUT_MAX_EVENTS")
[[ "$CUT_MAX_SEGMENTS" =~ ^[0-9]+$ ]] && CUT_ARGS+=(-cut-max-segments "$CUT_MAX_SEGMENTS")
# Derivation-time supersede (Fibery #344). MEMOIRE_SWEEP_SUPERSEDE=0 passes the
# binary's kill switch on EVERY execute leg, restoring the pre-#344 gate: a
# later cut's restatement of an earlier cut's proposal is rejected as a
# near-duplicate instead of superseding it. Anything else leaves the feature
# on, which is the default in the binary too.
SUPERSEDE_ARGS=()
[[ "${MEMOIRE_SWEEP_SUPERSEDE:-1}" == "0" ]] && SUPERSEDE_ARGS+=(-no-supersede)
# Per-session leg budget. Rhythm cuts multiply how many legs one session can
# yield, so halving DEPTH alone no longer bounds a tick's provider calls:
# MAX_SPLIT_DEPTH's 2^depth becomes a per-SESSION ceiling as well as a per-leg
# one, which keeps the worst case exactly what it was before rhythm cuts
# existed (64 executed legs per session by default). Structural skips are not
# legs and never consume it.
SESSION_LEG_BUDGET=$(( 1 << MAX_SPLIT_DEPTH ))
SESSION_LEGS=0
CAPTURE_CODEX_BIN="${MEMOIRE_CAPTURE_CODEX_BIN:-$SCRIPT_DIR/memoire-capture-codex-rollout.sh}"
CAPTURE_QWEN_BIN="${MEMOIRE_CAPTURE_QWEN_BIN:-$SCRIPT_DIR/memoire-capture-qwen-rollout.sh}"
CAPTURE_OPENCODE_BIN="${MEMOIRE_CAPTURE_OPENCODE_BIN:-$SCRIPT_DIR/memoire-capture-opencode.sh}"
CAPTURE_SINCE="${MEMOIRE_SWEEP_CAPTURE_SINCE:-2 hours ago}"
QUEUE_DIR="${MEMOIRE_QUEUE_DIR:-$MEMOIRE_HOME/queue}"
DRAIN_TIMEOUT="${MEMOIRE_DRAIN_TIMEOUT:-60}"
CAPTURE_TIMEOUT="${MEMOIRE_CAPTURE_TIMEOUT:-300}"
# Fail-open on knob typos: a nonnumeric timeout must fall back to the default,
# not abort the sweep mid-arithmetic under set -u (ortho review 2026-07-12).
# CAPTURE_TIMEOUT must be a POSITIVE integer: GNU timeout treats 0 as "no
# limit", which would silently reintroduce the unbounded-adapter hang.
[[ "$DRAIN_TIMEOUT" =~ ^[0-9]+$ ]] || DRAIN_TIMEOUT=60
[[ "$CAPTURE_TIMEOUT" =~ ^[1-9][0-9]*$ ]] || CAPTURE_TIMEOUT=300
LOG_DIR="$MEMOIRE_HOME/log"
LOG_FILE="$LOG_DIR/derive-auto.log"
# Same lock as the SessionEnd hook: hook and sweep never derive concurrently.
LOCK_FILE="$MEMOIRE_HOME/derive-auto.lock"

# Kill switch: log one marker line (best effort) and exit before any
# credential, dependency, or store access.
if [[ "${MEMOIRE_AUTO_DERIVE:-1}" == "0" ]]; then
  mkdir -p "$LOG_DIR" 2>/dev/null \
    && printf '%s sweep skipped=auto_derive_disabled\n' "$(date -Is)" >> "$LOG_FILE" 2>/dev/null
  exit 0
fi

# Store selection (dialect seam, memoire:D159 cutover): MEMOIRE_DB may be a
# SQLite path or a postgres:// DSN. When unset, load it from the canonical
# env file so every derive-lane consumer (this sweep, the SessionEnd hook,
# the API quadlet) reads the same source. Read without shell-eval so
# credential characters in the DSN stay inert; the derive binary consumes it
# from env, never argv. All store access below goes through the binary's
# -list-targets/-median-epoch helpers — this script no longer opens the
# store itself. Placed AFTER the kill switch: the switch's instant-exit
# contract must not depend on the env file being readable (round-1 LOW-2).
DB_ENV_FILE="${MEMOIRE_DB_ENV_FILE:-$MEMOIRE_HOME/db.env}"
if [[ -z "${MEMOIRE_DB:-}" && -r "$DB_ENV_FILE" ]]; then
  MEMOIRE_DB="$(grep -m1 '^MEMOIRE_DB=' "$DB_ENV_FILE" | cut -d= -f2-)"
fi
[[ -n "${MEMOIRE_DB:-}" ]] && export MEMOIRE_DB

# systemd credential -> env var named by KEY_ENV. The credential id is the
# lowercased env var name (see derive-timer.sh encrypt).
CRED_NAME="$(printf '%s' "$KEY_ENV" | tr '[:upper:]' '[:lower:]')"
if [[ -n "${CREDENTIALS_DIRECTORY:-}" && -r "$CREDENTIALS_DIRECTORY/$CRED_NAME" ]]; then
  export "$KEY_ENV"="$(cat "$CREDENTIALS_DIRECTORY/$CRED_NAME")"
fi

printenv "$KEY_ENV" >/dev/null 2>&1 || exit 0
[[ -x "$DERIVE_BIN" ]] || exit 0
command -v flock >/dev/null 2>&1 || exit 0
mkdir -p "$LOG_DIR" 2>/dev/null || exit 0

# prepass runs one foreign-harness capture adapter over its recent rollouts.
# Fail-open: any adapter failure is logged (sanitized) and the sweep goes on —
# a broken adapter must never stop derivation of what IS in the store. The
# adapter runs bounded (timeout — a hung adapter must not stall the tick) and
# with the provider credential stripped from its environment (capture has no
# business inheriting the derive key); its output lands in a temp file and
# only a bounded tail is logged (ortho review 2026-07-12).
prepass() { # adapter-name capture-bin
  local name="$1" bin="$2" out_file rc
  if [[ ! -x "$bin" ]]; then
    printf '%s sweep prepass adapter=%s skipped=capture_bin_missing\n' \
      "$(date -Is)" "$name" >> "$LOG_FILE"
    return 0
  fi
  if ! command -v timeout >/dev/null 2>&1; then
    printf '%s sweep prepass adapter=%s skipped=timeout_bin_missing\n' \
      "$(date -Is)" "$name" >> "$LOG_FILE"
    return 0
  fi
  out_file="$(mktemp "${TMPDIR:-/tmp}/memoire-prepass.XXXXXX")" || {
    printf '%s sweep prepass adapter=%s skipped=mktemp_failed\n' \
      "$(date -Is)" "$name" >> "$LOG_FILE"
    return 0
  }
  timeout "$CAPTURE_TIMEOUT" env -u "$KEY_ENV" \
    "$bin" --scan --since "$CAPTURE_SINCE" >"$out_file" 2>&1
  rc=$?
  printf '%s sweep prepass adapter=%s rc=%d %s\n' "$(date -Is)" "$name" "$rc" \
    "$(tail -c 500 "$out_file" | tr -c '[:print:]' ' ')" >> "$LOG_FILE"
  rm -f "$out_file"
}

# drain_wait polls pending+inflight queue depth until empty or the bounded
# timeout elapses (queue writes are async — the API drainer ticks every 2s).
# On timeout the sweep derives anyway and marks it; the next tick catches the
# residue.
drain_wait() {
  local deadline depth
  deadline=$(( $(date +%s) + DRAIN_TIMEOUT ))
  while :; do
    depth="$(find "$QUEUE_DIR/pending" "$QUEUE_DIR/inflight" \
      -maxdepth 1 -name '*.json' 2>/dev/null | wc -l)"
    if [[ "$depth" -eq 0 ]]; then
      printf '%s sweep prepass drain=ok\n' "$(date -Is)" >> "$LOG_FILE"
      return 0
    fi
    if [[ "$(date +%s)" -ge "$deadline" ]]; then
      printf '%s sweep prepass drain=timeout pending_left=%s\n' \
        "$(date -Is)" "$depth" >> "$LOG_FILE"
      return 0
    fi
    sleep 1
  done
}

prepass codex "$CAPTURE_CODEX_BIN"
prepass qwen "$CAPTURE_QWEN_BIN"
prepass opencode "$CAPTURE_OPENCODE_BIN"
drain_wait

# Unit separator (0x1f): unlike a tab, it cannot plausibly appear inside a
# project or session id, so field parsing stays unambiguous. The binary's
# -list-targets emits the same framing (engine-agnostic via the dialect
# seam). An unreachable store is logged, never silent — the raw-sqlite3
# predecessor of this call silently no-oped for the whole cutover gap.
US=$'\x1f'
if ! TARGETS_OUT="$("$DERIVE_BIN" -list-targets -since-hours "$SWEEP_HOURS" 2>&1)"; then
  printf '%s sweep skipped=list_targets_failed %s\n' "$(date -Is)" \
    "$(printf '%s' "$TARGETS_OUT" | tr -c '[:print:]' ' ' | tail -c 300)" >> "$LOG_FILE"
  # Mandatory-enumeration honesty (preflight audit finding 3, extends
  # D166): a sweep that could not even enumerate targets must not look
  # green — under systemd/k8s a zero exit here records HL5 lane success
  # over zero enumerated work. Successful EMPTY enumeration stays green;
  # intent-list degradation stays fail-open (enumeration is its net).
  exit 1
fi

# Notify intents are SCHEDULING INPUT, not telemetry (memoire:D165 R-10.2,
# round-1 MED-2): open intents run AHEAD of the recency enumeration (priority
# work — a notified session whose events fall outside MEMOIRE_SWEEP_HOURS is
# still attempted), and the enumeration stays as the safety net for sessions
# nobody notified about. The union is deduped first-occurrence-wins, so a
# session in both lists derives once. Fail-open: an intent-listing failure
# degrades to the plain enumeration with a log marker, never a silent skip.
if ! INTENTS_OUT="$("$DERIVE_BIN" -list-open-intents 2>&1)"; then
  printf '%s sweep intents=list_failed %s\n' "$(date -Is)" \
    "$(printf '%s' "$INTENTS_OUT" | tr -c '[:print:]' ' ' | tail -c 300)" >> "$LOG_FILE"
  INTENTS_OUT=""
fi
COMBINED="$(printf '%s\n%s\n' "$INTENTS_OUT" "$TARGETS_OUT" | awk 'NF && !seen[$0]++')"
[[ -n "$COMBINED" ]] || exit 0
mapfile -t ROWS <<< "$COMBINED"
[[ ${#ROWS[@]} -gt 0 ]] || exit 0

exec 9>"$LOCK_FILE" || exit 0
flock 9 || exit 0

# log_line writes one sanitized sweep log line. Display copies strip
# non-printables so control characters cannot forge lines.
log_line() { # project session rest
  local project="${1//[![:print:]]/ }" session="${2//[![:print:]]/ }" rest="$3"
  printf '%s sweep project=%s session=%s %s\n' "$(date -Is)" "$project" "$session" "$rest" >> "$LOG_FILE"
}

# derive_args builds the common window argv into DERIVE_ARGS. Epochs are
# appended only when pinned (>0) so whole-session runs keep their historical
# run identity. RAW project/session go to argv — each is one quoted argv
# element, and mutating a foreign-harness session id (dots, underscores, ...)
# would silently derive the wrong session.
derive_args() { # project session lo hi
  # -max-prompt-bytes carries the sweep ceiling into the prompt builder
  # (D211 step 3, review round-1 finding 2): the advisory already-captured
  # section yields to this headroom, so a window that fits on its own can
  # never be pushed over the ceiling — and into an unsplittable-window
  # loop — by fixed context. Applies to dry-run (honest sizing) and execute
  # (the run builds the same prompt the dry-run sized).
  #
  # The ONE exception is the ceiling probe: while CEILING_RESOLVED is 0 (env
  # override unset, first dry-run of the run not yet read) the flag is
  # omitted so the CLI applies the endpoint profile and reports it back in
  # effective_limits. resolve_ceiling settles the number from that first
  # dry-run and every later call — split legs and executes included — passes
  # it explicitly again: one resolution per sweep invocation, deterministic
  # argv, and the local (( pb <= MAX_PROMPT_BYTES )) comparison stays
  # consistent with the ceiling the CLI itself enforces.
  DERIVE_ARGS=(-provider "$PROVIDER" -model "$MODEL" -endpoint-id "$ENDPOINT_ID"
    -project "$1" -session-id "$2" -json)
  (( CEILING_RESOLVED )) && DERIVE_ARGS+=(-max-prompt-bytes "$MAX_PROMPT_BYTES")
  [[ -n "$REASONING_EFFORT" ]] && DERIVE_ARGS+=(-reasoning-effort "$REASONING_EFFORT")
  [[ "$3" -gt 0 ]] && DERIVE_ARGS+=(-start-epoch "$3")
  [[ "$4" -gt 0 ]] && DERIVE_ARGS+=(-end-epoch "$4")
  return 0
}

# run_execute performs one persisted derivation run for the window and logs
# the sanitized outcome (including denylist and min-content skip markers the
# binary emits). Leg counters feed the all-fail exit gate (D166 ruling (1),
# extended to the run-status layer by L4(a) — memoire:D198): a recorded
# failed run is rc=0 by documented policy (docs/MODEL-PROVIDER.md "Failure
# Modes" — the 89-failures class of 2026-07-22 was invisible at every
# level), so the leg is also judged on the "status" field of the execute
# JSON. Only status "failed" counts: partial/completed and the guard skip
# markers stay successes, and duplicate:true legs are excluded — a failed
# run is normally retried in place (never deduped), but the LOSER of a
# concurrent failed->running retry CAS returns Duplicate:true with the
# stale failed status (EnsureDerivationRun; review round-1 finding 3).
LEGS_ATTEMPTED=0
LEGS_FAILED=0
LEGS_STATUS_FAILED=0
run_execute() { # project session lo hi provisional
  local window=""
  [[ "$3" -gt 0 || "$4" -gt 0 ]] && window="window=$3..$4 "
  derive_args "$1" "$2" "$3" "$4"
  # -provisional marks the OPEN TAIL only (Fibery #344): the leg that ends at
  # the last event so far, derived while the session may still be running.
  # It rides the execute argv, never derive_args, which the dry-run shares —
  # a sizing probe has no run to mark.
  local -a prov_args=()
  [[ "${5:-0}" == "1" ]] && prov_args+=(-provisional)
  local out rc
  out="$("$DERIVE_BIN" -strategy=model -execute "${DERIVE_ARGS[@]}" \
    ${SUPERSEDE_ARGS[@]+"${SUPERSEDE_ARGS[@]}"} \
    ${prov_args[@]+"${prov_args[@]}"} \
    -execute-timeout "$EXECUTE_TIMEOUT" 2>&1)"
  rc=$?
  LEGS_ATTEMPTED=$((LEGS_ATTEMPTED + 1))
  SESSION_LEGS=$((SESSION_LEGS + 1))
  if (( rc != 0 )); then
    LEGS_FAILED=$((LEGS_FAILED + 1))
  elif [[ "$(json_str "$out" status)" == "failed" \
        && "$(json_bool "$out" duplicate)" != "true" ]]; then
    # duplicate:true is excluded (review round-1 finding 3): a caller that
    # LOSES the failed->running retry CAS gets Duplicate:true with the
    # STALE failed status while the CAS winner is actually retrying the
    # run — that is another lane's live work, not this leg's failure.
    LEGS_STATUS_FAILED=$((LEGS_STATUS_FAILED + 1))
  fi
  log_line "$1" "$2" "${window}rc=$rc $(printf '%s' "$out" | tr -c '[:print:]' ' ' | tail -c 1500)"
}

# json_int extracts a top-level integer field from the CLI's MarshalIndent
# output (exactly two-space indent, so nested prompt_meta fields never match).
json_int() { # json field
  printf '%s\n' "$1" | sed -n "s/^  \"$2\": \([0-9]\{1,\}\).*/\1/p" | head -n 1
}

# json_str extracts a top-level string field from the same MarshalIndent
# framing (two-space indent; nested fields — usage, prompt_meta — never
# match). Values are [a-z_] by contract (run statuses).
json_str() { # json field
  printf '%s\n' "$1" | sed -n "s/^  \"$2\": \"\([a-z_]\{1,\}\)\".*/\1/p" | head -n 1
}

# json_bool extracts a top-level boolean field, same framing.
json_bool() { # json field
  printf '%s\n' "$1" | sed -n "s/^  \"$2\": \(true\|false\).*/\1/p" | head -n 1
}

# json_block isolates one top-level object field's body, so the readers below
# cannot match a same-named key inside a sibling block (prompt_meta carries
# its own max_prompt_bytes). MarshalIndent puts the opening line and the
# closing brace at two-space indent and the members at four.
json_block() { # json field
  printf '%s\n' "$1" | sed -n "/^  \"$2\": {/,/^  }/p"
}

# json_sub_int / json_sub_str read one member of such a block.
json_sub_int() { # json block member
  json_block "$1" "$2" | sed -n "s/^    \"$3\": \([0-9]\{1,\}\).*/\1/p" | head -n 1
}

json_sub_str() { # json block member
  json_block "$1" "$2" | sed -n "s/^    \"$3\": \"\([a-z_]\{1,\}\)\".*/\1/p" | head -n 1
}

# resolve_ceiling settles this sweep's prompt-byte ceiling ONCE, from the
# first dry-run's effective_limits block, and marks it resolved so every
# later argv carries the number explicitly. The endpoint is fixed per sweep
# invocation, so one resolution covers the whole run.
#
# The profile governs only when it actually produced the number: source
# "profile" (per-limit key first — the CLI splits the sources when the two
# limits disagree) and a positive value. Every other outcome keeps the
# script fallback: an older or foreign JSON with no effective_limits block,
# an empty or non-numeric value, source "flag"/"default", or a dry-run that
# failed outright (its output parses to nothing here, and the caller's own
# error handling is untouched).
resolve_ceiling() { # dry_run_json
  (( CEILING_RESOLVED )) && return 0
  CEILING_RESOLVED=1
  local value source
  value="$(json_sub_int "$1" effective_limits max_prompt_bytes)"
  source="$(json_sub_str "$1" effective_limits max_prompt_bytes_source)"
  [[ -n "$source" ]] || source="$(json_sub_str "$1" effective_limits source)"
  if [[ "$source" == "profile" && "$value" =~ ^[0-9]+$ ]] && (( value > 0 )); then
    MAX_PROMPT_BYTES="$value"
  fi
  printf '%s sweep ceiling=%s source=%s endpoint=%s\n' \
    "$(date -Is)" "$MAX_PROMPT_BYTES" "${source:-fallback}" \
    "${ENDPOINT_ID//[![:print:]]/ }" >> "$LOG_FILE"
  return 0
}

# median_epoch returns the median canonical-event epoch inside [lo, hi] for
# the session — the split point for an oversized window. lo/hi are validated
# integers. The binary owns the query (engine-agnostic) and takes the
# canonical type list from the prompt builder itself, so the split always
# sees exactly the events the builder admits. RAW project/session go to
# argv, same contract as derive_args. Failure and empty window are both
# empty output — the caller's unsplittable-window marker covers them.
median_epoch() { # project session lo hi
  "$DERIVE_BIN" -median-epoch -project "$1" -session-id "$2" \
    -start-epoch "$3" -end-epoch "$4" 2>/dev/null
}

# cut_points prints the session's RHYTHM cut epochs, one per line, ordered —
# the first splitter, ahead of the byte-ceiling halving above. Empty output
# means the window needs no cut and derives whole. The binary owns the
# detector (memoire:D162: all derive-lane store access goes through its helper
# flags) and walks the same population the prompt builder admits, so the cuts
# always land where the model's own input has a seam.
#
# Fail-open, but NOT silently (unlike median_epoch, whose empty output is
# already covered by the caller's unsplittable-window marker): a detector
# failure here would quietly revert the whole lane to time slicing, so it is
# logged and then treated as "no cuts".
cut_points() { # project session
  local out rc
  out="$("$DERIVE_BIN" -cut-points -project "$1" -session-id "$2" \
    ${CUT_ARGS[@]+"${CUT_ARGS[@]}"} 2>&1)"
  rc=$?
  if (( rc != 0 )); then
    log_line "$1" "$2" "cuts=failed rc=$rc $(printf '%s' "$out" | tr -c '[:print:]' ' ' | tail -c 200)"
    return 0
  fi
  printf '%s' "$out"
}

# derive_window sizes the window with a local dry-run and either executes it
# or recursively halves it on the median event epoch.
derive_window() { # project session lo hi depth provisional
  local project="$1" session="$2" lo="$3" hi="$4" depth="$5" provisional="${6:-0}"
  if (( SESSION_LEGS >= SESSION_LEG_BUDGET )); then
    log_line "$project" "$session" "window=$lo..$hi rc=0 {\"skipped\":\"session_leg_budget_exhausted\",\"budget\":$SESSION_LEG_BUDGET}"
    return
  fi
  derive_args "$project" "$session" "$lo" "$hi"
  local dry pb
  dry="$("$DERIVE_BIN" -strategy=model -dry-run "${DERIVE_ARGS[@]}" 2>&1)"
  # Settle the ceiling before anything reads it — including the fall-back
  # execute below, which must not send an uncapped prompt because the sizing
  # probe failed.
  resolve_ceiling "$dry"
  pb="$(json_int "$dry" prompt_bytes)"
  if [[ ! "$pb" =~ ^[0-9]+$ ]]; then
    # Unexpected dry-run failure: fall back to a direct execute, which records
    # its own honest outcome (fail-open on the sizing probe, never silent).
    run_execute "$project" "$session" "$lo" "$hi" "$provisional"
    return
  fi
  if (( pb <= MAX_PROMPT_BYTES )); then
    run_execute "$project" "$session" "$lo" "$hi" "$provisional"
    return
  fi
  if (( depth >= MAX_SPLIT_DEPTH )); then
    log_line "$project" "$session" "window=$lo..$hi rc=0 {\"skipped\":\"split_depth_exceeded\",\"prompt_bytes\":$pb,\"depth\":$depth}"
    return
  fi
  # Resolve concrete bounds from the dry-run when the window is unpinned.
  if (( lo == 0 || hi == 0 )); then
    lo="$(json_int "$dry" window_start_epoch)"
    hi="$(json_int "$dry" window_end_epoch)"
    if [[ ! "$lo" =~ ^[0-9]+$ || ! "$hi" =~ ^[0-9]+$ ]]; then
      log_line "$project" "$session" "rc=0 {\"skipped\":\"unsplittable_window\",\"prompt_bytes\":$pb,\"reason\":\"no resolved bounds\"}"
      return
    fi
  fi
  local mid
  mid="$(median_epoch "$project" "$session" "$lo" "$hi")"
  [[ "$mid" =~ ^[0-9]+$ ]] || mid=""
  # Left leg is [lo, mid], right leg is [mid+1, hi]: keep the right leg
  # non-empty by pulling the split point below hi when the median sits on it.
  if [[ -n "$mid" ]] && (( mid >= hi )); then
    mid=$((hi - 1))
  fi
  if [[ -z "$mid" ]] || (( mid < lo )); then
    log_line "$project" "$session" "window=$lo..$hi rc=0 {\"skipped\":\"unsplittable_window\",\"prompt_bytes\":$pb}"
    return
  fi
  log_line "$project" "$session" "window=$lo..$hi rc=0 {\"split\":true,\"prompt_bytes\":$pb,\"mid\":$mid,\"depth\":$depth}"
  # Halving an open-tail window: only the RIGHT leg still ends at the tail, so
  # the marker travels with it. The left leg is a closed interval whose events
  # are all in the past, exactly like an inner rhythm segment.
  derive_window "$project" "$session" "$lo" "$mid" $((depth + 1)) 0
  derive_window "$project" "$session" $((mid + 1)) "$hi" $((depth + 1)) "$provisional"
}

# derive_session is the per-session entry: rhythm cuts first, then the existing
# dry-run + ceiling + halving path per segment.
#
# Segment bounds reuse the epoch-inclusive convention the halving split already
# applies: cuts c1..ck yield [0, c1], [c1+1, c2], ..., [ck+1, 0]. The OUTER
# edges stay unpinned on purpose — the CLI accepts a half-pinned window when
# -session-id is given, and leaving the last segment open-ended means events
# committed between the cut-points read and that segment's execute are still
# picked up, instead of waiting a whole tick. derive_window then resolves each
# segment's concrete bounds from its own dry-run, exactly as it already does
# for an unpinned whole-session window.
#
# Segments enter at depth 1: a rhythm cut IS a split, so it consumes one level
# of the halving budget rather than handing every segment a fresh one.
# Detection ran once for the whole window, so the segments hold no further cut
# points by construction and the pass never recurses.
derive_session() { # project session
  local project="$1" session="$2"
  SESSION_LEGS=0
  local -a cuts=()
  local last=0 raw="" c malformed=0 badline=""
  if [[ "$RHYTHM_CUTS" != "0" ]]; then
    raw="$(cut_points "$project" "$session")"
  fi
  if [[ -n "$raw" ]]; then
    while IFS= read -r c; do
      # The cut list is ALL-OR-NOTHING. One unusable line condemns the whole
      # output: a helper that emitted garbage has not told us the surviving
      # lines are meaningful, so trusting them would derive a partial,
      # silently wrong segmentation — the exact "partially trusted" posture
      # this lane's fail-open contract exists to refuse (review r1 F1).
      # Unusable = not a plausible epoch (1-19 digits, so the arithmetic
      # below cannot overflow) or not strictly greater than the previous cut,
      # which also rejects a 0 the helper should never emit.
      if [[ ! "$c" =~ ^[0-9]{1,19}$ ]] || (( c <= last )); then
        malformed=1
        badline="$c"
        break
      fi
      cuts+=("$c")
      last="$c"
    done <<< "$raw"
  fi
  if (( malformed )); then
    # Same cuts=failed marker family as the rc!=0 path in cut_points, so one
    # grep finds every degradation of this pass whatever caused it.
    log_line "$project" "$session" \
      "cuts=failed reason=malformed_output line=$(printf '%s' "$badline" | tr -c '[:print:]' ' ' | tail -c 60)"
    cuts=()
  fi
  # An EMPTY helper output is not malformed: it is the ordinary "this window
  # needs no cut" answer, and it lands here the same way, deriving whole.
  if (( ${#cuts[@]} == 0 )); then
    # The whole-session window ends at the last event so far, so it IS the open
    # tail (Fibery #344) — including on the fail-open paths above, where a
    # detector failure or a malformed cut list lands here.
    derive_window "$project" "$session" 0 0 0 1
    return
  fi
  local list=""
  for c in "${cuts[@]}"; do
    [[ -n "$list" ]] && list="$list,$c" || list="$c"
  done
  log_line "$project" "$session" "rc=0 {\"rhythm\":true,\"segments\":$(( ${#cuts[@]} + 1 )),\"cuts\":[$list]}"
  local lo=0
  for c in "${cuts[@]}"; do
    # Inner segments are CLOSED at a cut the detector already saw: their arc
    # ended, so they are never provisional.
    derive_window "$project" "$session" "$lo" "$c" 1 0
    lo=$((c + 1))
  done
  # The trailing segment is the open tail.
  derive_window "$project" "$session" "$lo" 0 1 1
}

for row in "${ROWS[@]}"; do
  project="${row%%"$US"*}"
  session="${row#*"$US"}"
  [[ -n "$project" && -n "$session" ]] || continue
  derive_session "$project" "$session"
done

# No-work intent settlement (memoire:D165, policy RATIFIED by the
# operator 2026-07-23): open intents older than the TTL whose session has
# no live canonical events close as expired — they can never be served by a
# run or guard skip. Fail-open: a settle failure logs and the next tick
# retries.
if settle_out="$("$DERIVE_BIN" -settle-intents -intent-ttl-hours "${MEMOIRE_INTENT_TTL_HOURS:-48}" 2>&1)"; then
  [[ "$settle_out" == "settled=0" ]] || log_line "-" "-" "intents $settle_out"
else
  log_line "-" "-" "intents settle_failed $(printf '%s' "$settle_out" | tr -c '[:print:]' ' ' | tail -c 200)"
fi

# Review-TTL sweep (slice E, memoire:D146, R-6): proposed observations never
# opened within the TTL lapse under the attributable system actor. Fail-open:
# a dead API must not fail the derive sweep — the next hourly run retries.
#
# MEMOIRE_LAPSE_PAUSED=1 is the DURABLE PAUSE SPELLING (triage lane v1, R-E).
# The interim pause is an enormous TTL (36500 days) set cluster-side, which
# works but lies: the lane looks armed and silently never fires, and nothing
# in the repo says the timer is off. This guard says it out loud — the call
# is skipped and the skip is LOGGED, so a paused lane is visible in the same
# place an active one is. The TTL pass-through is kept: pausing and tuning
# are different acts and must not share one knob.
LAPSE_ADDR="${MEMOIRE_ADDR:-127.0.0.1:37778}"
LAPSE_TOKEN_FILE="$MEMOIRE_HOME/token"
if [[ "${MEMOIRE_LAPSE_PAUSED:-0}" == "1" ]]; then
  log_line "-" "-" "lapse-expired skipped=lapse_paused"
elif [[ -f "$LAPSE_TOKEN_FILE" ]]; then
  lapse_out="$(curl -sf -m 30 -X POST "http://$LAPSE_ADDR/v1/admin/lapse-expired" \
    -H "Authorization: Bearer $(cat "$LAPSE_TOKEN_FILE")" \
    -H "Content-Type: application/json" \
    -d "{\"ttl_days\":${MEMOIRE_LAPSE_TTL_DAYS:-0}}" 2>/dev/null)" || lapse_out=""
  [[ -n "$lapse_out" ]] && log_line "-" "-" "lapse-expired $lapse_out"
fi

# D166 ruling (1), extended by L4(a) (memoire:D198): a sweep whose EVERY
# executed leg failed — at the exit-code layer OR the recorded-run-status
# layer — must not look green: exit non-zero so ExecStartPost / the
# CronJob record-success step is skipped and the lane surfaces red. The
# ruled scope is preserved exactly: partial success (any leg completed/
# partial/guard-served) still records lane success; structural skips
# (split_depth_exceeded, unsplittable_window) never call run_execute and
# sit outside both counters. The 42P01 window recorded a lane-success
# epoch over 100% rc=1 legs (exit-code layer, closed by D166); the
# 89-failures night recorded status="failed" on every run at rc=0
# (run-status layer, closed here).
if (( LEGS_ATTEMPTED > 0 && LEGS_FAILED + LEGS_STATUS_FAILED == LEGS_ATTEMPTED )); then
  log_line "-" "-" "all-legs-failed attempted=$LEGS_ATTEMPTED rc_failed=$LEGS_FAILED status_failed=$LEGS_STATUS_FAILED"
  exit 1
fi

exit 0
