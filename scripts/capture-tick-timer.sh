#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# capture-tick-timer.sh — install/manage the memoire-capture-tick systemd user
# timer (streaming capture, operator ruling 2026-09-08, Fibery #342).
#
# Usage:
#   scripts/capture-tick-timer.sh install     # write + enable the service/timer
#   scripts/capture-tick-timer.sh start|stop|status|run-once|uninstall
#
# The tick body holds NO credential: it captures transcript tails into the
# local file queue and, only when MEMOIRE_TICK_NOTIFY=1, POSTs an idempotent
# derive intent with the ordinary client token. Knobs are passed through an
# OPTIONAL EnvironmentFile (~/.memoire/capture-tick.env) so the unit itself
# never has to be edited to retune the cadence knobs.
#
# Cadence: every 10 minutes, Persistent=true (a tick missed while the host
# slept runs once on resume — the cursors make a late tick cheap), with a
# randomized delay so it never lands exactly on the hourly derive sweep.

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
SERVICE=memoire-capture-tick.service
TIMER=memoire-capture-tick.timer

cmd="${1:-}"
case "$cmd" in
install)
  if [[ "$ROOT" != "$CANONICAL_ROOT" ]]; then
    echo "WARNING: persisting ExecStart under $ROOT (not $CANONICAL_ROOT)." >&2
    echo "         If this is a temporary worktree, set MEMOIRE_REPO_ROOT or reinstall from the main checkout." >&2
  fi
  echo "unit will run: $ROOT/scripts/memoire-capture-tick.sh"
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_DIR/$SERVICE" <<EOF
[Unit]
Description=Memoire capture tick over recently-active Claude Code transcripts

[Service]
Type=oneshot
# Optional knobs (MEMOIRE_TICK_QUIET_S, MEMOIRE_TICK_SINCE_S,
# MEMOIRE_TICK_TIMEOUT, MEMOIRE_TICK_NOTIFY, MEMOIRE_BASE_URL...). The "-"
# prefix keeps the unit valid when the file does not exist.
EnvironmentFile=-%h/.memoire/capture-tick.env
ExecStart=$ROOT/scripts/memoire-capture-tick.sh
# Must exceed MEMOIRE_TICK_TIMEOUT (default 600s): the tick bounds its own
# adapter call, and systemd must not kill it first. The first tick after
# install bootstraps every cursorless transcript in the window and is the
# slow one; per-file cursors make the next one resume cleanly.
TimeoutStartSec=15m
Nice=10
EOF
  cat > "$UNIT_DIR/$TIMER" <<EOF
[Unit]
Description=Ten-minute Memoire capture tick

[Timer]
OnCalendar=*:0/10
RandomizedDelaySec=60
Persistent=true

[Install]
WantedBy=timers.target
EOF
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
