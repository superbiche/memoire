#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""memoire-coverage — Memoire's harness-parity validator.

Computes, from registry/capture-lanes.json and the LIVE machine, which capture
lane actually serves each fleet harness (liaison HARNESS-PARITY.md § 2-3) and
publishes the result as an owner coverage file Liaison's `harness.parity` row
reads: ~/.agents/coverage/memoire.json (schema liaison-coverage/1), carrying
producer identity (project, commit, host), generated_at, the sha256 of the
inventory it was computed against, and one disposition per route:

  wired     the lane's prerequisites all hold on this machine
  n/a       declared with a reason (a harness with no native session)
  gap       anything else, with the failing prerequisite named

Nothing here is authored: every `wired` is observed (timer active, env flag on,
script executable, hook present, extension byte-equal, exclusion file readable).
Exit 0 after writing; `--strict` exits 1 when any gap remains; `--print` shows
the table. No model, no network, no secrets.

Environment: MEMOIRE_HARNESS_INVENTORY (default ~/.agents/harnesses.json),
MEMOIRE_CAPTURE_LANES (default <repo>/registry/capture-lanes.json),
MEMOIRE_ACPX_REGISTRY (default ~/.agents/acpx.json), MEMOIRE_COVERAGE_OUT
(default ~/.agents/coverage/memoire.json), MEMOIRE_HOME, XDG_CONFIG_HOME,
MEMOIRE_SYSTEMCTL (test seam: a command answering `is-active <unit>`).
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import shutil
import socket
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SCHEMA = "liaison-coverage/1"
SURFACE = "memoire.capture"
LANES_SCHEMA = "memoire-capture-lanes/1"
INVENTORY_SCHEMA = "liaison-harness-registry/1"


def env_path(var: str, default: str) -> Path:
    return Path(os.environ.get(var, default)).expanduser()


def read_json(path: Path, schema: str | None = None) -> dict:
    data = json.loads(path.read_text(encoding="utf-8"))
    if schema is not None and data.get("schema") != schema:
        raise SystemExit(f"memoire-coverage: {path}: schema must be {schema!r}")
    return data


def capture_env() -> dict[str, str]:
    """Knobs from ~/.memoire/capture-tick.env (last assignment wins), over the process env."""
    env = dict(os.environ)
    home = env_path("MEMOIRE_HOME", "~/.memoire")
    env_file = home / "capture-tick.env"
    if env_file.is_file():
        for line in env_file.read_text(encoding="utf-8").splitlines():
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, value = line.split("=", 1)
            env[key.strip().removeprefix("export ").strip()] = value.strip().strip('"').strip("'")
    return env


def _systemctl() -> str | None:
    return os.environ.get("MEMOIRE_SYSTEMCTL") or shutil.which("systemctl")


def unit_active(unit: str) -> tuple[bool, str]:
    systemctl = _systemctl()
    if not systemctl:
        return False, "systemctl not available"
    try:
        proc = subprocess.run([systemctl, "--user", "is-active", unit], capture_output=True, text=True, timeout=10)
    except (OSError, subprocess.TimeoutExpired) as exc:
        return False, f"systemctl failed: {exc}"
    state = (proc.stdout or proc.stderr).strip()
    return state == "active", state or "unknown"


def timer_exec_script(timer: str) -> tuple[Path | None, str]:
    """The script the timer's SERVICE actually runs (ExecStart path), read from
    systemd — the lane is only as live as THAT file, not this checkout's copy."""
    systemctl = _systemctl()
    if not systemctl:
        return None, "systemctl not available"
    service = timer[:-len(".timer")] + ".service" if timer.endswith(".timer") else timer
    try:
        proc = subprocess.run([systemctl, "--user", "show", "-p", "ExecStart", "--value", service],
                              capture_output=True, text=True, timeout=10)
    except (OSError, subprocess.TimeoutExpired) as exc:
        return None, f"systemctl show failed: {exc}"
    out = (proc.stdout or "").strip()
    for token in out.replace("{", " ").replace("}", " ").replace(";", " ").split():
        if token.startswith("path="):
            return Path(token[len("path="):]), service
    return None, f"{service}: no ExecStart path in systemd's answer"


def timer_runs_executable(timer: str) -> list[str]:
    """Problems with the timer's live state: inactive, or ExecStart not executable."""
    problems: list[str] = []
    ok, state = unit_active(timer)
    if not ok:
        problems.append(f"{timer} {state}")
    exec_path, detail = timer_exec_script(timer)
    if exec_path is None:
        problems.append(detail)
    elif not (exec_path.is_file() and os.access(exec_path, os.X_OK)):
        problems.append(f"{timer} ExecStart not executable: {exec_path}")
    return problems


def hook_present(surface: Path, events: list[str], script_name: str) -> tuple[bool, str]:
    """The installed hook command for `script_name` exists on every event AND
    its script exists and is executable where the hook points (the capture
    service checkout, not necessarily this one)."""
    if not surface.is_file():
        return False, f"surface missing: {surface}"
    try:
        settings = json.loads(surface.read_text(encoding="utf-8"))
    except ValueError as exc:
        return False, f"surface unreadable: {exc}"
    missing: list[str] = []
    broken: list[str] = []
    for event in events:
        found = False
        for group in settings.get("hooks", {}).get(event, []) or []:
            for hook in group.get("hooks", []) or []:
                command = str(hook.get("command", ""))
                head = command.split()[0] if command.strip() else ""
                if Path(head).name == script_name:
                    found = True
                    target = Path(head).expanduser()
                    if not (target.is_file() and os.access(target, os.X_OK)):
                        broken.append(f"{event}: {head} not executable")
        if not found:
            missing.append(event)
    if missing:
        return False, f"hook {script_name} missing on {', '.join(missing)}"
    if broken:
        return False, "; ".join(broken)
    return True, f"hook {script_name} on {', '.join(events)}"


def check_native(native: dict, env: dict[str, str]) -> tuple[str, str]:
    lane = native.get("lane")
    if lane == "none":
        reason = native.get("reason", "")
        if native.get("applicable", True):
            return "gap", f"applicable native route has no capture lane: {reason or 'no reason given'}"
        return ("n/a", reason) if len(reason) >= 12 else ("gap", "lane none without a reason")
    problems: list[str] = []
    for rel in native.get("scripts", []):
        script = ROOT / rel
        if not (script.is_file() and os.access(script, os.X_OK)):
            problems.append(f"script not executable: {rel}")
    if lane in ("rollout-sweep", "store-sweep"):
        flag = native.get("enable_env")
        if flag and env.get(flag, "").lower() not in ("on", "1", "true"):
            problems.append(f"{flag} is not on")
    if native.get("timer"):
        problems += timer_runs_executable(native["timer"])
    if lane == "hooks":
        for rel in native.get("scripts", []):
            ok, detail = hook_present(Path(native["surface"]).expanduser(), native.get("events", []), Path(rel).name)
            if not ok:
                problems.append(detail)
    if lane == "extension":
        dest = Path(native["surface"]).expanduser()
        src = ROOT / native["source"]
        if not dest.is_file():
            problems.append(f"extension not installed: {dest}")
        elif not src.is_file():
            problems.append(f"extension source missing: {src}")
        elif dest.read_bytes() != src.read_bytes():
            problems.append(f"extension differs from source: {dest}")
    if lane not in ("hooks", "rollout-sweep", "store-sweep", "extension"):
        problems.append(f"unknown lane {lane!r}")
    if problems:
        return "gap", "; ".join(problems)
    return "wired", f"{lane}" + (f" via {native['timer']}" if native.get("timer") else "")


def check_acpx_lane(acpx_lane: dict, env: dict[str, str]) -> tuple[bool, str]:
    """`env` is the EFFECTIVE environment of the scheduled service (the env
    file over the process environment): every knob the adapter reads —
    enable flag, queue writer path — is resolved from that one view."""
    problems: list[str] = []
    for key in ("script", "runner"):
        rel = acpx_lane.get(key)
        if not rel:
            continue
        path = Path(rel).expanduser()
        path = path if path.is_absolute() else ROOT / path
        if not (path.is_file() and os.access(path, os.X_OK)):
            problems.append(f"{key} not executable: {rel}")
    queue = acpx_lane.get("queue_bin")
    queue_path = Path(env["MEMOIRE_QUEUE_BIN"]).expanduser() if env.get("MEMOIRE_QUEUE_BIN") else (ROOT / queue if queue else None)
    if queue_path is not None and not os.access(queue_path, os.X_OK):
        problems.append(f"queue writer missing: {queue_path} (make build)")
    for binary in ("jq", "python3", "timeout"):
        if shutil.which(binary) is None:
            problems.append(f"{binary} not on PATH")
    exclusion = Path(os.environ.get("MEMOIRE_ACPX_EXCLUDE_FILE") or
                     (env_path("XDG_CONFIG_HOME", "~/.config") / "memoire" / "acpx-exclude"))
    if not exclusion.is_file() or not os.access(exclusion, os.R_OK):
        problems.append(f"exclusion file missing or unreadable (fail-closed): {exclusion}")
    timer = acpx_lane.get("timer")
    if not timer:
        problems.append("no scheduled runner for the acpx journal adapter")
    else:
        flag = acpx_lane.get("enable_env")
        if flag and env.get(flag, "").lower() not in ("on", "1", "true"):
            problems.append(f"{flag} is not on")
        problems += timer_runs_executable(timer)
    return (not problems), ("; ".join(problems) if problems else f"journal adapter scheduled by {timer}")


def compute(inventory: dict, lanes: dict, acpx_registry: dict | None) -> dict:
    env = capture_env()
    capture_off = env.get("MEMOIRE_CAPTURE", "").lower() in ("off", "0", "false")
    acpx_ok, acpx_detail = check_acpx_lane(lanes["acpx_lane"], env)
    if acpx_registry is None:
        acpx_ok, acpx_detail = False, "acpx registry not found: every acpx route is unverifiable"
    owned_agents = {}
    for hid, spec in inventory["harnesses"].items():
        if spec.get("status") == "retired":
            continue
        for agent in spec.get("acpx_agents", []):
            owned_agents[agent] = hid
    configured_agents = set((acpx_registry or {}).get("agents", {}))
    table: dict[str, dict[str, dict]] = {}
    for hid, spec in inventory["harnesses"].items():
        if spec.get("status") == "retired":
            continue
        routes: dict[str, dict] = {}
        decl = lanes["harnesses"].get(hid)
        if decl is None:
            routes["native"] = {"disposition": "gap", "detail": "no capture lane declared for this harness"}
            table[hid] = routes
            continue
        disposition, detail = check_native(decl["native"], env)
        if capture_off and disposition == "wired":
            disposition, detail = "gap", "MEMOIRE_CAPTURE is off (kill switch): nothing is captured"
        cell = {"disposition": disposition, "detail": detail}
        if disposition == "n/a":
            cell["reason"] = detail
        routes["native"] = cell
        for agent in spec.get("acpx_agents", []):
            mode = decl.get("acpx")
            if acpx_registry is not None and agent not in configured_agents:
                routes[f"acpx:{agent}"] = {"disposition": "gap", "detail": f"agent {agent} not configured in the acpx registry"}
            elif capture_off:
                routes[f"acpx:{agent}"] = {"disposition": "gap", "detail": "MEMOIRE_CAPTURE is off (kill switch): nothing is captured"}
            elif mode == "boundaries" and routes["native"]["disposition"] != "wired":
                routes[f"acpx:{agent}"] = {"disposition": "gap", "detail": "boundaries-only mode declared but the native lane is not wired; full journal capture applies"}
            elif not acpx_ok:
                routes[f"acpx:{agent}"] = {"disposition": "gap", "detail": acpx_detail}
            else:
                routes[f"acpx:{agent}"] = {"disposition": "wired", "detail": f"acpx journal, {mode} mode; {acpx_detail}"}
        table[hid] = routes
    return table


def producer() -> dict:
    commit = "unknown"
    try:
        commit = subprocess.run(["git", "-C", str(ROOT), "rev-parse", "HEAD"], capture_output=True,
                                text=True, timeout=10).stdout.strip() or "unknown"
    except (OSError, subprocess.TimeoutExpired):
        pass
    return {"project": "memoire", "commit": commit, "host": os.environ.get("HOSTNAME") or socket.gethostname()}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="publish Memoire's capture coverage per harness")
    parser.add_argument("--strict", action="store_true", help="exit 1 when any gap remains")
    parser.add_argument("--print", action="store_true", help="print the table")
    parser.add_argument("--no-write", action="store_true", help="compute only")
    args = parser.parse_args(argv)

    inventory_path = env_path("MEMOIRE_HARNESS_INVENTORY", "~/.agents/harnesses.json")
    lanes_path = env_path("MEMOIRE_CAPTURE_LANES", str(ROOT / "registry" / "capture-lanes.json"))
    acpx_path = env_path("MEMOIRE_ACPX_REGISTRY", "~/.agents/acpx.json")
    out_path = env_path("MEMOIRE_COVERAGE_OUT", "~/.agents/coverage/memoire.json")
    if not inventory_path.is_file():
        print(f"memoire-coverage: harness inventory not found: {inventory_path}", file=sys.stderr)
        return 2
    inventory = read_json(inventory_path, INVENTORY_SCHEMA)
    lanes = read_json(lanes_path, LANES_SCHEMA)
    acpx_registry = read_json(acpx_path) if acpx_path.is_file() else None

    table = compute(inventory, lanes, acpx_registry)
    document = {
        "schema": SCHEMA,
        "producer": producer(),
        "generated_at": dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds"),
        "inventory_sha256": hashlib.sha256(inventory_path.read_bytes()).hexdigest(),
        "inventory": str(inventory_path),
        "surfaces": {SURFACE: table},
    }
    gaps = [(hid, route, cell["detail"]) for hid, routes in table.items()
            for route, cell in routes.items() if cell["disposition"] == "gap"]
    if not args.no_write:
        out_path.parent.mkdir(parents=True, exist_ok=True)
        tmp = out_path.with_suffix(".tmp")
        tmp.write_text(json.dumps(document, indent=2) + "\n", encoding="utf-8")
        os.replace(tmp, out_path)
    if args.print or args.strict:
        for hid, routes in table.items():
            for route, cell in routes.items():
                mark = {"wired": "ok ", "n/a": "n/a", "gap": "GAP"}[cell["disposition"]]
                print(f"{mark} {hid:<12} {route:<18} {cell['detail']}")
        print(f"{sum(len(r) for r in table.values())} route(s), {len(gaps)} gap(s)"
              + ("" if args.no_write else f"; written {out_path}"))
    return 1 if (args.strict and gaps) else 0


if __name__ == "__main__":
    sys.exit(main())
