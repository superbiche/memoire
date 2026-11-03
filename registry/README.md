# Memoire project registry

> **Status note (2026-07-18, liaison:D239):** `memoire-projects.seed.json` has been
> **regenerated under the liaison:D239 convention**, superseding the liaison:D238 seed. Keys are
> now the **git-root path relative to `~/dev`, slashes→dashes**, with subdirs
> rolling up to their git root (`client-a/app-one`→`client-a`;
> `beta-project/stack/{vacation,hostrider}`→`beta-project-stack`; all
> `infra/servers/*` →`personal-infra`). Under liaison:D239 the registry is DEMOTED to historical re-key
> targets + old-home table + scratch patterns; the primary LIVE key source is a
> repo-root `.memoire` file (precedence: `MEMOIRE_PROJECT` env > nearest
> `.memoire` > registry > basename). The seed doc string carries the full
> per-field convention. Dry-run + spot-check + install notes:
> `liaison-projects-drift/tmp/nightwatch-2026-07-18b/` (REKEY-DRYRUN-v2.md,
> INSTALL-READY.md). The liaison:D238 description below is preserved for history.

Paths → project-id registry that disambiguates Memoire's project identity.
Ruled in liaison:D238 (ruling 4, operator-ratified): a registry maps directory paths to
project ids; the directory **basename stays the display name and the fail-open
fallback**. The registry only steps in to split same-basename collisions
(`apps`, `infra`, `vacation`, `app-one`, `hostrider`, …) and to fold a project's
old-home paths into its current id.

Background and the original confidentiality trigger:
`inbox/2026-07-12-memoire-project-identity-collision.md` (client-a client
observations injected into a personal project because both keyed on basename
`app-one`).

## Files

- `memoire-projects.seed.json` — liaison:D239-regenerated seed, in-branch only. Ids
  follow the **ratified liaison:D239 convention** (no longer proposals); the file is the
  canonical content destined verbatim for `~/.agents/memoire-projects.json`, but
  install + store re-key are operator-gated morning steps, **not** done tonight.
- Resolver: `../scripts/memoire-resolve-project.sh`.
- Test: `../scripts/tests/resolve-project-test.sh`.

## Schema — `liaison-memoire-projects/1`

```jsonc
{
  "schema": "liaison-memoire-projects/1",
  "doc": "free-text provenance / status note",
  "projects": {
    "<project-id>": {
      "display": "app-one",                      // human/UI name; == pre-registry basename key
      "paths":   ["/abs/prefix", "/old/home/prefix"], // real dir prefixes owned by this id
      "aliases": ["old-basename-key"],           // OLD store project-keys to re-key (NOT resolution inputs)
      "scratch": false,                          // see below
      "note":    "optional per-entry rationale"
    }
  }
}
```

- **`display`** is the name a reader sees; it stays equal to the old
  basename-derived key, so nothing about the UI changes — only the routing does.
- **`paths`** are absolute directory prefixes. A project may own several: nested
  homes (`.../cctime` + `.../.claude/tools/cctime`), and old-home prefixes
  (`~/Sites/...`, `~/DevOps/...`) folded in so re-keyed/backfilled events land on
  the current id. In this seed, old-home prefixes are **proposed
  reconstructions** and flagged per entry.
- **`aliases`** are OLD basename project-keys already sitting in the store (or
  that the transcript backfill will produce). They are the targets a
  `memoire project-move` re-key consumes at ruling time — **the resolver does not
  read them**. They document "this old key belongs to this id".

## Resolution precedence (liaison:D239, refining liaison:D238)

The resolver (`memoire-resolve-project.sh <event-dir> [session-home-dir]`)
applies, in order:

1. **`MEMOIRE_PROJECT` env** — hard override, identical to every capture/inject
   adapter. Wins unconditionally.
2. **Nearest `.memoire` file** — walk-up from the attribution dir to the
   nearest `.memoire` FILE; its `project:` value wins. This is the PRIMARY
   live key source since liaison:D239; the registry is demoted to history + scratch
   patterns + multi-home exceptions. `capture: false` in the same file is the
   repo-level capture kill switch (session-level: `MEMOIRE_CAPTURE=off`).
3. **Longest path-prefix match** over every entry's `paths`. Matching is
   **component-boundary real prefix**: `/home/a/b` matches `/home/a/b` and
   `/home/a/b/c`, never `/home/a/bc`. When several entries match, the **longest**
   `paths` string wins — this is why a nested `.../srv-one/apps` entry beats both
   `personal-infra` and the `srv-one` server entry for that subtree.
4. **Basename fallback** — house convention: capture must never break. Any
   missing/invalid registry, absent `jq`, or unmatched dir degrades silently to
   `basename(dir)` — exactly today's behavior.

### Session-home attribution (liaison:D238)

The optional 2nd arg is the session **launch** dir. When present it is
**preferred** over the per-event cwd (1st arg), so a session that roams across
repos attributes to its home project instead of fragmenting. The per-event cwd
still rides on each stored event (`events.cwd`); secondary/roaming attribution is
derivable at read time. **No schema change** — this is a read/attribution-time
decision the caller drives by choosing which dir to pass.

## `scratch` flag

`scratch: true` marks an id as throwaway/pollution (e.g. the 218
`.../qwen-coder-bricks-eval/results/**/workspace` dirs, agent worktree scatter).
The resolver **supports** the flag (an entry still resolves to its id) but **no
seed entry ships with it** — the workspace-pollution **denylist is a separate
morning ruling**. Reserved shape only tonight.

## Install (morning step, operator-gated)

Live target: **`~/.agents/memoire-projects.json`** (Liaison-owned shared
config, alongside `memoire.json` / `cctime.json` / `mcp.json`). Override path via
`MEMOIRE_PROJECTS_REGISTRY`. Installing the file is **not** the store re-key: the
re-key (running `memoire project-move` to migrate historical rows onto the new
ids) is its own morning ruling — dry-run only tonight.

The resolver is wired into all six capture adapters, the injector, and
derive-auto (liaison:D238 slice + identity v2 stages 1-3, 2026-07-18); adapters also
forward the resolved root/source for server-side identity-conflict detection
(schema 0010).

## Expected `liaison memoire link` drift-check (pattern pointer)

`liaison memoire link` already installs/drift-checks the harness hook + config
surfaces (`~/.agents/memoire.json` → per-harness wiring, with `--check`). This
registry should grow the **same treatment**: `liaison memoire link --install`
places `memoire-projects.json` at the canonical path, and `--check` reports drift
between the in-repo source of truth and the installed copy (schema-version match,
valid JSON, `.projects` is an object). Same registry pattern as
`liaison skills link` / `liaison mcp link`. Not wired tonight — pointer only.
