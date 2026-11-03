# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
#
# memoire-subagent-extract-claude.jq — Claude Code transcript JSONL -> the
# normalized entry stream of memoire-subagent-select.jq.
# Run with -n over a stream of JSON STRINGS, one per store line (sa_json_lines in
# memoire-subagent-lib.sh; never -R, which corrupts multibyte characters at read
# buffer boundaries). Each string is parsed here: a bad line is skipped and
# counted, never fatal). --arg mode anchor : prints the newest prompt entry
# (or null). --arg mode window : prints {entries, window_cut, bad_lines}; needs
# --argjson anchor, --argjson cut.

#
# Never entries (design B7): sidechain lines, isMeta lines, compaction
# summaries, interruption markers, tool_result envelopes (they pair into tool
# entries), <task-notification>/<agent-message> envelopes, system reminders,
# the SessionStart Memoire block, attachments of any type (hook_additional_context
# among them), synthetic assistant lines.

def msg: (.message // {}) | if type == "object" then . else {} end;
def txt: if type == "string" then .
  elif type == "array" then ([.[] | select(type == "object" and .type == "text") | .text | select(type == "string")] | join("\n"))
  else "" end;
def synthetic: test("^\\s*(<system-reminder>|<task-notification>|<agent-message>|<bash-input>|<bash-stdout>|<bash-stderr>|<local-command|<command-name>|<command-message>|\\[Request interrupted|Another Claude session sent a message|# \\[memoire\\]|Caveat: The messages below|This session is being continued from a previous conversation)");
def ts: (.timestamp // "") as $t
  | if ($t | type) == "string" and ($t | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T"))
    then (try ($t | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601 | localtime | strftime("%H:%M:%S")) catch "--:--:--")
    else "--:--:--" end;
# Entries leave here bounded (the select rule cuts the same prefixes, so the
# cut is idempotent): prompt 3000, text 2400, input 400 characters, result
# head 600 + tail 600. A prompt of megabytes cannot break an argv limit.
def pre($n): if length > (2 * $n) then .[0:$n] + " " + .[(length - $n):] else . end;
def kv: if type == "object" then (to_entries | map(.key + "=" + (.value | if type == "string" then . else tojson end)) | join(" "))
  elif type == "string" then .
  elif . == null then ""
  else tojson end;

def prompt_of:
  select(.type == "user" and .isSidechain != true and .isMeta != true and .isCompactSummary != true)
  | msg.content as $c
  | if ($c | type) == "string" then $c
    elif ($c | type) == "array" then
      (if any($c[]; type == "object" and .type == "tool_result") then ""
       else ([$c[] | select(type == "object" and .type == "text") | .text | select(type == "string" and (synthetic | not))] | join("\n")) end)
    else "" end
  | select(. != "" and (synthetic | not));

def entries:
  . as $lines
  | ($lines | map(select(.isSidechain != true))) as $L
  | (reduce ($L[] | select(.type == "user") | msg.content | select(type == "array") | .[]
             | select(type == "object" and .type == "tool_result")) as $r ({};
       .[$r.tool_use_id // ""] = {err: ($r.is_error == true), text: ($r.content | txt)})) as $res
  | [ $L[]
      | . as $l
      | if .type == "user" then
          ($l | prompt_of) as $p | {kind: "prompt", ts: ($l | ts), text: $p[0:3000], id: ($l.uuid // "")}
        elif .type == "assistant" and (msg.model // "") != "<synthetic>" then
          ((msg.content // []) | if type == "string" then [{type: "text", text: .}] else . end) as $c
          | ( ([$c[] | select(type == "object" and .type == "text") | .text | select(type == "string")] | join("\n\n")) as $t
              | if $t != "" then {kind: "text", ts: ($l | ts), text: $t[0:2400]} else empty end ),
            ( $c[] | select(type == "object" and .type == "tool_use") as $u
              | ($res[$u.id // ""]) as $r
              | {kind: "tool", ts: ($l | ts), name: ($u.name // ""), input: ($u.input | kv | .[0:400]),
                 status: (if $r == null then "running" elif $r.err then "error" else "ok" end),
                 result: (($r.text // "") | pre(600))} )
        else empty end ];

def parsed: [inputs | select(. != "") | (try fromjson catch "BAD") | if type == "object" then . else "BAD" end];

if $mode == "anchor" then
  (parsed | map(select(. != "BAD")) | entries | map(select(.kind == "prompt")) | last)
else
  parsed as $P
  | ($P | map(select(. == "BAD")) | length) as $bad
  | ($P | map(select(. != "BAD")) | entries) as $E
  | ($E | map(if .kind == "prompt" and $anchor != null and .id == $anchor.id then . + {anchor: true} else . end)) as $M
  | if $anchor == null or any($M[]; .anchor == true) then {entries: $M, window_cut: $cut, bad_lines: $bad}
    else {entries: ([$anchor + {anchor: true}] + $M), window_cut: true, bad_lines: $bad} end
end
