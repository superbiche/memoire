#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-inject-claude.sh — SessionStart hook: emit recent semantic Memoire
# observations for the current project as additional context.
#
# INTERIM WIRING (2026-07-10): CC-only "smoke with benefits" for injection
# Slice A1. Per-harness injection wiring is Liaison's job long-term (like
# liaison/skills_link.py owns skills wiring); this script is used until the
# Liaison-owned version lands, then retired. Do not grow harness branches here.
#
# Reads the Claude Code hook JSON from stdin (uses .cwd for the project name)
# and prints up to two compact tagged blocks from GET /v1/timeline:
#   1. recent context (exclude_kind=raw + include_proposed=true)
#   2. operating notes (can_instruct=true — elevated observations), only when
#      non-empty; disable with MEMOIRE_INJECT_NOTES=0.
# Pull mode (pull-based retrieval v0): `inject_content: false` in the
# project's .memoire (or MEMOIRE_INJECT_CONTENT=off) replaces block 1's
# content with a search-on-subject rule; state headers and operating notes
# keep riding.
# Fail-open: any error exits 0, each block independently, so session start is
# never blocked.

# Budget anchor: captured FIRST so local prelude work (resolver calls,
# git probes) draws down the same clock the hook deadline does. Consumed
# by net_call_timeout() below.
INJECT_START_US=""
[[ -n "${EPOCHREALTIME:-}" ]] && INJECT_START_US="${EPOCHREALTIME//[.,]/}"

MEMOIRE_HOME="${MEMOIRE_HOME:-$HOME/.memoire}"
ADDR="${MEMOIRE_ADDR:-127.0.0.1:37778}"
# MEMOIRE_BASE_URL is the ratified single knob for every API consumer
# (memoire:D165, R-10 §3.2); MEMOIRE_ADDR stays as the host:port fallback.
BASE_URL="${MEMOIRE_BASE_URL:-http://$ADDR}"
RESOLVER="${MEMOIRE_RESOLVE_PROJECT:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/memoire-resolve-project.sh}"
TOKEN_FILE="${MEMOIRE_TOKEN_FILE:-$MEMOIRE_HOME/token}"
LIMIT="${MEMOIRE_INJECT_LIMIT:-15}"
TIMEOUT_S="${MEMOIRE_INJECT_TIMEOUT:-2}"
NOTES_ENABLED="${MEMOIRE_INJECT_NOTES:-1}"
NOTES_LIMIT="${MEMOIRE_INJECT_NOTES_LIMIT:-10}"
# Validated: NOTES_LIMIT feeds shell arithmetic (overflow detection), and
# the overflow probe requests cap+1 — which must stay within the
# timeline's server-side max of 500 (normalizeLimit FALLS BACK to 50 on
# an over-max request rather than clamping, which would silently lose
# notes with no warning). Hence the 499 ceiling.
[[ "$NOTES_LIMIT" =~ ^[1-9][0-9]*$ ]] || NOTES_LIMIT=10
(( NOTES_LIMIT > 499 )) && NOTES_LIMIT=499

fail_open() { exit 0; }

# Capture-backlog header (memoire:D147 slice 3 / D153): the hub cannot see
# what never arrived, so the CLIENT surfaces its own queue depth where the
# operator already looks. Pure local dir counts — deliberately BEFORE the
# curl/jq/token preflights, because a remote-only capture client (forwarder
# topology, no local memoire-api, no local token) is exactly the machine
# that must still see its backlog (slice-3 round-1 finding 2). Quiet in the
# healthy steady state (the drainer/forwarder clears items in seconds); loud
# when the backlog is deep, old, or poisoned. Fail-open like every block.
if [[ "${MEMOIRE_INJECT_BACKLOG:-1}" != "0" ]]; then
  BACKLOG_MIN="${MEMOIRE_INJECT_BACKLOG_MIN:-25}"
  BACKLOG_AGE_S="${MEMOIRE_INJECT_BACKLOG_AGE_S:-600}"
  # L4(b) dead-transport escalation (memoire:D198): the operational line
  # below rode every workstation session while the D187 forwarder was dead for
  # four days and produced no action — an age beyond any healthy gap needs
  # a DISTINCT, imperative surface, not another quiet line. Threshold
  # default 21600s (6h) is PROVISIONAL pending operator sign-off.
  TRANSPORT_DEAD_S="${MEMOIRE_INJECT_TRANSPORT_DEAD_S:-21600}"
  # Every threshold knob is normalized to a bounded base-10 integer BEFORE
  # any arithmetic (review round-1 finding 4): an invalid operational knob
  # aborted the block under `set -u` (a fail-open violation — this script
  # must never block SessionStart), and an int64-overflowing value wrapped
  # to zero and cried TRANSPORT DEAD over a fresh queue. Nine digits keeps
  # every comparison far from the wrap.
  [[ "$BACKLOG_MIN" =~ ^[1-9][0-9]{0,8}$ ]] || BACKLOG_MIN=25
  [[ "$BACKLOG_AGE_S" =~ ^[1-9][0-9]{0,8}$ ]] || BACKLOG_AGE_S=600
  [[ "$TRANSPORT_DEAD_S" =~ ^[1-9][0-9]{0,8}$ ]] || TRANSPORT_DEAD_S=21600
  PENDING_DEPTH="$(find "$MEMOIRE_HOME/queue/pending" -maxdepth 1 -name '*.json' -type f 2>/dev/null | wc -l)" || PENDING_DEPTH=0
  FAILED_DEPTH="$(find "$MEMOIRE_HOME/queue/failed" -maxdepth 1 -name '*.json' -type f ! -name '*.error.json' 2>/dev/null | wc -l)" || FAILED_DEPTH=0
  OLDEST_AGE=0
  if [[ "$PENDING_DEPTH" -gt 0 ]]; then
    OLDEST_EPOCH="$(find "$MEMOIRE_HOME/queue/pending" -maxdepth 1 -name '*.json' -type f -printf '%T@\n' 2>/dev/null | sort -n | head -1 | cut -d. -f1)" || OLDEST_EPOCH=""
    [[ -n "$OLDEST_EPOCH" ]] && OLDEST_AGE=$(( $(date +%s) - OLDEST_EPOCH ))
  fi
  if [[ "$OLDEST_AGE" -ge "$TRANSPORT_DEAD_S" ]]; then
    if (( OLDEST_AGE >= 172800 )); then OLDEST_HUMAN="$(( OLDEST_AGE / 86400 ))d"; else OLDEST_HUMAN="$(( OLDEST_AGE / 3600 ))h"; fi
    printf '# [memoire] TRANSPORT DEAD — tell the operator in your FIRST reply\n'
    printf '%s events stuck local->hub for %s (dead-transport threshold %ss): nothing has drained this queue for longer than any healthy gap — the D187 silent-death class. Do not silently continue: name this in your first reply and propose the checks (systemctl --user status memoire-forward.service on a thin client / the hub service on the hub; docs/RUNBOOK.md "Remote capture forwarding").\n\n' \
      "$PENDING_DEPTH" "$OLDEST_HUMAN" "$TRANSPORT_DEAD_S"
  elif [[ "$PENDING_DEPTH" -ge "$BACKLOG_MIN" || "$OLDEST_AGE" -ge "$BACKLOG_AGE_S" || "$FAILED_DEPTH" -gt 0 ]]; then
    printf '# [memoire] CAPTURE BACKLOG — surface this to the operator\n'
    printf '%s events pending local->hub delivery (oldest %ss old), %s failed. Events are safe in the local queue but not yet in the store; check the drainer/forwarder (memoire-forward -status; docs/RUNBOOK.md "Remote capture forwarding").\n\n' \
      "$PENDING_DEPTH" "$OLDEST_AGE" "$FAILED_DEPTH"
  fi
fi

# jq is needed from here on (hook-JSON parse); curl/token are READ
# prerequisites and are checked later, after every local-only block — a
# client with no read token or an unreachable hub must still emit the
# local-only blocks (pull rule, identity ask), round-1 review finding.
command -v jq >/dev/null 2>&1 || fail_open

HOOK_JSON="$(cat 2>/dev/null || true)"
CWD_VALUE="$(printf '%s' "$HOOK_JSON" | jq -r '.cwd // empty' 2>/dev/null)"
[[ -n "$CWD_VALUE" ]] || CWD_VALUE="${CLAUDE_PROJECT_DIR:-$PWD}"

# Attribution must match the capture side (same shared resolver over the hook
# cwd = session home) so injected context is read from the SAME project id the
# session's events are written under. Fail-open to basename. Verbose form:
# MRP_SOURCE drives the liaison:D239 ask-on-missing block below.
MRP_KEY="" MRP_ROOT="" MRP_SOURCE=""
if [[ -n "${MEMOIRE_PROJECT:-}" ]]; then
  PROJECT="$MEMOIRE_PROJECT" MRP_SOURCE="env"
else
  out="$("$RESOLVER" --verbose "$CWD_VALUE" 2>/dev/null)" || out=""
  IFS=$'\t' read -r MRP_KEY MRP_ROOT MRP_SOURCE <<<"$out"
  PROJECT="$MRP_KEY"
  if [[ -z "$PROJECT" ]]; then
    PROJECT="$(basename "$CWD_VALUE" 2>/dev/null)" || PROJECT="$CWD_VALUE"
    MRP_SOURCE=""
  fi
fi
[[ -n "$PROJECT" ]] || fail_open

# Pull mode (pull-based retrieval v0): on projects flagged with
# `inject_content: false` in the nearest .memoire (or session-level
# MEMOIRE_INJECT_CONTENT=off), observation CONTENT is not injected — the
# session searches once the work subject is clear. STATE stays push:
# backlog, clock, identity blocks and the operating notes are unaffected.
# Fail-open toward current behavior: only an explicit "off" from the
# resolver flips pull mode; a missing/erroring resolver keeps injection on.
# The gate keys on the session cwd and is orthogonal to MEMOIRE_PROJECT,
# same contract as the capture kill switch (liaison:D239 item 6).
CONTENT_ENABLED=1
if [[ "$("$RESOLVER" --inject-content-enabled "$CWD_VALUE" 2>/dev/null)" == "off" ]]; then
  CONTENT_ENABLED=0
fi

CONTEXT_PRINTED=0
if [[ "$CONTENT_ENABLED" != "1" ]]; then
  # The pull-mode rule rides INSTEAD of the content block (fleet rule,
  # deployed through this script's wiring — liaison memoire link). Printed
  # HERE, before any token/curl preflight or network call: the rule is
  # static local text and must survive missing read credentials and an
  # unreachable hub (round-1 review finding).
  printf '# [memoire] pull mode — content injection is OFF for this project\n'
  printf 'Recent-observation content is not auto-injected here (.memoire: inject_content: false). REQUIRED: once the work subject is clear — after the first real prompt or clarification, not before — issue Memoire search(es) for that subject (memoire_search MCP tool, or the my-memory discipline) and ground the work in what returns. Do not skip this because the loaded context already seems sufficient. Every search issued here also feeds the retrieval pilot'"'"'s reference query corpus (memoire:D183).\n\n'
  CONTEXT_PRINTED=1
fi

# liaison:D239 item 3 — ask-on-missing, never blocking: in a git repo without a
# .memoire file (resolution fell through to basename), carry a one-line
# instruction proposing the file with a suggested key. Key convention: git
# root relative to a strip-prefix list (MEMOIRE_KEY_PREFIXES, colon-separated,
# default $HOME/dev), slashes -> dashes. Outside the prefixes: propose without
# suggesting a conventional key. The decision is the operator's; capture keeps
# flowing under basename either way, and project-move folds early rows after.
if [[ "$MRP_SOURCE" == "basename" ]]; then
  GIT_ROOT="$(git -C "$CWD_VALUE" rev-parse --show-toplevel 2>/dev/null)" || GIT_ROOT=""
  if [[ -n "$GIT_ROOT" ]]; then
    SUGGESTED=""
    IFS=':' read -ra PREFIXES <<<"${MEMOIRE_KEY_PREFIXES:-$HOME/dev}"
    for prefix in "${PREFIXES[@]}"; do
      prefix="${prefix%/}"
      [[ -n "$prefix" ]] || continue
      if [[ "$GIT_ROOT" == "$prefix"/* ]]; then
        SUGGESTED="${GIT_ROOT#"$prefix"/}"
        SUGGESTED="${SUGGESTED//\//-}"
        break
      fi
    done
    printf '# [memoire] project identity — no .memoire file in this repo\n'
    if [[ -n "$SUGGESTED" ]]; then
      printf 'Captures use fallback key "%s". Propose to the operator (their decision): create %s/.memoire with `project: %s`, then run memoire-project-move to fold rows captured under the old key.\n\n' \
        "$PROJECT" "$GIT_ROOT" "$SUGGESTED"
    else
      printf 'Captures use fallback key "%s". Propose to the operator (their decision): create %s/.memoire with an explicit `project:` key, then run memoire-project-move to fold rows captured under the old key.\n\n' \
        "$PROJECT" "$GIT_ROOT"
    fi
  fi
fi

# Read prerequisites — everything below talks to the API. Fail-open exit
# here loses only the network blocks; every local-only block has already
# been emitted above.
command -v curl >/dev/null 2>&1 || fail_open
[[ -r "$TOKEN_FILE" ]] || fail_open
TOKEN="$(cat "$TOKEN_FILE" 2>/dev/null)" || fail_open
[[ -n "$TOKEN" ]] || fail_open

# Aggregate network budget (round-1/round-2 review findings): the per-call
# -m bound does not cap the SUM of calls — hangs against a blackholed hub
# overrun a 5s SessionStart hook deadline and can get the whole context
# discarded. Each request's -m is capped to the REMAINING budget (a
# start-only check was round-2's gap: a call starting near the budget end,
# or a raised MEMOIRE_INJECT_TIMEOUT, still overran the deadline), so a
# request either fits inside the budget — +1s worst case from SECONDS'
# whole-second granularity — or does not start. SECONDS counts from script
# start, drawing down the same clock the hook does; each block stays
# independently fail-open.
BUDGET_S="${MEMOIRE_INJECT_BUDGET_S:-4}"
[[ "$BUDGET_S" =~ ^[1-9][0-9]*$ ]] || BUDGET_S=4
[[ "$TIMEOUT_S" =~ ^[1-9][0-9]*$ ]] || TIMEOUT_S=2
# Sub-second deadline via EPOCHREALTIME (bash >= 5; microseconds, decimal
# separator is locale-dependent so both . and , are stripped). 0.5s
# headroom stays reserved inside the budget for post-call parsing (jq,
# alert formatting), and a request that cannot get 0.2s does not start —
# round-3 finding: whole-second SECONDS granularity ate the entire margin.
# Fallback without EPOCHREALTIME: whole-second remaining with 1s reserved.
# The INJECT_START_US anchor is captured at the very top of the script.
NET_HEADROOM_US=500000
net_call_timeout() {  # prints the capped -m value; fails when exhausted
  local now_us rem_us cap_us
  if [[ -n "$INJECT_START_US" && -n "${EPOCHREALTIME:-}" ]]; then
    now_us="${EPOCHREALTIME//[.,]/}"
    rem_us=$(( BUDGET_S * 1000000 - NET_HEADROOM_US - (now_us - INJECT_START_US) ))
  else
    rem_us=$(( (BUDGET_S - 1 - SECONDS) * 1000000 ))
  fi
  (( rem_us >= 200000 )) || return 1
  cap_us=$(( TIMEOUT_S * 1000000 ))
  (( rem_us > cap_us )) && rem_us=$cap_us
  printf '%d.%02d' $(( rem_us / 1000000 )) $(( (rem_us % 1000000) / 10000 ))
}

# Clock-skew header (memoire:D152): one-sided future-skew alert from
# /v1/health — occurred_at AHEAD of received_at beyond the threshold is a
# producer clock fault. Unauthenticated endpoint, fail-open.
if [[ "${MEMOIRE_INJECT_CLOCK_ALERT:-1}" != "0" ]] && CALL_M="$(net_call_timeout)"; then
  SKEW_COUNT="$(curl -sf -m "$CALL_M" "$BASE_URL/v1/health" 2>/dev/null | jq -r '.clock_skew_future_events // 0' 2>/dev/null)" || SKEW_COUNT=0
  if [[ "$SKEW_COUNT" =~ ^[0-9]+$ && "$SKEW_COUNT" -gt 0 ]]; then
    printf '# [memoire] CLOCK ALERT — surface this to the operator\n'
    printf '%s events have occurred_at AHEAD of received_at beyond the skew threshold: a producer clock is wrong. Check chrony/NTP on every capture machine (memoire:D152, D149).\n\n' \
      "$SKEW_COUNT"
  fi
fi

# Review-backlog header (memoire:D192): one quiet line of push STATE when
# this project has unreviewed derivation output — "N proposed across M
# sessions" plus the UI to act in. Deterministic and counted (D190
# doctrine); it names work, it does not nag — "ignoring the review queue
# is a supported lifestyle" stays binding, so the line only appears when
# non-zero and dies entirely with MEMOIRE_INJECT_REVIEW=0. Uses the
# sessions surface (per-session proposed_count summed client-side): no
# server change, one budget-gated read, fail-open like every block.
if [[ "${MEMOIRE_INJECT_REVIEW:-1}" != "0" ]] && CALL_M="$(net_call_timeout)"; then
  REVIEW_RESPONSE="$(curl -sf -m "$CALL_M" --get \
    --data-urlencode "project=$PROJECT" \
    --data-urlencode "proposed_only=true" \
    --data-urlencode "limit=100" \
    "$BASE_URL/v1/sessions" \
    -H "Authorization: Bearer $TOKEN" 2>/dev/null)" || REVIEW_RESPONSE=""
  if [[ -n "$REVIEW_RESPONSE" ]]; then
    REVIEW_LINE="$(printf '%s' "$REVIEW_RESPONSE" | jq -r --arg now "$(date +%s)" '
      [.sessions // [] | .[] | select((.proposed_count // 0) > 0)] as $s
      | ([$s[].proposed_count] | add // 0) as $n
      | if $n > 0 then
          ([$s[].started_at_epoch] | min) as $oldest
          | "\($n) proposed observation\(if $n == 1 then "" else "s" end) across \($s | length) session\(if ($s | length) == 1 then "" else "s" end)\(if $oldest != null and $oldest > 0 then " (oldest ~\((($now | tonumber) - $oldest) / 86400 | floor)d)" else "" end)"
        else empty end
    ' 2>/dev/null)" || REVIEW_LINE=""
    if [[ -n "$REVIEW_LINE" ]]; then
      printf '# [memoire] review backlog — %s\n' "$PROJECT"
      printf '%s — review at %s/ui/ (Sessions tab: skim the story, keep the gems, lapse the rest).\n\n' \
        "$REVIEW_LINE" "$BASE_URL"
    fi
  fi
fi

# Block 1: recent context. Fail-open per block — a failure here must not
# suppress the operating-notes block below, and vice versa. In pull mode the
# fetch still happens with limit=1: identity alerts attach server-side to
# project-filtered timeline reads and are STATE, so they must keep riding.
FETCH_LIMIT="$LIMIT"
[[ "$CONTENT_ENABLED" == "1" ]] || FETCH_LIMIT=1
RESPONSE=""
if CALL_M="$(net_call_timeout)"; then
  RESPONSE="$(curl -sf -m "$CALL_M" --get \
    --data-urlencode "project=$PROJECT" \
    --data-urlencode "exclude_kind=raw" \
    --data-urlencode "include_proposed=true" \
    --data-urlencode "limit=$FETCH_LIMIT" \
    "$BASE_URL/v1/timeline" \
    -H "Authorization: Bearer $TOKEN" 2>/dev/null)" || RESPONSE=""
fi

LINES=""
if [[ "$CONTENT_ENABLED" == "1" ]]; then
  LINES="$(printf '%s' "$RESPONSE" | jq -r '
    .hits // [] | .[] |
    "- [\(.id)] \(.kind)\(if .review_state == "proposed" or .review_state == "lapsed" then "~" else "" end) \((.captured_at_epoch // 0) | strftime("%Y-%m-%d")): \((.snippet // .text // "") | gsub("\\s+"; " ") | .[0:220])"
  ' 2>/dev/null)" || LINES=""
fi

# Identity-conflict alerts (liaison:D239 item 4): the server attaches them to
# project-filtered timeline reads. Loud, first, and fail-open — a parse
# failure must never suppress the context blocks.
ALERT_LINES="$(printf '%s' "$RESPONSE" | jq -r '.identity_alerts // [] | .[] | "! " + .' 2>/dev/null)" || ALERT_LINES=""
if [[ -n "$ALERT_LINES" ]]; then
  printf '# [memoire] IDENTITY ALERT — surface this to the operator\n%s\n\n' "$ALERT_LINES"
fi

if [[ -n "$LINES" ]]; then
  printf '# [memoire] recent context — %s (last %s semantic observations; ~ = unreviewed: proposed or lapsed)\n%s\n' \
    "$PROJECT" "$(printf '%s\n' "$LINES" | wc -l)" "$LINES"
  CONTEXT_PRINTED=1
fi

# Block 2: operating notes — elevated (can_instruct=1) observations for this
# project. Emitted only when non-empty; no ~ marker (elevated observations
# are user-confirmed by construction). Killable independently via
# MEMOIRE_INJECT_NOTES=0. Fetches limit+1 to DETECT overflow (memoire:D191):
# the read is newest-first and capped, so more elevated observations than
# the cap means older standing orders silently stop riding — that eviction
# must be loud, never silent.
if [[ "$NOTES_ENABLED" != "0" ]] && CALL_M="$(net_call_timeout)"; then
  NOTES_RESPONSE="$(curl -sf -m "$CALL_M" --get \
    --data-urlencode "project=$PROJECT" \
    --data-urlencode "exclude_kind=raw" \
    --data-urlencode "can_instruct=true" \
    --data-urlencode "limit=$(( NOTES_LIMIT + 1 ))" \
    "$BASE_URL/v1/timeline" \
    -H "Authorization: Bearer $TOKEN" 2>/dev/null)" || NOTES_RESPONSE=""

  NOTES_LINES="$(printf '%s' "$NOTES_RESPONSE" | jq -r '
    .hits // [] | .[] |
    "- [\(.id)] \(.kind) \((.captured_at_epoch // 0) | strftime("%Y-%m-%d")): \((.snippet // .text // "") | gsub("\\s+"; " ") | .[0:220])"
  ' 2>/dev/null)" || NOTES_LINES=""

  if [[ -n "$NOTES_LINES" ]]; then
    NOTES_COUNT="$(printf '%s\n' "$NOTES_LINES" | wc -l)"
    [[ "$CONTEXT_PRINTED" == "1" ]] && printf '\n'
    if (( NOTES_COUNT > NOTES_LIMIT )); then
      printf '# [memoire] operating notes — %s (OVERFLOW: more than %s elevated observations exist; older standing orders are benched — NOT riding — surface this to the operator: review and demote)\n%s\n' \
        "$PROJECT" "$NOTES_LIMIT" "$(printf '%s\n' "$NOTES_LINES" | head -n "$NOTES_LIMIT")"
    else
      printf '# [memoire] operating notes — %s (%s elevated observations; operator-confirmed, ride every session)\n%s\n' \
        "$PROJECT" "$NOTES_COUNT" "$NOTES_LINES"
    fi
  fi
fi
exit 0
