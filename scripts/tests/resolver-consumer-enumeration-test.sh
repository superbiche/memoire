#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# resolver-consumer-enumeration-test.sh — memoire:D136 slice A1 (liaison:D239).
#
# ENUMERATES every consumer of project resolution BY NAME — never by category —
# and proves each routes through scripts/memoire-resolve-project.sh. The
# derive-auto resolver hole existed precisely because that script fit no
# category ("capture adapter", "injector"); this test fails whenever a script
# starts attributing projects without enrolling here, forcing the author to
# wire the shared resolver or record a justified exemption.
#
# Scope: top-level scripts/*.sh. The opencode plugin (scripts/opencode-plugin,
# JS) attributes through memoire-capture-opencode.sh, which is enumerated.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT/scripts"

fail() { echo "FAIL: $1" >&2; exit 1; }

# Consumers that RESOLVE a project key from a directory. Each MUST reference
# the shared resolver (fail-open wiring is their own concern; the reference is
# the invariant).
RESOLVER_WIRED=(
  memoire-attribute-claude.sh
  memoire-capture-acpx.sh
  memoire-capture-muse.sh
  memoire-capture-claude.sh
  memoire-capture-claude-transcripts.sh
  memoire-capture-codex-rollout.sh
  memoire-capture-opencode.sh
  memoire-capture-qwen-rollout.sh
  memoire-capture-text.sh
  memoire-derive-auto.sh
  memoire-inject-claude.sh
  memoire-subagent-lib.sh      # sub-agent parent context: gate + project for the pointer, shared by the two injectors
)

# Thin wrappers that DELEGATE resolution by exec'ing a wired consumer. They
# must not pre-set MEMOIRE_PROJECT: that env value is a hard override and
# would defeat .memoire/registry precedence for every capture they relay.
DELEGATING_WRAPPERS=(
  memoire-capture-codex.sh
  memoire-capture-qwen.sh
)

# Injector wrappers that DELEGATE by invoking a wired injector script
# (round-1 review finding 5: the by-name enumeration must carry these too).
DELEGATING_INJECT_WRAPPERS=(
  memoire-inject-envelope.sh
)

# Scripts that touch project attribution surfaces but legitimately do NOT
# resolve a key from a directory. Justifications are load-bearing — re-verify
# before moving a script here instead of wiring it.
EXEMPT=(
  memoire-resolve-project.sh   # the resolver itself
  memoire-project-move.sh      # operator re-key tool: keys are explicit args
  memoire-derive-sweep.sh      # replays project values read FROM event rows
  memoire-triage-sweep.sh      # judges rows already attributed in the store; MEMOIRE_TRIAGE_PROJECT is an explicit operator narrowing, never a resolved key, and unset means the whole store
  seed.sh                      # fixed literal '-project memoire' seeding
  smoke.sh                     # fixed literal test project, scratch home
  memoire-relabel-observed.sh  # provenance re-label on already-attributed rows; basename is argv[0] help text
  memoire-rekey-derivation-runs.sh  # session-join fold of already-attributed run rows; basename is argv[0] help text
  memoire-bootstrap.sh         # prints hook-wiring snippets naming capture scripts; never attributes a project itself
)

for f in "${RESOLVER_WIRED[@]}"; do
  [[ -f "$f" ]] || fail "enumerated consumer missing: $f (update the enumeration)"
  grep -q "memoire-resolve-project.sh" "$f" \
    || fail "$f resolves a project but never references memoire-resolve-project.sh"
done

for f in "${DELEGATING_WRAPPERS[@]}"; do
  [[ -f "$f" ]] || fail "enumerated wrapper missing: $f (update the enumeration)"
  grep -Eq 'exec .*memoire-capture-text\.sh' "$f" \
    || fail "$f must delegate by exec'ing memoire-capture-text.sh"
  if grep -Eq '(^|[^#[:alnum:]_])(export[[:space:]]+)?MEMOIRE_PROJECT=' "$f"; then
    fail "$f must not pre-set MEMOIRE_PROJECT (hard override defeats resolution)"
  fi
done

for f in "${DELEGATING_INJECT_WRAPPERS[@]}"; do
  [[ -f "$f" ]] || fail "enumerated inject wrapper missing: $f (update the enumeration)"
  grep -q "memoire-inject-claude.sh" "$f" \
    || fail "$f must delegate to the wired injector memoire-inject-claude.sh"
  if grep -Eq '(^|[^#[:alnum:]_])(export[[:space:]]+)?MEMOIRE_PROJECT=' "$f"; then
    fail "$f must not pre-set MEMOIRE_PROJECT (hard override defeats resolution)"
  fi
done

# Discovery sweep: ANY top-level script matching a project-attribution smell
# must be enumerated above. This is the future-consumer tripwire. Referencing
# a wired consumer script is itself a smell: it catches delegation wrappers
# that carry no attribution keyword of their own (finding 5).
candidates="$(grep -El -- '-project([[:space:]]|\b)|MEMOIRE_PROJECT|"project"|basename|memoire-capture-text\.sh|memoire-inject-claude\.sh' ./*.sh | sed 's|^\./||' | sort || true)"
while IFS= read -r f; do
  [[ -n "$f" ]] || continue
  known=no
  for k in "${RESOLVER_WIRED[@]}" "${DELEGATING_WRAPPERS[@]}" "${DELEGATING_INJECT_WRAPPERS[@]}" "${EXEMPT[@]}"; do
    if [[ "$f" == "$k" ]]; then known=yes; break; fi
  done
  [[ "$known" == yes ]] || fail "unenumerated project-attribution consumer: $f — wire it through memoire-resolve-project.sh (or record a justified exemption) and enroll it in this test"
done <<<"$candidates"

# Enumeration hygiene: enumerated files must exist so the list cannot rot.
for f in "${EXEMPT[@]}"; do
  [[ -f "$f" ]] || fail "exempt entry missing: $f (update the enumeration)"
done

echo "resolver-consumer-enumeration-test: OK"
