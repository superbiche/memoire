#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# capture-health-timer.sh — install/manage the memoire-capture-health systemd
# user timer (MISSING #19): the alert outside sessions when capture is stuck or
# dead on a client.
#
# Usage:
#   scripts/capture-health-timer.sh install     # write + enable the service/timer
#   scripts/capture-health-timer.sh start|stop|status|run-once|uninstall
#
# The check body holds NO credential: it reads the local queue, the forwarder's
# forward-status.json and systemd user unit state, and makes at most one
# unauthenticated GET of the hub's /v1/health. Knobs (thresholds, reminder
# interval, MEMOIRE_HEALTH_SINK) come from the OPTIONAL EnvironmentFile
# (~/.memoire/capture-tick.env), so the unit never has to be edited to retune.
# The service exits 1 (warn) or 2 (critical) when a condition holds; the unit
# accepts those as success so the journal does not record a failed unit.
#
# Cadence: every 15 minutes, Persistent=true (a check missed while the host
# slept runs once on resume), with a randomized delay.

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
SERVICE=memoire-capture-health.service
TIMER=memoire-capture-health.timer

cmd="${1:-}"
case "$cmd" in
install)
  if [[ "$ROOT" != "$CANONICAL_ROOT" ]]; then
    echo "WARNING: persisting ExecStart under $ROOT (not $CANONICAL_ROOT)." >&2
    echo "         If this is a temporary worktree, set MEMOIRE_REPO_ROOT or reinstall from the main checkout." >&2
  fi
  echo "unit will run: $ROOT/scripts/memoire-capture-health.sh"
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_DIR/$SERVICE" <<EOF
[Unit]
Description=Memoire capture health check (queue, forwarder, capture units)

[Service]
Type=oneshot
# Optional knobs (MEMOIRE_INJECT_BACKLOG_MIN, MEMOIRE_INJECT_BACKLOG_AGE_S,
# MEMOIRE_INJECT_TRANSPORT_DEAD_S, MEMOIRE_HEALTH_*, MEMOIRE_BASE_URL...). The
# "-" prefix keeps the unit valid when the file does not exist.
EnvironmentFile=-%h/.memoire/capture-tick.env
ExecStart=$ROOT/scripts/memoire-capture-health.sh
# Exit 1 = warn, 2 = critical: conditions, not a failed run.
SuccessExitStatus=1 2
TimeoutStartSec=2m
Nice=10
EOF
  cat > "$UNIT_DIR/$TIMER" <<EOF
[Unit]
Description=Fifteen-minute Memoire capture health check

[Timer]
OnCalendar=*:0/15
RandomizedDelaySec=120
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
