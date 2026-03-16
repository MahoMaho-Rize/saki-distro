# AGENTS.md — Claw Distro

## Project Overview

Claw Distro is a coding agent tool runtime written in **Go 1.26**. It provides
MCP (Model Context Protocol) tool servers for an LLM-based coding agent running
on the TAG Gateway platform. The codebase has zero JavaScript/TypeScript — it is
pure Go with shell scripts for integration tests.

### Architecture

- **4 MCP tool servers**: `claw-fs` (:9100), `claw-exec` (:9101), `claw-web` (:9102), `claw-browser` (:9103)
- **2 ext_proc hooks**: `context-mgr` (context window management), `tool-guard` (security)
- **1 CLI client**: `claw` (interactive/single-shot)
- **Internal packages**: `internal/mcpserver`, `internal/workspace`, `internal/safenet`

## Build / Lint / Test Commands

```bash
# Build all binaries (static, CGO_ENABLED=0)
make build

# Run all unit tests (with race detector, 60s timeout)
make test

# Run all unit tests with verbose output
make test-v

# Run a single test by name
go test ./internal/workspace/... -run TestResolve_AllowsPathsUnderRoot -count=1 -race

# Run all tests in a single package
go test ./cmd/claw-fs/... -count=1 -race -timeout 60s

# Build + vet (quick check without linting)
make check

# Run golangci-lint (14 linters enabled)
make lint

# Format all Go files
make fmt

# Tidy go.mod
make tidy
```

### Integration / E2E Tests (shell-based)

```bash
# MCP protocol compliance + core tool pipeline (requires built binaries)
scripts/graytest.sh

# Full ReAct loop (requires ANTHROPIC_AUTH_TOKEN + TAG Gateway binary)
scripts/integration-test.sh

# Multi-turn session persistence test
scripts/multiturn-test.sh

# Complete stack: shadow layer, sessions, all tools, security
scripts/e2e-test.sh
```

## Code Style Guidelines

### Formatting

- Run `gofmt -s -w .` (via `make fmt`). No other formatter is used.
- No `.editorconfig` — rely on `gofmt` defaults (tabs for indentation).

### Imports

Two groups separated by a blank line: stdlib first, then internal/third-party.
Within each group, imports are sorted alphabetically. No import aliases.

```go
import (
    "context"
    "encoding/json"
    "fmt"

    "claw-distro/internal/mcpserver"
    "claw-distro/internal/workspace"
)
```

### Naming Conventions

- **Exported types**: PascalCase nouns — `Server`, `Tool`, `CallToolResult`, `ContentBlock`.
- **Unexported types**: Short names — `W` (workspace), fields as `camelCase`.
- **Acronyms**: Keep uppercase — `JSONRPC`, `ID`, `URL`, `RPC`, `HTTP`, `IP`, `DNS`.
- **Constructors**: `New(...)` / `NewShadow(...)` — standard Go factory pattern.
- **Receivers**: Pointer receivers, single-letter names (`s` for Server, `w` for W).
- **Variables**: Short and descriptive — `srv`, `ws`, `mux`, `req`, `resp`, `addr`, `buf`.
- **Env vars**: `SCREAMING_SNAKE_CASE` — `CLAW_WORKSPACE`, `CLAW_HOST_PROJECT`.
- **Error types**: Suffixed with `Error` (enforced by `errname` linter).

### Error Handling

- Return errors immediately: `if err != nil { return ..., err }`.
- Wrap with context using `fmt.Errorf("...: %w", err)`.
- In `main()`, print to stderr with binary prefix and exit: `fmt.Fprintf(os.Stderr, "claw-fs: %v\n", err); os.Exit(1)`.
- MCP tool handlers return `mcpserver.ErrorResult(err.Error())` — never panic.
- Intentionally discarded errors use blank assignment: `_ = json.NewEncoder(w).Encode(resp)`.
- The `errcheck` linter excludes: `(io.Closer).Close`, `io.Copy`, `(net/http.ResponseWriter).Write`, `fmt.Fprintf`, `fmt.Fprintln`.

### Types and Structs

- Prefer concrete types and function types (`ToolHandler`) over interfaces.
- JSON struct tags: `json:"name,omitempty"` style.
- Anonymous structs for one-off JSON deserialization (tool argument parsing).
- Named field syntax for struct initialization (never positional).
- `sync.RWMutex` placed directly before the fields it protects.

### Comments

- Package comments: `//`-style above `package` line, full sentences.
- Exported symbols: godoc-style `// Name does X.` starting with the symbol name.
- Section separators: `// --- Section Name ---` for logical grouping within files.
- Inline comments sparingly for non-obvious logic.

### Testing

- Test naming: `Test<Feature>` or `Test<Feature>_<Scenario>` (underscore-separated).
- Each test creates its own state via `t.TempDir()` — no shared fixtures.
- Factory helpers like `newTestFS(t)` call `t.Helper()` and return server + temp dir.
- RPC wrappers (`rpcCall`, `toolCall`) abstract MCP protocol calls in tests.
- Assertions: manual `if` + `t.Fatalf`/`t.Errorf` — no third-party assertion library.
- Test files use the same package (not `_test` suffix) for internal access.
- Tests run with `-race` and `-count=1` (no caching).

### Security Patterns

- Workspace path validation prevents traversal (`../`, symlinks to outside root).
- File blacklist blocks reading sensitive files (`.env`, `credentials.json`, `id_rsa`, etc.).
- SSRF protection validates URLs before fetching (blocks private IPs, metadata endpoints).
- Credential redaction strips secrets from tool output before sending to clients.
- Unicode homoglyph normalization and command obfuscation detection in `internal/safenet`.

### Dependencies

Minimal external dependencies — only `chromedp` (headless browser), `golang.org/x/text`
(Unicode), and a local `tag-gateway` module. No web framework — uses `net/http` stdlib.
All binaries are static (`CGO_ENABLED=0 -trimpath`).

### Linting (golangci-lint)

14 linters enabled: `govet`, `staticcheck`, `errcheck`, `ineffassign`, `unused`,
`nilerr`, `bodyclose`, `durationcheck`, `gocritic`, `gosec`, `errname`, `unconvert`,
`misspell` (US locale), `asciicheck`. See `.golangci.yml` for full config.

Notable exclusions:
- `gosec G204` (exec.Command) — expected in claw-exec
- `errcheck` suppressed in `_test.go` files
- `govet fieldalignment` disabled
- Misspell ignores "cancelled" and "behaviour"
