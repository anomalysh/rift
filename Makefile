# rift operator Makefile. Run `make` (or `make help`) for the target list.
SHELL := bash
.DEFAULT_GOAL := help

COMPOSE      := docker compose -f deploy/docker-compose.yml
COMPOSE_PROD := docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.prod.yml

# Load an untracked .env into the environment for the tooling targets (deploy,
# provision-key, mint-token). The compose targets read .env on their own. This
# is a single line so it can prefix a recipe command; a missing .env is fine.
LOAD_ENV := set -a; [ -f .env ] && . ./.env; set +a;

.PHONY: help build-server build-cli test e2e lint up down logs migrate deploy provision-key mint-token build-caddy release

help: ## Show this help
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z0-9_-]+:.*##/{printf "  \033[36m%-14s\033[0m %s\n",$$1,$$2}' $(MAKEFILE_LIST)

build-server: ## Build the riftd server binary (server/riftd)
	cd server && CGO_ENABLED=0 go build -o riftd ./cmd/riftd

build-cli: ## Build the CLI single-binary image (deploy/Dockerfile.cli)
	docker build -f deploy/Dockerfile.cli -t rift-cli:local .

test: ## Run server tests
	cd server && go test ./...

e2e: ## Full black-box e2e in Docker: real Caddy, real TLS, real CLI (ARGS=--mode internal)
	bash tools/e2e.sh $(ARGS)

build-caddy: ## Build Caddy with the DNS-01 plugins from .env (ARGS=--validate --push)
	bash tools/build-caddy.sh $(ARGS)

release: ## Cross-compile rift release artifacts into dist/release/<version> (ARGS=--clean)
	bash tools/release.sh $(ARGS)

lint: ## Vet Go, syntax-check shell scripts, validate compose
	cd server && go vet ./...
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
