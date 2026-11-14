# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas

.PHONY: check githooks test-core test-ops ops-image pg-shadow-up pg-shadow-down pg-copy pg-replay pg-diff dev derive test smoke check-health restart-smoke codex-capture-smoke codex-rollout-capture-test opencode-capture-test acpx-capture-test muse-capture-test coverage coverage-check qwen-rollout-smoke redaction-smoke claude-capture-smoke build ui quadlet-image quadlet-install quadlet-start quadlet-rebuild quadlet-stop quadlet-status quadlet-health fmt dist dist-sign dist-check

GO_TAGS := sqlite_fts5
# Build-stamp (MISSING #7): /v1/health reports the version the binary was
# built from. Unstamped builds (`go run`, `go test`) report "dev".
GO_LDFLAGS := -X memoire/services/api/internal/buildinfo.Version=$(shell head -1 VERSION)
UI_DIR := services/ui
UI_DIST := services/api/internal/ui/dist

dev:
	go run -tags '$(GO_TAGS)' -ldflags '$(GO_LDFLAGS)' ./services/api/cmd/api

derive:
	go run -tags '$(GO_TAGS)' ./services/api/cmd/derive $(ARGS)

# Test split (L0011 P3, memoire:D226/D228 docs/EXPORT-DESIGN.md §5):
# `test` = test-core (public core, green in the public checkout and in the
# home repo) + test-ops (ops scripts, local tooling lanes). Lanes whose
# subject is private (generated decision state, fleet registry, cdk8s /
# deploy parity, the export manifest) run only in the ledger:
# `make -f Makefile.ledger test` (Makefile.ledger ships home-only until the
# cutover, then becomes the ledger's Makefile). The split is static in both
# files — no marker-file branching, never an export-time rewrite.

# Tests never read the operator's project registry: an entry covering the
# temp directory (a scratch entry for /tmp, /var/tmp) replaces the basename
# fallback several tests assert. A test that needs a registry sets its own.
test-core test-ops: export MEMOIRE_PROJECTS_REGISTRY := /nonexistent/memoire-projects.json

# go:embed all:dist needs a built (or at least present) dist; build the UI
# once if it is missing so `go test` compiles on a fresh checkout.
test: test-core test-ops

# The owner gate (liaison HARNESS-PARITY.md § 3): the tests AND this machine's
# observed capture coverage. Red while any route is a gap; CI runs `make test`
# (hermetic), a host runs `make check`.
check: test coverage-check

test-core: $(UI_DIST)/index.html
	go test -tags '$(GO_TAGS)' ./services/api/...
	cd services/mcp && go test -race ./...
	bash scripts/tests/mcp-tokenless-stdio-test.sh
	bash scripts/tests/resolve-project-test.sh
	bash scripts/tests/check-health-test.sh
	bash scripts/tests/capture-project-wiring-test.sh
	bash scripts/tests/turn-ingest-adapter-test.sh
	bash scripts/tests/require-ran.sh bash scripts/tests/claude-prompt-identity-test.sh
	bash scripts/tests/require-ran.sh bash scripts/tests/claude-hook-queue-failure-test.sh
	$(MAKE) codex-rollout-capture-test
	$(MAKE) opencode-capture-test
	$(MAKE) acpx-capture-test
	$(MAKE) muse-capture-test
	bash scripts/tests/coverage-test.sh
	bash scripts/tests/inject-envelope-test.sh
	bash scripts/tests/inject-identity-test.sh
	bash scripts/tests/inject-pull-mode-test.sh
	bash scripts/tests/inject-transport-backlog-test.sh
	bash scripts/tests/inject-off-test.sh
	bash scripts/tests/opencode-plugin-test.sh
	bash scripts/tests/opencode-plugin-v2-test.sh
	bash scripts/tests/require-ran.sh bash scripts/tests/pi-extension-test.sh
	bash scripts/tests/require-ran.sh bash scripts/tests/pi-sweep-test.sh
	bash scripts/tests/require-ran.sh bash scripts/tests/subagent-context-plugin-test.sh
	bash scripts/tests/require-ran.sh bash scripts/tests/capture-trigger-plugin-test.sh
	bash scripts/tests/spdx-header-test.sh
	bash scripts/tests/version-stamp-test.sh
	bash tests/e2e/standalone-sqlite-smoke.sh

# Versioned drift gate (inbox 2026-08-01): hooks live in .githooks/ so a
# fresh clone gets the gate. Wiring is idempotent. The ledger's
# `make test` self-heals it (Makefile.ledger, first recipe line, guarded
# by the .githooks/ presence check); this public Makefile never touches
# a checkout's git config from `make test` — a public export has no
# .githooks/ and pointing core.hooksPath at a missing dir would silently
# disable the consumer's own .git/hooks (review r1 F2).
githooks:
	@if [ -d .githooks ]; then \
	  test "$$(git config core.hooksPath 2>/dev/null)" = ".githooks" || git config core.hooksPath .githooks; \
	else echo "githooks: .githooks/ not present in this checkout — skipping"; fi

# Publishing the harness-parity coverage file is a fleet-host side effect
# (liaison reads ~/.agents/coverage/memoire.json); scripts/tests/coverage-test.sh
# tests the validator everywhere. Hosts without the fleet inventory skip it.
test-ops:
	@inv="$${MEMOIRE_HARNESS_INVENTORY:-$$HOME/.agents/harnesses.json}"; \
	if [ -f "$$inv" ]; then python3 scripts/memoire-coverage.py --print; \
	else echo "coverage: no harness inventory at $$inv; publication skipped (not a fleet host)"; fi
	bash scripts/tests/live-turn-ingest-test.sh
	bash scripts/tests/capture-tick-test.sh
	bash scripts/tests/codex-capture-tick-test.sh
	bash scripts/tests/qwen-capture-tick-test.sh
	bash scripts/tests/opencode-capture-tick-test.sh
	bash scripts/tests/require-ran.sh bash scripts/tests/pi-capture-tick-test.sh
	bash scripts/tests/acpx-capture-tick-test.sh
	bash scripts/tests/muse-capture-tick-test.sh
	bash scripts/tests/capture-health-test.sh
	bash scripts/tests/kill-switch-test.sh
	bash scripts/tests/resolver-consumer-enumeration-test.sh
	bash scripts/tests/require-ran.sh bash scripts/tests/subagent-context-test.sh
	python3 scripts/tests/codex-parent-test.py
	bash scripts/tests/require-ran.sh bash scripts/tests/subagent-context-perf-test.sh
	bash scripts/tests/require-ran.sh bash scripts/tests/capture-trigger-test.sh
	bash scripts/tests/sweep-split-test.sh
	bash scripts/tests/sweep-ceiling-test.sh
	bash scripts/tests/derive-db-env-test.sh
	bash scripts/tests/hl5-trigger-test.sh
	bash scripts/tests/lapse-pause-test.sh
	bash scripts/tests/hl5-lane-units-test.sh
	bash scripts/tests/project-move-test.sh
	bash scripts/tests/relabel-observed-test.sh
	bash scripts/tests/rekey-derivation-runs-test.sh
	bash scripts/tests/retention-conformance-test.sh
	bash scripts/tests/snapshot-test.sh
	bash bench/derive/tests/derive-bench-test.sh
	# The L0011 split contract itself and the pre-push leak gate (P3).
	bash scripts/tests/test-lanes-test.sh
	bash scripts/tests/prepush-leak-test.sh

# Build the review SPA into the Go embed directory (services/api/internal/ui/dist).
ui:
	test -d $(UI_DIR)/node_modules || (cd $(UI_DIR) && npm ci --no-audit --fund=false)
	cd $(UI_DIR) && npm run build
	touch $(UI_DIST)/.gitkeep

$(UI_DIST)/index.html:
	$(MAKE) ui

smoke:
	./scripts/smoke.sh

check-health:
	./scripts/check-health.sh

restart-smoke: build
	./tests/e2e/restart-smoke.sh

codex-capture-smoke: build
	./tests/foreign/codex-capture-smoke.sh

# Builds its own queue writer from this checkout into its scratch directory,
# so it needs no `build` (no UI, no other binary).
codex-rollout-capture-test:
	./tests/foreign/codex-rollout-capture.sh

# Builds its own queue writer from this checkout into its scratch directory,
# so it needs no `build` (no UI, no other binary).
opencode-capture-test:
	./tests/foreign/opencode-capture.sh
	./tests/foreign/opencode-v2-capture.sh

# Synthetic fixtures and a stub queue writer: no build, no store, no network.
acpx-capture-test:
	./tests/foreign/acpx-journal-capture.sh

# Synthetic store, stub `muse export` and stub queue writer: no build, no
# store, no network, no Muse binary. MUSE_CONTRACT_BIN=<muse> adds the
# stub-vs-real export contract check.
muse-capture-test:
	./tests/foreign/muse-store-capture.sh

# Harness parity (liaison docs/design/HARNESS-PARITY.md): publish Memoire's
# OBSERVED capture coverage per fleet harness for liaison's `harness.parity`
# row (~/.agents/coverage/memoire.json). `coverage-check` is this project's
# own gate on its branchements: red while any route is a gap.
coverage:
	python3 scripts/memoire-coverage.py --print

coverage-check:
	python3 scripts/memoire-coverage.py --strict
	liaison harnesses coverage --owner memoire --strict

qwen-rollout-smoke: build
	./tests/foreign/qwen-rollout-capture-smoke.sh

redaction-smoke: build
	./tests/foreign/redaction-smoke.sh

claude-capture-smoke: build
	./tests/foreign/claude-capture-smoke.sh

claude-attribute-smoke: build
	./tests/foreign/claude-attribute-smoke.sh

build: ui
	mkdir -p bin
	go build -tags '$(GO_TAGS)' -ldflags '$(GO_LDFLAGS)' -o bin/memoire-api ./services/api/cmd/api
	go build -tags '$(GO_TAGS)' -o bin/memoire-derive ./services/api/cmd/derive
	go build -tags '$(GO_TAGS)' -o bin/memoire-triage ./services/api/cmd/triage
	go build -tags '$(GO_TAGS)' -o bin/memoire-queue ./services/api/cmd/queue
	go build -tags '$(GO_TAGS)' -o bin/memoire-redact-derived ./services/api/cmd/redactderived
	go build -tags '$(GO_TAGS)' -o bin/memoire-purge ./services/api/cmd/purge
	go build -tags '$(GO_TAGS)' -o bin/memoire-raw-pack ./services/api/cmd/rawpack
	go build -tags '$(GO_TAGS)' -o bin/memoire-forward ./services/api/cmd/forward
	go build -tags '$(GO_TAGS)' -o bin/memoire-mint-token ./services/api/cmd/minttoken
	go build -tags '$(GO_TAGS)' -o bin/memoire-pgshadow ./services/api/cmd/pgshadow
	go build -ldflags '-X main.serverVersion=$(shell head -1 VERSION)' -o bin/memoire-mcp ./services/mcp

# Standalone-SQLite release artifact (R-10.5, memoire:D165 Q4). Curated
# payload: client + local-service surface only — ops-lane scripts (sweep,
# snapshot, quadlet, hl5 trigger) and pgshadow stay out. SHA256SUMS covers
# the tarball; dist-sign is operator-present (YubiKey GPG detached sig).
DIST_VERSION := $(shell head -1 VERSION)
DIST_PLATFORM := $(shell go env GOOS)-$(shell go env GOARCH)
DIST_NAME := memoire-$(DIST_VERSION)-$(DIST_PLATFORM)
DIST_DIR := dist/$(DIST_NAME)
DIST_BINS := memoire-api memoire-queue memoire-mint-token memoire-mcp \
	memoire-derive memoire-forward memoire-purge memoire-redact-derived
DIST_SCRIPTS := memoire-capture-claude.sh memoire-capture-claude-transcripts.sh \
	memoire-capture-codex.sh memoire-capture-codex-rollout.sh \
	memoire-capture-opencode.sh memoire-capture-qwen.sh \
	memoire-capture-qwen-rollout.sh memoire-capture-text.sh \
	memoire-inject-claude.sh memoire-inject-envelope.sh \
	memoire-resolve-project.sh check-health.sh

dist: build
	rm -rf $(DIST_DIR) dist/$(DIST_NAME).tar.gz dist/SHA256SUMS dist/SHA256SUMS.asc
	mkdir -p $(DIST_DIR)/bin $(DIST_DIR)/scripts
	cp $(addprefix bin/,$(DIST_BINS)) $(DIST_DIR)/bin/
	cp $(addprefix scripts/,$(DIST_SCRIPTS)) $(DIST_DIR)/scripts/
	cp -r scripts/opencode-plugin $(DIST_DIR)/scripts/opencode-plugin
	cp -r scripts/pi-extension $(DIST_DIR)/scripts/pi-extension
	cp -r schema $(DIST_DIR)/schema
	cp -r schema-pg $(DIST_DIR)/schema-pg
	cp LICENSE VERSION QUICKSTART.md model-endpoints.example.json \
		derive-denylist.example.json $(DIST_DIR)/
	cp scripts/memoire-bootstrap.sh $(DIST_DIR)/memoire-bootstrap.sh
	tar -C dist -czf dist/$(DIST_NAME).tar.gz $(DIST_NAME)
	cd dist && sha256sum $(DIST_NAME).tar.gz > SHA256SUMS
	@echo "dist: dist/$(DIST_NAME).tar.gz + dist/SHA256SUMS"

dist-sign:
	test -f dist/SHA256SUMS
	rm -f dist/SHA256SUMS.asc
	gpg --armor --detach-sign --output dist/SHA256SUMS.asc dist/SHA256SUMS
	gpg --verify dist/SHA256SUMS.asc dist/SHA256SUMS
	@echo "dist-sign: dist/SHA256SUMS.asc"

# Release order: dist -> dist-check -> dist-sign. dist-check validates the
# EXISTING artifact (no dist dependency) so signing always follows a checked,
# immutable tarball; dist deletes any prior signature.
dist-check:
	bash scripts/tests/dist-contract-test.sh

quadlet-image:
	podman build --target api -t localhost/memoire:latest -f services/api/Containerfile .

# HL3 relocation (docs/HL3-HUB-RELOCATION-PLAN.md): the CronJob image.
ops-image:
	podman build --target ops -t localhost/memoire-ops:latest -f services/api/Containerfile .

# D218 vector lane: embed worker (freshness sweep CronJob + query-embed
# service). Model files are fetched and offline-verified inside the build.
embed-image:
	podman build -t localhost/memoire-embed:latest -f services/embed-worker/Containerfile .

# Registry images — WORKSTATION FALLBACK. The PRIMARY build path is CI
# (.github/workflows/images.yml — build + scan on every image-relevant push
# to main, operator-ruled 2026-08-05); this target exists for a CI outage and
# produces byte-for-byte the same tag scheme.
#
# IMMUTABLE pre-release semver tags, NEVER :latest (operator-ruled
# 2026-07-30): <VERSION>-dev.g<shortsha>. Refuses a dirty worktree so the tag
# names a real commit. Deploy = replace the two full-ref *_IMAGE consts in
# cdk8s/main.ts with the validated refs this target prints (tag@digest —
# never hand-assembled; r2 SIMPLIFICATION), `npm run synth`,
# `make -f Makefile.ledger cdk8s-verify`, commit, `make -f Makefile.ledger
# deploy` (the L6 parity gate, memoire:D199 — see below). Push auth is the
# caller's (one Bitwarden touch, transient login):
#   bw-env-run --file ~/Sync/config/setup-new-machines/bw-env.d/registry-superbiche.env -- \
#     sh -c 'podman login registry.superbiche.me -u "$REGISTRY_CI_PUSH_USER" \
#       --password-stdin <<<"$REGISTRY_CI_PUSH_PASSWORD" && make images-push; \
#       podman logout registry.superbiche.me'
IMAGE_REGISTRY := registry.superbiche.me/superbiche
IMAGE_TAG = $(shell head -1 VERSION)-dev.g$(shell git rev-parse --short=7 HEAD)

# Fleet image-scan gate (infra docs/image-scanning.md; inbox 2026-08-01):
# this local gate mirrors the CI gate for fallback builds — every FINAL image
# is scanned before ANY push, on either path, so a CI-outage build is held to
# the same bar as a CI build. Policy:
# fail on CRITICAL/HIGH with a fix available, unfixed report-only, dated
# exceptions in .trivyignore.yaml (validated fail-closed by the script).
IMAGE_SCAN_SH ?= $(abspath ../infra/scripts/image-scan/scan-image.sh)

images-push:
	@test -z "$$(git status --porcelain --untracked-files=no)" || { echo "refusing: dirty worktree — the image tag must name a real commit"; exit 1; }
	@test -x "$(IMAGE_SCAN_SH)" || { echo "refusing: fleet image-scan script not found ($(IMAGE_SCAN_SH)) — clone superbiche/infra beside this repo or set IMAGE_SCAN_SH (policy: infra docs/image-scanning.md)"; exit 1; }
	@if [ -f .trivyignore.yaml ]; then git ls-files --error-unmatch .trivyignore.yaml >/dev/null 2>&1 && test -z "$$(git status --porcelain -- .trivyignore.yaml)" || { echo "refusing: .trivyignore.yaml exists but is untracked or modified — accepted advisories must be committed state the image tag names (review r1 F1)"; exit 1; }; fi
	podman build --target api -t $(IMAGE_REGISTRY)/memoire:$(IMAGE_TAG) -f services/api/Containerfile .
	podman build --target ops -t $(IMAGE_REGISTRY)/memoire-ops:$(IMAGE_TAG) -f services/api/Containerfile .
	bash $(IMAGE_SCAN_SH) $(IMAGE_REGISTRY)/memoire:$(IMAGE_TAG)
	bash $(IMAGE_SCAN_SH) $(IMAGE_REGISTRY)/memoire-ops:$(IMAGE_TAG)
	# The printed digest is the one the registry stored (--digestfile), never
	# podman's local RepoDigests: those can predate the push and leave the
	# hub with an unpullable ref (ErrImagePull on 2026-10-09).
	@mkdir -p tmp/images-push
	podman push --digestfile tmp/images-push/memoire.digest $(IMAGE_REGISTRY)/memoire:$(IMAGE_TAG)
	podman push --digestfile tmp/images-push/memoire-ops.digest $(IMAGE_REGISTRY)/memoire-ops:$(IMAGE_TAG)
	@refs=""; for img in memoire memoire-ops; do \
	  digest=$$(cat tmp/images-push/$$img.digest); \
	  echo "$$digest" | grep -qE '^sha256:[0-9a-f]{64}$$' || { echo "refusing: no valid digest for $$img:$(IMAGE_TAG) (got '$$digest')"; exit 1; }; \
	  refs="$$refs  $(IMAGE_REGISTRY)/$$img:$(IMAGE_TAG)@$$digest\n"; \
	done; \
	echo "pushed $(IMAGE_TAG) — paste-ready deploy refs (scan ref == rollout ref):"; \
	printf "$$refs"; \
	echo "replace the two *_IMAGE consts in cdk8s/main.ts with the refs above (verbatim), npm run synth, make -f Makefile.ledger cdk8s-verify, commit, make -f Makefile.ledger deploy"

# cdk8s-synth / cdk8s-verify / deploy / deploy-diff moved to
# Makefile.ledger (L0011 P3): deployment configuration is a ledger
# subject (a31). `make -f Makefile.ledger deploy-diff` and friends.

quadlet-install:
	./scripts/quadlet.sh install

quadlet-start: quadlet-image quadlet-install
	./scripts/quadlet.sh start

quadlet-rebuild:
	./scripts/quadlet-rebuild.sh

quadlet-stop:
	./scripts/quadlet.sh stop

quadlet-status:
	./scripts/quadlet.sh status

quadlet-health:
	./scripts/quadlet.sh health

# --- slice-5 standing-shadow loop (docs/SLICE5-GROUNDWORK-DESIGN.md) ------
# The local rehearsal twin of the HL3 CNPG shadow. Not part of `make test`
# (needs podman); the loop is: up -> copy or replay -> diff -> down.
pg-shadow-up: build
	./scripts/pg-shadow.sh up

pg-shadow-down:
	./scripts/pg-shadow.sh down

# The URL never appears in argv, make's echoed recipe, or the shell -c
# line: recipes are @-silenced and resolve the URL via in-shell command
# substitution, which stays unexpanded in every process list (round-1
# finding 7, tightened round 2).
pg-copy:
	@MEMOIRE_PG_URL="$$(./scripts/pg-shadow.sh url --unsafe)" ./bin/memoire-pgshadow copy $(ARGS)

pg-replay:
	@MEMOIRE_PG_URL="$$(./scripts/pg-shadow.sh url --unsafe)" ./bin/memoire-pgshadow replay $(ARGS)

pg-diff:
	@MEMOIRE_PG_URL="$$(./scripts/pg-shadow.sh url --unsafe)" ./bin/memoire-pgshadow diff -out tmp/pg-parity-$$(date -u +%Y%m%dT%H%M%SZ).md $(ARGS)

# Dialect-seam conformance battery on BOTH engines (memoire:D157): the
# postgres leg creates and drops a throwaway database on the local shadow.
# Not part of `make test` (needs podman).
test-pg: pg-shadow-up
	@MEMOIRE_TEST_PG_URL="$$(./scripts/pg-shadow.sh url --unsafe)" go test -tags 'sqlite_fts5' -run 'TestEnginePairConformance|TestPGCollation|TestPGLocaleProvider|TestPGMigrateLock|TestBM25' ./services/api/internal/db/ -count=1 -v
	@MEMOIRE_TEST_PG_URL="$$(./scripts/pg-shadow.sh url --unsafe)" go test -tags 'sqlite_fts5' ./services/api/internal/pgshadow/ -count=1
	@MEMOIRE_TEST_PG_URL="$$(./scripts/pg-shadow.sh url --unsafe)" go test -tags 'sqlite_fts5' -run 'TestPG' ./services/api/internal/purge/ -count=1 -v
	@MEMOIRE_TEST_PG_URL="$$(./scripts/pg-shadow.sh url --unsafe)" go test -tags 'sqlite_fts5' ./services/api/internal/redactderived/ ./services/api/cmd/redactderived/ -count=1

fmt:
	gofmt -w services/api
