# rift operator Makefile. Run `make` (or `make help`) for the target list.
SHELL := bash
.DEFAULT_GOAL := help

COMPOSE      := docker compose -f deploy/docker-compose.yml
COMPOSE_PROD := docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.prod.yml

# Load an untracked .env into the environment for the tooling targets (deploy,
# provision-key, mint-token). The compose targets read .env on their own. This
# is a single line so it can prefix a recipe command; a missing .env is fine.
LOAD_ENV := set -a; [ -f .env ] && . ./.env; set +a;

.PHONY: help build-server build-cli test e2e lint up down logs migrate deploy provision-key mint-token build-caddy release release-docker gen-docs \
	setup ship verify check-dns provision harden hostcheck backup restore e2e-recovery e2e-hostcheck e2e-provision e2e-setup publish-images docs

help: ## Show this help
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z0-9_-]+:.*##/{printf "  \033[36m%-14s\033[0m %s\n",$$1,$$2}' $(MAKEFILE_LIST)

build-server: ## Build the riftd server binary (projects/server/riftd)
	cd projects/server && CGO_ENABLED=0 go build -o riftd ./cmd/riftd

build-cli: ## Build the CLI single-binary image (deploy/Dockerfile.cli)
	docker build -f deploy/Dockerfile.cli -t rift-cli:local .

test: ## Run server tests
	cd projects/server && go test ./...

e2e: ## Full black-box e2e in Docker: real Caddy, real TLS, real CLI (ARGS=--mode internal)
	bash tools/e2e.sh $(ARGS)

build-caddy: ## Build Caddy with the DNS-01 plugins from .env (ARGS=--validate --push)
	bash tools/build-caddy.sh $(ARGS)

release: ## Cross-compile rift release artifacts into dist/release/<version> (ARGS=--clean)
	bash tools/release.sh $(ARGS)

release-docker: ## Reproducible release build in a pinned Bun container -> dist/release/ (ARGS=--bun 1.3.12)
	bash tools/release-docker.sh $(ARGS)

gen-docs: ## Regenerate the man page + shell completions from the CLI spec
	cd projects/cli && bun src/index.ts man > ../../packaging/man/rift.1
	cd projects/cli && for sh in bash zsh fish; do bun src/index.ts completions $$sh > ../../packaging/completions/rift.$$sh; done

lint: ## Vet Go, syntax-check shell scripts, validate compose
	cd projects/server && go vet ./...
	bash -n tools/*.sh tools/lib/*.sh
	@command -v shellcheck >/dev/null 2>&1 && shellcheck tools/*.sh tools/lib/*.sh || echo "shellcheck not installed; skipped"
	$(COMPOSE) config -q

up: ## Start the local dev stack (add `--profile redis` for the redis backend)
	$(COMPOSE) up -d --build

down: ## Stop the local dev stack and remove its containers
	$(COMPOSE) down

logs: ## Follow logs from the local dev stack
	$(COMPOSE) logs -f

migrate: ## Apply DB migrations (riftd runs them on start; ensures pg is up, recreates riftd)
	$(COMPOSE) up -d postgres
	$(COMPOSE) up -d --force-recreate riftd

deploy: ## Build & deploy the stack to the VPS (ARGS=--dry-run to preview)
	@$(LOAD_ENV) bash tools/remote-deploy.sh $(ARGS)

provision-key: ## Generate a deploy key and install it on the VPS
	@$(LOAD_ENV) bash tools/ssh-provision-key.sh

mint-token: ## Mint an admin token: make mint-token NAME=alice
	@$(LOAD_ENV) bash tools/mint-token.sh $(NAME)

# --- guided setup & pipeline -------------------------------------------------
setup: ## Interactive wizard: generate an untracked .env (ARGS=--force)
	bash tools/setup.sh $(ARGS)

ship: ## Full pipeline: provision -> harden -> deploy -> verify (ARGS=--from deploy)
	bash tools/ship.sh $(ARGS)

verify: ## Assert a live deployment serves TLS correctly (gate; ARGS=--host IP)
	bash tools/verify-deploy.sh $(ARGS)

check-dns: ## Advisory DNS/split-horizon check (never fails; ARGS=--tls-mode dns01)
	bash tools/check-dns.sh $(ARGS)

# --- provisioning & hardening ------------------------------------------------
provision: ## Create a VPS and wait for SSH (ARGS=--dry-run)
	bash tools/provision.sh $(ARGS)

harden: ## Host-harden a rift VPS in place (ARGS=--check for a CI gate)
	bash tools/harden.sh $(ARGS)

hostcheck: ## Prove tools/harden.sh in a throwaway Debian container (ARGS=--keep)
	bash tools/e2e-hostcheck.sh $(ARGS)

# --- backup & recovery -------------------------------------------------------
backup: ## Back up Postgres + caddy_data (ARGS=--retain 14)
	bash tools/backup.sh $(ARGS)

restore: ## Restore a backup: make restore FROM=/opt/rift/backups/rift-<ts> (ARGS=--yes)
	bash tools/restore.sh --from $(FROM) $(ARGS)

# --- container images --------------------------------------------------------
publish-images: ## Build (ARGS=--push to publish) the ghcr container images
	bash tools/publish-images.sh $(ARGS)

# --- docs --------------------------------------------------------------------
docs: ## Build the documentation site (projects/docs-site/)
	cd projects/docs-site && bun install && bun run build

# --- isolated test harnesses (Docker, hermetic) -----------------------------
e2e-recovery: ## Prove backup/restore in a throwaway Docker stack (ARGS=--keep)
	bash tools/e2e-recovery.sh $(ARGS)

e2e-provision: ## Provisioning e2e: mock cloud API + sshd, no real cloud (ARGS=--keep)
	bash tools/e2e-provision.sh $(ARGS)

e2e-setup: ## Test the setup wizard against the real config validator
	bash tools/e2e-setup.sh $(ARGS)
