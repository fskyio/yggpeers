.DEFAULT_GOAL := help

GO ?= go
PYTHON ?= python3
NODE ?= node
NPM ?= npm
BINARY ?= build/yggpeers
PROBE_BINARY ?= build/yggpeers-probe
MAP_TOOLS_DIR ?= /tmp/yggpeers-map-tools
PLAYWRIGHT_VERSION ?= 1.58.2
MAP_ARGS ?=
GO_FILES := $(shell find . -type f -name '*.go' -not -path './.git/*' -not -path './vendor/*')

.PHONY: help build build-server build-probe run run-probe test vet lint fmt format fmt-check check clean map-build map-tools map-check

help: ## Show common development commands.
	@printf '%s\n' \
	  'build       Build the main instance and standalone probe.' \
	  'build-server Build the main binary at $(BINARY).' \
	  'build-probe Build the probe at $(PROBE_BINARY).' \
	  'run         Run the main instance with go run.' \
	  'run-probe   Run a standalone probe with go run.' \
	  'test        Run all Go tests.' \
	  'lint        Run go vet ./...' \
	  'fmt         Format Go source files.' \
	  'fmt-check   Check Go formatting without changing files.' \
	  'check       Run formatting, lint, and test checks.' \
	  'clean       Remove both built binaries.' \
	  'map-build   Regenerate the bundled PMTiles archive and catalog.' \
	  'map-tools   Install temporary Playwright browser-check tools.' \
	  'map-check   Run browser checks against an already-running app.'

build: build-server build-probe ## Build both executables.

build-server: ## Build the main application binary.
	@mkdir -p "$(dir $(BINARY))"
	$(GO) build -o "$(BINARY)" ./cmd/yggpeers

build-probe: ## Build the standalone probe without SQLite/CGO.
	@mkdir -p "$(dir $(PROBE_BINARY))"
	CGO_ENABLED=0 $(GO) build -o "$(PROBE_BINARY)" ./cmd/yggpeers-probe

run: ## Run the application directly from source.
	$(GO) run ./cmd/yggpeers $(ARGS)

run-probe: ## Run the standalone probe directly from source.
	$(GO) run ./cmd/yggpeers-probe $(ARGS)

test: ## Run all Go tests.
	$(GO) test ./...

vet: ## Run Go's static analysis checks.
	$(GO) vet ./...

lint: vet ## Alias for go vet ./...

fmt: ## Format Go source files in place.
	gofmt -w $(GO_FILES)

format: fmt ## Alias for fmt.

fmt-check: ## Fail if any Go source file needs formatting.
	@unformatted="$$(gofmt -l $(GO_FILES))"; \
	if [ -n "$$unformatted" ]; then \
	  printf 'Go files need formatting:\n%s\n' "$$unformatted"; \
	  exit 1; \
	fi

check: fmt-check lint test ## Run formatting, lint, and test checks.

clean: ## Remove the built executables.
	rm -f "$(BINARY)" "$(PROBE_BINARY)"

map-build: ## Regenerate bundled PMTiles assets (Tippecanoe 2.79.0 required).
	$(PYTHON) scripts/build-map.py $(MAP_ARGS)

map-tools: ## Install Playwright and Chromium under MAP_TOOLS_DIR.
	$(NPM) install --prefix "$(MAP_TOOLS_DIR)" playwright@$(PLAYWRIGHT_VERSION)
	"$(MAP_TOOLS_DIR)/node_modules/.bin/playwright" install chromium

map-check: ## Run browser checks; start the app with 'make run' first.
	NODE_PATH="$(MAP_TOOLS_DIR)/node_modules$${NODE_PATH:+:$$NODE_PATH}" $(NODE) scripts/check-map.cjs
