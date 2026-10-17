BINARY   := rabbithole
# A config file to import on start (optional; settings live in the store).
CONFIG   ?=
CONFIG_FLAG := $(if $(CONFIG),--config $(CONFIG),)
PKG      := ./...
NO_THINK ?=
THINK_FLAG := $(if $(NO_THINK),--no-think,)
ADDR     ?= 127.0.0.1:8080
DEBUG    ?=
DEBUG_FLAG := $(if $(DEBUG),--debug,)
DEV      ?=
DEV_FLAG := $(if $(DEV),--dev,)
GOLDEN   ?= ./docs/examples/golden.yaml
# Repo work keeps its store here, so it never touches the personal one in the user
# data dir and never sees the legacy-location warning. A RABBITHOLE_DB in the
# shell is respected (the binary reads it), and DB= on the command line wins over
# both. make does not read .env; the binary does.
DB       ?= ./data/rabbithole.db
ENV_DB   := $(and $(filter file,$(origin DB)),$(RABBITHOLE_DB))
DB_FLAG  := $(if $(ENV_DB),,--db '$(value DB)')
DB_FROM  := $(if $(ENV_DB),from your shell environment,$(if $(filter file,$(origin DB)),the repo default,from DB=))
FORMAT   ?= text

# Nearest git tag plus any commits past it.
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -X github.com/DanielBlei/rabbithole/cmd.version=$(VERSION)

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

# Says where a make target keeps its data, and how to change it.
USER_DATA_DIR := $(if $(filter Darwin,$(shell uname -s)),~/Library/Application Support/rabbithole,~/.local/share/rabbithole)
.PHONY: db-hint
db-hint:
	@printf '\033[33m▌\033[0m make keeps your data in \033[36m$(if $(ENV_DB),the store RABBITHOLE_DB names,$(DB))\033[0m ($(DB_FROM))\n'
	@printf '\033[33m▌\033[0m   Use another local store:  \033[36mmake $(or $(firstword $(MAKECMDGOALS)),serve) DB=/path/to/rabbithole.db\033[0m\n'
	@printf '\033[33m▌\033[0m   Use Postgres:             \033[36mmake $(or $(firstword $(MAKECMDGOALS)),serve) DB=postgres://user@host:5432/rabbithole\033[0m\n'
	@printf '\033[33m▌\033[0m                             with \033[36mRABBITHOLE_DB_PASSWORD\033[0m set in .env or your shell profile\n'
	@printf '\033[33m▌\033[0m                             (a password in the URL is accepted, but not recommended)\n'
	@printf '\033[33m▌\033[0m   Run without make:         \033[36m./$(BINARY)\033[0m uses \033[36m$(USER_DATA_DIR)\033[0m, or RABBITHOLE_DB when set\n'

.PHONY: setup
setup: ## Create .env from .env.example (if missing); the binary reads it itself
	@test -f .env || (cp .env.example .env && chmod 600 .env && echo "created .env (the binary reads it; RABBITHOLE_* keys only)")

##@ Build

.PHONY: build
build: ## Compile the binary to ./rabbithole
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

.PHONY: tidy
tidy: ## Sync go.mod/go.sum
	go mod tidy

.PHONY: clean
clean: ## Remove build artifacts (DATA=1 also deletes ./data: the database and digests)
	rm -f $(BINARY) coverage.out
	@if [ -n "$(DATA)" ] && [ -d data ]; then \
	  printf 'Delete ./data (database, digests)? This cannot be undone. [y/N] '; \
	  read ans; case "$$ans" in [yY]*) rm -rf data; echo "removed ./data";; *) echo "kept";; esac; \
	fi

.PHONY: clean-feeds
clean-feeds: ## Delete every feed item from the store, keeping todos, ideas and run history (CONFIG optional)
	@printf 'Delete every feed item from the store?\n'
	@printf 'Bookmarks, ratings and notes go too. Todos, ideas and run history are kept.\n'
	@printf 'The next ingest re-fetches and re-scores anything still in your feeds. [y/N] '
	@read ans; case "$$ans" in \
	  [yY]*) go run . items prune --all --include-saved $(DB_FLAG) $(CONFIG_FLAG);; \
	  *) echo "kept";; \
	esac

##@ ingest

.PHONY: sources
sources: db-hint build ## Add the example sources (docs/examples/feeds.yaml) to the store; repeat-safe
	./$(BINARY) sources import docs/examples/feeds.yaml $(DB_FLAG)

.PHONY: ingest
ingest: db-hint ## Fetch, rank, and record to the store (CONFIG optional, NO_THINK=1 to disable thinking)
	go run . ingest $(DB_FLAG) $(CONFIG_FLAG) $(THINK_FLAG) $(DEBUG_FLAG)

.PHONY: dry-run
dry-run: db-hint ## Print the ingest to stdout without writing files or recording items
	go run . ingest $(DB_FLAG) $(CONFIG_FLAG) --dry-run $(THINK_FLAG)

.PHONY: heuristic
heuristic: db-hint ## Offline ingest with the model-free keyword scorer (no Ollama needed)
	go run . ingest $(DB_FLAG) $(CONFIG_FLAG) --provider heuristic $(THINK_FLAG)

##@ Eval

.PHONY: eval
eval: ## Benchmark scoring against GOLDEN (GOLDEN, FORMAT; CONFIG optional)
	go run . eval benchmark $(DB_FLAG) $(GOLDEN) $(CONFIG_FLAG) --format $(FORMAT) $(THINK_FLAG)

.PHONY: eval-example
eval-example: ## Benchmark the shipped example dataset with the model-free scorer (no Ollama needed)
	go run . eval benchmark $(DB_FLAG) docs/examples/golden.yaml $(CONFIG_FLAG) --provider heuristic

.PHONY: eval-smoke
eval-smoke: ## Render every report format from the example dataset, model-free
	@for format in text markdown json; do \
	  printf '\033[36m==> %s\033[0m\n' "$$format"; \
	  go run . eval benchmark $(DB_FLAG) docs/examples/golden.yaml --config docs/examples/config.yaml \
	    --provider heuristic --format "$$format" > /dev/null || exit 1; \
	done
	@go run . eval benchmark $(DB_FLAG) docs/examples/golden.yaml --config docs/examples/config.yaml \
	  --provider heuristic | awk 'length > 120 { print "line over 120 columns: " $$0; fail=1 } END { exit fail }'

##@ Serve

.PHONY: serve
serve: db-hint build ## Serve the web UI and items API over HTTP (CONFIG optional, ADDR; DEBUG=1 for verbose ingest logging, DEV=1 to serve assets no-cache)
	./$(BINARY) serve $(DB_FLAG) $(CONFIG_FLAG) --addr $(ADDR) $(DEBUG_FLAG) $(DEV_FLAG)

##@ Test

.PHONY: test
test: ## Run the test suite
	@RABBITHOLE_DB= RABBITHOLE_DB_PASSWORD= go test $(PKG)

.PHONY: test-v
test-v: ## Run the test suite verbosely
	@RABBITHOLE_DB= RABBITHOLE_DB_PASSWORD= go test -v $(PKG)

.PHONY: test-race
test-race: ## Run tests with the race detector
	@RABBITHOLE_DB= RABBITHOLE_DB_PASSWORD= go test -race $(PKG)

.PHONY: bench
bench: ## Run ranking and store benchmarks
	@RABBITHOLE_DB= RABBITHOLE_DB_PASSWORD= go test -bench=. -benchmem ./internal/rank ./internal/store

.PHONY: cover
cover: ## Run tests and open an HTML coverage report
	@RABBITHOLE_DB= RABBITHOLE_DB_PASSWORD= go test -coverprofile=coverage.out $(PKG)
	@RABBITHOLE_DB= RABBITHOLE_DB_PASSWORD= go tool cover -html=coverage.out

# The suite runs on SQLite by default and needs nothing installed. These three
# run the same tests a second time against Postgres, which is opt-in: `check`
# does not depend on them, and neither does CI.

.PHONY: pg-up
pg-up: ## Start a throwaway Postgres for the store tests
	@./scripts/dev-postgres.sh up

.PHONY: pg-down
pg-down: ## Stop the throwaway Postgres and drop its data
	@./scripts/dev-postgres.sh down

.PHONY: test-pg
test-pg: ## Run the store suite against the throwaway Postgres
	@./scripts/dev-postgres.sh status >/dev/null || { echo "not running; start it with 'make pg-up'" >&2; exit 1; }
	@RABBITHOLE_DB= RABBITHOLE_TEST_POSTGRES="$$(./scripts/dev-postgres.sh dsn)" go test -count=1 ./internal/store/...

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
	go run . ingest $(DB_FLAG) $(CONFIG_FLAG) --debug $(THINK_FLAG)

.PHONY: trace
trace: ## Run the ingest with trace logging (raw model prompts/responses)
	go run . ingest $(DB_FLAG) $(CONFIG_FLAG) --trace $(THINK_FLAG)

.PHONY: db-dump-items
db-dump: ## Dump the items table via the sqlite3 CLI (uses DB)
	sqlite3 -header -column $(DB) "SELECT id, source, title, status, llm_score, user_score, digested_on, created_at FROM items ORDER BY created_at;"

