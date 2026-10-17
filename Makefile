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
# On a busy host one package can pass but take longer than go test's
# 10-minute default. The compat suites run real git, git-lfs and Python
# clients (compat/git took 168 s on a quiet host and over 600 s on a loaded
# one). The unit and integration tests in git-heavy packages slow down the
# same way: internal/datasets/pipeline took 29 s alone and over 600 s under
# load, and internal/upstream 350 s alone. scripts/check-test-timeouts.sh
# checks that every compat, integration and coverage run sets a timeout.
COMPAT_TIMEOUT ?= 30m
GO_TEST_TIMEOUT ?= 30m

# Postgres tests need Docker: test-go starts one Postgres container that
# every package shares (scripts/with-test-postgres.sh). Set
# HANGAR_TEST_POSTGRES_DSN to use an existing server instead.
export HANGAR_TEST_POSTGRES ?= 1
# S3 Blob store tests need Docker too: test-go starts one MinIO container
# that every package shares (scripts/with-test-minio.sh), so no package's
# MinIO depends on testcontainers' reaper. Set HANGAR_TEST_S3_ENDPOINT to
# use an existing S3-compatible server instead.
export HANGAR_TEST_MINIO ?= 1

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
generate: ## Regenerate the API code, the config, CLI and metrics references, and the dashboards
	$(GO) tool oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml
	cd ui && $(PNPM) run generate:api
	$(GO) run ./cmd/hangar config reference > docs/reference/configuration.md
	$(GO) run ./cmd/hangar reference cli > docs/reference/cli.md
	$(GO) run ./cmd/hangar reference metrics > docs/reference/metrics.md
	python3 scripts/hub-matrix.py
	python3 scripts/dashboards.py

GENERATED := internal/api/gen ui/src/api/schema.d.ts docs/reference/configuration.md docs/reference/cli.md \
  docs/reference/metrics.md docs/reference/hub-api-compatibility.md deploy/dashboards/grafana deploy/dashboards/kibana/hangar-dashboard.json

.PHONY: check-generate
check-generate: generate ## Fail if generated files differ from what is committed
	@git diff --exit-code -- $(GENERATED) \
	  || (echo "Generated files are out of date. Run make generate and commit the result." && exit 1)

## Lint ------------------------------------------------------------------------------

.PHONY: lint
lint: lint-go lint-ui lint-style ## Run every linter

.PHONY: lint-go
lint-go:
	golangci-lint run ./...
	# golangci-lint compiles one tag set; vet the compat backends it skips.
	$(GO) vet -tags compat,xetserver ./compat/xet
	$(GO) vet -tags compat,xetserver,xetreal ./compat/xet
	$(GO) vet -tags compat,w2real ./compat/hf

.PHONY: lint-ui
lint-ui: ui-deps
	cd ui && $(PNPM) run lint && $(PNPM) run typecheck

# House style: no em dashes in code, UI copy or docs we write, and British
# spellings in the docs (scripts/check-spelling.sh).
.PHONY: lint-style
lint-style:
	@if grep -rn --binary-files=without-match $$'\xe2\x80\x94' \
	    --include='*.go' --include='*.ts' --include='*.tsx' --include='*.md' --include='*.yaml' --include='*.yml' --include='*.py' \
	    --include='*.js' --include='*.sh' --include='*.json' --include='*.tpl' --include='*.txt' \
	    --exclude-dir=node_modules --exclude-dir=.venv --exclude-dir=.fixtures --exclude-dir=dist --exclude-dir=gen \
	    --exclude-dir=spec --exclude-dir=research \
	    cmd internal ui/src api compat docs deploy loadtest scripts .github README.md CLAUDE.md CONTEXT.md CONTRIBUTING.md SECURITY.md 2>/dev/null; then \
	  echo "House style: replace the em dashes above."; exit 1; fi
	@./scripts/check-spelling.sh
	@./scripts/check-test-timeouts.sh
	@./scripts/check-test-targets.sh
	@python3 scripts/check-hub-matrix.py

## Test ------------------------------------------------------------------------------

.PHONY: test
# Both suites run even when the first fails, so one run reports both
# results; the target fails if either does.
test: ## Run unit and integration tests (SQLite and Postgres)
	@status=0; \
	$(MAKE) --no-print-directory test-go || status=1; \
	$(MAKE) --no-print-directory test-ui || status=1; \
	exit $$status

.PHONY: test-go
test-go:
	scripts/with-test-postgres.sh scripts/with-test-minio.sh $(GO) test $(GOTESTFLAGS) -timeout $(GO_TEST_TIMEOUT) -tags integration ./...

.PHONY: test-ui
test-ui: ui-deps
	cd ui && $(PNPM) run test

.PHONY: test-compat
test-compat: ## Run the client compatibility suites against a real binary
	scripts/with-test-postgres.sh scripts/with-test-minio.sh $(GO) test -count=1 -timeout $(COMPAT_TIMEOUT) -tags compat ./compat/...
	$(UV) run --project compat pytest compat

.PHONY: fixtures
fixtures: ## Download the upstream test fixtures into compat/.fixtures
	$(UV) run --project compat python compat/fixtures.py

.PHONY: fuzz
fuzz: ## Run every fuzz target for FUZZTIME each (default 10m)
	./scripts/fuzz.sh $${FUZZTIME:-10m}

.PHONY: cover
cover: ## Check Go coverage per package against the floors in 13-testing.md
	scripts/with-test-postgres.sh scripts/with-test-minio.sh ./scripts/coverage.sh -timeout $(GO_TEST_TIMEOUT) -tags integration

.PHONY: e2e
e2e: ui-deps ## Run Playwright against the mock server
	cd ui && $(PNPM) exec playwright test

.PHONY: e2e-real
e2e-real: ui-deps ## Seed the running Hangar at HANGAR_URL and its Upstream, then run the e2e-real suite (see ui/playwright.real.config.ts)
	cd ui && $(PNPM) exec playwright test -c playwright.real.config.ts

## Packaging ---------------------------------------------------------------------------

DOCS_REQS := docs/requirements.txt
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: image
image: ## Build the container image for the local platform
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) -t $(IMAGE) .

.PHONY: image-multiarch
image-multiarch: ## Build the image for linux/amd64 and linux/arm64 (no push)
	docker buildx build --platform $(PLATFORMS) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
	  --build-arg DATE=$(DATE) -t $(IMAGE) .

KUBECONFORM = $(GO) run github.com/yannh/kubeconform/cmd/kubeconform@v0.7.0 \
	-strict -summary -kubernetes-version 1.33.0 -schema-location default \
	-schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'

.PHONY: release-rehearsal
release-rehearsal: image ## Rehearse the release's signing and SBOM steps with a throwaway key (needs cosign 3, syft, yq)
	./scripts/release-rehearsal.sh binaries
	./scripts/release-rehearsal.sh image $(IMAGE)

.PHONY: lint-deploy
lint-deploy: ## Lint the Helm chart, validate it with kubeconform, check the Compose files, the workflows' action refs and that jobs needing uv install it
	@# Offline renders cannot keep generated secrets, so the chart refuses to
	@# generate them; the CI values turn generation off here, and
	@# scripts/kind-chart-test.sh covers it on a cluster.
	helm lint deploy/helm/hangar -f deploy/helm/hangar/ci/default-values.yaml --set generateSecrets=false
	helm lint deploy/helm/hangar -f deploy/helm/examples/scale-values.yaml
	helm lint deploy/helm/hangar -f deploy/helm/examples/gitops-values.yaml
	@for values in deploy/helm/hangar/ci/default-values.yaml deploy/helm/examples/scale-values.yaml deploy/helm/examples/gitops-values.yaml; do \
	  helm template hangar deploy/helm/hangar -f $$values --set generateSecrets=false | $(KUBECONFORM); \
	done
	helm template hangar deploy/helm/hangar -f deploy/helm/examples/scale-values.yaml --set migrations.mode=initContainer | $(KUBECONFORM)
	@# The example values keep credentials in an existing Secret, so render the
	@# chart's own Secrets (secrets: in values) once more with a dummy value.
	helm template hangar deploy/helm/hangar -f deploy/helm/examples/scale-values.yaml \
	  --set existingSecret= --set generateSecrets=false --set secrets.HANGAR_DATABASE_DSN=postgres://lint@postgres/hangar | $(KUBECONFORM)
	@# Render the chart and check every Pod spec (deploy/chart_test.go).
	$(GO) test -count=1 -tags helm -run '^TestChart' ./deploy/
	@for f in deploy/compose/*.yaml; do HANGAR_INSTANCE_KEY=x HANGAR_ADMIN_PASSWORD=x POSTGRES_PASSWORD=x MINIO_ROOT_PASSWORD=x \
	  KEYCLOAK_ADMIN_PASSWORD=x HANGAR_OIDC_CLIENT_SECRET=x docker compose -f $$f config -q && echo "$$f ok"; done
	goreleaser check
	shellcheck scripts/*.sh loadtest/*.sh
	./scripts/check-action-refs.sh
	./scripts/check-workflow-uv.sh

.PHONY: test-kind
test-kind: ## Install the Helm chart on a throwaway kind cluster and run deploy/kind_test.go (IMAGE: a local Hangar image)
	./scripts/kind-chart-test.sh $(IMAGE)

.PHONY: docs
docs: ## Build the documentation site into site/ (needs uv)
	$(UV) run --no-project --with-requirements $(DOCS_REQS) mkdocs build --strict

.PHONY: docs-serve
docs-serve: ## Serve the documentation with live reload on :8000
	$(UV) run --no-project --with-requirements $(DOCS_REQS) mkdocs serve

.PHONY: smoke
smoke: ## Run the smoke test against HANGAR_URL
	./scripts/smoke.sh $${HANGAR_URL:-http://localhost:8080}

.PHONY: clean
clean: ## Remove build output
	rm -rf bin coverage.out tmp site dist
	find internal/web/dist -mindepth 1 ! -name .gitkeep -delete
