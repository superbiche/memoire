#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-derive-hl5-trigger.sh — early-fire gate for the nightly HL5 lane
# (memoire:D163, "A with B's trigger"). Runs after the hourly HL3 sweep
# (systemd: ExecStartPost; in-cluster: the hourly CronJob script invokes it
# after a SUCCESSFUL sweep only — a failed tick is an anomaly, never a
# reason to wake HL5). When the tick leaves more than N distinct oversized
# legs waiting (split_depth_exceeded — windows HL3 can never derive) AND
# the store confirms their sessions are still uncovered, start the
# Conductor-routed HL5 sweep early instead of waiting for 04:00.
#
# Two fire forms (memoire:D165 R-10.3, round-2 operator ruling
# 2026-07-28 — imperative in-cluster fire DROPPED, zero RBAC):
#   systemd host lane — the main path below: `systemctl --user start` of
#     HL5_SERVICE after its own guards (workstation hub / host-lane
#     capability, retained by the L3-form ruling).
#   kubernetes — the early-fire LANE: a third CronJob scheduled hourly
#     inside quiet hours runs `--lane-fresh-check` then
#     `--early-fire-check` and proceeds into the HL5 sweep in the SAME
#     Job when the gate passes. No API-server writes, no ServiceAccount,
#     every Job CronJob-owned (history limits + concurrencyPolicy Forbid
#     apply per lane). Cross-lane overlap with the 04:03 nightly is
#     bounded by the shared recency epoch (a COMPLETED early run holds
#     the nightly) and the sweep's own store-level serialization; the
#     residual — an early Job still running at 04:03 — costs a duplicate
#     prewarm of an already-awake node and is journaled, not hidden.
#
# Modes for the nightly service unit:
#   --lane-fresh-check   ExecCondition: exits 1 (clean skip) outside quiet
#                        hours or when the lane already completed within
#                        RECENT_S. Deliberate out-of-band runs go through
#                        the sweep SCRIPT directly with the HL5 env set —
#                        never through a force env on the unit (a sticky
#                        `systemctl set-environment` would silently bypass
#                        the safety gates forever; round-2 LOW-NEW-3).
#   --record-success     ExecStartPost: records the lane-success epoch.
#                        This FILE is the only recency evidence —
#                        systemd's Result/InactiveEnterTimestamp are
#                        refreshed by a condition-skip activation and can
#                        masquerade as success (round-2 HIGH-NEW-1).
#
# Guards (operator constraints, 2026-07-22 — HL5 lives in the office during
# a hot summer):
#   1. QUIET HOURS: fire only between QUIET_START and QUIET_END local time
#      (Europe/Paris on this host).
#   2. RECENCY: hold within RECENT_S (default 20h) of a RECORDED lane
#      success — lane recency bounds wake churn. The rounds-1-4 KNOWN
#      RESIDUAL (stale split_depth_exceeded markers re-marked every tick
#      after HL5 already derived the events, degenerating to one no-op
#      wake per night) is CLOSED by R-10.3: markers are now CANDIDATES
#      only, and a leg counts toward the threshold ONLY when the store
#      still reports its (project, session) uncovered
#      (`memoire-derive -list-outstanding` — the same events x
#      derivation_runs computation as GET /v1/derive/outstanding, via the
#      colocated binary rather than HTTP: the pod already holds direct
#      store access for the sweep, and a read via the binary mints no new
#      token/netpol seam).
#   3. ROLLING COOLDOWN: at least COOLDOWN_S (default 24h) between early
#      fires — at most one early fire per rolling day (round-2 MED-NEW-1),
#      recorded ONLY after an accepted start.
# Kill switches: the GLOBAL MEMOIRE_AUTO_DERIVE=0 (covers hook, sweeps,
# and this whole HL5 lane — "off" must never WOL a node; round-3
# HIGH-NEW-1) and the narrower MEMOIRE_HL5_TRIGGER=0. Anomalies (malformed
# state, query
# timeouts, parser misalignment) HOLD — the 04:00 timer is the safety net,
# and a missed optional early fire is always safe (round-2 LOW-NEW-1).

MEMOIRE_HOME="${MEMOIRE_HOME:-$HOME/.memoire}"
LOG_DIR="$MEMOIRE_HOME/log"
LOG_FILE="$LOG_DIR/derive-auto.log"
# Session-granular metric (round-2 operator ruling): the threshold counts
# outstanding sessions with >=1 in-window oversized marker — NOT marker
# legs, which overcount whenever covered and uncovered islands coexist in
# one session (round-1 finding 2). Default 3 is PROVISIONAL, tunable, with
# an explicit revisit trigger on the Q1 notify->completion latency
# journal's measured data.
THRESHOLD="${MEMOIRE_HL5_TRIGGER_SESSIONS:-3}"
QUIET_START="${MEMOIRE_HL5_QUIET_START:-22}"   # inclusive, 24h clock
QUIET_END="${MEMOIRE_HL5_QUIET_END:-7}"        # exclusive
WINDOW_MIN="${MEMOIRE_HL5_TRIGGER_WINDOW_MIN:-70}"
RECENT_S="${MEMOIRE_HL5_RECENT_S:-72000}"      # 20h
COOLDOWN_S="${MEMOIRE_HL5_COOLDOWN_S:-86400}"  # 24h rolling: one fire/day
HL5_SERVICE="${MEMOIRE_HL5_SERVICE:-memoire-derive-sweep-hl5.service}"
COOLDOWN_FILE="$LOG_DIR/.hl5-early-fire-epoch"
SUCCESS_FILE="$LOG_DIR/.hl5-lane-success-epoch"
SYSTEMCTL_TIMEOUT="${MEMOIRE_HL5_SYSTEMCTL_TIMEOUT:-10}"
OUTSTANDING_HOURS="${MEMOIRE_HL5_OUTSTANDING_HOURS:-24}"
# The derive binary next to this checkout (MEMOIRE_DERIVE_BIN overrides).
DERIVE_BIN="${MEMOIRE_DERIVE_BIN:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bin/memoire-derive}"

mark() { printf '%s hl5-trigger %s\n' "$(date -Is)" "$1" >> "$LOG_FILE" 2>/dev/null; }

# read_epoch_file <path>: prints a validated epoch, or "absent", or
# "malformed". Callers decide the policy per contract.
read_epoch_file() {
  local f="$1" v
  [[ -e "$f" ]] || { echo absent; return; }
  v="$(head -c 32 "$f" 2>/dev/null | tr -d '[:space:]')"
  [[ "$v" =~ ^[0-9]{1,12}$ ]] && echo "$v" || echo malformed
}

# write_epoch <file>: atomic same-directory write (tmp + rename), every
# step checked — a silently absent state file would erase both wake-churn
# guards (round-3 MED-NEW-1).
write_epoch() {
  local f="$1" tmp
  tmp="$(umask 077 && mktemp "$(dirname "$f")/.epoch.XXXXXX" 2>/dev/null)" || return 1
  if date +%s > "$tmp" 2>/dev/null && mv -f "$tmp" "$f" 2>/dev/null; then
    return 0
  fi
  rm -f "$tmp" 2>/dev/null
  return 1
}

# in_quiet_hours: handles the midnight-wrapping window (22 -> 7).
# MEMOIRE_HL5_NOW_HOUR is a test-only override for boundary coverage.
in_quiet_hours() {
  local hour
  hour="${MEMOIRE_HL5_NOW_HOUR:-$((10#$(date +%H)))}"
  hour=$((10#$hour))
  if (( QUIET_START > QUIET_END )); then
    (( hour >= QUIET_START || hour < QUIET_END ))
  else
    (( hour >= QUIET_START && hour < QUIET_END ))
  fi
}

# lane_recency: prints "recent", "stale", "none", or "anomaly" from the
# recorded success epoch (never from systemd properties — HIGH-NEW-1).
lane_recency() {
  local v
  v="$(read_epoch_file "$SUCCESS_FILE")"
  case "$v" in
    absent) echo none ;;
    malformed) echo anomaly ;;
    *) if (( $(date +%s) - v < RECENT_S )); then echo recent; else echo stale; fi ;;
  esac
}

# count_confirmed_sessions — the session-granular store-confirmed metric
# shared by the systemd main path and --early-fire-check. Counts
# outstanding (project, session) pairs with at least one in-window
# split_depth_exceeded marker. Timestamps convert in ONE `date -f` batch
# (offset-aware, DST-fold safe — round-1 LOW-2); the scan cap keeps the
# newest suffix so truncation can only UNDER-count (safe direction).
# Confirmation searches the store-truth identifier inside the RAW marker
# line (grep -F): identifiers may contain spaces, so whitespace
# tokenization cannot reassemble them (round-1 finding 3). Store
# identifiers with whitespace/backslashes are SKIPPED with a marker — an
# unconfirmed candidate under-counts. An empty outstanding list is NOT an
# anomaly: it is the staleness fix working (every marker refers to
# already-derived events). Documented residual: an adapter-supplied
# session id embedding the literal " window=" could forge a match;
# adapters mint UUID-shaped ids, and the forgery still needs
# store-confirmed outstanding work to count.
# Sets CONFIRMED_SESSIONS and returns 0; returns 1 after writing a held=
# marker on any anomaly (timestamp misalignment, query failure,
# malformed outstanding row).
count_confirmed_sessions() {
  CONFIRMED_SESSIONS=0
  local cutoff skipped row p s
  cutoff=$(( $(date +%s) - WINDOW_MIN * 60 ))
  raw_file="$(umask 077 && mktemp "${TMPDIR:-/tmp}/memoire-hl5-raw.XXXXXX")" || return 1
  epochs_file="$(umask 077 && mktemp "${TMPDIR:-/tmp}/memoire-hl5-epochs.XXXXXX")" || { rm -f "$raw_file"; return 1; }
  cand_file="$(umask 077 && mktemp "${TMPDIR:-/tmp}/memoire-hl5-cand.XXXXXX")" || { rm -f "$raw_file" "$epochs_file"; return 1; }
  keep_file="$(umask 077 && mktemp "${TMPDIR:-/tmp}/memoire-hl5-keep.XXXXXX")" || { rm -f "$raw_file" "$epochs_file" "$cand_file"; return 1; }
  trap 'rm -f "$raw_file" "$epochs_file" "$cand_file" "$keep_file"' EXIT
  grep 'split_depth_exceeded' "$LOG_FILE" 2>/dev/null | tail -n "${MEMOIRE_HL5_SCAN_LINES:-50000}" > "$raw_file"
  if [[ -s "$raw_file" ]]; then
    awk '{print $1}' "$raw_file" | date -f - +%s > "$epochs_file" 2>/dev/null
    if [[ "$(wc -l < "$epochs_file")" != "$(wc -l < "$raw_file")" ]]; then
      mark "held=timestamp_parse_anomaly"
      return 1
    fi
    paste "$epochs_file" "$raw_file" \
      | awk -F'\t' -v c="$cutoff" '$1 >= c { line = $0; sub(/^[^\t]*\t/, "", line); print line }' > "$cand_file"
  fi
  [[ -s "$cand_file" ]] || return 0
  # Ambiguity filter (round-2 F3): a marker line is trusted only when
  # " project=", " session=", " window=" each occur EXACTLY once. A
  # space-bearing identifier that embeds another field's key (session
  # "x project=p1 session=s1") would otherwise let a forged marker match
  # a SAFE outstanding pair — an over-count, the wrong direction. With
  # unique keys the grep -F match below is injective: the matched
  # occurrence is necessarily the real field and the identifiers compare
  # exactly. Dropped lines under-count (safe) and leave a marker.
  local dropped
  dropped="$(awk -v keep="$keep_file" '
    { a = $0
      p = gsub(/ project=/, "&", a)
      s = gsub(/ session=/, "&", a)
      w = gsub(/ window=/, "&", a)
      if (p == 1 && s == 1 && w == 1) print > keep; else d++ }
    END { print d + 0 }' "$cand_file")"
  if (( dropped > 0 )); then
    mark "ambiguous_markers_dropped n=$dropped"
  fi
  # A fully-dropped candidate set leaves no keep file: settle at 0 (safe).
  mv -f "$keep_file" "$cand_file" 2>/dev/null || return 0
  [[ -s "$cand_file" ]] || return 0
  local outstanding
  if ! outstanding="$(timeout "$SYSTEMCTL_TIMEOUT" "$DERIVE_BIN" -list-outstanding -since-hours "$OUTSTANDING_HOURS" 2>/dev/null)"; then
    mark "held=outstanding_query_failed"
    return 1
  fi
  skipped=0
  while IFS= read -r row; do
    [[ -z "$row" ]] && continue
    if [[ "$row" != *$'\x1f'* ]]; then
      mark "held=outstanding_parse_anomaly"
      return 1
    fi
    p="${row%%$'\x1f'*}"
    s="${row#*$'\x1f'}"
    if [[ -z "$p" || -z "$s" || "$s" == *$'\x1f'* ]]; then
      mark "held=outstanding_parse_anomaly"
      return 1
    fi
    if [[ "$p$s" =~ [[:space:]\\] ]]; then
      skipped=$((skipped + 1))
      continue
    fi
    if grep -qF " project=$p session=$s window=" "$cand_file"; then
      CONFIRMED_SESSIONS=$((CONFIRMED_SESSIONS + 1))
    fi
  done <<< "$outstanding"
  (( skipped > 0 )) && mark "outstanding_unsafe_identifiers_skipped n=$skipped"
  return 0
}

case "${1:-}" in
--record-success)
  mkdir -p "$LOG_DIR" 2>/dev/null || exit 1
  if write_epoch "$SUCCESS_FILE"; then
    mark "lane-success-recorded"
    exit 0
  fi
  # Persistence failed: report failure so the unit surfaces it — an
  # unrecorded success would let the next tick wake HL5 again. The file
  # marker is best-effort (the same unwritable directory likely blocks
  # it), so the journal gets the explanation via stderr.
  mark "lane-success-record-FAILED"
  echo "memoire-derive-hl5-trigger: lane success epoch could not be persisted" >&2
  exit 1
  ;;
--lane-fresh-check)
  if [[ "${MEMOIRE_AUTO_DERIVE:-1}" == "0" ]]; then
    mark "nightly-skip=auto_derive_disabled"
    exit 1
  fi
  if ! in_quiet_hours; then
    mark "nightly-skip=outside_quiet_hours"
    exit 1
  fi
  case "$(lane_recency)" in
    recent) mark "nightly-skip=lane_recent_success"; exit 1 ;;
    anomaly) mark "nightly-skip=success_state_anomaly"; exit 1 ;;
  esac
  exit 0
  ;;
--early-fire-check)
  # Gate for the cluster's early-fire LANE (round-2 pivot, operator-
  # ratified 2026-07-28). The lane script runs --lane-fresh-check FIRST
  # (kill switches, quiet hours, recency), so this mode owns the rest of
  # the retired imperative fire's guards: rolling cooldown, the
  # store-confirmed session count, and the cooldown record on pass.
  # exit 0 = proceed to the HL5 sweep inside THIS Job (all Jobs
  # CronJob-owned — no API-server writes, no RBAC); exit 1 = clean skip.
  # Ruling 3a pin: the cooldown gates ONLY this lane — --lane-fresh-check
  # never reads it, so a consumed cooldown can never starve the 04:03
  # nightly backstop.
  [[ "${MEMOIRE_AUTO_DERIVE:-1}" == "0" ]] && exit 1
  [[ "${MEMOIRE_HL5_TRIGGER:-1}" == "0" ]] && exit 1
  mkdir -p "$LOG_DIR" 2>/dev/null || exit 1
  command -v timeout >/dev/null 2>&1 || exit 1
  [[ -r "$LOG_FILE" ]] || { mark "early-skip=no_sweep_log"; exit 1; }
  cooldown_state="$(read_epoch_file "$COOLDOWN_FILE")"
  case "$cooldown_state" in
    absent) : ;;
    malformed) mark "early-skip=cooldown_state_anomaly"; exit 1 ;;
    *) (( $(date +%s) - cooldown_state < COOLDOWN_S )) && exit 1 ;;
  esac
  count_confirmed_sessions || exit 1
  if (( CONFIRMED_SESSIONS > THRESHOLD )); then
    # Cooldown records BEFORE the sweep: an accepted start consumes the
    # day's fire even if the sweep later fails (parity with the retired
    # imperative semantics). An unrecordable cooldown must NOT proceed —
    # the next tick would have no cooldown to obey (wake-churn guard,
    # round-3 MED-NEW-1).
    if ! write_epoch "$COOLDOWN_FILE"; then
      mark "early-skip=cooldown_record_failed"
      echo "memoire-derive-hl5-trigger: cooldown record failed; early fire skipped" >&2
      exit 1
    fi
    mark "early-fire sessions=$CONFIRMED_SESSIONS threshold=$THRESHOLD"
    exit 0
  fi
  mark "early-held sessions=$CONFIRMED_SESSIONS threshold=$THRESHOLD"
  exit 1
  ;;
esac

# --- systemd host-lane main path (host-lane capability retained by the
# L3-form ruling; the cluster uses the early-fire LANE above instead) ----
[[ "${MEMOIRE_AUTO_DERIVE:-1}" == "0" ]] && exit 0
[[ "${MEMOIRE_HL5_TRIGGER:-1}" == "0" ]] && exit 0
mkdir -p "$LOG_DIR" 2>/dev/null || exit 0
[[ -r "$LOG_FILE" ]] || exit 0
command -v systemctl >/dev/null 2>&1 || exit 0
command -v timeout >/dev/null 2>&1 || exit 0

in_quiet_hours || exit 0

# Rolling cooldown between early fires. Malformed state = anomaly = hold.
cooldown_state="$(read_epoch_file "$COOLDOWN_FILE")"
case "$cooldown_state" in
  absent) : ;;
  malformed) mark "held=cooldown_state_anomaly"; exit 0 ;;
  *) (( $(date +%s) - cooldown_state < COOLDOWN_S )) && exit 0 ;;
esac

case "$(lane_recency)" in
  recent) exit 0 ;;
  anomaly) mark "held=success_state_anomaly"; exit 0 ;;
esac

# Overlap guard. Only an explicitly confirmed not-running state proceeds;
# a timeout, a query failure, or any other answer is unknown-or-running
# and HOLDS (round-3 LOW-NEW-1).
state="$(timeout "$SYSTEMCTL_TIMEOUT" systemctl --user is-active "$HL5_SERVICE" 2>/dev/null)"
rc=$?
if (( rc == 124 )); then
  mark "held=is_active_timeout"
  exit 0
fi
if [[ "$state" != "inactive" && "$state" != "failed" ]]; then
  [[ "$state" == "active" || "$state" == "activating" ]] || mark "held=is_active_anomaly state=${state:-empty} rc=$rc"
  exit 0
fi

count_confirmed_sessions || exit 0

if (( CONFIRMED_SESSIONS > THRESHOLD )); then
  if timeout "$SYSTEMCTL_TIMEOUT" systemctl --user start --no-block "$HL5_SERVICE" 2>/dev/null; then
    if write_epoch "$COOLDOWN_FILE"; then
      mark "fired sessions=$CONFIRMED_SESSIONS threshold=$THRESHOLD"
    else
      # The start was accepted but the cooldown could not be persisted: say
      # so on stderr too (journal) — the next tick has no cooldown to obey.
      mark "fired-cooldown-record-FAILED sessions=$CONFIRMED_SESSIONS"
      echo "memoire-derive-hl5-trigger: cooldown record failed after accepted start" >&2
    fi
  else
    mark "start_failed service=$HL5_SERVICE sessions=$CONFIRMED_SESSIONS"
  fi
else
  mark "held sessions=$CONFIRMED_SESSIONS threshold=$THRESHOLD"
fi
exit 0
