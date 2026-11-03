#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# coverage-test.sh — scripts/memoire-coverage.py publishes an OBSERVED coverage
# file for liaison's harness.parity row (HARNESS-PARITY.md § 3): every `wired`
# comes from a live prerequisite, the file carries producer identity and the
# inventory hash, and a missing prerequisite is a named gap. Offline: a fake
# home, a stub systemctl, fixture registries.
#
#   wired      timer active + flag on + scripts executable -> wired
#   flag-off   the env flag off -> gap naming the flag
#   timer-dead systemctl says inactive -> gap naming the unit
#   hook       cc hook missing on one event -> gap naming the event
#   extension  pi extension differing from its source -> gap
#   none       a declared `none` lane with a reason -> n/a
#   acpx       acpx routes follow the lane runner state; boundaries over an
#              unwired native lane is a gap
#   file       schema, producer, inventory sha256, surface key; --strict exits 1

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COVERAGE="$ROOT/scripts/memoire-coverage.py"
command -v python3 >/dev/null || { echo "SKIP: python3 not available"; exit 0; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $1" >&2; exit 1; }
ok() { echo "ok: $1"; }

mkdir -p "$WORK/home/.claude" "$WORK/home/.pi/agent/extensions" "$WORK/mh" "$WORK/cfg/memoire" "$WORK/bin" "$WORK/out"
: > "$WORK/cfg/memoire/acpx-exclude"
cat > "$WORK/bin/systemctl" <<'STUB'
#!/usr/bin/env bash
# stub: `--user is-active <unit>` (units in $SYSTEMCTL_ACTIVE are active) and
# `--user show -p ExecStart --value <service>` (answers $SYSTEMCTL_EXEC, or a
# per-service override $SYSTEMCTL_EXEC_<name with - and . as _>)
if [[ "${2:-}" == "show" ]]; then
  svc="${6:-}"; key="SYSTEMCTL_EXEC_${svc//[-.]/_}"
  exec_path="${!key:-${SYSTEMCTL_EXEC:-}}"
  echo "{ path=$exec_path ; argv[]=$exec_path ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }"
  exit 0
fi
unit="${3:-}"
case " ${SYSTEMCTL_ACTIVE:-} " in *" $unit "*) echo active; exit 0 ;; esac
echo inactive; exit 3
STUB
chmod +x "$WORK/bin/systemctl"

cat > "$WORK/harnesses.json" <<'JSON'
{"schema": "liaison-harness-registry/1", "harnesses": {
  "cc": {"status": "active", "added": "2026-05-16", "aliases": ["claude"], "acpx_agents": ["claude"], "launch_surfaces": ["cc"], "native_cli": "claude", "config_home": "~/.claude"},
  "codex": {"status": "active", "added": "2026-05-16", "aliases": [], "acpx_agents": ["codex"], "launch_surfaces": ["codex"], "native_cli": "codex", "config_home": "~/.codex"},
  "pi": {"status": "active", "added": "2026-10-06", "aliases": [], "acpx_agents": ["pi"], "launch_surfaces": ["pi"], "native_cli": "pi", "config_home": "~/.pi/agent"},
  "muse": {"status": "trial", "added": "2026-10-13", "aliases": [], "acpx_agents": ["muse"], "launch_surfaces": ["muse"], "native_cli": "muse", "config_home": "~/.config/muse", "exercised": {"state": "no", "date": null, "session": null, "note": null}},
  "antigravity": {"status": "trial", "added": "2026-10-13", "aliases": [], "acpx_agents": ["antigravity"], "launch_surfaces": ["antigravity"], "native_cli": null, "config_home": null, "exercised": {"state": "no", "date": null, "session": null, "note": null}},
  "gemini": {"status": "retired", "added": "2026-05-16", "retired": "2026-10-15", "aliases": [], "acpx_agents": [], "launch_surfaces": [], "native_cli": "gemini", "config_home": "~/.gemini"}}}
JSON
cat > "$WORK/acpx.json" <<'JSON'
{"schema": "liaison-acpx-registry/1", "agents": {"claude": {"command": "x"}, "codex": {"command": "y"}, "pi": {"command": "z"}, "muse": {"command": "m"}, "antigravity": {"command": "a"}}}
JSON
cat > "$WORK/lanes.json" <<JSON
{"schema": "memoire-capture-lanes/1", "harnesses": {
  "cc": {"native": {"lane": "hooks", "surface": "$WORK/home/.claude/settings.json", "scripts": ["scripts/memoire-capture-claude.sh"], "events": ["UserPromptSubmit", "PostToolUse"], "timer": "memoire-capture-tick.timer"}, "acpx": "boundaries"},
  "codex": {"native": {"lane": "rollout-sweep", "enable_env": "MEMOIRE_CAPTURE_CODEX", "timer": "memoire-capture-codex-tick.timer", "scripts": ["scripts/memoire-capture-codex-rollout.sh"]}, "acpx": "boundaries"},
  "pi": {"native": {"lane": "extension", "surface": "$WORK/home/.pi/agent/extensions/memoire.js", "source": "scripts/pi-extension/memoire.js"}, "acpx": "boundaries"},
  "muse": {"native": {"lane": "none", "applicable": true, "reason": "no Muse session-store reader exists yet"}, "acpx": "journal"},
  "antigravity": {"native": {"lane": "none", "applicable": false, "reason": "acpx only in the fleet, no native session"}, "acpx": "journal"}},
  "acpx_lane": {"script": "scripts/memoire-capture-acpx.sh", "runner": "$WORK/bin/memoire-capture-acpx-tick.sh", "queue_bin": "bin/memoire-queue", "exclusion_file": "~/.config/memoire/acpx-exclude", "cursor": "~/.memoire/cursors/acpx-capture.json", "timer": "memoire-capture-acpx-tick.timer", "enable_env": "MEMOIRE_CAPTURE_ACPX"}}
JSON
# cc hooks installed, pointing at this checkout's script
CAP="$ROOT/scripts/memoire-capture-claude.sh"
cat > "$WORK/home/.claude/settings.json" <<JSON
{"hooks": {"UserPromptSubmit": [{"hooks": [{"type": "command", "command": "$CAP", "timeout": 5}]}],
           "PostToolUse": [{"hooks": [{"type": "command", "command": "$CAP", "timeout": 5}]}]}}
JSON
cp "$ROOT/scripts/pi-extension/memoire.js" "$WORK/home/.pi/agent/extensions/memoire.js"
printf 'MEMOIRE_CAPTURE_CODEX=on\nMEMOIRE_CAPTURE_ACPX=on\n' > "$WORK/mh/capture-tick.env"
ALL_TIMERS="memoire-capture-tick.timer memoire-capture-codex-tick.timer memoire-capture-acpx-tick.timer"
# the timers' live ExecStart scripts (what systemd runs) and a stub queue writer
printf '#!/usr/bin/env bash\nexit 0\n' > "$WORK/bin/memoire-queue"; chmod +x "$WORK/bin/memoire-queue"
# the acpx tick runner lands with the scheduled-tick slice (feat/acpx-journal-tick): a stub stands in for it here
printf '#!/usr/bin/env bash\nexit 0\n' > "$WORK/bin/memoire-capture-acpx-tick.sh"; chmod +x "$WORK/bin/memoire-capture-acpx-tick.sh"
EXEC_TICK="$ROOT/scripts/memoire-capture-tick.sh"; EXEC_CODEX="$ROOT/scripts/memoire-capture-codex-tick.sh"; EXEC_ACPX="$WORK/bin/memoire-capture-acpx-tick.sh"

run() { # VAR=value assignments and --flags, in any order
  local assigns=() flags=() a
  for a in "$@"; do case "$a" in --*) flags+=("$a") ;; *) assigns+=("$a") ;; esac; done
  env HOME="$WORK/home" MEMOIRE_HOME="$WORK/mh" XDG_CONFIG_HOME="$WORK/cfg" HOSTNAME=testhost \
    MEMOIRE_HARNESS_INVENTORY="$WORK/harnesses.json" MEMOIRE_CAPTURE_LANES="$WORK/lanes.json" \
    MEMOIRE_ACPX_REGISTRY="$WORK/acpx.json" MEMOIRE_COVERAGE_OUT="$WORK/out/memoire.json" \
    MEMOIRE_SYSTEMCTL="$WORK/bin/systemctl" SYSTEMCTL_ACTIVE="$ALL_TIMERS" MEMOIRE_QUEUE_BIN="$WORK/bin/memoire-queue" \
    SYSTEMCTL_EXEC_memoire_capture_tick_service="$EXEC_TICK" SYSTEMCTL_EXEC_memoire_capture_codex_tick_service="$EXEC_CODEX" \
    SYSTEMCTL_EXEC_memoire_capture_acpx_tick_service="$EXEC_ACPX" "${assigns[@]}" \
    python3 "$COVERAGE" --print "${flags[@]}" >"$WORK/stdout" 2>"$WORK/stderr"
  RC=$?
}
cell() { # harness route field
  python3 -c "import json,sys; d=json.load(open('$WORK/out/memoire.json'))['surfaces']['memoire.capture']; print(d['$1']['$2']['$3'])"
}

# wired
run
[[ $RC == 0 ]] || fail "wired: rc=$RC $(cat "$WORK/stderr")"
[[ "$(cell cc native disposition)" == wired ]] || fail "wired: cc native $(cell cc native detail)"
[[ "$(cell codex native disposition)" == wired ]] || fail "wired: codex native $(cell codex native detail)"
[[ "$(cell pi native disposition)" == wired ]] || fail "wired: pi native $(cell pi native detail)"
[[ "$(cell codex acpx:codex disposition)" == wired ]] || fail "wired: codex acpx $(cell codex acpx:codex detail)"
[[ "$(cell muse native disposition)" == gap ]] && [[ "$(cell muse native detail)" == *"applicable native route"* ]] || fail "none: muse native must be a GAP (applicable, unimplemented)"
[[ "$(cell antigravity native disposition)" == n/a ]] || fail "none: antigravity native should be n/a (not applicable)"
[[ "$(cell antigravity acpx:antigravity disposition)" == wired ]] || fail "acpx: antigravity journal route $(cell antigravity acpx:antigravity detail)"
[[ "$(cell muse acpx:muse disposition)" == wired ]] || fail "acpx: muse journal route $(cell muse acpx:muse detail)"
python3 - "$WORK/out/memoire.json" "$WORK/harnesses.json" <<'PY' || fail "file: shape"
import hashlib, json, sys
d = json.load(open(sys.argv[1]))
assert d["schema"] == "liaison-coverage/1", d["schema"]
assert d["producer"]["project"] == "memoire" and d["producer"]["host"] == "testhost" and d["producer"]["commit"]
assert d["inventory_sha256"] == hashlib.sha256(open(sys.argv[2], "rb").read()).hexdigest()
assert "gemini" not in d["surfaces"]["memoire.capture"], "retired harness must be absent"
assert d["generated_at"].endswith("+00:00")
PY
ok "wired + file shape"

# flag-off: the env file wins over the process environment (the file says on)
run MEMOIRE_CAPTURE_CODEX=off
[[ "$(cell codex native disposition)" == wired ]] || fail "flag-off: the env file must win over the process env ($(cell codex native detail))"
printf 'MEMOIRE_CAPTURE_CODEX=off\nMEMOIRE_CAPTURE_ACPX=on\n' > "$WORK/mh/capture-tick.env"
run
[[ "$(cell codex native disposition)" == gap ]] || fail "flag-off (file): codex should be a gap"
[[ "$(cell codex acpx:codex disposition)" == gap ]] && [[ "$(cell codex acpx:codex detail)" == *"native lane is not wired"* ]] || fail "acpx over unwired native: $(cell codex acpx:codex detail)"
printf 'MEMOIRE_CAPTURE_CODEX=on\nMEMOIRE_CAPTURE_ACPX=on\n' > "$WORK/mh/capture-tick.env"
ok "flag-off + boundaries over an unwired native lane"

# timer-dead
run SYSTEMCTL_ACTIVE="memoire-capture-tick.timer memoire-capture-acpx-tick.timer"
[[ "$(cell codex native detail)" == *"memoire-capture-codex-tick.timer inactive"* ]] || fail "timer-dead: $(cell codex native detail)"
ok "timer-dead"

# hook missing on one event
python3 - "$WORK/home/.claude/settings.json" <<'PY'
import json, sys
p = sys.argv[1]; d = json.load(open(p)); d["hooks"].pop("PostToolUse"); json.dump(d, open(p, "w"))
PY
run
[[ "$(cell cc native disposition)" == gap ]] && [[ "$(cell cc native detail)" == *"missing on PostToolUse"* ]] || fail "hook: $(cell cc native detail)"
[[ "$(cell cc acpx:claude disposition)" == gap ]] || fail "hook: cc acpx routes must follow the native lane"
ok "hook missing on one event"

# extension drift
printf '// drifted\n' >> "$WORK/home/.pi/agent/extensions/memoire.js"
run
[[ "$(cell pi native detail)" == *"differs from source"* ]] || fail "extension: $(cell pi native detail)"
ok "extension drift"

# the timer is active but its ExecStart points at a script that is gone
run SYSTEMCTL_EXEC_memoire_capture_codex_tick_service="$WORK/gone.sh"
[[ "$(cell codex native detail)" == *"ExecStart not executable"* ]] || fail "execstart: $(cell codex native detail)"
ok "timer ExecStart checked"

# journal prerequisites: runner and queue writer
run MEMOIRE_QUEUE_BIN="$WORK/no-queue"
[[ "$(cell codex acpx:codex detail)" == *"queue writer missing"* ]] || fail "queue: $(cell codex acpx:codex detail)"
ok "journal prerequisites"

# F12: the queue path the SERVICE will use comes from the env file (file over
# process); a missing path configured there is a gap even when the process
# environment or the checkout default has a usable writer
printf 'MEMOIRE_CAPTURE_CODEX=on\nMEMOIRE_CAPTURE_ACPX=on\nMEMOIRE_QUEUE_BIN=%s\n' "$WORK/moved-away/memoire-queue" > "$WORK/mh/capture-tick.env"
run
[[ "$(cell codex acpx:codex disposition)" == gap ]] && [[ "$(cell codex acpx:codex detail)" == *"queue writer missing: $WORK/moved-away/memoire-queue"* ]] || fail "F12: $(cell codex acpx:codex detail)"
printf 'MEMOIRE_CAPTURE_CODEX=on\nMEMOIRE_CAPTURE_ACPX=on\n' > "$WORK/mh/capture-tick.env"
ok "queue path from the effective service environment"

# kill switch: MEMOIRE_CAPTURE=off makes every route a gap, never a silent green
run MEMOIRE_CAPTURE=off
[[ "$(cell codex native disposition)" == gap ]] && [[ "$(cell codex native detail)" == *kill* ]] || fail "kill switch: $(cell codex native detail)"
[[ "$(cell codex acpx:codex disposition)" == gap ]] || fail "kill switch: acpx routes"
ok "kill switch"

# strict exits 1 while gaps remain
run --strict
[[ $RC == 1 ]] || fail "strict: rc=$RC"
ok "strict"

echo "coverage-test: all ok"
