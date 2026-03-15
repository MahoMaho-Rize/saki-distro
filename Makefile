# ============================================================
#  Claw Distro — Makefile
# ============================================================

GOFLAGS := -trimpath

.PHONY: build
build: ## Build all binaries
	CGO_ENABLED=0 go build $(GOFLAGS) -o bin/claw-fs ./cmd/claw-fs
	CGO_ENABLED=0 go build $(GOFLAGS) -o bin/claw-exec ./cmd/claw-exec
	CGO_ENABLED=0 go build $(GOFLAGS) -o bin/claw-web ./cmd/claw-web
	CGO_ENABLED=0 go build $(GOFLAGS) -o bin/claw-browser ./cmd/claw-browser
	CGO_ENABLED=0 go build $(GOFLAGS) -o bin/claw ./cmd/claw
	CGO_ENABLED=0 go build $(GOFLAGS) -o bin/context-mgr ./hooks/context-mgr

.PHONY: test
test: ## Run all tests
	go test ./... -count=1 -race -timeout 60s

.PHONY: test-v
test-v: ## Run all tests (verbose)
	go test ./... -count=1 -race -timeout 60s -v

.PHONY: check
check: ## Build + vet
	go build ./...
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Format
	gofmt -s -w .

.PHONY: tidy
tidy: ## Tidy go.mod
	go mod tidy

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin/

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := build
