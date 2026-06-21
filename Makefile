BINARY   := ai-searcher
CONFIG   ?= ./configs/config.example.yaml
PKG      := ./...
NO_THINK ?=
THINK_FLAG := $(if $(NO_THINK),--no-think,)
ADDR     ?= :8080

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

.PHONY: setup
setup: ## Create config.yaml and profile.md from the examples (if missing)
	@test -f configs/config.yaml  || (cp configs/config.example.yaml  configs/config.yaml  && echo "created configs/config.yaml")
	@test -f configs/profile.md   || (cp configs/profile.example.md   configs/profile.md   && echo "created configs/profile.md")

##@ Build

.PHONY: build
build: ## Compile the binary to ./ai-searcher
	go build -o $(BINARY) .

.PHONY: tidy
tidy: ## Sync go.mod/go.sum
	go mod tidy

.PHONY: clean
clean: ## Remove the binary, coverage, and generated data
	rm -f $(BINARY) coverage.out
	rm -rf data

##@ Run

.PHONY: run
run: ## Fetch, rank, and write today's digest (uses CONFIG, NO_THINK=1 to disable thinking)
	go run . run --config $(CONFIG) $(THINK_FLAG)

.PHONY: dry-run
dry-run: ## Print the digest to stdout without writing files or recording items
	go run . run --config $(CONFIG) --dry-run $(THINK_FLAG)

.PHONY: heuristic
heuristic: ## Offline run with the model-free keyword scorer (no Ollama needed)
	go run . run --config $(CONFIG) --provider heuristic $(THINK_FLAG)

.PHONY: serve
serve: ## Serve the items API over HTTP (uses CONFIG, ADDR; JSON only, no frontend yet)
	go run . serve --config $(CONFIG) --addr $(ADDR)

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

.PHONY: lint
lint: ## Run golangci-lint if installed, else fall back to vet
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run; else echo "golangci-lint not found; running go vet"; go vet $(PKG); fi

.PHONY: lint-fix
lint-fix: ## Run golangci-lint with --fix (formats + autofixes lint issues)
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not found"; exit 1; }
	golangci-lint run --fix

.PHONY: check
check: fmt vet test ## fmt + vet + test (run before committing)

##@ Debug

.PHONY: debug
debug: ## Run with verbose logging
	go run . run --config $(CONFIG) --debug $(THINK_FLAG)

.PHONY: trace
trace: ## Run with trace logging (raw model prompts/responses)
	go run . run --config $(CONFIG) --trace $(THINK_FLAG)

.PHONY: db-dump
db-dump: ## Dump the items table via the sqlite3 CLI (requires DB=path)
	@test -n "$(DB)" || { echo "usage: make db-dump DB=./data/ai-searcher.db" >&2; exit 1; }
	sqlite3 -header -column $(DB) "SELECT id, source, title, status, llm_score, user_score, digested_on, created_at FROM items ORDER BY created_at;"


