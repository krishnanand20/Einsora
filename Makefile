SHELL := /bin/bash
.DEFAULT_GOAL := help

# Local overrides (never committed). Exported so `go run`, `uv run`, and `npm` see them.
-include .env
export

CP  := controlplane
ML  := ml
WEB := web

# macOS can mark ml/.venv hidden (seen with repos in iCloud-synced folders), and Python 3.12+ then
# ignores its .pth files, so `import einsora_ml` fails. Clear the flag before running Python tools.
UNHIDE_VENV   = if [ "$$(uname)" = Darwin ] && [ -d $(ML)/.venv ]; then chflags -R nohidden $(ML)/.venv; fi

PG_URL        ?= postgres://einsora:einsora@localhost:5432/einsora?sslmode=disable
GOLANGCI_LINT ?= golangci-lint
# CRD schemas (KubeVirt, KServe, Gateway API, cert-manager, ...) from the community catalog.
CRD_SCHEMAS   := https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json
KUBECONFORM   := kubeconform -strict -summary -ignore-missing-schemas -schema-location default -schema-location '$(CRD_SCHEMAS)'
IMAGE_REPO    ?= ghcr.io/your-org
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------- setup
REQUIRED_TOOLS := go uv node npm
OPTIONAL_TOOLS := pre-commit docker helm kubeconform golangci-lint

.PHONY: doctor setup
doctor: ## Check that the tools Einsora needs are installed (macOS or Linux)
	@missing=""; \
	for t in $(REQUIRED_TOOLS); do \
	  if command -v $$t >/dev/null 2>&1; then printf '  ok        %s\n' "$$t"; \
	  else printf '  MISSING   %s\n' "$$t"; missing="$$missing $$t"; fi; \
	done; \
	for t in $(OPTIONAL_TOOLS); do \
	  if command -v $$t >/dev/null 2>&1; then printf '  ok        %s (optional)\n' "$$t"; \
	  else printf '  -         %s (optional: hooks and Datacenter checks)\n' "$$t"; fi; \
	done; \
	if [ -n "$$missing" ]; then \
	  echo; echo "Missing required tools:$$missing"; \
	  if [ "$$(uname)" = Darwin ]; then echo "Install them with:  brew install go uv node"; \
	  else echo "Go: https://go.dev/dl   uv: curl -LsSf https://astral.sh/uv/install.sh | sh   Node 22+: https://nodejs.org"; fi; \
	  echo "Then re-run:        make setup"; exit 1; \
	fi

setup: doctor ## Install dev dependencies for all components
	cd $(CP) && go mod download
	cd $(ML) && uv sync
	@$(UNHIDE_VENV)
	cd $(WEB) && npm install
	cd site && npm install
	-pre-commit install

# ---------------------------------------------------------------- website
.PHONY: site site-dev
site: ## Build the website (site/) into site/dist from the repo's docs; fails on a broken link
	cd site && npm run build

site-dev: ## Website dev server: http://localhost:4321/Einsora/
	cd site && npm run dev

# ---------------------------------------------------------------- local dev
.PHONY: dev dev-pg web-dev migrate
dev: ## All-in-one local stack: API+controller+usage accounting, in-memory store, fake backend (:8080; macOS or Linux)
	cd $(CP) && go run ./cmd/einsora dev

dev-pg: ## Same as dev, but against Postgres in Docker
	docker compose up -d --wait postgres
	cd $(CP) && EINSORA_DATABASE_URL='$(PG_URL)' go run ./cmd/einsora migrate
	cd $(CP) && EINSORA_DATABASE_URL='$(PG_URL)' go run ./cmd/einsora dev

web-dev: ## Dashboard dev server on :5173 (proxies /v1 to :8080)
	cd $(WEB) && npm run dev

migrate: ## Apply DB migrations to $$EINSORA_DATABASE_URL
	cd $(CP) && go run ./cmd/einsora migrate

# ---------------------------------------------------------------- test
.PHONY: test test-cp test-cp-pg test-ml test-web
test: test-cp test-ml test-web ## Run all tests

test-cp: ## Go tests (in-memory store; Postgres tests skip without EINSORA_TEST_DATABASE_URL)
	cd $(CP) && go test ./...

test-cp-pg: ## Go tests including Postgres store tests
	docker compose up -d --wait postgres
	cd $(CP) && EINSORA_TEST_DATABASE_URL='$(PG_URL)' go test ./...

test-ml: ## Python ML layer tests
	@$(UNHIDE_VENV)
	cd $(ML) && uv run pytest

test-web: ## Dashboard tests
	cd $(WEB) && npm test

# ---------------------------------------------------------------- lint / format
.PHONY: lint lint-cp lint-ml lint-web lint-yaml fmt
lint: lint-cp lint-ml lint-web lint-yaml ## Run all linters

lint-cp:
	cd $(CP) && test -z "$$(gofmt -l .)" || (gofmt -l . && echo 'run: make fmt' && exit 1)
	cd $(CP) && go vet ./... && GOOS=linux go vet ./...
	cd $(CP) && $(GOLANGCI_LINT) run
	cd $(CP) && GOOS=linux $(GOLANGCI_LINT) run # Linux-only files (drivers), even on a Mac

lint-ml:
	@$(UNHIDE_VENV)
	cd $(ML) && uv run ruff check . && uv run ruff format --check . && uv run mypy

lint-web:
	cd $(WEB) && npm run typecheck

lint-yaml:
	uvx yamllint -c .yamllint.yaml .

fmt: ## Format all code
	cd $(CP) && gofmt -w .
	@$(UNHIDE_VENV)
	cd $(ML) && uv run ruff format . && uv run ruff check --fix .

# ---------------------------------------------------------------- deploy (render only, never applies)
.PHONY: deploy-check
deploy-check: ## Lint + render charts and validate with kubeconform (no cluster needed)
	helm lint deploy/charts/einsora --set-file catalog=deploy/catalog.yaml
	helm lint deploy/charts/cluster-config -f deploy/environments/single-node.yaml
	helm template einsora deploy/charts/einsora --set-file catalog=deploy/catalog.yaml | $(KUBECONFORM)
	@# A region missing from the catalog must fail to render, not leave workloads pending.
	@! helm template einsora deploy/charts/einsora --set-file catalog=deploy/catalog.yaml --set cluster.region=nowhere >/dev/null 2>&1 \
		|| (echo "the chart rendered a region that isn't in the catalog" && exit 1)
	for env in single-node production; do \
	  helm template cluster-config deploy/charts/cluster-config -f deploy/environments/$$env.yaml | $(KUBECONFORM) || exit 1; \
	done
	$(KUBECONFORM) deploy/examples

# ---------------------------------------------------------------- cross-platform builds (AGENTS.md rule 11)
PLATFORMS := linux/amd64 linux/arm64 darwin/arm64

.PHONY: build-all
build-all: ## Cross-compile einsora for linux/amd64, linux/arm64, darwin/arm64 into controlplane/bin/
	cd $(CP) && for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  echo "building einsora for $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -o bin/einsora-$$os-$$arch ./cmd/einsora || exit 1; \
	done

# ---------------------------------------------------------------- images
.PHONY: images
images: ## Build container images for controlplane, ml, web
	docker build -t $(IMAGE_REPO)/einsora-controlplane:$(VERSION) $(CP)
	docker build -t $(IMAGE_REPO)/einsora-ml:$(VERSION) $(ML)
	docker build -t $(IMAGE_REPO)/einsora-web:$(VERSION) $(WEB)
