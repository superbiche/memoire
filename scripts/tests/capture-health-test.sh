#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# capture-health-test.sh — tests of the capture health check
# (scripts/memoire-capture-health.sh, MISSING #19) and its timer installer
# (scripts/capture-health-timer.sh). test-ops lane (`make test`). Offline:
# systemctl, notify-send and curl are stubs on a toolbox PATH that holds none
# of the real ones; every run starts from `env -i`, so no MEMOIRE_* variable
# of the calling shell (a hub URL, a token) reaches the check.
#
#   conditions  every condition off just below its threshold, on at it
#   forwarder   consecutive failures, stale last success, grace, never, absent file
#   units       active / inactive / disabled / not installed / no next elapse / no user bus
#   clock       hub skew: zero, positive, unreachable, no base URL, switched off
#   state       first alert, no repeat, reminder, recovery notice, escalation, silence
#   desktop     notify-send arguments and urgency; absent binary; absent bus
#   sink        stdin JSON, event, failure, timeout, not executable, no event
#   knobs       env file beats environment, last assignment wins, invalid values fall back
#   status      --status shape, no side effect, exit codes
#   own         unreadable or missing MEMOIRE_HOME, missing jq, bad argument: exit 0 + one line
#   review fixes (round 2): previous_state, systemctl timeout, cleared, clock increase,
#               clearing without a toast, transport dead vs a live forwarder, inflight,
#               failed tick service, unwritable state, sink kill noise, no HOME
#   round 3     systemctl timeout keeps unit conditions firing (unknown), state-file fallback with
#               home_unwritable, curl timeout, list-timers marker, pending_total
#   timer       the unit generator (systemctl is a stub; nothing is installed)

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHECK="${MEMOIRE_HEALTH_CHECK_UNDER_TEST:-$ROOT/scripts/memoire-capture-health.sh}"
TIMER_SH="$ROOT/scripts/capture-health-timer.sh"
command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }

WORK="$(mktemp -d "${TMPDIR:-/var/tmp}/capture-health-test.XXXXXX")" || { echo "FAIL: mktemp -d"; exit 1; }
cleanup() { chmod -R u+rwx "$WORK" 2>/dev/null; rm -rf "$WORK"; }
trap cleanup EXIT

FAILURES=0
fail() { echo "FAIL: $1" >&2; FAILURES=$((FAILURES + 1)); }
check() { echo "ok: $1"; }

NOW=1800000000
# Toolbox: the tools the check needs, none of systemctl / notify-send / curl.
TOOLS="$WORK/tools"; mkdir -p "$TOOLS"
for t in bash sh env jq find awk grep sed tr head tail wc date cat mv rm chmod mkdir sleep timeout sort cut dirname hostname touch ls printf stat tee; do
  p="$(command -v "$t" 2>/dev/null)" && [[ -x "$p" ]] && ln -sf "$p" "$TOOLS/$t"
done

# mkenv NAME: scratch home, stubs. Stub state lives under $d/sc (systemctl),
# $d/curl (curl reply), $d/notify.log (notify-send calls).
mkenv() {
  local d="$WORK/$1"
  mkdir -p "$d/home" "$d/mh/queue/pending" "$d/mh/queue/failed" "$d/mh/queue/inflight" "$d/sc/result" "$d/bin" "$d/sc/enabled" "$d/sc/active" "$d/xdg" "$d/tmpd"
  cat > "$d/bin/systemctl" <<'STUB'
#!/usr/bin/env bash
sc="$STUB_SC"; echo "$*" >> "$sc/calls"
[[ "$1" == --user ]] && shift
verb="$1"; shift
[[ -e "$sc/unreachable" ]] && { echo "Failed to connect to bus: No medium found" >&2; exit 1; }
[[ -e "$sc/hang" ]] && sleep 20
case "$verb" in
  is-enabled) if [[ -f "$sc/enabled/$1" ]]; then cat "$sc/enabled/$1"; else echo "Failed to get unit file state for $1: No such file or directory" >&2; exit 1; fi ;;
  is-active) if [[ -f "$sc/active/$1" ]]; then cat "$sc/active/$1"; else echo inactive; exit 3; fi ;;
  list-timers) [[ -f "$sc/timers" ]] && cat "$sc/timers"; exit 0 ;;
  show) if [[ -f "$sc/result/${@: -1}" ]]; then cat "$sc/result/${@: -1}"; else printf 'Result=success\nExecMainStatus=0\n'; fi ;;
  *) exit 0 ;;
esac
STUB
  cat > "$d/bin/notify-send" <<'STUB'
#!/usr/bin/env bash
{ printf 'BUS=%s\n' "${DBUS_SESSION_BUS_ADDRESS:-}"; printf 'ARG<%s>\n' "$@"; echo ---; } >> "$STUB_NOTIFY"
exit "${STUB_NOTIFY_RC:-0}"
STUB
  cat > "$d/bin/curl" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "$STUB_CURL.calls"
[[ -f "$STUB_CURL.hang" ]] && sleep 20
[[ -f "$STUB_CURL.rc" ]] && exit "$(cat "$STUB_CURL.rc")"
cat "$STUB_CURL"
STUB
  chmod +x "$d/bin/"*
  printf '%s' "$d"
}
# mkq DIR N AGE [DIR2]: N pending files whose mtime is AGE seconds before NOW
mkq() {
  local d="$1" n="$2" age="$3" sub="${4:-pending}" i f
  for ((i = 0; i < n; i++)); do
    f="$d/mh/queue/$sub/$((1000 + i))-claude-code-$i.json"
    : > "$f"; touch -d "@$((NOW - age))" "$f"
  done
}
# runq DIR [KEY=VALUE ...]: like run, with the pending queue aged 700 s at the run's own clock T
runq() {
  find "$1/mh/queue/pending" -type f -exec touch -d "@$(( ${T:-$NOW} - 700 ))" {} + 2>/dev/null
  run "$@"
}
# run DIR [KEY=VALUE ...] [-- ARGS...]: the check in DIR's scratch environment;
# sets RC, OUT, ERR (files $d/out and $d/err)
run() {
  local d="$1"; shift
  local envs=() args=()
  while (( $# )); do [[ "$1" == -- ]] && { shift; args=("$@"); break; }; envs+=("$1"); shift; done
  env -i PATH="$d/bin:$TOOLS" HOME="$d/home" MEMOIRE_HOME="$d/mh" MEMOIRE_HEALTH_NOW="${T:-$NOW}" HOSTNAME=testhost TMPDIR="$d/tmpd" \
    STUB_SC="$d/sc" STUB_NOTIFY="$d/notify.log" STUB_CURL="$d/curl" \
    ${envs[@]+"${envs[@]}"} bash "$CHECK" ${args[@]+"${args[@]}"} > "$d/out" 2> "$d/err"
  RC=$?
}
fire() { jq -r '.conditions[] | select(.firing) | .name' "$1/out" | paste -sd, -; }
cval() { jq -r --arg n "$2" '.conditions[] | select(.name == $n) | .value' "$1/out"; }
cfire() { jq -r --arg n "$2" '.conditions[] | select(.name == $n) | .firing' "$1/out"; }
nnotify() { [[ -f "$1/notify.log" ]] && grep -c '^---$' "$1/notify.log" || echo 0; }

# ---- conditions ---------------------------------------------------------------------
t_conditions() {
  local d; d="$(mkenv cond)"
  run "$d" -- --status
  [[ $RC == 0 && "$(jq -r .state "$d/out")" == ok && "$(fire "$d")" == "" ]] && check "conditions: an empty queue is ok, exit 0" || fail "conditions: empty queue rc=$RC $(cat "$d/out")"

  d="$(mkenv depth)"; mkq "$d" 24 5; run "$d" -- --status
  [[ "$(cfire "$d" backlog_depth)" == false && $RC == 0 ]] && check "conditions: depth 24 is below the default 25" || fail "conditions: depth 24 fires ($RC)"
  mkq "$d" 25 5; run "$d" -- --status
  [[ "$(cfire "$d" backlog_depth)" == true && "$(cval "$d" backlog_depth)" == 25 && $RC == 1 ]] && check "conditions: depth 25 fires warn (exit 1)" || fail "conditions: depth 25 ($RC) $(fire "$d")"

  d="$(mkenv age)"; mkq "$d" 1 599; run "$d" -- --status
  [[ "$(cfire "$d" backlog_age)" == false && $RC == 0 ]] && check "conditions: oldest 599 s is below 600" || fail "conditions: age 599 fires"
  mkq "$d" 1 600; run "$d" -- --status
  [[ "$(cfire "$d" backlog_age)" == true && "$(cval "$d" backlog_age)" == 600 && $RC == 1 ]] && check "conditions: oldest 600 s fires warn" || fail "conditions: age 600 ($RC)"

  d="$(mkenv failed)"; mkq "$d" 0 0; : > "$d/mh/queue/failed/1-x.error.json"; run "$d" -- --status
  [[ "$(cfire "$d" failed_items)" == false && "$(cval "$d" failed_items)" == 0 ]] && check "conditions: an .error.json sidecar is not a failed item" || fail "conditions: sidecar counted"
  : > "$d/mh/queue/failed/1-x.json"; run "$d" -- --status
  [[ "$(cfire "$d" failed_items)" == true && "$(cval "$d" failed_items)" == 1 && $RC == 1 ]] && check "conditions: one failed item fires warn" || fail "conditions: failed item ($RC)"

  d="$(mkenv dead)"; mkq "$d" 1 21599; run "$d" -- --status
  [[ "$(cfire "$d" transport_dead)" == false && $RC == 1 ]] && check "conditions: oldest 21599 s is warn, not yet dead" || fail "conditions: 21599 ($RC) $(fire "$d")"
  mkq "$d" 1 21600; run "$d" -- --status
  [[ "$(cfire "$d" transport_dead)" == true && "$(jq -r .state "$d/out")" == critical && $RC == 2 ]] && check "conditions: oldest 21600 s is transport dead, critical, exit 2" || fail "conditions: 21600 ($RC)"
}

# ---- forwarder ------------------------------------------------------------------------
fwd() { printf '{"consecutive_failures":%s,"forward_url":"https://h/v1","forwarded_total":3,"last_success_epoch":%s}\n' "$2" "$3" > "$1/mh/forward-status.json"; }
t_forwarder() {
  local d; d="$(mkenv fwd)"
  fwd "$d" 9 "$NOW"; run "$d" -- --status
  [[ "$(cfire "$d" forward_failures)" == false && $RC == 0 ]] && check "forwarder: 9 consecutive failures is below 10" || fail "forwarder: 9 fires"
  fwd "$d" 10 "$NOW"; run "$d" -- --status
  [[ "$(cfire "$d" forward_failures)" == true && $RC == 1 ]] && check "forwarder: 10 consecutive failures fires warn" || fail "forwarder: 10 ($RC)"

  d="$(mkenv stale)"; mkq "$d" 1 300
  fwd "$d" 0 $((NOW - 1799)); run "$d" -- --status
  [[ "$(cfire "$d" forward_stale)" == false ]] && check "forwarder: last success 1799 s ago with a pending queue is below 1800" || fail "forwarder: stale 1799"
  fwd "$d" 0 $((NOW - 1800)); run "$d" -- --status
  [[ "$(cfire "$d" forward_stale)" == true && "$(cval "$d" forward_stale)" == 1800 ]] && check "forwarder: last success 1800 s ago with a pending queue fires" || fail "forwarder: stale 1800"
  fwd "$d" 0 0; run "$d" -- --status
  [[ "$(cfire "$d" forward_stale)" == true && "$(cval "$d" forward_stale)" == never ]] && check "forwarder: a forwarder that never succeeded, queue not empty, fires with value never" || fail "forwarder: never"
  rm -f "$d"/mh/queue/pending/*; fwd "$d" 0 $((NOW - 90000)); run "$d" -- --status
  [[ "$(cfire "$d" forward_stale)" == false ]] && check "forwarder: an empty queue never makes the last success stale" || fail "forwarder: stale on empty queue"
  mkq "$d" 1 119; run "$d" -- --status
  [[ "$(cfire "$d" forward_stale)" == false ]] && check "forwarder: a queue younger than the grace (119 s) stays silent" || fail "forwarder: grace 119"
  mkq "$d" 1 120; run "$d" -- --status
  [[ "$(cfire "$d" forward_stale)" == true ]] && check "forwarder: a queue at the grace (120 s) is tested" || fail "forwarder: grace 120"

  d="$(mkenv nofwd)"; run "$d" -- --status
  [[ "$(jq -r '.forwarder.present' "$d/out")" == false && "$(jq -r '.skipped | index("forward_status:absent") != null' "$d/out")" == true && "$(cfire "$d" forward_failures)" == "" ]] \
    && check "forwarder: no forward-status.json (a hub) skips the forwarder conditions" || fail "forwarder: absent file"
  echo '{not json' > "$d/mh/forward-status.json"; run "$d" -- --status
  [[ $RC == 0 && "$(jq -r '.skipped | index("forward_status:unparseable") != null' "$d/out")" == true ]] && check "forwarder: an unparseable status file is skipped, not an alert" || fail "forwarder: unparseable"
}

# ---- units ------------------------------------------------------------------------------
setunit() { # DIR UNIT ENABLED [ACTIVE]
  printf '%s\n' "$3" > "$1/sc/enabled/$2"
  [[ -n "${4:-}" ]] && printf '%s\n' "$4" > "$1/sc/active/$2"
  return 0
}
t_units() {
  local d t=memoire-capture-tick.timer
  d="$(mkenv u1)"; run "$d" -- --status
  [[ $RC == 0 && "$(fire "$d")" == "" && "$(jq -r '.units["memoire-capture-tick.timer"]' "$d/out")" == not-installed ]] \
    && check "units: absent units are 'not installed', not an alert" || fail "units: absent ($RC) $(fire "$d")"

  setunit "$d" $t enabled active; echo "Sat 2026-10-03 12:15:00 CEST 5min left Sat 2026-10-03 12:00:12 CEST 9min ago $t memoire-capture-tick.service" > "$d/sc/timers"
  run "$d" -- --status
  [[ $RC == 0 && "$(cfire "$d" "unit:$t")" == false ]] && check "units: an enabled, active timer with a next elapse is quiet" || fail "units: healthy timer ($RC)"

  setunit "$d" $t enabled inactive; run "$d" -- --status
  [[ "$(cfire "$d" "unit:$t")" == true && "$(cval "$d" "unit:$t")" == inactive && $RC == 1 ]] && check "units: an enabled timer that is inactive fires warn" || fail "units: inactive timer"

  setunit "$d" $t enabled active; echo "n/a n/a Sat 2026-10-03 12:00:12 CEST 9min ago $t memoire-capture-tick.service" > "$d/sc/timers"; run "$d" -- --status
  [[ "$(cfire "$d" "unit:$t")" == true && "$(cval "$d" "unit:$t")" == no-next-elapse ]] && check "units: an active timer with no next elapse (n/a) fires" || fail "units: n/a next elapse"
  : > "$d/sc/timers"; run "$d" -- --status
  [[ "$(cval "$d" "unit:$t")" == no-next-elapse ]] && check "units: an active timer missing from list-timers fires" || fail "units: timer missing from the list"
  # The tick service is mid-run: systemd prints the timer with no next elapse
  # until the service ends. Not an alert.
  setunit "$d" memoire-capture-tick.service enabled activating; echo "- - Sat 2026-10-03 12:00:12 CEST 14ms ago $t memoire-capture-tick.service" > "$d/sc/timers"; run "$d" -- --status
  [[ $RC == 0 && "$(cfire "$d" "unit:$t")" == false && "$(cval "$d" "unit:$t")" == active:service-running ]] && check "units: a timer with no next elapse whose service is running is quiet" || fail "units: service mid-run ($RC) $(cval "$d" "unit:$t")"
  setunit "$d" memoire-capture-tick.service enabled inactive; run "$d" -- --status
  [[ "$(cfire "$d" "unit:$t")" == true && "$(cval "$d" "unit:$t")" == no-next-elapse ]] && check "units: no next elapse with the service idle still fires" || fail "units: idle service, no next elapse"
  # The service read times out (stub hangs on is-active of the service only): no new alert, value unknown.
  setunit "$d" memoire-capture-tick.service enabled activating; sed -i 's|^  is-active) |  is-active) [[ "$1" == *.service \&\& -e "$sc/hang-service" ]] \&\& sleep 20; |' "$d/bin/systemctl"; : > "$d/sc/hang-service"
  run "$d" MEMOIRE_HEALTH_SYSTEMCTL_TIMEOUT_S=1 -- --status
  [[ $RC == 0 && "$(cfire "$d" "unit:$t")" == false && "$(cval "$d" "unit:$t")" == unknown ]] && check "units: no next elapse and an unanswered service read is unknown, not a new alert" || fail "units: service read timeout ($RC) $(cval "$d" "unit:$t")"
  rm -f "$d/sc/hang-service"
  # The service ended between the list-timers snapshot and the service read: the fresh look sees a next elapse, quiet.
  setunit "$d" memoire-capture-tick.service enabled inactive
  cat > "$d/sc/timers-script" <<'TS'
if [[ -e "$sc/timers-second" ]]; then echo "Sat 2026-10-03 12:15:00 CEST 5min left Sat 2026-10-03 12:00:12 CEST 9min ago memoire-capture-tick.timer memoire-capture-tick.service"; else echo "- - Sat 2026-10-03 12:00:12 CEST 2s ago memoire-capture-tick.timer memoire-capture-tick.service"; : > "$sc/timers-second"; fi; exit 0
TS
  sed -i 's|^  list-timers) .*|  list-timers) source "$sc/timers-script" ;;|' "$d/bin/systemctl"; rm -f "$d/sc/timers-second"
  run "$d" -- --status
  [[ $RC == 0 && "$(cfire "$d" "unit:$t")" == false && "$(cval "$d" "unit:$t")" == active ]] && check "units: a next elapse on the fresh look after the service ended is quiet" || fail "units: fresh look ($RC) $(cval "$d" "unit:$t")"
  rm -f "$d/sc/active/memoire-capture-tick.service" "$d/sc/enabled/memoire-capture-tick.service"

  setunit "$d" $t disabled; run "$d" -- --status
  [[ $RC == 0 && "$(cfire "$d" "unit:$t")" == "" && "$(jq -r '.units["memoire-capture-tick.timer"]' "$d/out")" == disabled ]] && check "units: a disabled timer is the operator's choice, no alert" || fail "units: disabled ($RC)"

  d="$(mkenv u2)"; local f=memoire-forward.service
  setunit "$d" $f enabled inactive; run "$d" -- --status
  [[ "$(cfire "$d" "unit:$f")" == true && $RC == 1 ]] && check "units: an enabled forwarder that is not active fires" || fail "units: forwarder inactive"
  setunit "$d" $f enabled active; run "$d" -- --status
  [[ "$(cfire "$d" "unit:$f")" == false && $RC == 0 ]] && check "units: an active forwarder is quiet" || fail "units: forwarder active"

  d="$(mkenv u3)"; mkq "$d" 1 599; run "$d" -- --status
  [[ "$(cfire "$d" "unit:$f")" == false ]] && check "units: no forwarder installed, queue not yet ageing: quiet" || fail "units: not installed, young queue"
  mkq "$d" 1 600; run "$d" -- --status
  [[ "$(cfire "$d" "unit:$f")" == true && "$(cval "$d" "unit:$f")" == not-installed ]] && check "units: no forwarder installed and the queue is ageing: fires" || fail "units: not installed, old queue"
  setunit "$d" memoire.service enabled active; run "$d" -- --status
  [[ "$(cfire "$d" "unit:$f")" == false ]] && check "units: a local drainer (memoire.service active) explains the missing forwarder" || fail "units: hub drainer"

  d="$(mkenv u4)"; : > "$d/sc/unreachable"; run "$d" -- --status
  [[ $RC == 0 && "$(jq -r '.skipped | index("units:systemd-user-unreachable") != null' "$d/out")" == true ]] && check "units: no user bus for systemctl skips the unit checks" || fail "units: unreachable manager"
  env -i PATH="$TOOLS" HOME="$d/home" MEMOIRE_HOME="$d/mh" MEMOIRE_HEALTH_NOW="$NOW" bash "$CHECK" --status > "$d/out" 2> "$d/err"
  [[ $? == 0 && "$(jq -r '.skipped | index("units:no-systemctl") != null' "$d/out")" == true ]] && check "units: no systemctl on the host skips the unit checks" || fail "units: no systemctl"
}

# ---- clock ---------------------------------------------------------------------------------
t_clock() {
  local d; d="$(mkenv clock)"
  run "$d" -- --status
  [[ ! -e "$d/curl.calls" && "$(jq -r '.skipped | index("clock_skew:no-base-url") != null' "$d/out")" == true ]] && check "clock: no MEMOIRE_BASE_URL, no call" || fail "clock: called without a base URL"
  echo '{"status":"ok","clock_skew_future_events":0}' > "$d/curl"
  run "$d" MEMOIRE_BASE_URL=http://hub.test:1/ -- --status
  [[ "$(cfire "$d" clock_skew)" == false && "$(wc -l < "$d/curl.calls")" == 1 && "$(cat "$d/curl.calls")" == *"-m 3 http://hub.test:1/v1/health" && $RC == 0 ]] \
    && check "clock: skew 0 is quiet; one GET of <base>/v1/health bounded to 3 s" || fail "clock: zero ($RC) $(cat "$d/curl.calls")"
  grep -qiE 'authorization|token|-H' "$d/curl.calls" && fail "clock: the GET carries a credential or header"
  rm -f "$d/mh/health-state.json"
  echo '{"clock_skew_future_events":4}' > "$d/curl"; run "$d" MEMOIRE_BASE_URL=http://hub.test:1 -- --status
  [[ "$(cfire "$d" clock_skew)" == false && $RC == 0 ]] && check "clock: a lifetime count of 4 with no previous value only seeds, no alert" || fail "clock: seed fires ($RC)"
  # alert mode: seed 4, then the same count, then an increase, then the same again
  local nb="MEMOIRE_BASE_URL=http://hub.test:1" nn="MEMOIRE_HEALTH_NO_NOTIFY=1" r1 r2 r3 r4
  run "$d" $nb $nn; r1=$RC
  run "$d" $nb $nn; r2=$RC
  echo '{"clock_skew_future_events":6}' > "$d/curl"; run "$d" $nb $nn -- --status; local sv="$(cval "$d" clock_skew)/$(cfire "$d" clock_skew)"
  run "$d" $nb $nn; r3=$RC
  run "$d" $nb $nn; r4=$RC
  [[ "$r1$r2$r3$r4" == 0010 && "$sv" == 2/true && "$(jq -r .clock_skew_seen "$d/mh/health-state.json")" == 6 ]] && check "clock: alert only when the count rose since the previous run (delta 2), clear when it did not" || fail "clock: increase $r1$r2$r3$r4 $sv"
  echo '{"clock_skew_future_events":1}' > "$d/curl"; run "$d" $nb $nn; [[ $RC == 0 ]] && check "clock: a lower count (hub restored) does not alert" || fail "clock: lower count"
  echo '{"clock_skew_future_events":null}' > "$d/curl"; run "$d" $nb $nn -- --status
  [[ $RC == 0 && "$(jq -r '.skipped | index("clock_skew:unreachable") != null' "$d/out")" == true && "$(jq -r .clock_skew_seen "$d/mh/health-state.json")" == 1 ]] \
    && check "clock: a null count (hub read failed) is skipped and keeps the baseline" || fail "clock: null count ($RC)"
  echo 7 > "$d/curl.rc"; run "$d" MEMOIRE_BASE_URL=http://hub.test:1 -- --status
  [[ $RC == 0 && "$(jq -r '.skipped | index("clock_skew:unreachable") != null' "$d/out")" == true ]] && check "clock: an unreachable hub is fail-open (skipped, exit 0)" || fail "clock: unreachable ($RC)"
  rm -f "$d/curl.rc"; echo 'garbage' > "$d/curl"; run "$d" MEMOIRE_BASE_URL=http://hub.test:1 -- --status
  [[ $RC == 0 && "$(cfire "$d" clock_skew)" == "" ]] && check "clock: a non-JSON reply is fail-open" || fail "clock: garbage ($RC)"
  echo '{"clock_skew_future_events":4}' > "$d/curl"; rm -f "$d/curl.calls"; run "$d" MEMOIRE_BASE_URL=http://hub.test:1 MEMOIRE_HEALTH_CLOCK=0 -- --status
  [[ ! -e "$d/curl.calls" && $RC == 0 ]] && check "clock: MEMOIRE_HEALTH_CLOCK=0 skips the call" || fail "clock: switch ignored"
}

# ---- state machine + desktop ---------------------------------------------------------------
t_state() {
  local d; d="$(mkenv st)"; mkq "$d" 1 700
  local bus=MEMOIRE_HEALTH_X=1 sf="$d/mh/health-state.json"
  T=$NOW runq "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/nonexistent/bus
  [[ $RC == 1 && "$(nnotify "$d")" == 1 && "$(grep -c 'event=change' "$d/err")" == 1 && "$(grep -c '^memoire-capture-health:' "$d/err")" == 1 ]] \
    && check "state: first warn notifies once and logs one line (event=change)" || fail "state: first alert rc=$RC n=$(nnotify "$d") $(cat "$d/err")"
  [[ "$(stat -c %a "$sf")" == 600 && -z "$(ls "$d/mh" | grep 'tmp')" ]] && check "state: health-state.json is 0600, no temporary left" || fail "state: file mode/tmp $(stat -c %a "$sf")"
  grep -q '^ARG<-u>' "$d/notify.log" && sed -n 3,4p "$d/notify.log" | tr '\n' ' ' | grep -q 'ARG<normal>' && check "state: warn goes out at urgency normal" || fail "state: warn urgency: $(cat "$d/notify.log")"
  grep -q 'ARG<Memoire capture WARN on testhost>' "$d/notify.log" && grep -q 'backlog_age = 700 (threshold 600)' "$d/notify.log" && check "state: the notification names the host and the condition with value and threshold" || fail "state: text: $(cat "$d/notify.log")"

  T=$((NOW + 900)) runq "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/nonexistent/bus
  [[ $RC == 1 && "$(nnotify "$d")" == 1 && "$(grep -c 'event=none' "$d/err")" == 1 ]] && check "state: the same state 15 min later does not repeat" || fail "state: repeat n=$(nnotify "$d")"
  T=$((NOW + 21599)) runq "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/nonexistent/bus
  [[ "$(nnotify "$d")" == 1 ]] && check "state: one second before the reminder (21599 s): silent" || fail "state: early reminder"
  T=$((NOW + 21600)) runq "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/nonexistent/bus
  [[ "$(nnotify "$d")" == 2 && "$(grep -c 'event=reminder' "$d/err")" == 1 ]] && grep -q '(reminder)' "$d/notify.log" && check "state: reminder at 21600 s" || fail "state: reminder n=$(nnotify "$d")"
  T=$((NOW + 21700)) runq "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/nonexistent/bus
  [[ "$(nnotify "$d")" == 2 ]] && check "state: the reminder clock restarts at the reminder" || fail "state: reminder clock"

  # escalation warn -> critical
  rm -f "$d"/mh/queue/pending/*; mkq "$d" 1 30000
  T=$((NOW + 21800)) run "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/nonexistent/bus
  [[ $RC == 2 && "$(nnotify "$d")" == 3 ]] && tail -12 "$d/notify.log" | grep -q 'ARG<critical>' && check "state: warn to critical notifies at urgency critical" || fail "state: escalation rc=$RC $(tail -12 "$d/notify.log")"

  # recovery
  rm -f "$d"/mh/queue/pending/*
  T=$((NOW + 21900)) run "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/nonexistent/bus
  [[ $RC == 0 && "$(nnotify "$d")" == 4 && "$(grep -c 'event=recovered' "$d/err")" == 1 ]] && grep -q 'ARG<Memoire capture healthy again on testhost>' "$d/notify.log" && check "state: recovery sends one 'healthy again' notice, exit 0" || fail "state: recovery n=$(nnotify "$d")"
  T=$((NOW + 22800)) run "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/nonexistent/bus
  T=$((NOW + 99999)) run "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/nonexistent/bus
  [[ "$(nnotify "$d")" == 4 && "$(jq -r .state "$sf")" == ok ]] && check "state: healthy runs after recovery stay silent" || fail "state: silence after recovery"

  # a changed set of conditions inside the same severity is a change
  d="$(mkenv st2)"; mkq "$d" 1 700
  runq "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/x
  : > "$d/mh/queue/failed/1-x.json"
  T=$((NOW + 60)) runq "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/x
  [[ "$(nnotify "$d")" == 2 ]] && check "state: a new condition under the same severity notifies" || fail "state: new condition n=$(nnotify "$d")"
  # a corrupt state file is a first run
  d="$(mkenv st3)"; mkq "$d" 1 700; echo '{bad' > "$d/mh/health-state.json"
  runq "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/x
  [[ "$(nnotify "$d")" == 1 && "$(jq -r .state "$d/mh/health-state.json")" == warn ]] && check "state: a corrupt state file is treated as a first run and rewritten" || fail "state: corrupt file"
  # a healthy first run writes nothing to the desktop
  d="$(mkenv st4)"; runq "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/x
  [[ "$(nnotify "$d")" == 0 && $RC == 0 ]] && check "state: a healthy first run is silent" || fail "state: healthy first run"
}

t_desktop() {
  local d; d="$(mkenv dk)"; mkq "$d" 1 700
  rm -f "$d/bin/notify-send"
  run "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/x
  [[ $RC == 1 && "$(grep -c 'notify=no-notify-send' "$d/err")" == 1 && "$(jq -r .state "$d/mh/health-state.json")" == warn ]] && check "desktop: no notify-send: no error, the log carries it, state still recorded" || fail "desktop: absent binary rc=$RC $(cat "$d/err")"

  d="$(mkenv dk2)"; mkq "$d" 1 700
  run "$d" XDG_RUNTIME_DIR="$d/xdg"
  [[ $RC == 1 && "$(grep -c 'notify=no-bus' "$d/err")" == 1 && ! -e "$d/notify.log" ]] && check "desktop: no session bus: notify-send not called, log says no-bus" || fail "desktop: no bus rc=$RC $(cat "$d/err")"

  d="$(mkenv dk3)"; mkq "$d" 1 700
  if command -v python3 >/dev/null; then
    python3 -c "import socket,sys; s=socket.socket(socket.AF_UNIX); s.bind(sys.argv[1])" "$d/xdg/bus"
    run "$d" XDG_RUNTIME_DIR="$d/xdg"
    grep -q "^BUS=unix:path=$d/xdg/bus\$" "$d/notify.log" && check "desktop: a bus socket under XDG_RUNTIME_DIR is found and passed to notify-send" || fail "desktop: XDG bus: $(cat "$d/notify.log" 2>&1) $(cat "$d/err")"
  fi
  d="$(mkenv dk4)"; mkq "$d" 1 700
  run "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/x STUB_NOTIFY_RC=1
  [[ $RC == 1 && "$(grep -c 'notify=failed' "$d/err")" == 1 ]] && check "desktop: a failing notify-send is logged, not fatal" || fail "desktop: failing notify-send"
  d="$(mkenv dk5)"; mkq "$d" 1 700
  run "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/x -- --status
  [[ ! -e "$d/notify.log" && ! -e "$d/mh/health-state.json" ]] && check "desktop: --status never notifies and never writes state" || fail "desktop: --status side effect"
}

# ---- sink -------------------------------------------------------------------------------------
mksink() { # DIR BODY: executable sink recording stdin, event and state
  local f="$1/sink.sh"
  printf '#!/usr/bin/env bash\ncat > "%s/sink.in.$$"\necho "$MEMOIRE_HEALTH_EVENT $MEMOIRE_HEALTH_STATE" >> "%s/sink.env"\n%s\n' "$1" "$1" "${2:-exit 0}" > "$f"
  chmod +x "$f"; printf '%s' "$f"
}
t_sink() {
  local d s; d="$(mkenv sk)"; mkq "$d" 1 700; s="$(mksink "$d")"
  run "$d" MEMOIRE_HEALTH_SINK="$s"
  local in; in="$(cat "$d"/sink.in.* 2>/dev/null)"
  [[ "$(jq -r '[.state, .event, .previous_state, .host, (.conditions | length > 0)] | @tsv' <<<"$in")" == $'warn\tchange\tnone\ttesthost\ttrue' && "$(cat "$d/sink.env")" == "change warn" && "$(grep -c 'sink=ok' "$d/err")" == 1 ]] \
    && check "sink: receives the status JSON with event and previous_state on stdin, event/state in its environment" || fail "sink: payload: $in $(cat "$d/err")"
  rm -f "$d"/sink.in.*
  T=$((NOW + 900)) runq "$d" MEMOIRE_HEALTH_SINK="$s"
  [[ "$(wc -l < "$d/sink.env")" == 1 ]] && check "sink: not called when nothing changed" || fail "sink: called without an event"
  T=$((NOW + 21600)) runq "$d" MEMOIRE_HEALTH_SINK="$s"
  [[ "$(tail -1 "$d/sink.env")" == "reminder warn" ]] && check "sink: called on a reminder" || fail "sink: reminder"
  rm -f "$d"/mh/queue/pending/*; T=$((NOW + 22000)) run "$d" MEMOIRE_HEALTH_SINK="$s"
  [[ "$(tail -1 "$d/sink.env")" == "recovered ok" && $RC == 0 ]] && check "sink: called on recovery with state ok" || fail "sink: recovery"

  d="$(mkenv sk2)"; mkq "$d" 1 700; s="$(mksink "$d" 'exit 3')"
  run "$d" MEMOIRE_HEALTH_SINK="$s"
  [[ $RC == 1 && "$(grep -c 'sink=failed:3' "$d/err")" == 1 && "$(jq -r .state "$d/mh/health-state.json")" == warn ]] && check "sink: a failing sink is logged (failed:3), never fatal, state still recorded" || fail "sink: failure rc=$RC $(cat "$d/err")"

  d="$(mkenv sk3)"; mkq "$d" 1 700; s="$(mksink "$d" 'sleep 30')"
  local t0=$SECONDS
  run "$d" MEMOIRE_HEALTH_SINK="$s" MEMOIRE_HEALTH_SINK_TIMEOUT_S=1
  [[ $RC == 1 && "$(grep -c 'sink=timeout' "$d/err")" == 1 && $((SECONDS - t0)) -lt 8 ]] && check "sink: a hanging sink is killed at its timeout, logged, not fatal" || fail "sink: timeout rc=$RC t=$((SECONDS - t0)) $(cat "$d/err")"

  d="$(mkenv sk4)"; mkq "$d" 1 700; : > "$d/plain"
  run "$d" MEMOIRE_HEALTH_SINK="$d/plain"
  [[ $RC == 1 && "$(grep -c 'sink=not-executable' "$d/err")" == 1 ]] && check "sink: a non-executable path is logged, not fatal" || fail "sink: not executable"
  run "$d" MEMOIRE_HEALTH_SINK="$d/missing"
  [[ $RC == 1 ]] && check "sink: a missing path is not fatal" || fail "sink: missing"
  d="$(mkenv sk5)"; mkq "$d" 1 700; s="$(mksink "$d")"
  printf 'MEMOIRE_HEALTH_SINK=%s\n' "$s" > "$d/mh/capture-tick.env"
  run "$d"
  [[ -f "$d/sink.env" ]] && check "sink: MEMOIRE_HEALTH_SINK is read from capture-tick.env" || fail "sink: env file"
}

# ---- knobs ---------------------------------------------------------------------------------------
t_knobs() {
  local d; d="$(mkenv kn)"; mkq "$d" 3 5
  run "$d" MEMOIRE_INJECT_BACKLOG_MIN=3 -- --status
  [[ "$(cfire "$d" backlog_depth)" == true && "$(jq -r '.conditions[0].threshold' "$d/out")" == 3 ]] && check "knobs: MEMOIRE_INJECT_BACKLOG_MIN from the environment" || fail "knobs: env"
  printf 'MEMOIRE_INJECT_BACKLOG_MIN=2\nexport MEMOIRE_INJECT_BACKLOG_MIN="4" # later wins\n' > "$d/mh/capture-tick.env"
  run "$d" MEMOIRE_INJECT_BACKLOG_MIN=2 -- --status
  [[ "$(jq -r '.conditions[0].threshold' "$d/out")" == 4 && "$(cfire "$d" backlog_depth)" == false ]] && check "knobs: the env file wins over the environment, last assignment wins, export/quotes/comment tolerated" || fail "knobs: file: $(jq -c '.conditions[0]' "$d/out")"
  printf 'MEMOIRE_INJECT_BACKLOG_AGE_S=50\nMEMOIRE_INJECT_TRANSPORT_DEAD_S=100\nMEMOIRE_HEALTH_FAIL_N=2\nMEMOIRE_HEALTH_STALE_S=60\nMEMOIRE_HEALTH_GRACE_S=10\n' > "$d/mh/capture-tick.env"
  rm -f "$d"/mh/queue/pending/*; mkq "$d" 1 120; fwd "$d" 2 $((NOW - 150)); setunit "$d" memoire-forward.service enabled active; run "$d" -- --status
  [[ "$(fire "$d")" == "backlog_age,transport_dead,forward_failures,forward_stale" && $RC == 2 ]] && check "knobs: age, dead, fail count, stale and grace are all tunable from the file" || fail "knobs: tunables: $(fire "$d")"

  d="$(mkenv kn2)"; mkq "$d" 25 5
  printf 'MEMOIRE_INJECT_BACKLOG_MIN=abc\nMEMOIRE_INJECT_BACKLOG_AGE_S=0\nMEMOIRE_INJECT_TRANSPORT_DEAD_S=99999999999\nMEMOIRE_HEALTH_REMIND_S=-5\n' > "$d/mh/capture-tick.env"
  run "$d" MEMOIRE_HEALTH_FAIL_N=1.5 -- --status
  [[ "$(jq -r '[.conditions[0].threshold, .conditions[1].threshold, .conditions[3].threshold] | @tsv' "$d/out")" == $'25\t600\t21600' && $RC == 1 \
     && "$(grep -o 'invalid_knobs=[^ ]*' "$d/err")" == "invalid_knobs=MEMOIRE_INJECT_BACKLOG_MIN,MEMOIRE_INJECT_BACKLOG_AGE_S,MEMOIRE_INJECT_TRANSPORT_DEAD_S,MEMOIRE_HEALTH_FAIL_N,MEMOIRE_HEALTH_REMIND_S" ]] \
    && check "knobs: invalid values (word, 0, overflow, negative, decimal) fall back to the defaults and are named in the log" || fail "knobs: invalid: $(cat "$d/err")"
  rm -f "$d/mh/capture-tick.env"; run "$d" MEMOIRE_HEALTH_REMIND_S=60 DBUS_SESSION_BUS_ADDRESS=unix:path=/x
  T=$((NOW + 61)) run "$d" MEMOIRE_HEALTH_REMIND_S=60 DBUS_SESSION_BUS_ADDRESS=unix:path=/x
  [[ "$(nnotify "$d")" == 2 ]] && check "knobs: MEMOIRE_HEALTH_REMIND_S tunes the reminder" || fail "knobs: remind"
}

# ---- status shape + exit codes ------------------------------------------------------------------------
t_status() {
  local d; d="$(mkenv ss)"; mkq "$d" 1 700; fwd "$d" 1 "$NOW"; setunit "$d" memoire-forward.service enabled active
  run "$d" -- --status
  [[ "$(wc -l < "$d/out")" == 1 ]] && jq -e '
      (.state | IN("ok","warn","critical")) and (.host | type == "string") and (.time | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]{8}Z$")) and (.time_epoch | type == "number")
      and (.firing | type == "array") and (.skipped | type == "array") and (.units | type == "object")
      and (.queue | has("pending") and has("oldest_pending_age_s") and has("failed"))
      and (.forwarder | has("present") and has("consecutive_failures") and has("last_success_epoch"))
      and (.conditions | length >= 6 and all(has("name") and has("firing") and has("severity") and has("value") and has("threshold") and has("detail")))' "$d/out" >/dev/null \
    && check "status: --status prints one JSON object with state, host, time, conditions (value, threshold), queue, forwarder, units" || fail "status: shape: $(cat "$d/out")"
  [[ "$(jq -r '.time' "$d/out")" == "2027-01-15T08:00:00Z" ]] && check "status: time is UTC ISO 8601 from the epoch" || fail "status: time $(jq -r .time "$d/out")"
  [[ "$(grep -c '^memoire-capture-health:' "$d/err")" == 1 && "$(grep -c 'mode=status' "$d/err")" == 1 ]] && check "status: one stderr log line per run in name: key=value form" || fail "status: log line: $(cat "$d/err")"
  # exit codes: ok 0 / warn 1 / critical 2, alert mode and status mode agree
  local d2; d2="$(mkenv ec)"
  run "$d2" DBUS_SESSION_BUS_ADDRESS=unix:path=/x; local r0=$RC
  mkq "$d2" 1 700; run "$d2" DBUS_SESSION_BUS_ADDRESS=unix:path=/x; local r1=$RC; run "$d2" -- --status; local s1=$RC
  mkq "$d2" 1 40000; run "$d2" DBUS_SESSION_BUS_ADDRESS=unix:path=/x; local r2=$RC; run "$d2" -- --status; local s2=$RC
  [[ "$r0$r1$s1$r2$s2" == 01122 ]] && check "status: exit codes ok 0, warn 1, critical 2 in both modes" || fail "status: exit codes $r0$r1$s1$r2$s2"
}

# ---- the check's own errors -----------------------------------------------------------------------------
t_own() {
  local d; d="$(mkenv own)"; mkq "$d" 1 40000
  if [[ "$(id -u)" != 0 ]]; then
    chmod 000 "$d/mh"
    run "$d"; local rc=$RC
    chmod 755 "$d/mh"
    [[ $rc == 0 && "$(grep -c 'error=memoire-home-unreadable' "$d/err")" == 1 && "$(grep -c . "$d/err")" == 1 && ! -s "$d/out" ]] && check "own: an unreadable MEMOIRE_HOME exits 0 with one log line" || fail "own: unreadable home rc=$rc $(cat "$d/err")"
    chmod 000 "$d/mh"; run "$d" -- --status; rc=$RC; chmod 755 "$d/mh"
    [[ $rc == 0 ]] && check "own: --status on an unreadable home also exits 0" || fail "own: status unreadable"
  fi
  run "$d" MEMOIRE_HOME="$d/does-not-exist"
  [[ $RC == 0 && "$(grep -c 'error=memoire-home-unreadable' "$d/err")" == 1 ]] && check "own: a missing MEMOIRE_HOME exits 0 with one log line" || fail "own: missing home"
  run "$d" -- --bogus
  [[ $RC == 0 && "$(grep -c 'error=bad-argument' "$d/err")" == 1 ]] && check "own: a bad argument logs and exits 0" || fail "own: bad arg"
  env -i PATH="$d/bin" HOME="$d/home" MEMOIRE_HOME="$d/mh" "$(command -v bash)" "$CHECK" > "$d/out" 2> "$d/err"
  [[ $? == 0 && "$(grep -c 'error=jq-missing' "$d/err")" == 1 ]] && check "own: no jq logs and exits 0" || fail "own: jq missing: $(cat "$d/err")"
  chmod 500 "$d/mh"; run "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/x TMPDIR="$d/nowhere"; local rc2=$RC; chmod 755 "$d/mh"
  [[ $rc2 == 2 && "$(grep -c 'state_write=failed' "$d/err")" == 1 ]] && check "own: a state file that cannot be written is logged; the condition's exit code stands" || fail "own: state write rc=$rc2 $(cat "$d/err")"
}

# ---- cost ----------------------------------------------------------------------------------------------------
t_cost() {
  local d; d="$(mkenv cost)"; mkq "$d" 30 700; setunit "$d" memoire-forward.service enabled active
  local t0 t1 i; t0="$(date +%s%N)"
  for i in 1 2 3; do run "$d" -- --status; done
  t1="$(date +%s%N)"
  (( (t1 - t0) / 3000000 < 1500 )) && check "cost: a run takes $(( (t1 - t0) / 3000000 )) ms (bound 1500 ms in this test)" || fail "cost: slow run $(( (t1 - t0) / 3000000 )) ms"
}

# ---- timer ----------------------------------------------------------------------------------------------------
real_fp() { stat -c '%n %Y %s' "$HOME/.config/systemd/user/memoire-capture-health".{timer,service} 2>/dev/null | paste -sd'|' -; }
t_timer() {
  local REAL_FP0; REAL_FP0="$(real_fp)"
  local d; d="$(mkenv tm)"; local cfg="$d/cfg" home="$d/home"
  mkdir -p "$cfg"
  tcall() { env -i PATH="$d/bin:$TOOLS" HOME="$home" XDG_CONFIG_HOME="$cfg" STUB_SC="$d/sc" "$@" bash "$TIMER_SH" "${V:-install}" > "$d/tout" 2> "$d/terr"; }
  V=install tcall
  local rc=$? svc="$cfg/systemd/user/memoire-capture-health.service" tim="$cfg/systemd/user/memoire-capture-health.timer"
  [[ $rc == 0 && -f "$svc" && -f "$tim" ]] && check "timer: install writes the service and the timer under XDG_CONFIG_HOME" || fail "timer: install rc=$rc $(cat "$d/terr")"
  grep -q "^ExecStart=$ROOT/scripts/memoire-capture-health.sh\$" "$svc" && grep -q '^EnvironmentFile=-%h/.memoire/capture-tick.env$' "$svc" && grep -q '^Type=oneshot$' "$svc" && grep -q '^SuccessExitStatus=1 2$' "$svc" \
    && check "timer: the service runs the check, reads the optional env file, accepts exit 1 and 2" || fail "timer: service content: $(cat "$svc")"
  grep -q '^OnCalendar=\*:0/15$' "$tim" && grep -q '^Persistent=true$' "$tim" && grep -q '^RandomizedDelaySec=[1-9]' "$tim" && grep -q '^WantedBy=timers.target$' "$tim" \
    && check "timer: every 15 minutes, Persistent=true, randomized delay" || fail "timer: timer content: $(cat "$tim")"
  grep -qiE 'token|password|secret|credential|LoadCredential|Bearer' "$svc" "$tim" && fail "timer: a credential or token reference in the units" || check "timer: no credential in the units"
  # The canonical root the installer warns about is the repo's main worktree
  # (git-derived; no personal checkout layout in the warning itself). The
  # installer runs with a restricted PATH (no git), so the script falls back
  # to the .git file when this checkout is a linked worktree.
  CANON="$(git -C "$ROOT" worktree list --porcelain 2>/dev/null | awk 'NR==1{print $2}' || true)"
  if [[ -n "$CANON" && "$CANON" != "$ROOT" ]]; then
    [[ "$(cat "$d/terr")" == *"WARNING: persisting ExecStart under $ROOT"* ]] && check "timer: a non-canonical checkout warns about the persisted ExecStart path" || fail "timer: canonical-root warning: $(cat "$d/terr")"
  else
    ! grep -q 'WARNING: persisting ExecStart' "$d/terr" || fail "timer: the canonical checkout must not warn: $(cat "$d/terr")"
  fi
  grep -q 'daemon-reload' "$d/sc/calls" && grep -q 'enable --now memoire-capture-health.timer' "$d/sc/calls" && check "timer: install reloads and enables the timer (stub)" || fail "timer: install calls: $(cat "$d/sc/calls")"
  : > "$d/sc/calls"
  for V in start stop status run-once; do V=$V tcall; done
  grep -q 'start memoire-capture-health.timer' "$d/sc/calls" && grep -q 'disable --now memoire-capture-health.timer' "$d/sc/calls" && grep -q 'status memoire-capture-health.timer' "$d/sc/calls" && grep -q 'start memoire-capture-health.service' "$d/sc/calls" \
    && check "timer: start, stop, status and run-once reach the right units" || fail "timer: verbs: $(cat "$d/sc/calls")"
  V=uninstall tcall
  [[ ! -e "$svc" && ! -e "$tim" ]] && check "timer: uninstall removes both unit files" || fail "timer: uninstall"
  V=bogus tcall; [[ $? == 1 && "$(cat "$d/terr")" == usage:* ]] && check "timer: an unknown verb prints usage and exits 1" || fail "timer: usage"
  # The real user config may already carry the timer (an installed machine): compare a
  # fingerprint taken before the calls, instead of asserting absence.
  [[ "$(real_fp)" == "$REAL_FP0" ]] && check "timer: nothing changed under the real user config" || fail "timer: real config touched"
}

# ---- round 2: review fixes ---------------------------------------------------------------------------------
t_r2() {
  local d s in t0 t1
  # F1: previous_state is ok after a healthy run
  d="$(mkenv f1)"; s="$(mksink "$d")"
  run "$d" MEMOIRE_HEALTH_SINK="$s"; mkq "$d" 1 700
  T=$((NOW + 900)) runq "$d" MEMOIRE_HEALTH_SINK="$s"
  in="$(cat "$d"/sink.in.* 2>/dev/null)"
  [[ "$(jq -r .previous_state <<<"$in")" == ok && "$(grep -c 'prev=ok event=change' "$d/err")" == 1 ]] && check "F1: previous_state is ok (sink and log) on an ok to warn transition" || fail "F1: previous_state: $in $(cat "$d/err")"

  # F2: a hanging systemctl is bounded, unknown, never a condition
  d="$(mkenv f2)"; setunit "$d" memoire-forward.service enabled active; setunit "$d" memoire-capture-tick.timer enabled active
  : > "$d/sc/hang"; t0="$(date +%s%N)"
  run "$d" MEMOIRE_HEALTH_SYSTEMCTL_TIMEOUT_S=1 -- --status
  t1="$(date +%s%N)"
  [[ $RC == 0 && $(( (t1 - t0) / 1000000 )) -lt 4000 && "$(wc -l < "$d/sc/calls")" == 1 && "$(grep -c 'systemctl_timeouts=1' "$d/err")" == 1 \
     && "$(jq -r '.skipped | index("units:systemctl-timeout") != null' "$d/out")" == true && "$(fire "$d")" == "" && "$(jq -r '.units["memoire-forward.service"]' "$d/out")" == unknown ]] \
    && check "F2: a hanging systemctl is cut at the knob ($(( (t1 - t0) / 1000000 )) ms), one call only, units unknown, no condition, exit 0" || fail "F2: hang rc=$RC t=$(( (t1 - t0) / 1000000 )) calls=$(wc -l < "$d/sc/calls") $(cat "$d/err") $(fire "$d")"

  # F3 / F5: cleared, remind_s, a pure clearing does not reach the desktop but reaches the sink
  d="$(mkenv f3)"; s="$(mksink "$d")"; mkq "$d" 30 700; setunit "$d" memoire-forward.service enabled active
  runq "$d" MEMOIRE_HEALTH_SINK="$s" DBUS_SESSION_BUS_ADDRESS=unix:path=/x
  rm -f "$d"/mh/queue/pending/*; mkq "$d" 1 700; rm -f "$d"/sink.in.*
  T=$((NOW + 60)) runq "$d" MEMOIRE_HEALTH_SINK="$s" DBUS_SESSION_BUS_ADDRESS=unix:path=/x
  in="$(cat "$d"/sink.in.* 2>/dev/null)"
  [[ "$(jq -c '[.event, .state, .firing, .cleared, .remind_s]' <<<"$in")" == '["change","warn",["backlog_age"],["backlog_depth"],21600]' && "$(nnotify "$d")" == 1 && "$(grep -c 'notify=suppressed-clearing' "$d/err")" == 1 ]] \
    && check "F3/F5: a condition that clears at the same state: sink gets change with cleared and the remaining firing, no desktop toast" || fail "F3/F5: clearing: $in n=$(nnotify "$d") $(cat "$d/err")"
  T=$((NOW + 120)) runq "$d" MEMOIRE_HEALTH_SINK="$s" DBUS_SESSION_BUS_ADDRESS=unix:path=/x
  : > "$d/mh/queue/failed/1-x.json"; rm -f "$d"/sink.in.*
  T=$((NOW + 180)) runq "$d" MEMOIRE_HEALTH_SINK="$s" DBUS_SESSION_BUS_ADDRESS=unix:path=/x
  [[ "$(nnotify "$d")" == 2 && "$(jq -c .cleared "$d"/sink.in.*)" == '[]' ]] && check "F5: a newly added condition still notifies; cleared is empty when nothing cleared" || fail "F5: addition n=$(nnotify "$d")"
  rm -f "$d"/mh/queue/pending/* "$d"/mh/queue/failed/*; rm -f "$d"/sink.in.*
  T=$((NOW + 240)) runq "$d" MEMOIRE_HEALTH_SINK="$s" DBUS_SESSION_BUS_ADDRESS=unix:path=/x
  [[ "$(jq -c '[.event, .cleared]' "$d"/sink.in.*)" == '["recovered",["backlog_age","failed_items"]]' ]] && check "F3: a recovered event carries every cleared condition" || fail "F3: recovered cleared: $(cat "$d"/sink.in.*)"

  # F6: transport dead against a live forwarder
  d="$(mkenv f6)"; mkq "$d" 3 30000; setunit "$d" memoire-forward.service enabled active
  fwd "$d" 2 $((NOW - 100)); run "$d" -- --status
  [[ "$(cfire "$d" transport_dead)" == false && "$(cfire "$d" backlog_age)" == true && $RC == 1 ]] && check "F6: old items, a forwarder running with a fresh success (laptop after resume): backlog warn, not dead" || fail "F6: laptop rc=$RC $(fire "$d")"
  fwd "$d" 2 0; run "$d" -- --status
  [[ "$(cfire "$d" transport_dead)" == true && $RC == 2 ]] && check "F6: no success yet with an old queue is dead" || fail "F6: never rc=$RC $(fire "$d")"
  fwd "$d" 2 $((NOW - 21600)); run "$d" -- --status
  [[ "$(cfire "$d" transport_dead)" == true ]] && check "F6: a last success older than the dead threshold is dead" || fail "F6: old success"
  fwd "$d" 2 $((NOW - 100)); setunit "$d" memoire-forward.service enabled inactive; run "$d" -- --status
  [[ "$(cfire "$d" transport_dead)" == true ]] && check "F6: a fresh success but the forwarder not running is dead" || fail "F6: not running"
  rm -f "$d/sc/enabled/memoire-forward.service" "$d/sc/active/memoire-forward.service"; run "$d" -- --status
  [[ "$(cfire "$d" transport_dead)" == true ]] && check "F6: a fresh success but no forwarder unit is dead" || fail "F6: not installed"
  d="$(mkenv f6b)"; mkq "$d" 1 30000; run "$d" -- --status
  [[ "$(cfire "$d" transport_dead)" == true ]] && check "F6: no forwarder status file (hub): age alone decides" || fail "F6: hub"

  # F7: inflight counts for depth and age; a failed tick service is a warn
  d="$(mkenv f7)"; mkq "$d" 24 5; mkq "$d" 1 5 inflight; run "$d" -- --status
  [[ "$(cfire "$d" backlog_depth)" == true && "$(cval "$d" backlog_depth)" == 25 && "$(jq -r '.queue | [.pending, .inflight] | @tsv' "$d/out")" == $'24\t1' ]] && check "F7: 24 pending + 1 inflight reaches the depth threshold; queue shows both" || fail "F7: inflight depth: $(jq -c .queue "$d/out")"
  d="$(mkenv f7b)"; mkq "$d" 1 5; mkq "$d" 1 700 inflight; run "$d" -- --status
  [[ "$(cfire "$d" backlog_age)" == true && "$(cval "$d" backlog_age)" == 700 ]] && check "F7: an old inflight item sets the oldest age" || fail "F7: inflight age"
  d="$(mkenv f7c)"; local t=memoire-capture-tick.timer
  setunit "$d" $t enabled active; echo "Sat 2026-10-03 12:15:00 CEST 5min left Sat 2026-10-03 12:00:12 CEST 9min ago $t memoire-capture-tick.service" > "$d/sc/timers"
  run "$d" -- --status
  [[ "$(cfire "$d" unit_failed)" == false && $RC == 0 ]] && check "F7: a tick service whose last run succeeded: unit_failed quiet" || fail "F7: unit_failed on success"
  printf 'Result=exit-code\nExecMainStatus=1\n' > "$d/sc/result/memoire-capture-tick.service"; run "$d" -- --status
  [[ "$(cfire "$d" unit_failed)" == true && "$(cfire "$d" "unit:$t")" == false && $RC == 1 && "$(jq -r '.conditions[] | select(.name == "unit_failed") | .detail' "$d/out")" == *memoire-capture-tick.service* ]] \
    && check "F7: a failed tick service fires unit_failed (warn) while the timer is active with a next elapse" || fail "F7: unit_failed rc=$RC $(fire "$d")"

  # F8: a sink that ignores TERM adds no second stderr line
  d="$(mkenv f8)"; mkq "$d" 1 700; s="$(mksink "$d" 'trap "" TERM; while :; do sleep 1; done')"
  t0=$SECONDS; run "$d" MEMOIRE_HEALTH_SINK="$s" MEMOIRE_HEALTH_SINK_TIMEOUT_S=1
  [[ $RC == 1 && "$(grep -c . "$d/err")" == 1 && "$(grep -c 'sink=timeout' "$d/err")" == 1 && $((SECONDS - t0)) -lt 8 ]] && check "F8: a sink killed at its timeout leaves exactly one log line" || fail "F8: lines: $(cat "$d/err")"

  # F9: HOME and MEMOIRE_HOME both unset
  d="$(mkenv f9)"
  env -i PATH="$d/bin:$TOOLS" bash "$CHECK" > "$d/out" 2> "$d/err"
  [[ $? == 0 && "$(grep -c . "$d/err")" == 1 && "$(grep -c 'error=memoire-home-unset' "$d/err")" == 1 ]] && check "F9: neither HOME nor MEMOIRE_HOME set: exit 0, one log line" || fail "F9: $(cat "$d/err")"

  # F10: an unwritable state file is state unknown
  if [[ "$(id -u)" != 0 ]]; then
    d="$(mkenv f10)"; mkq "$d" 1 700; s="$(mksink "$d")"; chmod 500 "$d/mh"
    for t in 1 2 3; do run "$d" MEMOIRE_HEALTH_SINK="$s" DBUS_SESSION_BUS_ADDRESS=unix:path=/x TMPDIR="$d/nowhere"; done
    chmod 755 "$d/mh"
    [[ $RC == 1 && "$(nnotify "$d")" == 0 && ! -e "$d/sink.env" && "$(grep -c 'state_write=failed' "$d/err")" == 1 && "$(grep -c . "$d/err")" == 1 ]] && check "F10: unwritable state, warn: one log line per run, no notification and no sink call, exit 1" || fail "F10: warn n=$(nnotify "$d") $(cat "$d/err")"
    d="$(mkenv f10b)"; mkq "$d" 1 40000; chmod 500 "$d/mh"
    for t in 1 2; do run "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/x TMPDIR="$d/nowhere"; done
    chmod 755 "$d/mh"
    [[ $RC == 2 && "$(nnotify "$d")" == 2 && "$(grep -c 'event=reminder' "$d/err")" == 0 ]] && check "F10: unwritable state, critical: notifies (never as a reminder), exit 2" || fail "F10: critical n=$(nnotify "$d")"
    # F10 (round-3 note): a failed write over an existing state file still reports previous_state none
    d="$(mkenv f10c)"; s="$(mksink "$d")"; mkq "$d" 1 700
    run "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/x
    mkq "$d" 1 40000; chmod 500 "$d/mh"
    T=$((NOW + 60)) run "$d" MEMOIRE_HEALTH_SINK="$s" DBUS_SESSION_BUS_ADDRESS=unix:path=/x TMPDIR="$d/nowhere"
    chmod 755 "$d/mh"
    [[ $RC == 2 && "$(jq -c '[.event, .previous_state, .state, .cleared]' "$d"/sink.in.*)" == '["change","none","critical",[]]' && "$(grep -c 'prev=none event=change' "$d/err")" == 1 ]] \
      && check "F10: a failed state write over an existing state file reports previous_state none and cleared []" || fail "F10: previous_state over existing file: $(cat "$d"/sink.in.* 2>/dev/null) $(cat "$d/err")"
  fi
}

# ---- round 3 -----------------------------------------------------------------------------------------------
t_r3() {
  local d s t0 t1 u=memoire-forward.service
  # G1: alert, systemctl timeout, alert, recovery: no false recovery, one toast
  d="$(mkenv g1)"; s="$(mksink "$d")"; setunit "$d" $u enabled failed
  local env=(MEMOIRE_HEALTH_SINK="$s" DBUS_SESSION_BUS_ADDRESS=unix:path=/x MEMOIRE_HEALTH_SYSTEMCTL_TIMEOUT_S=1)
  T=$NOW run "$d" "${env[@]}"
  : > "$d/sc/hang"; T=$((NOW + 900)) run "$d" "${env[@]}"
  local rc2=$RC out2 err2; out2="$(cat "$d/out")"; err2="$(cat "$d/err")"
  [[ "$rc2" == 1 && "$err2" == *"event=none"* && "$err2" == *"firing=unit:$u"* && "$err2" != *recovered* && "$(nnotify "$d")" == 1 \
     && "$(wc -l < "$d/sink.env")" == 1 ]] && check "G1: a systemctl timeout keeps the firing unit condition: no event, no recovery toast, no sink call" || fail "G1: timeout run rc=$rc2 n=$(nnotify "$d") $err2"
  run "$d" "${env[@]}" -- --status
  jq -e --arg u "unit:$u" '(.conditions[] | select(.name == $u) | [.firing, .value, (.detail | test("did not answer"))]) == [true, "unknown", true] and (.skipped | index("units:systemctl-timeout") != null)' "$d/out" >/dev/null 2>&1 \
    && check "G1: the carried condition has value unknown and a detail saying the manager did not answer" || fail "G1: carried condition: $(jq -c '.conditions[]|select(.firing)' "$d/out")"
  rm -f "$d/sc/hang"; T=$((NOW + 1800)) run "$d" "${env[@]}"
  local rc3=$RC err3; err3="$(cat "$d/err")"; T=$((NOW + 1800)) run "$d" "${env[@]}" -- --status
  [[ $rc3 == 1 && "$err3" == *"event=none"* && "$(nnotify "$d")" == 1 && "$(jq -r '.conditions[] | select(.name == "unit:'$u'") | .value' "$d/out")" == failed ]] \
    && check "G1: when the manager answers again the real state takes over, still one toast" || fail "G1: third run rc=$rc3 $err3"
  setunit "$d" $u enabled active; T=$((NOW + 2700)) run "$d" "${env[@]}"
  [[ "$(grep -c 'event=recovered' "$d/err")" == 1 && "$(nnotify "$d")" == 2 ]] && check "G1: a real recovery is still announced" || fail "G1: recovery: $(cat "$d/err")"
  # an unreachable manager, and a timeout of is-enabled with unit_failed firing, behave the same
  d="$(mkenv g1b)"; s="$(mksink "$d")"; local t=memoire-capture-tick.timer
  setunit "$d" $t enabled active; echo "Sat 2026-10-03 12:15:00 CEST 5min left Sat 2026-10-03 12:00:12 CEST 9min ago $t memoire-capture-tick.service" > "$d/sc/timers"
  printf 'Result=exit-code\nExecMainStatus=1\n' > "$d/sc/result/memoire-capture-tick.service"
  T=$NOW run "$d" MEMOIRE_HEALTH_SINK="$s"
  : > "$d/sc/unreachable"; T=$((NOW + 900)) run "$d" MEMOIRE_HEALTH_SINK="$s"
  local rcu=$RC erru; erru="$(cat "$d/err")"; T=$((NOW + 900)) run "$d" MEMOIRE_HEALTH_SINK="$s" -- --status
  [[ $rcu == 1 && "$erru" == *"event=none"* && "$(jq -r '.conditions[] | select(.name == "unit_failed") | [.firing, .value] | @tsv' "$d/out")" == $'true\tunknown' && "$(wc -l < "$d/sink.env")" == 1 ]] \
    && check "G1: an unreachable user manager keeps unit_failed firing as unknown, no recovery" || fail "G1: unreachable rc=$rcu $erru"

  # an is-active answer that cannot be read is no answer either
  d="$(mkenv g1c)"; setunit "$d" $u enabled failed
  T=$NOW run "$d"; printf 'garbage\n' > "$d/sc/active/$u"; T=$((NOW + 900)) run "$d"
  [[ $RC == 1 && "$(grep -c 'event=none' "$d/err")" == 1 && "$(grep -c 'firing=unit:'$u "$d/err")" == 1 ]] && check "G1: an unreadable is-active answer keeps a firing unit condition (unknown)" || fail "G1: unreadable answer rc=$RC $(cat "$d/err")"

  # G2: an unwritable home falls back, with a home_unwritable warn and the normal state machine
  if [[ "$(id -u)" != 0 ]]; then
    d="$(mkenv g2)"; s="$(mksink "$d")"; mkq "$d" 1 700; chmod 500 "$d/mh"
    local genv=(MEMOIRE_HEALTH_SINK="$s" DBUS_SESSION_BUS_ADDRESS=unix:path=/x XDG_RUNTIME_DIR="$d/xdg")
    runq "$d" "${genv[@]}"; local rc1=$RC
    [[ $rc1 == 1 && "$(grep -c 'event=change' "$d/err")" == 1 && "$(jq -r '.firing | index("home_unwritable") != null' "$d"/sink.in.*)" == true && -f "$d/xdg/memoire-health-state.json" \
       && "$(stat -c %a "$d/xdg/memoire-health-state.json")" == 600 && "$(grep -c 'state_write=failed' "$d/err")" == 0 ]] \
      && check "G2: unwritable home, warn: state goes to XDG_RUNTIME_DIR, one change with home_unwritable, no write failure" || fail "G2: first run rc=$rc1 $(cat "$d/err")"
    jq -e '(.conditions[] | select(.name == "home_unwritable") | [.firing, .severity, (.detail | test("Permission denied"))]) == [true, "warn", true]' "$d"/sink.in.* >/dev/null 2>&1 && check "G2: home_unwritable is a warn with the error as detail" || fail "G2: condition: $(jq -c '.conditions[]|select(.name=="home_unwritable")' "$d"/sink.in.*)"
    T=$((NOW + 900)) runq "$d" "${genv[@]}"
    [[ $RC == 1 && "$(grep -c 'event=none' "$d/err")" == 1 && "$(nnotify "$d")" == 1 ]] && check "G2: the lasting warn is not repeated on the next run" || fail "G2: second run $(cat "$d/err")"
    T=$((NOW + 21600 + 100)) runq "$d" "${genv[@]}"
    [[ "$(grep -c 'event=reminder' "$d/err")" == 1 && "$(nnotify "$d")" == 2 ]] && check "G2: with the fallback the reminder fires after remind_s" || fail "G2: reminder $(cat "$d/err")"
    chmod 755 "$d/mh"; T=$((NOW + 21600 + 1000)) runq "$d" "${genv[@]}"
    [[ "$(jq -c '[.event, (.cleared | index("home_unwritable") != null)]' "$(ls -t "$d"/sink.in.* | head -n 1)")" == '["change",true]' ]] && [[ -f "$d/mh/health-state.json" && "$(grep -c 'prev=warn' "$d/err")" == 1 ]] \
      && check "G2: when the home is writable again the fallback state is read, home_unwritable clears, the home takes over" || fail "G2: home back $(cat "$d/err")"
    # TMPDIR fallback when XDG_RUNTIME_DIR is unset, uid in the name
    d="$(mkenv g2b)"; mkq "$d" 1 700; chmod 500 "$d/mh"
    runq "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/x; local rc3=$RC
    T=$((NOW + 900)) runq "$d" DBUS_SESSION_BUS_ADDRESS=unix:path=/x; chmod 755 "$d/mh"
    [[ $rc3 == 1 && -f "$d/tmpd/memoire-health-state-$(id -u).json" && "$(nnotify "$d")" == 1 && "$(grep -c 'event=none' "$d/err")" == 1 ]] && check "G2: without XDG_RUNTIME_DIR the state goes to TMPDIR with the uid in the name" || fail "G2: tmp fallback rc=$rc3 $(ls "$d/tmpd") $(cat "$d/err")"

    # H1: a fallback dated in the future never wins over the home, and a fallback is removed once the home is written
    d="$(mkenv h1)"; s="$(mksink "$d")"; mkdir -p "$d/xdg"
    jq -nc --argjson t "$NOW" '{state:"ok", firing:"", updated_epoch:($t-900), last_alert_epoch:0}' > "$d/mh/health-state.json"
    jq -nc --argjson t "$NOW" '{state:"warn", firing:"backlog_depth", updated_epoch:($t+100000), last_alert_epoch:($t+100000)}' > "$d/xdg/memoire-health-state.json"
    run "$d" MEMOIRE_HEALTH_SINK="$s" DBUS_SESSION_BUS_ADDRESS=unix:path=/x XDG_RUNTIME_DIR="$d/xdg"
    [[ $RC == 0 && "$(grep -c 'prev=ok' "$d/err")" == 1 && "$(grep -c 'event=none' "$d/err")" == 1 && "$(nnotify "$d")" == 0 && ! -e "$d/xdg/memoire-health-state.json" ]] \
      && check "H1: a future-dated fallback is ignored (no spurious recovery) and removed once the home is written" || fail "H1: rc=$RC $(cat "$d/err") $(ls "$d/xdg")"
    # H3: a symlinked fallback is never read
    d="$(mkenv h3)"; s="$(mksink "$d")"; mkdir -p "$d/xdg"; chmod 500 "$d/mh"
    jq -nc --argjson t "$NOW" '{state:"warn", firing:"backlog_depth", updated_epoch:($t-100), last_alert_epoch:($t-100)}' > "$d/planted.json"
    ln -s "$d/planted.json" "$d/xdg/memoire-health-state.json"
    run "$d" MEMOIRE_HEALTH_SINK="$s" DBUS_SESSION_BUS_ADDRESS=unix:path=/x XDG_RUNTIME_DIR="$d/xdg"; chmod 755 "$d/mh"
    [[ "$(grep -c 'prev=none' "$d/err")" == 1 ]] && check "H3: a symlinked fallback state file is not read" || fail "H3: $(cat "$d/err")"
  fi

  # curl is bounded by timeout (the stub ignores -m)
  d="$(mkenv curlhang)"; : > "$d/curl.hang"; echo '{}' > "$d/curl"
  t0=$SECONDS; run "$d" MEMOIRE_BASE_URL=http://hub.invalid -- --status
  [[ $RC == 0 && $((SECONDS - t0)) -lt 9 && "$(jq -r '.skipped | index("clock_skew:unreachable") != null' "$d/out")" == true ]] && check "clock: a hung curl is cut by timeout, clock_skew:unreachable" || fail "clock: curl hang rc=$RC t=$((SECONDS - t0))"

  # list-timers failure carries a skipped marker
  d="$(mkenv lt)"; setunit "$d" memoire-capture-tick.timer enabled active
  sed -i 's|list-timers) .*|list-timers) echo "boom" >\&2; exit 1 ;;|' "$d/bin/systemctl"
  run "$d" -- --status
  [[ "$(jq -r '.skipped | index("units:list-timers-failed") != null' "$d/out")" == true && "$(fire "$d")" == "" ]] && check "units: a failed list-timers is named in skipped and raises nothing" || fail "units: list-timers $(jq -c .skipped "$d/out")"

  # G6: log and JSON count the same thing
  d="$(mkenv g6)"; mkq "$d" 3 5; mkq "$d" 2 5 inflight; run "$d" -- --status
  [[ "$(jq -c '.queue | [.pending, .inflight, .pending_total]' "$d/out")" == '[3,2,5]' && "$(grep -c 'pending_total=5' "$d/err")" == 1 && "$(grep -c ' pending=' "$d/err")" == 0 ]] && check "G6: queue.pending_total and the log's pending_total= are pending plus inflight" || fail "G6: $(jq -c .queue "$d/out") $(cat "$d/err")"
}

for t in conditions forwarder units clock state desktop sink knobs status own cost r2 r3 timer; do "t_$t"; done

if (( FAILURES )); then echo "capture-health-test: $FAILURES failure(s)" >&2; exit 1; fi
echo "capture-health-test: all ok"
