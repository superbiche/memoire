#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-capture-health.sh — one check of a client's capture path, run by the
# memoire-capture-health user timer (MISSING #19). Session-start injection is
# off by default, so nothing else announces a stuck or dead forwarding queue.
# No credential, no write to the queue, no call to the hub except one optional
# unauthenticated GET of /v1/health. Local reads only: the file queue, the
# forwarder's forward-status.json, systemd user unit state.
#
# Usage:
#   memoire-capture-health.sh            evaluate, alert on a state change or reminder
#   memoire-capture-health.sh --status   print the status JSON, alert nothing, write nothing
#
# Exit code: 0 ok, 1 warn, 2 critical. An error of the check itself (unusable
# MEMOIRE_HOME, missing jq, bad argument) logs one line and exits 0: the
# conditions drive the exit code, never the check's own failures.
#
# Thresholds (the injector's names and defaults where they exist; the process
# environment or $MEMOIRE_HOME/capture-tick.env, the file wins when it carries
# the key, last assignment wins):
#   MEMOIRE_INJECT_BACKLOG_MIN        25     pending depth            -> warn
#   MEMOIRE_INJECT_BACKLOG_AGE_S      600    oldest pending age (s)   -> warn
#   MEMOIRE_INJECT_TRANSPORT_DEAD_S   21600  oldest pending age (s)   -> critical
#   MEMOIRE_HEALTH_FAIL_N             10     forwarder consecutive failures -> warn
#   MEMOIRE_HEALTH_STALE_S            1800   age of last forward success, queue non-empty -> warn
#   MEMOIRE_HEALTH_GRACE_S            120    oldest pending age below which the stale test is silent
#   MEMOIRE_HEALTH_CLOCK              1      0 skips the hub clock-skew read
#   MEMOIRE_HEALTH_SYSTEMCTL_TIMEOUT_S 5     bound on each systemctl call; the first timeout stops all further calls
#   MEMOIRE_BASE_URL                  (none) hub URL for the clock-skew read; unset skips it
# Alerting:
#   MEMOIRE_HEALTH_REMIND_S           21600  reminder interval while a condition persists
#   MEMOIRE_HEALTH_SINK               (none) executable run with the status JSON on stdin
#   MEMOIRE_HEALTH_SINK_TIMEOUT_S     10     bound on the sink
# State file: $MEMOIRE_HOME/health-state.json; when that cannot be written, the first
# writable of $XDG_RUNTIME_DIR/memoire-health-state.json and
# ${TMPDIR:-/tmp}/memoire-health-state-<uid>.json, with a home_unwritable warn.
# Test hooks: MEMOIRE_HEALTH_NOW (epoch), MEMOIRE_HEALTH_NO_NOTIFY=1.

NAME=memoire-capture-health
T0_MS=""
[[ -n "${EPOCHREALTIME:-}" ]] && T0_MS="${EPOCHREALTIME//[.,]/}"

# One log line per run, stderr (the journal under the timer), `name: key=value`.
LOGKV=""
kv() { LOGKV+=" $1=$2"; }
emit_log() {
  local ms=""
  if [[ -n "$T0_MS" && -n "${EPOCHREALTIME:-}" ]]; then
    local t1="${EPOCHREALTIME//[.,]/}"; ms=" ms=$(( (t1 - T0_MS) / 1000 ))"
  fi
  printf '%s:%s%s\n' "$NAME" "$LOGKV" "$ms" >&2
}
own_error() { kv error "$1"; emit_log; exit 0; }

MODE=alert
case "${1:-}" in
  "") ;;
  --status) MODE=status ;;
  -h|--help) sed -n '8,12p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 0 ;;
  *) kv error "bad-argument"; emit_log; exit 0 ;;
esac

command -v jq >/dev/null 2>&1 || own_error "jq-missing"

[[ -n "${MEMOIRE_HOME:-}" || -n "${HOME:-}" ]] || own_error "memoire-home-unset"
MEMOIRE_HOME="${MEMOIRE_HOME:-$HOME/.memoire}"
[[ -d "$MEMOIRE_HOME" && -r "$MEMOIRE_HOME" && -x "$MEMOIRE_HOME" ]] || own_error "memoire-home-unreadable"
ENV_FILE="$MEMOIRE_HOME/capture-tick.env"
STATE_FILE="$MEMOIRE_HOME/health-state.json"
FWD_STATUS="$MEMOIRE_HOME/forward-status.json"

# cfg KEY: the env file wins when it carries the key (last assignment, same
# extraction as tr_key_value in memoire-trigger-lib.sh); otherwise the process
# environment.
cfg() {
  local key="$1" line val
  if [[ -r "$ENV_FILE" ]]; then
    line="$(grep -E "^[[:space:]]*(export[[:space:]]+)?${key}[[:space:]]*=" "$ENV_FILE" 2>/dev/null | tail -n 1)" || line=""
    if [[ -n "$line" ]]; then
      val="${line#*=}"; val="${val%%#*}"
      printf '%s' "$val" | tr -d '\r' | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' -e "s/^[\"']//" -e "s/[\"']\$//"
      return 0
    fi
  fi
  printf '%s' "${!key:-}"
}
INVALID=""
# cfg_int VAR KEY DEFAULT: set VAR to a bounded positive base-10 integer, else
# the default (an invalid value is named in the log line).
cfg_int() {
  local v; v="$(cfg "$2")"
  if [[ "$v" =~ ^[1-9][0-9]{0,8}$ ]]; then printf -v "$1" '%s' "$v"; return; fi
  [[ -z "$v" ]] || INVALID+="${INVALID:+,}$2"
  printf -v "$1" '%s' "$3"
}

cfg_int BACKLOG_MIN MEMOIRE_INJECT_BACKLOG_MIN 25
cfg_int BACKLOG_AGE_S MEMOIRE_INJECT_BACKLOG_AGE_S 600
cfg_int DEAD_S MEMOIRE_INJECT_TRANSPORT_DEAD_S 21600
cfg_int FAIL_N MEMOIRE_HEALTH_FAIL_N 10
cfg_int STALE_S MEMOIRE_HEALTH_STALE_S 1800
cfg_int GRACE_S MEMOIRE_HEALTH_GRACE_S 120
cfg_int REMIND_S MEMOIRE_HEALTH_REMIND_S 21600
cfg_int SINK_TIMEOUT_S MEMOIRE_HEALTH_SINK_TIMEOUT_S 10
cfg_int SCTL_T MEMOIRE_HEALTH_SYSTEMCTL_TIMEOUT_S 5
CLOCK_ON="$(cfg MEMOIRE_HEALTH_CLOCK)"
[[ "$CLOCK_ON" == 0 || "$CLOCK_ON" == off || "$CLOCK_ON" == false ]] && CLOCK_ON=0 || CLOCK_ON=1
BASE_URL="$(cfg MEMOIRE_BASE_URL)"
SINK="$(cfg MEMOIRE_HEALTH_SINK)"
[[ -n "$INVALID" ]] && kv invalid_knobs "$INVALID"

if [[ "${MEMOIRE_HEALTH_NOW:-}" =~ ^[0-9]{1,12}$ ]]; then NOW="$MEMOIRE_HEALTH_NOW"; else printf -v NOW '%(%s)T' -1; fi
HOST="${HOSTNAME:-$(hostname 2>/dev/null || echo unknown)}"

# systemctl, always bounded. After the first timeout no further call is made
# (a hung user manager hangs every call), so the run stays within about one
# timeout of systemctl time.
SCTL_OUT=""; SCTL_RC=0; SCTL_DEAD=0; SCTL_TIMEOUTS=0
sctl() {
  SCTL_OUT=""; SCTL_RC=0
  if (( SCTL_DEAD )); then SCTL_RC=124; return 0; fi
  SCTL_OUT="$(timeout -k 1 "$SCTL_T" systemctl --user "$@" 2>&1 </dev/null)"; SCTL_RC=$?
  if (( SCTL_RC == 124 || SCTL_RC == 137 )); then SCTL_DEAD=1; SCTL_TIMEOUTS=$(( SCTL_TIMEOUTS + 1 )); SCTL_RC=124; fi
  return 0
}
declare -A U_ACTIVE
# sc_active UNIT: U_ACTIVE[UNIT], cached; "unknown" when systemctl is absent, timed out or unclear.
sc_active() {
  [[ -n "${U_ACTIVE[$1]:-}" ]] && return 0
  local o=unknown
  if command -v systemctl >/dev/null 2>&1; then
    sctl is-active "$1"
    if (( SCTL_RC != 124 )); then
      o="${SCTL_OUT%%$'\n'*}"
      [[ "$o" =~ ^(active|inactive|failed|activating|deactivating|reloading|maintenance|refreshing)$ ]] || o=unknown
    fi
  fi
  U_ACTIVE[$1]="$o"
}

# ---- previous state (alert mode and --status read it; only alert mode writes) ----
# The state lives in the first writable of STATE_FILE and two fallbacks; the
# previous state is the newest valid file among them (updated_epoch).
# Fields are joined with US (0x1f), not a tab: an empty field must survive the split.
STATE_CANDS=("$STATE_FILE")
[[ -n "${XDG_RUNTIME_DIR:-}" ]] && STATE_CANDS+=("${XDG_RUNTIME_DIR%/}/memoire-health-state.json")
STATE_CANDS+=("${TMPDIR:-/tmp}/memoire-health-state-${UID}.json")
PREV=none; PREV_FP=""; PREV_ALERT=0; PSKEW=""; PREV_UPD=-1
for sf in "${STATE_CANDS[@]}"; do
  [[ -r "$sf" ]] || continue
  # A fallback lives in a shared directory: only a regular file of our own,
  # never a symlink (a planted file must not become the previous state).
  if [[ "$sf" != "$STATE_FILE" ]]; then
    [[ -L "$sf" || ! -f "$sf" || ! -O "$sf" ]] && continue
  fi
  p=""; pfp=""; pa=""; pskew=""; pu=""
  IFS=$'\x1f' read -r p pfp pa pskew pu < <(jq -r '[(.state // ""), (.firing // ""), ((.last_alert_epoch // 0) | tostring), ((.clock_skew_seen // "") | tostring), ((.updated_epoch // 0) | tostring)] | join("\u001f")' "$sf" 2>/dev/null) || p=""
  # A fallback dated more than 300 s ahead of now (a stepped clock, a test run)
  # is ignored: it would otherwise win over the home for ever. The home file is
  # ours alone and is read whatever its date.
  if [[ "$sf" != "$STATE_FILE" && "$pu" =~ ^[0-9]+$ ]] && (( pu > NOW + 300 )); then continue; fi
  if [[ "$p" == ok || "$p" == warn || "$p" == critical ]] && [[ "$pa" =~ ^[0-9]+$ && "$pu" =~ ^[0-9]+$ ]] && (( pu > PREV_UPD )); then
    PREV="$p"; PREV_FP="$pfp"; PREV_ALERT="$pa"; PREV_UPD="$pu"; PSKEW=""
    [[ "$pskew" =~ ^[0-9]+$ ]] && PSKEW="$pskew"
  fi
done

# probe_state_file PATH: 0 when a small file can be created and removed there;
# sets PROBE_ERR to the failure text otherwise.
PROBE_ERR=""
probe_state_file() {
  local t="$1.probe.$$" e
  if e="$( { umask 077; printf '%0256d' 0 >| "$t"; } 2>&1 )"; then
    command rm -f "$t" 2>/dev/null; return 0
  fi
  command rm -f "$t" 2>/dev/null
  PROBE_ERR="${e##*: }"; PROBE_ERR="${PROBE_ERR//[$'\t\n\r']/ }"; PROBE_ERR="${PROBE_ERR:-write failed}"
  return 1
}
WRITE_TO=""; HOME_UNWRITABLE=0; HOME_ERR=""
if [[ "$MODE" == alert ]]; then
  for sf in "${STATE_CANDS[@]}"; do
    [[ "$sf" == "$STATE_FILE" ]] || { [[ -d "${sf%/*}" ]] || continue; }
    if probe_state_file "$sf"; then WRITE_TO="$sf"; break; fi
    [[ "$sf" == "$STATE_FILE" ]] && { HOME_UNWRITABLE=1; HOME_ERR="$PROBE_ERR"; }
  done
fi

# ---- conditions -----------------------------------------------------------
# One TSV row per condition: name, firing(0|1), severity, value, threshold, detail.
CONDS=""
STATE=ok
FIRING=""
SKIPPED=""
cond() { # name firing severity value threshold detail
  CONDS+="$1"$'\t'"$2"$'\t'"$3"$'\t'"$4"$'\t'"$5"$'\t'"$6"$'\n'
  if [[ "$2" == 1 ]]; then
    FIRING+="${FIRING:+,}$1"
    if [[ "$3" == critical ]]; then STATE=critical; elif [[ "$STATE" == ok ]]; then STATE=warn; fi
  fi
}
skip() { SKIPPED+="${SKIPPED:+,}$1"; }

# Queue: pending plus inflight (the forwarder renames an item pending -> inflight
# for the POST and Reconcile returns crashed inflight items to pending, so an
# inflight item is an undrained lease; rename keeps its mtime, so its age counts).
# One find per directory; poison sidecars live in failed/ and are excluded there.
PENDING=0; INFLIGHT=0; OLDEST_EPOCH=0; OLDEST_AGE=0; FAILED=0
qscan() { # DIR -> Q_N Q_OLD
  Q_N=0; Q_OLD=0
  [[ -d "$1" ]] || return 0
  read -r Q_N Q_OLD < <(find "$1" -maxdepth 1 -name '*.json' -type f -printf '%T@\n' 2>/dev/null \
    | awk 'BEGIN{m=""} {c++; if(m==""||$1<m)m=$1} END{printf "%d %d\n", c, m}')
  [[ "$Q_N" =~ ^[0-9]+$ ]] || Q_N=0
  [[ "$Q_OLD" =~ ^[0-9]+$ ]] || Q_OLD=0
}
qscan "$MEMOIRE_HOME/queue/pending"; PENDING_ONLY=$Q_N; OLDEST_EPOCH=$Q_OLD
qscan "$MEMOIRE_HOME/queue/inflight"; INFLIGHT=$Q_N
(( Q_N > 0 && Q_OLD > 0 && (OLDEST_EPOCH == 0 || Q_OLD < OLDEST_EPOCH) )) && OLDEST_EPOCH=$Q_OLD
PENDING=$(( PENDING_ONLY + INFLIGHT ))
if (( PENDING > 0 && OLDEST_EPOCH > 0 )); then
  OLDEST_AGE=$(( NOW - OLDEST_EPOCH )); (( OLDEST_AGE < 0 )) && OLDEST_AGE=0
fi
if [[ -d "$MEMOIRE_HOME/queue/failed" ]]; then
  FAILED="$(find "$MEMOIRE_HOME/queue/failed" -maxdepth 1 -name '*.json' -type f ! -name '*.error.json' 2>/dev/null | wc -l)"
  [[ "$FAILED" =~ ^[0-9]+$ ]] || FAILED=0
fi

# Forwarder state (a hub or a client without the forwarder has no file).
FWD_PRESENT=false; FWD_FAILS=0; FWD_LAST=0
if [[ -r "$FWD_STATUS" ]]; then
  if IFS=$'\t' read -r fails last < <(jq -r '[(.consecutive_failures // 0), (.last_success_epoch // 0)] | map(tostring) | @tsv' "$FWD_STATUS" 2>/dev/null) \
     && [[ "$fails" =~ ^[0-9]+$ && "$last" =~ ^[0-9]+$ ]]; then
    FWD_PRESENT=true; FWD_FAILS="$fails"; FWD_LAST="$last"
  else
    skip forward_status:unparseable
  fi
else
  skip forward_status:absent
fi

f=0; (( PENDING >= BACKLOG_MIN )) && f=1
cond backlog_depth "$f" warn "$PENDING" "$BACKLOG_MIN" "events pending or inflight local->hub"
f=0; (( PENDING > 0 && OLDEST_AGE >= BACKLOG_AGE_S )) && f=1
cond backlog_age "$f" warn "$OLDEST_AGE" "$BACKLOG_AGE_S" "age in seconds of the oldest pending or inflight event"
f=0; (( FAILED >= 1 )) && f=1
cond failed_items "$f" warn "$FAILED" 1 "poisoned events in queue/failed"
# Dead only when nothing is draining: a forwarder that succeeded within the
# threshold and is still running (a laptop after a resume, retrying) is the
# ordinary backlog warn. No forwarder status, no success yet, an old success or
# a forwarder that is not running keep it dead.
f=0
if (( PENDING > 0 && OLDEST_AGE >= DEAD_S )); then
  f=1
  if [[ "$FWD_PRESENT" == true ]] && (( FWD_LAST > 0 && NOW - FWD_LAST < DEAD_S )); then
    sc_active memoire-forward.service
    case "${U_ACTIVE[memoire-forward.service]}" in inactive|failed|deactivating) ;; *) f=0 ;; esac
  fi
fi
cond transport_dead "$f" critical "$OLDEST_AGE" "$DEAD_S" "the oldest event is older than any healthy gap and the forwarder has not succeeded within it or is not running"

if [[ "$FWD_PRESENT" == true ]]; then
  f=0; (( FWD_FAILS >= FAIL_N )) && f=1
  cond forward_failures "$f" warn "$FWD_FAILS" "$FAIL_N" "forwarder consecutive failures"
  # The forwarder writes its status only after a cycle that did work or failed,
  # so an idle gap makes the last success old: the test needs a queue that has
  # had time (GRACE_S) to drain.
  f=0; v=0
  if (( PENDING > 0 && OLDEST_AGE >= GRACE_S )); then
    if (( FWD_LAST == 0 )); then v=never; f=1; else v=$(( NOW - FWD_LAST )); (( v >= STALE_S )) && f=1; fi
  fi
  cond forward_stale "$f" warn "$v" "$STALE_S" "age in seconds of the last forwarder success while the queue is not empty"
fi

# Units. Only installed units count; an absent unit is "not installed". A
# systemctl call that timed out leaves its unit "unknown": logged, never a condition.
UNITS_JSON='{}'
unreachable=0
UNK_DETAIL="the systemd user manager did not answer; carried from the previous run"
was_firing() { [[ ",$PREV_FP," == *",$1,"* ]]; }
if command -v systemctl >/dev/null 2>&1; then
  declare -A U_ENABLED U_EXPECT
  ALL_UNITS=(memoire-forward.service memoire-capture-tick.timer memoire-capture-codex-tick.timer memoire-capture-opencode-tick.timer)
  for u in "${ALL_UNITS[@]}" memoire.service; do
    sctl is-enabled "$u"; out="${SCTL_OUT%%$'\n'*}"
    if (( SCTL_RC == 124 )); then U_ENABLED[$u]=unknown; U_EXPECT[$u]=0; continue; fi
    case "$out" in
      *"Failed to connect"*|*"No medium found"*|*"DBUS_SESSION_BUS_ADDRESS"*) unreachable=1; break ;;
      enabled|enabled-runtime|linked|linked-runtime|static|indirect|generated|transient|alias) U_ENABLED[$u]="$out"; U_EXPECT[$u]=1 ;;
      disabled|masked|masked-runtime) U_ENABLED[$u]="$out"; U_EXPECT[$u]=0 ;;
      *) U_ENABLED[$u]="not-installed"; U_EXPECT[$u]=0 ;;
    esac
  done
  if (( unreachable )); then
    skip units:systemd-user-unreachable
  else
    for u in "${ALL_UNITS[@]}" memoire.service; do
      [[ "${U_EXPECT[$u]}" == 1 ]] && sc_active "$u"
    done
    TIMERS_OUT=""; TIMERS_OK=0
    if (( ! SCTL_DEAD )); then sctl list-timers --all --no-pager --no-legend; if (( SCTL_RC == 0 )); then TIMERS_OUT="$SCTL_OUT"; TIMERS_OK=1; elif (( SCTL_RC != 124 )); then skip units:list-timers-failed; fi; fi
    FAILED_SVCS=""; TICK_SEEN=0; SHOW_UNKNOWN=0
    for u in "${ALL_UNITS[@]}"; do
      if [[ "${U_EXPECT[$u]}" == 1 ]]; then
        f=0; v="${U_ACTIVE[$u]}"
        d="an enabled unit must be active; a timer must have a next elapse"
        if [[ "$v" == unknown ]]; then
          # No answer: a unit that was firing stays firing, with value unknown.
          was_firing "unit:$u" && { f=1; d="$UNK_DETAIL"; }
        else
          [[ "$v" == active ]] || f=1
          if [[ "$u" == *.timer && $f == 0 && $TIMERS_OK == 1 ]]; then
            row="$(printf '%s\n' "$TIMERS_OUT" | grep -F " $u" | head -n 1)" || row=""
            first="${row%% *}"
            if [[ -z "$row" || "$first" == "-" || "$first" == "n/a" ]]; then
              # While the triggered service runs, systemd shows the timer with no
              # next elapse (it is recomputed when the service ends). A tick that
              # is mid-run is healthy, not stuck: the check firing in the same
              # second as the tick raised a warn and a recovery fifteen minutes
              # apart, twice an evening, on a sound machine.
              svc="${u%.timer}.service"; sc_active "$svc"
              case "${U_ACTIVE[$svc]}" in
                active|activating|deactivating|reloading) v="active:service-running" ;;
                unknown)
                  # No answer about the service: a timeout is never a new alert.
                  v=unknown; was_firing "unit:$u" && { f=1; d="$UNK_DETAIL"; } ;;
                *)
                  # The service may have ended between the list-timers snapshot
                  # and the service read: one fresh look before raising.
                  sctl list-timers --all --no-pager --no-legend
                  row=""; (( SCTL_RC == 0 )) && { row="$(printf '%s\n' "$SCTL_OUT" | grep -F " $u" | head -n 1)" || row=""; }
                  first="${row%% *}"
                  if [[ -n "$row" && "$first" != "-" && "$first" != "n/a" ]]; then v=active; else v="no-next-elapse"; f=1; fi ;;
              esac
            fi
          elif [[ "$u" == *.timer && $f == 0 && $TIMERS_OK == 0 ]] && was_firing "unit:$u"; then
            v=unknown; f=1; d="$UNK_DETAIL"
          fi
        fi
        cond "unit:$u" "$f" warn "$v" active "$d"
        if [[ "$u" == *-tick.timer ]]; then
          TICK_SEEN=1; svc="${u%.timer}.service"
          sctl show -p Result,ExecMainStatus "$svc"
          if (( SCTL_RC == 124 )); then SHOW_UNKNOWN=1; else
            res="$(grep -m1 '^Result=' <<<"$SCTL_OUT")"; res="${res#Result=}"
            [[ -n "$res" && "$res" != success ]] && FAILED_SVCS+="${FAILED_SVCS:+,}$svc"
          fi
        fi
      elif [[ "$u" == memoire-forward.service && "${U_ENABLED[$u]}" == not-installed ]]; then
        # Not installed is fine unless the queue says something should drain it
        # and no local drainer (memoire.service, hub topology) is running.
        f=0
        if (( PENDING > 0 && OLDEST_AGE >= BACKLOG_AGE_S )) && [[ "${U_ACTIVE[memoire.service]:-}" != active ]]; then f=1; fi
        cond "unit:$u" "$f" warn not-installed active "queue is ageing and no drainer is installed or active"
      fi
    done
    # A tick timer can be active with a next elapse while its service fails on
    # every run: capture is dead with nothing queued.
    if (( TICK_SEEN )); then
      f=0; nf=0
      if [[ -n "$FAILED_SVCS" ]]; then f=1; nf="$(tr ',' '\n' <<<"$FAILED_SVCS" | wc -l)"; fi
      d="capture tick services whose last run failed: ${FAILED_SVCS:-none}"
      if (( f == 0 && SHOW_UNKNOWN )) && was_firing unit_failed; then f=1; nf=unknown; d="$UNK_DETAIL"; fi
      cond unit_failed "$f" warn "$nf" 1 "$d"
    fi
    UNITS_JSON="$(for u in "${ALL_UNITS[@]}"; do
      s="${U_ENABLED[$u]}"; [[ "${U_EXPECT[$u]}" == 1 ]] && s="${U_ACTIVE[$u]}"
      printf '%s\t%s\n' "$u" "$s"
    done | jq -Rn '[inputs | split("\t") | {(.[0]): .[1]}] | add')" || UNITS_JSON='{}'
  fi
  (( SCTL_TIMEOUTS > 0 )) && { skip units:systemctl-timeout; kv systemctl_timeouts "$SCTL_TIMEOUTS"; }
  # The manager did not answer (timeout or unreachable): a unit condition that
  # was firing and was not evaluated this run stays firing, value unknown. A
  # check problem never reads as a recovery.
  if (( SCTL_TIMEOUTS > 0 || unreachable )); then
    IFS=, read -ra _uf <<<"$PREV_FP"
    for n in ${_uf[@]+"${_uf[@]}"}; do
      [[ "$n" == unit:* || "$n" == unit_failed ]] || continue
      [[ ",$FIRING," == *",$n,"* ]] && continue
      grep -qxF -- "$n" < <(cut -f1 <<<"$CONDS") && continue
      if [[ "$n" == unit_failed ]]; then cond "$n" 1 warn unknown 1 "$UNK_DETAIL"; else cond "$n" 1 warn unknown active "$UNK_DETAIL"; fi
    done
  fi
else
  skip units:no-systemctl
fi

# Hub clock skew: one short unauthenticated GET, fail-open.
SKEW_SEEN="$PSKEW"
if [[ "$CLOCK_ON" == 0 ]]; then
  skip clock_skew:disabled
elif [[ -z "$BASE_URL" ]]; then
  skip clock_skew:no-base-url
elif ! command -v curl >/dev/null 2>&1; then
  skip clock_skew:no-curl
else
  skew="$(timeout -k 1 5 curl -sf -m 3 "${BASE_URL%/}/v1/health" 2>/dev/null | jq -r '.clock_skew_future_events' 2>/dev/null)" || skew=""
  if [[ "$skew" =~ ^[0-9]+$ ]]; then
    # The hub's count is a lifetime count: alert on an increase since the
    # previous run only; the first run seeds the value.
    SKEW_SEEN="$skew"; d=0
    if [[ "$PSKEW" =~ ^[0-9]+$ ]] && (( skew > PSKEW )); then d=$(( skew - PSKEW )); fi
    f=0; (( d >= 1 )) && f=1
    cond clock_skew "$f" warn "$d" 1 "new hub events whose occurred_at is ahead of received_at since the previous run (lifetime count $skew; a producer clock is wrong)"
  else
    skip clock_skew:unreachable
  fi
fi

# The state file under MEMOIRE_HOME cannot be written (alert mode probes it):
# a warn of its own, so a lasting warn is never silent on a full or read-only home.
if (( HOME_UNWRITABLE )); then
  cond home_unwritable 1 warn "$STATE_FILE" writable "$HOME_ERR"
  if [[ -n "$WRITE_TO" ]]; then kv state_file fallback; else kv state_file none; fi
fi

# fp_norm LIST: the comma list sorted, so a carried condition's position never reads as a change.
fp_norm() { tr ',' '\n' <<<"$1" | sort | tr '\n' ','; }

# ---- cleared / added since the previous run -------------------------------
CLEARED=""; ADDED=""
IFS=, read -ra _pf <<<"$PREV_FP"
for n in ${_pf[@]+"${_pf[@]}"}; do [[ ",$FIRING," == *",$n,"* ]] || CLEARED+="${CLEARED:+,}$n"; done
IFS=, read -ra _cf <<<"$FIRING"
for n in ${_cf[@]+"${_cf[@]}"}; do [[ ",$PREV_FP," == *",$n,"* ]] || ADDED+="${ADDED:+,}$n"; done

# ---- status JSON ----------------------------------------------------------
printf -v ISO '%(%Y-%m-%dT%H:%M:%SZ)T' "$NOW"
[[ "${TZ:-}" == UTC ]] || ISO="$(TZ=UTC printf '%(%Y-%m-%dT%H:%M:%SZ)T' "$NOW")"
STATUS="$(jq -nc --arg state "$STATE" --arg host "$HOST" --arg time "$ISO" --argjson epoch "$NOW" \
  --arg home "$MEMOIRE_HOME" --arg conds "$CONDS" --arg firing "$FIRING" --arg cleared "$CLEARED" --arg skipped "$SKIPPED" \
  --argjson remind "$REMIND_S" \
  --argjson pending "$PENDING_ONLY" --argjson inflight "$INFLIGHT" --argjson ptotal "$PENDING" --argjson age "$OLDEST_AGE" --argjson failed "$FAILED" \
  --argjson fpresent "$FWD_PRESENT" --argjson ffails "$FWD_FAILS" --argjson flast "$FWD_LAST" \
  --argjson units "$UNITS_JSON" '
  {state: $state, host: $host, time: $time, time_epoch: $epoch, memoire_home: $home,
   firing: ($firing | split(",") | map(select(length > 0))),
   cleared: ($cleared | split(",") | map(select(length > 0))),
   remind_s: $remind,
   conditions: ($conds | split("\n") | map(select(length > 0) | split("\t") |
     {name: .[0], firing: (.[1] == "1"), severity: .[2],
      value: (.[3] | tonumber? // .), threshold: (.[4] | tonumber? // .), detail: .[5]})),
   queue: {pending: $pending, inflight: $inflight, pending_total: $ptotal, oldest_pending_age_s: $age, failed: $failed},
   forwarder: {present: $fpresent, consecutive_failures: $ffails, last_success_epoch: $flast},
   units: $units,
   skipped: ($skipped | split(",") | map(select(length > 0)))}')" || own_error "status-build-failed"

kv state "$STATE"
kv firing "${FIRING:--}"
kv pending_total "$PENDING"; kv oldest_s "$OLDEST_AGE"; kv failed "$FAILED"

finish() { emit_log; case "$STATE" in critical) exit 2 ;; warn) exit 1 ;; *) exit 0 ;; esac; }

if [[ "$MODE" == status ]]; then
  printf '%s\n' "$STATUS"
  kv mode status
  finish
fi

# ---- state machine --------------------------------------------------------
EVENT=""
if [[ "$STATE" == ok ]]; then
  [[ "$PREV" == warn || "$PREV" == critical ]] && EVENT=recovered
elif [[ "$PREV" != "$STATE" || "$(fp_norm "$PREV_FP")" != "$(fp_norm "$FIRING")" ]]; then
  EVENT=change
elif (( NOW - PREV_ALERT >= REMIND_S )); then
  EVENT=reminder
fi

# State, tmp+mv, 0600, in WRITE_TO (the home, else a fallback, see probe above).
# last_alert_epoch moves on every event (the sink hears it too), so the sink's
# silence is bounded by REMIND_S. Written before the alerts: when no location
# is writable the previous state is unknown, so only a critical state notifies
# (every run, no reminder, no recovery notice) and a failed write never repeats
# a warn on every run.
LAST_ALERT="$PREV_ALERT"; [[ -n "$EVENT" ]] && LAST_ALERT="$NOW"
STATE_UNKNOWN=0
if [[ -n "$WRITE_TO" ]] && tmp="$WRITE_TO.tmp.$$" && ( umask 077; jq -nc --arg state "$STATE" --arg firing "$FIRING" --argjson since "$NOW" --argjson la "$LAST_ALERT" --argjson skew "${SKEW_SEEN:-null}" \
     '{state: $state, firing: $firing, updated_epoch: $since, last_alert_epoch: $la, clock_skew_seen: $skew}' >| "$tmp" ) 2>/dev/null \
   && chmod 600 "$tmp" 2>/dev/null && command mv -f "$tmp" "$WRITE_TO" 2>/dev/null; then
  # The home holds the state again: a fallback left behind must not outlive it
  # (it would give one spurious recovery after a wiped home or a reinstall).
  if [[ "$WRITE_TO" == "$STATE_FILE" ]]; then
    for sf in "${STATE_CANDS[@]:1}"; do [[ -f "$sf" && ! -L "$sf" && -O "$sf" ]] && command rm -f "$sf" 2>/dev/null; done
  fi
else
  [[ -n "${tmp:-}" ]] && command rm -f "$tmp" 2>/dev/null
  kv state_write failed
  STATE_UNKNOWN=1; PREV=none; PREV_FP=""; CLEARED=""; ADDED=""
  if [[ "$STATE" == critical ]]; then EVENT=change; else EVENT=""; fi
fi
kv prev "$PREV"
kv event "${EVENT:-none}"

NOTIFY_RESULT=none; SINK_RESULT=none
if [[ -n "$EVENT" ]]; then
  # Desktop. A change that only removes conditions at the same state is good
  # news with nothing to act on, and a toast titled like a new alert would
  # mislead: it goes to the sink (cleared filled) and the log, not the desktop.
  if [[ "$EVENT" == change && "$PREV" == "$STATE" && -z "$ADDED" ]]; then
    NOTIFY_RESULT=suppressed-clearing
  elif [[ "${MEMOIRE_HEALTH_NO_NOTIFY:-}" == 1 ]]; then
    NOTIFY_RESULT=disabled
  elif ! command -v notify-send >/dev/null 2>&1; then
    NOTIFY_RESULT=no-notify-send
  else
    bus="${DBUS_SESSION_BUS_ADDRESS:-}"
    if [[ -z "$bus" && -n "${XDG_RUNTIME_DIR:-}" && -S "$XDG_RUNTIME_DIR/bus" ]]; then bus="unix:path=$XDG_RUNTIME_DIR/bus"; fi
    if [[ -z "$bus" ]]; then
      NOTIFY_RESULT=no-bus
    else
      urgency=normal; title=""; body=""
      case "$EVENT" in
        recovered) title="Memoire capture healthy again on $HOST"; body="Was: $PREV_FP" ;;
        *) [[ "$STATE" == critical ]] && urgency=critical
           title="Memoire capture ${STATE^^} on $HOST"
           [[ "$EVENT" == reminder ]] && title="$title (reminder)"
           [[ "$PREV" == critical && "$STATE" == warn ]] && title="$title (was CRITICAL)"
           body="$(jq -r '.conditions[] | select(.firing) | "\(.name) = \(.value) (threshold \(.threshold)): \(.detail)"' <<<"$STATUS" 2>/dev/null)"
           [[ -n "$CLEARED" ]] && body+=$'\n'"Cleared: $CLEARED" ;;
      esac
      if DBUS_SESSION_BUS_ADDRESS="$bus" timeout 5 notify-send -u "$urgency" -a memoire-capture-health -- "$title" "$body" >/dev/null 2>&1; then
        NOTIFY_RESULT=sent
      else
        NOTIFY_RESULT=failed
      fi
    fi
  fi

  # Sink: the seam for an Alertmanager poster living outside this repo.
  if [[ -n "$SINK" ]]; then
    if [[ ! -x "$SINK" || -d "$SINK" ]]; then
      SINK_RESULT=not-executable
    else
      PAYLOAD="$(jq -c --arg event "$EVENT" --arg prev "$PREV" --argjson unknown "$STATE_UNKNOWN" \
        '. + {event: $event, previous_state: $prev} | if $unknown == 1 then .cleared = [] else . end' <<<"$STATUS" 2>/dev/null)" || PAYLOAD="$STATUS"
      # A subshell with its own stderr sink; the explicit exit keeps bash from
      # exec-ing timeout in place, so the "Killed" notice for a sink killed at
      # its timeout goes to /dev/null instead of adding a second journal line.
      ( MEMOIRE_HEALTH_EVENT="$EVENT" MEMOIRE_HEALTH_STATE="$STATE" \
          timeout -k 1 "$SINK_TIMEOUT_S" "$SINK" <<<"$PAYLOAD" >/dev/null 2>&1; exit $? ) 2>/dev/null
      src=$?
      case "$src" in 0) SINK_RESULT=ok ;; 124|137) SINK_RESULT=timeout ;; *) SINK_RESULT="failed:$src" ;; esac
    fi
  fi
fi
kv notify "$NOTIFY_RESULT"; kv sink "$SINK_RESULT"
finish
