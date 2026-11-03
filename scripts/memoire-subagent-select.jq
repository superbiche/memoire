# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
#
# memoire-subagent-select.jq — the ONE selection rule of the sub-agent parent
# context block (Claude Code and Codex injectors; the OpenCode plugin mirrors
# it in JavaScript, pinned by scripts/tests/subagent-context-test.sh).
#
# Input : {"entries": [...], "window_cut": bool}
#   entries, chronological; each {kind: prompt|text|tool, ts: "HH:MM:SS", ...}
#     prompt {text}   text {text}   tool {name, input, status, result}
#   exactly zero or one entry carries "anchor": true (the prompt that opened
#   the parent's current turn).
# Args  : --argjson maxc N (block chars) --argjson maxe N (entries)
#         --arg header S --arg pointer S
# Output: the block as raw text ("" when there is nothing to say).
#
# Fit (round 3): the block is at most maxc characters for every accepted maxc
# (600 and up). The anchor line is cut (with an ellipsis) to the room left
# after the header, the pointer, the separators and a reserve of 80 for the
# boundary and omission lines, and never below 120 characters; when even that
# does not fit, the block is the header and the pointer only (which exceeds
# maxc only when those two lines alone do). The entries fill what remains.
#
# Block order: header; earlier-turn entries (only when budget remains);
# "--- current turn ---" (only when such entries exist); the anchor (or the
# no-prompt note); the current turn's entries, oldest to newest; the omission
# line; the pointer. The walk is newest first, current turn before earlier
# turns, and stops at the first entry that does not fit, so the kept set is a
# contiguous newest suffix (anchor excluded).

# flat: every run of whitespace, C0/C1 control characters (NUL, ESC, U+0085
# included) and the Unicode line breaks U+2028/U+2029 becomes one space, so a
# parent's content cannot imitate the block's own lines. scan is linear in
# the string; gsub is not (measured: 15 s on 160 KB). Every text is cut to a
# bounded prefix BEFORE it is flattened, so the cost is bounded per entry.
def flat: [scan("[^\u0000-\u0020\u007f-\u009f\u2028\u2029]+")] | join(" ");
def cap($n): if length > $n then .[0:($n - 1)] + "…" else . end;
def pre($n): if length > (2 * $n) then .[0:$n] + " " + .[(length - $n):] else . end;
def excerpt: pre(600) | flat | if length > 220 then .[0:140] + "…" + .[(length - 79):] else . end;
def line:
  if .kind == "prompt" then "> [prompt] " + (.text | .[0:3000] | flat | cap(1500))
  elif .kind == "text" then "- " + .ts + " text: " + (.text | .[0:2400] | flat | cap(1200))
  else
    "- " + .ts + " tool "
    + ([((.name // "") | .[0:200] | flat | cap(60)), ((.input // "") | .[0:400] | flat | cap(200))] | map(select(. != "")) | join(" "))
    + " → " + .status
    + (if (.result // "") != "" then " " + (.result | excerpt) else "" end)
  end;

def mina: 120;

(.entries // []) as $E
| (.window_cut // false) as $cut
| ($E | length) as $n
| ($E | map(.anchor == true) | index(true)) as $a
| ($maxc - ($header | length) - ($pointer | length) - 4 - 80) as $room
| (if $a == null then "(no operator prompt in the recent window)" else ($E[$a] | line) end) as $top0
| (if ($top0 | length) <= $room then $top0
   elif $room >= mina then $top0[0:($room - 1)] + "…"
   else null end) as $top
| if $n == 0 then ""
  elif $top == null then ([$header, $pointer] | join("\n"))
  else
    ($maxc - ($header | length) - ($pointer | length) - ($top | length) - 4 - 80) as $budget
    | (if $a == null then [range($n - 1; -1; -1)]
       else [range($n - 1; $a; -1)] + [range($a - 1; -1; -1)] end) as $order
    # lines are rendered lazily, only for the entries the walk visits
    | (reduce $order[] as $i ({used: 0, kept: [], stop: false};
        if .stop then .
        elif (.kept | length) >= $maxe then .stop = true
        else ($E[$i] | line) as $l
          | if (.used + ($l | length) + 1) > $budget then .stop = true
            else .used += ($l | length) + 1 | .kept += [{i: $i, l: $l}] end
        end)) as $st
    | ($st.kept | sort_by(.i)) as $kept
    | ($kept | map(select($a != null and .i < $a) | .l)) as $pre
    | ($kept | map(select($a == null or .i > $a) | .l)) as $cur
    | (($n - (if $a == null then 0 else 1 end)) - ($kept | length)) as $omit
    | ([$header]
       + $pre
       + (if ($pre | length) > 0 then ["--- current turn ---"] else [] end)
       + [$top]
       + $cur
       + (if $omit > 0 then ["(+" + ($omit | tostring) + " " + (if $cut then "or more " else "" end) + "earlier entries omitted)"]
          elif $cut then ["(earlier entries not read)"]
          else [] end)
       + [$pointer]) | join("\n")
  end
