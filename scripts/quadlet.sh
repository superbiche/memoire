#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UNIT_SRC="$ROOT/deploy/quadlet/memoire.container"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/containers/systemd"
UNIT_DST="$UNIT_DIR/memoire.container"
SERVICE="memoire.service"
IMAGE="localhost/memoire:latest"
BASE="${MEMOIRE_BASE:-http://127.0.0.1:37778/v1}"

usage() {
  cat <<'USAGE' >&2
Usage: quadlet.sh <install|start|stop|rebuild|status|health|uninstall>

Installs and operates Memoire's rootless Podman Quadlet unit.
Use rebuild to build the image, reload the unit, restart the service, and
wait for readiness. Run health for strict operational thresholds.
USAGE
}

require_systemctl() {
  command -v systemctl >/dev/null 2>&1 || {
    echo "systemctl not found" >&2
    exit 1
  }
}

install_unit() {
  install -D -m 0644 "$UNIT_SRC" "$UNIT_DST"
  systemctl --user daemon-reload
  echo "installed $UNIT_DST"
}

require_podman() {
  command -v podman >/dev/null 2>&1 || {
    echo "podman not found" >&2
    exit 1
  }
}

build_image() {
  require_podman
  podman build --target api -t "$IMAGE" -f "$ROOT/services/api/Containerfile" "$ROOT"
}

ensure_image() {
  require_podman
  if ! podman image exists "$IMAGE"; then
    echo "missing image $IMAGE; run: make quadlet-image" >&2
    exit 1
  fi
}

wait_health() {
  for _ in $(seq 1 30); do
    if "$ROOT/scripts/check-health.sh" >/dev/null 2>&1; then
      "$ROOT/scripts/check-health.sh"
      return 0
    fi
    sleep 1
  done
  echo "Memoire did not pass health check" >&2
  journalctl --user -u "$SERVICE" -n 80 --no-pager >&2 || true
  exit 1
}

wait_ready() {
  for _ in $(seq 1 60); do
    if curl -fsS "$BASE/readiness" >/dev/null 2>&1; then
      curl -fsS "$BASE/readiness"
      printf '\n'
      return 0
    fi
    sleep 1
  done
  echo "Memoire did not become ready" >&2
  journalctl --user -u "$SERVICE" -n 80 --no-pager >&2 || true
  exit 1
}

report_health() {
  if ! "$ROOT/scripts/check-health.sh"; then
    echo "warning: rebuild completed, but operational health check failed" >&2
  fi
}

cmd="${1:-}"
case "$cmd" in
  install)
    require_systemctl
    install_unit
    ;;
  start)
    require_systemctl
    ensure_image
    if [[ ! -f "$UNIT_DST" ]]; then
      install_unit
    fi
    systemctl --user start "$SERVICE"
    wait_health
    ;;
  rebuild)
    require_systemctl
    build_image
    install_unit
    systemctl --user restart "$SERVICE"
    wait_ready
    report_health
    ;;
  stop)
    require_systemctl
    systemctl --user stop "$SERVICE"
    ;;
  status)
    require_systemctl
    systemctl --user status "$SERVICE" --no-pager
    ;;
  health)
    "$ROOT/scripts/check-health.sh"
    ;;
  uninstall)
    require_systemctl
    systemctl --user stop "$SERVICE" 2>/dev/null || true
    rm -f "$UNIT_DST"
    systemctl --user daemon-reload
    echo "removed $UNIT_DST"
    ;;
  -h|--help|"")
    usage
    [[ -n "$cmd" ]]
    ;;
  *)
    echo "unknown command: $cmd" >&2
    usage
    exit 2
    ;;
esac
