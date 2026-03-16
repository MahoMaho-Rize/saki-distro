# ============================================================
#  Claw Distro — Makefile
# ============================================================
#
#  Distro flavors:
#    make build       — full build (all 4 MCP servers + CLI + hooks)
#    make build-core  — core only (fs + exec + CLI + hooks, no browser/web)
#    make build-lite  — minimal (fs + exec only, no CLI/hooks)
# ============================================================

GOFLAGS := -trimpath -ldflags '-s -w'

# ── Core binaries (always included) ─────────────────────────

CORE_BINS := bin/claw-fs bin/claw-exec

bin/claw-fs: cmd/claw-fs/main.go
	CGO_ENABLED=0 go build $(GOFLAGS) -o $@ ./cmd/claw-fs

bin/claw-exec: cmd/claw-exec/main.go
	CGO_ENABLED=0 go build $(GOFLAGS) -o $@ ./cmd/claw-exec

# ── Extended binaries (optional) ─────────────────────────────

EXT_BINS := bin/claw-web bin/claw-browser

bin/claw-web: cmd/claw-web/main.go
	CGO_ENABLED=0 go build $(GOFLAGS) -o $@ ./cmd/claw-web

bin/claw-browser: cmd/claw-browser/main.go
	CGO_ENABLED=0 go build $(GOFLAGS) -o $@ ./cmd/claw-browser

# ── CLI + hooks ──────────────────────────────────────────────

TOOL_BINS := bin/claw bin/context-mgr bin/tool-guard

bin/claw: cmd/claw/main.go
	CGO_ENABLED=0 go build $(GOFLAGS) -o $@ ./cmd/claw

bin/context-mgr: hooks/context-mgr/main.go
	CGO_ENABLED=0 go build $(GOFLAGS) -o $@ ./hooks/context-mgr

bin/tool-guard: hooks/tool-guard/main.go
	CGO_ENABLED=0 go build $(GOFLAGS) -o $@ ./hooks/tool-guard

# ── Distro flavors ───────────────────────────────────────────

.PHONY: build build-core build-lite
build: $(CORE_BINS) $(EXT_BINS) $(TOOL_BINS) ## Full build (all binaries)
build-core: $(CORE_BINS) $(TOOL_BINS) ## Core: fs + exec + CLI + hooks (no browser/web)
build-lite: $(CORE_BINS) ## Lite: fs + exec only (minimal)

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
