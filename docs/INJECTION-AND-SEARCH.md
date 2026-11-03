# Injection & search — the read-surface contract

Rework boundary: this document describes existing behavior and compatibility.
The approved target direction and agent discretion live in the maintainers'
rework mandate (private ledger, `policies/rework.md`). Existing review gestures and read filters
do not prescribe the replacement architecture. Unrelated evidence, privacy and
published consumer obligations remain applicable.

Filed as MISSING #2 by the 2026-07-24 nightwatch recon (the `inject-and-search`
gesture was the only one of the eight with no owning document); authored
2026-08-05 under D235.

This document owns two surfaces as contracts: the session-start injection
surface — what a harness receives before the operator's first word, **when
injection is switched on (it is off by default; see "Master
switch" below)** — and the
search/read surface, HTTP and MCP, a session reaches for afterwards. It states
what the blocks are, how their content is selected, which knob resolves the API
base, and what foreign clients are owed. Operational detail stays in
`RUNBOOK.md` and the root `QUICKSTART.md`; current state, in `VERSION` and `MISSING.md`.

## Master switch: off by default

Session-start injection is **off by default**. `MEMOIRE_INJECT=on|1|true` turns
it on; unset, or any other value, the injector prints nothing at all — no block,
no health header, no operating notes — makes no network call, reads no token,
and exits 0. The SessionStart entries stay registered: the hook fires and exits
silently. Retrieval is pull: the search and MCP tools are unchanged and are the
path to Memoire content.

The switch is read like the other opt-in switches: `capture-tick.env` under
`$MEMOIRE_HOME` wins when it carries the key (last assignment, never sourced);
otherwise the process environment decides. With the switch on, everything below
holds unchanged, every existing knob included. Everything below this section
describes the behavior **with `MEMOIRE_INJECT=on`**.

## The state/knowledge split (vocabulary is binding)

**STATE is PUSH; KNOWLEDGE is PULL on flagged projects** (D190) — the design
line of the whole injection surface, and both words are used in that sense
everywhere below. STATE is the deterministic, counted, near-zero-cost facts
about the system a session runs inside: capture backlog, dead transport, clock
skew, identity ask and alerts, review backlog. They hold regardless of the
session's subject and cost a handful of lines. **State blocks are never
suppressed by pull mode.**

KNOWLEDGE is observation content — the recent-context block. Blanket content
injection is a cache that can lie: it claims to know relevance before the
session's subject exists. On a flagged project, `inject_content: false` in the
nearest `.memoire` (or the session-level `MEMOIRE_INJECT_CONTENT=off`)
**replaces the content block with a deferred-search rule** — search once the
work subject is clear, after the first real prompt or clarification, never
before. The gate lives in the shared resolver (`--inject-content-enabled`).
**Once injection is on, content is on by default and the toggle is opt-in per project:** only an explicit
`off` flips pull mode, and a missing or erroring resolver keeps injection on —
fail-open points at the current behavior, never at silence.

**Operating notes stay PUSH even on flagged projects** (D190, D191): elevated
`can_instruct` observations are operator-promoted standing orders with an
explicit ride-every-session contract (D146 R-5 family) — a subscription, not
relevance-guessed knowledge, and pulling them would silently break it.

## The session-start blocks and their selection rules

**There is one inject script for every harness**,
`scripts/memoire-inject-claude.sh`: Claude Code, Codex, opencode and Pi run it
directly, qwen through the envelope wrapper. Per-harness branches do not belong
in it — a second implementation is how parity dies.
The early exits are `MEMOIRE_ATTRIB_FORK` — the attribution fork of a Claude
session (`scripts/memoire-attribute-claude.py`) has no tools and must answer
from its own transcript, so the script emits nothing inside it — and the master
switch above (nothing unless `MEMOIRE_INJECT` is on).

The blocks, in emission order, each with the condition that raises it:

- `# [memoire] TRANSPORT DEAD — tell the operator in your FIRST reply` — oldest
  pending file older than `MEMOIRE_INJECT_TRANSPORT_DEAD_S` (21600s); imperative
  because the quiet line below rode four dead days unactioned (D198).
- `# [memoire] CAPTURE BACKLOG — surface this to the operator` — pending depth
  ≥ `MEMOIRE_INJECT_BACKLOG_MIN` (25), oldest ≥ `MEMOIRE_INJECT_BACKLOG_AGE_S`
  (600s), or any failed item — the hub cannot see what never arrived (D153).
- `# [memoire] pull mode — content injection is OFF for this project` — pull
  mode is on; rides instead of the content block.
- `# [memoire] project identity — no .memoire file in this repo` — resolution
  fell through to basename in a git repo; proposes a key, operator decides.
- `# [memoire] CLOCK ALERT — surface this to the operator` —
  `clock_skew_future_events` on `/v1/health` is non-zero (D152).
- `# [memoire] review backlog — <project>` — unreviewed derivation output,
  summed client-side over `GET /v1/sessions`; it names work, never nags (D192).
- `# [memoire] IDENTITY ALERT — surface this to the operator` — the server
  attached identity-conflict lines to the project-filtered timeline read.
- `# [memoire] recent context — <project> (last N semantic observations; ~ =
  unreviewed: proposed or lapsed)` — the KNOWLEDGE block.
- `# [memoire] operating notes — <project> (N elevated observations;
  operator-confirmed, ride every session)` — the standing orders.

**Recent context is recency, not semantic ranking.** It is `GET /v1/timeline`
with `exclude_kind=raw`, `include_proposed=true`, and a default limit of 15
(`MEMOIRE_INJECT_LIMIT`) — newest first, no query and no scoring. "Semantic" in
the header means non-raw kinds, nothing more; reading it as relevance ranking
is the misunderstanding this sentence prevents. A `~` marks `review_state`
proposed or lapsed (D146).

**Operating notes are the elevated set, and their eviction must be loud.** Same
timeline read with `can_instruct=true`; that flag's semantics are owned by
`ELEVATION-DESIGN.md`, not here, and no `~` appears by construction. The read
fetches cap+1 (cap default 10, `MEMOIRE_INJECT_NOTES_LIMIT`) purely to detect
overflow: the read is newest-first and capped, so more elevated observations
than the cap means older standing orders stopped riding — the header then says
so and instructs review/demote (D191).

Four behaviors bind every block. **Per-block error independence, within the
budget:** a failed request never aborts the blocks after it — but the budget is
one pot, so a slow failure can exhaust it, and the remaining network blocks are
then skipped by design: the deadline is the contract, and every local block has
already printed. **Local-only blocks precede any network preflight** —
the backlog pair, the pull rule, and the identity ask are pure local file work
printed ahead of the curl/token checks, so a client with no read token or a
blackholed hub still gets them. **Fail-open everywhere:** every path ends in
`exit 0` — injection must never block a session start. **The network budget is
aggregate:** `MEMOIRE_INJECT_BUDGET_S` (default 4) bounds the sum of the reads
and rides under the 5s SessionStart hook deadline; each request's timeout is
capped to the remaining budget, or it does not start.

**Project scoping is resolved by one shared resolver**,
`scripts/memoire-resolve-project.sh`, in fixed precedence: `MEMOIRE_PROJECT`,
the nearest `.memoire` walking up, longest real path-prefix match in the
registry, then basename as the fail-open fallback; linked worktrees fold to the
parent checkout (D164). Injection must read the project id capture writes under
— hence one resolver, not two derivations.

## The MCP tool surface and its stability contract

Four tools, read-only: `memoire_search` (full-text search, `q` required),
`memoire_timeline` (most recent observations, no required param),
`memoire_get_observations` (full bodies by id, `ids` required, **max 50 per
call**), `memoire_context` (prompt-ready context for a topic, `q` required).
`memoire_search`, `memoire_timeline` and `memoire_context` take a `days`
parameter (default 30; 0 lifts the bound) that bounds the read to the last
`days` days by occurred time.

**L0010 R1 = A made the MCP read a dated recent raw lookup.** Hindsight owns
memory reads — what was decided or learned — so the tools no longer ask for
derived observations at all: the `kind`, `exclude_kind` and `include_proposed`
parameters are retired, the tools send no `include_derived`, and they serve
raw capture projections (the API's default) bounded by `days`.
`memoire_context` lines carry the occurrence date
(`[id] YYYY-MM-DD project label: snippet`). A by-id fetch
(`memoire_get_observations`, `GET /v1/observations/{id}`) is exact and takes
no `occurred_after` bound. The compact
hit-line format above is unchanged; the sibling continuation line it could
carry for co-derived hits no longer appears, because raw hits carry no
derivation run.

**The tool descriptions carry the standing rule for the consumer** (operator
ruling 2026-09-25, `docs/rework/retrieval-consumer/README.md` "Ruled"; text
rewritten by L0010, 2026-11-01): the memory skill is never loaded by agents,
the tool declaration is the carrier they always read. `services/mcp/tools.go`
appends two constants to the descriptions — `memoryRule` on all four tools
(Memoire is a dated capture log: raw records of what happened, prompts, tool
calls, tool output, ids; dated evidence, not current truth; for what was
decided or learned, use Hindsight; a hit corroborates and dates; cite the
observation id in brackets; verify against the files when one carries the
hit, say when none is found) and `memoryFetchBodies` on the three tools that
return compact hits (fetch the full body of every hit relied on).
`memorySearchShapes` (ticket id, then failure class in words, then feature or
entity) was retired with L0010: it steered searches at the derived memory
Hindsight now owns. Measured on unit 4 of the replay campaign, the rule alone
moved citations from 0/3 to 2/3; the citation discipline it carried is kept
in the rewritten text. The OpenAI Responses route Codex uses has no
1024-char description cap (that cap is Chat Completions), demonstrated live
on 2026-09-25 (review chain `2026-09-25-memoire-tooldesc-rule`).

**A hit carries its own governance verdict.** `/v1/search` and `/v1/timeline`
serve, per hit, `occurred_at_epoch` (when the observation happened) and
`provenance` — the schema/0007 governance column: `observed` (captured
verbatim), `inferred` (derived by a model), `user_confirmed` (the operator
confirmed it), `imported`, `generated`, empty for rows predating the column.
`captured_at_epoch` is served alongside and is the SAME `occurred_at_epoch`
value under a misleading legacy name; it stays because the review UI, the
inject scripts and the parity tests read it, and `occurred_at_epoch` is the
honest name a new consumer should read. Nothing is captured at that instant —
`received_at_epoch` is the capture clock, and it lives on the event, not the
hit.

**The MCP compact line is
`[id] kind~ YYYY-MM-DD project provenance/review_state[!]: snippet`.** `~`
after kind marks unreviewed (`proposed` or `lapsed`, D146 R-6); `!` after
review_state marks `can_instruct=1`, an elevated operating note; provenance
renders as `-` when the server sends none, so the field count is stable. This
shape deliberately DIVERGES from the injection block's line, which is
unchanged: an MCP caller judges results without a per-id fetch round and needs
the governance values inline, where the injection block spends its budget on
body text. A downstream consumer can therefore elevation-tag and date a
result set from `memoire_search` alone.

**A hit carries its run-siblings.** `/v1/search`, `/v1/timeline`,
`/v1/context` and `/v1/observations/{id}` serve, per hit, `derivation_run_id`
and `siblings` — the other observations of that derivation run (Fibery #335).
Since #343 a run is one rhythm segment, one arc of a session, so run-siblings
are the story the hit was derived with: the decision and the task it ruled
travel together instead of ranking apart. A sibling is `{id, kind,
review_state, can_instruct}` and **no text travels in it**, so the redaction
and privacy surface is unchanged; a consumer spends
`memoire_get_observations` only on the ids it wants. At most 8 siblings ride
one hit. The context string and the MCP compact output render them as one
indented continuation line under the hit —
`  ↳ same run: [204165] task~, [204164] task~!` — where `~` marks unreviewed
(`proposed` or `lapsed`) and `!` marks `can_instruct=1`, the same two markers
the compact line uses. The compact line itself is unchanged, and a hit with
no siblings renders exactly as before.

**A sibling is always fetchable by the caller who received it** (operator
ruling, 2026-09-12). A sibling is a pointer, and a pointer that 404s is worse
than no pointer, so the sibling read applies the SAME visibility clauses
`/v1/observations/{id}` applies for that request: soft-delete, the
`include_proposed` review band, the `include_noise` exclusion and the feed
actor-gate all come from one clause builder, so the two predicates cannot
drift. `include_proposed=false` yields no unreviewed siblings and
`include_noise=false` yields no noise-flagged ones. A request's own scope
never narrows its siblings — a sibling is defined by its run, not by the
hit's project, session or kind. Two request knobs are deliberately NOT
honored on siblings because the by-id route cannot express them:
`review_state` (the band falls back to the `include_proposed` band) and
`include_agent_accepted` (the actor-gate stays closed). `raw`, `rejected`
and `superseded` rows never surface as siblings either. This is ADDITIVE
under the legend below — no tool, parameter, type
or auth change — so `API-SURFACE.json` is untouched, and the session-start
injection block and its line format are untouched too.

**Which lane a query rides.** `GET /v1/search`, `POST /v1/search` and
`POST /v1/context` all go through one `SearchWithFilter`. On the hub the
hybrid lane is deployment-default ON (`MEMOIRE_HYBRID_SEARCH=1`), so
`memoire_search` and `memoire_context` ride RRF fusion of the FTS and vector
lanes by default. The D218 literal guard routes exact-id-shaped queries —
uppercase ticket ids, `#`-refs, derivation run ids, commit shas — to FTS-only,
because fixed-depth fusion measurably dilutes literal ranking; file-path-shaped
queries are NOT literals and keep full fusion. The per-request `hybrid=true|false`
override exists on the HTTP surface only and is deliberately not an MCP tool
parameter: a tool caller gets the deployment's ruled lane, not a per-call knob.

**The MCP server is a thin read-only adapter over the HTTP surface — never a
second implementation of search or ingest** (`ARCHITECTURE.md`). That law keeps
foreign clients at parity: every tool delegates to a `/v1` endpoint, so a curl
and a tool call answer from the same code path.

**The HTTP endpoints and the MCP tools together are the versioned API surface,
and a breaking change to them is what a version bump means** (D172). That
surface is tracked in `API-SURFACE.json` — every route with its auth class and
required parameters, every tool with its parameters and their types — and
enforced by contract tests riding `make test` (the CI gate): the tests derive
what the code actually serves and fail on any divergence, so editing that file
is the deliberate act of changing the contract. BREAKING: removing or renaming
an endpoint, a tool, or a parameter; making an optional parameter required;
changing a parameter's type (or an array parameter's item type); changing an
auth class in EITHER direction — tightening breaks callers, loosening is a
security ruling and never silently additive. ADDITIVE: a new endpoint, a new
tool, a new optional MCP tool parameter.

**The gate is narrower than that taxonomy on the HTTP side, deliberately.** It
covers four things: route membership — as far as a tripwire over the server
package's own registrations binds it, so a wrapper added where the process
assembles its handler stays review's job — each route's auth class (probed
behaviorally against an enumerated credential-and-transport truth table),
the NAMES of top-level required parameters, and the `body_mode` of the routes
declaring one. That last marker names a body posture that deviates from the
ordinary named-fields one and probes it: the fail-open capture custody promise
is now gated — `POST /v1/events` must answer 202 to malformed bytes, so a
parse-before-queue change trips the suite — as is the counterpart claim that
`POST /v1/events/batch` really does refuse a body that is not JSON. Changing or
removing a route's `body_mode` is BREAKING. Three axes stay outside it: optional
parameters, parameter types — HTTP entries record names only — and nested body
shapes such as the per-item fields of an embeddings batch. Optionality and
nesting are not mechanically derivable from the handlers, and a hand-listed
set would be a cache that can lie. Changing any of the three is still a
contract change under the legend above; it is simply one no test will force
you to record. The MCP side carries no such gap: tool parameters, types, item
types and requiredness are all read from the schema `tools/list` serves. The
inventory states its own boundary; read it before ruling a diff.

**The handshake is served tokenless** (D202). `initialize` and `tools/list`
answer unconditionally; the token loads lazily on the first call and retries on
every call, so a token rendered after launch heals without a restart. An auth
failure is a per-call diagnosis naming the path tried and how it was chosen,
never silent startup death — a harness reads that death as "this server has no
tools". The reported version is build-stamped from `VERSION`; unstamped builds
report `dev` (D227).

## Sub-agent parent context

A sub-agent starts with its own prompt and nothing else: it does not know what
its parent did in the last minutes. Where the harness offers a hook at
sub-agent start, Memoire fills that gap with a **local read** of the parent's
own transcript or store on the same machine. No hub call, no queue, no capture
trigger is involved, so the block is as fresh as the parent's store.

| harness | surface | script |
|---|---|---|
| Claude Code | `SubagentStart` hook (the input carries the parent's `session_id` and `transcript_path`) | `scripts/memoire-subagent-context-claude.sh` |
| Codex | `SubagentStart` hook; 0.165 supplies the child's rollout and turn identity | `scripts/memoire-subagent-context-codex.sh` |
| opencode | `chat.message` of a child session, in `scripts/opencode-plugin/memoire.js` | the plugin (JavaScript) |

**Opt-in, inert by default.** `MEMOIRE_SUBAGENT_CONTEXT=on|1|true` in
`~/.memoire/capture-tick.env` (or `$MEMOIRE_HOME/capture-tick.env`) turns it on.
The file is read on every invocation, one key extracted, last assignment wins,
never sourced; a missing file or key means nothing is injected. The per-project
`capture: false` gate (and `MEMOIRE_CAPTURE=off`) is checked for the parent's
directory and the sub-agent's; either one off means no injection.

**Codex local compatibility.** The start hook resolves the immediate parent
from the child's `session_meta.source.subagent.thread_spawn.parent_thread_id`.
The shared `session_id` is not the immediate parent identity. The resolver
checks child and parent metadata identities, rejects symlink escapes, and
opens only their bounded metadata records before checking both directories'
capture policy. It searches rollout filenames in `$CODEX_HOME/sessions` and
`$CODEX_HOME/archived_sessions` (default home: `~/.codex`); the existing capture
overrides `CODEX_SESSIONS_DIR` and `CODEX_ARCHIVED_SESSIONS_DIR` are supported.
Configured roots may be symlinked; symlinks below those roots are rejected.
After resolution, the diagnostic session ID identifies the immediate parent.
Lookup stops at 100,000 directory entries or 500 ms. Missing, ambiguous,
unsupported or malformed parent state produces no context and a content-free
diagnostic. Remote-only stores are unsupported. Python 3 is required for this
resolver. An upstream start-hook contract carrying the verified parent path
would supersede this local compatibility resolver; other Codex versions need
native qualification before their path semantics are assumed.

**One selection rule.** `scripts/memoire-subagent-select.jq` is the rule for
both shell injectors, each with its own extractor
(`memoire-subagent-extract-claude.jq`, `-codex.jq`) that maps its store onto
the same entry stream (prompt, assistant text, tool call with status and result
excerpt). The plugin implements the same rule in JavaScript, and
`scripts/tests/subagent-context-test.sh` builds one scenario for all three and
requires the same block structure, under the default bounds, under custom
`MEMOIRE_SUBAGENT_MAX_*` values and for the switch file's parsing.

- *Anchor.* The prompt that opened the parent's current turn is always
  included, up to 1500 characters. Its own backwards scan is bounded at 16 MiB
  and independent of the 512 KiB window the other entries are read from. Claude:
  the newest real prompt among the last 60 candidate lines of that scan (the
  scan is a `grep` prefilter, then one `jq` pass over those 60). Codex: the first client-authored user message after
  the newest `task_started` in the verified parent's rollout; the hook's
  `turn_id` belongs to the child. If none is found the block
  says `(no operator prompt in the recent window)`. ACP/T3 messages marked
  `client_authored: false` remain excluded as operator anchors; their parent
  activity can still be injected.
- *Fill.* Newest first inside the current turn; when budget remains, entries
  from before the anchor are added, and they are rendered chronologically above
  the anchor, separated from it by the line `--- current turn ---`.
- *Order.* Header, earlier-turn entries, boundary line, anchor, the current
  turn's entries oldest to newest, the omission line
  `(+N earlier entries omitted)` (`or more` when the read window cut the store),
  and last the pointer line.
- *Bounds.* `MEMOIRE_SUBAGENT_MAX_CHARS` (default 8000, block total, floor 600),
  `MEMOIRE_SUBAGENT_MAX_ENTRIES` (60), read from the environment of the hook
  process (the two shell injectors) and of the OpenCode process (the plugin),
  validated the same way (a positive integer, else the default). Per entry:
  text 1200, tool input 200, result excerpt 220 (head 140, tail 79). Every text
  is cut to a bounded prefix (prompt 3000, text 2400, input 400, result head 600
  plus tail 600 characters) *before* it is flattened, so the work per entry is
  bounded whatever the size of a tool result; flattening a whole 100 KB result
  first was quadratic. The block fits every accepted `MAX_CHARS` (600 and up):
  the anchor line is cut, with an ellipsis, to the room left after the header,
  the pointer, the separators and a reserve of 80 characters for the boundary
  and omission lines, never below 120 characters; when even that does not fit,
  the block is the header and the pointer only. That last form exceeds the bound
  only when those two lines alone do (a very long transcript path at the 600
  floor). A block over the harness's `additionalContext` cap is replaced by a
  preview and a file path and the sub-agent would see none of it. Whether the harness counts that cap in
  code points or UTF-16 units is not measured; the budget counts code points.
- *Fill window.* The last 512 KiB of the store. Claude writes each Read result
  twice on one line, so a few large results can fill it; while the window holds
  fewer than `MAX_ENTRIES` entries and the file has more, it widens to 2 MiB,
  then 4 MiB. When the window did not reach the start of the store the omission
  line says `or more`; when nothing was omitted but older content was not read
  it says `(earlier entries not read)`.
- *Neutralised content.* Inside an entry every run of whitespace, C0 and C1
  control characters (NUL, ESC, U+0085 included) and the Unicode line breaks
  U+2028 and U+2029 becomes one space, on the three sides. A tool result or a
  prompt cannot render a line that looks like the block's own header, boundary
  line, omission line or pointer.
- *Reading the store.* The stores are assumed to be compact JSON, one record
  per line (`"type":"user"` without a space after the colon), as Claude Code and
  Codex write them: the Claude anchor prefilter (`grep`) and the Codex anchor
  scan (`awk`) match that spelling, so a store written with spaces after the
  colons would lose its anchor. Lines reach `jq` as JSON strings (a byte-level
  `sed`, never `jq -R`, which turns a multibyte character that straddles a read
  buffer into U+FFFD) and are parsed one by one.
- *Bad lines.* A malformed line, or a lone-surrogate escape that `jq` refuses,
  is skipped and counted (`bad_lines=N` in the log); the rest of the store is
  injected. An unreadable transcript or a missing `jq` gives no output and one
  log line.
- *Never entries.* Claude: sidechain lines, `isMeta` lines, compaction
  summaries, interruption markers, tool-result and agent-message envelopes,
  `hook_additional_context` and other attachments, system reminders, the
  SessionStart Memoire block, the operator's `!` shell-input envelopes
  (`<bash-input>`, `<bash-stdout>`, `<bash-stderr>`). Codex: developer messages,
  injected instruction and environment-context user messages,
  `<user_shell_command>` envelopes, inter-agent envelopes, reasoning, inherited
  parent records of a forked thread. Codex result excerpts start at the command
  output: the fixed exec header (chunk id, wall time, exit code, token count) is
  dropped, the status is still read from it. opencode: every part with
  `synthetic: true`.
- *Pointer.* The last line of every block: `Parent session <id> (project
  <project>). This block is the parent's recent activity only. Older context:
  search Memoire (memoire_search) under project <project>, or read <transcript
  path or session reference>.`

**Catch-up poll.** The parent's transcript lands a moment after the hook
starts. The Claude injector waits (bound 1.5 s) for an `Agent`/`Task` tool call
with no result yet, or for the size to hold still for 500 ms. The Codex
injector waits (bound 1 s) for a `spawn_agent` call with no output yet, or 500
ms of stable size, and logs its elapsed time before emitting; its registered
timeout (4 s, declared in the script header) leaves at least 1.5 s over the
worst measured run: the tests measure a busy 15 MB turn with a growing rollout
and a quiet one, best and worst of three. The Claude hook is registered at 5 s.
Both hooks read their stdin before any gate, keep their working copies in a
private directory under `$MEMOIRE_HOME/tmp/subagent-context/` (mode 0700,
removed on every exit path; a run killed with SIGKILL leaves its directory and
the next run removes it), and stop the `jq`, `grep`, `sed` and `awk` children of the heavy steps (the
selection, the anchor scan, the window extraction and the catch-up poll checks)
when told to stop (TERM, INT, HUP). Four small steps are not interruptible and
run to their own bound, which is short on a regular file: the copy of the fill
window (`tail`, up to 4 MiB), the `jq` that counts the entries, the resolver
calls, and the `head | jq` that reads the first rollout line; a TERM is served
when they return.

**Provider boundary (opencode).** The block is sent to the child's model. When
the child runs on another provider than the parent's last assistant message, or
either is unknown, only the pointer line is sent, and the log says
`xprovider=withheld`, unless `MEMOIRE_SUBAGENT_CONTEXT_XPROVIDER=on` (also
opt-in, same file). Claude Code and Codex sub-agents stay on the parent's
provider; no check.

**opencode specifics.** The existing timeline part is unchanged; the parent
block is a second synthetic part. Every `chat.message` of a child session
injects, so a child resumed through `task_id` receives what the parent did since
the previous block, plus the anchor. The parent's messages are fetched with
`limit=201` (the page is 200; the extra message only tells whether older ones
exist, and is dropped, so `or more earlier entries omitted` is said only when the
page was cut). That the route returns the *newest* messages is an assumption: it
rests on a reading of the binary's strings and has not been verified live, and
neither has whether the server honours 201. OpenCode
writes one assistant message per model step, so a turn of more than about 200
steps pushes the user message that opened it out of the page; the plugin then
reads that message through the newest assistant message's `parentID`
(`session.message`) and uses it as the anchor. After a compaction in the middle
of the turn that message is a synthetic "continue" or a compaction message, or
it is gone: the plugin then makes one wider fetch (`limit=1000`, same deadline)
and anchors on the newest real operator prompt in it, which can be the prompt
that opened the turn before the compaction; the synthetic message is never the
anchor. With no real prompt, or a failing fetch, the block opens with the
no-prompt note. The child's provider is `input.model.providerID`, else the new
message's own `model.providerID`; unknown only when neither is there (then only
the pointer is sent). The timeline injection and the
parent-context build run concurrently under one deadline.

**Known limit: nested sub-agents.** A sub-agent that itself launches a
sub-agent gets the *root* session's block on Claude Code: the `SubagentStart`
payload of the grandchild carries the root's `session_id` and `transcript_path`,
so the block describes the root's activity, not the direct parent's, and the
pointer names the root as the parent session. Fixing it needs the hook input to
name the direct parent (its agent id, or the path of its own
`subagents/agent-*.jsonl` transcript); until then the block is right for the
first level only. Not fixed in increment 1.

**Logging.** The two shell injectors write one line per invocation to
`$MEMOIRE_HOME/hooks/subagent.log`; the OpenCode plugin writes a line only for a
child session that reaches a status (a non-child session, or a switch that is
off, logs nothing). Fields: status, ids, sizes, poll outcome, skipped bad
lines, elapsed time. Never content. The `session`, `agent` and `type` values
that come from the harness are stripped of control characters and bounded to
128 characters. There is no
pre-injection redaction here: the parent's content is already in its own store,
the block is what the sub-agent's model receives, and what lands in the child's
transcript passes the queue-write redaction gate when the readers capture it.

## Base-URL resolution (one knob)

**`MEMOIRE_BASE_URL` is the ratified single knob of the read path** (D165,
`R10-DESIGN.md` §3.2), shipped across the inject hooks, the MCP service, and the
notify tooling. Resolution there is `MEMOIRE_BASE_URL`, then
`http://$MEMOIRE_ADDR`, then `http://127.0.0.1:37778`.

The knob is not yet universal, and this contract does not pretend it is. The
sweep-derived inventory of every consumer that does not yet honor the knob
lives once, on `gap:base-url-unification` in `ROADMAP.md`, and is deliberately
not duplicated here — a consumer outside the rule is a gap to close, never a
second convention.

**There is no config-file fallback, deliberately.** Env or default; a third
source would let a consumer be pointed somewhere the operator cannot see from
the environment. Local installs keep the loopback default and set nothing; a
hub client sets exactly two things, `MEMOIRE_FORWARD_URL` for capture and
`MEMOIRE_BASE_URL` for reads. Read tokens resolve `MEMOIRE_TOKEN_FILE`, then
`$MEMOIRE_HOME/token`, then `~/.memoire/token` — never the forwarding token,
which is separate (D153).

## Foreign-client read parity

**Read parity is a hard requirement, not a feature.** Every read endpoint is
reachable with `curl -H "Authorization: Bearer …"` — no Anthropic-only headers,
no SDK gating, no client-class checks anywhere in the auth path.

The `read` scope covers the GET read surface plus `POST /v1/search` and
`POST /v1/context` — POSTs only because their bodies outgrow a query string. It
does not cover admin paths that happen to be GETs (`/v1/derivation-runs`,
`/v1/derive/outstanding`, the observation review-log, the `/v1/embeddings`
worker seam of D218, and the `/v1/triage/` reads of the triage lane, which
carry cross-project observation text alongside a judge's reasoning about
it), which leak full text or accept poisonable input. A `?token=` query
fallback exists **for read scope only**. The attribution-at-Stop read surface
is read scope: the `usage` aggregate the observation list and get routes
attach (counts by verdict, sessions, last use; absent when no `memory_usage`
event names the row) and `GET /v1/observations/{id}/usage`, the per-turn
reason lines, which are the consuming agent's own prose about hits it was
served in the operator's session, the same exposure as the observation text.

Harness shape is adapted at the edge, never in the payload:
`scripts/memoire-inject-envelope.sh` wraps the inject script's stdout in the
Claude SessionStart JSON shape — qwen-code was measured to inject
`hookSpecificOutput.additionalContext` and drop plain stdout. With injection off
the inject script prints nothing and the wrapper prints nothing either, which is
how qwen-code is told there is no additional context. opencode shells
into the same script from its plugin; codex reuses it over its own hooks file.
Pi shells into it from `scripts/pi-extension/memoire.js` at the first prompt of
a session and carries the output as the `memoire` section of the system prompt
for every run of that session; the same extension captures the session live
(`pi_entry`, `pi_assistant_turn`) on `agent_settled`.
Sub-agent parent context is a separate, opt-in surface (section above).

**Parity of results is exact, not approximate.** The injection and timeline
parity law admits no ranking tolerance — identical row sets across engines and
paths, any diff a FAIL (`SLICE5-GROUNDWORK-DESIGN.md`); search parity carries an
overlap threshold only because ranking is involved there.

Honesty note: `tests/foreign/codex-read-smoke.md` and `qwen-read-smoke.md` are
runbooks a session executes by hand, not CI-executed scripts. Whether the claim
earns a mechanical nightly smoke is an open question of record in
`KICKOFF-PREP.md`; until it is answered, this guarantee rests on manual runs.

## What this contract does not own

- Elevation and `can_instruct` semantics — `ELEVATION-DESIGN.md`; this document
  states only how the elevated set is read and emitted.
- Governance flags (`can_evidence`, review states, noise) — `GOVERNANCE-DESIGN.md`.
- Hybrid-search operation — RRF fusion, degrade-to-FTS posture, the
  `MEMOIRE_HYBRID_SEARCH` default, the per-request `hybrid=` override —
  `RUNBOOK.md` and D218.
- The Liaison evidence window `GET /v1/events/tool-window` — the
  `serve-the-seams` gesture (D116, D117), which keeps it raw-events-only.
- The full env-var knob table — `RUNBOOK.md` and the root `QUICKSTART.md`.
