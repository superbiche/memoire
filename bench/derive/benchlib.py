#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Shared helpers for the derive bench (docs/DERIVE-BENCH-DESIGN.md).

Stdlib only, by house rule: the bench ships with the repo and must run on a
bare checkout with no dependency install. HTTP is urllib, JSON is json, and
the bearer token is read from a file and only ever placed in an Authorization
header — never printed, never passed in argv, never written to a fixture.
"""

import json
import math
import os
import re
import time
import unicodedata
import urllib.error
import urllib.parse
import urllib.request

FIXTURE_VERSION = 1
INCUMBENT_VERSION = 1
PACK_VERSION = 1
BALLOT_VERSION = 1

# Providers whose derivation runs are MODEL derivations, mirroring
# `isModelProvider` in services/api/cmd/derive/main.go (memoire:D65, extended
# by memoire:D233). A run outside this set — `rules`, `manual` — produced its
# observations on a path the bench is not comparing, so its rows are recorded
# under `unattributed` rather than counted as the incumbent's answer.
MODEL_PROVIDERS = ("local-qwen", "openai", "byok")

# Every review state the store admits (db.allowedReviewState). The incumbent
# baseline is defined by PROVENANCE, not by review state, so it is fetched
# state by state: `GET /v1/observations` with no `review_state` returns
# ACCEPTED ONLY, and `include_proposed=true` widens that to
# accepted/proposed/lapsed and no further (db.observationWhere) — rejected,
# superseded, disputed and soft_deleted are reachable only by naming them.
REVIEW_STATES = ("proposed", "accepted", "lapsed", "rejected", "superseded",
                 "disputed", "soft_deleted")

# The derivation input set, mirroring
# services/api/internal/modelderive/builder.go `canonicalInputEventTypes`.
# The bench cannot call that Go function, so the list is duplicated here and
# tests/derive-bench-test.sh asserts the two are identical: a type added to
# the derive path fails the suite instead of silently freezing a window that
# is missing a whole class of turns.
CANONICAL_INPUT_EVENT_TYPES = [
    "claude_PostToolUse",
    "claude_UserPromptSubmit",
    "claude_assistant_turn",
    "codex_assistant_turn",
    "opencode_assistant_turn",
    "pi_assistant_turn",
    "qwen_assistant_turn",
]

# db.normalizeLimit caps the tool-window query at 500 and the handler emits no
# cursor, so the freezer pages by epoch instead (see Client.tool_window_ids).
TOOL_WINDOW_PAGE = 500

# The serving-side thinking policies a derive profile may declare, mirroring
# config.validateDeriveProfile (docs/DERIVE-PROFILE-DESIGN.md §4).
THINKING_POLICIES = ("on", "off", "budget")


class BenchError(Exception):
    """An operator-facing bench failure: bad config, bad store, bad fixture."""


def effective_max_prompt_bytes(profile):
    """The profile's byte ceiling: floor((ctx - completion - margin) * bpt).

    The Python half of a parity gate: config.DeriveProfile.EffectiveMaxPromptBytes
    must return the identical integer for the same inputs, asserted over shared
    vectors by tests/profile-test.py and the Go test beside it. Keep the
    expression shape — subtract first, one multiply, floor once — or the two
    sides can disagree on a rounding boundary while both look correct.
    """
    room = (int(profile["ctx_tokens"])
            - int(profile["max_completion_tokens"])
            - int(profile.get("margin_tokens", 0)))
    if room <= 0:
        return 0
    return int(math.floor(room * float(profile["bytes_per_token"])))


def validate_profile(endpoint_id, profile):
    """Reject an incoherent profile as loudly as the Go loader does.

    The bench reads the same registry file as memoire, so a profile memoire
    would refuse must not silently bench a different geometry here.
    """
    def need(field, ok, want):
        if not ok:
            raise BenchError(
                f"endpoint {endpoint_id!r} profile: {field} {want}"
            )

    if not isinstance(profile, dict):
        raise BenchError(f"endpoint {endpoint_id!r} profile must be an object")
    for field in ("ctx_tokens", "max_completion_tokens", "bytes_per_token"):
        need(field, field in profile, "is required")
    ctx = int(profile["ctx_tokens"])
    completion = int(profile["max_completion_tokens"])
    margin = int(profile.get("margin_tokens", 0))
    bpt = float(profile["bytes_per_token"])
    need("ctx_tokens", ctx > 0, "must be > 0")
    need("max_completion_tokens", completion > 0, "must be > 0")
    need("margin_tokens", margin >= 0, "must be >= 0")
    need("bytes_per_token", bpt > 0, "must be > 0")
    need("max_completion_tokens + margin_tokens", completion + margin < ctx,
         f"must be < ctx_tokens ({ctx}) — the prompt needs room too")

    thinking = profile.get("thinking")
    if thinking is None:
        return profile
    policy = thinking.get("policy")
    budget = int(thinking.get("budget_tokens", 0))
    need("thinking.policy", policy in THINKING_POLICIES,
         f"{policy!r} is not one of {'|'.join(THINKING_POLICIES)}")
    if policy == "budget":
        need("thinking.budget_tokens", budget > 0,
             'is required (> 0) with policy "budget"')
    else:
        need("thinking.budget_tokens", budget == 0,
             f'is only valid with policy "budget", got policy {policy!r}')
    return profile


def default_base_url():
    """The ratified single knob for every API consumer (scripts/check-health.sh)."""
    base = os.environ.get("MEMOIRE_BASE_URL")
    if base:
        return base.rstrip("/")
    return "http://" + os.environ.get("MEMOIRE_ADDR", "127.0.0.1:37778")


def default_token_file():
    home = os.environ.get("MEMOIRE_HOME") or os.path.join(
        os.path.expanduser("~"), ".memoire"
    )
    return os.environ.get("MEMOIRE_TOKEN_FILE") or os.path.join(home, "token")


def read_token(path):
    try:
        with open(path, "r", encoding="utf-8") as fh:
            token = fh.read().strip()
    except OSError as err:
        raise BenchError(f"cannot read the bearer token file {path}: {err.strerror}")
    if not token:
        raise BenchError(f"bearer token file {path} is empty")
    return token


class Client:
    """A minimal read/write client for one memoire API.

    Every call goes through the versioned HTTP surface — the bench never opens
    the store directly, so what it exercises is what a user's client sees.
    """

    def __init__(self, base_url, token, timeout=60.0):
        self.base_url = base_url.rstrip("/")
        self._token = token
        self.timeout = timeout

    def _request(self, method, path, params=None, body=None):
        url = self.base_url + path
        if params:
            url += "?" + urllib.parse.urlencode(params)
        data = None
        req = urllib.request.Request(url, method=method)
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            req.add_header("Content-Type", "application/json")
        req.add_header("Authorization", "Bearer " + self._token)
        try:
            with urllib.request.urlopen(req, data=data, timeout=self.timeout) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as err:
            detail = err.read().decode("utf-8", "replace")[:400]
            raise BenchError(f"{method} {path} -> HTTP {err.code}: {detail}")
        except urllib.error.URLError as err:
            raise BenchError(f"{method} {path} -> {err.reason}")
        if not raw:
            return {}
        return json.loads(raw)

    def get(self, path, params=None):
        return self._request("GET", path, params=params)

    def post(self, path, body):
        return self._request("POST", path, body=body)

    def health(self):
        return self.get("/v1/health")

    def stats(self):
        return self.get("/v1/stats")

    def session(self, session_id):
        return self.get("/v1/sessions/" + urllib.parse.quote(session_id, safe="")).get(
            "session", {}
        )

    def event(self, event_id):
        return self.get(f"/v1/events/{int(event_id)}").get("event", {})

    def tool_window_ids(self, session_id, event_type, project=""):
        """Every event id of one type in a session, oldest first.

        `GET /v1/events/tool-window` is the only session-scoped event query on
        the read surface. It caps at TOOL_WINDOW_PAGE rows and returns no
        cursor, so a long session is paged by re-anchoring `start` on the last
        epoch seen (results are ordered occurred_at_epoch ASC, id ASC and the
        bounds are inclusive) and de-duplicated by id. A full page that adds no
        new id means more than a page of events share one epoch second — the
        window is unpageable through this surface, and the freezer refuses
        rather than writing a truncated fixture.
        """
        seen = {}
        order = []
        start = 0
        while True:
            params = {
                "session_id": session_id,
                "event_type": event_type,
                "limit": TOOL_WINDOW_PAGE,
            }
            if project:
                params["project"] = project
            if start:
                params["start"] = start
            page = self.get("/v1/events/tool-window", params).get("events") or []
            added = 0
            for rec in page:
                eid = int(rec["id"])
                if eid in seen:
                    continue
                seen[eid] = int(rec["occurred_at_epoch"])
                order.append(eid)
                added += 1
            if len(page) < TOOL_WINDOW_PAGE:
                break
            last_epoch = int(page[-1]["occurred_at_epoch"])
            if added == 0 or last_epoch == start:
                # Declared boundary: the read surface caps pages at
                # TOOL_WINDOW_PAGE rows with no cursor, so exactly-500 and
                # 501+ events at one epoch are indistinguishable here. Both
                # refuse loudly rather than risk a silently truncated fixture.
                raise BenchError(
                    f"session {session_id}: {TOOL_WINDOW_PAGE} or more "
                    f"{event_type} events share epoch {last_epoch}, which the "
                    "capped tool-window surface cannot page past — this "
                    "session is not freezable (declared bench limitation); "
                    "pick another session"
                )
            start = last_epoch
        return order

    def observations(self, project, session_id, review_state="",
                     derivation_run_id="", limit=500, include_noise=True):
        """All matching observations, following the list cursor to exhaustion.

        `include_noise` defaults ON, against the read surface's own default:
        `db.observationWhere` drops `noise_candidate = 1` rows unless the
        caller opts in, and it does so on explicit-review_state reads too. The
        model sets that flag itself (prompt v2.3 noise genres, memoire:D211),
        so leaving it off would silently delete a side's noisiest proposals —
        exactly what the pack's noise axis exists to weigh.

        `include_derived` is always sent ON: both consumers (the incumbent
        read and the run driver) ask for the derived output of a derivation
        run, and L0010's raw-only read default hides it without the opt-in.
        """
        out = []
        cursor = ""
        while True:
            params = {"project": project, "session_id": session_id, "limit": limit,
                      "include_derived": "true"}
            if review_state:
                params["review_state"] = review_state
            if derivation_run_id:
                params["derivation_run_id"] = derivation_run_id
            if review_state in ("", "proposed"):
                params["include_proposed"] = "true"
            if include_noise:
                params["include_noise"] = "true"
            if cursor:
                params["cursor"] = cursor
            body = self.get("/v1/observations", params)
            out.extend(body.get("observations") or [])
            cursor = body.get("next_cursor") or ""
            if not cursor:
                break
        return out

    def derivation_run(self, run_id):
        return self.get(
            "/v1/derivation-runs/" + urllib.parse.quote(run_id, safe="")
        ).get("derivation_run", {})


def safe_name(value):
    """A filesystem-safe stem for a session id or candidate label."""
    cleaned = re.sub(r"[^A-Za-z0-9._-]+", "-", value.strip()).strip("-.")
    return cleaned or "unnamed"


def write_json(path, payload):
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(payload, fh, indent=2, sort_keys=True)
        fh.write("\n")
    os.replace(tmp, path)


def read_json(path):
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)


def fixture_path(data_dir, set_name, session_id):
    return os.path.join(data_dir, safe_name(set_name), safe_name(session_id) + ".json")


def incumbent_path(data_dir, set_name, session_id):
    """The incumbent baseline sits beside the fixtures, under the same custody.

    It holds raw observation text pulled from the user's own store, so it lives
    in the gitignored data dir with the transcripts — never under tmp/ results,
    never in the repo.
    """
    return os.path.join(data_dir, safe_name(set_name), "incumbent",
                        safe_name(session_id) + ".json")


def load_fixture(path):
    try:
        fixture = read_json(path)
    except OSError as err:
        raise BenchError(f"cannot read fixture {path}: {err.strerror}")
    except ValueError as err:
        raise BenchError(f"fixture {path} is not valid JSON: {err}")
    version = fixture.get("fixture_version")
    if version != FIXTURE_VERSION:
        raise BenchError(
            f"fixture {path} is version {version!r}, this bench speaks "
            f"{FIXTURE_VERSION} — re-freeze the eval set"
        )
    for key in ("project", "session_id", "events"):
        if not fixture.get(key):
            raise BenchError(f"fixture {path} is missing {key!r}")
    return fixture


def replay_envelope(event):
    """The ingest envelope that reproduces one frozen event.

    Everything `derive.CanonicalInputHash` and the prompt builder read is
    carried across: project, session, adapter, source event id, type, cwd,
    payload, text, and the ORIGINAL occurred_at_epoch (the queue writer only
    defaults the epoch when the envelope omits it).
    """
    envelope = {
        "schema_version": 1,
        "project": event["project"],
        "session_id": event["session_id"],
        "source_adapter": event.get("source_adapter") or "bench-replay",
        "event_type": event["event_type"],
        "payload": event.get("payload") if event.get("payload") is not None else {},
        "occurred_at_epoch": int(event["occurred_at_epoch"]),
    }
    for key in ("source_event_id", "cwd", "text"):
        if event.get(key):
            envelope[key] = event[key]
    return envelope


def sort_events(events):
    """Fixture order: the order derivation itself sorts by (rules.go)."""
    return sorted(events, key=lambda e: (int(e["occurred_at_epoch"]), int(e["id"])))


# --- near-dup gate, ported from internal/modelderive/neardup.go --------------
#
# The pack aligns two observation sets with the SAME gate the store uses to
# reject paraphrase duplicates (memoire:D212), so "these two say the same
# thing" means in a pack exactly what it means in production.
#
# PORTING HAZARD, load-bearing: Python's str.isdigit()/str.isalpha() are not
# equivalent to Go's unicode.IsDigit/IsLetter. '2' (U+00B2) is isdigit() in
# Python but category No, which Go's Nd-only IsDigit rejects; Roman numerals
# are isalpha() but category Nl, which Go's L*-only IsLetter rejects. Either
# divergence changes the normalized form and therefore every trigram, so the
# categories are matched explicitly below. Case folding is the one place the
# two languages cannot be made identical by construction (Python's str.lower()
# is full-case, Go's ToLower is simple per-rune), which is why
# tests/neardup-golden.json is the shared oracle both sides assert against.

# CALIBRATED LAW, not bench knobs: measured against 7,710 labeled pairs from
# the live hub corpus. tests/derive-bench-test.sh asserts both against
# neardup.go, so a recalibration there fails the bench instead of silently
# aligning packs under a policy production no longer runs.
NEARDUP_LO_THRESHOLD = 0.45
NEARDUP_HI_THRESHOLD = 0.90

# BENCH-LOCAL CONSTANT — explicitly NOT calibrated law like the two thresholds
# above. It rests on no corpus: it is the readability floor under which two
# observations are not worth showing a judge side by side at all, chosen so a
# pack does not hand out dozens of unrelated pairs. Tune it freely. Every pack
# records the floor it was built under in its metadata.
DISPUTED_FLOOR = 0.30


def normalize_for_neardup(text):
    """neardup.go normalizeForNearDup: lowercase, non-alphanumerics to space.

    Lowercasing runs PER CHARACTER, and takes only the first character when one
    expands. Both halves are required for rune-equivalence with Go's
    `strings.ToLower`, which is a simple per-rune mapping:

    - Per character, because Python's whole-string `str.lower()` applies
      Unicode CONTEXT rules that Go has no equivalent of. Final_Sigma turns a
      word-final 'Σ' into 'ς', while Go maps 'Σ' to 'σ' wherever it sits.
      Lowering one character at a time denies the rule the context it needs.
    - First character only, because full lowercase can expand one character
      into several where Go's simple mapping yields exactly one. U+0130 ('İ')
      lowercases to 'i' + U+0307 in Python but to plain 'i' in Go; the
      combining mark would then be categorized as a non-letter, become a
      space, and shift every trigram after it. U+0130 is the ONLY
      unconditional lowercase expansion in SpecialCasing.txt, so taking the
      first character is exact here rather than a heuristic that happens to
      work.

    Both divergences are pinned by vectors in tests/neardup-golden.json.

    KNOWN RESIDUAL, not fixable here: the two runtimes carry different Unicode
    table versions (measured 2026-08-24: Python 16.0.0, Go 15.0.0). Across the
    repertoire both know, the two normalizers agree on every scalar value —
    verified by an exhaustive 1,112,064-code-point scan, 0 mismatches. The
    disagreements are exactly the ~5k code points ASSIGNED IN THE NEWER
    VERSION: Go reads them as unassigned and spaces them out, Python keeps them
    as letters. The skew is one-directional, it closes when Go's tables catch
    up, and correcting for it would mean shipping a Unicode table inside the
    bench — worse than the near-zero chance of a brand-new script appearing in
    a dev-session transcript.
    """
    out = []
    for ch in text:
        lowered = ch.lower()
        ch = lowered[0] if lowered else ch
        category = unicodedata.category(ch)
        out.append(ch if category[0] == "L" or category == "Nd" else " ")
    return " ".join("".join(out).split())


def trigram_set(norm):
    """neardup.go trigramSet, including its under-3-runes whole-string case."""
    runes = list(norm)
    if len(runes) < 3:
        return {norm} if runes else set()
    return {"".join(runes[i:i + 3]) for i in range(len(runes) - 2)}


def digit_tokens(norm):
    """neardup.go digitTokens: tokens carrying at least one Nd rune."""
    return {
        token for token in norm.split()
        if any(unicodedata.category(ch) == "Nd" for ch in token)
    }


def neardup_profile(text):
    norm = normalize_for_neardup(text)
    return {"norm": norm, "trigrams": trigram_set(norm), "digits": digit_tokens(norm)}


def jaccard(left, right):
    if not left and not right:
        return 1.0
    intersection = len(left & right)
    union = len(left) + len(right) - intersection
    if union == 0:
        return 0.0
    return intersection / union


def neardup_hit(left, right):
    """neardup.go nearDupHit — (score, policy hit) for two profiles."""
    score = jaccard(left["trigrams"], right["trigrams"])
    if score >= NEARDUP_HI_THRESHOLD:
        return score, True
    if score >= NEARDUP_LO_THRESHOLD and left["digits"] == right["digits"]:
        return score, True
    return score, False


def neardup_text_hit(left_text, right_text):
    return neardup_hit(neardup_profile(left_text), neardup_profile(right_text))


def align_sets(left_texts, right_texts, disputed_floor=DISPUTED_FLOOR):
    """Mechanically align two observation sets (design doc §3).

    Pairs clearing the production near-dup policy are EQUIVALENT and drop out
    of judgment; pairs below the policy but at or above the bench-local floor
    are shown side by side as disputed; everything left is present in one side
    only. Both matchings are greedy 1:1 by descending score, so one strong
    observation can never stand in for several on the other side.

    Returns (equivalent, disputed, left_only, right_only) where the pair lists
    carry index pairs into the two input lists. The whole computation runs
    BEFORE blinding, on incumbent/candidate order, and is symmetric in score,
    so which side ends up behind which nonce cannot change the alignment.
    """
    left_profiles = [neardup_profile(t) for t in left_texts]
    right_profiles = [neardup_profile(t) for t in right_texts]
    scored = []
    for i, left in enumerate(left_profiles):
        for j, right in enumerate(right_profiles):
            score, hit = neardup_hit(left, right)
            scored.append((score, hit, i, j))
    scored.sort(key=lambda row: (-row[0], row[2], row[3]))

    used_left, used_right = set(), set()
    equivalent, disputed = [], []
    for score, hit, i, j in scored:
        if not hit or i in used_left or j in used_right:
            continue
        used_left.add(i)
        used_right.add(j)
        equivalent.append({"left": i, "right": j, "score": round(score, 6)})
    for score, hit, i, j in scored:
        if hit or score < disputed_floor or i in used_left or j in used_right:
            continue
        used_left.add(i)
        used_right.add(j)
        disputed.append({"left": i, "right": j, "score": round(score, 6)})
    return (
        equivalent,
        disputed,
        [i for i in range(len(left_texts)) if i not in used_left],
        [j for j in range(len(right_texts)) if j not in used_right],
    )


def neardup_best(text, candidates):
    """The strongest policy hit of one text against a list — the sanity probe.

    `hit_index` is -1 when nothing clears the policy; `best_score` is reported
    either way so a near-miss reads differently from a total absence.
    """
    profile = neardup_profile(text)
    result = {"hit_index": -1, "hit_score": 0.0, "best_index": -1, "best_score": 0.0}
    for i, other in enumerate(candidates):
        score, hit = neardup_hit(profile, neardup_profile(other))
        if score > result["best_score"] or result["best_index"] < 0:
            result["best_index"], result["best_score"] = i, round(score, 6)
        if hit and (score > result["hit_score"] or result["hit_index"] < 0):
            result["hit_index"], result["hit_score"] = i, round(score, 6)
    return result


def wait_for(predicate, timeout_s, interval_s=0.1):
    deadline = time.monotonic() + timeout_s
    last = None
    while time.monotonic() < deadline:
        last = predicate()
        if last:
            return last
        time.sleep(interval_s)
    return last
