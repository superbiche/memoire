#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail
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
SERVICE=memoire-capture-acpx-tick.service
TIMER=memoire-capture-acpx-tick.timer
case "${1:-}" in
install)
  [[ "$ROOT" == "$CANONICAL_ROOT" ]] || echo "WARNING: persisting ExecStart under $ROOT; reinstall from $CANONICAL_ROOT after landing." >&2
  t="${MEMOIRE_ACPX_TICK_TIMEOUT:-240}"; k="${MEMOIRE_ACPX_TICK_KILL_S:-10}"
  [[ "$t" =~ ^[1-9][0-9]*$ ]] || t=240
  [[ "$k" =~ ^[1-9][0-9]*$ ]] || k=10
  bound=$((t + k + 60))
  mkdir -p "$UNIT_DIR"
  [[ ! -L "$UNIT_DIR/$SERVICE" && ! -L "$UNIT_DIR/$TIMER" ]] || { echo 'unit symlink refused' >&2; exit 1; }
  cat > "$UNIT_DIR/$SERVICE" <<UNIT
[Unit]
Description=Memoire capture tick over acpx worker sessions

[Service]
Type=oneshot
# Optional non-secret capture switches, read by systemd on every invocation.
# MEMOIRE_CAPTURE_ACPX=on enables; the tick never sources this file.
EnvironmentFile=-%h/.memoire/capture-tick.env
ExecStart=$ROOT/scripts/memoire-capture-acpx-tick.sh
TimeoutStartSec=${bound}s
Nice=10
UNIT
  cat > "$UNIT_DIR/$TIMER" <<UNIT
[Unit]
Description=Ten-minute Memoire acpx worker-session capture tick

[Timer]
OnCalendar=*:3/10
RandomizedDelaySec=60
Persistent=true

[Install]
WantedBy=timers.target
UNIT
  systemctl --user daemon-reload
  systemctl --user enable --now "$TIMER"
  echo "installed + enabled $TIMER TimeoutStartSec=${bound}s"
  ;;
start) systemctl --user start "$TIMER" ;;
stop) systemctl --user disable --now "$TIMER" ;;
status) systemctl --user list-timers "$TIMER" --no-pager ;;
run-once) systemctl --user start "$SERVICE" ;;
uninstall)
  systemctl --user disable --now "$TIMER"
  command rm -f "$UNIT_DIR/$TIMER" "$UNIT_DIR/$SERVICE"
  systemctl --user daemon-reload
  ;;
*) echo "usage: $0 install|start|stop|status|run-once|uninstall" >&2; exit 1 ;;
esac
