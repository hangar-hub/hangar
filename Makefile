# Hangar build, test and release tasks. Run `make help` for a list.

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

GO        ?= go
PNPM      ?= corepack pnpm
UV        ?= uv
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
BIN       := bin/hangar
IMAGE     ?= ghcr.io/jamesagarside/hangar:dev
GOTESTFLAGS ?= -race -count=1

# Postgres tests run with testcontainers (Docker). Set
# HANGAR_TEST_POSTGRES_DSN to use an existing server instead.
export HANGAR_TEST_POSTGRES ?= 1

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_.-]+:.*## / {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## Build -----------------------------------------------------------------------

.PHONY: build
build: ui go-build ## Build the UI and the hangar binary into bin/

.PHONY: go-build
go-build: ## Build the hangar binary with whatever UI is in internal/web/dist
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/hangar

.PHONY: ui
ui: ui-deps ## Build the web UI into internal/web/dist
	cd ui && $(PNPM) run build
	@touch internal/web/dist/.gitkeep

.PHONY: ui-deps
ui-deps:
	cd ui && $(PNPM) install --frozen-lockfile

.PHONY: dev
dev: ## Run the server on :8080 and the UI with live reload on :5173
	@mkdir -p tmp/dev
	@trap 'kill 0' EXIT; \
	$(GO) run ./cmd/hangar server --public-url http://localhost:5173 \
	  --set server.data_dir=tmp/dev --set database.dsn=tmp/dev/hangar.db \
	  --set storage.fs.path=tmp/dev/blobs --set git.cache_dir=tmp/dev/git-cache \
	  --set telemetry.log_format=text & \
	cd ui && $(PNPM) run dev

## Generated code ----------------------------------------------------------------

.PHONY: generate
generate: ## Regenerate the API code from api/openapi.yaml and the config reference
	$(GO) tool oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml
	cd ui && $(PNPM) run generate:api
	$(GO) run ./cmd/hangar config reference > docs/reference/configuration.md

.PHONY: check-generate
check-generate: generate ## Fail if generated files differ from what is committed
	@git diff --exit-code -- internal/api/gen ui/src/api/schema.d.ts docs/reference/configuration.md \
	  || (echo "Generated files are out of date. Run make generate and commit the result." && exit 1)

## Lint ------------------------------------------------------------------------------

.PHONY: lint
lint: lint-go lint-ui lint-style ## Run every linter

.PHONY: lint-go
lint-go:
	golangci-lint run ./...

.PHONY: lint-ui
lint-ui: ui-deps
	cd ui && $(PNPM) run lint && $(PNPM) run typecheck

# House style: no em dashes in code, UI copy or docs we write.
.PHONY: lint-style
lint-style:
	@if grep -rn --binary-files=without-match $$'\xe2\x80\x94' \
	    --include='*.go' --include='*.ts' --include='*.tsx' --include='*.md' --include='*.yaml' --include='*.yml' --include='*.py' \
	    --exclude-dir=node_modules --exclude-dir=.venv --exclude-dir=.fixtures --exclude-dir=dist --exclude-dir=gen \
	    cmd internal ui/src api compat docs/build docs/dependencies.md docs/reference README.md CLAUDE.md CONTEXT.md 2>/dev/null; then \
	  echo "House style: replace the em dashes above."; exit 1; fi

## Test ------------------------------------------------------------------------------

.PHONY: test
test: test-go test-ui ## Run unit and integration tests (SQLite and Postgres)

.PHONY: test-go
test-go:
	$(GO) test $(GOTESTFLAGS) -tags integration ./...

.PHONY: test-ui
test-ui: ui-deps
	cd ui && $(PNPM) run test

.PHONY: test-compat
test-compat: ## Run the client compatibility suites against a real binary
	$(GO) test -count=1 -tags compat ./compat/...
	$(UV) run --project compat pytest compat

.PHONY: fixtures
fixtures: ## Download the upstream test fixtures into compat/.fixtures
	$(UV) run --project compat python compat/fixtures.py

.PHONY: fuzz
fuzz: ## Run every fuzz target for FUZZTIME each (default 10m)
	./scripts/fuzz.sh $${FUZZTIME:-10m}

.PHONY: cover
cover: ## Check Go coverage per package against the floors in 13-testing.md
	./scripts/coverage.sh -tags integration

.PHONY: e2e
e2e: ui-deps ## Run Playwright against HANGAR_URL (default http://localhost:8080)
	cd ui && $(PNPM) exec playwright test

## Packaging ---------------------------------------------------------------------------

.PHONY: image
image: Dockerfile ## Build the container image for the local platform
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE) .

.PHONY: docs
docs: mkdocs.yml ## Build the documentation site
	mkdocs build --strict

.PHONY: clean
clean: ## Remove build output
	rm -rf bin coverage.out tmp
	find internal/web/dist -mindepth 1 ! -name .gitkeep -delete
