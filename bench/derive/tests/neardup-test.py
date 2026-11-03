#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Python side of the near-dup golden-vector parity gate.

`neardup-golden.json` is asserted by BOTH this file and
services/api/internal/modelderive/neardup_golden_test.go. Divergence between
the production gate and the bench's port therefore goes loud in `make test`,
whichever side moved.

Run standalone or via tests/derive-bench-test.sh:
  python3 bench/derive/tests/neardup-test.py
"""

import json
import os
import sys

TESTS_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(TESTS_DIR))

import benchlib  # noqa: E402

# Scores are float arithmetic on both sides; compare within tolerance rather
# than by equality, and keep the tolerance far tighter than any threshold gap.
TOLERANCE = 1e-9

failures = []


def check(condition, message):
    if not condition:
        failures.append(message)


def main():
    golden = json.load(open(os.path.join(TESTS_DIR, "neardup-golden.json"),
                            encoding="utf-8"))
    check(golden["thresholds"]["lo"] == benchlib.NEARDUP_LO_THRESHOLD,
          f"golden lo threshold {golden['thresholds']['lo']} != "
          f"benchlib {benchlib.NEARDUP_LO_THRESHOLD}")
    check(golden["thresholds"]["hi"] == benchlib.NEARDUP_HI_THRESHOLD,
          f"golden hi threshold {golden['thresholds']['hi']} != "
          f"benchlib {benchlib.NEARDUP_HI_THRESHOLD}")

    for case in golden["normalize"]:
        got = benchlib.normalize_for_neardup(case["in"])
        check(got == case["normalized"],
              f"normalize[{case['name']}]: {got!r} != {case['normalized']!r}")
        tokens = sorted(benchlib.digit_tokens(got))
        check(tokens == case["digit_tokens"],
              f"digit_tokens[{case['name']}]: {tokens} != {case['digit_tokens']}")

    for case in golden["pairs"]:
        score, hit = benchlib.neardup_text_hit(case["a"], case["b"])
        check(abs(score - case["score"]) <= TOLERANCE,
              f"pair[{case['name']}]: score {score!r} != {case['score']!r}")
        check(hit == case["hit"],
              f"pair[{case['name']}]: hit {hit} != {case['hit']}")

    # The under-three-runes branch: the whole normalized string is the single
    # trigram, and a string that normalizes to nothing has an EMPTY set — not a
    # set holding "". The distinction decides whether two blank-normalizing
    # texts score 1.0 (both empty) or 0.0, so it is pinned here.
    check(benchlib.trigram_set("ok") == {"ok"}, "trigram_set('ok') lost the short-string branch")
    check(benchlib.trigram_set("") == set(), "trigram_set('') must be empty")
    check(benchlib.jaccard(set(), set()) == 1.0, "jaccard of two empty sets is 1")

    # Alignment: EQUIVALENT pairs are matched 1:1, so a single strong
    # observation cannot absorb two on the other side.
    equivalent, disputed, left_only, right_only = benchlib.align_sets(
        ["the deploy gate requires a clean worktree before running images-push"],
        ["the deploy gate requires a clean worktree before images-push",
         "the deploy gate requires a clean worktree prior to images-push"],
    )
    check(len(equivalent) == 1, f"one incumbent row can only pair once: {equivalent}")
    check(left_only == [], f"the paired incumbent row is not one-sided: {left_only}")
    check(len(disputed) + len(right_only) == 1,
          f"the unpaired candidate row must surface exactly once: {disputed} {right_only}")

    if failures:
        for message in failures:
            print("FAIL: " + message, file=sys.stderr)
        return 1
    print("neardup parity: OK "
          f"({len(golden['normalize'])} normalize, {len(golden['pairs'])} pairs)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
