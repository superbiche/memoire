#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# claude-hook-queue-failure-test.sh — a queue write that fails (the redaction
# key cannot be loaded: fail-closed, redact-v5 ruling Q8 / operator ruling a1
# 2026-10-08) must be LOUD without making the hook fail: the writer's stderr
# reaches the hook log, a persistent desktop toast is sent (critical urgency,
# no expiry) when notify-send and a session bus exist, the hook still exits 0
# in its fail-open default, and a successful write sends no toast.
# notify-send is a recorder stub on a toolbox PATH that holds no real one.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HOOK="$ROOT/scripts/memoire-capture-claude.sh"
command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/home/hooks" "$WORK/bin"
NOTIFY="$WORK/notify.log"; : > "$NOTIFY"
cat > "$WORK/bin/notify-send" <<STUB
#!/usr/bin/env bash
# one record per call: the body may carry newlines
printf 'CALL %s\n' "\$(printf '%s ' "\$@" | tr '\n' ' ')" >> "$NOTIFY"
STUB
cat > "$WORK/queue-fail" <<'STUB'
#!/usr/bin/env bash
echo "memoire-queue: redaction key: open /nowhere/redaction-key: permission denied" >&2
exit 1
STUB
cat > "$WORK/queue-ok" <<'STUB'
#!/usr/bin/env bash
echo "/queue/pending/x.json"
STUB
chmod +x "$WORK/bin/notify-send" "$WORK/queue-fail" "$WORK/queue-ok"

fail() { echo "FAIL: $1" >&2; exit 1; }

fire() { # fire <queue-bin> [env...]
  local q="$1"; shift
  jq -nc '{session_id:"s1",cwd:"/work/proj",hook_event_name:"UserPromptSubmit",prompt:"hello"}' |
    env PATH="$WORK/bin:$PATH" DBUS_SESSION_BUS_ADDRESS="unix:path=$WORK/none" \
      MEMOIRE_HOME="$WORK/home" MEMOIRE_QUEUE_BIN="$q" MEMOIRE_PROJECT=proj \
      MEMOIRE_PROJECTS_REGISTRY=/nonexistent "$@" "$HOOK"
}

LOG="$WORK/home/hooks/claude.log"

# 1. Failing write, fail-open default: exit 0, the writer's reason in the log, one critical persistent toast.
fire "$WORK/queue-fail" || fail "fail-open hook exited non-zero"
grep -q 'queue write failed: memoire-queue: redaction key: ' "$LOG" || fail "writer stderr not in the hook log: $(cat "$LOG")"
grep -q 'nowhere/redaction-key' "$LOG" || fail "reason truncated or lost"
(( $(grep -c "^CALL " "$NOTIFY") == 1 )) || fail "want 1 toast, got $(grep -c "^CALL " "$NOTIFY")"
grep -q -- '-u critical' "$NOTIFY" || fail "toast not critical: $(cat "$NOTIFY")"
grep -q -- '-t 0' "$NOTIFY" || fail "toast expires: $(cat "$NOTIFY")"
grep -q 'NOT written' "$NOTIFY" || fail "toast title: $(cat "$NOTIFY")"
grep -q 'redaction key' "$NOTIFY" || fail "toast body lacks the reason: $(cat "$NOTIFY")"

# 2. Strict mode: same toast, and the hook now fails with the message on stderr.
: > "$NOTIFY"
err="$(fire "$WORK/queue-fail" MEMOIRE_CLAUDE_STRICT=1 2>&1 >/dev/null)" && fail "strict hook exited zero"
grep -q 'redaction key' <<<"$err" || fail "strict stderr lacks the reason: $err"
(( $(grep -c "^CALL " "$NOTIFY") == 1 )) || fail "strict: want 1 toast, got $(grep -c "^CALL " "$NOTIFY")"

# 3. MEMOIRE_CLAUDE_NO_NOTIFY=1 silences the toast, not the log.
: > "$NOTIFY"; : > "$LOG"
fire "$WORK/queue-fail" MEMOIRE_CLAUDE_NO_NOTIFY=1 || fail "no-notify hook exited non-zero"
(( $(grep -c "^CALL " "$NOTIFY") == 0 )) || fail "no-notify sent a toast"
grep -q 'queue write failed' "$LOG" || fail "no-notify lost the log line"

# 4. No session bus: no toast, no failure.
: > "$NOTIFY"
jq -nc '{session_id:"s1",cwd:"/work/proj",hook_event_name:"UserPromptSubmit",prompt:"hello"}' |
  env -u DBUS_SESSION_BUS_ADDRESS -u XDG_RUNTIME_DIR PATH="$WORK/bin:$PATH" MEMOIRE_HOME="$WORK/home" \
    MEMOIRE_QUEUE_BIN="$WORK/queue-fail" MEMOIRE_PROJECT=proj MEMOIRE_PROJECTS_REGISTRY=/nonexistent "$HOOK" || fail "no-bus hook exited non-zero"
(( $(grep -c "^CALL " "$NOTIFY") == 0 )) || fail "no-bus sent a toast"

# 5. Successful write: no toast, nothing logged as a failure.
: > "$NOTIFY"; : > "$LOG"
fire "$WORK/queue-ok" || fail "ok hook exited non-zero"
(( $(grep -c "^CALL " "$NOTIFY") == 0 )) || fail "success sent a toast"
grep -q 'queue write failed' "$LOG" && fail "success logged a failure"

echo "claude-hook-queue-failure-test: OK"
