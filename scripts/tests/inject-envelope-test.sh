#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# inject-envelope-test.sh — regression tests for memoire-inject-envelope.sh
# (qwen SessionStart JSON envelope, 2026-07-12). Pins the orthogonal-review
# repairs: large payloads must survive the jq handoff (stdin transport, not
# argv — a single argv entry caps at ~128 KiB on Linux), the payload must be
# wrapped byte-identical (trailing newline preserved), empty inner output must
# emit nothing, and a hung inner script must be bounded by the wrapper
# deadline. The wrapper resolves its sibling inject script by its own
# location, so each case copies the wrapper next to a stub inner script.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WRAPPER="$ROOT/memoire-inject-envelope.sh"
command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }
# The wrapper hard-requires coreutils timeout by design (no timeout -> it
# suppresses injection rather than run unbounded), so every case needs it.
command -v timeout >/dev/null || { echo "SKIP: timeout not available"; exit 0; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

stage() { # stage <dir> — copy wrapper into a fresh dir, return its path
  mkdir -p "$WORK/$1"
  cp "$WRAPPER" "$WORK/$1/memoire-inject-envelope.sh"
  chmod +x "$WORK/$1/memoire-inject-envelope.sh"
}

# --- 1. large payload (> MAX_ARG_STRLEN) survives, byte-identical ------------
stage large
{ printf '# [memoire] recent context — big\n'
  yes -- '- [0] fact quote " backslash \ café 🙂 line' | head -20000 || true; } > "$WORK/large/payload"
PAYLOAD_BYTES="$(wc -c < "$WORK/large/payload")"
(( PAYLOAD_BYTES > 200000 )) || fail "large fixture too small ($PAYLOAD_BYTES bytes)"
cat > "$WORK/large/memoire-inject-claude.sh" <<STUB
#!/usr/bin/env bash
cat "$WORK/large/payload"
STUB
chmod +x "$WORK/large/memoire-inject-claude.sh"

OUT_FILE="$WORK/large/out.json"
printf '{"cwd":"/tmp/x"}' | "$WORK/large/memoire-inject-envelope.sh" > "$OUT_FILE" \
  || fail "large: wrapper exited nonzero"
[[ -s "$OUT_FILE" ]] || fail "large: wrapper emitted nothing for a valid $PAYLOAD_BYTES-byte payload"
[[ "$(wc -l < "$OUT_FILE")" == "1" ]] || fail "large: envelope is not a single line"
jq -e '(.hookSpecificOutput.hookEventName == "SessionStart")' "$OUT_FILE" >/dev/null \
  || fail "large: envelope shape wrong"
jq -j '.hookSpecificOutput.additionalContext' "$OUT_FILE" > "$WORK/large/roundtrip"
cmp -s "$WORK/large/payload" "$WORK/large/roundtrip" \
  || fail "large: additionalContext is not byte-identical to inner stdout"

# --- 2. empty inner output → no envelope, exit 0 -----------------------------
stage empty
printf '#!/usr/bin/env bash\nexit 0\n' > "$WORK/empty/memoire-inject-claude.sh"
chmod +x "$WORK/empty/memoire-inject-claude.sh"
OUT="$(printf '{"cwd":"/tmp/x"}' | "$WORK/empty/memoire-inject-envelope.sh")" \
  || fail "empty: wrapper exited nonzero"
[[ -z "$OUT" ]] || fail "empty: wrapper emitted output for empty inner stdout"

# --- 3. TERM-ignoring hung inner script is still killed (-k escalation) ------
stage hang
printf '#!/usr/bin/env bash\ntrap "" TERM\nsleep 30\n' > "$WORK/hang/memoire-inject-claude.sh"
chmod +x "$WORK/hang/memoire-inject-claude.sh"
START="$(date +%s)"
OUT="$(printf '{"cwd":"/tmp/x"}' | MEMOIRE_INJECT_WRAPPER_TIMEOUT=1 "$WORK/hang/memoire-inject-envelope.sh")" \
  || fail "hang: wrapper exited nonzero"
ELAPSED=$(( $(date +%s) - START ))
[[ -z "$OUT" ]] || fail "hang: wrapper emitted output from a killed inner script"
(( ELAPSED < 10 )) || fail "hang: wrapper took ${ELAPSED}s, TERM-ignoring inner not killed"

# --- 4. invalid deadline (0 disables GNU timeout) falls back to the default --
stage junk
printf '#!/usr/bin/env bash\nsleep 30\n' > "$WORK/junk/memoire-inject-claude.sh"
chmod +x "$WORK/junk/memoire-inject-claude.sh"
START="$(date +%s)"
OUT="$(printf '{"cwd":"/tmp/x"}' | MEMOIRE_INJECT_WRAPPER_TIMEOUT=0 "$WORK/junk/memoire-inject-envelope.sh")" \
  || fail "junk: wrapper exited nonzero"
ELAPSED=$(( $(date +%s) - START ))
[[ -z "$OUT" ]] || fail "junk: wrapper emitted output from a killed inner script"
(( ELAPSED < 20 )) || fail "junk: deadline=0 not clamped, inner ran ${ELAPSED}s"

echo "inject-envelope-test: OK"
