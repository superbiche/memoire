#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# memoire-bootstrap.sh — standalone-SQLite first-run bootstrap (R-10.5).
#
# Runs from a dist tarball root or the repo root; everything resolves
# relative to this script. Local-first: no network beyond 127.0.0.1, no
# state outside MEMOIRE_HOME (default ~/.memoire) except the optional
# systemd user unit. STANDALONE means standalone: ambient hub/engine
# configuration (MEMOIRE_DB, MEMOIRE_PG_URL, MEMOIRE_FORWARD_URL) is
# rejected loudly — a custom engine setup runs the API itself.
#
#   memoire-bootstrap.sh check          verify layout + required tools
#   memoire-bootstrap.sh init           one-shot init: start API, wait
#                                       healthy, prove ownership, stop —
#                                       home/token/DB exist afterwards
#   memoire-bootstrap.sh systemd        install + enable a user unit
#                                       running memoire-api, wait healthy
#   memoire-bootstrap.sh hooks-claude   print the Claude Code hooks JSON
#                                       for capture + injection (no file
#                                       is edited; paste it yourself)
#   memoire-bootstrap.sh                check + init + next steps

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SELF="$SELF_DIR/$(basename "${BASH_SOURCE[0]}")"
ROOT="$SELF_DIR"
# Dist layout has this script at the root; repo layout has it in scripts/.
if [[ ! -d "$ROOT/bin" && -d "$ROOT/../bin" ]]; then
  ROOT="$(cd "$ROOT/.." && pwd)"
fi

API_BIN="$ROOT/bin/memoire-api"
ADDR="${MEMOIRE_ADDR:-127.0.0.1:37778}"
BASE_URL="http://$ADDR"
HOME_DIR="${MEMOIRE_HOME:-$HOME/.memoire}"
# A relative MEMOIRE_HOME means one store for init (cwd-relative) and
# another for a systemd service (home-relative) — canonicalize against
# the invocation cwd once, so every path below means one directory.
case "$HOME_DIR" in /*) ;; *) HOME_DIR="$PWD/$HOME_DIR" ;; esac
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
UNIT_NAME="memoire-api.service"

die() { echo "memoire-bootstrap: $*" >&2; exit 1; }

reject_ambient_engine_config() {
  local bad=()
  [[ -n "${MEMOIRE_DB:-}" ]] && bad+=(MEMOIRE_DB)
  [[ -n "${MEMOIRE_PG_URL:-}" ]] && bad+=(MEMOIRE_PG_URL)
  [[ -n "${MEMOIRE_FORWARD_URL:-}" ]] && bad+=(MEMOIRE_FORWARD_URL)
  [[ ${#bad[@]} -eq 0 ]] || die "ambient ${bad[*]} set — this bootstrap is standalone-SQLite only and will not touch an external store. Unset it, or run bin/memoire-api yourself."
}

# The systemd unit preserves EXACTLY MEMOIRE_HOME and MEMOIRE_ADDR. Every
# other API-affecting knob is rejected rather than silently dropped
# (round-2 F4): an init honoring it and a unit ignoring it would target
# different behavior.
reject_unpreservable_config() {
  local bad=()
  for v in MEMOIRE_SCHEMA_DIR MEMOIRE_MODEL_ENDPOINTS MEMOIRE_DERIVE_DENYLIST \
    MEMOIRE_REDACT MEMOIRE_FAILED_TTL_DAYS MEMOIRE_SKEW_ALERT_SECONDS \
    MEMOIRE_FORWARD_TOKEN_FILE; do
    [[ -n "${!v:-}" ]] && bad+=("$v")
  done
  [[ ${#bad[@]} -eq 0 ]] || die "ambient ${bad[*]} set — the installed unit preserves only MEMOIRE_HOME and MEMOIRE_ADDR; unset the rest or write the unit yourself."
}

# systemd unit serialization safety: rather than escaping the full systemd
# grammar, refuse values outside a safe charset (paths with spaces, quotes,
# %, backslashes).
unit_safe() { # $1=label $2=value
  [[ "$2" =~ ^[A-Za-z0-9/._:-]+$ ]] \
    || die "$1 contains characters the generated systemd unit cannot carry safely ($2) — write the unit yourself."
}

check() {
  local missing=0
  for b in memoire-api memoire-queue memoire-mint-token memoire-mcp; do
    [[ -x "$ROOT/bin/$b" ]] || { echo "MISSING binary: bin/$b" >&2; missing=1; }
  done
  [[ -d "$ROOT/schema" ]] || { echo "MISSING dir: schema/" >&2; missing=1; }
  [[ -f "$ROOT/LICENSE" ]] || { echo "MISSING file: LICENSE" >&2; missing=1; }
  # Required by init and the shipped capture/inject scripts. Hooks are
  # fail-open, so a missing tool would otherwise mean SILENT non-capture.
  for t in curl jq sha256sum timeout find; do
    command -v "$t" >/dev/null || { echo "MISSING tool: $t (required by the capture/inject scripts)" >&2; missing=1; }
  done
  [[ "$missing" -eq 0 ]] || die "layout or tooling incomplete under $ROOT"
  # Adapter-specific, not core: opencode capture reads state via sqlite3.
  command -v sqlite3 >/dev/null \
    || echo "note: sqlite3 not found — opencode capture needs it; other harnesses are unaffected" >&2
  echo "check: OK ($ROOT)"
}

healthy() { curl -fsS --max-time 2 "$BASE_URL/v1/health" >/dev/null 2>&1; }

# Ownership proof: the API on $ADDR must accept THIS home's bearer token.
# A healthy response alone can come from a foreign service or an API
# serving a different home (round-1 finding 3).
owned() {
  [[ -f "$HOME_DIR/token" ]] || return 1
  curl -fsS --max-time 2 -H "Authorization: Bearer $(cat "$HOME_DIR/token")" \
    "$BASE_URL/v1/stats" >/dev/null 2>&1
}

wait_pid_healthy() { # $1=pid $2=tries
  local pid="$1" tries="${2:-50}"
  for ((i = 0; i < tries; i++)); do
    kill -0 "$pid" 2>/dev/null || return 1
    if healthy; then return 0; fi
    sleep 0.2
  done
  return 1
}

# init child bookkeeping: a GLOBAL, because EXIT traps outlive function
# locals. stop_init_child TERM-waits bounded, KILLs as fallback, reaps.
INIT_PID=""
stop_init_child() {
  [[ -n "$INIT_PID" ]] || return 0
  kill "$INIT_PID" 2>/dev/null || true
  for ((i = 0; i < 25; i++)); do
    kill -0 "$INIT_PID" 2>/dev/null || break
    sleep 0.2
  done
  kill -9 "$INIT_PID" 2>/dev/null || true
  wait "$INIT_PID" 2>/dev/null || true
  INIT_PID=""
}

init() {
  reject_ambient_engine_config
  if healthy; then
    if owned; then
      echo "init: an API owned by $HOME_DIR is already healthy on $ADDR; nothing to do"
      return 0
    fi
    die "something already answers on $ADDR but does not accept $HOME_DIR/token — stop it or set MEMOIRE_ADDR"
  fi
  # INT/TERM must EXIT (a returned handler would resume execution and
  # could report success despite cancellation — round-3 finding 3); EXIT
  # owns the bounded child cleanup.
  trap 'exit 130' INT
  trap 'exit 143' TERM
  trap stop_init_child EXIT
  "$API_BIN" &
  INIT_PID=$!
  wait_pid_healthy "$INIT_PID" || die "API did not become healthy on $ADDR (or exited early)"
  owned || die "API is healthy on $ADDR but rejects $HOME_DIR/token — refusing to claim init succeeded"
  stop_init_child
  trap - EXIT INT TERM
  echo "init: OK — home=$HOME_DIR db=$HOME_DIR/memoire.db token=$HOME_DIR/token"
}

# systemd rollback guard: armed BEFORE the first unit mutation, disarmed
# only after the ownership loop succeeds (round-3 finding 2). Any exit in
# between — set -e failure of daemon-reload/enable, die, INT/TERM —
# lands here. A pre-existing unit is REFUSED up front (round-4 finding 1:
# a backup/restore transaction would also have to snapshot and reapply
# enabled/active state; a first-run bootstrap has no business replacing
# an operator's unit), so rollback only ever removes what this run wrote.
SYSD_ARMED=0
SYSD_STAGED=""
SYSD_INODE=""
sysd_rollback() {
  # The staged temp file is ours exactly when SYSD_STAGED is set (mktemp
  # created it exclusively); the published unit path is ours only when
  # SYSD_ARMED is set AND the inode still matches what this run
  # published (round-7: a boolean alone cannot prove the current entry
  # is still ours).
  if [[ -n "$SYSD_STAGED" ]]; then
    rm -f "$SYSD_STAGED"
    SYSD_STAGED=""
  fi
  [[ "$SYSD_ARMED" -eq 1 ]] || return 0
  systemctl --user disable --now "$UNIT_NAME" >/dev/null 2>&1 || true
  if [[ -n "$SYSD_INODE" \
    && "$(stat -c %i "$UNIT_DIR/$UNIT_NAME" 2>/dev/null)" == "$SYSD_INODE" ]]; then
    rm -f "$UNIT_DIR/$UNIT_NAME"
  fi
  systemctl --user daemon-reload 2>/dev/null || true
  echo "memoire-bootstrap: systemd install rolled back" >&2
}

systemd_install() {
  reject_ambient_engine_config
  reject_unpreservable_config
  command -v systemctl >/dev/null || die "systemctl not found"
  # The unit pins the RESOLVED home/addr unconditionally, so both must
  # always pass the unit grammar — not only when the caller set them.
  unit_safe "install path" "$API_BIN"
  unit_safe "MEMOIRE_HOME" "$HOME_DIR"
  unit_safe "MEMOIRE_ADDR" "$ADDR"
  # Anything already answering on the address would win the bind race and
  # make the unit's health unprovable (round-2 new finding 1): refuse.
  ! healthy || die "something already answers on $ADDR — stop it before installing the unit"
  # -L catches what -e misses: a dangling symlink is operator state too,
  # and writing through it would mutate a target OUTSIDE the unit dir
  # that rollback cannot see (round-5 finding 1).
  [[ ! -e "$UNIT_DIR/$UNIT_NAME" && ! -L "$UNIT_DIR/$UNIT_NAME" ]] \
    || die "$UNIT_DIR/$UNIT_NAME already exists — this bootstrap only installs fresh units; remove or manage the existing one yourself"
  mkdir -p "$UNIT_DIR"
  trap 'exit 130' INT
  trap 'exit 143' TERM
  trap sysd_rollback EXIT
  # The user manager's own environment reaches user services regardless
  # of this shell (round-3 finding 1): pin the resolved standalone
  # home/addr unconditionally and strip every other API-affecting
  # variable at the unit level. UnsetEnvironment is applied last by
  # systemd, so the two pinned names must not appear in it.
  # Stage-then-publish (rounds 6-7): mktemp creates the staged file
  # exclusively with an unpredictable name (a $$-derived path could be
  # pre-created or symlinked); ln -T refuses EVERY existing destination
  # entry — -T is what stops GNU ln from descending into a directory or
  # dir-symlink that raced the check. Arming happens immediately after
  # publication, and rollback verifies the published inode before
  # removing the final path.
  SYSD_STAGED="$(mktemp "$UNIT_DIR/.$UNIT_NAME.bootstrap.XXXXXX")"
  cat > "$SYSD_STAGED" <<UNIT
[Unit]
Description=Memoire local API (standalone SQLite)

[Service]
ExecStart=$API_BIN
Restart=on-failure
Environment="MEMOIRE_HOME=$HOME_DIR"
Environment="MEMOIRE_ADDR=$ADDR"
UnsetEnvironment=MEMOIRE_DB MEMOIRE_PG_URL MEMOIRE_FORWARD_URL MEMOIRE_FORWARD_TOKEN_FILE MEMOIRE_SCHEMA_DIR MEMOIRE_MODEL_ENDPOINTS MEMOIRE_DERIVE_DENYLIST MEMOIRE_REDACT MEMOIRE_FAILED_TTL_DAYS MEMOIRE_SKEW_ALERT_SECONDS

[Install]
WantedBy=default.target
UNIT
  ln -T "$SYSD_STAGED" "$UNIT_DIR/$UNIT_NAME" 2>/dev/null \
    || die "$UNIT_DIR/$UNIT_NAME appeared during install — refusing to touch it"
  SYSD_INODE="$(stat -c %i "$SYSD_STAGED")"
  SYSD_ARMED=1
  rm -f "$SYSD_STAGED"
  SYSD_STAGED=""
  systemctl --user daemon-reload
  systemctl --user enable --now "$UNIT_NAME"
  # Ownership proof is three-legged: the UNIT is active with a live
  # MainPID, AND the API on the address accepts this home's token.
  local verified=0 main_pid=""
  for ((i = 0; i < 100; i++)); do
    if systemctl --user is-active --quiet "$UNIT_NAME"; then
      main_pid="$(systemctl --user show -p MainPID --value "$UNIT_NAME" 2>/dev/null || true)"
      if [[ "$main_pid" =~ ^[0-9]+$ && "$main_pid" -gt 0 ]] \
        && kill -0 "$main_pid" 2>/dev/null && healthy && owned; then
        verified=1
        break
      fi
    fi
    sleep 0.2
  done
  [[ "$verified" -eq 1 ]] \
    || die "unit did not come up as a healthy, token-owning service on $ADDR"
  SYSD_ARMED=0
  trap - EXIT INT TERM
  echo "systemd: OK — $UNIT_DIR/$UNIT_NAME enabled, active (MainPID $main_pid), healthy"
}

hooks_claude() {
  cat <<JSON
Add to ~/.claude/settings.json (merge into any existing "hooks" object):

{
  "hooks": {
    "SessionStart": [
      { "hooks": [ { "type": "command", "command": "$ROOT/scripts/memoire-inject-claude.sh", "timeout": 5 } ] }
    ],
    "UserPromptSubmit": [
      { "hooks": [ { "type": "command", "command": "$ROOT/scripts/memoire-capture-claude.sh", "timeout": 5 } ] }
    ],
    "PostToolUse": [
      { "hooks": [ { "type": "command", "command": "$ROOT/scripts/memoire-capture-claude.sh", "timeout": 5 } ] }
    ]
  }
}

Derivation (observations from raw events) is opt-in and needs a model
endpoint; Codex / Qwen / opencode wiring: see QUICKSTART.md.
JSON
}

case "${1:-all}" in
  check) check ;;
  init) check; init ;;
  systemd) check; systemd_install ;;
  hooks-claude) hooks_claude ;;
  all)
    check
    init
    cat <<NEXT

Next steps:
  1. Run the API:            $API_BIN
     (or as a service:       $SELF systemd)
  2. Wire your harness:      $SELF hooks-claude
  3. First search:           see QUICKSTART.md
NEXT
    ;;
  *) die "unknown command: $1 (valid: check, init, systemd, hooks-claude)" ;;
esac
