# 快速上手指南

本文档帮助你在本地从零启动 Claw Distro 全栈环境。

## 前置条件

| 依赖 | 版本要求 | 用途 |
|------|----------|------|
| Go | 1.26+ | 编译所有二进制 |
| Docker | 20+ | 沙箱镜像构建 / fallback 执行运行时 |
| TAG Gateway (`tagd`) | 最新 | LLM 编排网关，位于 `/home/yujian_shi/Edge-Agent/tagd` |
| `session-hook` | 最新 | 会话持久化钩子，位于 `/home/yujian_shi/Edge-Agent/bin/session-hook` |
| `ANTHROPIC_AUTH_TOKEN` | — | Anthropic API 密钥（环境变量） |

可选依赖：
- **bubblewrap (`bwrap`)** — 轻量级沙箱，启动比 Docker 快 120 倍（8ms vs 716ms）
- **Brave Search API Key** — `BRAVE_SEARCH_API_KEY` 环境变量，启用 `web_search` 工具
- **Chromium** — `claw-browser` 自动通过 chromedp 管理，无需手动安装

## 一、编译

```bash
make build
```

产出 6 个静态二进制到 `bin/` 目录：

| 二进制 | 说明 |
|--------|------|
| `bin/claw-fs` | 文件系统 MCP 服务器 |
| `bin/claw-exec` | Shell 执行 MCP 服务器 |
| `bin/claw-web` | 网页搜索/抓取 MCP 服务器 |
| `bin/claw-browser` | 无头浏览器 MCP 服务器 |
| `bin/claw` | CLI 客户端 |
| `bin/context-mgr` | 上下文管理 ext_proc 钩子 |

## 二、构建沙箱镜像（首次）

```bash
docker build -t saki-sandbox:latest -f deploy/Dockerfile.sandbox .
```

如果使用 bwrap 运行时，还需导出 rootfs（一次性操作）：

```bash
mkdir -p .data/rootfs
docker create --name tmp saki-sandbox:latest
docker export tmp | tar -xf - -C .data/rootfs
docker rm tmp
```

## 三、一键启动全栈

```bash
# 默认模式（独立工作区）
./scripts/deploy-local.sh

# 影子层模式（只读挂载你的项目，代理写入隔离到暂存区）
./scripts/deploy-local.sh /path/to/your/project
```

启动后会看到服务状态：

```
=== Service Status ===
  ● claw-fs         port 9100
  ● claw-exec       port 9101
  ● claw-web        port 9102
  ● claw-browser    port 9103
  ● tagd            port 8090
```

## 四、使用 CLI

```bash
# 单次对话
./bin/claw -endpoint http://127.0.0.1:8090/v1/chat/completions "创建一个 hello world Python 脚本并运行"

# 交互式 REPL（多轮对话，自动会话管理）
./bin/claw -endpoint http://127.0.0.1:8090/v1/chat/completions -i

# 指定会话 key（恢复之前的对话）
./bin/claw -s my-session -endpoint http://127.0.0.1:8090/v1/chat/completions "继续上次的任务"

# 指定模型
./bin/claw -model claude-sonnet-4-6 -endpoint http://127.0.0.1:8090/v1/chat/completions "你好"
```

CLI 参数速查：

| 参数 | 环境变量 | 默认值 | 说明 |
|------|----------|--------|------|
| `-endpoint` | `CLAW_ENDPOINT` | `http://localhost:8080/v1/chat/completions` | TAG Gateway 地址 |
| `-model` | `CLAW_MODEL` | `claude-opus-4-6` | 使用的 LLM 模型 |
| `-s` | `CLAW_SESSION` | 自动生成 | 会话 key |
| `-system` | — | 无 | 自定义 system prompt |
| `-i` | — | false | 进入交互式 REPL 模式 |

## 五、通过 API 调用

Claw Distro 暴露标准的 OpenAI 兼容 API（支持 SSE 流式输出）：

```bash
curl -N http://127.0.0.1:8090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-Session-Key: my-session" \
  -d '{
    "model": "claude-sonnet-4-6",
    "stream": true,
    "messages": [{"role": "user", "content": "读取 src/main.py 并告诉我它做了什么"}]
  }'
```

## 六、停止所有服务

```bash
./scripts/deploy-local.sh stop
```

## 七、运行测试

```bash
# 单元测试（不需要外部依赖）
make test

# MCP 协议合规性 + 工具管线灰度测试（需要先 make build）
scripts/graytest.sh

# 完整集成测试（需要 ANTHROPIC_AUTH_TOKEN）
scripts/integration-test.sh

# 完整端到端测试（需要 ANTHROPIC_AUTH_TOKEN + session-hook）
scripts/e2e-test.sh
```

## Docker Compose 部署（替代方案）

如果不想用 `deploy-local.sh`，可以用 Docker Compose：

```bash
# 设置宿主项目路径（可选）
export HOST_PROJECT=/path/to/your/project
export ANTHROPIC_API_KEY=your-key

docker-compose -f deploy/docker-compose.yaml up -d
```

注意：Compose 模式下只包含 `claw-fs` + `claw-exec` + `tagd` 三个核心服务，
不包含 `claw-web`、`claw-browser`、`context-mgr` 等扩展组件。

## 目录结构

```
.data/              # 运行时数据（自动创建）
  ├── workspace/    # 代理工作区
  ├── rootfs/       # bwrap 沙箱 rootfs
  ├── sessions.db   # 会话持久化 SQLite
  ├── *.log         # 各服务日志
  └── tagd.yaml     # 动态生成的网关配置
.run/               # PID 文件（自动创建）
bin/                # 编译产物
config/             # 配置模板
deploy/             # Docker 部署文件
```
