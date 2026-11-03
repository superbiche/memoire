#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# derive-timer.sh — install/manage the memoire-derive-sweep systemd user
# timer (operator ruling 2026-07-10: TPM-encrypted credential).
#
# Usage:
#   scripts/derive-timer.sh encrypt   # TPM2/host-encrypt the HL key into the
#                                     # user credstore (reads the value from
#                                     # the ambient env var, never echoes it)
#   scripts/derive-timer.sh install   # write + enable the service/timer units
#   scripts/derive-timer.sh start|stop|status|run-once
#
# The service loads the credential via LoadCredentialEncrypted and
# memoire-derive-sweep.sh exports it under the api_key_env name, so the key
# value at rest exists only TPM/host-encrypted and is never in a unit file,
# argv, or the journal.

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
KEY_ENV="${MEMOIRE_DERIVE_KEY_ENV:-LLAMA_API_KEY_HL5}"
CRED_NAME="$(printf '%s' "$KEY_ENV" | tr '[:upper:]' '[:lower:]')"
CREDSTORE="${XDG_CONFIG_HOME:-$HOME/.config}/credstore.encrypted"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
SERVICE=memoire-derive-sweep.service
TIMER=memoire-derive-sweep.timer

cmd="${1:-}"
case "$cmd" in
encrypt)
  printenv "$KEY_ENV" >/dev/null 2>&1 || {
    echo "env var $KEY_ENV not set in this shell; run from a shell where it is ambient" >&2
    exit 1
  }
  mkdir -p "$CREDSTORE"
  # Value flows env -> pipe -> encrypted file; never argv, never terminal.
  printenv "$KEY_ENV" | tr -d '\r\n' |
    systemd-creds encrypt --user --name="$CRED_NAME" - "$CREDSTORE/$CRED_NAME"
  chmod 600 "$CREDSTORE/$CRED_NAME"
  echo "encrypted credential written: $CREDSTORE/$CRED_NAME"
  ;;
install)
  [[ -r "$CREDSTORE/$CRED_NAME" ]] || {
    echo "missing $CREDSTORE/$CRED_NAME — run '$0 encrypt' first" >&2
    exit 1
  }
  if [[ "$ROOT" != "$CANONICAL_ROOT" ]]; then
    echo "WARNING: persisting ExecStart under $ROOT (not $CANONICAL_ROOT)." >&2
    echo "         If this is a temporary worktree, set MEMOIRE_REPO_ROOT or reinstall from the main checkout." >&2
  fi
  echo "unit will run: $ROOT/scripts/memoire-derive-sweep.sh"
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_DIR/$SERVICE" <<EOF
[Unit]
Description=Memoire derivation sweep over recently-active sessions

[Service]
Type=oneshot
LoadCredentialEncrypted=$CRED_NAME
# Early-fire trigger DISABLED by default (operator ruling 2026-07-22,
# D163 residual MED-2: HL3 re-marks oversized legs even after HL5 derived
# them, so the marker count can degenerate to one stale fire per night).
# Arm by flipping to 1 once the R-10 store-backed outstanding check lands.
Environment=MEMOIRE_HL5_TRIGGER=0
ExecStart=$ROOT/scripts/memoire-derive-sweep.sh
# Early-fire gate for the nightly HL5 lane (memoire:D163) — quiet-hours +
# recency + cooldown guarded; fail-open. The "-" prefix ignores its exit
# code and the timeout(1) wrapper bounds its wall clock so a wedged
# systemctl client or log scan can never hold the hourly unit in its start
# transaction (round-1 MED-1).
ExecStartPost=-/usr/bin/timeout 60 $ROOT/scripts/memoire-derive-hl5-trigger.sh
# A sweep serializes N sessions x up to 15m each behind one lock.
TimeoutStartSec=2h
Nice=10
EOF
  cat > "$UNIT_DIR/$TIMER" <<EOF
[Unit]
Description=Hourly Memoire derivation sweep

[Timer]
OnCalendar=hourly
RandomizedDelaySec=10m
Persistent=true

[Install]
WantedBy=timers.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable --now "$TIMER"
  echo "installed + enabled $TIMER"
  ;;
install-hl5)
  # Nightly Conductor-routed HL5 sweep (memoire:D163): 04:00 local time
  # (Europe/Paris on this host) — late enough not to disturb late-evening
  # work, early enough for the heat to dissipate by morning (operator,
  # 2026-07-22). The Reveille Conductor handles WOL, model load, proxying,
  # and reap/poweroff; the pre-warm step absorbs its 2-4 min cold path.
  HL5_CRED=reveille_api_key
  [[ -r "$CREDSTORE/$HL5_CRED" ]] || {
    echo "missing $CREDSTORE/$HL5_CRED — run 'MEMOIRE_DERIVE_KEY_ENV=REVEILLE_API_KEY $0 encrypt' first" >&2
    exit 1
  }
  if [[ "$ROOT" != "$CANONICAL_ROOT" ]]; then
    echo "WARNING: persisting ExecStart under $ROOT (not $CANONICAL_ROOT)." >&2
    echo "         If this is a temporary worktree, set MEMOIRE_REPO_ROOT or reinstall from the main checkout." >&2
  fi
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_DIR/memoire-derive-sweep-hl5.service" <<EOF
[Unit]
Description=Nightly Memoire derivation sweep on HL5 via the Reveille Conductor

[Service]
Type=oneshot
LoadCredentialEncrypted=$HL5_CRED
Environment=MEMOIRE_DERIVE_ENDPOINT=hl5-conductor
Environment=MEMOIRE_DERIVE_MODEL=brain
# Derive runs the brain at low reasoning effort (operator ruling 2026-08-26).
Environment=MEMOIRE_DERIVE_REASONING_EFFORT=low
Environment=MEMOIRE_DERIVE_KEY_ENV=REVEILLE_API_KEY
Environment=MEMOIRE_SWEEP_MAX_PROMPT_BYTES=110000
# ExecCondition (not Pre): a delayed/duplicate activation outside quiet
# hours, or one arriving after the lane already completed tonight, SKIPS
# cleanly instead of waking HL5 in the office by day (round-1
# HIGH-1/MED-3). Recency evidence is the recorded success epoch, never
# systemd unit properties (a condition skip refreshes those — round-2
# HIGH-NEW-1). Deliberate out-of-band runs: invoke the sweep SCRIPT
# directly with the HL5 env — no force env on the unit (round-2 LOW-NEW-3).
ExecCondition=$ROOT/scripts/memoire-derive-hl5-trigger.sh --lane-fresh-check
ExecStartPre=-$ROOT/scripts/memoire-conductor-prewarm.sh
ExecStart=$ROOT/scripts/memoire-derive-sweep.sh
ExecStartPost=$ROOT/scripts/memoire-derive-hl5-trigger.sh --record-success
TimeoutStartSec=4h
Nice=10
EOF
  cat > "$UNIT_DIR/memoire-derive-sweep-hl5.timer" <<EOF
[Unit]
Description=Nightly (04:00 local) Memoire HL5 derivation sweep

[Timer]
OnCalendar=*-*-* 04:00:00
RandomizedDelaySec=5m
# No Persistent=true: a missed 04:00 (host asleep/off) must NOT catch up
# after boot — that would wake HL5 during prohibited daytime hours
# (round-1 HIGH-1). The early-fire trigger covers catch-up within the next
# quiet-hours window instead.

[Install]
WantedBy=timers.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable --now memoire-derive-sweep-hl5.timer
  echo "installed + enabled memoire-derive-sweep-hl5.timer"
  ;;
start) systemctl --user start "$TIMER" ;;
stop) systemctl --user disable --now "$TIMER" ;;
status)
  systemctl --user status "$TIMER" --no-pager || true
  systemctl --user list-timers "$TIMER" --no-pager || true
  ;;
run-once) systemctl --user start "$SERVICE" ;;
*)
  echo "usage: $0 encrypt|install|install-hl5|start|stop|status|run-once" >&2
  exit 1
  ;;
esac
