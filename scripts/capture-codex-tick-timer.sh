#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# capture-codex-tick-timer.sh — install/manage the memoire-capture-codex-tick
# systemd user timer: the Codex rollout capture, on a timer of its own next to
# the Claude capture tick (capture-tick-timer.sh), sharing nothing with it.
#
# Usage:
#   scripts/capture-codex-tick-timer.sh install     # write + enable the service/timer
#   scripts/capture-codex-tick-timer.sh start|stop|status|run-once|uninstall
#
# The tick body holds NO credential: it captures Codex rollout tails into the
# local file queue. It ships DISABLED inside the unit: MEMOIRE_CAPTURE_CODEX
# must be set to on|1|true in the OPTIONAL EnvironmentFile
# (~/.memoire/capture-tick.env, the same file the Claude tick reads), where the
# cadence knobs (MEMOIRE_TICK_QUIET_S, MEMOIRE_TICK_SINCE_S,
# MEMOIRE_CODEX_TICK_TIMEOUT, ...) live too, so the unit itself never has to be
# edited.
#
# Cadence: every 10 minutes, five minutes after the Claude tick, Persistent=true
# (a tick missed while the host slept runs once on resume — the cursors make a
# late tick cheap), with a randomized delay.

# The installed unit persists an absolute ExecStart path. Default to the
# script's own checkout but let MEMOIRE_REPO_ROOT override, and warn loudly
# when installing from anything that is not the canonical main checkout
# (e.g. a review worktree that will be deleted).
ROOT="${MEMOIRE_REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
# The canonical root is the main worktree of the repository this script
# lives in (no personal checkout layout in the default): git-derived when
# git is available; without it (a hermetic test) a linked worktree's .git
# file still names it — its `gitdir:` line points into the main checkout's
# .git/worktrees/, three levels above the main root. A bare archive has no
# .git at all and is its own canonical root.
CANONICAL_ROOT="$(git -C "$ROOT" worktree list --porcelain 2>/dev/null | awk 'NR==1{print $2}' || true)"
if [[ -z "$CANONICAL_ROOT" && -f "$ROOT/.git" ]]; then
  gitdir="$(sed -n 's/^gitdir:[[:space:]]*//p' "$ROOT/.git" 2>/dev/null | head -n 1 || true)"
  [[ -n "$gitdir" && "$gitdir" == /* ]] && CANONICAL_ROOT="$(cd "$(dirname "$(dirname "$(dirname "$gitdir")")")" 2>/dev/null && pwd || true)"
fi
[[ -n "$CANONICAL_ROOT" ]] || CANONICAL_ROOT="$ROOT"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
ENV_FILE="$HOME/.memoire/capture-tick.env"

# The unit's start timeout follows the tick's own bound: MEMOIRE_CODEX_TICK_
# TIMEOUT + MEMOIRE_CODEX_TICK_KILL_S + a margin, read here at generation time
# (the environment first, then the environment file), with the tick's
# defaults and validation. Tuning either variable later needs a reinstall.
TIMEOUT_MARGIN_S=60
env_value() { # NAME: the value of NAME in the environment, else in the env file
  local v="${!1:-}"
  if [[ -z "$v" && -r "$ENV_FILE" ]]; then
    v="$(sed -n "s/^$1=[\"']\{0,1\}\([0-9]*\)[\"']\{0,1\}[[:space:]]*\$/\1/p" "$ENV_FILE" | tail -n 1)"
  fi
  printf '%s' "$v"
}
tick_timeout_s() {
  local t k
  t="$(env_value MEMOIRE_CODEX_TICK_TIMEOUT)"; k="$(env_value MEMOIRE_CODEX_TICK_KILL_S)"
  [[ "$t" =~ ^[1-9][0-9]*$ ]] || t=240
  [[ "$k" =~ ^[1-9][0-9]*$ ]] || k=10
  echo "$t $k"
}
SERVICE=memoire-capture-codex-tick.service
TIMER=memoire-capture-codex-tick.timer

cmd="${1:-}"
case "$cmd" in
install)
  if [[ "$ROOT" != "$CANONICAL_ROOT" ]]; then
    echo "WARNING: persisting ExecStart under $ROOT (not $CANONICAL_ROOT)." >&2
    echo "         If this is a temporary worktree, set MEMOIRE_REPO_ROOT or reinstall from the main checkout." >&2
  fi
  echo "unit will run: $ROOT/scripts/memoire-capture-codex-tick.sh"
  read -r BOUND_S KILL_S < <(tick_timeout_s)
  START_TIMEOUT_S=$(( BOUND_S + KILL_S + TIMEOUT_MARGIN_S ))
  echo "TimeoutStartSec=${START_TIMEOUT_S}s (tick bound ${BOUND_S}s + kill delay ${KILL_S}s + margin ${TIMEOUT_MARGIN_S}s)"
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_DIR/$SERVICE" <<UNIT
[Unit]
Description=Memoire capture tick over recently-active Codex rollout transcripts

[Service]
Type=oneshot
# MEMOIRE_CAPTURE_CODEX=on|1|true enables the capture; the other optional knobs
# (MEMOIRE_TICK_QUIET_S, MEMOIRE_TICK_SINCE_S, MEMOIRE_CODEX_TICK_TIMEOUT,
# MEMOIRE_CODEX_TICK_KILL_S, ...) live in the same file. The "-" prefix keeps
# the unit valid when the file does not exist.
EnvironmentFile=-%h/.memoire/capture-tick.env
ExecStart=$ROOT/scripts/memoire-capture-codex-tick.sh
# Generated as MEMOIRE_CODEX_TICK_TIMEOUT (${BOUND_S}s) + MEMOIRE_CODEX_TICK_KILL_S
# (${KILL_S}s) + ${TIMEOUT_MARGIN_S}s: the tick bounds its own adapter call, and systemd must
# not kill it first. Reinstall after changing either variable. The first tick
# after enabling bootstraps every cursorless rollout in the window; per-file
# cursors make the next one resume where it stopped.
TimeoutStartSec=${START_TIMEOUT_S}s
Nice=10
UNIT
  cat > "$UNIT_DIR/$TIMER" <<UNIT
[Unit]
Description=Ten-minute Memoire Codex capture tick

[Timer]
OnCalendar=*:5/10
RandomizedDelaySec=60
Persistent=true

[Install]
WantedBy=timers.target
UNIT
  systemctl --user daemon-reload
  systemctl --user enable --now "$TIMER"
  echo "installed + enabled $TIMER"
  ;;
start) systemctl --user start "$TIMER" ;;
stop) systemctl --user disable --now "$TIMER" ;;
status)
  systemctl --user status "$TIMER" --no-pager || true
  systemctl --user list-timers "$TIMER" --no-pager || true
  ;;
run-once) systemctl --user start "$SERVICE" ;;
uninstall)
  systemctl --user disable --now "$TIMER" || true
  rm -f "$UNIT_DIR/$TIMER" "$UNIT_DIR/$SERVICE"
  systemctl --user daemon-reload
  echo "removed $TIMER and $SERVICE"
  ;;
*)
  echo "usage: $0 install|start|stop|status|run-once|uninstall" >&2
  exit 1
  ;;
esac
