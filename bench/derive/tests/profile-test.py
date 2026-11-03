#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Python side of the derive-profile formula parity gate.

`derive-profile-golden.json` is asserted by BOTH this file and
services/api/internal/config/derive_profile_golden_test.go. The bench resolves
`--max-prompt-bytes` from an endpoint profile; memoire resolves the same
ceiling from the same registry entry — a divergence would bench a window
production never sends, so it goes loud in `make test` and in the bench suite,
whichever side moved.

Run standalone or via tests/derive-bench-test.sh:
  python3 bench/derive/tests/profile-test.py
"""

import json
import os
import sys

TESTS_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(TESTS_DIR))

import benchlib  # noqa: E402

failures = []


def check(condition, message):
    if not condition:
        failures.append(message)


def main():
    golden = json.load(open(os.path.join(TESTS_DIR, "derive-profile-golden.json"),
                            encoding="utf-8"))
    cases = golden.get("cases") or []
    check(bool(cases), "golden vectors carry no cases")

    for case in cases:
        name, profile = case["name"], case["profile"]
        try:
            benchlib.validate_profile(name, profile)
        except benchlib.BenchError as err:
            failures.append(f"{name}: golden profile must be valid geometry: {err}")
            continue
        got = benchlib.effective_max_prompt_bytes(profile)
        # Exact, not approximate: floor() has no tolerance, and the
        # rounding-boundary case is only meaningful compared exactly.
        check(got == case["effective_max_prompt_bytes"],
              f"{name}: effective_max_prompt_bytes = {got}, golden says "
              f"{case['effective_max_prompt_bytes']}")

    # The validator must refuse what the Go loader refuses, or the bench would
    # run a geometry the binary rejects.
    rejects = [
        ("no-room", {"ctx_tokens": 1000, "max_completion_tokens": 900,
                     "bytes_per_token": 2.2, "margin_tokens": 100}),
        ("bad-policy", {"ctx_tokens": 32768, "max_completion_tokens": 8192,
                        "bytes_per_token": 2.2, "margin_tokens": 512,
                        "thinking": {"policy": "maybe"}}),
        ("budget-without-tokens", {"ctx_tokens": 32768, "max_completion_tokens": 8192,
                                   "bytes_per_token": 2.2, "margin_tokens": 512,
                                   "thinking": {"policy": "budget"}}),
        ("tokens-without-budget", {"ctx_tokens": 32768, "max_completion_tokens": 8192,
                                   "bytes_per_token": 2.2, "margin_tokens": 512,
                                   "thinking": {"policy": "off", "budget_tokens": 4096}}),
        ("zero-bytes-per-token", {"ctx_tokens": 32768, "max_completion_tokens": 8192,
                                  "bytes_per_token": 0, "margin_tokens": 512}),
    ]
    for name, profile in rejects:
        try:
            benchlib.validate_profile(name, profile)
        except benchlib.BenchError:
            continue
        failures.append(f"{name}: validate_profile accepted an invalid profile")

    if failures:
        for message in failures:
            print(f"FAIL: {message}", file=sys.stderr)
        return 1
    print(f"derive-profile parity: OK ({len(cases)} vectors, "
          f"{len(rejects)} rejections)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
