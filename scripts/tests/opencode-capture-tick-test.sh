#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# opencode-capture-tick-test.sh — tests of the OpenCode capture tick
# (scripts/memoire-capture-opencode-tick.sh) and its timer installer
# (scripts/capture-opencode-tick-timer.sh). test-ops lane (`make test`): both
# scripts stay home. Offline; the adapter is a stub in most cases, the real
# adapter (with a queue writer built from this checkout, over a scratch
# database) in two.
#
#   off        the tick is disabled unless MEMOIRE_CAPTURE_OPENCODE is on|1|true;
#              other values are silent, an unrecognised one logs one marker
#   real       with the real adapter and a scratch database: exactly that
#              database's events reach the queue (a decoy store under HOME is
#              never read), the time window and the bound reach the adapter,
#              one summary line
#   bounds     an adapter that ignores TERM, one that leaves a child holding
#              its output, one that exits non-zero, one that is slow: the tick
#              ends within its bound plus the kill delay, logs ONE line, exits 0
#   detail     the adapter's stop line and failure lines reach the log line,
#              sanitised and bounded; a real bounded run logs its stop line
#   locks      the tick's lock is its own: a held Claude or Codex tick lock does
#              not stop it, a held OpenCode lock does not stop the Claude tick
#   skips      kill switch and preconditions log one marker each; a machine
#              without a database logs no_database=1 and exits 0
#   source     a fallback to the archive copy shows in the tick's log: source=
#              on the line, one extra line with the reason
#   inherit    the adapter and its children do not inherit the tick's lock: a
#              child left running by the adapter does not block the next tick
#   sigkill    a tick killed with SIGKILL leaves nothing the next tick keeps
#   claude     neither the Claude tick nor the Codex tick knows OpenCode
#   timer      the unit generator: files, environment file, start timeout
#              (the tick's bound + kill delay + margin, printed), cadence at
#              minute 7 (Claude 0, Codex 5), verbs (systemctl is a stub;
#              nothing is installed)
#
# Everything runs in a scratch directory made by mktemp -d under
# ${TMPDIR:-/var/tmp}, removed at exit.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TICK="$ROOT/scripts/memoire-capture-opencode-tick.sh"
CLAUDE_TICK="$ROOT/scripts/memoire-capture-tick.sh"
TIMER_SH="$ROOT/scripts/capture-opencode-tick-timer.sh"
CLAUDE_TIMER_SH="$ROOT/scripts/capture-tick-timer.sh"
ADAPTER="$ROOT/scripts/memoire-capture-opencode.sh"
RESOLVER="$ROOT/scripts/memoire-resolve-project.sh"
FIXTURE_SQL="$ROOT/tests/foreign/fixtures/opencode-census.sql"
command -v flock >/dev/null || { echo "SKIP: flock not available"; exit 0; }
command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }

WORK="$(mktemp -d "${TMPDIR:-/var/tmp}/opencode-tick-test.XXXXXX")" || { echo "FAIL: mktemp -d"; exit 1; }
KILL_PIDS=()
cleanup() {
  local p
  for p in ${KILL_PIDS[@]+"${KILL_PIDS[@]}"}; do kill -9 "$p" 2>/dev/null; done
  rm -rf "$WORK"
}
trap cleanup EXIT
mkdir -p "$WORK/bin"

FAILURES=0
fail() { echo "FAIL: $1" >&2; FAILURES=$((FAILURES + 1)); }
check() { echo "ok: $1"; }

QUEUE_BIN="${OPENCODE_CAPTURE_QUEUE_BIN:-}"
if [[ -z "$QUEUE_BIN" ]] && command -v go >/dev/null; then
  QUEUE_BIN="$WORK/bin/memoire-queue"
  (cd "$ROOT" && go build -tags sqlite_fts5 -o "$QUEUE_BIN" ./services/api/cmd/queue) || QUEUE_BIN=""
fi

mkenv() { # name -> dir
  local d="$WORK/$1"
  mkdir -p "$d/home" "$d/mh" "$d/cur" "$d/tmp" "$d/tool"
  printf '%s' "$d"
}
# run_tick DIR [KEY=VALUE ...]: the OpenCode tick in DIR's scratch environment
run_tick() {
  local d="$1"; shift
  env -u MEMOIRE_CAPTURE -u MEMOIRE_CAPTURE_OPENCODE -u MEMOIRE_OPENCODE_DB -u OPENCODE_DB -u MEMOIRE_OPENCODE_DB_FALLBACK \
    PATH="$PATH" HOME="$d/home" MEMOIRE_HOME="$d/mh" TMPDIR="$d/tmp" \
    MEMOIRE_OPENCODE_DB="$d/db" MEMOIRE_OPENCODE_DB_FALLBACK="$d/nofb" \
    MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR="$d/tool" MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR_FALLBACK="$d/nofb2" \
    MEMOIRE_CAPTURE_CURSOR_DIR="$d/cur" MEMOIRE_QUEUE_BIN="$QUEUE_BIN" MEMOIRE_RESOLVE_PROJECT="$RESOLVER" \
    MEMOIRE_PROJECTS_REGISTRY="$d/none" MEMOIRE_REDACT=0 \
    "$@" bash "$TICK"
}

# mkdb DB NSESSIONS [ROWS_PER_SESSION]: a scratch OpenCode database (the schema of the census
# fixture) holding sessions changed 3 hours ago, with non-ASCII text
mkdb() {
  local db="$1" n="$2" rows="${3:-1}" t i k
  t=$(( $(date +%s) * 1000 - 3 * 3600 * 1000 ))
  sed "s|__TOOL_OUT__|$WORK/none|g" "$FIXTURE_SQL" | sqlite3 "$WORK/schema.db" 2>/dev/null
  rm -f "$db"
  sqlite3 "$WORK/schema.db" .schema | sqlite3 "$db"
  {
    for i in $(seq 1 "$n"); do
      echo "INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES ('tk$i', 'prj_t', 's', '/tmp/tick-scratch', 't', '1', $t, $t);"
      for k in $(seq 1 "$rows"); do
        echo "INSERT INTO message VALUES ('tkm$i-$k', 'tk$i', $((t + k)), $((t + k)), '{\"role\":\"user\",\"time\":{\"created\":$((t + k))}}');"
        echo "INSERT INTO part VALUES ('tkp$i-$k', 'tkm$i-$k', 'tk$i', $((t + k)), $((t + k)), '{\"type\":\"text\",\"text\":\"tick é日本 $i $k\"}');"
      done
    done
  } | sqlite3 "$db"
}
LOGF() { printf '%s/mh/log/capture-opencode-tick.log' "$1"; }
qids() { cat "$1"/mh/queue/pending/*.json 2>/dev/null | jq -r '.source_event_id' | sort -u; }
stub() { # name body...: an adapter stub
  local f="$WORK/bin/$1"; shift
  printf '#!/usr/bin/env bash\n%s\n' "$*" > "$f"
  chmod +x "$f"
  printf '%s' "$f"
}

# ---- off --------------------------------------------------------------------------
t_off() {
  local d s v; d="$(mkenv off)"
  s="$(stub rec-argv "printf '%s\\n' \"\$*\" >> \"$d/argv.log\"; printf '0\\t0\\t0\\t0\\t0\\t0\\n'")"
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE_BIN="$s"; local rc=$?
  [[ $rc == 0 && ! -e "$d/argv.log" && ! -e "$(LOGF "$d")" ]] \
    && check "off: the tick is disabled by default, logs nothing, never runs the adapter, exits 0" \
    || fail "off: default run: rc=$rc argv=$(ls "$d/argv.log" 2>&1) log=$(cat "$(LOGF "$d")" 2>&1)"
  for v in off 0 false ""; do
    run_tick "$d" MEMOIRE_CAPTURE_OPENCODE="$v" MEMOIRE_CAPTURE_OPENCODE_BIN="$s"
  done
  [[ ! -e "$d/argv.log" && ! -e "$(LOGF "$d")" ]] || fail "off: off|0|false|'' must stay silent and disabled"
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=yes MEMOIRE_CAPTURE_OPENCODE_BIN="$s"
  [[ ! -e "$d/argv.log" && "$(grep -c . "$(LOGF "$d")")" == 1 && "$(cat "$(LOGF "$d")")" == *" tick_opencode skipped=opencode_switch_unrecognised" ]] \
    || fail "off: an unrecognised value must log one skipped=opencode_switch_unrecognised: $(cat "$(LOGF "$d")" 2>&1)"
  for v in 1 on true; do
    rm -f "$d/argv.log"
    run_tick "$d" MEMOIRE_CAPTURE_OPENCODE="$v" MEMOIRE_CAPTURE_OPENCODE_BIN="$s"
    [[ -e "$d/argv.log" ]] || fail "off: MEMOIRE_CAPTURE_OPENCODE=$v must enable the tick"
  done
  (( FAILURES == 0 )) && check "off: 1|on|true enable; off|0|false|'' silent; anything else one marker"
}

# ---- real adapter -------------------------------------------------------------------
t_real() {
  [[ -n "$QUEUE_BIN" ]] || { echo "SKIP: real: no queue writer (go missing)"; return; }
  local d wrapper ref
  d="$(mkenv real)"
  mkdb "$d/db" 2
  touch "$d/home/x"; rm -f "$d/home/x"
  # A decoy under HOME: a tick that fell back to the default source would queue its ids.
  mkdir -p "$d/home/.local/share/opencode"
  mkdb "$d/home/.local/share/opencode/opencode.db" 1
  sqlite3 "$d/home/.local/share/opencode/opencode.db" "UPDATE session SET id = 'decoy1'; UPDATE message SET session_id = 'decoy1'; UPDATE part SET session_id = 'decoy1';"
  ref="$(env HOME="$d/home" MEMOIRE_HOME="$d/mh" MEMOIRE_OPENCODE_DB="$d/db" MEMOIRE_OPENCODE_DB_FALLBACK="$d/nofb" MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR="$d/tool" \
      MEMOIRE_PROJECTS_REGISTRY="$d/none" MEMOIRE_RESOLVE_PROJECT="$RESOLVER" MEMOIRE_QUEUE_BIN="$QUEUE_BIN" \
      bash "$ADAPTER" --scan --dry-run 2>/dev/null | jq -r '.source_event_id' | sort -u)"
  wrapper="$WORK/bin/real-wrap"
  cat > "$wrapper" <<WRAP
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$d/argv.log"
exec bash "$ADAPTER" "\$@"
WRAP
  chmod +x "$wrapper"
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_CAPTURE_OPENCODE_BIN="$wrapper"
  [[ -n "$ref" && "$(qids "$d")" == "$ref" ]] && ! qids "$d" | grep -q 'decoy1' \
    || fail "real: with MEMOIRE_OPENCODE_DB set the queued id set must be exactly the scratch database's (got $(qids "$d" | grep -c .) ids, want $(echo "$ref" | grep -c .))"
  grep -q -- "--scan" "$d/argv.log" && grep -q -- "--summary" "$d/argv.log" && grep -q -- "--quiet-for 120" "$d/argv.log" \
    && grep -Eq -- "--since @[0-9]+" "$d/argv.log" && grep -Eq -- "--max-seconds [0-9]+" "$d/argv.log" \
    || fail "real: the scan, the quiet gate, the window and the bound must reach the adapter: $(cat "$d/argv.log")"
  [[ "$(grep -c . "$(LOGF "$d")")" == 1 ]] && grep -Eq " tick_opencode sessions=2 events=[0-9]+ failed_sessions=0 rejected=0 blobs=0 stopped=0 left=0 source=primary rc=0\$" "$(LOGF "$d")" \
    || fail "real: one summary line expected, got: $(cat "$(LOGF "$d")")"
  [[ -z "$(ls -A "$d/tmp")" && -z "$(ls -A "$d/mh/tmp" 2>/dev/null | grep -v '^opencode-capture$')" ]] \
    || fail "real: the tick left files behind: $(ls -A "$d/tmp" "$d/mh/tmp" | tr '\n' ' ')"
  [[ -z "$(find "$d/mh/tmp/opencode-capture" -mindepth 1 -maxdepth 1 -name 'run.*' 2>/dev/null)" ]] || fail "real: the adapter left a work directory behind"
  rm -f "$d/argv.log"
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_TICK_SINCE_S=600 MEMOIRE_TICK_QUIET_S=7 MEMOIRE_CAPTURE_OPENCODE_BIN="$wrapper"
  local since now
  since="$(grep -o -- '--since @[0-9]*' "$d/argv.log" | head -1 | tr -dc '0-9')"; now="$(date +%s)"
  [[ -n "$since" ]] && (( now - since >= 599 && now - since <= 620 )) && grep -q -- '--quiet-for 7' "$d/argv.log" \
    || fail "real: MEMOIRE_TICK_SINCE_S=600 gave a window start $((now - ${since:-0})) s ago (argv: $(cat "$d/argv.log"))"
  (( FAILURES == 0 )) && check "real: exactly the scratch database's $(echo "$ref" | grep -c .) ids are queued (the decoy HOME store is never read); the scan, the quiet gate, the window and the bound reach the adapter; one summary line; no temp file"
}

# ---- bounds ------------------------------------------------------------------------
now_ms() { date +%s%3N; }
bounded_case() { # label stub timeout kill max_elapsed_s expect_rc [expect_timed_out]
  local label="$1" s="$2" T="$3" K="$4" maxe="$5" erc="$6" eto="${7:-0}" d t0 t1 el rc n
  d="$(mkenv "bnd-$label")"
  t0="$(now_ms)"
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_CAPTURE_OPENCODE_BIN="$s" MEMOIRE_OPENCODE_TICK_TIMEOUT="$T" MEMOIRE_OPENCODE_TICK_KILL_S="$K" 2>"$d/stderr"
  rc=$?
  t1="$(now_ms)"; el=$(( t1 - t0 ))
  n="$(grep -c . "$(LOGF "$d")" 2>/dev/null)"
  (( rc == 0 )) || fail "bounds/$label: the tick must exit 0, got $rc"
  (( el <= maxe * 1000 )) || fail "bounds/$label: the tick took $el ms, the limit is $((maxe * 1000)) ms (bound $T s + kill delay $K s)"
  [[ ! -s "$d/stderr" ]] || fail "bounds/$label: the tick wrote on stderr: $(head -c 200 "$d/stderr")"
  [[ "$n" == 1 ]] || fail "bounds/$label: want exactly one log line, got $n: $(cat "$(LOGF "$d")" 2>&1)"
  grep -Eq " tick_opencode sessions=[0-9]+ events=[0-9]+ failed_sessions=[0-9]+ rejected=[0-9]+ blobs=[0-9]+ stopped=[0-9]+ left=[0-9]+ source=[a-z]+ rc=$erc( |\$)" "$(LOGF "$d")" \
    || fail "bounds/$label: want rc=$erc in the line: $(cat "$(LOGF "$d")" 2>&1)"
  if [[ "$eto" == 1 ]]; then grep -q ' timed_out=1' "$(LOGF "$d")" || fail "bounds/$label: want timed_out=1"
  else ! grep -q ' timed_out=1' "$(LOGF "$d")" || fail "bounds/$label: unexpected timed_out=1"; fi
  [[ -z "$(ls -A "$d/mh/tmp" 2>/dev/null)" && -z "$(ls -A "$d/tmp")" ]] || fail "bounds/$label: the tick left files behind"
  printf '%s' "$el"
}
t_bounds() {
  local s el1 el2 el3 el4 el5 d pf
  # 1. ignores TERM: TERM at 2 s, KILL 2 s later
  s="$(stub ignore-term "trap '' TERM; while :; do sleep 0.2; done")"
  el1="$(bounded_case ignore-term "$s" 2 2 6 137 1)"
  # 2. leaves a child holding its output (stdout and stderr files stay open), then exits
  d="$WORK/holder.pids"
  s="$(stub holds-output "( sleep 31.5 ) & echo \$! >> \"$d\"; printf '1\\t2\\t0\\t0\\t0\\t0\\n'; exit 0")"
  el2="$(bounded_case holds-output "$s" 20 5 3 0 0)"
  for pf in $(cat "$d" 2>/dev/null); do KILL_PIDS+=("$pf"); done
  # 3. holds the output AND hangs: the whole group is stopped at the bound
  s="$(stub holds-and-hangs "( exec sleep 31.7 ) & echo \$! >> \"$d\"; trap '' TERM; while :; do sleep 0.2; done")"
  el3="$(bounded_case holds-and-hangs "$s" 2 2 6 137 1)"
  for pf in $(cat "$d" 2>/dev/null); do KILL_PIDS+=("$pf"); done
  # 4. exits non-zero at once
  s="$(stub exit3 "exit 3")"
  el4="$(bounded_case exit3 "$s" 20 5 3 3 0)"
  # 5. slow: over the bound (TERM ends it) and under it (normal summary)
  s="$(stub slow-over "sleep 10; printf '1\\t5\\t0\\t0\\t0\\t0\\n'")"
  el5="$(bounded_case slow-over "$s" 2 2 5 124 1)"
  s="$(stub slow-under "sleep 1; printf '1\\t5\\t0\\t0\\t0\\t0\\n'")"
  bounded_case slow-under "$s" 5 2 4 0 0 >/dev/null
  # the summary of the slow-under case reached the line
  d="$WORK/bnd-slow-under"
  grep -q " tick_opencode sessions=1 events=5 failed_sessions=0 rejected=0 blobs=0 stopped=0 left=0 source=primary rc=0\$" "$(LOGF "$d")" || fail "bounds: the summary of a run inside its bound was not logged: $(cat "$(LOGF "$d")")"
  (( FAILURES == 0 )) && check "bounds: TERM-ignoring adapter ended in ${el1} ms (bound 2 s + kill 2 s); output-holding child ${el2} ms; hang holding output ${el3} ms; exit 3 ${el4} ms; slow ${el5} ms; one line and exit 0 each"
}

# ---- detail ------------------------------------------------------------------------
t_detail() {
  local d s line; d="$(mkenv detail)"
  s="$(stub noisy "printf 'memoire-capture-opencode: stopped cleanly on time: cursors written, 300 session(s) left for the next run; source=primary-immutable sessions=2\\n' >&2
printf 'memoire-capture-opencode: ses_y: queue write failed after 3 of 9 item(s): boom \"quoted\" \\001ctl %s\\n' \"\$(head -c 400 /dev/zero | tr '\\\\0' 'z')\" >&2
printf 'memoire-capture-opencode: ses_z: session read failed\\n' >&2
printf 'memoire-capture-opencode: ses_w: mapping failed: boom\\n' >&2
printf 'memoire-capture-opencode: read failed (rc=1): boom\\n' >&2
printf '2\\t3\\t1\\t0\\t1\\t300\\n'; exit 1")"
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_CAPTURE_OPENCODE_BIN="$s"
  line="$(cat "$(LOGF "$d")")"
  [[ "$(grep -c . <<<"$line")" == 1 ]] || fail "detail: still one line per tick: $line"
  [[ "$line" == *" tick_opencode sessions=2 events=3 failed_sessions=1 rejected=0 blobs=0 stopped=1 left=300 source=primary rc=1 "* ]] || fail "detail: summary fields wrong: $line"
  [[ "$line" == *'stop="memoire-capture-opencode: stopped cleanly on time: cursors written, 300 session(s) left for the next run'* ]] || fail "detail: the stop line is missing: $line"
  [[ "$line" == *'fail="memoire-capture-opencode: ses_y: queue write failed after 3 of 9 item(s)'* ]] || fail "detail: the failure line is missing: $line"
  [[ "$line" == *"ses_z: session read failed"* ]] || fail "detail: the second failure line is missing: $line"
  [[ "$line" != *"ses_w"* && "$line" != *"rc=1): boom"* ]] || fail "detail: at most two failure lines: $line"
  [[ "$line" != *'boom "quoted"'* && "$line" != *$'\001'* ]] || fail "detail: quotes and control characters must be stripped: $line"
  (( ${#line} < 700 )) || fail "detail: the line is not bounded (${#line} characters)"
  # the real adapter's own wording is what the tick looks for
  if [[ -n "$QUEUE_BIN" ]]; then
    d="$(mkenv detail-real)"
    mkdb "$d/db" 1 60
    cat > "$WORK/bin/slowq" <<SLOWQ
#!/usr/bin/env bash
sleep 1.3
exec "$QUEUE_BIN" "\$@"
SLOWQ
    chmod +x "$WORK/bin/slowq"
    run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_QUEUE_BIN="$WORK/bin/slowq" MEMOIRE_OPENCODE_ROW_CHUNK=5 MEMOIRE_OPENCODE_TICK_TIMEOUT=8
    line="$(cat "$(LOGF "$d")")"
    [[ "$(grep -c . <<<"$line")" == 1 && "$line" == *" stopped=1 "* && "$line" == *" rc=0"* && "$line" =~ stop=\"[^\"]*stopped\ cleanly\ on\ time:\ cursors\ written,\ [0-9]+\ session\(s\)\ left ]] \
      || fail "detail: a real bounded run must log its stop line in its one line: $line"
  fi
  (( FAILURES == 0 )) && check "detail: the adapter's stop line and first two failure lines are logged in the one line, sanitised and cut ($(printf '%s' "$line" | wc -c) chars for a real bounded run)"
}

# ---- locks -------------------------------------------------------------------------
t_locks() {
  local d s cc holder; d="$(mkenv locks)"
  s="$(stub summary "printf '1\\t1\\t0\\t0\\t0\\t0\\n'")"
  # the Claude tick's lock held: the OpenCode tick runs
  mkdir -p "$d/mh"
  ( exec 8>"$d/mh/capture-tick.lock"; flock -n 8 && sleep 4 ) &
  holder=$!; sleep 0.4
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_CAPTURE_OPENCODE_BIN="$s"
  grep -q " tick_opencode sessions=1 events=1 " "$(LOGF "$d")" || fail "locks: a held Claude tick lock must not stop the OpenCode tick: $(cat "$(LOGF "$d")" 2>&1)"
  wait "$holder" 2>/dev/null
  # the Codex tick's lock held: the OpenCode tick runs
  rm -f "$(LOGF "$d")"
  ( exec 8>"$d/mh/capture-codex-tick.lock"; flock -n 8 && sleep 4 ) &
  holder=$!; sleep 0.4
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_CAPTURE_OPENCODE_BIN="$s"
  grep -q " tick_opencode sessions=1 events=1 " "$(LOGF "$d")" || fail "locks: a held Codex tick lock must not stop the OpenCode tick: $(cat "$(LOGF "$d")" 2>&1)"
  wait "$holder" 2>/dev/null
  # the OpenCode tick's lock held: the OpenCode tick skips, the Claude tick runs
  rm -f "$(LOGF "$d")"
  ( exec 8>"$d/mh/capture-opencode-tick.lock"; flock -n 8 && sleep 4 ) &
  holder=$!; sleep 0.4
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_CAPTURE_OPENCODE_BIN="$s"
  [[ "$(cat "$(LOGF "$d")")" == *" tick_opencode skipped=locked" ]] || fail "locks: a held OpenCode lock must give skipped=locked: $(cat "$(LOGF "$d")" 2>&1)"
  mkdir -p "$d/projects"
  cc="$(stub fake-cc "printf 'proj-a\\tsess-1\\t2\\n'; exit 1")"
  env -u MEMOIRE_CAPTURE PATH="$PATH" HOME="$d/home" MEMOIRE_HOME="$d/mh" TMPDIR="$d/tmp" MEMOIRE_CC_PROJECTS_DIR="$d/projects" \
    MEMOIRE_CAPTURE_CC_BIN="$cc" MEMOIRE_CAPTURE_OPENCODE=1 bash "$CLAUDE_TICK"
  [[ "$(cat "$d/mh/log/capture-tick.log")" == *" tick files_new=1 turns=2 sessions=1 notify=off rc=1" ]] \
    || fail "locks: the Claude tick must run while the OpenCode lock is held: $(cat "$d/mh/log/capture-tick.log" 2>&1)"
  wait "$holder" 2>/dev/null
  (( FAILURES == 0 )) && check "locks: each tick has its own lock: the OpenCode tick runs while the Claude or the Codex lock is held; the Claude tick runs while the OpenCode lock is held"
}

# ---- skips -------------------------------------------------------------------------
t_skips() {
  local d s; d="$(mkenv skips)"
  s="$(stub summary "printf '1\\t1\\t0\\t0\\t0\\t0\\n'")"
  run_tick "$d" MEMOIRE_CAPTURE=off MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_CAPTURE_OPENCODE_BIN="$s"
  [[ "$(cat "$(LOGF "$d")")" == *" tick_opencode skipped=capture_disabled" ]] || fail "skips: MEMOIRE_CAPTURE=off: $(cat "$(LOGF "$d")" 2>&1)"
  rm -f "$(LOGF "$d")"
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_CAPTURE_OPENCODE_BIN="$d/absent"
  [[ "$(cat "$(LOGF "$d")")" == *" tick_opencode skipped=capture_bin_missing" ]] || fail "skips: missing adapter: $(cat "$(LOGF "$d")" 2>&1)"
  rm -f "$(LOGF "$d")"
  # a machine without OpenCode: the real adapter has nothing to read (exit 0), the tick says so
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_OPENCODE_DB="$d/none1" MEMOIRE_OPENCODE_DB_FALLBACK="$d/none2"
  [[ "$(grep -c . "$(LOGF "$d")")" == 1 && "$(cat "$(LOGF "$d")")" == *" tick_opencode sessions=0 events=0 failed_sessions=0 rejected=0 blobs=0 stopped=0 left=0 source=primary rc=0 no_database=1" ]] \
    || fail "skips: no database: $(cat "$(LOGF "$d")" 2>&1)"
  (( FAILURES == 0 )) && check "skips: kill switch, missing adapter and no database each log one marker and exit 0"
}

# ---- source ------------------------------------------------------------------------
# A fallback to the archive copy is visible in the tick's log.
t_source() {
  [[ -n "$QUEUE_BIN" ]] || { echo "SKIP: source: no queue writer (go missing)"; return; }
  local h f hl fl s d2 n
  h="$(mkenv src-healthy)"; mkdb "$h/db" 1
  run_tick "$h" MEMOIRE_CAPTURE_OPENCODE=1
  hl="$(cat "$(LOGF "$h")")"
  f="$(mkenv src-fallback)"; mkdb "$f/archive.db" 1
  run_tick "$f" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_OPENCODE_DB="$f/absent.db" MEMOIRE_OPENCODE_DB_FALLBACK="$f/archive.db"
  fl="$(head -n 1 "$(LOGF "$f")")"
  [[ "$(grep -c . <<<"$hl")" == 1 && "$hl" == *" source=primary rc=0" ]] || fail "source: a healthy read logs one line with source=primary: $hl"
  [[ "$(grep -c . "$(LOGF "$f")")" == 2 && "$fl" == *" source=archive rc=0" ]] || fail "source: a fallback logs source=archive and one extra line: $(cat "$(LOGF "$f")")"
  [[ "$(sed 's/^[^ ]* //' <<<"$hl")" != "$(sed 's/^[^ ]* //' <<<"$fl")" ]] || fail "source: the tick line of a fallback must differ from a healthy read (timestamp aside): $fl"
  tail -n 1 "$(LOGF "$f")" | grep -Eq ' tick_opencode source=archive reason="[^"]+"$' || fail "source: the extra line must name the reason: $(tail -n 1 "$(LOGF "$f")")"
  (( $(qids "$f" | grep -c .) > 0 )) || fail "source: the fallback run captured nothing from the archive copy"
  # the summary TSV of the adapter: source (and blobs) are its 7th and 8th fields
  n="$(env HOME="$f/home" MEMOIRE_HOME="$f/mh" MEMOIRE_OPENCODE_DB="$f/absent.db" MEMOIRE_OPENCODE_DB_FALLBACK="$f/archive.db" MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR="$f/tool" \
      MEMOIRE_PROJECTS_REGISTRY="$f/none" MEMOIRE_RESOLVE_PROJECT="$RESOLVER" MEMOIRE_QUEUE_BIN="$QUEUE_BIN" MEMOIRE_CAPTURE_CURSOR_DIR="$f/cur2" \
      bash "$ADAPTER" --scan --summary --dry-run 2>/dev/null | tail -n 1 | awk -F'\t' '{print NF ":" $8}')"
  [[ "$n" == "8:archive" ]] || fail "source: the adapter's summary must end with the source (8 fields, archive): $n"
  # a stub adapter with the 8-field summary: the tick copies blobs and source
  s="$(stub tsv8 "printf '1\\t2\\t0\\t0\\t0\\t0\\t3\\tarchive\\n'; echo 'memoire-capture-opencode: primary database busy error (rc=5): x; falling back to the archive copy (immutable=1)' >&2")"
  d2="$(mkenv src-stub)"
  run_tick "$d2" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_CAPTURE_OPENCODE_BIN="$s"
  head -n 1 "$(LOGF "$d2")" | grep -q " tick_opencode sessions=1 events=2 failed_sessions=0 rejected=0 blobs=3 stopped=0 left=0 source=archive rc=0\$" \
    && tail -n 1 "$(LOGF "$d2")" | grep -q 'source=archive reason="memoire-capture-opencode: primary database busy error' \
    || fail "source: blobs and source must reach the line, the reason the extra line: $(cat "$(LOGF "$d2")")"
  (( FAILURES == 0 )) && check "source: a fallback logs source=archive and one extra line naming the reason; a healthy read logs source=primary; the summary carries source and blobs"
}

# ---- inherit -----------------------------------------------------------------------
t_inherit() {
  local d s pf p rc; d="$(mkenv inherit)"; pf="$d/child.pid"
  # the adapter leaves a background child that outlives it (and the tick)
  s="$(stub leaves-child "( exec sleep 30 ) </dev/null >/dev/null 2>&1 & echo \$! >> \"$pf\"; printf '1\\t1\\t0\\t0\\t0\\t0\\n'")"
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_CAPTURE_OPENCODE_BIN="$s"
  for p in $(cat "$pf" 2>/dev/null); do KILL_PIDS+=("$p"); done
  [[ -s "$pf" ]] && kill -0 "$(head -1 "$pf")" 2>/dev/null || fail "inherit: the scenario is void: the child is not running"
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_CAPTURE_OPENCODE_BIN="$s"; rc=$?
  [[ $rc == 0 && "$(grep -c ' tick_opencode sessions=1 events=1 ' "$(LOGF "$d")")" == 2 && "$(grep -c 'skipped=locked' "$(LOGF "$d")")" == 0 ]] \
    || fail "inherit: a child left running by the adapter must not hold the tick lock: $(cat "$(LOGF "$d")" 2>&1)"
  # the descriptor is closed for the adapter itself
  s="$(stub fd-probe "if [ -e /proc/self/fd/8 ]; then echo open > \"$d/fd8\"; else echo closed > \"$d/fd8\"; fi; printf '0\\t0\\t0\\t0\\t0\\t0\\n'")"
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_CAPTURE_OPENCODE_BIN="$s"
  [[ "$(cat "$d/fd8" 2>/dev/null)" == closed ]] || fail "inherit: descriptor 8 (the tick lock) must be closed in the adapter: $(cat "$d/fd8" 2>&1)"
  (( FAILURES == 0 )) && check "inherit: the adapter and a child it leaves running do not inherit the tick lock; the next tick takes it"
}

# ---- sigkill -----------------------------------------------------------------------
t_sigkill() {
  local d s pid i; d="$(mkenv sigkill)"
  s="$(stub sleeper "sleep 3; printf '1\\t1\\t0\\t0\\t0\\t0\\n'")"
  ( exec setsid env -u MEMOIRE_CAPTURE PATH="$PATH" HOME="$d/home" MEMOIRE_HOME="$d/mh" TMPDIR="$d/tmp" \
      MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_CAPTURE_OPENCODE_BIN="$s" bash "$TICK" ) >/dev/null 2>&1 &
  pid=$!
  for i in $(seq 1 100); do ls -d "$d"/mh/tmp/opencode-tick.* >/dev/null 2>&1 && break; sleep 0.05; done
  kill -KILL "$pid" 2>/dev/null # the tick only: its adapter stub is orphaned and ends on its own
  wait "$pid" 2>/dev/null
  ls -d "$d"/mh/tmp/opencode-tick.* >/dev/null 2>&1 || fail "sigkill: the scenario is void: no output directory was left by the killed tick"
  sleep 3.3 # the orphaned adapter stub ends
  s="$(stub summary "printf '0\\t0\\t0\\t0\\t0\\t0\\n'")"
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_CAPTURE_OPENCODE_BIN="$s"
  [[ -z "$(ls -A "$d/mh/tmp" 2>/dev/null)" && -z "$(ls -A "$d/tmp")" ]] \
    || fail "sigkill: the next tick must remove what a SIGKILLed tick left: $(ls -A "$d/mh/tmp")"
  # a directory that is not a tick's, and a plain file, stay
  mkdir -p "$d/mh/tmp/other-dir"; : > "$d/mh/tmp/opencode-capture-note"
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_CAPTURE_OPENCODE_BIN="$s"
  [[ -d "$d/mh/tmp/other-dir" && -e "$d/mh/tmp/opencode-capture-note" ]] || fail "sigkill: the sweep removed something that is not a tick directory"
  # a held lock: the sweep does not run (another tick may be alive)
  mkdir -p "$d/mh/tmp/opencode-tick.LIVE1"
  ( exec 8>"$d/mh/capture-opencode-tick.lock"; flock -n 8 && sleep 2 ) &
  local holder=$!; sleep 0.4
  run_tick "$d" MEMOIRE_CAPTURE_OPENCODE=1 MEMOIRE_CAPTURE_OPENCODE_BIN="$s"
  [[ -d "$d/mh/tmp/opencode-tick.LIVE1" ]] || fail "sigkill: a tick that does not hold the lock must not sweep"
  wait "$holder" 2>/dev/null
  (( FAILURES == 0 )) && check "sigkill: a tick killed with SIGKILL leaves its output directory; the next tick removes it (and only tick directories, only under the lock)"
}

# ---- claude ------------------------------------------------------------------------
t_claude() {
  if grep -qi opencode "$CLAUDE_TICK" "$ROOT/scripts/memoire-capture-codex-tick.sh"; then fail "claude: neither scripts/memoire-capture-tick.sh nor the Codex tick may mention the OpenCode capture"
  else check "claude: the Claude and Codex ticks have no OpenCode leg (their own tests pin their behaviour)"; fi
}

# ---- timer -------------------------------------------------------------------------
t_timer() {
  local d bin cfg rt calls seen
  d="$(mkenv timer)"; bin="$WORK/sbin"; mkdir -p "$bin" "$d/xdg" "$d/rt"
  cat > "$bin/systemctl" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$d/systemctl.log"
exit 0
STUB
  chmod +x "$bin/systemctl"
  # Guard: the stub must be what the installers find; a real systemctl must never run.
  [[ "$(PATH="$bin:$PATH" bash -c 'command -v systemctl')" == "$bin/systemctl" ]] || { fail "timer: the systemctl stub is not first in PATH"; return; }
  tenv() { env PATH="$bin:$PATH" HOME="$d/home" XDG_CONFIG_HOME="$d/xdg" XDG_RUNTIME_DIR="$d/rt" DBUS_SESSION_BUS_ADDRESS= "$@"; }
  cfg="$d/xdg/systemd/user"
  tenv bash "$TIMER_SH" install >"$d/install.out" 2>"$d/install.err"
  [[ $? == 0 && -f "$cfg/memoire-capture-opencode-tick.service" && -f "$cfg/memoire-capture-opencode-tick.timer" && "$(ls "$cfg" | wc -l)" == 2 ]] \
    || fail "timer: install must write exactly the opencode service and timer: $(ls "$cfg" 2>&1)"
  grep -q "^ExecStart=$ROOT/scripts/memoire-capture-opencode-tick.sh\$" "$cfg/memoire-capture-opencode-tick.service" \
    || fail "timer: ExecStart must be this checkout's opencode tick"
  # same environment file as the Claude timer's unit
  tenv XDG_CONFIG_HOME="$d/xdg-claude" bash "$CLAUDE_TIMER_SH" install >/dev/null 2>&1
  [[ "$(grep '^EnvironmentFile=' "$cfg/memoire-capture-opencode-tick.service")" == "$(grep '^EnvironmentFile=' "$d/xdg-claude/systemd/user/memoire-capture-tick.service")" \
     && -n "$(grep '^EnvironmentFile=-%h/.memoire/capture-tick.env$' "$cfg/memoire-capture-opencode-tick.service")" ]] \
    || fail "timer: the opencode unit must read the same optional environment file as the Claude unit"
  grep -q '^Type=oneshot$' "$cfg/memoire-capture-opencode-tick.service" && grep -q '^TimeoutStartSec=310s$' "$cfg/memoire-capture-opencode-tick.service" \
    || fail "timer: oneshot with TimeoutStartSec=310s (bound 240 s + kill delay 10 s + margin 60 s) expected"
  grep -q '^TimeoutStartSec=310s (tick bound 240s + kill delay 10s + margin 60s)$' "$d/install.out" \
    || fail "timer: the generator must print the start timeout it used: $(cat "$d/install.out")"
  # the value follows the tick's variables at generation time: environment, then the environment file
  tenv MEMOIRE_OPENCODE_TICK_TIMEOUT=600 MEMOIRE_OPENCODE_TICK_KILL_S=20 bash "$TIMER_SH" install >"$d/install2.out" 2>/dev/null
  grep -q '^TimeoutStartSec=680s$' "$cfg/memoire-capture-opencode-tick.service" && grep -q '^TimeoutStartSec=680s (tick bound 600s + kill delay 20s + margin 60s)$' "$d/install2.out" \
    || fail "timer: TimeoutStartSec must be 600 + 20 + 60 = 680s from the environment: $(grep TimeoutStartSec "$cfg/memoire-capture-opencode-tick.service") / $(cat "$d/install2.out")"
  mkdir -p "$d/home/.memoire"
  printf 'MEMOIRE_OPENCODE_TICK_TIMEOUT=100\nMEMOIRE_OPENCODE_TICK_KILL_S="5"\nMEMOIRE_CAPTURE_OPENCODE=on\n' > "$d/home/.memoire/capture-tick.env"
  tenv bash "$TIMER_SH" install >"$d/install3.out" 2>/dev/null
  grep -q '^TimeoutStartSec=165s$' "$cfg/memoire-capture-opencode-tick.service" \
    || fail "timer: TimeoutStartSec must be 100 + 5 + 60 = 165s from the environment file: $(grep TimeoutStartSec "$cfg/memoire-capture-opencode-tick.service")"
  tenv MEMOIRE_OPENCODE_TICK_TIMEOUT=abc MEMOIRE_OPENCODE_TICK_KILL_S=0 bash "$TIMER_SH" install >"$d/install4.out" 2>/dev/null
  grep -q '^TimeoutStartSec=310s$' "$cfg/memoire-capture-opencode-tick.service" \
    || fail "timer: non-numeric or zero values must fall back to the tick's defaults (310s): $(grep TimeoutStartSec "$cfg/memoire-capture-opencode-tick.service")"
  rm -f "$d/home/.memoire/capture-tick.env"
  tenv bash "$TIMER_SH" install >/dev/null 2>&1
  grep -q '^OnCalendar=\*:7/10$' "$cfg/memoire-capture-opencode-tick.timer" && grep -q '^Persistent=true$' "$cfg/memoire-capture-opencode-tick.timer" \
    && grep -q '^WantedBy=timers.target$' "$cfg/memoire-capture-opencode-tick.timer" \
    || fail "timer: cadence lines wrong: $(cat "$cfg/memoire-capture-opencode-tick.timer")"
  ! grep -q 'memoire-capture-tick\.' "$cfg/memoire-capture-opencode-tick.service" "$cfg/memoire-capture-opencode-tick.timer" \
    || fail "timer: the opencode units must not name the Claude units"
  # The canonical root the installer warns about is the repo's main worktree
  # (git-derived; no personal checkout layout in the warning itself).
  CANON="$(git -C "$ROOT" worktree list --porcelain 2>/dev/null | awk 'NR==1{print $2}' || true)"
  if [[ -n "$CANON" && "$CANON" != "$ROOT" ]]; then
    grep -q "not $CANON" "$d/install.err" || fail "timer: installing from a non-canonical root must warn"
  else
    ! grep -q "WARNING: persisting ExecStart" "$d/install.err" || fail "timer: the canonical checkout must not warn"
  fi
  seen="$(cat "$d/systemctl.log")"
  [[ "$seen" == *"--user daemon-reload"* && "$seen" == *"--user enable --now memoire-capture-opencode-tick.timer"* ]] \
    || fail "timer: install must reload and enable the opencode timer: $seen"
  : > "$d/systemctl.log"
  local v
  for v in start stop status run-once; do tenv bash "$TIMER_SH" "$v" >/dev/null 2>&1; done
  calls="$(cat "$d/systemctl.log")"
  [[ "$calls" == *"--user start memoire-capture-opencode-tick.timer"* && "$calls" == *"--user disable --now memoire-capture-opencode-tick.timer"* \
     && "$calls" == *"--user status memoire-capture-opencode-tick.timer"* && "$calls" == *"--user start memoire-capture-opencode-tick.service"* ]] \
    || fail "timer: verbs map to the wrong systemctl calls: $calls"
  ! grep -q 'memoire-capture-tick\.' "$d/systemctl.log" || fail "timer: the opencode installer touched the Claude units"
  tenv bash "$TIMER_SH" uninstall >/dev/null 2>&1
  [[ ! -e "$cfg/memoire-capture-opencode-tick.service" && ! -e "$cfg/memoire-capture-opencode-tick.timer" ]] || fail "timer: uninstall must remove both files"
  tenv bash "$TIMER_SH" nonsense >/dev/null 2>&1; [[ $? == 1 ]] || fail "timer: an unknown verb must exit 1"
  (( FAILURES == 0 )) && check "timer: install writes the opencode service and timer only (same environment file as the Claude unit, TimeoutStartSec = bound + kill delay + 60 s, printed, every 10 min at :07), verbs and uninstall map to the opencode units; systemctl was a stub"
}

for t in off real bounds detail locks skips source inherit sigkill claude timer; do
  if [[ -z "${OPENCODE_TICK_ONLY:-}" || " $OPENCODE_TICK_ONLY " == *" $t "* ]]; then "t_$t"; fi
done

if (( FAILURES > 0 )); then
  echo "opencode-capture-tick-test: $FAILURES failure(s)" >&2
  exit 1
fi
echo "opencode-capture-tick-test: OK"
