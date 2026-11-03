#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# codex-rollout-capture.sh - guarantee tests for the Codex rollout capture-all
# adapter (scripts/memoire-capture-codex-rollout.sh) and the queue CLI batch
# input and writer it goes through (services/api/cmd/queue). The tick and the
# timer of the Codex capture are tested in scripts/tests/codex-capture-tick-test.sh.
# `make test-core` runs this script (it needs no Memoire home, no store and no
# network: everything lives in a scratch directory), and so does
# `make codex-rollout-capture-test`.
#
#   identity     ids do not depend on path, run number or machine
#   cursor-identity  a cursor survives a reboot (another st_dev and file_id),
#                a replaced appended copy and an archive move or its reverse;
#                a pre-file_id cursor is accepted on its inode and rewritten;
#                other content under the same inode is read again, logged
#   capture      capture-all: 200,000-char message, 50,000-char output,
#                nothing truncated, byte for byte, multibyte text
#   time         every event carries its record's time
#   attrib       the shared resolver runs on the session cwd
#   inherit      sub-agent inherited records: captured, flagged, never a turn
#   admit        only codex_assistant_turn is a derivation-admitted type
#   text         the turn text is the exact concatenation, nothing trimmed
#   utf8         records of 1 byte to 64 KiB made of 2, 3 and 4 byte
#                characters, placed so that every 4 KiB boundary of the file
#                falls inside a character, reach the queue byte for byte
#   turn-tail    a turn spanning several tail runs is one turn event, emitted
#                when its closing record is read
#   turn-stuck   a turn with no closing record is emitted by the adapter as a
#                partial turn (id ...:partial) only past MEMOIRE_CODEX_STUCK_S,
#                and superseded when it later closes
#   partial-id   a partial turn id never equals a terminal turn id, also when
#                an outer turn closes while an inner one is open
#   write-failure  a failed write of any event, the turn event included,
#                heals on rerun
#   segment      a segment file (ordinals far above line numbers) heals the same
#   bound, kill, term, order  the cursor is written after every batch: a
#                bound, a kill or a timeout leaves progress; files go oldest
#                change first
#   workdir      the work directory lives under $MEMOIRE_HOME/tmp, private, and
#                a killed run's directory is removed by the next run; no
#                conversation text reaches TMPDIR or /tmp
#   jq-version   jq older than 1.7 ends the run with one log line when there
#                is a file to read; with nothing to read the run exits 0
#   redaction-key  the redaction key is created once per batch
#   queue-signal a batch interrupted by SIGTERM leaves no temp file in the
#                pending directory; the temp files a SIGKILL leaves are swept
#                by the next batch (old ones only, never an item)
#   stale-temp   the sweep never removes the temp file of a live writer, even
#                one frozen (SIGSTOP) past the age bound, which completes after
#                SIGCONT; a single-item temp file goes only past 24 hours
#   token-count  event_msg/token_count is not captured
#   metadata     the top-level metadata of a record is captured
#   prov-collision  a payload holding both k and source_k keeps both
#   equiv        a full pass and any sequence of tail passes give the same ids
#                and the same content for every capture-all event and every
#                closed turn (NOT for partial turns: see the test)
#   census       synthetic fixtures (tests/foreign/fixtures/codex-census/)
#                reproduce each record type of the store format; event ids,
#                types, times, payloads and turn texts match an independent model
#
# Environment (all optional):
#   CODEX_CAPTURE_ADAPTER    adapter under test (default: the repository's)
#   CODEX_CAPTURE_QUEUE_BIN  queue binary (default: built from this checkout
#                            into the scratch directory with `go build`)
#   CODEX_CAPTURE_ONLY       space-separated test names to run (default: all)
#   CODEX_CAPTURE_KEEP       keep the scratch directory (debugging)
#   CODEX_CAPTURE_CENSUS_DIR MANUAL CHECK, never set by `make`: a directory of
#       rollouts to run through the census comparator too. Use it on a copy of
#       personal sessions, in a scratch TMPDIR, and delete the copy afterwards;
#       the comparator prints ids, types and counts, never record content.
#
# Everything runs in a scratch directory made by mktemp -d under
# ${TMPDIR:-/var/tmp} and removed at exit, with a scratch MEMOIRE_HOME, cursor
# directory, queue and HOME. No test names the real session store or
# ~/.memoire; a test that reached them would be a defect of the test.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ADAPTER="${CODEX_CAPTURE_ADAPTER:-$ROOT/scripts/memoire-capture-codex-rollout.sh}"
QUEUE_BIN="${CODEX_CAPTURE_QUEUE_BIN:-}"
RESOLVER="$ROOT/scripts/memoire-resolve-project.sh"
CENSUS_FIXTURES="$ROOT/tests/foreign/fixtures/codex-census"
ONLY="${CODEX_CAPTURE_ONLY:-}"

command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }
command -v flock >/dev/null || { echo "SKIP: flock not available"; exit 0; }
[[ -f "$ADAPTER" ]] || { echo "FAIL: adapter not found: $ADAPTER"; exit 1; }

WORK="$(mktemp -d "${TMPDIR:-/var/tmp}/codex-capture-test.XXXXXX")" || { echo "FAIL: mktemp -d"; exit 1; }
trap '[[ -n "${CODEX_CAPTURE_KEEP:-}" ]] && echo "kept: $WORK" || rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin"
if [[ -z "$QUEUE_BIN" ]]; then
  QUEUE_BIN="$WORK/bin/memoire-queue"
  command -v go >/dev/null || { echo "FAIL: go is needed to build the queue writer (or set CODEX_CAPTURE_QUEUE_BIN)"; exit 1; }
  (cd "$ROOT" && go build -tags sqlite_fts5 -o "$QUEUE_BIN" ./services/api/cmd/queue) \
    || { echo "FAIL: go build of the queue writer failed"; exit 1; }
fi
[[ -x "$QUEUE_BIN" ]] || { echo "FAIL: queue binary not found: $QUEUE_BIN"; exit 1; }

FAILURES=0
fail() { echo "FAIL: $1" >&2; FAILURES=$((FAILURES + 1)); }
check() { echo "ok: $1"; }
want() { [[ -z "$ONLY" || " $ONLY " == *" $1 "* ]]; }

# Registry: /tmp/codex-cap-proj maps to a test project; anything else falls
# back to the basename.
cat > "$WORK/registry.json" <<'JSON'
{
  "schema": "liaison-memoire-projects/1",
  "projects": {
    "codex-cap-test": { "display": "codex-cap-test", "paths": ["/tmp/codex-cap-proj"] }
  }
}
JSON

# Failure-injecting queue wrapper. FAILQ_RE: fail at the first event whose
# source_event_id matches (the events before it are written, as the real
# batch writer does); FAILQ_SLEEP: seconds to sleep per call. Handles both the
# batch input and the per-event flags, so the same test can run against an
# adapter that uses either.
cat > "$WORK/bin/failq" <<'STUB'
#!/usr/bin/env bash
real="$FAILQ_REAL"
args=("$@")
batch="" single=""
for ((i = 0; i < ${#args[@]}; i++)); do
  case "${args[i]}" in
    -batch-file) batch="${args[i+1]}" ;;
    -source-event-id) single="${args[i+1]}" ;;
  esac
done
[[ -n "${FAILQ_SLEEP:-}" ]] && sleep "$FAILQ_SLEEP"
if [[ -n "$batch" && -n "${FAILQ_RE:-}" ]]; then
  n=0
  while IFS= read -r line; do
    id="${line#*\"source_event_id\":\"}"; id="${id%%\"*}"
    [[ "$id" =~ $FAILQ_RE ]] && break
    n=$((n + 1))
  done < "$batch"
  total="$(wc -l < "$batch")"
  if (( n < total )); then
    head -n "$n" "$batch" > "$batch.pre"
    if (( n > 0 )); then
      newargs=()
      for a in "${args[@]}"; do [[ "$a" == "$batch" ]] && newargs+=("$batch.pre") || newargs+=("$a"); done
      "$real" "${newargs[@]}"
    fi
    echo "failq: forced failure at $id" >&2
    exit 1
  fi
fi
if [[ -z "$batch" && -n "${FAILQ_RE:-}" && "$single" =~ $FAILQ_RE ]]; then
  echo "failq: forced failure at $single" >&2
  exit 1
fi
exec "$real" "${args[@]}"
STUB
chmod +x "$WORK/bin/failq"

# ---- helpers ---------------------------------------------------------------------
# (qset and turns drop payload.rollout_file: it carries the path of the file
# on the machine that wrote it, so two scratch directories never agree on it.)
mkenv() { # name -> dir with home, mh, cur, tmp, ss
  local d="$WORK/$1"
  mkdir -p "$d/home" "$d/mh" "$d/cur" "$d/tmp" "$d/ss"
  printf '%s' "$d"
}

# runad DIR args... : run the adapter in DIR's scratch environment. Extra
# KEY=VALUE pairs (no spaces) arrive in $XE, a queue wrapper in $QBO.
runad() {
  local d="$1"; shift
  env -u MEMOIRE_CAPTURE -u MEMOIRE_PROJECT -u CODEX_SESSIONS_DIR -u CODEX_ARCHIVED_SESSIONS_DIR \
    HOME="$d/home" MEMOIRE_HOME="$d/mh" MEMOIRE_CAPTURE_CURSOR_DIR="$d/cur" \
    MEMOIRE_QUEUE_BIN="${QBO:-$QUEUE_BIN}" FAILQ_REAL="$QUEUE_BIN" \
    MEMOIRE_PROJECTS_REGISTRY="$WORK/registry.json" MEMOIRE_RESOLVE_PROJECT="$RESOLVER" \
    TMPDIR="$d/tmp" $XE \
    bash "$ADAPTER" "$@"
}
XE=""
QBO=""

qjson() { cat "$1"/mh/queue/pending/*.json 2>/dev/null; }
qcount() { ls "$1/mh/queue/pending" 2>/dev/null | wc -l; }
qids() { qjson "$1" | jq -r '.source_event_id' | sort -u; }
qset() { qjson "$1" | jq -cS '{id: .source_event_id, t: .event_type, at: .occurred_at, x: (.text // ""), p: (.payload | del(.rollout_file))}' | sort -u; }
turns() { qjson "$1" | jq -cS 'select(.event_type == "codex_assistant_turn") | {id: .source_event_id, at: .occurred_at, x: (.text // ""), p: (.payload | del(.rollout_file))}' | sort -u; }
qreset() { rm -rf "$1/mh/queue"; }
cursor_line() { jq -r '.line' "$1"/cur/*.json 2>/dev/null | head -1; }
cursor_count() { ls "$1/cur" 2>/dev/null | grep -c '\.json$'; }

ts() { printf '2026-06-01T%02d:%02d:%02d.000Z' $(($1 / 3600 % 24)) $(($1 / 60 % 60)) $(($1 % 60)); }
rec() { # ordinal type payload-json [extra top-level members, e.g. ,"metadata":{}]
  printf '{"timestamp":"%s","type":"%s","payload":%s,"ordinal":%s%s}\n' "$(ts "$1")" "$2" "$3" "$1" "${4:-}"
}
meta() { # thread [extra payload members]
  printf '{"timestamp":"2026-06-01T00:00:00.000Z","type":"session_meta","payload":{"id":"%s","cwd":"/tmp/codex-cap-proj","timestamp":"2026-06-01T00:00:00.000Z"%s}}\n' "$1" "${2:-}"
}
started() { rec "$1" event_msg "{\"type\":\"task_started\",\"turn_id\":\"$2\"}"; }
complete() { rec "$1" event_msg "{\"type\":\"task_complete\",\"turn_id\":\"$2\"}"; }
asst() { rec "$1" response_item "$(jq -cn --arg t "$2" '{type:"message",role:"assistant",content:[{type:"output_text",text:$t}]}')" "${3:-}"; }
usermsg() { rec "$1" response_item "$(jq -cn --arg t "$2" '{type:"message",role:"user",content:[{type:"input_text",text:$t}]}')"; }
tokencount() { rec "$1" event_msg '{"type":"token_count","info":{"n":1},"rate_limits":{}}'; }
toolcall() { rec "$1" response_item "{\"type\":\"function_call\",\"name\":\"exec\",\"arguments\":\"{}\",\"call_id\":\"c$1\"}"; }
touch_ago() { touch -d "$2" "$1"; }

# A deterministic file of N records: turns of ten records with text.
gen_records() { # thread n
  meta "$1"
  local i
  for ((i = 1; i <= $2; i++)); do
    case $((i % 10)) in
      1) started "$i" "tt$i" ;;
      2) usermsg "$i" "question $i — où ça ?" ;;
      3|7) toolcall "$i" ;;
      5) asst "$i" "réponse $i — ✓ 😀" ;;
      6) tokencount "$i" ;;
      9) complete "$i" "tt$((i - 8))" ;;
      *) rec "$i" turn_context '{"model":"m"}' ;;
    esac
  done
}

# The reference: what one full pass over a file queues, in a fresh scratch.
fullpass_set() { # file
  local d; d="$(mkenv "full-$$-$RANDOM$RANDOM")"
  runad "$d" "$1" >/dev/null 2>&1
  qset "$d"
}

# ============================================================================
# T-identity - ids do not depend on path, run number or machine
# ============================================================================
t_identity() {
  local d; d="$(mkenv identity)"
  gen_records th-id 30 > "$d/ss/rollout-a.jsonl"
  runad "$d" --tail "$d/ss/rollout-a.jsonl" >/dev/null 2>&1
  local first; first="$(qids "$d")"
  cp "$d/ss/rollout-a.jsonl" "$d/ss/renamed.jsonl"
  qreset "$d"
  runad "$d" --tail "$d/ss/renamed.jsonl" >/dev/null 2>&1
  local second; second="$(qids "$d")"
  [[ -n "$first" && "$first" == "$second" ]] \
    && check "T-identity: same ids after a rename ($(grep -c . <<<"$first") ids)" \
    || fail "T-identity: id set changed with the path"
  qreset "$d"
  rm -f "$d"/cur/*.json
  runad "$d" --tail "$d/ss/rollout-a.jsonl" >/dev/null 2>&1
  [[ "$(qids "$d")" == "$first" ]] || fail "T-identity: cursor loss changed the id set"
  qreset "$d"
  runad "$d" --tail "$d/ss/rollout-a.jsonl" >/dev/null 2>&1   # cursor present: nothing new
  [[ "$(qcount "$d")" == 0 ]] && check "T-identity: a second tail run over an unchanged file queues nothing" \
    || fail "T-identity: an unchanged file re-queued $(qcount "$d") items"
}

# ============================================================================
# T-cursor-identity - what a cursor still describes
# ============================================================================
t_cursor_identity() {
  local d f cur ino first late f0=$FAILURES
  d="$(mkenv cursor-identity)"
  f="$d/ss/rollout-a.jsonl"
  gen_records th-ci 30 > "$f"
  runad "$d" --tail "$f" >/dev/null 2>&1
  cur="$(ls "$d"/cur/*.json 2>/dev/null | head -1)"
  first="$(qids "$d")"
  ino="$(stat -c %i "$f")"
  [[ -n "$cur" && "$(jq -r .file_id "$cur")" == "$(stat -f -c %i "$f"):$ino" ]] \
    || fail "cursor-identity: file_id must be <f_fsid>:<inode> (got $(jq -r .file_id "$cur" 2>&1))"
  [[ "$(jq -r .head_len "$cur")" == 4096 \
     && "$(jq -r .head_sha "$cur")" == "$(head -c 4096 "$f" | sha256sum | cut -d' ' -f1)" ]] \
    || fail "cursor-identity: head_sha must hash the first min(4096, offset) bytes"

  # A reboot: st_dev renumbered, and a filesystem whose f_fsid moved too.
  jq -c --arg i "$ino" '.dev_inode = "999:" + $i | .file_id = "ffff:" + $i' "$cur" > "$cur.x" && mv -f "$cur.x" "$cur"
  qreset "$d"
  runad "$d" --tail "$f" >/dev/null 2>"$d/err"
  [[ "$(qcount "$d")" == 0 ]] || fail "cursor-identity: an idle file re-queued $(qcount "$d") items after a reboot"
  [[ "$(jq -r .file_id "$cur")" == "$(stat -f -c %i "$f"):$ino" ]] || fail "cursor-identity: the cursor was not rewritten with the current file_id"
  grep -q 'cursor does not match' "$d/err" && fail "cursor-identity: a matching cursor was logged as rejected"

  # A replaced, appended copy (a new inode, as a Syncthing receiver sees it).
  { cat "$f"; started 31 tt31; asst 32 "late answer"; complete 33 tt31; } > "$f.new"
  mv -f "$f.new" "$f"
  qreset "$d"
  runad "$d" --tail "$f" >/dev/null 2>&1
  late="$(qids "$d")"
  [[ -n "$late" && -z "$(comm -12 <(printf '%s\n' "$first") <(printf '%s\n' "$late"))" ]] \
    && grep -q ':31$' <<<"$late" \
    || fail "cursor-identity: a replaced appended copy must queue only the new records (got $(grep -c . <<<"$late"))"

  # A cursor written before file_id: accepted on its inode, then rewritten.
  ino="$(stat -c %i "$f")"
  jq -c --arg i "$ino" 'del(.file_id, .head_len, .head_sha) | .dev_inode = "999:" + $i' "$cur" > "$cur.x" && mv -f "$cur.x" "$cur"
  qreset "$d"
  runad "$d" --tail "$f" >/dev/null 2>&1
  [[ "$(qcount "$d")" == 0 && "$(jq -r .file_id "$cur")" == "$(stat -f -c %i "$f"):$ino" ]] \
    || fail "cursor-identity: a legacy cursor must be accepted once and rewritten (queued $(qcount "$d"))"
  jq -c 'del(.file_id, .head_len, .head_sha) | .dev_inode = "999:1"' "$cur" > "$cur.x" && mv -f "$cur.x" "$cur"
  qreset "$d"
  runad "$d" --tail "$f" >/dev/null 2>"$d/err"
  qids "$d" | grep -q '^codex:th-ci:1$' \
    && grep -q 'cursor does not match the file, full pass' "$d/err" && grep -q 'rescans=1' "$d/err" \
    || fail "cursor-identity: a legacy cursor naming another inode must give a logged full pass"

  # A copy sharing the first 4 KiB whose last consumed record changed (a sync
  # conflict winner), renamed over the file, then appended: read from line 0.
  gen_records th-ci 30 > "$f.new"
  mv -f "$f.new" "$f"
  qreset "$d"; rm -f "$d"/cur/*.json
  runad "$d" --tail "$f" >/dev/null 2>&1
  # Same length, so the offset still starts a line.
  { head -n 30 "$f"; tail -n 1 "$f" | sed 's/"model":"m"/"model":"n"/'; asst 31 "after the conflict"; } > "$f.new"
  cmp -s <(head -c 4096 "$f") <(head -c 4096 "$f.new") && ! cmp -s <(head -n 31 "$f") <(head -n 31 "$f.new") \
    && [[ "$(head -n 31 "$f.new" | wc -c)" == "$(wc -c < "$f")" ]] \
    || fail "cursor-identity: setup: the conflict copy must share the first 4 KiB and the consumed length, not the content"
  mv -f "$f.new" "$f"
  qreset "$d"
  runad "$d" --tail "$f" >/dev/null 2>"$d/err"
  qids "$d" | grep -q '^codex:th-ci:facts$' && grep -q 'cursor does not match' "$d/err" \
    || fail "cursor-identity: a copy that diverged after a shared head must be read from line 0"
  ino="$(stat -c %i "$f")"

  # Other content under the same inode (rewritten in place, longer).
  gen_records th-other 40 >| "$f"
  [[ "$(stat -c %i "$f")" == "$ino" ]] || fail "cursor-identity: setup: the rewrite changed the inode"
  qreset "$d"
  runad "$d" --tail "$f" >/dev/null 2>"$d/err"
  qids "$d" | grep -q '^codex:th-other:1$' && grep -q 'cursor does not match' "$d/err" \
    || fail "cursor-identity: other content under the same inode must be read from line 0"

  # An archive move and its reverse keep the cursor.
  local sd="$d/home/.codex/sessions/2026/06/01" ad="$d/home/.codex/archived_sessions"
  local name="rollout-2026-06-01T00-00-00-th-ci2.jsonl"
  mkdir -p "$sd" "$ad"
  gen_records th-ci2 20 > "$sd/$name"
  qreset "$d"; rm -f "$d"/cur/*.json
  runad "$d" --tail "$sd/$name" >/dev/null 2>&1
  mv "$sd/$name" "$ad/$name"
  qreset "$d"
  runad "$d" --tail --scan --since "@0" >/dev/null 2>"$d/err"
  [[ "$(qcount "$d")" == 0 && "$(cursor_count "$d")" == 1 ]] && ! grep -q 'cursor does not match' "$d/err" \
    || fail "cursor-identity: an archived rollout must keep its cursor (queued $(qcount "$d"), cursors $(cursor_count "$d"))"
  mv "$ad/$name" "$sd/$name"
  qreset "$d"
  runad "$d" --tail --scan --since "@0" >/dev/null 2>&1
  [[ "$(qcount "$d")" == 0 && "$(cursor_count "$d")" == 1 ]] \
    || fail "cursor-identity: an unarchived rollout must keep its cursor (queued $(qcount "$d"))"
  cp "$sd/$name" "$ad/$name"
  qreset "$d"
  runad "$d" --tail "$ad/$name" >/dev/null 2>&1
  [[ "$(qcount "$d")" -gt 0 && "$(cursor_count "$d")" == 2 ]] \
    || fail "cursor-identity: a copy whose source still exists must not take its cursor"
  (( FAILURES == f0 )) && check "cursor-identity: reboot, replaced copy, legacy cursor, same-inode rewrite, archive move and reverse"
}

# ============================================================================
# T-capture - nothing truncated
# ============================================================================
t_capture() {
  local d; d="$(mkenv capture)"
  local big="$WORK/big.txt" out="$WORK/out.txt"
  jq -nj '"é—😀x" * 50000' > "$big"     # 200,000 characters, 2-, 3- and 4-byte ones
  jq -nj '"ñ€𝄞y" * 12500' > "$out"       # 50,000 characters
  {
    meta th-cap
    started 1 tc1
    jq -cn --rawfile t "$big" '{timestamp:"2026-06-01T00:00:02.000Z",type:"response_item",payload:{type:"message",role:"assistant",content:[{type:"output_text",text:$t}]},ordinal:2}'
    jq -cn --rawfile o "$out" '{timestamp:"2026-06-01T00:00:03.000Z",type:"response_item",payload:{type:"function_call_output",call_id:"c",output:$o},ordinal:3}'
    rec 4 response_item '{"type":"reasoning","summary":[{"type":"summary_text","text":"gardé — kept"}],"encrypted_content":"ZZZZZZZZZZ"}'
    complete 5 tc1
  } > "$d/ss/rollout-cap.jsonl"
  runad "$d" --tail "$d/ss/rollout-cap.jsonl" >/dev/null 2>&1
  local n
  n="$(qjson "$d" | jq -r 'select(.source_event_id == "codex:th-cap:2") | .payload.content[0].text | length')"
  [[ "$n" == 200000 ]] || fail "T-capture: the 200000-char message came back as $n"
  n="$(qjson "$d" | jq -r 'select(.source_event_id == "codex:th-cap:3") | .payload.output | length')"
  [[ "$n" == 50000 ]] || fail "T-capture: the 50000-char output came back as $n"
  n="$(qjson "$d" | jq -r 'select(.event_type == "codex_assistant_turn") | .text | length')"
  [[ "$n" == 200000 ]] || fail "T-capture: the turn text has $n chars, want 200000"
  qjson "$d" | jq -e 'select(.source_event_id == "codex:th-cap:4") | .payload.encrypted_content == {encrypted: true, bytes: 10} and .payload.summary[0].text == "gardé — kept"' >/dev/null \
    || fail "T-capture: encrypted_content must become a byte count and the summary stay"
  # byte for byte: what the queue holds is the source text
  qjson "$d" | jq -j 'select(.source_event_id == "codex:th-cap:2") | .payload.content[0].text' | cmp -s - "$big" \
    || fail "T-capture: the 200000-char message differs from its source"
  qjson "$d" | jq -j 'select(.source_event_id == "codex:th-cap:3") | .payload.output' | cmp -s - "$out" \
    || fail "T-capture: the 50000-char output differs from its source"
  qjson "$d" | jq -j 'select(.event_type == "codex_assistant_turn") | .text' | cmp -s - "$big" \
    || fail "T-capture: the turn text differs from its source"
  ! qjson "$d" | grep -q 'text_truncated' || fail "T-capture: a truncation flag appeared"
  (( FAILURES == 0 )) && check "T-capture: multi-KB records pass whole, encrypted_content is a size"
}

# ============================================================================
# T-time - occurred_at is the record's time, never the run's
# ============================================================================
t_time() {
  local d; d="$(mkenv time)"
  gen_records th-time 12 > "$d/ss/rollout-t.jsonl"
  runad "$d" --tail "$d/ss/rollout-t.jsonl" >/dev/null 2>&1
  local bad
  bad="$(qjson "$d" | jq -r 'select(.source_event_id | test(":[0-9]+$")) | select(.occurred_at != ("2026-06-01T00:" + (.source_event_id | capture(":(?<o>[0-9]+)$").o | tonumber | (. / 60 | floor | tostring | if length < 2 then "0" + . else . end)) + ":" + (.source_event_id | capture(":(?<o>[0-9]+)$").o | tonumber | (. % 60 | tostring | if length < 2 then "0" + . else . end)) + ".000Z")) | .source_event_id')"
  [[ -z "$bad" ]] && check "T-time: every record event carries its record's own timestamp" \
    || fail "T-time: wrong occurred_at on: $bad"
}

# ============================================================================
# T-attrib - the resolver runs on the session cwd
# ============================================================================
t_attrib() {
  local d; d="$(mkenv attrib)"
  gen_records th-attr 3 > "$d/ss/rollout-a.jsonl"
  runad "$d" --tail "$d/ss/rollout-a.jsonl" >/dev/null 2>&1
  local projects; projects="$(qjson "$d" | jq -r '.project' | sort -u)"
  [[ "$projects" == "codex-cap-test" ]] && check "T-attrib: cwd resolved through the registry" \
    || fail "T-attrib: project was '$projects'"
  qjson "$d" | jq -e 'select(.source_event_id == "codex:th-attr:1") | .cwd == "/tmp/codex-cap-proj" and .payload.memoire_project_source == "registry"' >/dev/null \
    || fail "T-attrib: cwd and project source must ride the event"
}

# ============================================================================
# T-inherit - inherited sub-agent records
# ============================================================================
t_inherit() {
  local d; d="$(mkenv inherit)"
  {
    meta th-child ',"parent_thread_id":"th-parent","subagent_history_start_ordinal":4'
    started 1 pa
    asst 2 "mots hérités — inherited"
    complete 3 pa
    started 4 own1
    asst 5 "own — mots à moi"
    complete 6 own1
  } > "$d/ss/rollout-c.jsonl"
  runad "$d" --tail "$d/ss/rollout-c.jsonl" >/dev/null 2>&1
  local inh own
  inh="$(qjson "$d" | jq -r 'select(.payload.inherited == true) | .source_event_id' | sort | tr '\n' ' ')"
  [[ "$inh" == "codex:th-child:1 codex:th-child:2 codex:th-child:3 " ]] \
    || fail "T-inherit: inherited ids were '$inh'"
  own="$(qjson "$d" | jq -r 'select(.event_type == "codex_assistant_turn") | .source_event_id + "=" + .text')"
  [[ "$own" == "codex:th-child:t6=own — mots à moi" ]] || fail "T-inherit: turns were '$own'"
  qjson "$d" | jq -e 'select(.event_type == "codex_thread_facts") | .payload.parent_thread_id == "th-parent" and .payload.inherited_ordinal_range == [0, 4]' >/dev/null \
    || fail "T-inherit: facts must carry the parent and the inherited range"
  (( FAILURES == 0 )) && check "T-inherit: inherited records captured and flagged, never a turn"
}

# ============================================================================
# T-admit - only the turn event is a derivation-admitted type
# ============================================================================
t_admit() {
  local d; d="$(mkenv admit)"
  cp "$CENSUS_FIXTURES"/rollout-2026-05-01T09-00-00-census-alpha.jsonl "$d/ss/rollout-a.jsonl"
  runad "$d" --tail "$d/ss/rollout-a.jsonl" >/dev/null 2>&1
  local admitted='claude_UserPromptSubmit claude_PostToolUse claude_assistant_turn codex_assistant_turn qwen_assistant_turn opencode_assistant_turn pi_assistant_turn'
  local types t bad=""
  types="$(qjson "$d" | jq -r '.event_type' | sort -u)"
  for t in $types; do [[ " $admitted " == *" $t "* && "$t" != codex_assistant_turn ]] && bad="$bad $t"; done
  [[ -z "$bad" ]] || fail "T-admit: capture-all events use admitted types:$bad"
  [[ "$types" == *codex_assistant_turn* ]] && check "T-admit: the turn event is the only admitted type ($(grep -c . <<<"$types") types)" \
    || fail "T-admit: no turn event"
  # Only turn events carry text.
  local texty
  texty="$(qjson "$d" | jq -r 'select((.text // "") != "" and .event_type != "codex_assistant_turn") | .source_event_id')"
  [[ -z "$texty" ]] || fail "T-admit: text on non-turn events: $texty"
}

# ============================================================================
# T-text - the turn text is the exact concatenation (nothing trimmed)
# ============================================================================
t_text() {
  local d; d="$(mkenv text)"
  {
    meta th-text
    started 1 tx
    asst 2 $'première ligne — é\n'
    asst 3 $'second\n\n\ntroisième 😀'
    asst 4 $'nul\u0000byte ñ\n'
    asst 5 $'  padded end €\n\n'
    complete 6 tx
  } > "$d/ss/rollout-x.jsonl"
  runad "$d" --tail "$d/ss/rollout-x.jsonl" >/dev/null 2>&1
  # Independent expectation: the message texts joined by a blank line.
  jq -j -s '[.[] | select(.type == "response_item") | .payload.content[0].text] | join("\n\n")' "$d/ss/rollout-x.jsonl" > "$d/expected.txt" 2>/dev/null
  # (session_meta line has no .payload.content: filtered by the select)
  qjson "$d" | jq -j 'select(.event_type == "codex_assistant_turn") | .text' > "$d/actual.txt"
  if [[ -s "$d/expected.txt" ]] && cmp -s "$d/expected.txt" "$d/actual.txt"; then
    check "T-text: the turn text is the exact concatenation ($(wc -c < "$d/actual.txt") bytes, newlines and NUL kept)"
  else
    fail "T-text: turn text differs from the joined messages ($(wc -c < "$d/expected.txt") expected, $(wc -c < "$d/actual.txt") actual bytes)"
  fi
}

# ============================================================================
# turn-tail - a turn spanning several tail runs
# ============================================================================
t_turn_tail() {
  local d f
  d="$(mkenv turntail)"; f="$d/ss/rollout-tail.jsonl"
  { meta th-tail; started 1 tA; asst 2 "part é A"; tokencount 3; } > "$f"
  runad "$d" --tail "$f" >/dev/null 2>&1
  { asst 4 "part — B"; toolcall 5; } >> "$f"
  runad "$d" --tail "$f" >/dev/null 2>&1
  local open_turns; open_turns="$(turns "$d" | grep -c .)"
  [[ "$open_turns" == 0 ]] || fail "turn-tail: a turn event was emitted before its closing record was read ($open_turns)"
  { asst 6 "part 😀 C"; complete 7 tA; } >> "$f"
  runad "$d" --tail "$f" >/dev/null 2>&1
  local want_text=$'part é A\n\npart — B\n\npart 😀 C'
  local got; got="$(turns "$d" | jq -r '.id + "|" + .x' | sed 's/^/ /')"
  [[ "$(turns "$d" | grep -c .)" == 1 && "$(turns "$d" | jq -r '.id')" == "codex:th-tail:t7" ]] \
    || fail "turn-tail: want exactly one turn event codex:th-tail:t7, got: $(turns "$d" | jq -r '.id' | tr '\n' ' ')"
  [[ "$(turns "$d" | jq -r '.x')" == "$want_text" ]] || fail "turn-tail: the turn text lost a part: $got"
  local tailset fullset
  tailset="$(qset "$d")"; fullset="$(fullpass_set "$f")"
  [[ "$tailset" == "$fullset" ]] && check "turn-tail: three tail runs give one turn event, the full text, the full-pass id set and content" \
    || fail "turn-tail: tail runs and a full pass differ:
$(diff <(echo "$tailset") <(echo "$fullset") | cut -c1-200 | head -8)"
}

# turn-tail, the stuck rule: the adapter emits an open turn only when the file is idle
t_turn_stuck() {
  local d f
  d="$(mkenv turnstuck)"; f="$d/ss/rollout-s.jsonl"
  { meta th-stk; started 1 tS; asst 2 "avant la pause — é"; toolcall 3; } > "$f"
  runad "$d" --tail "$f" >/dev/null 2>&1
  [[ "$(turns "$d" | grep -c .)" == 0 ]] || fail "turn-stuck: an open turn on a fresh file must wait"
  touch_ago "$f" '3 hours ago'
  runad "$d" --tail "$f" >/dev/null 2>&1
  local partial; partial="$(turns "$d" | jq -r 'select(.p.closed_by == "adapter") | .id')"
  [[ "$partial" == "codex:th-stk:t3:partial" ]] || fail "turn-stuck: want the partial turn codex:th-stk:t3:partial, got '$partial'"
  turns "$d" | jq -e 'select(.p.closed_by == "adapter") | .x == "avant la pause — é" and .at == "'"$(ts 3)"'"' >/dev/null \
    || fail "turn-stuck: the partial turn must carry its text and the time of its last record"
  local n1; n1="$(qcount "$d")"
  runad "$d" --tail "$f" >/dev/null 2>&1
  [[ "$(qcount "$d")" == "$n1" ]] || fail "turn-stuck: a second run on the idle file queued $(($(qcount "$d") - n1)) more items"
  { asst 4 "après la pause — ñ"; complete 5 tS; } >> "$f"
  runad "$d" --tail "$f" >/dev/null 2>&1
  local full; full="$(turns "$d" | jq -c 'select(.p.closed_by == "terminal")')"
  [[ "$(jq -r '.id' <<<"$full")" == "codex:th-stk:t5" \
     && "$(jq -r '.x' <<<"$full")" == $'avant la pause — é\n\naprès la pause — ñ' \
     && "$(jq -r '.p.supersedes' <<<"$full")" == "codex:th-stk:t3:partial" ]] \
    && check "turn-stuck: idle turn emitted by the adapter once, the full turn supersedes it; both are visible" \
    || fail "turn-stuck: the closing turn was: $full"
  [[ "$(turns "$d" | grep -c .)" == 2 ]] || fail "turn-stuck: both turn events must exist"
  # The documented exception to "a full pass equals any tail sequence": the
  # partial turn and the supersedes link exist only because a run saw the file
  # idle; a full pass over the final file sees a closed turn and has neither.
  local fullset; fullset="$(fullpass_set "$f")"
  if grep -q ':partial' <<<"$fullset" || grep -q '"supersedes"' <<<"$fullset"; then
    fail "turn-stuck: a full pass over the closed file must carry no partial turn and no supersedes link"
  else
    check "turn-stuck: the partial turn and the supersedes link are the accepted difference between the tail sequence and a full pass"
  fi
}

# ============================================================================
# write-failure - a failed write heals, whichever event it hits (the turn event included)
# ============================================================================
wf_fixture() { { meta th-wfail; started 1 tF; asst 2 "réponse — é"; complete 3 tF; usermsg 4 "suivant ñ"; toolcall 5; asst 6 "loose €"; }; }
t_write_failure() {
  local d f ref id ok=1 out
  d="$(mkenv wfref)"; f="$d/ss/rollout-wf.jsonl"; wf_fixture > "$f"
  ref="$(fullpass_set "$f")"
  for id in facts 1 2 3 t3 4 6; do
    d="$(mkenv "wf-$id")"; f="$d/ss/rollout-wf.jsonl"; wf_fixture > "$f"
    QBO="$WORK/bin/failq" XE="FAILQ_RE=^codex:th-wfail:$id\$" runad "$d" --tail "$f" >/dev/null 2>&1
    (( $? == 0 )) && { fail "write-failure: a failed write of $id must make the run exit non-zero"; ok=0; }
    runad "$d" --tail "$f" >/dev/null 2>&1
    out="$(qset "$d")"
    if [[ "$out" != "$ref" ]]; then
      fail "write-failure: after a failure at $id and a healed rerun the queue differs from a full pass: missing ids [$(comm -13 <(echo "$out" | jq -r .id | sort -u) <(echo "$ref" | jq -r .id | sort -u) | tr '\n' ' ')] $(diff <(echo "$out" | cut -c1-200) <(echo "$ref" | cut -c1-200) | head -4)"
      ok=0
    fi
  done
  (( ok == 1 )) && check "write-failure: a failed write at any of 7 positions, the turn event included, heals to the full set"
  # And in a non-tail (full pass) run.
  d="$(mkenv wffull)"; f="$d/ss/rollout-wf.jsonl"; wf_fixture > "$f"
  QBO="$WORK/bin/failq" XE="FAILQ_RE=^codex:th-wfail:t3\$" runad "$d" "$f" >/dev/null 2>&1
  runad "$d" --tail "$f" >/dev/null 2>&1
  [[ "$(qset "$d")" == "$ref" ]] || fail "write-failure: a full pass that failed at the turn event did not heal"
}

# ============================================================================
# segment - segment files: ordinals are not lines
# ============================================================================
seg_fixture() {
  printf '{"timestamp":"2026-06-01T00:00:00.000Z","type":"session_meta","payload":{"id":"th-seg","cwd":"/tmp/codex-cap-proj","history_base":{"thread_id":"th-segf","end_ordinal_exclusive":100,"end_byte_offset":1234}}}\n'
  started 100 tG; asst 101 "seg réponse — é"; complete 102 tG; usermsg 103 "seg suite ñ"; toolcall 104; asst 105 "seg fin €"
}
t_segment() {
  local d f ref out id ok=1 line
  d="$(mkenv segref)"; f="$d/ss/rollout-seg.jsonl"; seg_fixture > "$f"
  ref="$(fullpass_set "$f")"
  [[ "$(jq -r .id <<<"$(head -1 <<<"$ref")")" == codex:th-segf:* ]] || fail "segment: ids must use the base thread"
  for id in 101 103 105; do
    d="$(mkenv "seg-$id")"; f="$d/ss/rollout-seg.jsonl"; seg_fixture > "$f"
    QBO="$WORK/bin/failq" XE="FAILQ_RE=^codex:th-segf:$id\$" runad "$d" --tail "$f" >/dev/null 2>&1
    line="$(cursor_line "$d")"
    if [[ -n "$line" ]] && (( line > $(wc -l < "$f") )); then
      fail "segment: the cursor line $line is an ordinal (the file has $(wc -l < "$f") lines)"; ok=0
    fi
    runad "$d" --tail "$f" >/dev/null 2>&1
    out="$(qset "$d")"
    if [[ "$out" != "$ref" ]]; then
      fail "segment: failure at ordinal $id in a segment file lost: $(comm -13 <(echo "$out" | jq -r .id | sort -u) <(echo "$ref" | jq -r .id | sort -u) | tr '\n' ' ')"
      ok=0
    fi
  done
  (( ok == 1 )) && check "segment: a segment file heals at three positions; the cursor counts lines"
}

# ============================================================================
# bounds - the cursor moves after every batch; bounds, kills, ordering
# ============================================================================
t_bound() {
  local d f ref line prev=0 runs=0 rc
  d="$(mkenv bound)"; f="$d/ss/rollout-big.jsonl"; gen_records th-bound 600 > "$f"
  ref="$(fullpass_set "$f")"
  local total; total="$(wc -l < "$f")"
  local started ended
  started=$SECONDS
  QBO="$WORK/bin/failq" XE="FAILQ_SLEEP=0.4 MEMOIRE_CODEX_BATCH_RECORDS=40" runad "$d" --tail --max-seconds 2 "$f" >"$d/o1" 2>"$d/e1"
  rc=$?
  ended=$SECONDS
  line="$(cursor_line "$d")"
  [[ $rc == 0 ]] || fail "bounds: a run that reaches its time bound must exit 0 (rc=$rc)"
  [[ -n "$line" ]] && (( line > 0 && line < total )) || fail "bounds: after a bounded run the cursor must sit inside the file (line='$line' of $total)"
  grep -Eq "line $line, [0-9]+ lines left" "$d/e1" || fail "bounds: the stop must log the file, the line reached and the lines left: $(tail -3 "$d/e1")"
  (( ended - started <= 8 )) || fail "bounds: the bounded run took $((ended - started)) s"
  prev="${line:-0}"
  while (( runs < 40 )); do
    runs=$((runs + 1))
    QBO="$WORK/bin/failq" XE="FAILQ_SLEEP=0.05 MEMOIRE_CODEX_BATCH_RECORDS=40" runad "$d" --tail --max-seconds 1 "$f" >/dev/null 2>&1
    line="$(cursor_line "$d")"
    [[ -n "$line" ]] && (( line >= prev )) || { fail "bounds: the cursor went backwards ($prev -> $line)"; break; }
    (( line >= total )) && break
    prev="$line"
  done
  [[ "$(qset "$d")" == "$ref" ]] && check "bounds: bounded runs progress and finish in $((runs + 1)) runs with the full-pass set" \
    || fail "bounds: bounded runs did not reach the full set ($(qset "$d" | wc -l) vs $(echo "$ref" | wc -l) items)"
}

t_kill() {
  local d f ref line prev=0 runs=0 total
  d="$(mkenv kill)"; f="$d/ss/rollout-big.jsonl"; gen_records th-kill 500 > "$f"
  ref="$(fullpass_set "$f")"
  total="$(wc -l < "$f")"
  local first=""
  while (( runs < 60 )); do
    runs=$((runs + 1))
    (
      timeout -s KILL 1.6 env -u MEMOIRE_CAPTURE HOME="$d/home" MEMOIRE_HOME="$d/mh" MEMOIRE_CAPTURE_CURSOR_DIR="$d/cur" \
        MEMOIRE_QUEUE_BIN="$WORK/bin/failq" FAILQ_REAL="$QUEUE_BIN" FAILQ_SLEEP=0.3 MEMOIRE_CODEX_BATCH_RECORDS=25 \
        MEMOIRE_PROJECTS_REGISTRY="$WORK/registry.json" MEMOIRE_RESOLVE_PROJECT="$RESOLVER" TMPDIR="$d/tmp" \
        bash "$ADAPTER" --tail "$f" >/dev/null 2>&1
      : # (not the last command: the subshell must not be the process timeout kills)
    ) 2>/dev/null
    sleep 0.5   # let orphaned children of the killed run finish
    line="$(cursor_line "$d")"
    [[ -z "$first" ]] && first="${line:-none}"
    [[ -n "$line" ]] && (( line >= prev )) || { fail "bounds: the cursor went backwards after a kill ($prev -> $line)"; break; }
    (( line >= total )) && break
    prev="${line:-0}"
  done
  [[ "$first" != none ]] && (( first > 0 )) || fail "bounds: a run killed after 1.6 s left no cursor (first='$first'): the next run would start again"
  [[ "$(qset "$d")" == "$ref" ]] && check "bounds: runs killed with SIGKILL every 1.6 s still converge on the full set ($runs runs, first cursor at line $first)" \
    || fail "bounds: killed runs did not converge ($(qset "$d" | wc -l) vs $(echo "$ref" | wc -l) items after $runs runs)"
}

t_term() {
  local d f rc line
  d="$(mkenv term)"; f="$d/ss/rollout-big.jsonl"; gen_records th-term 300 > "$f"
  (
    timeout 1.5 env -u MEMOIRE_CAPTURE HOME="$d/home" MEMOIRE_HOME="$d/mh" MEMOIRE_CAPTURE_CURSOR_DIR="$d/cur" \
      MEMOIRE_QUEUE_BIN="$WORK/bin/failq" FAILQ_REAL="$QUEUE_BIN" FAILQ_SLEEP=0.3 MEMOIRE_CODEX_BATCH_RECORDS=20 \
      MEMOIRE_PROJECTS_REGISTRY="$WORK/registry.json" MEMOIRE_RESOLVE_PROJECT="$RESOLVER" TMPDIR="$d/tmp" \
      bash "$ADAPTER" --tail "$f" >/dev/null 2>&1
    echo $? > "$d/rc"
  ) 2>/dev/null
  sleep 0.5
  rc="$(cat "$d/rc")"; line="$(cursor_line "$d")"
  [[ "$rc" == 124 ]] || fail "bounds: timeout's own status should be 124, got $rc"
  [[ -n "$line" ]] && (( line > 0 )) || fail "bounds: a run ended by SIGTERM left no cursor (line='$line')"
  [[ -z "$(ls -A "$d/tmp")" ]] || fail "bounds: a run ended by SIGTERM left files in TMPDIR: $(ls -A "$d/tmp" | tr '\n' ' ')"
  [[ -z "$(ls -a "$d/mh/queue/pending" 2>/dev/null | grep '^\.[^.]')" ]] || fail "bounds: SIGTERM left temp files in the queue directory"
  check "bounds: a run ended by SIGTERM (timeout) keeps its progress (cursor at line $line) and leaves no temp file"
}

t_order() {
  local d fa fb
  d="$(mkenv order)"; fa="$d/ss/rollout-a-newer.jsonl"; fb="$d/ss/rollout-b-older.jsonl"
  gen_records th-newer 120 > "$fa"; gen_records th-older 120 > "$fb"
  touch_ago "$fb" '5 hours ago'; touch_ago "$fa" '4 hours ago'
  local n
  for n in 1 2; do
    XE="MEMOIRE_CODEX_BATCH_RECORDS=30" runad "$d" --tail --scan --sessions-dir "$d/ss" --archived-sessions-dir "$d/noarch" --max-events 3 >/dev/null 2>&1
    if [[ -n "$(ls "$d/cur" 2>/dev/null | grep -v lock)" ]]; then :; fi
  done
  local older_ids newer_ids
  older_ids="$(qids "$d" | grep -c ':th-older:')"; newer_ids="$(qids "$d" | grep -c ':th-newer:')"
  (( older_ids > 0 && newer_ids == 0 )) \
    && check "bounds: oldest change first; the unfinished file keeps its place across runs ($older_ids ids of the older file, none of the newer)" \
    || fail "bounds: order or place lost: older=$older_ids newer=$newer_ids (want older>0, newer=0)"
}

# ============================================================================
# token-count / metadata - mapping details
# ============================================================================
t_token_count() {
  local d; d="$(mkenv tokcount)"
  { meta th-tokc; tokencount 1; started 2 t7; asst 3 "x é"; tokencount 4; complete 5 t7; } > "$d/ss/rollout-a.jsonl"
  runad "$d" --tail "$d/ss/rollout-a.jsonl" >/dev/null 2>&1
  local n; n="$(qjson "$d" | jq -r 'select(.payload.record_payload_type == "token_count") | .source_event_id' | grep -c .)"
  [[ "$n" == 0 ]] && ! qids "$d" | grep -Eq ':(1|4)$' \
    && check "token-count: event_msg/token_count records are not captured" \
    || fail "token-count: $n token_count event(s) captured"
}
t_metadata() {
  local d; d="$(mkenv meta)"
  {
    meta th-meta
    usermsg 1 "bonjour — hello"
    rec 2 response_item '{"type":"message","role":"assistant","content":[{"type":"output_text","text":"à"}]}' ',"metadata":{"client_authored":false,"retained_source":"x—é","nested":{"encrypted_content":"QQQQ"}},"zz_future_key":[1,2]'
  } > "$d/ss/rollout-a.jsonl"
  runad "$d" --tail "$d/ss/rollout-a.jsonl" >/dev/null 2>&1
  qjson "$d" | jq -e 'select(.source_event_id == "codex:th-meta:2") | .payload.record_metadata == {client_authored: false, retained_source: "x—é", nested: {encrypted_content: {encrypted: true, bytes: 4}}} and .payload.record_extra == {zz_future_key: [1, 2]}' >/dev/null \
    && check "metadata: the top-level metadata (and any other extra key) of a record is captured" \
    || fail "metadata: record_metadata/record_extra missing or wrong: $(qjson "$d" | jq -c 'select(.source_event_id == "codex:th-meta:2") | .payload | {record_metadata, record_extra}')"
}

t_prov_collision() {
  local d; d="$(mkenv provcol)"
  {
    meta th-pc
    usermsg 1 "bonjour"
    rec 2 response_item '{"type":"message","role":"assistant","content":[{"type":"output_text","text":"à"}],"thread_id":"own-A","source_thread_id":"own-B","source_source_thread_id":"own-C","line":"own-L"}'
  } > "$d/ss/rollout-a.jsonl"
  runad "$d" --tail "$d/ss/rollout-a.jsonl" >/dev/null 2>&1
  qjson "$d" | jq -e 'select(.source_event_id == "codex:th-pc:2") | .payload
      | .thread_id == "th-pc" and .source_thread_id == "own-A" and .source_source_thread_id == "own-B" and .source_source_source_thread_id == "own-C"
        and .line == 2 and .source_line == "own-L" and (has("source_source_line") | not)' >/dev/null \
    && check "prov-collision: a payload holding both k and source_k keeps both (source_k, source_source_k), nothing overwritten" \
    || fail "prov-collision: a provenance key collision lost a value: $(qjson "$d" | jq -c 'select(.source_event_id == "codex:th-pc:2") | .payload | with_entries(select(.key | test("thread_id|line")))')"
}

# ============================================================================
# utf8 - what reaches the queue is the source, byte for byte, whatever the
# characters and wherever a 4 KiB boundary of the file falls
# ============================================================================
# The generator lays out a rollout of records of about 1 byte to 64 KiB of
# content (assistant messages, tool outputs, prompts, reasoning summaries, and
# lines that are not JSON at all) made of 2-, 3- and 4-byte characters. It
# follows the byte offset of the file while it writes, so that no character
# ends exactly on a multiple of 4096 and every multiple of 4096 (the read size
# that used to split characters) falls INSIDE a character - also when it would
# otherwise have fallen in the JSON around a small record (the previous record
# is then made longer). The test checks that property on the produced file
# before it runs the adapter.
cat > "$WORK/utf8gen.jq" <<'JQ'
def W: [2,3,4,3,4,2];
def CH: {"2": ["é","ß","ñ"], "3": ["—","€","中"], "4": ["😀","𝄞","🚀"]};
def nextb($off): (($off / 4096 | floor) + 1) * 4096;
def ts: "2026-06-01T00:00:00.000Z";
def fixed($k): $k == "meta" or $k == "start" or $k == "end";
def full($k; $j):
  if $k == "meta" then "{\"timestamp\":\"" + ts + "\",\"type\":\"session_meta\",\"payload\":{\"id\":\"th-utf8\",\"cwd\":\"/tmp/codex-cap-proj\",\"timestamp\":\"" + ts + "\"}}\n"
  elif $k == "start" then "{\"timestamp\":\"" + ts + "\",\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":\"tU\"},\"ordinal\":\($j)}\n"
  else "{\"timestamp\":\"" + ts + "\",\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"turn_id\":\"tU\"},\"ordinal\":\($j)}\n" end;
def tpl($k; $j):
  if $k == "asst" then ["{\"timestamp\":\"" + ts + "\",\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"", "\"}]},\"ordinal\":\($j)}\n"]
  elif $k == "user" then ["{\"timestamp\":\"" + ts + "\",\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"", "\"}]},\"ordinal\":\($j)}\n"]
  elif $k == "tool" then ["{\"timestamp\":\"" + ts + "\",\"type\":\"response_item\",\"payload\":{\"type\":\"function_call_output\",\"call_id\":\"c\($j)\",\"output\":\"", "\"},\"ordinal\":\($j)}\n"]
  elif $k == "reason" then ["{\"timestamp\":\"" + ts + "\",\"type\":\"response_item\",\"payload\":{\"type\":\"reasoning\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"", "\"}]},\"ordinal\":\($j)}\n"]
  else ["NOT JSON: ", "\n"] end;
# bytes between the end of the content of record $j and the start of the next content
def gapfor($specs; $j):
  (tpl($specs[$j][0]; $j)[1] | utf8bytelength) as $s
  | ([range($j + 1; $specs | length)]
     | reduce .[] as $m ({acc: 0, done: false};
         if .done then .
         elif fixed($specs[$m][0]) then .acc += (full($specs[$m][0]; $m) | utf8bytelength)
         else .acc += (tpl($specs[$m][0]; $m)[0] | utf8bytelength) | .done = true end) | .acc) as $rest
  | $s + $rest;
def build($start; $n; $gap):
  {a: [], off: $start, i: 0, c: {"2": 0, "3": 0, "4": 0}}
  | until((.off - $start) >= $n and nextb(.off) > (.off + $gap);
      (W[.i % 6]) as $w
      | if ((.off % 4096) + $w) == 4096
        then .a += ["a"] | .off += 1
        else .a += [CH[($w | tostring)][(.i / 6 | floor) % 3]] | .off += $w | .i += 1 | .c[($w | tostring)] += 1 end)
  | {s: (.a | join("")), end: .off, c: .c};
[["meta",0],["start",0],
 ["asst",1],["asst",2],["asst",3],["asst",5],["asst",9],["asst",100],["asst",1000],["asst",4095],
 ["asst",4096],["asst",4097],["asst",8191],["asst",20000],["asst",65535],["asst",65536],
 ["end",0],
 ["tool",1],["tool",4096],["tool",65536],["tool",100],
 ["user",7],["user",5000],["reason",3],["reason",9000],
 ["raw",1],["raw",4097],["raw",30000]] as $specs
| reduce range(0; $specs | length) as $j ({off: 0, out: [], man: [], c: {"2": 0, "3": 0, "4": 0}};
    $specs[$j] as [$k, $n]
    | if fixed($k)
      then full($k; $j) as $t | .out += [$t] | .off += ($t | utf8bytelength)
      else tpl($k; $j) as [$h, $sf]
        | (.off + ($h | utf8bytelength)) as $c0
        | build($c0; $n; gapfor($specs; $j)) as $b
        | .out += [$h, $b.s, $sf]
        | .off = $b.end + ($sf | utf8bytelength)
        | .man += ["\($j) \($k) \($b.end - $c0)"]
        | .c["2"] += $b.c["2"] | .c["3"] += $b.c["3"] | .c["4"] += $b.c["4"]
      end)
| {file: (.out | join("")), manifest: .man, chars: .c}
JQ

utf8_check() { # dir label : compare every record of the source with the queue
  local d="$1" label="$2" f="$1/ss/rollout-u.jsonl" j k bytes jp n=0 bad=0
  while read -r j k bytes; do
    n=$((n + 1))
    case "$k" in
      asst|user) jp='.payload.content[0].text' ;;
      tool) jp='.payload.output' ;;
      reason) jp='.payload.summary[0].text' ;;
      raw) jp='' ;;
    esac
    if [[ "$k" == raw ]]; then
      sed -n "$((j + 1))p" "$f" > "$d/src.txt"
      qjson "$d" | jq -j "select(.source_event_id == \"codex:th-utf8:ln$j\" and .payload.unparsed_line == true) | .payload.raw" > "$d/got.txt"
      printf '\n' >> "$d/got.txt"
    else
      sed -n "$((j + 1))p" "$f" | jq -j "$jp" > "$d/src.txt"
      qjson "$d" | jq -j "select(.source_event_id == \"codex:th-utf8:$j\") | $jp" > "$d/got.txt"
    fi
    if ! cmp -s "$d/src.txt" "$d/got.txt"; then
      bad=$((bad + 1)); fail "utf8 ($label): line $j ($k, $bytes content bytes) differs from its source (first difference: $(cmp "$d/src.txt" "$d/got.txt" 2>&1 | grep -o 'byte [0-9]*' | head -1))"
    fi
  done < "$d/manifest.txt"
  # the turn text: the assistant messages joined by a blank line
  local a
  for a in $(grep ' asst ' "$d/manifest.txt" | cut -d' ' -f1); do sed -n "$((a + 1))p" "$f"; done \
    | jq -j -s '[.[] | .payload.content[0].text] | join("\n\n")' > "$d/src.txt"
  qjson "$d" | jq -j 'select(.event_type == "codex_assistant_turn") | .text' > "$d/got.txt"
  cmp -s "$d/src.txt" "$d/got.txt" || { bad=$((bad + 1)); fail "utf8 ($label): the turn text differs from its source ($(wc -c < "$d/src.txt") vs $(wc -c < "$d/got.txt") bytes)"; }
  # no character was replaced anywhere in the queue
  local fffd; fffd="$(qjson "$d" | grep -c $'\xef\xbf\xbd')"
  [[ "$fffd" == 0 ]] || { bad=$((bad + 1)); fail "utf8 ($label): $fffd queue line(s) carry U+FFFD"; }
  (( bad == 0 )) && echo "$n"
}
t_utf8() {
  local d f size nb=0 badb=0 v
  d="$(mkenv utf8)"; f="$d/ss/rollout-u.jsonl"
  jq -n -c -f "$WORK/utf8gen.jq" > "$d/gen.json" || { fail "utf8: the generator failed"; return; }
  jq -j '.file' "$d/gen.json" > "$f"
  jq -r '.manifest[]' "$d/gen.json" > "$d/manifest.txt"
  size="$(wc -c < "$f")"
  # every 4096-multiple of the file is a continuation byte (inside a character)
  while read -r v; do
    nb=$((nb + 1)); (( v >= 128 && v <= 191 )) || badb=$((badb + 1))
  done < <(od -An -v -tu1 -w4096 "$f" | sed -n '2,$p' | sed 's/^ *//; s/ .*//')
  (( badb == 0 && nb >= 60 && size > 250000 )) \
    || { fail "utf8: the fixture is wrong: $badb of $nb boundaries outside a character, $size bytes"; return; }
  jq -e '.chars["2"] > 100 and .chars["3"] > 100 and .chars["4"] > 100' "$d/gen.json" >/dev/null \
    || { fail "utf8: the fixture lacks 2-, 3- or 4-byte characters: $(jq -c .chars "$d/gen.json")"; return; }
  # one pass, one chunk: the chunk file has the boundaries of the source
  runad "$d" --tail "$f" >/dev/null 2>&1
  local n1 n2
  n1="$(utf8_check "$d" "one chunk")"
  # the same file again in chunks of 5 records, and by a full (non-tail) pass
  qreset "$d"; rm -f "$d"/cur/*
  XE="MEMOIRE_CODEX_BATCH_RECORDS=5" runad "$d" --tail "$f" >/dev/null 2>&1
  n2="$(utf8_check "$d" "chunks of 5")"
  qreset "$d"; rm -f "$d"/cur/*
  runad "$d" "$f" >/dev/null 2>&1
  utf8_check "$d" "full pass" >/dev/null
  [[ -n "$n1" && -n "$n2" ]] \
    && check "utf8: $n1 records of 1 byte to 64 KiB (2-, 3-, 4-byte characters; all $nb 4 KiB boundaries of a $size-byte file inside a character) reach the queue byte for byte, in one chunk, in chunks of 5 and in a full pass"
}

# ============================================================================
# partial-id - a partial turn id never equals a terminal turn id
# ============================================================================
t_partial_id() {
  local d f
  d="$(mkenv partialid)"; f="$d/ss/rollout-o.jsonl"
  # An outer turn (tA) and an inner one (tB) are open; the OUTER one closes
  # while the inner one is still open. The record that closes tA (ordinal 5)
  # is also the last record tB has seen.
  { meta th-ooo; started 1 tA; asst 2 "a-texte — é"; started 3 tB; asst 4 "b-texte — ñ"; complete 5 tA; } > "$f"
  touch_ago "$f" '2 hours ago'
  runad "$d" --tail "$f" >/dev/null 2>&1
  local ids
  ids="$(turns "$d" | jq -r '.id' | sort | tr '\n' ' ')"
  [[ "$ids" == "codex:th-ooo:t5 codex:th-ooo:t5:partial " ]] \
    || fail "partial-id: want the terminal codex:th-ooo:t5 and the partial codex:th-ooo:t5:partial, got: $ids"
  turns "$d" | jq -e 'select(.id == "codex:th-ooo:t5") | .x == "a-texte — é" and .p.closed_by == "terminal"' >/dev/null \
    || fail "partial-id: the terminal turn t5 must be tA's text"
  turns "$d" | jq -e 'select(.id == "codex:th-ooo:t5:partial") | .x == "b-texte — ñ" and .p.closed_by == "adapter"' >/dev/null \
    || fail "partial-id: the partial turn must be tB's text, closed_by adapter"
  # tB continues and closes: the full turn supersedes the PARTIAL id.
  { asst 6 "b-suite 😀"; complete 7 tB; } >> "$f"
  runad "$d" --tail "$f" >/dev/null 2>&1
  turns "$d" | jq -e 'select(.id == "codex:th-ooo:t7") | .x == "b-texte — ñ\n\nb-suite 😀" and .p.supersedes == "codex:th-ooo:t5:partial"' >/dev/null \
    || fail "partial-id: the closing turn must supersede codex:th-ooo:t5:partial: $(turns "$d" | jq -c 'select(.id == "codex:th-ooo:t7")')"
  # No id is used by two different events.
  local dup
  dup="$(qjson "$d" | jq -r '.source_event_id' | sort | uniq -d)"
  [[ -z "$dup" ]] || fail "partial-id: ids used twice: $dup"
  (( FAILURES == 0 )) && check "partial-id: outer turn closes while an inner one is open: t5 (terminal) and t5:partial stay distinct, t7 supersedes the partial"
}

# ============================================================================
# workdir - private work directory under the Memoire home, cleared after a kill
# ============================================================================
wd_runs() { ls "$1/mh/tmp/codex-capture" 2>/dev/null | grep -c '^run\.'; }
wd_wait() { # dir : wait (5 s) until a run has its events file
  local i
  for ((i = 0; i < 100; i++)); do
    ls "$1"/mh/tmp/codex-capture/run.*/events >/dev/null 2>&1 && return 0
    sleep 0.05
  done
  return 1
}
t_workdir() {
  local d f base pid marker="zqxmarker$$$RANDOM"
  d="$(mkenv workdir)"; f="$d/ss/rollout-w.jsonl"; base="$d/mh/tmp/codex-capture"
  { gen_records th-wd 120; asst 121 "texte secret $marker é"; } > "$f"
  touch "$d/stamp"
  # 1. a run in flight: private directory, private files, no copy in TMPDIR or /tmp
  ( QBO="$WORK/bin/failq" XE="FAILQ_SLEEP=2 MEMOIRE_CODEX_BATCH_RECORDS=1000" runad "$d" --tail "$f" >/dev/null 2>&1 ) &
  pid=$!
  if ! wd_wait "$d"; then fail "workdir: no work directory appeared under $base"; wait "$pid"; return; fi
  [[ "$(stat -c %a "$d/mh/tmp")" == 700 && "$(stat -c %a "$base")" == 700 ]] || fail "workdir: $d/mh/tmp and codex-capture must be mode 700"
  [[ -z "$(find "$base" -mindepth 1 -type d ! -perm 700 2>/dev/null)" ]] || fail "workdir: a work directory is not mode 700"
  [[ -z "$(find "$base" -type f ! -perm 600 2>/dev/null)" ]] || fail "workdir: a work file is not mode 600: $(find "$base" -type f ! -perm 600 | head -3 | tr '\n' ' ')"
  grep -rlq "$marker" "$base" 2>/dev/null || fail "workdir: the conversation copy is not where it should be ($base)"
  [[ -z "$(ls -A "$d/tmp")" ]] || fail "workdir: TMPDIR was used: $(ls -A "$d/tmp" | tr '\n' ' ')"
  [[ -z "$(find /tmp -maxdepth 3 -type f -newer "$d/stamp" 2>/dev/null | xargs -r grep -l "$marker" 2>/dev/null)" ]] \
    || fail "workdir: conversation text reached /tmp"
  wait "$pid"
  [[ "$(wd_runs "$d")" == 0 ]] || fail "workdir: a finished run left $(wd_runs "$d") work directory(ies)"
  # 2. kill -9 in the middle of a chunk: one directory is left (its lock is free once the
  # group is dead); the next run removes it, holds at most one while it runs, none after.
  qreset "$d"; rm -f "$d"/cur/*
  env -u MEMOIRE_CAPTURE HOME="$d/home" MEMOIRE_HOME="$d/mh" MEMOIRE_CAPTURE_CURSOR_DIR="$d/cur" \
    MEMOIRE_QUEUE_BIN="$WORK/bin/failq" FAILQ_REAL="$QUEUE_BIN" FAILQ_SLEEP=5 MEMOIRE_CODEX_BATCH_RECORDS=1000 \
    MEMOIRE_PROJECTS_REGISTRY="$WORK/registry.json" MEMOIRE_RESOLVE_PROJECT="$RESOLVER" TMPDIR="$d/tmp" \
    setsid bash "$ADAPTER" --tail "$f" >/dev/null 2>&1 &
  pid=$!
  wd_wait "$d" || { fail "workdir: the run to kill never started"; kill -9 -- "-$pid" 2>/dev/null; return; }
  kill -9 -- "-$pid" 2>/dev/null
  wait "$pid" 2>/dev/null
  sleep 0.3
  [[ "$(wd_runs "$d")" == 1 ]] || fail "workdir: after kill -9 want 1 leftover work directory, got $(wd_runs "$d")"
  # a live run's directory is not removed by another run, whatever its age: hold one
  # alive (slow queue), age its directory, start a second run on another file
  local g="$d/ss/rollout-w2.jsonl"; gen_records th-wd2 30 > "$g"
  ( QBO="$WORK/bin/failq" XE="FAILQ_SLEEP=3 MEMOIRE_CODEX_BATCH_RECORDS=1000" runad "$d" --tail "$f" >/dev/null 2>&1 ) &
  pid=$!
  local i seen=0
  for ((i = 0; i < 100; i++)); do
    (( $(wd_runs "$d") >= 1 )) && ls "$base"/run.*/events >/dev/null 2>&1 && { seen=1; break; }
    sleep 0.05
  done
  (( seen == 1 )) || fail "workdir: the second run never showed its work directory"
  [[ "$(wd_runs "$d")" == 1 ]] || fail "workdir: while the next run works, want exactly 1 work directory (the leftover cleared), got $(wd_runs "$d")"
  touch -d '3 days ago' "$base"/run.* 2>/dev/null
  ( QBO="$WORK/bin/failq" XE="FAILQ_SLEEP=0.2 MEMOIRE_CODEX_BATCH_RECORDS=1000" runad "$d" --tail "$g" >/dev/null 2>&1 )
  [[ "$(wd_runs "$d")" == 1 ]] || fail "workdir: a run that ended, and another that started, must leave the aged directory of the live run alone (got $(wd_runs "$d") directories)"
  wait "$pid"
  [[ "$(wd_runs "$d")" == 0 ]] || fail "workdir: after the runs, $(wd_runs "$d") work directory(ies) remain"
  (( FAILURES == 0 )) && check "workdir: private (700/600) under the Memoire home, no copy in TMPDIR or /tmp; kill -9 leaves one directory, the next run clears it (an aged directory of a LIVE run is kept: the lock decides)"
}

# ============================================================================
# jq-version - jq older than 1.7 stops the run with one line
# ============================================================================
mk_jq_stub() { # version dir
  mkdir -p "$2"
  cat > "$2/jq" <<STUB
#!/usr/bin/env bash
if [[ "\${1:-}" == "--version" ]]; then echo "$1"; exit 0; fi
exec $(command -v jq) "\$@"
STUB
  chmod +x "$2/jq"
}
t_jq_version() {
  local d v rc
  d="$(mkenv jqver)"; gen_records th-jq 12 > "$d/ss/rollout-j.jsonl"
  mk_jq_stub jq-1.6 "$WORK/jq16"
  PATH="$WORK/jq16:$PATH" runad "$d" --tail "$d/ss/rollout-j.jsonl" >"$d/o" 2>"$d/e"
  rc=$?
  [[ $rc == 1 ]] && grep -q "jq >= 1.7 is required, found 'jq-1.6'" "$d/e" && [[ "$(grep -c . "$d/e")" == 1 ]] \
    || fail "jq-version: jq 1.6 must end the run with rc 1 and one log line (rc=$rc): $(cat "$d/e")"
  [[ "$(qcount "$d")" == 0 && "$(wd_runs "$d")" == 0 && "$(cursor_count "$d")" == 0 ]] \
    || fail "jq-version: jq 1.6 must leave no queue item, work directory or cursor"
  for v in jq-1.7 jq-1.7.1-dirty jq-1.8.1 jq-2.0 jq-1.10; do
    mk_jq_stub "$v" "$WORK/jqok"
    qreset "$d"; rm -f "$d"/cur/*
    PATH="$WORK/jqok:$PATH" runad "$d" --tail "$d/ss/rollout-j.jsonl" >/dev/null 2>&1
    (( $(qcount "$d") > 0 )) || fail "jq-version: $v must be accepted"
  done
  # nothing to read: exit 0 and a summary whatever the jq version, nothing written
  d="$(mkenv jqver-empty)"
  PATH="$WORK/jq16:$PATH" runad "$d" --tail --scan --summary --sessions-dir "$d/ss" --archived-sessions-dir "$d/noarch" >"$d/o" 2>"$d/e"
  rc=$?
  [[ $rc == 0 && "$(cat "$d/o")" == $'0\t0\t0\t0\t0' && ! -e "$d/mh/tmp" ]] \
    || fail "jq-version: jq 1.6 with an empty sessions directory must exit 0 with a zero summary and write nothing (rc=$rc out=$(cat "$d/o") err=$(cat "$d/e"))"
  PATH="$WORK/jq16:$PATH" runad "$d" --tail --scan --summary --sessions-dir "$d/none" --archived-sessions-dir "$d/none2" >"$d/o" 2>"$d/e"
  rc=$?
  [[ $rc == 0 && "$(cat "$d/o")" == $'0\t0\t0\t0\t0' && ! -e "$d/mh/tmp" ]] \
    || fail "jq-version: jq 1.6 with a missing sessions directory must exit 0 with a zero summary and write nothing (rc=$rc out=$(cat "$d/o") err=$(cat "$d/e"))"
  # ... and with a file to read, the same scan stops on the version
  cp "$d/../jqver/ss/rollout-j.jsonl" "$d/ss/rollout-j.jsonl"
  PATH="$WORK/jq16:$PATH" runad "$d" --tail --scan --summary --sessions-dir "$d/ss" --archived-sessions-dir "$d/noarch" >"$d/o" 2>"$d/e"
  rc=$?
  [[ $rc == 1 ]] && grep -q "jq >= 1.7 is required, found 'jq-1.6'" "$d/e" \
    || fail "jq-version: jq 1.6 with a file to read must still stop the run (rc=$rc): $(cat "$d/e")"
  (( FAILURES == 0 )) && check "jq-version: jq-1.6 stops the run when there is a file to read (rc 1, one log line, nothing written) and exits 0 when there is none (empty or missing directory); 1.7, 1.7.1, 1.8.1, 1.10 and 2.0 run"
}

# ============================================================================
# redaction-key - one redaction key per batch
# ============================================================================
t_redaction_key() {
  local d f markers
  d="$(mkenv rkey)"; f="$WORK/rkey.jsonl"
  # Straight through the queue writer: the adapter writes its facts event
  # first, which would create the key before the racy batch.
  jq -nc 'range(0; 200) | {source_event_id: ("codex:key:\(.)"), event_type: "codex_record", payload: {output: "la clé AKIA''IOSFODNN7EXAMPLE — fin"}}' > "$f"
  [[ ! -e "$d/mh/redaction-key" ]] || fail "redaction-key: the fresh home already has a key"
  qs_batch "$d" "$f" >/dev/null 2>&1
  markers="$(qjson "$d" | grep -o '\[REDACTED:[^]]*\]' | sort -u | wc -l)"
  [[ "$markers" == 1 ]] || fail "redaction-key: the same secret in 200 items of a fresh home got $markers markers, want 1"
  [[ "$(qjson "$d" | grep -c 'AKIA''IOSFODNN7EXAMPLE')" == 0 ]] || fail "redaction-key: the secret survived"
  [[ "$(qjson "$d" | grep -c '\[REDACTED:')" -ge 200 ]] || fail "redaction-key: not every item carries the marker"
  [[ "$(stat -c %s "$d/mh/redaction-key" 2>/dev/null)" == 32 ]] || fail "redaction-key: want one 32-byte key on disk"
  (( FAILURES == 0 )) && check "redaction-key: the same secret in 200 items of a fresh home yields one marker"
}

# ============================================================================
# queue-signal - a batch killed or terminated leaves no usable temp file behind
# ============================================================================
qs_batch() { # dir file [args...]: run the queue batch writer in DIR's scratch home
  local d="$1" file="$2"; shift 2
  env HOME="$d/home" MEMOIRE_HOME="$d/mh" "$QUEUE_BIN" -project p -session-id s -source-adapter codex-cli -cwd /w "$@" -batch-file "$file"
}
qs_batch_bg() { # as qs_batch, for "&": exec, so $! is the writer itself on every bash (5.2 keeps the function's subshell as the job and a signal would stop there)
  local d="$1" file="$2"; shift 2
  exec env HOME="$d/home" MEMOIRE_HOME="$d/mh" "$QUEUE_BIN" -project p -session-id s -source-adapter codex-cli -cwd /w "$@" -batch-file "$file"
}
qs_temps() { ls -a "$1/mh/queue/pending" 2>/dev/null | grep -c '^\.[0-9]'; }
qs_wait_temp() { # dir : wait (10 s) until a temp file exists
  local i
  for ((i = 0; i < 2000; i++)); do
    (( $(qs_temps "$1") > 0 )) && return 0
    sleep 0.005
  done
  return 1
}
t_queue_signal() {
  local d pid rc big="$WORK/qs-big.jsonl" small="$WORK/qs-small.jsonl" n
  d="$(mkenv qsig)"
  jq -nc 'range(0; 3000) | {source_event_id: ("codex:sig:\(.)"), event_type: "codex_record", payload: {k: ("é—😀" * 4000)}}' > "$big"
  jq -nc 'range(0; 3) | {source_event_id: ("codex:small:\(.)"), event_type: "note", text: "petit — é"}' > "$small"
  mkdir -p "$d/mh/queue/pending"
  # SIGTERM while items are being prepared: nothing is left in the pending directory
  qs_batch_bg "$d" "$big" >/dev/null 2>&1 &
  pid=$!
  qs_wait_temp "$d" || fail "queue-signal: the batch never staged a temp file"
  kill -TERM "$pid" 2>/dev/null
  wait "$pid" 2>/dev/null; rc=$?
  [[ $rc == 143 ]] || fail "queue-signal: SIGTERM must end the batch with 143 (rc=$rc)"
  [[ "$(qs_temps "$d")" == 0 ]] || fail "queue-signal: SIGTERM left $(qs_temps "$d") temp file(s) in the pending directory"
  # SIGKILL: no handler can help; the temp files stay, and the next batches deal with them
  rm -rf "$d/mh/queue"; mkdir -p "$d/mh/queue/pending"
  qs_batch_bg "$d" "$big" >/dev/null 2>&1 &
  pid=$!
  qs_wait_temp "$d" || fail "queue-signal: the batch never staged a temp file (kill)"
  kill -KILL "$pid" 2>/dev/null
  wait "$pid" 2>/dev/null
  n="$(qs_temps "$d")"
  (( n > 0 )) || fail "queue-signal: SIGKILL left no temp file, the scenario is void"
  local tmps keep
  tmps="$(ls -a "$d/mh/queue/pending" | grep '^\.[0-9]')"
  keep="$(head -1 <<<"$tmps")"
  # a fresh batch leaves recent temp files alone (a live writer may own them)
  qs_batch "$d" "$small" >/dev/null 2>&1
  [[ "$(qs_temps "$d")" == "$n" ]] || fail "queue-signal: a batch removed recent temp files ($n -> $(qs_temps "$d"))"
  # old ones are removed by the next batch, and only them
  local t
  for t in $tmps; do [[ "$t" == "$keep" ]] || touch -d '1 hour ago' "$d/mh/queue/pending/$t"; done
  touch -d '1 hour ago' "$d/mh/queue/pending/.unrelated-note"
  cp "$d/mh/queue/pending/$(ls "$d/mh/queue/pending" | grep -v '^\.' | head -1)" "$d/old-item.json"
  local item; item="$(ls "$d/mh/queue/pending" | grep -v '^\.' | head -1)"
  touch -d '1 hour ago' "$d/mh/queue/pending/$item"
  local items_before; items_before="$(ls "$d/mh/queue/pending" | grep -vc '^\.')"
  qs_batch "$d" "$small" >/dev/null 2>&1
  [[ "$(qs_temps "$d")" == 1 && -e "$d/mh/queue/pending/$keep" ]] \
    || fail "queue-signal: after the sweep want only the recent temp file, got $(qs_temps "$d")"
  [[ -e "$d/mh/queue/pending/.unrelated-note" ]] || fail "queue-signal: the sweep removed a file that is not a temp file"
  [[ -e "$d/mh/queue/pending/$item" && "$(ls "$d/mh/queue/pending" | grep -vc '^\.')" -ge "$items_before" ]] \
    || fail "queue-signal: the sweep touched a queue item"
  (( FAILURES == 0 )) && check "queue-signal: SIGTERM mid-batch leaves 0 temp files; after SIGKILL ($n left) the next batches sweep the old ones ($((n - 1)) removed), keep recent ones, never an item"
}

# ============================================================================
# stale-temp - the sweep never takes the temp file of a live writer
# ============================================================================
t_stale_temp() {
  local d pid rc big="$WORK/st-big.jsonl" small="$WORK/st-small.jsonl" before after sum items i
  d="$(mkenv stale)"
  jq -nc 'range(0; 3000) | {source_event_id: ("codex:frozen:\(.)"), event_type: "codex_record", payload: {k: ("é—😀" * 4000)}}' > "$big"
  jq -nc 'range(0; 3) | {source_event_id: ("codex:small:\(.)"), event_type: "note", text: "petit — é"}' > "$small"
  mkdir -p "$d/mh/queue/pending"
  # a batch writer frozen (SIGSTOP) for longer than the age bound: a second batch
  # with the bound cut to 1 s keeps its temp files, and it completes after SIGCONT.
  # The freeze must land while the writer stages its first chunk (256 items of
  # 36 KB, seconds of work): at least 8 temp files exist and no item is renamed.
  # A freeze that lands elsewhere (a loaded machine) voids the attempt: retry.
  local attempt=0 frozen=0 nt ni
  while (( attempt < 3 && ! frozen )); do
    attempt=$((attempt + 1))
    rm -rf "$d/mh/queue"; mkdir -p "$d/mh/queue/pending"
    qs_batch_bg "$d" "$big" >"$d/frozen.out" 2>"$d/frozen.err" &
    pid=$!
    for ((i = 0; i < 4000; i++)); do
      (( $(qs_temps "$d") >= 8 )) && break
      sleep 0.005
    done
    kill -STOP "$pid"
    nt="$(qs_temps "$d")"
    ni="$(ls "$d/mh/queue/pending" | grep -vc '^\.')"
    if (( nt >= 1 && ni == 0 )); then
      frozen=1
    else
      kill -CONT "$pid"; kill -KILL "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
    fi
  done
  (( frozen )) || fail "stale-temp: in 3 attempts the freeze never landed inside the first chunk (temps=$nt items=$ni)"
  before="$(ls -a "$d/mh/queue/pending" | grep '^\.[0-9]' | sort | md5sum)"
  sleep 2.5
  MEMOIRE_QUEUE_STALE_TEMP_S=1 qs_batch "$d" "$small" >/dev/null 2>&1
  after="$(ls -a "$d/mh/queue/pending" | grep '^\.[0-9]' | sort | md5sum)"
  [[ "$before" == "$after" ]] || fail "stale-temp: a batch swept the temp files of a frozen live writer"
  kill -CONT "$pid"
  wait "$pid"; rc=$?
  items="$(ls "$d/mh/queue/pending" | grep -vc '^\.')"
  [[ $rc == 0 && "$(grep -c . "$d/frozen.out")" == 3000 && "$items" == 3003 && "$(qs_temps "$d")" == 0 ]] \
    || fail "stale-temp: the frozen batch must complete after SIGCONT (rc=$rc printed=$(grep -c . "$d/frozen.out") items=$items temps=$(qs_temps "$d") err=$(head -c 200 "$d/frozen.err"))"
  # the single-item form carries no pid: only 24 hours old goes, whatever the bound
  sum="$(printf '%064d' 7)"
  rm -f "$d"/mh/queue/pending/.[0-9]*
  : > "$d/mh/queue/pending/.1790000000000-codex-cli-$sum.json.tmp"
  : > "$d/mh/queue/pending/.1790000000001-codex-cli-$sum.json.tmp"
  touch -d '11 minutes ago' "$d/mh/queue/pending/.1790000000000-codex-cli-$sum.json.tmp"
  touch -d '25 hours ago' "$d/mh/queue/pending/.1790000000001-codex-cli-$sum.json.tmp"
  MEMOIRE_QUEUE_STALE_TEMP_S=1 qs_batch "$d" "$small" >/dev/null 2>&1
  [[ -e "$d/mh/queue/pending/.1790000000000-codex-cli-$sum.json.tmp" && ! -e "$d/mh/queue/pending/.1790000000001-codex-cli-$sum.json.tmp" ]] \
    || fail "stale-temp: a single-item temp file aged 11 minutes must stay and one aged 25 hours must go"
  (( FAILURES == 0 )) && check "stale-temp: a writer frozen with SIGSTOP inside its first chunk ($nt temp files, attempt $attempt of 3) past the age bound keeps its temp files and completes after SIGCONT (3000 lines, 0 temps); a single-item temp file aged 11 minutes stays, 25 hours goes"
}

# ============================================================================
# T-equiv - a full pass and any sequence of tail passes agree
# ============================================================================
# The equality holds for every capture-all event and for every CLOSED turn. It
# does NOT hold for partial turn events (closed_by "adapter"): a partial exists
# only when a run sees the file idle past MEMOIRE_CODEX_STUCK_S, so which
# partials exist, and the supersedes link of the terminal turn that later
# closes them, depend on the time of the runs (an accepted exception, stated in
# the adapter header; turn-stuck shows it). The fixture below has no stuck turn.
t_equiv() {
  local d src ref cuts n i out
  src="$CENSUS_FIXTURES/rollout-2026-05-01T09-00-00-census-alpha.jsonl"
  ref="$(fullpass_set "$src")"
  n="$(wc -l < "$src")"
  for cuts in "7 19 33" "1 2 3 4 5 6 8" "12 13 40" "46"; do
    d="$(mkenv "equiv-${cuts// /-}")"
    : > "$d/ss/rollout-eq.jsonl"
    local prev=0
    for i in $cuts $n; do
      sed -n "$((prev + 1)),${i}p" "$src" >> "$d/ss/rollout-eq.jsonl"
      XE="MEMOIRE_CODEX_BATCH_RECORDS=6" runad "$d" --tail "$d/ss/rollout-eq.jsonl" >/dev/null 2>&1
      prev="$i"
    done
    out="$(qset "$d")"
    if [[ "$out" != "$ref" ]]; then
      fail "T-equiv: tail runs cut at [$cuts] differ from a full pass:
$(diff <(echo "$out" | jq -r .id) <(echo "$ref" | jq -r .id) | head -6)
$(diff <(echo "$out") <(echo "$ref") | cut -c1-160 | head -4)"
      return
    fi
  done
  check "T-equiv: full pass and 4 different tail sequences (small chunks) give identical ids and content for capture-all events and closed turns ($(echo "$ref" | grep -c .) events; partial turns excluded by design)"
}

# ============================================================================
# T-census - fixtures reproduce every record type; independent model
# ============================================================================
cat > "$WORK/expected.jq" <<'JQ'
def enc: if type == "object" then with_entries(if .key == "encrypted_content" and (.value | type) == "string" then .value = {encrypted: true, bytes: (.value | utf8bytelength)} else .value |= enc end) elif type == "array" then map(enc) else . end;
def ptype: (.payload | if type == "object" then (.type // "") else "" end);
def role: (.payload | if type == "object" then (.role // "") else "" end);
def etype:
  .type as $t | ptype as $p | role as $r |
  if $t == "response_item" and $p == "message" then ({"assistant": "codex_assistant_message", "user": "codex_prompt", "developer": "codex_developer_message"}[$r] // "codex_record")
  elif $t == "response_item" then ({"reasoning": "codex_reasoning", "custom_tool_call": "codex_tool_call", "custom_tool_call_output": "codex_tool_result", "function_call": "codex_tool_call", "function_call_output": "codex_tool_result", "web_search_call": "codex_web_search", "tool_search_call": "codex_tool_search", "tool_search_output": "codex_tool_result", "compaction": "codex_compaction", "agent_message": "codex_agent_message"}[$p] // "codex_record")
  elif $t == "event_msg" then ({"item_completed": "codex_item", "task_started": "codex_turn_started", "task_complete": "codex_turn_complete", "turn_aborted": "codex_turn_aborted", "thread_settings_applied": "codex_settings", "thread_goal_updated": "codex_goal"}[$p] // "codex_record")
  else ({"turn_context": "codex_turn_context", "token_usage_record": "codex_token_usage", "compacted": "codex_compaction", "inter_agent_communication_metadata": "codex_agent_trigger"}[$t] // "codex_record") end;
def notcaptured($line): (.type == "session_meta" and $line == 0) or (.type == "event_msg" and ptype == "token_count") or .type == "world_state" or .type == "realtime_item";
def msgtext: [(.payload.content // [])[] | select(type == "object" and (.text | type) == "string" and .text != "") | .text] | join("\n\n");
($src | split("\n") | if .[-1] == "" then .[:-1] else . end) as $lines
| ($lines[0] | fromjson | .payload) as $m
| ($m.history_base.thread_id // $m.id) as $thread
| ($m.subagent_history_start_ordinal // 0) as $ssto
| ($m.timestamp) as $mts
| [{id: ("codex:" + $thread + ":facts"), t: "codex_thread_facts", at: $mts, x: "", p: null}]
  + [range(0; $lines | length) as $i
     | ($lines[$i] | try fromjson catch null) as $r
     | if $r == null or ($r | type) != "object" then
         {id: ("codex:" + $thread + ":ln" + ($i | tostring)), t: "codex_record", at: $mts, x: "",
          p: {unparsed_line: true, bytes: ($lines[$i] | utf8bytelength), raw: $lines[$i]}}
       elif ($r | notcaptured($i)) then empty
       else
         ($r.ordinal // null) as $o
         | ($o // $i) as $idx
         | (if $o != null then ($o | tostring) else "ln" + ($i | tostring) end) as $k
         | ($idx >= $ssto) as $own
         | ($r | ptype) as $p
         | {id: ("codex:" + $thread + ":" + $k), t: ($r | etype), at: $r.timestamp, x: "",
            p: ((($r.payload | if type == "object" then . else {} end) | enc)
                + (if $own then {} else {inherited: true} end)
                + (if $own and $r.type == "event_msg" and ($p == "user_message" or $p == "agent_message" or $p == "agent_reasoning") then {legacy_ui: true} else {} end)
                + (if ($r.payload | type) != "object" and $r.payload != null then {payload_value: $r.payload} else {} end)
                + (if ($r | has("metadata")) then {record_metadata: ($r.metadata | enc)} else {} end))}
       end]
  + ( reduce range(0; $lines | length) as $i ({st: [], out: []};
        ($lines[$i] | try fromjson catch null) as $r
        | if $r == null or ($r | type) != "object" then .
          else
            (($r.ordinal // $i) >= $ssto) as $own
            | ($r.ordinal // null) as $o
            | (if $o != null then ($o | tostring) else "ln" + ($i | tostring) end) as $k
            | ($r | ptype) as $p
            | if $own and $r.type == "event_msg" and $p == "task_started" then .st += [{id: ($r.payload.turn_id // ""), text: [], n: 0}]
              elif $own and $r.type == "response_item" and $p == "message" and ($r | role) == "assistant" then
                (if (.st | length) > 0 and (($r | msgtext) != "") then .st[-1].text += [($r | msgtext)] else . end)
              elif $own and $r.type == "event_msg" and ($p == "task_complete" or $p == "turn_aborted") then
                ([.st | to_entries[] | select(.value.id == ($r.payload.turn_id // ""))] | last) as $hit
                | if $hit == null then .
                  else (.st[$hit.key]) as $e
                       | .st = (.st[:$hit.key] + .st[$hit.key + 1:])
                       | if ($e.text | length) > 0
                         then .out += [{id: ("codex:" + $thread + ":t" + $k), t: "codex_assistant_turn", at: $r.timestamp, x: ($e.text | join("\n\n")), p: null}]
                         else . end
                  end
              else . end
          end) | .out )
| sort_by(.id)
JQ
census_file() { # fixture-file label
  local src="$1" d out exp act
  d="$(mkenv "census-$2")"
  cp "$src" "$d/ss/rollout-c.jsonl"
  runad "$d" "$d/ss/rollout-c.jsonl" >/dev/null 2>&1
  jq -n -c -S --rawfile src "$src" -f "$WORK/expected.jq" > "$d/expected.json" 2>"$d/expected.err" || { fail "T-census: the expected-model program failed on $2: $(head -c 300 "$d/expected.err")"; return; }
  exp="$(jq -c '.[]' "$d/expected.json" | sort)"
  # actual, in the same shape: provenance keys removed from record payloads
  act="$(qjson "$d" | jq -cS '
      {id: .source_event_id, t: .event_type, at: .occurred_at, x: (.text // ""),
       p: (if .event_type == "codex_assistant_turn" or .event_type == "codex_thread_facts" then null
           else (.payload | del(.thread_id, .ordinal, .line, .rollout_file, .record_type, .record_payload_type, .memoire_project_root, .memoire_project_source)) end)}' | sort)"
  # unparsed lines: the adapter also adds thread_id/line/rollout_file, removed above
  if [[ "$exp" == "$act" ]]; then
    check "T-census: $2: $(echo "$exp" | grep -c .) events equal the model (ids, types, times, payloads, turn texts)"
  else
    fail "T-census: $2: events differ from the model:
$(diff <(echo "$exp" | cut -c1-260) <(echo "$act" | cut -c1-260) | head -12)"
  fi
  # provenance: ordinal / record_type agree with the source
  local bad
  bad="$(qjson "$d" | jq -r 'select(.payload.record_type != null) | select(.payload.ordinal == null or .payload.thread_id == null) | .source_event_id')"
  [[ -z "$bad" ]] || fail "T-census: $2: provenance missing on: $bad"
  # explicit expected-zeroes
  local z
  z="$(qjson "$d" | jq -r 'select(.payload.record_payload_type == "token_count" or .payload.record_type == "world_state" or .payload.record_type == "realtime_item") | .source_event_id')"
  [[ -z "$z" ]] || fail "T-census: $2: not-captured classes appeared: $z"
  # exactly one facts event per thread
  [[ "$(qjson "$d" | jq -r 'select(.event_type == "codex_thread_facts") | .source_event_id' | grep -c .)" == 1 ]] \
    || fail "T-census: $2: want exactly one facts event"
}
t_census() {
  local f n=0
  for f in "$CENSUS_FIXTURES"/*.jsonl; do
    n=$((n + 1)); census_file "$f" "fixture$n"
  done
  [[ $n -ge 4 ]] || fail "T-census: fewer than 4 fixtures found in $CENSUS_FIXTURES"
  # Record types present in the fixtures cover the store format table.
  local types missing="" t
  types="$(cat "$CENSUS_FIXTURES"/*.jsonl | jq -r 'select(type == "object") | .type + "/" + (if (.payload | type) == "object" then (.payload.type // "") else "" end)' 2>/dev/null | sort -u)"
  for t in session_meta/ turn_context/ response_item/message response_item/reasoning response_item/custom_tool_call response_item/custom_tool_call_output \
           response_item/function_call response_item/function_call_output response_item/web_search_call response_item/tool_search_call \
           response_item/tool_search_output response_item/compaction response_item/agent_message event_msg/item_completed event_msg/task_started \
           event_msg/task_complete event_msg/turn_aborted event_msg/thread_settings_applied event_msg/thread_goal_updated event_msg/token_count \
           event_msg/user_message event_msg/agent_message event_msg/agent_reasoning token_usage_record/ compacted/ world_state/ \
           inter_agent_communication_metadata/ realtime_item/; do
    if [[ "$t" == */ ]]; then grep -q "^$t" <<<"$types" || missing="$missing $t"
    else grep -qx "$t" <<<"$types" || missing="$missing $t"; fi
  done
  [[ -z "$missing" ]] && check "T-census: the fixtures hold every record type of the format table" \
    || fail "T-census: fixture record types missing:$missing"
  if [[ -n "${CODEX_CAPTURE_CENSUS_DIR:-}" ]]; then
    n=0
    for f in "$CODEX_CAPTURE_CENSUS_DIR"/*.jsonl; do [[ -e "$f" ]] || continue; n=$((n + 1)); census_file "$f" "manual$n"; done
    echo "note: manual census ran over $n file(s) from CODEX_CAPTURE_CENSUS_DIR"
  fi
}

# ---- run -----------------------------------------------------------------------------
for t in identity cursor_identity capture time attrib inherit admit text utf8 turn_tail turn_stuck partial_id write_failure segment bound kill term order workdir jq_version redaction_key queue_signal stale_temp token_count metadata prov_collision equiv census; do
  want "$t" || want "${t%%_*}" || continue
  "t_$t"
done

if (( FAILURES > 0 )); then
  echo "codex-rollout-capture: $FAILURES failure(s)" >&2
  exit 1
fi
echo "codex-rollout-capture: OK"
