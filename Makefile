BINARY  := ai-searcher
CONFIG  ?= ./configs/config.example.yaml
PKG     := ./...

.DEFAULT_GOAL := help

## help: list available targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /' | sort

## setup: create config.yaml and profile.md from the examples (if missing)
.PHONY: setup
setup:
	@test -f configs/config.yaml  || (cp configs/config.example.yaml  configs/config.yaml  && echo "created configs/config.yaml")
	@test -f configs/profile.md   || (cp configs/profile.example.md   configs/profile.md   && echo "created configs/profile.md")

## build: compile the binary to ./$(BINARY)
.PHONY: build
build:
	go build -o $(BINARY) .

## run: fetch, rank, and write today's digest (uses CONFIG)
.PHONY: run
run:
	go run . run --config $(CONFIG)

## dry-run: print the digest to stdout without writing files or recording items
.PHONY: dry-run
dry-run:
	go run . run --config $(CONFIG) --dry-run

## heuristic: offline run with the model-free keyword scorer (no Ollama needed)
.PHONY: heuristic
heuristic:
	go run . run --config $(CONFIG) --provider heuristic

## debug: run with verbose logging
.PHONY: debug
debug:
	go run . run --config $(CONFIG) --debug

## test: run the test suite
.PHONY: test
test:
	go test $(PKG)

## test-v: run the test suite verbosely
.PHONY: test-v
test-v:
	go test -v $(PKG)

## test-race: run tests with the race detector
.PHONY: test-race
test-race:
	go test -race $(PKG)

## cover: run tests and open an HTML coverage report
.PHONY: cover
cover:
	go test -coverprofile=coverage.out $(PKG)
	go tool cover -html=coverage.out

## vet: run go vet
.PHONY: vet
vet:
	go vet $(PKG)

## fmt: format all Go source
.PHONY: fmt
fmt:
	gofmt -w -s .

## lint: run golangci-lint if installed, else fall back to vet
.PHONY: lint
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run; else echo "golangci-lint not found; running go vet"; go vet $(PKG); fi

## tidy: sync go.mod/go.sum
.PHONY: tidy
tidy:
	go mod tidy

## check: fmt + vet + test (run before committing)
.PHONY: check
check: fmt vet test

## clean: remove the binary, coverage, and generated data
.PHONY: clean
clean:
	rm -f $(BINARY) coverage.out
	rm -rf data
