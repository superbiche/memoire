#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# prepush-leak-test.sh — the L0011 pre-push leak gate (.githooks/pre-push).
# The hook scans EVERY outgoing commit (file contents, file names and the
# commit message) with scripts/leak/leak-scan.sh: the generic shapes in
# scripts/leak/leak-patterns-generic.txt, the exemptions in
# scripts/leak/leak-exemptions.txt, plus the sensitive set at
# $HOME/.memoire/export/leak-patterns.txt when that file exists (maintainers).
# Without it the hook scans generic shapes only and says so.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fail() { echo "FAIL: $1" >&2; exit 1; }
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

export HOME="$WORK/home"
mkdir -p "$HOME"
export GIT_CONFIG_GLOBAL="$WORK/gitconfig" GIT_CONFIG_NOSYSTEM=1
git config --global user.name tester
git config --global user.email tester@example.invalid
git config --global init.defaultBranch main

# A minimal repository that carries the hook and the scanner, as a clone would.
mk_repo() { # <dir>
  local d="$1"
  git init -q "$d"
  mkdir -p "$d/.githooks" "$d/scripts/leak"
  cp "$ROOT/.githooks/pre-push" "$d/.githooks/pre-push"
  cp "$ROOT/scripts/leak/leak-scan.sh" "$ROOT/scripts/leak/leak-patterns-generic.txt" "$d/scripts/leak/"
  : > "$d/scripts/leak/leak-exemptions.txt"
  echo "hello" > "$d/README.md"
  git -C "$d" add -A
  git -C "$d" commit -qm "initial"
  git -C "$d" config core.hooksPath .githooks
  git init -q --bare "$d.remote.git"
  git -C "$d" remote add origin "$d.remote.git"
  git -C "$d" push -q origin main 2>"$WORK/initial-push.err" || { cat "$WORK/initial-push.err" >&2; fail "a clean initial push must pass"; }
}

push_ok() { git -C "$1" push -q origin HEAD:main 2>"$WORK/push.err"; }

# 1. A clean commit pushes.
R="$WORK/r1"; mk_repo "$R"
echo "more" >> "$R/README.md"; git -C "$R" commit -qam "clean change"
push_ok "$R" || { cat "$WORK/push.err" >&2; fail "a clean commit must push"; }

# 2. A generic-shape hit in a file is refused, and nothing reaches the remote.
R="$WORK/r2"; mk_repo "$R"
echo "server at 192.168.7.7" > "$R/notes.md"; git -C "$R" add notes.md; git -C "$R" commit -qm "add notes"
before="$(git -C "$R.remote.git" rev-parse main)"
push_ok "$R" && fail "a commit carrying a LAN address must be refused"
[[ "$(git -C "$R.remote.git" rev-parse main)" == "$before" ]] || fail "the refused push moved the remote"

# 3. An intermediate commit is scanned, not only the tip.
R="$WORK/r3"; mk_repo "$R"
echo "path /home/michel/x" > "$R/a.md"; git -C "$R" add a.md; git -C "$R" commit -qm "leak"
git -C "$R" rm -q a.md; git -C "$R" commit -qm "remove it again"
push_ok "$R" && fail "a leak in an intermediate commit must be refused even when the tip is clean"

# 4. The commit message is scanned.
R="$WORK/r4"; mk_repo "$R"
echo "x" >> "$R/README.md"; git -C "$R" commit -qam "copied from /home/michel/dev"
push_ok "$R" && fail "a leak in a commit message must be refused"

# 5. File names are scanned.
R="$WORK/r5"; mk_repo "$R"
echo "x" > "$R/host.lan.md"; git -C "$R" add host.lan.md; git -C "$R" commit -qm "add file"
push_ok "$R" && fail "a leak in a file name must be refused"

# 6. The sensitive set applies when present, and only then.
R="$WORK/r6"; mk_repo "$R"
echo "for zorblaxclient only" > "$R/b.md"; git -C "$R" add b.md; git -C "$R" commit -qm "add b"
push_ok "$R" || { cat "$WORK/push.err" >&2; fail "without a sensitive set, a non-generic word must push"; }
grep -qi 'generic' "$WORK/push.err" || fail "without a sensitive set the hook must say it scanned generic shapes only"
mkdir -p "$HOME/.memoire/export"; echo "zorblaxclient" > "$HOME/.memoire/export/leak-patterns.txt"
echo "again zorblaxclient" >> "$R/b.md"; git -C "$R" commit -qam "touch b"
push_ok "$R" && fail "with a sensitive set present, its patterns must refuse the push"
rm -f "$HOME/.memoire/export/leak-patterns.txt"

# 7. Exemptions from the repository's own list apply.
R="$WORK/r7"; mk_repo "$R"
printf 'see 192.168.0.1 docs\n' > "$R/c.md"
printf 'line\tc.md\tsee 192.168.0.1 docs\n' > "$R/scripts/leak/leak-exemptions.txt"
git -C "$R" add -A; git -C "$R" commit -qm "documented example"
push_ok "$R" || { cat "$WORK/push.err" >&2; fail "an exempted line must push"; }

# 8. Deleting a remote branch is not scanned and passes.
R="$WORK/r8"; mk_repo "$R"
git -C "$R" push -q origin HEAD:refs/heads/tmp 2>/dev/null || fail "setup push of a second branch"
git -C "$R" push -q origin --delete tmp 2>"$WORK/push.err" || { cat "$WORK/push.err" >&2; fail "a branch deletion must pass"; }

# 9. A push to a private maintainer repository is not scanned; a look-alike is.
R="$WORK/r9"; mk_repo "$R"
mkdir -p "$WORK/srv/superbiche"
git init -q --bare "$WORK/srv/superbiche/memoire-ledger.git"
git -C "$R" remote add ledger "$WORK/srv/superbiche/memoire-ledger.git"
echo "private 192.168"".9.9" > "$R/p.md"; git -C "$R" add p.md; git -C "$R" commit -qm "private note"
git -C "$R" push -q ledger HEAD:main 2>"$WORK/push.err" || { cat "$WORK/push.err" >&2; fail "a push to the private ledger must not be scanned"; }
git init -q --bare "$WORK/srv/superbiche/memoire-ledger-copy.git"
git -C "$R" remote add copy "$WORK/srv/superbiche/memoire-ledger-copy.git"
git -C "$R" push -q copy HEAD:main 2>/dev/null && fail "a look-alike destination must be scanned"

# 10. export-ignore cannot hide a tracked file from the scan (review r1 F1).
R="$WORK/r10"; mk_repo "$R"
echo "server at 192.168"".3.3" > "$R/hidden.md"
printf 'hidden.md export-ignore\n' > "$R/.gitattributes"
git -C "$R" add -A; git -C "$R" commit -qm "add hidden"
push_ok "$R" && fail "an export-ignored tracked file must still be scanned"

# 11. A git read failure refuses the push instead of passing it (review r1 F2).
R="$WORK/r11"; mk_repo "$R"
echo "more" >> "$R/README.md"; git -C "$R" commit -qam "clean change"
(
  git() { if [[ "${1:-}" == rev-list ]]; then echo "synthetic git read failure" >&2; return 128; fi; command git "$@"; }
  export -f git
  command git -C "$R" push -q origin HEAD:main 2>"$WORK/push.err"
) && fail "a failing git rev-list inside the hook must refuse the push"

echo "prepush-leak-test: OK"
