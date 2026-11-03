# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
#
# memoire-subagent-extract-codex.jq — Codex rollout JSONL -> the normalized
# entry stream of memoire-subagent-select.jq. Record classification follows
# the accepted reader (memoire-capture-codex-rollout.sh, evtype): response_item
# message role user -> prompt, assistant -> text, developer kept apart;
# function_call and custom_tool_call -> tool call; function_call_output and
# custom_tool_call_output -> result, paired by call_id.
# Run with -n over a stream of JSON STRINGS, one per store line (sa_json_lines in
# memoire-subagent-lib.sh; never -R, which corrupts multibyte characters at read
# buffer boundaries). Each string is parsed here: a bad line is skipped and
# counted, never fatal). --arg mode anchor : prints the FIRST prompt entry
# of the lines (the caller passes the lines from the current turn's task_started
# on) or null. --arg mode window : prints {entries, window_cut, bad_lines}; needs
# --argjson anchor, --argjson cut, --argjson hist (subagent_history_start_ordinal
# of line 0, 0 when none).
#
# Never entries: inherited records (ordinal < hist), event_msg records,
# session_meta, reasoning, developer messages, agent_message envelopes, and
# user messages that are injected instruction blocks (AGENTS.md, environment
# context, inter-agent envelopes) or not client-authored.

def txt: if type == "string" then .
  elif type == "array" then ([.[] | select(type == "object") | (.text // empty) | select(type == "string")] | join("\n\n"))
  elif type == "object" then ((.content // .text // tojson) | if type == "string" then . elif type == "array" then ([.[] | select(type == "object") | (.text // empty)] | join("\n")) else tojson end)
  else "" end;
def injected: test("^\\s*(<environment_context>|<user_instructions>|<INSTRUCTIONS>|<permissions instructions>|<collaboration_mode>|<subagent_notification>|<turn_aborted>|<skill>|<user_action>|<user_shell_command>|<inter_agent|<agent_message|<agent-message|# AGENTS\\.md instructions)");
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
def args_kv: . as $s | if type == "string" then (try ($s | fromjson | kv) catch $s) else kv end;
def status_of($out):
  if $out == null then "running"
  else ($out | pre(3000) | (capture("(?i)exit(?:ed with)?[ _]?code[\": ]+(?<c>-?[0-9]+)") // capture("\"exit_code\"[: ]+(?<c>-?[0-9]+)") // null)) as $m
    | if $m == null then "unknown" elif ($m.c | tonumber) == 0 then "ok" else "error" end
  end;
# The exec output starts with a fixed header (Chunk ID, Wall time, exit code,
# token count, "Output:"); the excerpt starts at the output itself. The status
# is read from the full text before this is applied.
def strip_exec_header: if startswith("Chunk ID:") then (split("\nOutput:\n") | if length > 1 then (.[1:] | join("\nOutput:\n")) else join("") end) else . end;
def id: ((.payload.id // "") + "@" + (.timestamp // ""));

def entries:
  . as $lines
  | ($lines | map(select(.type == "response_item" and (.payload | type) == "object" and ((.ordinal // null) == null or .ordinal >= $hist)))) as $L
  | (reduce ($L[] | select(.payload.type == "function_call_output" or .payload.type == "custom_tool_call_output")) as $r ({};
       .[$r.payload.call_id // ""] = ($r.payload.output | txt))) as $res
  | [ $L[]
      | . as $l
      | .payload as $p
      | if $p.type == "message" and $p.role == "user" then
          (($p.content | txt)) as $t
          | if $t != "" and ($t | injected | not) and ($l.metadata.client_authored != false)
            then {kind: "prompt", ts: ($l | ts), text: $t[0:3000], id: ($l | id)} else empty end
        elif $p.type == "message" and $p.role == "assistant" then
          (($p.content | txt)) as $t
          | if $t != "" then {kind: "text", ts: ($l | ts), text: $t[0:2400]} else empty end
        elif $p.type == "function_call" or $p.type == "custom_tool_call" then
          ($res[$p.call_id // ""]) as $o
          | {kind: "tool", ts: ($l | ts), name: ($p.name // ""),
             input: (($p.arguments // $p.input) | args_kv | .[0:400]),
             status: status_of($o), result: (($o // "") | strip_exec_header | pre(600))}
        else empty end ];

def parsed: [inputs | select(. != "") | (try fromjson catch "BAD") | if type == "object" then . else "BAD" end];

if $mode == "anchor" then
  (parsed | map(select(. != "BAD")) | entries | map(select(.kind == "prompt")) | first)
else
  parsed as $P
  | ($P | map(select(. == "BAD")) | length) as $bad
  | ($P | map(select(. != "BAD")) | entries) as $E
  | ($E | map(if .kind == "prompt" and $anchor != null and .id == $anchor.id then . + {anchor: true} else . end)) as $M
  | if $anchor == null or any($M[]; .anchor == true) then {entries: $M, window_cut: $cut, bad_lines: $bad}
    else {entries: ([$anchor + {anchor: true}] + $M), window_cut: true, bad_lines: $bad} end
end
