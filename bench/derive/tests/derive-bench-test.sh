#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# derive-bench-test.sh — the derive bench's own gate (docs/DERIVE-BENCH-DESIGN.md).
# test-ops lane (`make test`): the bench is operated against the user's store.
#
# It exercises the slice-1 pieces against REAL surfaces end to end:
#
#   1. PARITY: the bench's canonical input event types equal the derivation
#      set in services/api/internal/modelderive/builder.go — a type added
#      there fails here instead of silently freezing a partial window.
#   2. FREEZER: a scratch memoire column stands in for the user's store; the
#      freezer pulls a session out of it through the read surface and the
#      fixture is asserted against what was captured.
#   3. ROUND TRIP: every fixture event's replay envelope reproduces the type,
#      epoch, payload and text the fixture froze.
#   4. RUN DRIVER: a throwaway home is seeded from that fixture and derived
#      through the real seam against a local OpenAI-compatible stub, in BOTH
#      postures — the default production min-content guard (this thin window
#      is skipped before any provider call) and the abstention-probe opt-out
#      that reaches the model. The per-run JSON is asserted: genre, replay
#      proof, grounded observations, and the run provenance (the leg vector)
#      the pack builder and scorecard read.
#   4c/4d. WINDOW SPLIT: the port of the production sweep's adaptive split,
#      driven through the real CLI. A ceiling derived from the measured
#      whole-window size forces a split; asserted are leg count, exact
#      partition of the parent window (where a median-on-hi off-by-one would
#      show), oldest-first order, cross-run determinism, aggregation, and the
#      depth cap refusing a window with NO provider request.
#   4f. RHYTHM CUTS: the port of the sweep's rhythm-first windowing
#      (memoire:D0261) and the provisional open tail (memoire:D0262), driven
#      through the real -cut-points helper. A session with one long pause must
#      cut there FIRST, halve only inside a segment that still exceeds the
#      ceiling, mark the trailing leg provisional in the leg record AND in the
#      run row the binary wrote, reproduce its cuts, and fail open — status
#      "failed", whole-session provisional leg — when the detector refuses.
#   5. NEAR-DUP PARITY: the bench's Python port of the production near-dup
#      gate is asserted against the golden vectors that
#      modelderive's neardup_golden_test.go asserts too.
#   6. PACK / BALLOT / SCORECARD: synthetic incumbent baselines and candidate
#      results (deterministic content, so the alignment outcome is knowable)
#      go through pack.py, hand-filled ballots, and scorecard.py. Asserted:
#      blinding (no whitelisted-out field reaches a judge-visible file, side
#      nonces differ across sessions), EQUIVALENT collapse, the sanity-probe
#      flag, and the malformed-ballot refusal.
#
# No network: the source column, the throwaway column, and the model endpoint
# all bind 127.0.0.1, and the stub key is a placeholder both sides agree on.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
BENCH="$ROOT/bench/derive"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/memoire-derive-bench.XXXXXX")"
SRC_HOME="$WORK/source-home"
API_PID=""
STUB_PID=""
REFUSE_PID=""

cleanup() {
  for p in "${API_PID:-}" "${STUB_PID:-}" "${REFUSE_PID:-}"; do
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
  [[ -f "$WORK/source-api.log" ]] && tail -20 "$WORK/source-api.log" >&2
  exit 1
}

command -v python3 >/dev/null 2>&1 || fail "python3 is required by the derive bench"

# --- 1. canonical input event-type parity -----------------------------------
awk '/^var canonicalInputEventTypes/,/^}/' \
  "$ROOT/services/api/internal/modelderive/builder.go" \
  | grep -oE '"[A-Za-z_]+":' | tr -d '":' | LC_ALL=C sort > "$WORK/go-types.txt"
[[ -s "$WORK/go-types.txt" ]] || fail "could not read canonicalInputEventTypes from builder.go"
python3 - "$BENCH" > "$WORK/bench-types.txt" <<'PY'
import sys
sys.path.insert(0, sys.argv[1])
import benchlib
print("\n".join(sorted(benchlib.CANONICAL_INPUT_EVENT_TYPES)))
PY
diff -u "$WORK/go-types.txt" "$WORK/bench-types.txt" >/dev/null \
  || { diff -u "$WORK/go-types.txt" "$WORK/bench-types.txt" >&2; \
       fail "bench canonical input event types drifted from builder.go"; }

# --- 1b. model-provider parity ----------------------------------------------
# benchlib.MODEL_PROVIDERS decides which historical derivation runs count as
# the incumbent's answer. A provider added to the Go switch but not here would
# not fail anything — it would silently shrink the baseline, and the bench
# would compare a candidate against less than the incumbent actually produced.
#
# The case list mixes quoted literals with package constants
# (`modelderive.ProviderBYOK`), so the extractor resolves constant references
# to their literals and REFUSES rather than skipping one it cannot resolve: an
# assertion that silently under-counts is worse than no assertion at all.
python3 - "$ROOT" "$BENCH" <<'PY' || fail "bench model providers drifted from isModelProvider"
import glob, re, sys

root, bench = sys.argv[1], sys.argv[2]
sys.path.insert(0, bench)
import benchlib

source = open(f"{root}/services/api/cmd/derive/main.go", encoding="utf-8").read()
body = re.search(r"func isModelProvider\(provider string\) bool \{(.*?)\n\}", source, re.S)
if not body:
    sys.exit("could not locate isModelProvider in services/api/cmd/derive/main.go")
cases = re.search(r"\bcase\s+(.+?):", body.group(1), re.S)
if not cases:
    sys.exit("isModelProvider carries no case list")

package = "\n".join(
    open(path, encoding="utf-8").read()
    for path in sorted(glob.glob(f"{root}/services/api/internal/modelderive/*.go"))
)

providers = set()
for term in (t.strip() for t in cases.group(1).split(",")):
    literal = re.fullmatch(r'"([^"]+)"', term)
    if literal:
        providers.add(literal.group(1))
        continue
    reference = re.fullmatch(r"(?:\w+\.)?(\w+)", term)
    if not reference:
        sys.exit(f"unparsable term in the isModelProvider case list: {term!r}")
    name = reference.group(1)
    const = re.search(rf"\b{re.escape(name)}\s*=\s*\"([^\"]+)\"", package)
    if not const:
        sys.exit(
            f"the isModelProvider case list names {term!r}, which resolves to no "
            "string constant in internal/modelderive — resolve it here rather "
            "than letting this assertion quietly drop a provider"
        )
    providers.add(const.group(1))

bench_side = set(benchlib.MODEL_PROVIDERS)
if providers != bench_side:
    sys.exit(
        "MODEL_PROVIDERS drift:\n"
        f"  isModelProvider: {sorted(providers)}\n"
        f"  benchlib:        {sorted(bench_side)}\n"
        f"  only in Go:      {sorted(providers - bench_side)}\n"
        f"  only in bench:   {sorted(bench_side - providers)}"
    )
print(f"model-provider parity: OK ({sorted(providers)})")
PY

# --- 1c. window-split constant parity ---------------------------------------
# run.py ports the sweep's adaptive window split, and the incumbent's
# observations came through the SWEEP's constants. A ceiling or depth cap that
# drifted from the script would silently bench a different windowing than
# production runs — the comparison's whole premise.
python3 - "$ROOT" "$BENCH" <<'PY' || fail "window-split constants drifted from the sweep script"
import re, sys

root, bench = sys.argv[1], sys.argv[2]
script = open(f"{root}/scripts/memoire-derive-sweep.sh", encoding="utf-8").read()
driver = open(f"{bench}/run.py", encoding="utf-8").read()

wanted = {}
for name, var in (("max_prompt_bytes", "MEMOIRE_SWEEP_MAX_PROMPT_BYTES"),
                  ("split_depth", "MEMOIRE_SWEEP_MAX_SPLIT_DEPTH")):
    m = re.search(rf'\$\{{{var}:-(\d+)\}}', script)
    if not m:
        sys.exit(f"could not read {var}'s default from memoire-derive-sweep.sh")
    wanted[name] = int(m.group(1))

# The ceiling is now resolved (flag > endpoint profile > fallback), so the
# pinned constant is run.py's FALLBACK rather than the flag's default. What the
# gate protects is unchanged: a bench that names no ceiling must run the
# sweep's.
m = re.search(r"^DEFAULT_MAX_PROMPT_BYTES = (\d+)", driver, re.M)
if not m:
    sys.exit("could not read run.py's DEFAULT_MAX_PROMPT_BYTES")
if int(m.group(1)) != wanted["max_prompt_bytes"]:
    sys.exit(f"DEFAULT_MAX_PROMPT_BYTES is {m.group(1)}, "
             f"sweep says {wanted['max_prompt_bytes']}")
if not re.search(r'add_argument\("--max-prompt-bytes", type=int, default=None',
                 driver):
    sys.exit("run.py's --max-prompt-bytes must default to None so an endpoint "
             "profile can supply the ceiling")

m = re.search(r'add_argument\("--split-depth", type=int, default=(\d+)', driver)
if not m:
    sys.exit("could not read run.py's --split-depth default")
if int(m.group(1)) != wanted["split_depth"]:
    sys.exit(f"--split-depth default is {m.group(1)}, "
             f"sweep says {wanted['split_depth']}")
print(f"window-split parity: OK (ceiling={wanted['max_prompt_bytes']}, "
      f"depth={wanted['split_depth']})")
PY

# --- 1d. derive-profile formula parity --------------------------------------
# The bench resolves its ceiling from an endpoint profile; memoire resolves the
# same ceiling from the same registry entry. Shared golden vectors, asserted on
# both sides (the Go half is TestDeriveProfileGoldenVectors in internal/config).
python3 "$BENCH/tests/profile-test.py" \
  || fail "the bench's derive-profile formula diverged from the golden vectors"

# --- 1e. sealed-kind parity for the specialist shim -------------------------
# The shim tells NuExtract which kinds exist (its template is the schema) and
# tells MemReader the same set in its system prompt. A kind added to the
# ontology but missing from the shim would be silently unextractable for every
# specialist candidate — the bench would measure the shim's blind spot and
# report it as the model's omission.
python3 - "$ROOT" "$BENCH" <<'PY' || fail "shim sealed kinds drifted from SealedKinds()"
import re, sys

root, bench = sys.argv[1], sys.argv[2]
sys.path.insert(0, bench)
import shim

source = open(f"{root}/services/api/internal/modelderive/types.go", encoding="utf-8").read()
body = re.search(r"func SealedKinds\(\) \[\]SealedKind \{(.*?)\n\}", source, re.S)
if not body:
    sys.exit("could not locate SealedKinds in internal/modelderive/types.go")
go_kinds = re.findall(r'\{Name:\s*"([a-z_]+)"', body.group(1))
if not go_kinds:
    sys.exit("SealedKinds carries no Name entries — refusing to assert nothing")

if list(shim.SEALED_KINDS) != go_kinds:
    sys.exit(
        "shim sealed-kind drift:\n"
        f"  SealedKinds(): {go_kinds}\n"
        f"  shim:          {list(shim.SEALED_KINDS)}"
    )
# The set must also actually reach both specialists' request shapes.
template = shim.NUEXTRACT_TEMPLATE["observations"][0]["kind"][0]
if template != go_kinds:
    sys.exit(f"the NuExtract template's kind enum drifted: {template}")
missing = [k for k in go_kinds if k not in shim.MEMREADER_SYSTEM]
if missing:
    sys.exit(f"MemReader's system prompt never names: {missing}")
print(f"shim sealed-kind parity: OK ({len(go_kinds)} kinds)")
PY

# --- 2. a scratch column standing in for the user's store -------------------
mkdir -p "$SRC_HOME"
go build -tags sqlite_fts5 -o "$WORK/memoire-api" "$ROOT/services/api/cmd/api" \
  || fail "source api build"

BASE=""
for PORT in $(seq 37861 37880); do
  env -i PATH="$PATH" HOME="$SRC_HOME" \
    MEMOIRE_ADDR="127.0.0.1:$PORT" \
    MEMOIRE_HOME="$SRC_HOME" \
    MEMOIRE_SCHEMA_DIR="$ROOT/schema" \
    "$WORK/memoire-api" > "$WORK/source-api.log" 2>&1 &
  API_PID="$!"
  for _ in $(seq 1 50); do
    if curl -fsS "http://127.0.0.1:$PORT/v1/health" >/dev/null 2>&1; then
      # Ownership: a health answer can come from a foreign listener that won
      # the bind (standalone-smoke rule) — ours must accept OUR minted token.
      if kill -0 "$API_PID" 2>/dev/null \
        && OWN_TOKEN="$(tr -d '\r\n' < "$SRC_HOME/token" 2>/dev/null)" \
        && [[ -n "$OWN_TOKEN" ]] \
        && curl -fsS -H "Authorization: Bearer $OWN_TOKEN" \
             "http://127.0.0.1:$PORT/v1/stats" >/dev/null 2>&1; then
        BASE="http://127.0.0.1:$PORT"
      fi
      break
    fi
    kill -0 "$API_PID" 2>/dev/null || break
    sleep 0.1
  done
  [[ -n "$BASE" ]] && break
  kill "$API_PID" 2>/dev/null || true
  wait "$API_PID" 2>/dev/null || true
  API_PID=""
done
[[ -n "$BASE" ]] || fail "source column never became ready (owned) on any probe port"

TOKEN="$(tr -d '\r\n' < "$SRC_HOME/token")"
AUTH=(-H "Authorization: Bearer $TOKEN")
SESSION="bench-src-1"
PROJECT="bench-src"

# capture takes the session and a uniquifier explicitly: a bash prefix
# assignment before a FUNCTION call does not reliably scope to that call, and
# two events sharing an epoch need distinct source ids and bodies or the queue
# writer's content hash collapses them into one file.
capture() { # capture <event_type> <epoch> <text> [session] [uniquifier]
  local session="${4:-$SESSION}" uniq="${5:-0}"
  curl -fsS "${AUTH[@]}" -H 'Content-Type: application/json' -X POST "$BASE/v1/events" -d '{
    "schema_version":1, "project":"'"$PROJECT"'", "session_id":"'"$session"'",
    "source_adapter":"bench-test", "source_event_id":"'"$1-$2-$uniq"'",
    "event_type":"'"$1"'", "occurred_at_epoch":'"$2"',
    "cwd":"/repo/bench",
    "payload":{"text":"'"$3"' (n'"$uniq"')"},
    "text":"'"$3"' (n'"$uniq"')"
  }' | grep -q '"queued":true' || fail "$1 not queue-accepted"
}
capture claude_UserPromptSubmit 1750000001 "the bench pangolin asks for a fixture"
capture claude_PostToolUse      1750000002 "the bench pangolin ran a tool"
capture claude_assistant_turn   1750000003 "the bench pangolin concluded the slice"

# A second session for the window-split legs. Its events carry BULK so the
# per-event content dominates the prompt template: a split ceiling derived from
# the whole-window size then leaves room for the leaves to fit, which is what
# makes the leg assertions about splitting rather than about boilerplate.
SPLIT_SESSION="bench-split-1"
BULK="$(python3 -c 'print("the pangolin measured the window and wrote it down " * 60, end="")')"
N=0
for epoch in 1750000101 1750000102 1750000103 1750000104 \
             1750000105 1750000106 1750000107 1750000108; do
  N=$((N + 1))
  capture claude_assistant_turn "$epoch" "$BULK" "$SPLIT_SESSION" "$N"
done

# A third session for the median-on-hi edge: the middle canonical epoch EQUALS
# the window's upper bound, so the split point must be pulled below it or the
# upper half comes back empty and its events are dropped or double-derived.
EDGE_SESSION="bench-edge-1"
capture claude_assistant_turn 1750000201 "$BULK" "$EDGE_SESSION" 1
capture claude_assistant_turn 1750000205 "$BULK" "$EDGE_SESSION" 2
capture claude_assistant_turn 1750000205 "$BULK" "$EDGE_SESSION" 3

# A fourth session for the RHYTHM cut (memoire:D0261): two bursts of bulk
# assistant turns separated by a pause far above the binary's default long-pause
# threshold (1800 s, cmd/derive/cutpoints.go defaultCutGapSeconds), and well
# under its 120-event count cap, so exactly ONE cut fires and it fires on the
# pause. BULK again, so a ceiling between one segment and the whole session
# exists to derive.
RHYTHM_SESSION="bench-rhythm-1"
N=0
for epoch in 1750000301 1750000302 1750000303 1750000304 \
             1750100301 1750100302 1750100303 1750100304; do
  N=$((N + 1))
  capture claude_assistant_turn "$epoch" "$BULK" "$RHYTHM_SESSION" "$N"
done

WANT_EVENTS=22
for _ in $(seq 1 80); do
  COUNT="$(curl -fsS "${AUTH[@]}" "$BASE/v1/stats" | python3 -c \
    'import json,sys; print(json.load(sys.stdin)["event_count"])' 2>/dev/null || echo 0)"
  [[ "$COUNT" == "$WANT_EVENTS" ]] && break
  sleep 0.2
done
[[ "$COUNT" == "$WANT_EVENTS" ]] || fail "source column drained $COUNT/$WANT_EVENTS events"

# --- 3. the freezer, against that column's read surface ---------------------
DATA="$WORK/data"
MEMOIRE_BASE_URL="$BASE" MEMOIRE_TOKEN_FILE="$SRC_HOME/token" \
  python3 "$BENCH/freeze.py" --set t1 --session "$SESSION" --data-dir "$DATA" \
  > "$WORK/freeze.log" 2>&1 || { cat "$WORK/freeze.log" >&2; fail "freeze.py failed"; }

FIXTURE="$DATA/t1/$SESSION.json"
[[ -f "$FIXTURE" ]] || fail "freezer wrote no fixture at $FIXTURE"

python3 - "$BENCH" "$FIXTURE" <<'PY' || fail "fixture assertions"
import json, sys
sys.path.insert(0, sys.argv[1])
import benchlib

fx = benchlib.load_fixture(sys.argv[2])
assert fx["project"] == "bench-src", fx["project"]
assert fx["counts"]["events"] == 3, fx["counts"]
types = [e["event_type"] for e in fx["events"]]
assert types == ["claude_UserPromptSubmit", "claude_PostToolUse",
                 "claude_assistant_turn"], types
epochs = [e["occurred_at_epoch"] for e in fx["events"]]
assert epochs == sorted(epochs) == [1750000001, 1750000002, 1750000003], epochs
assert "pangolin" in fx["events"][0]["text"], fx["events"][0]

# 3b. replay round trip: the envelope reproduces every field derivation reads.
for event in fx["events"]:
    envelope = benchlib.replay_envelope(event)
    assert envelope["event_type"] == event["event_type"]
    assert envelope["occurred_at_epoch"] == event["occurred_at_epoch"]
    assert envelope["project"] == event["project"]
    assert envelope["session_id"] == event["session_id"]
    assert envelope["source_adapter"] == event["source_adapter"]
    assert envelope["source_event_id"] == event["source_event_id"]
    assert envelope["text"] == event["text"]
    assert envelope["payload"] == event["payload"]
    assert "id" not in envelope  # store-assigned, never replayed
print("fixture: OK")
PY

# 3c. The incumbent collector, against the same read surface. The source
# column holds no observations, so the baseline is empty — but the run is the
# only place the suite proves the REAL handler accepts the exact query shapes
# incumbent.py sends: one explicit `review_state=` fetch per admitted state,
# each with `include_noise=true`. A state the surface stopped accepting, or a
# param it started rejecting, fails here instead of on the user's hub.
MEMOIRE_BASE_URL="$BASE" MEMOIRE_TOKEN_FILE="$SRC_HOME/token" \
  python3 "$BENCH/incumbent.py" --set t1 --data-dir "$DATA" \
  > "$WORK/incumbent.log" 2>&1 \
  || { cat "$WORK/incumbent.log" >&2; fail "incumbent.py failed"; }

python3 - "$DATA/t1/incumbent/$SESSION.json" <<'PY' || fail "incumbent assertions"
import json, sys
b = json.load(open(sys.argv[1]))
assert b["incumbent_version"] == 1 and b["side"] == "incumbent", b
assert b["project"] == "bench-src" and b["fixture"]["events"] == 3, b

# THE PROVENANCE GATE, proven on real rows. Ingest mirrors every captured
# event into an `accepted` observation carrying no derivation run. Those are
# not a derivation's answer to anything, and counting them as the incumbent's
# side would hand it three free observations per session that no model
# produced. All three must land in `unattributed`, named.
assert b["counts"]["model"] == 0, b["counts"]
assert b["counts"]["unattributed"] == 3, b["counts"]
assert b["observations"] == [], b["observations"]
assert {r["reason"] for r in b["unattributed"]} == {"no_derivation_run"}, b["unattributed"]
# Review state is orthogonal to provenance: these are `accepted` and still
# excluded, because WHO derived them is the question, not how they were judged.
assert b["counts"]["by_review_state"] == {"accepted": 3}, b["counts"]
assert b["prompt_versions"] == [], b["prompt_versions"]
assert b["counts"]["soft_deleted"] == 0 and b["soft_deleted"] == [], b
print("incumbent: OK")
PY

# The bucket routing itself, as law rather than as a store round trip: a
# soft-deleted row leaves the judged side NO MATTER WHAT DERIVED IT. The check
# is ordered before the provenance check on purpose — soft-delete is
# visibility deletion with no purge behind it, so a hidden row must never
# resurface in an artifact handed to judge sessions.
python3 - "$BENCH" <<'PY' || fail "soft-delete routing law"
import sys
sys.path.insert(0, sys.argv[1])
import incumbent

MODEL = {"provider": "local-qwen"}
RULES = {"provider": "rules"}

for run in (MODEL, RULES, {}):
    assert incumbent.classify_row("soft_deleted", run) == "soft_deleted", run
for state in ("proposed", "accepted", "lapsed", "rejected", "superseded", "disputed"):
    assert incumbent.classify_row(state, MODEL) == "model", state
    assert incumbent.classify_row(state, RULES) == "unattributed", state
    assert incumbent.classify_row(state, {}) == "unattributed", state
print("soft-delete routing: OK")
PY

# The freezer must refuse a session it cannot freeze, loudly.
if MEMOIRE_BASE_URL="$BASE" MEMOIRE_TOKEN_FILE="$SRC_HOME/token" \
  python3 "$BENCH/freeze.py" --set t1 --session no-such-session --data-dir "$DATA" \
  > "$WORK/freeze-empty.log" 2>&1; then
  fail "freezer accepted a session with no canonical events"
fi
grep -q "no canonical input events" "$WORK/freeze-empty.log" \
  || fail "freezer's empty-session refusal lost its reason"

# --- 4. the run driver, against a local OpenAI-compatible stub --------------
python3 "$BENCH/tests/openai-stub.py" --model bench-model > "$WORK/stub.out" 2>&1 &
STUB_PID="$!"
STUB_ADDR=""
for _ in $(seq 1 50); do
  STUB_ADDR="$(awk '/^LISTEN /{print $2; exit}' "$WORK/stub.out" 2>/dev/null || true)"
  [[ -n "$STUB_ADDR" ]] && break
  kill -0 "$STUB_PID" 2>/dev/null || fail "model stub died before listening"
  sleep 0.1
done
[[ -n "$STUB_ADDR" ]] || fail "model stub never reported its address"

cat > "$WORK/model-endpoints.json" <<JSON
{
  "endpoints": [
    {
      "endpoint_id": "bench-stub",
      "base_url": "http://$STUB_ADDR",
      "api_key_env": "DERIVE_BENCH_STUB_KEY",
      "models": ["bench-model"]
    }
  ]
}
JSON

# 4a. Default posture: the production min-content guard is ON, so this thin
# window is skipped before any provider call — the deviation is opt-in.
GUARDED="$WORK/out-guarded"
DERIVE_BENCH_STUB_KEY="derive-bench-stub-placeholder-key" \
  python3 "$BENCH/run.py" --candidate guarded --endpoint-id bench-stub \
    --model bench-model --set t1 --data-dir "$DATA" \
    --endpoints "$WORK/model-endpoints.json" --out "$GUARDED" \
  > "$WORK/run-guarded.log" 2>&1 \
  || { cat "$WORK/run-guarded.log" >&2; fail "run.py (guarded) failed"; }

python3 - "$GUARDED/$SESSION.json" <<'PY' || fail "guarded-run assertions"
import json, sys
r = json.load(open(sys.argv[1]))
assert r["min_content_guard"] is True, r["min_content_guard"]
assert r["replay"]["ok"] is True, r["replay"]
assert r["observations"] == [], r["observations"]
# The guard now skips a LEG: this window fits the ceiling, so it derives as one
# leg, and that leg carries the skip. The session genre summarizes the legs.
assert len(r["legs"]) == 1, r["legs"]
assert r["legs"][0]["genre"].startswith("skipped:"), r["legs"][0]
assert r["genre"] == "legs:ok=0,failed=0,skipped=1", r["genre"]
# A guard skip is not a coverage gap: the events reached a prompt-sized window,
# production would have skipped it too, and nothing was structurally unseen.
assert r["legs"][0]["coverage_gap"] is False, r["legs"][0]
assert r["run"]["coverage_gap"] is False, r["run"]
print("guarded run: OK")
PY

# 4b. The abstention-probe posture: the explicit opt-out reaches the model.
OUT="$WORK/out"
DERIVE_BENCH_STUB_KEY="derive-bench-stub-placeholder-key" \
  python3 "$BENCH/run.py" --candidate stub --endpoint-id bench-stub \
    --model bench-model --set t1 --data-dir "$DATA" --no-min-content-guard \
    --endpoints "$WORK/model-endpoints.json" --out "$OUT" \
  > "$WORK/run.log" 2>&1 || { cat "$WORK/run.log" >&2; fail "run.py failed"; }

RESULT="$OUT/$SESSION.json"
[[ -f "$RESULT" ]] || fail "run driver wrote no result at $RESULT"

python3 - "$RESULT" "$OUT/_summary.json" <<'PY' || fail "run-result assertions"
import json, sys

r = json.load(open(sys.argv[1]))
assert r["genre"] == "ok", (r["genre"], r.get("error"), r.get("derive", {}).get("stderr_tail"))
assert r["min_content_guard"] is False, r["min_content_guard"]
assert r["replay"]["ok"] is True, r["replay"]
assert r["replay"]["events"] == 3, r["replay"]
assert r["candidate"] == "stub" and r["model"] == "bench-model", r
# A window this small fits the production ceiling, so it derives as ONE leg —
# the split is adaptive, not unconditional.
assert len(r["legs"]) == 1, r["legs"]
leg = r["legs"][0]
assert leg["genre"] == "ok", leg
assert leg["depth"] == 0, leg
# It fit unpinned, so it kept the whole-session run identity: no epoch bounds.
assert (leg["window_start_epoch"], leg["window_end_epoch"]) == (0, 0), leg
assert 0 < leg["prompt_bytes"] <= r["max_prompt_bytes"], (leg, r["max_prompt_bytes"])
assert leg["run"]["status"] == "completed", leg["run"]
assert r["max_prompt_bytes"] == 70000 and r["split_depth_cap"] == 6, r

# Provenance the pack builder and the scorecard read — now a leg VECTOR.
assert r["run"]["legs"] == 1, r["run"]
assert leg["run"]["input_hash"].startswith("sha256:"), leg["run"]
assert r["run"]["prompt_versions"] and all(r["run"]["prompt_versions"]), r["run"]
assert r["run"]["data_boundaries"] == ["local"], r["run"]
assert r["run"]["input_hash_vector"] == [[0, 0, leg["run"]["input_hash"]]], r["run"]
assert r["run"]["coverage_gap"] is False, r["run"]
assert "dry_run_ms" in r["timings"], r["timings"]

# The observation set, grounded on a real in-window event id.
obs = r["observations"]
assert len(obs) == 1, obs
assert obs[0]["kind"] == "fact" and obs[0]["review_state"] == "proposed", obs[0]
assert obs[0]["source_event_ids"], obs[0]
assert "stub answered about" in obs[0]["text"], obs[0]

# Timings are data, not decoration.
for key in ("seed_ms", "drain_ms", "derive_ms"):
    assert key in r["timings"], r["timings"]

# The throwaway id mapping the pack builder remaps candidate source refs
# through: ids in fixture order, one per replayed event. Without it a
# candidate's grounding cannot be projected onto the transcript at all.
ids = r["replay"]["event_ids"]
assert len(ids) == 3 and ids == sorted(ids), ids
assert all(int(i) > 0 for i in ids), ids
assert set(obs[0]["source_event_ids"]) <= set(ids), (obs[0], ids)

s = json.load(open(sys.argv[2]))
assert s["candidate"] == "stub" and len(s["runs"]) == 1, s
assert s["runs"][0]["input_hash_vector"] == r["run"]["input_hash_vector"], s
assert s["runs"][0]["legs"] == 1 and s["runs"][0]["coverage_gap"] is False, s
print("run driver: OK")
PY

# No secret may reach a bench artifact. Checked again at the end of the run so
# the slice-2 trees — packs, ballots, sealed mapping, scorecards — are covered
# too; see the second call after the scorecard leg.
check_no_secret() {
  if grep -rq "derive-bench-stub-placeholder-key" "$@" 2>/dev/null; then
    fail "a bench artifact carries the endpoint key — the bearer is name-only"
  fi
}
check_no_secret "$OUT" "$GUARDED" "$DATA"

# The throwaway home is removed unless --keep-home was asked for.
if [[ -d "$DATA/homes/stub/$SESSION" ]]; then
  fail "throwaway home survived the run"
fi

# --- 4c. the adaptive window split ------------------------------------------
# The port of the sweep's derive_window, driven through the REAL CLI against
# the stub column. Whole-session single-shot was retired because production
# never sends it; these legs are what production actually sends.
MEMOIRE_BASE_URL="$BASE" MEMOIRE_TOKEN_FILE="$SRC_HOME/token" \
  python3 "$BENCH/freeze.py" --set t-split --session "$SPLIT_SESSION" \
    --session "$EDGE_SESSION" --data-dir "$DATA" \
  > "$WORK/freeze-split.log" 2>&1 \
  || { cat "$WORK/freeze-split.log" >&2; fail "freeze.py (split sessions)"; }

# The ceiling is DERIVED from the measured whole-window size rather than
# hardcoded: a prompt-template change would otherwise silently turn this into a
# no-split test that still passed.
WHOLE_BYTES="$(DERIVE_BENCH_STUB_KEY="derive-bench-stub-placeholder-key" \
  python3 "$BENCH/run.py" --candidate probe --endpoint-id bench-stub \
    --model bench-model --set t-split --session "$SPLIT_SESSION" \
    --data-dir "$DATA" --no-min-content-guard --max-prompt-bytes 100000000 \
    --endpoints "$WORK/model-endpoints.json" --out "$WORK/out-probe" \
  > "$WORK/run-probe.log" 2>&1 \
  && python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['legs'][0]['prompt_bytes'])" \
       "$WORK/out-probe/$SPLIT_SESSION.json")" \
  || { cat "$WORK/run-probe.log" >&2; fail "could not measure the whole-window prompt"; }
SPLIT_CEILING=$((WHOLE_BYTES / 3))
[[ "$SPLIT_CEILING" -gt 0 ]] || fail "measured whole window is $WHOLE_BYTES bytes"

run_split() { # run_split <label> <out> [extra args...]
  local label="$1" out="$2"; shift 2
  DERIVE_BENCH_STUB_KEY="derive-bench-stub-placeholder-key" \
    python3 "$BENCH/run.py" --candidate "$label" --endpoint-id bench-stub \
      --model bench-model --set t-split --data-dir "$DATA" \
      --no-min-content-guard --endpoints "$WORK/model-endpoints.json" \
      --out "$out" "$@" > "$WORK/run-$label.log" 2>&1 \
    || { cat "$WORK/run-$label.log" >&2; fail "run.py ($label) failed"; }
}

run_split split-a "$WORK/out-split-a" --max-prompt-bytes "$SPLIT_CEILING"
# A second identical run: the split boundaries come from median canonical
# epochs in a store holding exactly the fixture, so they must be reproducible.
run_split split-b "$WORK/out-split-b" --max-prompt-bytes "$SPLIT_CEILING"

python3 - "$WORK/out-split-a" "$WORK/out-split-b" "$SPLIT_SESSION" "$EDGE_SESSION" \
  "$SPLIT_CEILING" <<'PY' || fail "window-split assertions"
import json, os, sys

a_dir, b_dir, split_session, edge_session, ceiling = sys.argv[1:6]
ceiling = int(ceiling)
a = json.load(open(f"{a_dir}/{split_session}.json"))
b = json.load(open(f"{b_dir}/{split_session}.json"))
legs = a["legs"]

assert len(legs) >= 2, f"an oversized window must split: {legs}"
assert a["genre"] == "ok", (a["genre"], [l["genre"] for l in legs])

# This stays a pure HALVING test: these events sit 1 s apart and number well
# under the detector's event-count cap, so no rhythm cut fires. A detector
# default that started cutting them fails HERE rather than silently rewriting
# what the split test measures.
assert a["rhythm"]["status"] == "ok", a["rhythm"]
assert a["rhythm"]["cuts"] == [] and a["rhythm"]["segments"] == 1, a["rhythm"]
assert all(l["segment"] == 0 for l in legs), legs

# Sequential, oldest window first — leg N's writes gate leg N+1 through the
# write-time near-dup gate, so the order is part of the determinism contract.
starts = [l["window_start_epoch"] for l in legs]
assert starts == sorted(starts), starts

# The leaves PARTITION the parent window exactly: no event dropped, none
# derived twice. This is where a median-on-hi off-by-one would show.
epochs = [1750000101, 1750000108]
assert legs[0]["window_start_epoch"] == epochs[0], legs[0]
assert legs[-1]["window_end_epoch"] == epochs[1], legs[-1]
for prev, nxt in zip(legs, legs[1:]):
    assert nxt["window_start_epoch"] == prev["window_end_epoch"] + 1, (prev, nxt)

# Every executed leg fit the ceiling; every leg is pinned (only a whole-session
# window that fits stays unpinned) and carries its own run row.
for leg in legs:
    assert leg["depth"] >= 1, leg
    assert leg["window_start_epoch"] > 0 and leg["window_end_epoch"] > 0, leg
    if leg["genre"] == "ok":
        assert leg["prompt_bytes"] <= ceiling, leg
        assert leg["run"]["run_id"] and leg["run"]["input_hash"].startswith("sha256:"), leg
    assert leg["coverage_gap"] is False, leg

# Observations are the aggregate across legs, and the run summary is a vector.
assert len(a["observations"]) == sum(1 for l in legs if l["genre"] == "ok"), a["observations"]
assert a["run"]["legs"] == len(legs), a["run"]
assert len(a["run"]["input_hash_vector"]) == len(legs), a["run"]
assert a["run"]["coverage_gap"] is False, a["run"]

# Determinism: same fixture, same windows, same input hashes.
assert a["run"]["input_hash_vector"] == b["run"]["input_hash_vector"], (
    a["run"]["input_hash_vector"], b["run"]["input_hash_vector"])

# The median-on-hi edge: events at t, t+4, t+4. The median IS the upper bound,
# so the split point is pulled to hi-1 and the upper half keeps both t+4
# events instead of coming back empty.
edge = json.load(open(f"{a_dir}/{edge_session}.json"))
edge_legs = edge["legs"]
assert edge["rhythm"]["cuts"] == [], edge["rhythm"]
assert len(edge_legs) >= 2, f"the edge window must still split: {edge_legs}"
assert edge_legs[0]["window_start_epoch"] == 1750000201, edge_legs[0]
assert edge_legs[-1]["window_end_epoch"] == 1750000205, edge_legs[-1]
for prev, nxt in zip(edge_legs, edge_legs[1:]):
    assert nxt["window_start_epoch"] == prev["window_end_epoch"] + 1, (prev, nxt)
upper = [l for l in edge_legs if l["window_start_epoch"] <= 1750000205 <= l["window_end_epoch"]]
assert len(upper) == 1, upper
assert upper[0]["window_start_epoch"] <= 1750000205, upper[0]
print(f"window split: OK ({len(legs)} legs, edge {len(edge_legs)} legs)")
PY

# 4d. The depth cap refuses a window the PORT itself, with no CLI call — so a
# coverage gap costs no provider request. --split-depth 0 reaches the cap
# immediately, which is the only way to test this without a fixture carrying
# 2^6 distinct epochs.
REQ_BEFORE="$(grep -c '^REQUEST ' "$WORK/stub.out" || true)"
run_split split-capped "$WORK/out-capped" \
  --max-prompt-bytes "$SPLIT_CEILING" --split-depth 0
REQ_AFTER="$(grep -c '^REQUEST ' "$WORK/stub.out" || true)"

python3 - "$WORK/out-capped/$SPLIT_SESSION.json" "$REQ_BEFORE" "$REQ_AFTER" \
  <<'PY' || fail "split-depth-cap assertions"
import json, sys

r = json.load(open(sys.argv[1]))
before, after = int(sys.argv[2]), int(sys.argv[3])
legs = r["legs"]
assert len(legs) == 1, legs
assert legs[0]["genre"] == "split_depth_exceeded", legs[0]
assert legs[0]["coverage_gap"] is True, legs[0]
assert legs[0]["run"] == {}, legs[0]
assert legs[0]["prompt_bytes"] > r["max_prompt_bytes"], legs[0]
# The window was never resolved: the depth check fires BEFORE bounds
# resolution, exactly as the shell orders it.
assert (legs[0]["window_start_epoch"], legs[0]["window_end_epoch"]) == (0, 0), legs[0]
assert r["genre"] == "legs:ok=0,failed=0,skipped=1", r["genre"]
assert r["observations"] == [], r["observations"]
assert r["run"]["coverage_gap"] is True, r["run"]
assert r["run"]["coverage_gap_windows"] == [[0, 0, "split_depth_exceeded"]], r["run"]
# The whole point: a refused leg reaches no provider.
assert after == before, f"the stub saw {after - before} request(s) for a refused leg"
print("split depth cap: OK (no provider call)")
PY

# 4e. A refused leg records WHICH refusal it was.
# `-json` emits `refused` as a bare bool, so the classification is recovered
# from the run row's meta_json. It decides a ruling: a prompt adapter
# plausibly fixes an empty answer, nothing fixes a safety refusal.
#
# Its own stub instance, because the refusal is keyed on a chat-completion
# ordinal and the shared stub's counter carries every earlier leg in the file.
python3 "$BENCH/tests/openai-stub.py" --model bench-model \
  --content-filter-request 1 --empty-content-request 2 \
  > "$WORK/stub-refuse.out" 2>&1 &
REFUSE_PID="$!"
REFUSE_ADDR=""
for _ in $(seq 1 50); do
  REFUSE_ADDR="$(awk '/^LISTEN /{print $2; exit}' "$WORK/stub-refuse.out" 2>/dev/null || true)"
  [[ -n "$REFUSE_ADDR" ]] && break
  kill -0 "$REFUSE_PID" 2>/dev/null || fail "refusal stub died before listening"
  sleep 0.1
done
[[ -n "$REFUSE_ADDR" ]] || fail "refusal stub never reported its address"

python3 - "$WORK/model-endpoints.json" "$REFUSE_ADDR" <<'PY' || fail "could not register the refusal stub"
import json, sys
path, addr = sys.argv[1], sys.argv[2]
doc = json.load(open(path, encoding="utf-8"))
doc["endpoints"].append({
    "endpoint_id": "bench-stub-refuse", "base_url": f"http://{addr}",
    "api_key_env": "DERIVE_BENCH_STUB_KEY", "models": ["bench-model"],
})
json.dump(doc, open(path, "w", encoding="utf-8"), indent=2)
PY

DERIVE_BENCH_STUB_KEY="derive-bench-stub-placeholder-key" \
  python3 "$BENCH/run.py" --candidate split-refuse --endpoint-id bench-stub-refuse \
    --model bench-model --set t-split --session "$SPLIT_SESSION" --data-dir "$DATA" \
    --no-min-content-guard --endpoints "$WORK/model-endpoints.json" \
    --max-prompt-bytes "$SPLIT_CEILING" --out "$WORK/out-refuse" \
  > "$WORK/run-refuse.log" 2>&1 \
  || { cat "$WORK/run-refuse.log" >&2; fail "run.py (refusal) failed"; }

python3 - "$WORK/out-refuse/$SPLIT_SESSION.json" <<'PY' || fail "refusal-classification assertions"
import json, sys

r = json.load(open(sys.argv[1]))
legs = r["legs"]
assert len(legs) >= 3, legs

# Legs execute in order, one chat completion each, so ordinals 1 and 2 are the
# first two legs.
first, second = legs[0], legs[1]
for leg in (first, second):
    assert leg["genre"] == "refused", leg
    assert leg["outcome"]["refused"] is True, leg["outcome"]
    assert leg["outcome"]["written_count"] == 0, leg["outcome"]
    assert leg["run"]["status"] == "failed", leg["run"]

# THE POINT: the two refusals are distinguishable, which `refused` alone
# cannot do.
assert first["diagnosis"]["refusal"] == "content_filter", first["diagnosis"]
assert second["diagnosis"]["refusal"] == "empty model content", second["diagnosis"]
assert first["diagnosis"]["refusal"] != second["diagnosis"]["refusal"]

# A leg that answered normally carries no refusal.
ok_legs = [l for l in legs if l["genre"] == "ok"]
assert ok_legs, [l["genre"] for l in legs]
assert "refusal" not in ok_legs[0].get("diagnosis", {}), ok_legs[0]
assert ok_legs[0]["outcome"]["refused"] is False, ok_legs[0]["outcome"]
assert ok_legs[0]["outcome"]["status"] == "completed", ok_legs[0]["outcome"]

assert r["genre"].startswith("legs:ok="), r["genre"]
print(f"refusal classification: OK ({len(legs)} legs, 2 refused distinctly)")
PY

# The whitelist proof: meta_json carries the model's full reply (memoire:D44)
# and a bench result must never become a second home for it.
for FIELD in raw_response response_extra already_captured prompt_preview; do
  if grep -rqF "$FIELD" "$WORK/out-refuse" "$WORK/out-split-a"; then
    fail "a bench result carries '$FIELD' — the leg diagnosis is a whitelist"
  fi
done

kill "$REFUSE_PID" 2>/dev/null || true
wait "$REFUSE_PID" 2>/dev/null || true
REFUSE_PID=""

# --- 4f. rhythm cuts, the FIRST splitter ------------------------------------
# memoire:D0261: the sweep cuts each session at its own rhythm through the
# binary's -cut-points helper and halves only what rhythm left above the
# ceiling. memoire:D0262/Fibery #344: the trailing segment ends at the last
# event so far, so it is the open tail and carries -provisional. run.py's
# derive_session is that port, driven here through the REAL helper and the real
# CLI — the whole premise of the bench is that it windows what production
# windows.
MEMOIRE_BASE_URL="$BASE" MEMOIRE_TOKEN_FILE="$SRC_HOME/token" \
  python3 "$BENCH/freeze.py" --set t-rhythm --session "$RHYTHM_SESSION" \
    --data-dir "$DATA" \
  > "$WORK/freeze-rhythm.log" 2>&1 \
  || { cat "$WORK/freeze-rhythm.log" >&2; fail "freeze.py (rhythm session)"; }

# Its own set, not t-split: run_split derives the whole set, so adding a session
# there would change 4c/4d's legs and the stub's request ordinals.
run_rhythm() { # run_rhythm <label> <out> [extra args...]
  local label="$1" out="$2"; shift 2
  DERIVE_BENCH_STUB_KEY="derive-bench-stub-placeholder-key" \
    python3 "$BENCH/run.py" --candidate "$label" --endpoint-id bench-stub \
      --model bench-model --set t-rhythm --session "$RHYTHM_SESSION" \
      --data-dir "$DATA" --no-min-content-guard \
      --endpoints "$WORK/model-endpoints.json" --out "$out" "$@" \
    > "$WORK/run-$label.log" 2>&1 \
    || { cat "$WORK/run-$label.log" >&2; fail "run.py ($label) failed"; }
}

# Both ceilings are MEASURED, never hardcoded: a prompt-template change would
# otherwise turn this into a no-split or an all-split test that still passed.
# The whole session with rhythm OFF gives T + 8B bytes; with rhythm ON the two
# segments give T + 4B, so one subtraction yields the per-event size B and both
# ceilings follow from it.
run_rhythm rhythm-whole "$WORK/out-rhythm-whole" \
  --max-prompt-bytes 100000000 --no-rhythm-cuts
run_rhythm rhythm-probe "$WORK/out-rhythm-probe" --max-prompt-bytes 100000000

RHYTHM_PLAN="$(python3 - "$WORK/out-rhythm-whole/$RHYTHM_SESSION.json" \
  "$WORK/out-rhythm-probe/$RHYTHM_SESSION.json" <<'PY'
import json, sys

whole = json.load(open(sys.argv[1]))
probe = json.load(open(sys.argv[2]))

# --no-rhythm-cuts is the pre-#352 lane: no helper call at all.
assert whole["rhythm"] == {"enabled": False, "status": "disabled",
                           "cuts": [], "segments": 1, "knobs": {}}, whole["rhythm"]
assert len(whole["legs"]) == 1, whole["legs"]
assert whole["legs"][0]["depth"] == 0, whole["legs"][0]
w = whole["legs"][0]["prompt_bytes"]

assert probe["rhythm"]["status"] == "ok", probe["rhythm"]
assert probe["rhythm"]["cuts"] == [1750000304], probe["rhythm"]
assert len(probe["legs"]) == 2, probe["legs"]
s = max(leg["prompt_bytes"] for leg in probe["legs"])
assert 0 < s < w, (s, w)
b = (w - s) // 4
assert b > 0, (w, s, b)
# `s` fits either segment but not the whole session; `s - b // 2` fits a HALF
# segment (T + 2B or T + 3B) but neither whole segment (T + 4B).
print(s, s - b // 2)
PY
)" || fail "could not measure the rhythm ceilings"
read -r RHYTHM_CEILING RHYTHM_HALVE_CEILING <<< "$RHYTHM_PLAN"
[[ "$RHYTHM_CEILING" -gt 0 && "$RHYTHM_HALVE_CEILING" -gt 0 ]] \
  || fail "rhythm ceilings measured as '$RHYTHM_PLAN'"

run_rhythm rhythm-a "$WORK/out-rhythm-a" --max-prompt-bytes "$RHYTHM_CEILING"
# A second identical run: the cuts come from the fixture's own epochs, so they
# must be reproducible, and so must the resulting input hashes.
run_rhythm rhythm-b "$WORK/out-rhythm-b" --max-prompt-bytes "$RHYTHM_CEILING"

python3 - "$WORK/out-rhythm-a/$RHYTHM_SESSION.json" \
  "$WORK/out-rhythm-b/$RHYTHM_SESSION.json" "$RHYTHM_CEILING" \
  <<'PY' || fail "rhythm-cut assertions"
import json, sys

a = json.load(open(sys.argv[1]))
b = json.load(open(sys.argv[2]))
ceiling = int(sys.argv[3])
legs = a["legs"]
cut = 1750000304

assert a["genre"] == "ok", (a["genre"], [l["genre"] for l in legs])
assert a["rhythm"]["enabled"] is True, a["rhythm"]
assert a["rhythm"]["status"] == "ok", a["rhythm"]
assert a["rhythm"]["cuts"] == [cut], a["rhythm"]
assert a["rhythm"]["segments"] == 2, a["rhythm"]
assert "detail" not in a["rhythm"], a["rhythm"]
assert a["rhythm"]["knobs"] == {}, "an unset knob is not passed: the binary governs"

# Rhythm cut FIRST, and it consumed one level of the halving budget: two legs,
# both at depth 1, neither halved.
assert len(legs) == 2, legs
assert [l["depth"] for l in legs] == [1, 1], legs
assert [l["segment"] for l in legs] == [0, 1], legs
assert [l["genre"] for l in legs] == ["ok", "ok"], legs
for leg in legs:
    assert leg["prompt_bytes"] <= ceiling, leg
    assert leg["coverage_gap"] is False, leg

# The segment bounds, exactly as derive_session hands them over: [0, c1] and
# [c1 + 1, 0]. The OUTER edges stay UNPINNED, so the first and last legs keep
# the run identity the sweep gives them.
assert (legs[0]["window_start_epoch"], legs[0]["window_end_epoch"]) == (0, cut), legs[0]
assert (legs[1]["window_start_epoch"], legs[1]["window_end_epoch"]) == (cut + 1, 0), legs[1]

# The run rows resolve only the UNPINNED edge from the fetched events
# (orchestrate.go:241-248), so leg 0 keeps the cut as its end and leg 1 keeps
# cut + 1 as its start: the two rows are +1-contiguous and together span every
# event of the session. No event is dropped, none is derived twice.
rows = [(l["run"]["window_start_epoch"], l["run"]["window_end_epoch"]) for l in legs]
assert rows == [(1750000301, cut), (cut + 1, 1750100304)], rows
assert rows[1][0] == rows[0][1] + 1, rows
assert sum(l["run"]["observation_count"] for l in legs) == len(a["observations"]), a["run"]
assert len(a["observations"]) == 2, a["observations"]

# The open tail (Fibery #344): the trailing segment is provisional, the inner
# one is not — in what the PORT asked for and in what the BINARY recorded.
assert [l["provisional"] for l in legs] == [False, True], legs
assert [l["run"]["provisional"] for l in legs] == [False, True], legs

# Session-level provenance: the cuts ride beside the input-hash vector, because
# they are the first splitter and two candidates that cut differently never saw
# the same windows.
assert a["run"]["rhythm_cuts"] == [cut], a["run"]
assert a["run"]["segments"] == 2, a["run"]
assert a["run"]["legs"] == 2, a["run"]
assert a["run"]["input_hash_vector"] == [[0, cut, legs[0]["run"]["input_hash"]],
                                         [cut + 1, 0, legs[1]["run"]["input_hash"]]], a["run"]

# Determinism: same fixture, same cuts, same windows, same input hashes.
assert b["rhythm"]["cuts"] == a["rhythm"]["cuts"], (a["rhythm"], b["rhythm"])
assert b["run"]["input_hash_vector"] == a["run"]["input_hash_vector"], (
    a["run"]["input_hash_vector"], b["run"]["input_hash_vector"])
print(f"rhythm cuts: OK (cut at {cut}, 2 segments, tail provisional)")
PY

# Rhythm AND halving: a ceiling neither segment clears. Rhythm still cuts
# first, then each segment halves INSIDE itself, and only the leg that still
# ends at the tail keeps the provisional marker.
run_rhythm rhythm-halve "$WORK/out-rhythm-halve" \
  --max-prompt-bytes "$RHYTHM_HALVE_CEILING"

python3 - "$WORK/out-rhythm-halve/$RHYTHM_SESSION.json" "$RHYTHM_HALVE_CEILING" \
  <<'PY' || fail "rhythm-plus-halving assertions"
import json, sys

r = json.load(open(sys.argv[1]))
ceiling = int(sys.argv[2])
legs = r["legs"]
cut = 1750000304

assert r["rhythm"]["cuts"] == [cut] and r["rhythm"]["segments"] == 2, r["rhythm"]
assert len(legs) >= 4, legs
# Halving is the FALLBACK: it happened inside a segment, so every leg sits
# below its segment's depth 1.
assert all(l["depth"] >= 2 for l in legs), legs
assert [l["segment"] for l in legs] == sorted(l["segment"] for l in legs), legs
assert {l["segment"] for l in legs} == {0, 1}, legs

for segment in (0, 1):
    group = [l for l in legs if l["segment"] == segment]
    assert len(group) >= 2, (segment, group)
    # Each segment's halves partition that segment, oldest first.
    starts = [l["window_start_epoch"] for l in group]
    assert starts == sorted(starts), group
    for prev, nxt in zip(group, group[1:]):
        assert nxt["window_start_epoch"] == prev["window_end_epoch"] + 1, (prev, nxt)
    # Only the tail keeps the marker: segment 0 is closed, so no leg of it is
    # provisional, and in segment 1 only the RIGHT-most leg is.
    want = [False] * len(group)
    if segment == 1:
        want[-1] = True
    assert [l["provisional"] for l in group] == want, (segment, group)
    assert [l["run"]["provisional"] for l in group] == want, (segment, group)

for leg in legs:
    assert leg["genre"] == "ok", leg
    assert leg["prompt_bytes"] <= ceiling, leg
assert r["run"]["rhythm_cuts"] == [cut] and r["run"]["segments"] == 2, r["run"]
print(f"rhythm + halving: OK ({len(legs)} legs, halved inside each segment)")
PY

# --no-rhythm-cuts restores the pre-#352 geometry: no helper call, no cut, and
# the whole session halves on its median exactly as it did before.
run_rhythm rhythm-off "$WORK/out-rhythm-off" \
  --max-prompt-bytes "$RHYTHM_CEILING" --no-rhythm-cuts

python3 - "$WORK/out-rhythm-off/$RHYTHM_SESSION.json" <<'PY' \
  || fail "no-rhythm-cuts assertions"
import json, sys

r = json.load(open(sys.argv[1]))
legs = r["legs"]

assert r["rhythm"]["status"] == "disabled", r["rhythm"]
assert r["rhythm"]["enabled"] is False, r["rhythm"]
assert r["rhythm"]["cuts"] == [] and r["rhythm"]["segments"] == 1, r["rhythm"]
assert r["run"]["rhythm_cuts"] == [] and r["run"]["segments"] == 1, r["run"]

# Pre-#352 halving: the whole-session window did not fit, so it resolved its own
# bounds and halved once. Both legs are PINNED at real epochs — the rhythm lane
# above leaves the outer edges at 0 instead, which is how the two geometries
# are told apart.
assert len(legs) == 2, legs
assert [l["depth"] for l in legs] == [1, 1], legs
assert [l["segment"] for l in legs] == [0, 0], legs
assert legs[0]["window_start_epoch"] == 1750000301, legs[0]
assert legs[-1]["window_end_epoch"] == 1750100304, legs[-1]
assert legs[1]["window_start_epoch"] == legs[0]["window_end_epoch"] + 1, legs
# A whole-session window IS the open tail, so halving it keeps the marker on
# the right leg only.
assert [l["provisional"] for l in legs] == [False, True], legs
assert [l["run"]["provisional"] for l in legs] == [False, True], legs
print("no-rhythm-cuts: OK (pre-#352 halving geometry)")
PY

# Fail-open, NOT silent: a knob the binary refuses (runCutPoints rejects a
# negative, cmd/derive/main.go:1292-1294) records the degradation and derives
# the whole session once, as the open tail it is.
run_rhythm rhythm-fail "$WORK/out-rhythm-fail" \
  --max-prompt-bytes 100000000 --cut-gap-seconds=-1

python3 - "$WORK/out-rhythm-fail/$RHYTHM_SESSION.json" <<'PY' \
  || fail "rhythm fail-open assertions"
import json, sys

r = json.load(open(sys.argv[1]))
legs = r["legs"]

assert r["rhythm"]["status"] == "failed", r["rhythm"]
assert r["rhythm"]["enabled"] is True, r["rhythm"]
assert r["rhythm"]["cuts"] == [], r["rhythm"]
assert r["rhythm"]["knobs"] == {"cut-gap-seconds": -1}, r["rhythm"]
# The detail names the rc and the binary's own complaint, printable-only.
assert r["rhythm"]["detail"].startswith("rc="), r["rhythm"]
assert "must not be negative" in r["rhythm"]["detail"], r["rhythm"]

# The whole session derived once, and it is an open tail like any other
# window that ends at the last event so far.
assert len(legs) == 1, legs
assert legs[0]["genre"] == "ok", legs[0]
assert legs[0]["depth"] == 0 and legs[0]["segment"] == 0, legs[0]
assert (legs[0]["window_start_epoch"], legs[0]["window_end_epoch"]) == (0, 0), legs[0]
assert legs[0]["provisional"] is True, legs[0]
assert legs[0]["run"]["provisional"] is True, legs[0]["run"]
print("rhythm fail-open: OK (whole session, provisional, degradation recorded)")
PY

check_no_secret "$WORK/out-rhythm-a" "$WORK/out-rhythm-halve" \
  "$WORK/out-rhythm-fail"

# --- 5. near-dup golden-vector parity ---------------------------------------
# The Python port and the production gate assert the same committed vectors
# (the Go half is TestNearDupGolden* in modelderive), so a change to either
# implementation that moves a score or flips a policy hit fails here rather
# than silently aligning packs under a policy production no longer runs.
python3 "$BENCH/tests/neardup-test.py" \
  || fail "the bench's near-dup port diverged from the golden vectors"

# --- 6. pack / ballot / scorecard -------------------------------------------
# Synthetic inputs, deliberately: the alignment outcome must be knowable ahead
# of time for the assertions below to mean anything, and a stub model's output
# is not. The run driver's real output was already asserted in leg 4.
DATA2="$WORK/data2"
PACKOUT="$WORK/out-packs"
python3 - "$DATA2" "$PACKOUT" <<'PY' || fail "could not build synthetic pack inputs"
import json, os, sys

data_dir, out_dir = sys.argv[1], sys.argv[2]
SET, PROJECT, CANDIDATE = "t2", "packtest", "synthcand"

def write(path, payload):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(payload, fh, indent=2, sort_keys=True)
        fh.write("\n")

def fixture(session, base_id, base_epoch, texts):
    events = [{
        "id": base_id + i, "project": PROJECT, "session_id": session,
        "source_adapter": "bench-test", "source_event_id": f"{session}-{i}",
        "event_type": "claude_assistant_turn", "occurred_at_epoch": base_epoch + i,
        "cwd": "/repo/bench", "payload": {"text": text}, "text": text,
    } for i, text in enumerate(texts)]
    return {
        "fixture_version": 1, "frozen_at_utc": "2026-08-24T00:00:00+00:00",
        "project": PROJECT, "session_id": session,
        "event_types": ["claude_assistant_turn"],
        "counts": {"events": len(events),
                   "by_type": {"claude_assistant_turn": len(events)}},
        "epoch_range": {"start": base_epoch, "end": base_epoch + len(events) - 1},
        "session_meta": {}, "events": events,
    }

def incumbent(session, prompt_version, rows, event_count, hidden=()):
    hidden = list(hidden)
    by_state = {}
    for row in rows + hidden:
        by_state[row["review_state"]] = by_state.get(row["review_state"], 0) + 1
    return {
        "incumbent_version": 1, "bench": "derive", "side": "incumbent",
        "project": PROJECT, "session_id": session,
        "collected_at_utc": "2026-08-24T00:00:00+00:00",
        "fixture": {"path": "-", "frozen_at_utc": "-", "events": event_count},
        "prompt_versions": [prompt_version],
        "derivation_versions": ["observation-derive-v2"],
        "counts": {"model": len(rows), "unattributed": 0,
                   "soft_deleted": len(hidden),
                   "by_review_state": by_state,
                   "by_provider": {"local-qwen": len(rows) + len(hidden)}},
        "runs": {}, "observations": rows, "unattributed": [],
        "soft_deleted": hidden,
    }

def obs(oid, kind, text, state, refs):
    return {"id": oid, "kind": kind, "text": text, "confidence": 0.8,
            "review_state": state, "content_hash": "sha256:INCUMBENTLEAKCANARY",
            "provenance": "inferred", "noise_candidate": 0, "can_instruct": 0,
            "source_event_ids": refs, "created_at_epoch": 1750100000,
            "derivation_run_id": "run-inc-" + session_tag(oid),
            "provider": "local-qwen", "model": "incumbent-model",
            "prompt_version": "derive-observations-v2.4",
            "derivation_version": "observation-derive-v2"}

def session_tag(oid):
    return "a" if oid < 770100 else "b"

def candidate(session, event_ids, prompt_version, rows, gap_windows=()):
    """A run.py result in the CURRENT schema: a session is a vector of legs."""
    gap_windows = list(gap_windows)
    legs = [{
        "window_start_epoch": 0, "window_end_epoch": 0, "depth": 0,
        "prompt_bytes": 1234, "genre": "ok", "derive_ms": 4242, "exit_code": 0,
        "coverage_gap": False,
        "run": {"run_id": "cand-run-" + session, "status": "completed",
                "prompt_version": prompt_version,
                "derivation_version": "observation-derive-v2",
                "input_hash": "sha256:synthetic-input-hash",
                "data_boundary": "local", "window_start_epoch": 0,
                "window_end_epoch": 0, "observation_count": len(rows),
                "proposed_observation_count": len(rows)},
    }]
    for lo, hi in gap_windows:
        legs.append({
            "window_start_epoch": lo, "window_end_epoch": hi, "depth": 6,
            "prompt_bytes": 999999, "genre": "split_depth_exceeded",
            "derive_ms": 0, "coverage_gap": True, "run": {},
        })
    return {
        "bench": "derive", "candidate": CANDIDATE, "endpoint_id": "synth-endpoint",
        "model": "synth-model", "set": SET, "project": PROJECT,
        "session_id": session,
        "fixture": {"path": "-", "frozen_at_utc": "-",
                    "events": len(event_ids), "by_type": {}},
        "min_content_guard": False,
        "max_prompt_bytes": 70000, "split_depth_cap": 6,
        "genre": "ok" if not gap_windows else "legs:ok=1,failed=0,skipped=1",
        "started_at_utc": "2026-08-24T00:00:00+00:00",
        "timings": {"seed_ms": 1, "drain_ms": 1, "dry_run_ms": 7,
                    "derive_ms": 4242},
        "replay": {"ok": True, "events": len(event_ids), "event_ids": event_ids},
        "legs": legs,
        "run": {
            "legs": len(legs),
            "prompt_versions": [prompt_version],
            "derivation_versions": ["observation-derive-v2"],
            "data_boundaries": ["local"],
            "input_hash_vector": [[l["window_start_epoch"], l["window_end_epoch"],
                                   l["run"].get("input_hash", "")] for l in legs],
            "observation_count": len(rows),
            "proposed_observation_count": len(rows),
            "coverage_gap": bool(gap_windows),
            "coverage_gap_windows": [[lo, hi, "split_depth_exceeded"]
                                     for lo, hi in gap_windows],
        },
        "observations": rows,
    }

def cand_obs(text, refs):
    return {"kind": "fact", "text": text, "confidence": 0.7,
            "source_event_ids": refs, "review_state": "proposed",
            "content_hash": "sha256:CANDIDATELEAKCANARY",
            "noise_candidate": 0, "can_instruct": 0}

# Session A: one incumbent row and one candidate row are a documented near-dup
# pair (the digit-free negation blindness), so exactly one EQUIVALENT collapse
# is expected. One incumbent row cites an event the fixture does not carry —
# the purged-reference path, which must count and not crash.
write(f"{data_dir}/{SET}/pack-a.json", fixture("pack-a", 9001, 1750100001, [
    "the drainer failed on postgres during the nightly sweep",
    "minting sources db.env in process",
    "the freezer refused a capped session",
]))
write(f"{data_dir}/{SET}/incumbent/pack-a.json", incumbent("pack-a", "derive-observations-v2.4", [
    obs(770001, "gotcha", "The queue drainer is broken on the PostgreSQL engine",
        "accepted", [9001]),
    obs(770002, "constraint", "Minting must run with ~/.memoire/db.env sourced "
        "in-process; credential values must never appear in argv.", "proposed", [9002]),
    obs(770003, "fact", "The freezer refuses a session with 500 same-epoch events",
        "lapsed", [9003]),
    obs(770004, "fact", "PR #67 review completed successfully", "proposed", [9999]),
], 3, hidden=[
    # A row the operator hid. Its text is deliberately a near-dup of a
    # candidate observation, so a leak would not merely show up in the grep
    # below — it would gate, raising equivalent_count to 2 and dropping
    # one_side_count, which the counts asserted above would catch on their own.
    obs(770005, "fact", "SOFTDELETEDCANARY the hourly sweep re-derived whole "
        "sessions because the event fetch had no coverage exclusions",
        "soft_deleted", [9002]),
]))

# Session B: the candidate produces nothing resembling the accepted gem, so
# the sanity probe must flag it. Its incumbent ran under an older prompt
# version, so the drift flag must fire too.
write(f"{data_dir}/{SET}/pack-b.json", fixture("pack-b", 9101, 1750200001, [
    "minting sources db.env in process",
    "the tool window is capped at five hundred rows",
]))
write(f"{data_dir}/{SET}/incumbent/pack-b.json", incumbent("pack-b", "derive-observations-v2.3", [
    obs(770101, "constraint", "Minting must run with ~/.memoire/db.env sourced "
        "in-process; credential values must never appear in argv.", "accepted", [9101]),
], 2))

write(f"{data_dir}/{SET}/set.json", {"set": SET, "sessions": ["pack-a", "pack-b"],
                                     "updated_at_utc": "2026-08-24T00:00:00+00:00"})

run_dir = f"{out_dir}/{CANDIDATE}"
write(f"{run_dir}/pack-a.json", candidate("pack-a", [11, 12, 13], "derive-observations-v2.4", [
    cand_obs("The queue drainer is fixed on the PostgreSQL engine", [11]),
    cand_obs("The hourly sweep re-derived whole sessions because the event fetch "
             "had no coverage exclusion.", [12]),
    cand_obs("v1 vs base judge verdict: Base model preferred 92% of the time "
             "(FT 2.97 vs Base 4.64).", [13]),
]))
# pack-b's candidate additionally carries a COVERAGE GAP: one window never
# reached the model, so the incumbent saw events it structurally could not.
# The scorecard must asterisk the omission axis for it — and the pack must stay
# silent, because only one side can ever have gaps.
write(f"{run_dir}/pack-b.json", candidate("pack-b", [21, 22], "derive-observations-v2.4", [
    cand_obs("The freezer pages the tool-window query by re-anchoring start", [21]),
], gap_windows=[(1750200002, 1750200002)]))

results = {s: json.load(open(f"{run_dir}/{s}.json", encoding="utf-8"))
           for s in ("pack-a", "pack-b")}
write(f"{run_dir}/_summary.json", {
    "candidate": CANDIDATE, "endpoint_id": "synth-endpoint", "model": "synth-model",
    "set": SET, "finished_at_utc": "2026-08-24T00:00:00+00:00",
    "runs": [{"session_id": s, "genre": results[s]["genre"],
              "observations": len(results[s]["observations"]),
              "legs": results[s]["run"]["legs"],
              "input_hash_vector": results[s]["run"]["input_hash_vector"],
              "prompt_versions": results[s]["run"]["prompt_versions"],
              "coverage_gap": results[s]["run"]["coverage_gap"],
              "coverage_gap_windows": results[s]["run"]["coverage_gap_windows"],
              "derive_ms": 4242}
             for s in ("pack-a", "pack-b")],
})
print("synthetic pack inputs: OK")
PY

python3 "$BENCH/pack.py" --set t2 --data-dir "$DATA2" --out "$PACKOUT" \
  > "$WORK/pack.log" 2>&1 || { cat "$WORK/pack.log" >&2; fail "pack.py failed"; }

python3 - "$PACKOUT" <<'PY' || fail "pack assertions"
import glob, json, os, sys

out = sys.argv[1]
sealed = [json.load(open(p)) for p in sorted(glob.glob(f"{out}/sealed/*.json"))]
assert len(sealed) == 2, sealed
by_session = {s["session_id"]: s for s in sealed}
assert set(by_session) == {"pack-a", "pack-b"}, list(by_session)

packs = {}
for s in sealed:
    pack_dir = os.path.join(out, "packs", *s["pack_id"].split("/"))
    packs[s["session_id"]] = json.load(open(os.path.join(pack_dir, "pack.json")))
    assert os.path.exists(os.path.join(pack_dir, "ballot.md")), pack_dir
    # The transcript is session-scoped and sits beside the pack dir, exactly
    # where the pack's relative pointer says it does.
    assert os.path.exists(os.path.join(pack_dir, "..", "transcript.md")), pack_dir

a, b = packs["pack-a"], packs["pack-b"]

# EQUIVALENT collapse: the documented negation pair gates, so it drops out of
# judgment and only the genuine deltas remain.
assert a["metadata"]["equivalent_count"] == 1, a["metadata"]
assert a["metadata"]["disputed_count"] == 0, a["metadata"]
assert a["metadata"]["one_side_count"] == 5, a["metadata"]
assert len(a["equivalent"]) == 1 and len(a["one_side"]) == 5, a
assert b["metadata"]["equivalent_count"] == 0, b["metadata"]
assert b["metadata"]["one_side_count"] == 2, b["metadata"]

# A source ref the fixture cannot resolve is counted, never fatal, and the
# count is an aggregate: a per-side count would say which side is historical.
assert a["metadata"]["unresolved_source_refs"] == 1, a["metadata"]
assert a["metadata"]["prompt_version_drift"] is False, a["metadata"]
assert b["metadata"]["prompt_version_drift"] is True, b["metadata"]
assert a["metadata"]["disputed_floor"] > 0, a["metadata"]

# Source refs are fixture ordinals in a shared key space, never store ids.
for pack in (a, b):
    for entry in pack["one_side"]:
        for ref in entry["observation"]["source_refs"]:
            assert 1 <= ref <= pack["metadata"]["transcript_events"], (ref, pack["metadata"])
        assert sorted(entry["observation"]) == ["confidence", "kind", "source_refs", "text"], entry

# Blinding: four side nonces plus two pack nonces, all distinct. A nonce reused
# across sessions would let one cracked pack unblind the set.
nonces = a["sides"] + b["sides"] + [s["pack_id"].split("/")[1] for s in sealed]
assert len(set(nonces)) == 6, nonces
assert set(by_session["pack-a"]["sides"]) == set(a["sides"]), (by_session["pack-a"], a["sides"])
assert sorted(by_session["pack-a"]["sides"].values()) == ["candidate", "incumbent"], by_session["pack-a"]

# Soft-deleted rows are held back from judging but stay in the operator's
# custody: the sealed mapping counts how much of the baseline was withheld,
# and the local baseline file still carries the row itself.
assert by_session["pack-a"]["counts"]["incumbent_soft_deleted"] == 1, by_session["pack-a"]
assert by_session["pack-b"]["counts"]["incumbent_soft_deleted"] == 0, by_session["pack-b"]
# It never counted as an observation of the judged side.
assert by_session["pack-a"]["counts"]["incumbent_observations"] == 4, by_session["pack-a"]
print("pack: OK")
PY

# The withheld row survives in the user's own gitignored baseline file — the
# ruling stays re-decidable without re-collecting from the store.
python3 - "$DATA2/t2/incumbent/pack-a.json" <<'PY' || fail "soft-delete custody"
import json, sys
b = json.load(open(sys.argv[1]))
assert b["counts"]["soft_deleted"] == 1, b["counts"]
assert len(b["soft_deleted"]) == 1, b["soft_deleted"]
assert "SOFTDELETEDCANARY" in b["soft_deleted"][0]["text"], b["soft_deleted"][0]
assert all("SOFTDELETEDCANARY" not in r["text"] for r in b["observations"]), b["observations"]
print("soft-delete custody: OK")
PY

# Leak-proofing: the whitelist is asserted from the outside, over every
# judge-visible byte. Each canary is a field the pack builder must have
# dropped, or a token that identifies a side.
# The leg-derived tokens matter as much as the observation fields: only ONE
# side can have coverage gaps, so any gap marker in a judge-visible file
# unblinds the pack outright. ("legs" alone is not a canary — it is an ordinary
# English word that can appear in transcript prose.)
for LEAK in review_state content_hash derivation_run_id created_at_epoch \
            noise_candidate INCUMBENTLEAKCANARY CANDIDATELEAKCANARY \
            SOFTDELETEDCANARY soft_deleted \
            split_depth_exceeded unsplittable_window coverage_gap \
            prompt_bytes input_hash refusal raw_response \
            synthcand incumbent sealed 770001 770101; do
  if grep -rqF "$LEAK" "$PACKOUT/packs"; then
    grep -rlF "$LEAK" "$PACKOUT/packs" >&2
    fail "judge-visible pack tree carries '$LEAK' — the observation whitelist leaked"
  fi
done

# The transcript is the readability projection judges ground against.
TRANSCRIPT="$PACKOUT/packs/pack-a/transcript.md"
[[ -f "$TRANSCRIPT" ]] || fail "no transcript at $TRANSCRIPT"
grep -q "## Event 1 — claude_assistant_turn — epoch 1750100001" "$TRANSCRIPT" \
  || fail "transcript lost its ordinal/type/epoch heading"
grep -q "## Event 3 —" "$TRANSCRIPT" || fail "transcript truncated the session"
grep -q "the freezer refused a capped session" "$TRANSCRIPT" \
  || fail "transcript dropped an event's text"

# A candidate result predating the throwaway-id mapping cannot be packed at
# all: its observations' grounding could not be projected onto the transcript,
# and a pack built on silently dropped citations would be worse than no pack.
STRIPPED="$WORK/data-stripped"
STRIPOUT="$WORK/out-stripped"
mkdir -p "$STRIPOUT"
cp -r "$DATA2" "$STRIPPED"
cp -r "$PACKOUT/synthcand" "$STRIPOUT/synthcand"
python3 - "$STRIPOUT/synthcand/pack-a.json" <<'PY' || fail "could not strip event_ids"
import json, sys
result = json.load(open(sys.argv[1]))
del result["replay"]["event_ids"]
json.dump(result, open(sys.argv[1], "w"), indent=2, sort_keys=True)
PY
if python3 "$BENCH/pack.py" --set t2 --data-dir "$STRIPPED" --out "$STRIPOUT" \
  --session pack-a > "$WORK/pack-stripped.log" 2>&1; then
  cat "$WORK/pack-stripped.log" >&2
  fail "pack.py packed a candidate result carrying no replay.event_ids"
fi
grep -q "re-run the candidate" "$WORK/pack-stripped.log" \
  || { cat "$WORK/pack-stripped.log" >&2; fail "the missing-event_ids refusal lost its remedy"; }

# --- 6b. ballots and the scorecard ------------------------------------------
fill_ballots() { # fill_ballots <mode> [out-dir]
  python3 - "${2:-$PACKOUT}" "$1" <<'PY' || fail "could not fill ballots ($1)"
import glob, json, os, sys

out, mode = sys.argv[1], sys.argv[2]
BEGIN, END = "<!-- BALLOT-BEGIN -->", "<!-- BALLOT-END -->"
for path in sorted(glob.glob(f"{out}/sealed/*.json")):
    sealed = json.load(open(path))
    roles = {role: nonce for nonce, role in sealed["sides"].items()}
    pack_dir = os.path.join(out, "packs", *sealed["pack_id"].split("/"))
    ballot_path = os.path.join(pack_dir, "ballot.md")
    prefs = {
        "omission": roles["candidate"], "grounding": roles["candidate"],
        "relevance": "tie", "noise": roles["incumbent"],
        "dedup": "tie", "abstention": "tie",
    }
    pack_id, judge = sealed["pack_id"], "suite"
    if mode == "malformed":
        prefs["omission"] = "not-a-side-label"
    elif mode == "badpack":
        # A ballot carrying another pack's id: the shape a copied or misplaced
        # ballot takes, which would otherwise vote on a comparison it never saw.
        pack_id = "some-other-session/deadbeefcafe"
    elif mode == "nojudge":
        judge = ""
    block = json.dumps({
        "ballot_version": 1, "pack_id": pack_id, "judge": judge,
        "axes": {axis: {"prefer": prefer, "evidence": "suite fixture"}
                 for axis, prefer in prefs.items()},
    }, indent=2)
    text = open(ballot_path, encoding="utf-8").read()
    head = text.split(BEGIN, 1)[0]
    open(ballot_path, "w", encoding="utf-8").write(
        f"{head}{BEGIN}\n```json\n{block}\n```\n{END}\n")
print(f"ballots filled ({mode})")
PY
}

# Each malformed shape must be refused BY FILE, with its reason intact.
expect_ballot_refusal() { # expect_ballot_refusal <mode> <needle>
  fill_ballots "$1"
  if python3 "$BENCH/scorecard.py" --set t2 --data-dir "$DATA2" --out "$PACKOUT" \
    --check-ballots > "$WORK/ballots-$1.log" 2>&1; then
    cat "$WORK/ballots-$1.log" >&2
    fail "scorecard accepted a '$1' ballot"
  fi
  grep -q "REFUSED" "$WORK/ballots-$1.log" \
    || { cat "$WORK/ballots-$1.log" >&2; fail "'$1' refusal lost its REFUSED marker"; }
  grep -q "$2" "$WORK/ballots-$1.log" \
    || { cat "$WORK/ballots-$1.log" >&2; fail "'$1' refusal lost its reason"; }
}

# An untouched ballot is UNFILLED, not malformed — a mid-flight bench must not
# read as broken.
python3 "$BENCH/scorecard.py" --set t2 --data-dir "$DATA2" --out "$PACKOUT" \
  --check-ballots > "$WORK/ballots-pending.log" 2>&1 \
  || { cat "$WORK/ballots-pending.log" >&2; fail "an unfilled ballot must validate, not refuse"; }
grep -q "2 ballot(s) parsed, 0 filled, 0 refused" "$WORK/ballots-pending.log" \
  || { cat "$WORK/ballots-pending.log" >&2; fail "unfilled ballots misreported"; }

fill_ballots valid
python3 "$BENCH/scorecard.py" --set t2 --data-dir "$DATA2" --out "$PACKOUT" \
  > "$WORK/scorecard.log" 2>&1 || { cat "$WORK/scorecard.log" >&2; fail "scorecard.py failed"; }

python3 - "$PACKOUT" <<'PY' || fail "scorecard assertions"
import json, os, sys

out = sys.argv[1]
card = json.load(open(os.path.join(out, "scorecard-synthcand.json")))
assert card["candidate"] == "synthcand", card["candidate"]
assert card["packs"] == 2 and card["ballots_filled"] == 2, card
assert card["ballots_pending"] == [], card["ballots_pending"]

# Unblinding: the ballots named nonces, the scorecard must read sides.
assert card["axes"]["omission"] == {"candidate": 2, "incumbent": 0, "tie": 0}, card["axes"]
assert card["axes"]["noise"] == {"candidate": 0, "incumbent": 2, "tie": 0}, card["axes"]
assert card["axes"]["relevance"] == {"candidate": 0, "incumbent": 0, "tie": 2}, card["axes"]

assert card["alignment"] == {"equivalent": 1, "disputed": 0, "one_side": 7}, card["alignment"]
assert card["runs"]["input_hash_consistent"] is True, card["runs"]
assert card["runs"]["prompt_version_drift_sessions"] == ["pack-b"], card["runs"]
assert card["runs"]["derive_ms"]["total"] == 8484, card["runs"]["derive_ms"]

# Sanity probe: pack-a's accepted gem has a near-dup on the candidate side,
# pack-b's does not — a candidate missing gems is flagged before any ballot.
probe = {row["session_id"]: row for row in card["sanity_probe"]["by_session"]}
assert card["sanity_probe"]["flagged_sessions"] == ["pack-b"], card["sanity_probe"]
assert probe["pack-a"]["accepted_gems"] == 1 and probe["pack-a"]["matched"] == 1, probe
assert probe["pack-b"]["matched"] == 0 and len(probe["pack-b"]["missed"]) == 1, probe

# Coverage gaps are FLAGGED, never excluded — the opposite of an input-hash
# divergence. pack-b's ballot still counts; the omission axis just carries an
# asterisk because the candidate never saw one window at all.
gaps = card["coverage_gaps"]
assert [row["session_id"] for row in gaps] == ["pack-b"], gaps
assert gaps[0]["windows"] == [[1750200002, 1750200002, "split_depth_exceeded"]], gaps
assert card["packs"] == 2 and card["ballots_filled"] == 2, card
assert card["excluded_sessions"] == [], card["excluded_sessions"]

report = open(os.path.join(out, "scorecard-synthcand.md"), encoding="utf-8").read()
assert "Derive-bench scorecard" in report and "FLAGGED" in report, report[:400]
assert "COVERAGE GAPS" in report and "omission" in report, report[:900]
print("scorecard: OK")
PY

# A ballot the scorecard cannot trust is refused BY FILE, and refusal is loud.
expect_ballot_refusal malformed "neither a side label"
# The ballot must be bound to the pack it was written for, and a filled one
# must name its judge — otherwise a stray ballot votes anonymously on a
# comparison it never saw.
expect_ballot_refusal badpack "cannot be moved between packs"
expect_ballot_refusal nojudge "must name the judge"

check_no_secret "$PACKOUT" "$DATA2"

# --- 6c. input-hash divergence excludes a session from every aggregate ------
# Two candidates that did not see the same canonical input for a session are
# not answering the same question there. Its ballots must leave every total —
# and the card must say so, rather than refusing the whole run: the other
# session in the same bench is still comparable (memoire:D0048).
DIVOUT="$WORK/out-divergent"
python3 - "$PACKOUT" "$DIVOUT" <<'PY' || fail "could not build the divergent case"
import json, os, shutil, sys

src, dst = sys.argv[1], sys.argv[2]
# The comparable unit is the ordered leg VECTOR, not one hash — so the
# divergence is injected there.
for label, hash_a in (("cand-a", "sha256:synthetic-input-hash"),
                      ("cand-b", "sha256:DIVERGENT-input-hash")):
    run_dir = os.path.join(dst, label)
    shutil.copytree(os.path.join(src, "synthcand"), run_dir)
    for name in sorted(os.listdir(run_dir)):
        path = os.path.join(run_dir, name)
        doc = json.load(open(path, encoding="utf-8"))
        doc["candidate"] = label
        if name == "_summary.json":
            for run in doc["runs"]:
                if run["session_id"] == "pack-a":
                    run["input_hash_vector"] = [[0, 0, hash_a]]
        elif doc["session_id"] == "pack-a":
            doc["run"]["input_hash_vector"] = [[0, 0, hash_a]]
            doc["legs"][0]["run"]["input_hash"] = hash_a
        json.dump(doc, open(path, "w", encoding="utf-8"), indent=2, sort_keys=True)
print("divergent candidates: OK")
PY

python3 "$BENCH/pack.py" --set t2 --data-dir "$DATA2" --out "$DIVOUT" \
  > "$WORK/pack-div.log" 2>&1 || { cat "$WORK/pack-div.log" >&2; fail "pack.py (divergent)"; }
fill_ballots valid "$DIVOUT"
python3 "$BENCH/scorecard.py" --set t2 --data-dir "$DATA2" --out "$DIVOUT" \
  > "$WORK/scorecard-div.log" 2>&1 \
  || { cat "$WORK/scorecard-div.log" >&2; fail "scorecard.py (divergent)"; }

python3 - "$DIVOUT" <<'PY' || fail "divergence-exclusion assertions"
import json, os, sys

out = sys.argv[1]
for label in ("cand-a", "cand-b"):
    card = json.load(open(os.path.join(out, f"scorecard-{label}.json")))
    # pack-a diverged; pack-b did not. Only pack-b may reach any number.
    assert card["packs"] == 1, (label, card["packs"])
    assert card["packs_excluded"] == 1, (label, card["packs_excluded"])
    assert [r["session_id"] for r in card["excluded_sessions"]] == ["pack-a"], card
    assert card["excluded_sessions"][0]["reason"] == "input_hash_divergent", card
    assert card["ballots_filled"] == 1, (label, card["ballots_filled"])
    # One ballot's worth of axis votes, not two.
    assert card["axes"]["omission"]["candidate"] == 1, card["axes"]
    assert card["axes"]["noise"]["incumbent"] == 1, card["axes"]
    # The probe and the timings follow the same exclusion.
    assert [r["session_id"] for r in card["sanity_probe"]["by_session"]] == ["pack-b"], card
    assert card["runs"]["derive_ms"]["total"] == 4242, card["runs"]["derive_ms"]
    assert card["runs"]["input_hash_consistent"] is False, card["runs"]
    assert card["runs"]["input_hash_divergent_sessions"] == ["pack-a"], card["runs"]
    report = open(os.path.join(out, f"scorecard-{label}.md"), encoding="utf-8").read()
    assert "EXCLUDED FROM EVERY NUMBER BELOW" in report, report[:600]
    assert "pack-a" in report, report[:600]
print("divergence exclusion: OK")
PY

# --- 6d. one authoritative pack per candidate x session ---------------------
# Runs LAST: --force drops the ballots filled above, so nothing may depend on
# them past this point.
if python3 "$BENCH/pack.py" --set t2 --data-dir "$DATA2" --out "$PACKOUT" \
  > "$WORK/pack-rerun.log" 2>&1; then
  cat "$WORK/pack-rerun.log" >&2
  fail "pack.py packed a second comparison for a candidate/session pair"
fi
grep -q "already has a pack for session" "$WORK/pack-rerun.log" \
  || { cat "$WORK/pack-rerun.log" >&2; fail "the duplicate-pack refusal lost its reason"; }

python3 "$BENCH/pack.py" --set t2 --data-dir "$DATA2" --out "$PACKOUT" --force \
  > "$WORK/pack-force.log" 2>&1 \
  || { cat "$WORK/pack-force.log" >&2; fail "pack.py --force failed"; }

python3 - "$PACKOUT" <<'PY' || fail "pack replacement assertions"
import glob, json, os, sys

out = sys.argv[1]
sealed = [json.load(open(p)) for p in sorted(glob.glob(f"{out}/sealed/*.json"))]
# Replaced, not appended: still one record per candidate x session.
assert len(sealed) == 2, [s["pack_id"] for s in sealed]
pairs = sorted((s["candidate"], s["session_id"]) for s in sealed)
assert pairs == [("synthcand", "pack-a"), ("synthcand", "pack-b")], pairs
for record in sealed:
    session = record["session_id"]
    nonces = [d for d in os.listdir(f"{out}/packs/{session}")
              if os.path.isdir(f"{out}/packs/{session}/{d}")]
    assert nonces == [record["pack_id"].split("/")[1]], (session, nonces)
    # The superseded pack's ballot went with it — no orphan to unblind.
    assert os.path.exists(f"{out}/packs/{session}/{nonces[0]}/ballot.md"), session
    assert os.path.exists(f"{out}/packs/{session}/transcript.md"), session
print("pack replacement: OK")
PY

# --- 7. the specialist prompt-adapter shim ----------------------------------
# Unit half: translation and proxy behavior against a scripted upstream.
python3 "$BENCH/tests/shim-test.py" \
  || fail "the specialist shim's translation or proxy behavior regressed"

# End-to-end half: a real shim process in front of the suite's own stub,
# driven exactly as the local-qwen adapter drives an endpoint — availability
# probe, then one chat completion carrying a REAL builder prompt. The stub
# answers in the SPECIALIST's shape; what comes back out of the shim must be
# the sealed derive envelope.
SHIM_WORK="$WORK/shim"
mkdir -p "$SHIM_WORK"

shim_e2e() { # mode canned-json-file expected-kind expected-text
  local mode="$1" canned="$2" want_kind="$3" want_text="$4"
  local stub_out="$SHIM_WORK/$mode-stub.out" shim_out="$SHIM_WORK/$mode-shim.out"
  local stub_pid shim_pid stub_addr="" shim_addr=""

  python3 "$BENCH/tests/openai-stub.py" --model specialist-model \
    --canned-response-file "$canned" > "$stub_out" 2>&1 &
  stub_pid="$!"
  for _ in $(seq 1 50); do
    stub_addr="$(awk '/^LISTEN /{print $2; exit}' "$stub_out" 2>/dev/null || true)"
    [[ -n "$stub_addr" ]] && break
    kill -0 "$stub_pid" 2>/dev/null || { cat "$stub_out" >&2; fail "$mode stub died"; }
    sleep 0.1
  done
  [[ -n "$stub_addr" ]] || fail "$mode stub never reported its address"

  python3 "$BENCH/shim.py" --mode "$mode" --listen 127.0.0.1:0 \
    --upstream "http://$stub_addr" --log "$SHIM_WORK/$mode.log" \
    > "$shim_out" 2>&1 &
  shim_pid="$!"
  for _ in $(seq 1 50); do
    shim_addr="$(awk '/^LISTEN /{print $2; exit}' "$shim_out" 2>/dev/null || true)"
    [[ -n "$shim_addr" ]] && break
    kill -0 "$shim_pid" 2>/dev/null || { cat "$shim_out" >&2; fail "$mode shim died"; }
    sleep 0.1
  done
  [[ -n "$shim_addr" ]] || fail "$mode shim never reported its address"

  python3 - "$shim_addr" "$BENCH/tests/derive-prompt-fixture.txt" \
    "$want_kind" "$want_text" <<'PY'
import json, sys, urllib.request

addr, fixture, want_kind, want_text = sys.argv[1:5]
key = "derive-bench-stub-placeholder-key"

# Availability probe first, exactly as the adapter does (memoire:D0054).
req = urllib.request.Request(f"http://{addr}/v1/models",
                             headers={"Authorization": "Bearer " + key})
with urllib.request.urlopen(req) as resp:
    models = json.loads(resp.read())
assert [m["id"] for m in models["data"]] == ["specialist-model"], models

prompt = open(fixture, encoding="utf-8").read()
payload = {"model": "specialist-model", "max_tokens": 12288, "temperature": 0,
           "stream": False, "messages": [{"role": "user", "content": prompt}]}
req = urllib.request.Request(
    f"http://{addr}/v1/chat/completions",
    data=json.dumps(payload).encode("utf-8"),
    headers={"Content-Type": "application/json", "Authorization": "Bearer " + key},
    method="POST")
with urllib.request.urlopen(req) as resp:
    body = json.loads(resp.read())

assert body["choices"][0]["finish_reason"] == "stop", body
content = body["choices"][0]["message"]["content"]
envelope = json.loads(content)
assert envelope["schema_version"] == "1", envelope
obs = envelope["observations"]
assert len(obs) == 1, obs
assert obs[0]["kind"] == want_kind, obs[0]
assert obs[0]["text"] == want_text, obs[0]
# Grounded on a real event id from the fixture window, and carrying the
# constant confidence the shim injects for a model that self-reports none.
assert obs[0]["source_event_ids"] == [94102], obs[0]
assert obs[0]["confidence"] == 0.5, obs[0]
assert obs[0]["provenance"] == "inferred", obs[0]
assert "adapter_note" not in obs[0], obs[0]
print(f"shim end-to-end: OK ({want_kind})")
PY
  local rc=$?
  kill "$shim_pid" "$stub_pid" 2>/dev/null || true
  wait "$shim_pid" "$stub_pid" 2>/dev/null || true
  (( rc == 0 )) || fail "$mode shim end-to-end assertions"

  # The shim log is operational only: never the bearer, never payload text.
  grep -q "translated mode=$mode" "$SHIM_WORK/$mode.log" \
    || fail "$mode shim did not log its translation"
  if grep -qi "bearer\|derive-bench-stub-placeholder-key" "$SHIM_WORK/$mode.log"; then
    fail "$mode shim log leaked a credential"
  fi
}

cat > "$SHIM_WORK/nuextract.json" <<'JSON'
{
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "{\"observations\": [{\"kind\": [\"decision\"], \"text\": \"the derive profile lives on the registry entry\", \"rationale\": \"operator ruling\", \"source_event_ids\": [\"94102\"]}, {\"kind\": null, \"text\": null, \"rationale\": null, \"source_event_ids\": []}]}"
    },
    "finish_reason": "stop"
  }],
  "usage": {"prompt_tokens": 900, "completion_tokens": 60, "total_tokens": 960}
}
JSON
shim_e2e nuextract "$SHIM_WORK/nuextract.json" decision \
  "the derive profile lives on the registry entry"

cat > "$SHIM_WORK/memreader.json" <<'JSON'
{
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "<think>The window records a ruling worth storing.</think>",
      "tool_calls": [{
        "id": "call_1",
        "type": "function",
        "function": {
          "name": "add_memory",
          "arguments": "{\"memory_list\": [{\"key\": \"profile ruling\", \"memory_type\": \"LongTermMemory\", \"value\": \"the derive profile lives on the registry entry\", \"tags\": [\"decision\", \"memoire\"], \"source_event_ids\": [\"94102\"]}], \"summary\": \"derive geometry\"}"
        }
      }]
    },
    "finish_reason": "tool_calls"
  }],
  "usage": {"prompt_tokens": 900, "completion_tokens": 120, "total_tokens": 1020}
}
JSON
shim_e2e memreader "$SHIM_WORK/memreader.json" decision \
  "the derive profile lives on the registry entry"

echo "derive-bench-test: OK"
