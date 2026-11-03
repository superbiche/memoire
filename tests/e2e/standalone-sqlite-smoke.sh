#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# standalone-sqlite-smoke.sh — the ADOPTION-COLUMN guard (memoire:D165, Q4
# ruling: "constraint now, artifact later"). Boots the full standalone-SQLite
# column — API + in-process drainer + store on a scratch home — under a
# SCRUBBED environment (env -i: zero PG, zero credstore, zero ambient
# config) and exercises one capture -> drain -> search loop plus the derive
# notify surface. If anyone introduces a hard dependency that breaks "one
# binary, one MEMOIRE_HOME, zero external services", this fails the default
# test path.
#
# Deliberately NOT set: MEMOIRE_DB (the SQLite default under MEMOIRE_HOME is
# the adoption contract), any db.env, any credential store.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/memoire-standalone-smoke.XXXXXX")"
HOME_DIR="$WORK/home"
mkdir -p "$HOME_DIR"
PID=""

STUB_PID=""

cleanup() {
  for p in "${PID:-}" "${STUB_PID:-}"; do
    if [[ -n "$p" ]] && kill -0 "$p" 2>/dev/null; then
      kill "$p" 2>/dev/null || true
      wait "$p" 2>/dev/null || true
    fi
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $1" >&2
  cat "$WORK/server.log" >&2 2>/dev/null || true
  exit 1
}

# Self-building (go build is cached): the smoke must run from `make test`
# without a prior `make build`. Built with the SAME version stamp as the
# Makefile/Containerfile (MISSING #7) so the health assertion below proves
# the stamped binary reports the repo's VERSION.
REPO_VERSION="$(head -1 "$ROOT/VERSION")"
go build -tags sqlite_fts5 \
  -ldflags "-X memoire/services/api/internal/buildinfo.Version=$REPO_VERSION" \
  -o "$WORK/memoire-api" "$ROOT/services/api/cmd/api" \
  || fail "standalone build"

# env -i is the point: the process sees ONLY these variables. A hard
# dependency on ambient PG DSNs, credstores, or operator config surfaces
# here as a boot failure. Port hermeticity (round-1 LOW-1): walk a small
# port range so concurrent `make test` runs (worktrees) or an unrelated
# listener never fail the default path; a boot death on one port retries
# the next, a death on the LAST port is reported.
BASE=""
for PORT in $(seq "${MEMOIRE_STANDALONE_PORT:-37781}" "$(( ${MEMOIRE_STANDALONE_PORT:-37781} + 20 ))"); do
  env -i PATH="$PATH" HOME="$HOME_DIR" \
    MEMOIRE_ADDR="127.0.0.1:$PORT" \
    MEMOIRE_HOME="$HOME_DIR" \
    MEMOIRE_SCHEMA_DIR="$ROOT/schema" \
    "$WORK/memoire-api" > "$WORK/server.log" 2>&1 &
  PID="$!"
  for _ in $(seq 1 50); do
    if curl -fsS "http://127.0.0.1:$PORT/v1/health" >/dev/null 2>&1; then
      # OWNERSHIP check (round-2 LOW-1): a health answer alone can come
      # from ANOTHER process that won the bind (concurrent worktree
      # smokes). Ours must still be alive AND accept our scratch token.
      if kill -0 "$PID" 2>/dev/null \
        && OWN_TOKEN="$(tr -d '\r\n' < "$HOME_DIR/token" 2>/dev/null)" \
        && [[ -n "$OWN_TOKEN" ]] \
        && curl -fsS -H "Authorization: Bearer $OWN_TOKEN" \
             "http://127.0.0.1:$PORT/v1/stats" >/dev/null 2>&1; then
        BASE="http://127.0.0.1:$PORT/v1"
      fi
      break
    fi
    kill -0 "$PID" 2>/dev/null || break
    sleep 0.1
  done
  [[ -n "$BASE" ]] && break
  # Bind loss, boot death, or a foreign listener: reap and try the next port.
  kill "$PID" 2>/dev/null || true
  wait "$PID" 2>/dev/null || true
  PID=""
done
[[ -n "$BASE" ]] || fail "API never became ready (owned) on any probe port"

HEALTH="$(curl -fsS "$BASE/health")"
grep -q '"search_engine":"fts5"' <<<"$HEALTH" \
  || fail "standalone column must run the SQLite engine, got: $HEALTH"
# The version regression (MISSING #7): health must report what the binary
# was built from — the repo's VERSION file, never a source literal.
grep -q "\"version\":\"$REPO_VERSION\"" <<<"$HEALTH" \
  || fail "health version must equal VERSION ($REPO_VERSION), got: $HEALTH"
# Readiness on a healthy standalone column must actually pass its probes
# (MISSING #8): store round-trip + queue writability.
READY="$(curl -fsS "$BASE/readiness")" \
  || fail "readiness must return 200 on a healthy column"
grep -q '"status":"ready"' <<<"$READY" \
  || fail "readiness must report ready, got: $READY"
grep -q '"store":"ok"' <<<"$READY" && grep -q '"queue":"ok"' <<<"$READY" \
  || fail "readiness must carry per-check body, got: $READY"

TOKEN="$(tr -d '\r\n' < "$HOME_DIR/token")" || fail "API must mint its own token"
AUTH=(-H "Authorization: Bearer $TOKEN")

# One full capture -> drain -> search loop.
curl -fsS "${AUTH[@]}" -H 'Content-Type: application/json' -X POST "$BASE/events" -d '{
  "schema_version":1, "project":"standalone", "session_id":"sa-1",
  "source_adapter":"smoke", "source_event_id":"sa-evt-1",
  "event_type":"claude_UserPromptSubmit",
  "payload":{"prompt":"the standalone axolotl boots alone"},
  "text":"the standalone axolotl boots alone"
}' | grep -q '"queued":true' || fail "event not queue-accepted"

for _ in $(seq 1 50); do
  HITS="$(curl -fsS "${AUTH[@]}" "$BASE/search?q=axolotl&project=standalone" 2>/dev/null)" || HITS=""
  grep -q "standalone axolotl" <<<"$HITS" && break
  sleep 0.2
done
grep -q "standalone axolotl" <<<"$HITS" || fail "capture->drain->search loop did not close"

# Derive notify surface works standalone (idempotency included).
BODY='{"project":"standalone","session_id":"sa-1"}'
curl -fsS "${AUTH[@]}" -H 'Content-Type: application/json' -X POST "$BASE/derive/notify" -d "$BODY" \
  | grep -q '"notify_count":1' || fail "first notify"
curl -fsS "${AUTH[@]}" -H 'Content-Type: application/json' -X POST "$BASE/derive/notify" -d "$BODY" \
  | grep -q '"notify_count":2' || fail "notify idempotency"
curl -fsS "${AUTH[@]}" "$BASE/derive/outstanding" \
  | grep -q '"outstanding"' || fail "outstanding surface"

# --- BYOK derive legs (docs/BYOK-DERIVE-DESIGN.md §5; memoire:D232) ---------
# The standalone lane's derivation is a user-provided endpoint: capture ->
# byok derive against a local stub speaking EACH ruled dialect -> proposed
# rows land with byok provenance. No network, no real keys (the stub key is
# a placeholder both sides agree on). This leg is what turns the README
# matrix cell "derivation: model endpoint you provide" into a claim.
go build -tags sqlite_fts5 -o "$WORK/memoire-derive" "$ROOT/services/api/cmd/derive" \
  || fail "derive build"
# The stub is its own stdlib-only module (deliberately outside go.work: it
# is a test fixture, not part of the shipped services); GOWORK=off scopes
# the build to it.
(cd "$ROOT/tests/e2e/byokstub" && GOWORK=off go build -o "$WORK/byokstub" .) \
  || fail "byok stub build"

# The acceptance claim is about PERSISTED provenance (review r1 F5), so the
# legs read the run row and observation row back from the store rather than
# trusting the CLI's echo of its own inputs.
command -v sqlite3 >/dev/null 2>&1 || fail "sqlite3 is required for the byok provenance readback"

"$WORK/byokstub" > "$WORK/stub.out" 2>&1 &
STUB_PID="$!"
STUB_ADDR=""
for _ in $(seq 1 50); do
  STUB_ADDR="$(awk '/^LISTEN /{print $2; exit}' "$WORK/stub.out" 2>/dev/null || true)"
  [[ -n "$STUB_ADDR" ]] && break
  kill -0 "$STUB_PID" 2>/dev/null || fail "byok stub died before listening"
  sleep 0.1
done
[[ -n "$STUB_ADDR" ]] || fail "byok stub never reported its address"

for DIALECT in openai-v1 anthropic; do
  SESSION="sa-byok-$DIALECT"
  curl -fsS "${AUTH[@]}" -H 'Content-Type: application/json' -X POST "$BASE/events" -d '{
    "schema_version":1, "project":"standalone", "session_id":"'"$SESSION"'",
    "source_adapter":"smoke", "source_event_id":"'"$SESSION"'-evt-1",
    "event_type":"claude_UserPromptSubmit",
    "payload":{"prompt":"derive this window through the '"$DIALECT"' quokka endpoint"},
    "text":"derive this window through the '"$DIALECT"' quokka endpoint"
  }' | grep -q '"queued":true' || fail "byok $DIALECT event not queue-accepted"

  DRAINED=""
  for _ in $(seq 1 50); do
    HITS="$(curl -fsS "${AUTH[@]}" "$BASE/search?q=quokka&project=standalone&session_id=$SESSION" 2>/dev/null)" || HITS=""
    grep -q "$DIALECT quokka" <<<"$HITS" && { DRAINED=1; break; }
    sleep 0.2
  done
  [[ -n "$DRAINED" ]] || fail "byok $DIALECT capture never drained"

  # Same scrubbed-environment discipline as the API boot: the derive CLI
  # sees ONLY the byok config surface. -no-min-content-guard: the leg's
  # window is deliberately one small event; transport is what is under test.
  env -i PATH="$PATH" HOME="$HOME_DIR" \
    MEMOIRE_HOME="$HOME_DIR" \
    MEMOIRE_SCHEMA_DIR="$ROOT/schema" \
    MEMOIRE_BYOK_BASE_URL="http://$STUB_ADDR" \
    MEMOIRE_BYOK_DIALECT="$DIALECT" \
    MEMOIRE_BYOK_MODEL="stub-model" \
    MEMOIRE_BYOK_DATA_BOUNDARY="local" \
    MEMOIRE_BYOK_API_KEY_ENV="BYOK_SMOKE_KEY" \
    BYOK_SMOKE_KEY="byok-stub-placeholder-key" \
    "$WORK/memoire-derive" -strategy=model -provider=byok -model=stub-model \
    -execute -no-min-content-guard -project=standalone -session-id="$SESSION" -json \
    > "$WORK/derive-$DIALECT.json" 2>"$WORK/derive-$DIALECT.err" \
    || { cat "$WORK/derive-$DIALECT.err" >&2; fail "byok $DIALECT derive invocation failed"; }

  grep -q '"status": "completed"' "$WORK/derive-$DIALECT.json" \
    || fail "byok $DIALECT run not completed: $(cat "$WORK/derive-$DIALECT.json")"
  grep -q '"written_count": 1' "$WORK/derive-$DIALECT.json" \
    || fail "byok $DIALECT run wrote no observation: $(cat "$WORK/derive-$DIALECT.json")"

  # Persisted provenance, read from the STORE (review r1 F5): the run row
  # must carry provider/boundary/model/endpoint and the terminal meta_json
  # dialect; the observation row must link to the run and land proposed.
  RUN_ID="$(sed -n 's/.*"run_id": "\([^"]*\)".*/\1/p' "$WORK/derive-$DIALECT.json")"
  [[ -n "$RUN_ID" ]] || fail "byok $DIALECT run id missing from CLI output"
  RUN_ROW="$(sqlite3 "$HOME_DIR/memoire.db" "SELECT provider||'|'||data_boundary||'|'||model||'|'||endpoint_id||'|'||json_extract(meta_json,'\$.dialect')||'|'||status FROM derivation_runs WHERE id='$RUN_ID';")"
  [[ "$RUN_ROW" == "byok|local|stub-model|byok|$DIALECT|completed" ]] \
    || fail "byok $DIALECT persisted run provenance wrong: $RUN_ROW"
  OBS_COUNT="$(sqlite3 "$HOME_DIR/memoire.db" "SELECT count(*) FROM observations WHERE derivation_run_id='$RUN_ID' AND review_state='proposed' AND provenance='inferred' AND session_id='$SESSION';")"
  [[ "$OBS_COUNT" == 1 ]] \
    || fail "byok $DIALECT expected exactly 1 proposed inferred observation linked to $RUN_ID, got $OBS_COUNT"

  OBS="$(curl -fsS "${AUTH[@]}" "$BASE/observations?project=standalone&session_id=$SESSION&review_state=proposed&include_derived=true")"
  grep -q "the byok $DIALECT stub answered" <<<"$OBS" \
    || fail "byok $DIALECT observation not visible: $OBS"
done

echo "standalone-sqlite-smoke: OK"
