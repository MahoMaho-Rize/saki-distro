
# 🐾 Claw Distro — Minimal Coding Agent Runtime

**5,000 lines of Go. 4 MCP servers. 1 gateway kernel. That's the entire agent.**

Claw Distro is the minimal distribution built on [TAG Gateway](https://github.com/MahoMaho-Rize/Edge-Agent) — a coding agent runtime that does what 800,000-line TypeScript monoliths do, in under 20,000 lines of pure Go.

## The Argument

Modern coding agents are absurdly bloated. OpenClaw ships ~800K lines of TypeScript — a 7-layer HITL permission system, an archive bomb detector with 4-tier budget accounting, a Canvas WebUI isolation layer, a cron scheduler, a plugin marketplace, and dozens of other subsystems that exist because the architecture demands them.

Claw Distro takes the opposite position: **the agent is not the application. The agent is the kernel plus a thin tool layer.**

| | OpenClaw (reference) | Claw Distro |
|---|---|---|
| Language | TypeScript | Go |
| Total source | ~800,000 lines | ~20,000 lines (kernel 15K + distro 5K) |
| Runtime overhead | Electron + Node.js + npm | Single static binary, 12 MB |
| Sandbox startup | ~700ms (Docker) | **~8ms** (bubblewrap + exported rootfs) |
| Tool integration | Proprietary plugin API | MCP (open standard) |
| Agent loop | Embedded in application | **Gateway kernel** — ReAct loop at L7, invisible to clients |
| Security model | 7-layer in-process guards | 5-layer defense-in-depth (bwrap + path validation + SSRF + redaction + prompt injection) |
| Client requirement | Electron app or VS Code extension | **Any HTTP client** (`curl` works) |

The 40:1 code ratio is not a flex — it's a design constraint. Every line in a security-critical system is an attack surface. Less code means fewer bugs, faster audits, and smaller blast radius.

## Architecture

```text
┌─────────────────────────────────────────────────┐
│                  TAG Gateway Kernel              │
│  FSM Scanner → ReAct Loop → Provider Router     │
│  Filter Chain → ext_proc Hooks → Handle Cache   │
└──────────┬──────────────────────────┬───────────┘
           │ MCP (JSON-RPC over HTTP) │
    ┌──────┴──────┐            ┌──────┴──────┐
    │  claw-fs    │            │  claw-exec  │
    │  :9100      │            │  :9101      │
    │  7 tools    │            │  4 tools    │
    │  Shadow FS  │            │  bwrap sandbox
    └─────────────┘            └─────────────┘
    ┌─────────────┐            ┌─────────────┐
    │  claw-web   │            │ claw-browser│
    │  :9102      │            │  :9103      │
    │  2 tools    │            │  6 tools    │
    │  SSRF guard │            │  chromedp   │
    └─────────────┘            └─────────────┘
```

**4 MCP tool servers**, each a single-purpose binary:

| Server | Port | Tools | Key Feature |
|--------|------|-------|-------------|
| `claw-fs` | 9100 | read, write, edit, list, glob, grep, stat | Shadow layer FS (read-only host + writable staging) |
| `claw-exec` | 9101 | exec, exec_background, kill, ps | bwrap sandbox: 8ms startup, PID namespace, ro-rootfs |
| `claw-web` | 9102 | web_search, web_fetch | SSRF protection (private IP / cloud metadata / DNS rebind) |
| `claw-browser` | 9103 | navigate, screenshot, click, type, evaluate, snapshot | Headless Chrome via chromedp |

**3 ext_proc hooks** (independent processes, hot-reloadable):

| Hook | Purpose |
|------|---------|
| `session-hook` | SQLite session persistence for multi-turn conversations |
| `context-mgr` | 5-layer context compression (system protection, tool result truncation, old message eviction) |
| `tool-guard` | Prompt injection detection, command obfuscation detection, Unicode homoglyph normalization |

## The Sandbox: bwrap vs Docker

The key innovation is using bubblewrap with an exported Docker rootfs instead of running Docker containers:

```
Traditional:  docker run --rm python:3.12-slim cmd     →  ~700ms
Claw Distro:  bwrap --ro-bind .data/rootfs / cmd       →  ~8ms
```

The rootfs (754 MB, Python 3.11 + Node 18 + Git) is exported once from a Docker image, then mounted read-only by bwrap for every execution. No daemon, no container lifecycle, no image pulls. The workspace directory is bind-mounted as the only writable path.

## Quick Start

```bash
# Build
make build

# Start everything (4 MCP servers + TAG Gateway + ext_proc hooks)
./scripts/deploy-local.sh

# Start with a host project mounted (shadow layer)
./scripts/deploy-local.sh /path/to/your/project

# Interactive mode
./bin/claw -endpoint http://127.0.0.1:8090/v1/chat/completions -i

# One-shot
./bin/claw -endpoint http://127.0.0.1:8090/v1/chat/completions "fix the failing test in pkg/auth"

# Stop
./scripts/deploy-local.sh stop
```

## Security: 5 Layers

1. **bwrap sandbox** — PID namespace isolation, read-only rootfs, host `/home` invisible, `--die-with-parent`
2. **Path validation** — traversal (`../`), symlink resolution, null byte injection, hardlink detection, sensitive file blacklist (`.env`, `id_rsa`)
3. **SSRF protection** — private IP / loopback / link-local / CGNAT / cloud metadata endpoints blocked, DNS rebind defense
4. **Output redaction** — 15 credential patterns (OpenAI / GitHub / AWS / PEM keys) auto-masked
5. **Prompt injection guard** — 10 injection patterns detected + XML boundary wrapping, command obfuscation detection, Unicode homoglyph normalization

## Documentation

| Document | Location |
|----------|----------|
| Usage guide | [`docs/USAGE.md`](docs/USAGE.md) |
| Runtime design | [`docs/RUNTIME_DESIGN.md`](docs/RUNTIME_DESIGN.md) |
| TAG Gateway kernel | [Edge-Agent](https://github.com/MahoMaho-Rize/Edge-Agent) |
| ext_proc protocol | [EXTPROC_PROTOCOL.md](https://github.com/MahoMaho-Rize/Edge-Agent/blob/dev/docs/EXTPROC_PROTOCOL.md) |
| MCP plugin guide | [mcp-plugin-development-guide.md](https://github.com/MahoMaho-Rize/Edge-Agent/blob/dev/docs/mcp-plugin-development-guide.md) |

---

# 🐾 Claw Distro — 最小编码代理运行时

**5,000 行 Go。4 个 MCP 服务器。1 个网关内核。这就是整个 agent。**

Claw Distro 是基于 [TAG Gateway](https://github.com/MahoMaho-Rize/Edge-Agent) 构建的最小发行版——一个用不到 20,000 行纯 Go 实现的编码代理运行时，做到了 800,000 行 TypeScript 巨型项目做的事情。

## 立场

现代编码代理臃肿到荒谬。OpenClaw 大约 80 万行 TypeScript——7 层 HITL 权限系统、4 级预算的解压炸弹检测器、Canvas WebUI 隔离层、cron 调度器、插件市场，以及数十个因架构需要而存在的子系统。

Claw Distro 的立场相反：**agent 不是应用程序。agent 是内核加一层薄工具层。**

| | OpenClaw（参考） | Claw Distro |
|---|---|---|
| 语言 | TypeScript | Go |
| 总源码 | ~800,000 行 | ~20,000 行（内核 15K + 发行版 5K）|
| 运行时开销 | Electron + Node.js + npm | 单个静态二进制，12 MB |
| 沙箱启动 | ~700ms (Docker) | **~8ms**（bubblewrap + 导出 rootfs）|
| 工具集成 | 私有插件 API | MCP（开放标准）|
| Agent 循环 | 嵌入应用中 | **网关内核** — L7 层 ReAct 循环，客户端无感知 |
| 安全模型 | 7 层进程内防护 | 5 层纵深防御（bwrap + 路径校验 + SSRF + 脱敏 + 注入检测）|
| 客户端要求 | Electron 或 VS Code 插件 | **任何 HTTP 客户端**（`curl` 就行）|

40:1 的代码比不是炫耀——是设计约束。安全关键系统中，每一行代码都是攻击面。代码越少，bug 越少，审计越快，爆炸半径越小。

## 快速开始

```bash
make build
./scripts/deploy-local.sh /path/to/your/project
./bin/claw -endpoint http://127.0.0.1:8090/v1/chat/completions -i
```

详细使用指南见 [`docs/USAGE.md`](docs/USAGE.md)。
