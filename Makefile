# rift operator Makefile — deploy/provision/backup/release workflows.
#
# Dev and test workflows live in mise, not here: `mise run ci` (lint, type-check,
# build, test, scan), `mise run e2e` (+ e2e:recovery / e2e:provision / e2e:setup /
# e2e:hostcheck), `mise run gen-docs`, and `mise //projects/<p>:<task>` for a
# single project. Run `make` (or `make help`) for the operator target list.
#
# `tools/rift-ops` is an equivalent, discoverable front door: `rift-ops <group>
# <verb>` (e.g. `rift-ops deploy status`); run it with no args for the grouped
# command tree. Every target below maps to a `rift-ops` verb.
SHELL := bash
.DEFAULT_GOAL := help

COMPOSE      := docker compose -f deploy/docker-compose.yml
COMPOSE_PROD := docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.prod.yml

# The tooling targets need no .env plumbing here: every script that needs the
# untracked .env loads it itself (tools/lib/runtime.sh load_env, which honors
# RIFT_ENV_FILE), so `make X` and `rift-ops ...` behave identically.
SH_SCRIPTS = $(filter-out tools/install.sh,$(wildcard tools/*.sh tools/lib/*.sh tools/cmd/*/*.sh tools/recovery/*.sh tools/providers/*.sh))

.PHONY: help build-cli build-caddy release release-docker lint up down logs migrate \
	deploy rollback rotate-secret teardown provision-key mint-token setup ship verify check-dns \
	doctor status remote-logs cert-watch provision harden backup restore publish-images

help: ## Show this help
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z0-9_-]+:.*##/{printf "  \033[36m%-15s\033[0m %s\n",$$1,$$2}' $(MAKEFILE_LIST)

# --- build & release ---------------------------------------------------------
build-cli: ## Build the CLI single-binary image (deploy/Dockerfile.cli)
	docker build -f deploy/Dockerfile.cli -t rift-cli:local .

build-caddy: ## Build Caddy with the DNS-01 plugins from .env (ARGS=--validate --push)
	bash tools/rift-ops release caddy $(ARGS)

release: ## Cross-compile rift release artifacts into dist/release/<version> (ARGS=--clean)
	bash tools/rift-ops release cli $(ARGS)

release-docker: ## Reproducible release build in a pinned Bun container -> dist/release/ (ARGS=--bun 1.3.14)
	bash tools/rift-ops release docker $(ARGS)

lint: ## Vet Go, syntax-check shell scripts, validate compose, check version pins
	cd projects/server && go vet ./...
	bash -n $(SH_SCRIPTS) tools/rift-ops && sh -n tools/install.sh
	@if command -v shellcheck >/dev/null 2>&1; then \
		shellcheck -x --severity=warning $(SH_SCRIPTS) tools/rift-ops && \
		shellcheck -x -s sh --severity=warning tools/install.sh; \
	else echo "shellcheck not installed; skipped"; fi
	bash tools/check-versions.sh
	bash tools/smoke.sh
	$(COMPOSE) config -q
	RIFT_TLS_MODE=http01 $(COMPOSE_PROD) config -q
	RIFT_TLS_MODE=http01 $(COMPOSE_PROD) -f deploy/docker-compose.tcp.yml -f deploy/docker-compose.tls.yml config -q

# --- local dev stack ---------------------------------------------------------
up: ## Start the local dev stack (add `--profile redis` for the redis backend)
	$(COMPOSE) up -d --build

down: ## Stop the local dev stack and remove its containers
	$(COMPOSE) down

logs: ## Follow logs from the local dev stack
	$(COMPOSE) logs -f

migrate: ## Apply DB migrations (riftd runs them on start; ensures pg is up, recreates riftd)
	$(COMPOSE) up -d postgres
	$(COMPOSE) up -d --force-recreate riftd

# --- deploy ------------------------------------------------------------------
deploy: ## Build & deploy the stack to the VPS (ARGS=--dry-run or --plan to preview)
	bash tools/rift-ops deploy deploy $(ARGS)

rollback: ## Roll riftd back to the previous image saved by the last deploy (ARGS=--yes)
	bash tools/rift-ops deploy rollback $(ARGS)

rotate-secret: ## Rotate a secret on the VPS: make rotate-secret WHICH=admin (or peer)
	bash tools/rift-ops secret rotate "$(WHICH)" $(ARGS)

teardown: ## Destroy the provisioned instance + local state (ARGS=--backup --yes)
	bash tools/rift-ops backup teardown $(ARGS)

provision-key: ## Generate a deploy key and install it on the VPS
	bash tools/rift-ops provision key

mint-token: ## Mint an admin token: make mint-token NAME=alice
	bash tools/rift-ops secret mint-token "$(NAME)"

# --- guided setup & pipeline -------------------------------------------------
setup: ## Interactive wizard: generate an untracked .env (ARGS=--force)
	bash tools/rift-ops config setup $(ARGS)

ship: ## Full pipeline: provision -> harden -> deploy -> verify (ARGS=--from deploy)
	bash tools/rift-ops deploy ship $(ARGS)

verify: ## Assert a live deployment serves TLS correctly (gate; ARGS=--host IP)
	bash tools/rift-ops deploy verify $(ARGS)

check-dns: ## Advisory DNS/split-horizon check (never fails; ARGS=--tls-mode dns01)
	bash tools/rift-ops host check-dns $(ARGS)

# --- health & observability --------------------------------------------------
doctor: ## Check this workstation is ready for the tooling (ARGS=--strict)
	bash tools/rift-ops host doctor $(ARGS)

status: ## Live snapshot of the deployed stack: containers, disk, memory (ARGS=--strict)
	bash tools/rift-ops deploy status $(ARGS)

remote-logs: ## Tail the DEPLOYED stack's logs over SSH (ARGS='-f riftd')
	bash tools/rift-ops deploy logs $(ARGS)

cert-watch: ## Warn before a served TLS cert expires (ARGS='--days 14 --strict')
	bash tools/rift-ops cert watch $(ARGS)

# --- provisioning & hardening ------------------------------------------------
provision: ## Create a VPS and wait for SSH (ARGS=--dry-run)
	bash tools/rift-ops provision create $(ARGS)

harden: ## Host-harden a rift VPS in place (ARGS=--check for a CI gate)
	bash tools/rift-ops host harden $(ARGS)

# --- backup & recovery -------------------------------------------------------
backup: ## Back up Postgres + caddy_data (ARGS=--retain 14)
	bash tools/rift-ops backup backup $(ARGS)

restore: ## Restore a backup: make restore FROM=/opt/rift/backups/rift-<ts> (ARGS=--yes)
	bash tools/rift-ops backup restore --from "$(FROM)" $(ARGS)

# --- container images --------------------------------------------------------
publish-images: ## Build (ARGS=--push to publish) the ghcr container images
	bash tools/rift-ops release images $(ARGS)
