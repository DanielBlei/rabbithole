BINARY   := rabbithole
CONFIG   ?= ./configs/config.example.yaml
PKG      := ./...
NO_THINK ?=
THINK_FLAG := $(if $(NO_THINK),--no-think,)
ADDR     ?= :8080
DEBUG    ?=
DEBUG_FLAG := $(if $(DEBUG),--debug,)

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

# Nudge shown when a target still runs on the shipped example config, which ranks against project generic configurations
.PHONY: config-hint
config-hint:
	@case "$(CONFIG)" in *example*) \
	  printf '\033[33m▌\033[0m\n'; \
	  printf '\033[33m▌\033[0m Running on \033[36m$(CONFIG)\033[0m —> the shipped defaults.\n'; \
	  printf '\033[33m▌\033[0m\n'; \
	  printf '\033[33m▌\033[0m The Rabbit Hole ranks what it reads against your interests.\n'; \
	  printf '\033[33m▌\033[0m\n'; \
	  printf '\033[33m▌\033[0m   1. \033[36mmake setup\033[0m            your own config, feeds and profile\n'; \
	  printf '\033[33m▌\033[0m   2. \033[36mconfigs/feeds.yaml\033[0m    the RSS feeds to pull from\n'; \
	  printf '\033[33m▌\033[0m   3. \033[36mconfigs/profile.md\033[0m    what you care about, in your words\n'; \
	  printf '\033[33m▌\033[0m\n'; \
	  printf '\033[33m▌\033[0m Then: \033[36mCONFIG=./configs/config.yaml make serve\033[0m (or edit CONFIG in the Makefile)\n'; \
	  printf '\033[33m▌\033[0m\n'; \
	  printf '\033[33m▌\033[0m Details: \033[36mdocs/configuration.md\033[0m\n'; \
	  printf '\033[33m▌\033[0m\n'; \
	esac

.PHONY: setup
setup: ## Create config.yaml, feeds.yaml and profile.md from the examples (if missing)
	@test -f configs/config.yaml  || ( \
	  cp configs/config.example.yaml configs/config.yaml && \
	  sed -i -e 's|profile.example.md|profile.md|' -e 's|feeds.example.yaml|feeds.yaml|' configs/config.yaml && \
	  echo "created configs/config.yaml (pointing at your profile.md and feeds.yaml)" )
	@test -f configs/feeds.yaml   || (cp configs/feeds.example.yaml   configs/feeds.yaml   && echo "created configs/feeds.yaml")
	@test -f configs/profile.md   || (cp configs/profile.example.md   configs/profile.md   && echo "created configs/profile.md")

##@ Build

.PHONY: build
build: ## Compile the binary to ./rabbithole
	go build -o $(BINARY) .

.PHONY: tidy
tidy: ## Sync go.mod/go.sum
	go mod tidy

.PHONY: clean
clean: ## Remove build artifacts (DATA=1 also deletes ./data — the database and digests)
	rm -f $(BINARY) coverage.out
	@if [ -n "$(DATA)" ] && [ -d data ]; then \
	  printf 'Delete ./data (database, digests)? This cannot be undone. [y/N] '; \
	  read ans; case "$$ans" in [yY]*) rm -rf data; echo "removed ./data";; *) echo "kept";; esac; \
	fi

##@ ingest

.PHONY: ingest
ingest: config-hint ## Fetch, rank, record, and write today's markdown ingest (uses CONFIG, NO_THINK=1 to disable thinking)
	go run . ingest --config $(CONFIG) --markdown $(THINK_FLAG)

.PHONY: db-only
db-only: config-hint ## Fetch, rank, and record to the store only (no markdown file)
	go run . ingest --config $(CONFIG) $(THINK_FLAG)

.PHONY: dry-run
dry-run: config-hint ## Print the ingest to stdout without writing files or recording items
	go run . ingest --config $(CONFIG) --dry-run $(THINK_FLAG)

.PHONY: heuristic
heuristic: config-hint ## Offline ingest with the model-free keyword scorer (no Ollama needed)
	go run . ingest --config $(CONFIG) --provider heuristic --markdown $(THINK_FLAG)

##@ Serve

.PHONY: serve
serve: config-hint ## Serve the web UI and items API over HTTP (uses CONFIG, ADDR; DEBUG=1 for verbose ingest logging)
	go run . serve --config $(CONFIG) --addr $(ADDR) $(DEBUG_FLAG)

##@ Test

.PHONY: test
test: ## Run the test suite
	go test $(PKG)

.PHONY: test-v
test-v: ## Run the test suite verbosely
	go test -v $(PKG)

.PHONY: test-race
test-race: ## Run tests with the race detector
	go test -race $(PKG)

.PHONY: cover
cover: ## Run tests and open an HTML coverage report
	go test -coverprofile=coverage.out $(PKG)
	go tool cover -html=coverage.out

##@ Quality

.PHONY: vet
vet: ## Run go vet
	go vet $(PKG)

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -w -s .

NEED_GOLANGCI = command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint is required (CI runs it)."; echo "install: https://golangci-lint.run/welcome/install/"; exit 1; }

.PHONY: lint
lint: ## Run golangci-lint (also covers gofmt/goimports/golines and vet)
	@$(NEED_GOLANGCI)
	golangci-lint run

.PHONY: lint-fix
lint-fix: ## Run golangci-lint with --fix (formats + autofixes lint issues)
	@$(NEED_GOLANGCI)
	golangci-lint run --fix

# Unpinned: a stale scanner defeats the purpose.
GOVULNCHECK ?= go run golang.org/x/vuln/cmd/govulncheck@latest

.PHONY: vulncheck
vulncheck: ## Scan dependencies for known vulnerabilities
	$(GOVULNCHECK) $(PKG)

.PHONY: check
check: lint test-race build ## The gate CI runs (run before committing; needs golangci-lint)

##@ Debug

.PHONY: debug
debug: ## Run the ingest with verbose logging
	go run . ingest --config $(CONFIG) --debug $(THINK_FLAG)

.PHONY: trace
trace: ## Run the ingest with trace logging (raw model prompts/responses)
	go run . ingest --config $(CONFIG) --trace $(THINK_FLAG)

.PHONY: db-dump-items
db-dump: ## Dump the items table via the sqlite3 CLI (requires DB=path)
	@test -n "$(DB)" || { echo "usage: make db-dump DB=./data/rabbithole.db" >&2; exit 1; }
	sqlite3 -header -column $(DB) "SELECT id, source, title, status, llm_score, user_score, digested_on, created_at FROM items ORDER BY created_at;"


