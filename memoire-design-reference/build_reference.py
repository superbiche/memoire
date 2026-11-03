#!/usr/bin/env python3
"""Assemble the single-file reading edition; never touches a Mémoire store."""
from __future__ import annotations

import json
import re
from pathlib import Path
from urllib.parse import unquote

ROOT = Path(__file__).resolve().parent
OUTPUT = ROOT / "REFERENCE-COMPLETE.md"
ORDER: list[tuple[str, str]] = [
    ("README.md", "reference-map"),
    ("REVISION-NOTES.md", "revision-notes"),
    *[(f"design/{name}", f"chapter-{name[:2]}") for name in [
        "01-intent-and-invariants.md", "02-evidence-views-and-lifecycles.md",
        "03-work-identity-and-time.md", "04-derivation-and-consolidation.md",
        "05-retrieval-and-progressive-disclosure.md", "06-alignment-and-design-meetings.md",
        "07-measurement-and-learning.md", "08-locality-security-and-operations.md",
        "09-experiments-and-rollout.md", "10-acceptance-scenarios.md",
        "11-borrowing-and-research.md", "12-decisions-and-open-choices.md",
    ]],
    ("templates/meeting-packet.md", "template-meeting-packet"),
    ("templates/meeting-outcomes.md", "template-meeting-outcomes"),
    ("templates/meeting-feedback.md", "template-meeting-feedback"),
    ("templates/improvement-experiment.md", "template-improvement-experiment"),
    ("sources/README.md", "source-register"),
    ("sources/original-addendum.md", "source-addendum"),
]
LINK = re.compile(r"(!?\[[^\]\n]*\]\()([^\s)]+)(\))")


def combine() -> None:
    anchors = {(ROOT / name).resolve(): anchor for name, anchor in ORDER}
    chunks = [
        "# Mémoire — complete design reference\n\n"
        "**Edition 1.1 · 17 September 2026.** Generated from the modular reference. "
        "Edit the owning modular chapters and run `python3 build_reference.py` to regenerate.\n\n"
        "The separate [first-agent handoff](FIRST-AGENT-HANDOFF.md) limits the initial implementation. "
        "This document contains the full design, experiments, acceptance cases, templates and source basis.\n\n"
        "[Reference map](#reference-map) · [Meeting design](#chapter-06) · "
        "[Measurement](#chapter-07) · [Acceptance cases](#chapter-10) · [Sources](#source-register)\n"
    ]
    for name, anchor in ORDER:
        source = ROOT / name
        if not source.is_file():
            raise FileNotFoundError(f"Missing source chapter: {source}")
        text = source.read_text(encoding="utf-8")

        def relink(match: re.Match[str]) -> str:
            label, target, close = match.groups()
            if re.match(r"^(?:[a-zA-Z][a-zA-Z0-9+.-]*:|#)", target):
                return match.group(0)
            path_part, sep, fragment = target.partition("#")
            resolved = (source.parent / unquote(path_part)).resolve()
            if resolved == OUTPUT.resolve():
                target = "#reference-map"
            elif resolved in anchors:
                target = f"#{fragment}" if sep else f"#{anchors[resolved]}"
            else:
                try:
                    target = resolved.relative_to(ROOT).as_posix()
                except ValueError:
                    raise ValueError(f"Out-of-package link in {name}: {target}")
                if sep:
                    target += f"#{fragment}"
            return label + target + close

        text = LINK.sub(relink, text)
        lines: list[str] = []
        in_fence = False
        fence_char = ""
        for line in text.splitlines():
            match = re.match(r"^\s*(`{3,}|~{3,})", line)
            if match:
                mark = match.group(1)[0]
                if not in_fence:
                    in_fence, fence_char = True, mark
                elif mark == fence_char:
                    in_fence = False
                lines.append(line)
            elif not in_fence and re.match(r"^#{1,5} ", line):
                lines.append("#" + line)
            else:
                lines.append(line)
        if in_fence:
            raise ValueError(f"Unclosed code fence in {name}")
        chunks.append(f'\n---\n\n<a id="{anchor}"></a>\n\n' + "\n".join(lines) + "\n")
    OUTPUT.write_text("\n".join(chunks), encoding="utf-8")
    print(json.dumps({"output": OUTPUT.name, "source_documents": len(ORDER),
                      "words": len(re.findall(r"\S+", OUTPUT.read_text(encoding="utf-8")))}, indent=2))


if __name__ == "__main__":
    combine()
