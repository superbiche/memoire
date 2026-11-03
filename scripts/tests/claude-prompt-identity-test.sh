#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# claude-prompt-identity-test.sh — the UserPromptSubmit source_event_id must tell
# two identical prompts of one session apart ("yes", "go"), stay stable for a
# re-fire at the same position, leave the first occurrence on the historical
# formula, and fail open when the transcript cannot be read. The queue writer is
# a recorder stub: only the -source-event-id it receives is asserted.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HOOK="$ROOT/scripts/memoire-capture-claude.sh"
command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/home/hooks"
REC="$WORK/ids.log"; : > "$REC"
cat > "$WORK/queue" <<STUB
#!/usr/bin/env bash
while [[ \$# -gt 0 ]]; do
  if [[ "\$1" == "-source-event-id" ]]; then printf '%s\n' "\$2" >> "$REC"; fi
  shift
done
STUB
chmod +x "$WORK/queue"

fail() { echo "FAIL: $1" >&2; exit 1; }

# fire <session> <prompt> [transcript_path] -> prints the id the hook produced
fire() {
  local sid="$1" prompt="$2" tp="${3-}" before after
  before="$(wc -l < "$REC" 2>/dev/null || echo 0)"
  jq -nc --arg s "$sid" --arg p "$prompt" --arg t "$tp" \
    '{session_id:$s,cwd:"/work/proj",hook_event_name:"UserPromptSubmit",prompt:$p} + (if $t == "" then {} else {transcript_path:$t} end)' |
    MEMOIRE_HOME="$WORK/home" MEMOIRE_QUEUE_BIN="$WORK/queue" MEMOIRE_PROJECT=proj \
      MEMOIRE_PROJECTS_REGISTRY=/nonexistent MEMOIRE_CLAUDE_STRICT=1 "$HOOK" || fail "hook exited non-zero"
  after="$(wc -l < "$REC")"
  (( after == before + 1 )) || fail "hook wrote $((after - before)) queue entries, want 1"
  tail -n 1 "$REC"
}

# The hook fires before Claude Code appends the prompt to the transcript, so a
# scenario appends the user line AFTER each fire.
append_prompt() { # <transcript> <prompt>
  jq -nc --arg p "$2" '{type:"user",message:{role:"user",content:$p},sessionId:"s"}' >> "$1"
}
append_noise() { # <transcript> <text>: lines that must never count as a prompt
  jq -nc --arg p "$2" '{type:"assistant",message:{role:"assistant",content:[{type:"text",text:$p}]}}' >> "$1"
  jq -nc --arg p "$2" '{type:"user",message:{role:"user",content:[{type:"tool_result",content:$p}]}}' >> "$1"
  jq -nc --arg p "$2" '{type:"assistant",message:{role:"assistant",content:[{type:"text",text:({type:"user",message:{role:"user",content:$p}}|tojson)}]}}' >> "$1"
  # a user-role line whose content is the prompt but is followed by more keys is not a prompt line
  printf '{"type":"user","message":{"role":"user","content":%s,"extra":1}}\n' "$(jq -nc --arg p "$2" '$p')" >> "$1"
  printf '{"type":"user","message":{"role":"user","content":%s}}\n' "$(jq -nc --arg p "$2 please" '$p')" >> "$1"
}

T="$WORK/t1.jsonl"; : > "$T"

# 1. history stays stable: the first occurrence keeps the historical id.
LEGACY=claude-userpromptsubmit-3e3274ccb55ed2df14f3c4d3090a5fae00361fa348ed2bcba07fc55b8b8d4c87
id1="$(fire sess-legacy yes "$T")"
[[ "$id1" == "$LEGACY" ]] || fail "first occurrence id changed: $id1 != $LEGACY"
# recorded ids of the ordinal formula (legacy object + "ordinal":k), computed with jq|sha256sum
ORD1=claude-userpromptsubmit-e99cddc8c3b7438369c26ee13441517445f7970865e49dd2277f43eeb764299a
ORD2=claude-userpromptsubmit-016bb861fb6693f898d2bc5b26ed5ef8402391f14dcbbbbddab32d482dc76ae8
GO_LEGACY=claude-userpromptsubmit-2c3ffa87fd1ad5d3722c1099b1210c4a507461a0cf04eaf679ee94f3bb9b9b28
id1_notp="$(fire sess-legacy yes)"
[[ "$id1_notp" == "$LEGACY" ]] || fail "no transcript_path must keep the historical id: $id1_notp"

# 2. same prompt, same position, same id (a re-fire before the line lands).
id1b="$(fire sess-legacy yes "$T")"
[[ "$id1b" == "$id1" ]] || fail "re-fire at the same position changed the id"

# 3. two identical prompts at different positions get different ids.
append_prompt "$T" yes
append_noise "$T" yes
id2="$(fire sess-legacy yes "$T")"
[[ "$id2" != "$id1" ]] || fail "second identical prompt reused the first id (would be dropped by the hub)"
[[ "$id2" == "$ORD1" ]] || fail "ordinal 1 id is not the recorded one (noise lines counted or ordinal off): $id2"
id2b="$(fire sess-legacy yes "$T")"
[[ "$id2b" == "$id2" ]] || fail "re-fire of the second prompt changed the id"
append_prompt "$T" yes
id3="$(fire sess-legacy yes "$T")"
[[ "$id3" != "$id1" && "$id3" != "$id2" ]] || fail "third identical prompt collided"
[[ "$id3" == "$ORD2" ]] || fail "ordinal 2 id is not the recorded one: $id3"
[[ "$id2" == claude-userpromptsubmit-* && "$id3" == claude-userpromptsubmit-* ]] || fail "id prefix changed"

# 4. a different prompt in the same session keeps its own first-occurrence id.
idg="$(fire sess-legacy go "$T")"
[[ "$idg" != "$id1" ]] || fail "different prompt collided"
[[ "$idg" == "$GO_LEGACY" && "$idg" == "$(fire sess-legacy go)" ]] || fail "first 'go' after other prompts lost its historical id: $idg"
append_prompt "$T" go
[[ "$(fire sess-legacy go "$T")" != "$idg" ]] || fail "second 'go' collided with the first"

# 5. other sessions are unaffected by this transcript's history, and a prompt
#    with quotes, newlines and unicode is counted through its JSON escaping.
T2="$WORK/t2.jsonl"; : > "$T2"
[[ "$(fire sess-other yes "$T2")" == "$(fire sess-other yes)" ]] || fail "fresh session first id differs from historical"
weird=$'say "hi"\nà la \\ fin'
w1="$(fire sess-w "$weird" "$T2")"; append_prompt "$T2" "$weird"
w2="$(fire sess-w "$weird" "$T2")"
[[ "$w1" != "$w2" ]] || fail "escaped prompt second occurrence collided"

# 6. fail open. A missing transcript (first prompt of every session) is silent;
#    an existing but unreadable file, a failed scan and a scan that hangs log a line.
LOG="$WORK/home/hooks/claude.log"; : > "$LOG"
id_miss="$(fire sess-legacy yes "$WORK/does-not-exist.jsonl")"
[[ "$id_miss" == "$LEGACY" ]] || fail "missing transcript: want historical id, got $id_miss"
[[ ! -s "$LOG" ]] || fail "missing transcript must not log: $(cat "$LOG")"
if [[ "$(id -u)" != 0 ]]; then
  U="$WORK/unreadable.jsonl"; : > "$U"; chmod 000 "$U"
  id_bad="$(fire sess-legacy yes "$U")"
  [[ "$id_bad" == "$LEGACY" ]] || fail "unreadable transcript: want historical id, got $id_bad"
  grep -q "transcript unreadable" "$LOG" || fail "unreadable transcript was not logged"
fi
# a FIFO with no writer blocks grep on open: the scan must be cut off (2 s) well inside the hook's 5 s
mkfifo "$WORK/hang.fifo"; : > "$LOG"
s=$(date +%s%N)
export WORK HOOK REC
id_hang="$(timeout 4.5 bash -c "$(declare -f fire fail); fire sess-legacy yes '$WORK/hang.fifo'")" || fail "hook hung or failed on a blocking transcript (scan not bounded)"
e=$(date +%s%N)
[[ "$id_hang" == "$LEGACY" ]] || fail "hung scan: want historical id, got $id_hang"
grep -q "scan failed" "$LOG" || fail "hung scan was not logged"
(( (e - s) / 1000000 < 4000 )) || fail "scan bound too loose: $(( (e - s) / 1000000 )) ms"
# the pattern (prompt text) never lands in a file
[[ -z "$(find "$WORK/home" "${TMPDIR:-/tmp}" -maxdepth 1 -name '*.pat' -newer "$REC" 2>/dev/null)" ]] || fail "a pattern file was left behind"

# 7. cost: a large transcript must not be read in the hook's critical path for long.
BIG="$WORK/big.jsonl"
yes '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"filler filler filler filler filler filler filler filler filler filler filler filler"}]}}' | head -c 30000000 > "$BIG"
big0="$(fire sess-big yes "$BIG")"
append_prompt "$BIG" yes
s=$(date +%s%N); big1="$(fire sess-big yes "$BIG")"; e=$(date +%s%N)
ms=$(( (e - s) / 1000000 ))
echo "large transcript ($(stat -c %s "$BIG") bytes) hook wall time: ${ms} ms"
[[ "$big0" != "$big1" ]] || fail "30 MB transcript: position 0 and position 1 got the same id (scan missed or timed out)"
# the scan itself is cut off at 2 s (falling back to the legacy id, caught above); this limit only
# catches a scan that is slow but under the cut, and is sized for a loaded CI runner (measured ~80 ms)
(( ms < 2500 )) || fail "hook too slow on a large transcript: ${ms} ms"

echo "claude-prompt-identity-test: OK"
