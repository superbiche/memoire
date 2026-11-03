#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# dist-contract-test.sh — value-level contract on the standalone release
# artifact (R-10.5). Runs via `make dist-check` (needs `make dist` first);
# NOT in the default `make test` path — building the full dist per test
# run is the release lane's cost, not the dev loop's.
#
# Proves, from the artifact alone (round-1 findings 1, 3, 7 folded in):
#   1. SHA256SUMS has exactly one entry, naming this tarball, and verifies.
#   2. The archive matches an EXACT manifest — an added ops file, fleet
#      artifact, or dropped payload file fails (allowlist, not denylist).
#   3. No fleet-private topology leaks into shipped text (RFC1918
#      addresses, personal home and checkout paths) — one documented
#      exemption: the plugins' fleet-default inject path, which
#      MEMOIRE_INJECT_SCRIPT overrides for tarball installs.
#   4. The packaged memoire-api initializes a fresh MEMOIRE_HOME from an
#      EMPTY cwd under a SCRUBBED environment (exe-relative schema,
#      migration, token mint) — the zero-config standalone claim.
#   5. Ambient engine config (MEMOIRE_DB) makes bootstrap REFUSE — the
#      standalone lane can never write an external store.
#   6. A healthy-but-foreign API on the target address makes init FAIL —
#      health without token ownership is not success.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
VERSION="$(head -1 "$ROOT/VERSION")"
PLATFORM="$(go env GOOS)-$(go env GOARCH)"
NAME="memoire-$VERSION-$PLATFORM"
TARBALL="$ROOT/dist/$NAME.tar.gz"

fail() { echo "FAIL: $1" >&2; exit 1; }
[[ -f "$TARBALL" ]] || fail "tarball missing: $TARBALL (run make dist first)"

WORK="$(mktemp -d)"
PIDS=()
cleanup() {
  local p
  for p in "${PIDS[@]:-}"; do
    kill "$p" 2>/dev/null || true
    for ((i = 0; i < 25; i++)); do
      kill -0 "$p" 2>/dev/null || break
      sleep 0.2
    done
    kill -9 "$p" 2>/dev/null || true
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

# 1. Checksum file: exactly one entry, naming this tarball, verifying.
[[ "$(wc -l < "$ROOT/dist/SHA256SUMS")" -eq 1 ]] \
  || fail "SHA256SUMS must contain exactly one entry"
grep -q " $NAME.tar.gz\$" "$ROOT/dist/SHA256SUMS" \
  || fail "SHA256SUMS entry does not name $NAME.tar.gz"
(cd "$ROOT/dist" && sha256sum -c SHA256SUMS >/dev/null) \
  || fail "SHA256SUMS does not verify the tarball"

# 2. Exact manifest. Top-level payload, bins, and scripts are pinned
# literally; schema trees ship whole, so their expectation derives from
# the tracked repo state.
MANIFEST="$WORK/expected.txt"
{
  printf '%s\n' \
    "LICENSE" "QUICKSTART.md" "VERSION" "memoire-bootstrap.sh" \
    "model-endpoints.example.json" "derive-denylist.example.json" \
    "bin/memoire-api" "bin/memoire-queue" "bin/memoire-mint-token" \
    "bin/memoire-mcp" "bin/memoire-derive" "bin/memoire-forward" \
    "bin/memoire-purge" "bin/memoire-redact-derived" \
    "scripts/memoire-capture-claude.sh" \
    "scripts/memoire-capture-claude-transcripts.sh" \
    "scripts/memoire-capture-codex.sh" \
    "scripts/memoire-capture-codex-rollout.sh" \
    "scripts/memoire-capture-opencode.sh" \
    "scripts/memoire-capture-qwen.sh" \
    "scripts/memoire-capture-qwen-rollout.sh" \
    "scripts/memoire-capture-text.sh" \
    "scripts/memoire-inject-claude.sh" \
    "scripts/memoire-inject-envelope.sh" \
    "scripts/memoire-resolve-project.sh" \
    "scripts/check-health.sh" \
    "scripts/opencode-plugin/memoire.js" \
    "scripts/pi-extension/memoire.js"
  git -C "$ROOT" ls-files schema/ schema-pg/
} | sort > "$MANIFEST"
tar -tzf "$TARBALL" | sed "s|^$NAME/||" | sed '\|/$|d' | sed '/^$/d' \
  | sort > "$WORK/actual.txt"
diff -u "$MANIFEST" "$WORK/actual.txt" >&2 \
  || fail "archive contents deviate from the exact manifest (see diff above)"

# Extract for the remaining checks.
tar -C "$WORK" -xzf "$TARBALL"
DIST="$WORK/$NAME"

# 3a. Fleet-privacy + secret-shape scan over EVERY shipped text file —
# via the SHARED export scanner (curated-export review r1 F6: the dist
# gate carried a weaker forked predicate; the two public release
# artifacts must answer the same security question). Patterns = the
# export's generic set + the canonical sensitive set (required: the dist
# lane is home-side release tooling). The plugins' fleet-default lines are
# the same exact-once line exemptions the export carries.
LEAKS="$WORK/leaks.txt"
DIST_EXEMPTIONS="$WORK/dist-exemptions.txt"
printf 'line\tscripts/opencode-plugin/memoire.js\t%s\n' \
  '    || `${process.env.HOME}/dev/personal/memoire/scripts/memoire-inject-claude.sh`;' \
  > "$DIST_EXEMPTIONS"
printf 'line\tscripts/pi-extension/memoire.js\t%s\n' \
  '  || `${process.env.HOME}/dev/personal/memoire/scripts/memoire-inject-claude.sh`;' \
  >> "$DIST_EXEMPTIONS"
bash "$ROOT/scripts/leak/leak-scan.sh" \
  --tree "$DIST" --report "$LEAKS" --require-sensitive \
  --generic "$ROOT/scripts/leak/leak-patterns-generic.txt" \
  --exemptions "$DIST_EXEMPTIONS" \
  || { cat "$LEAKS" >&2 2>/dev/null; fail "fleet-private or secret-shaped value in shipped text (leak-scan refused)"; }

# 3b. Executable modes: every shipped program must be runnable as
# extracted (round-2 finding on names-only manifest).
while IFS= read -r f; do
  [[ -x "$f" ]] || fail "shipped program not executable: ${f#"$DIST"/}"
done < <(find "$DIST/bin" -type f; find "$DIST/scripts" -name '*.sh' -type f; echo "$DIST/memoire-bootstrap.sh")

# 3c. Behavioral plugin pin: run the plugin unit suite against the
# EXTRACTED copy — the override assertion in that suite fails if
# MEMOIRE_INJECT_SCRIPT stops selecting the executed script.
command -v node >/dev/null || fail "node required by the release lane (plugin behavioral check)"
MEMOIRE_PLUGIN_PATH="$DIST/scripts/opencode-plugin/memoire.js" \
  node "$ROOT/scripts/tests/opencode-plugin-test.mjs" >/dev/null \
  || fail "extracted opencode plugin failed its behavioral suite"
MEMOIRE_PI_EXTENSION_PATH="$DIST/scripts/pi-extension/memoire.js" \
  node "$ROOT/scripts/tests/pi-extension-test.mjs" >/dev/null \
  || fail "extracted Pi extension failed its behavioral suite"

head -3 "$DIST/scripts/memoire-capture-claude.sh" | grep -q "SPDX-License-Identifier: AGPL-3.0-only" \
  || fail "shipped script lost its SPDX header"
"$DIST/memoire-bootstrap.sh" check >/dev/null \
  || fail "bootstrap check failed on extracted layout"

# 4. Zero-config init: empty cwd, scrubbed environment, scratch home.
EMPTY="$WORK/empty-cwd"
HOME_DIR="$WORK/memoire-home"
FAKE_HOME="$WORK/fake-user-home"
mkdir -p "$EMPTY" "$FAKE_HOME"
(cd "$EMPTY" && env -i PATH="$PATH" HOME="$FAKE_HOME" \
  MEMOIRE_HOME="$HOME_DIR" MEMOIRE_ADDR="127.0.0.1:37877" \
  "$DIST/memoire-bootstrap.sh" init >/dev/null) \
  || fail "bootstrap init failed from empty cwd under scrubbed env"
[[ -f "$HOME_DIR/token" ]] || fail "token not minted at $HOME_DIR/token"
[[ -s "$HOME_DIR/memoire.db" ]] || fail "memoire.db not created/migrated"

# 5. Ambient engine config must be refused, not honored.
if (cd "$EMPTY" && env -i PATH="$PATH" HOME="$FAKE_HOME" \
  MEMOIRE_HOME="$WORK/other-home" MEMOIRE_ADDR="127.0.0.1:37878" \
  MEMOIRE_DB="postgres://nope.invalid/db" \
  "$DIST/memoire-bootstrap.sh" init >/dev/null 2>&1); then
  fail "bootstrap init accepted ambient MEMOIRE_DB (must refuse)"
fi

# 6. Healthy-but-foreign API: owner A on the port, init for home B must
# fail rather than claim success off a foreign health response. The
# fixture PROVES itself first (round-2 new finding 2): owner alive,
# healthy, and authenticating with its own token — otherwise the negative
# assertion below could pass for the wrong reason.
OWNER_HOME="$WORK/owner-home"
env -i PATH="$PATH" HOME="$FAKE_HOME" MEMOIRE_HOME="$OWNER_HOME" \
  MEMOIRE_ADDR="127.0.0.1:37879" "$DIST/bin/memoire-api" \
  >/dev/null 2>&1 &
OWNER_PID=$!
PIDS+=("$OWNER_PID")
owner_ready=0
for ((i = 0; i < 50; i++)); do
  kill -0 "$OWNER_PID" 2>/dev/null || fail "owner API fixture exited early"
  if [[ -f "$OWNER_HOME/token" ]] \
    && curl -fsS --max-time 2 -H "Authorization: Bearer $(cat "$OWNER_HOME/token")" \
      http://127.0.0.1:37879/v1/stats >/dev/null 2>&1; then
    owner_ready=1
    break
  fi
  sleep 0.2
done
[[ "$owner_ready" -eq 1 ]] || fail "owner API fixture never became healthy+authenticated"
if (cd "$EMPTY" && env -i PATH="$PATH" HOME="$FAKE_HOME" \
  MEMOIRE_HOME="$WORK/intruder-home" MEMOIRE_ADDR="127.0.0.1:37879" \
  "$DIST/memoire-bootstrap.sh" init >/dev/null 2>&1); then
  fail "bootstrap init claimed success against a foreign-owned API"
fi

echo "dist-contract-test: OK"
