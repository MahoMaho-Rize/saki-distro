# 运行维护指南

本文档面向运维人员，涵盖 Claw Distro 的日常运维、故障排查、监控和安全注意事项。

---

## 1. 架构总览

```
用户请求
   │
   ▼
┌──────────┐
│  TAG     │  :8090   LLM 编排网关（ReAct 循环、SSE 流式）
│  Gateway │─────────────────────────────────────────────┐
└────┬─────┘                                             │
     │ ext_proc hooks                                    │
     ├── session-hook (order=10)  会话持久化 (SQLite)    │
     ├── context-mgr  (order=20)  5 层上下文管理         │
     │                                                   │
     │ MCP tool servers (JSON-RPC 2.0 over HTTP)         │
     ├── claw-fs      :9100  文件系统（7 工具）          │
     ├── claw-exec    :9101  Shell 执行（4 工具）        │
     ├── claw-web     :9102  网页搜索/抓取（2 工具）     │
     └── claw-browser :9103  无头浏览器（6 工具）        │
                                                         │
                                              Anthropic API
```

所有组件均为独立进程，通过 HTTP 通信，无共享内存。

## 2. 服务管理

### 启动

```bash
# 一键启动全栈（推荐）
./scripts/deploy-local.sh                     # 独立工作区
./scripts/deploy-local.sh /path/to/project    # 影子层模式
```

### 停止

```bash
./scripts/deploy-local.sh stop
```

此命令会：
1. 停止 `saki-sandbox` Docker 容器（如存在）
2. 根据 `.run/*.pid` 文件逐个终止所有服务进程
3. 清理 PID 文件

### 手动启动单个服务

如果需要单独调试某个服务：

```bash
# claw-fs（文件系统）
CLAW_WORKSPACE=/workspace CLAW_FS_ADDR=:9100 ./bin/claw-fs

# claw-fs 影子层模式
CLAW_WORKSPACE=/workspace CLAW_HOST_PROJECT=/path/to/project CLAW_FS_ADDR=:9100 ./bin/claw-fs

# claw-exec（Shell 执行）
CLAW_WORKSPACE=/workspace CLAW_EXEC_ADDR=:9101 CLAW_EXEC_RUNTIME=bwrap CLAW_EXEC_ROOTFS=.data/rootfs ./bin/claw-exec

# claw-web（网页工具）
CLAW_WEB_ADDR=:9102 BRAVE_SEARCH_API_KEY=xxx ./bin/claw-web

# claw-browser（浏览器）
CLAW_BROWSER_ADDR=:9103 ./bin/claw-browser
```

## 3. 环境变量参考

### 通用

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `CLAW_WORKSPACE` | `/workspace` | 代理工作区根目录 |

### claw-fs

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `CLAW_FS_ADDR` | `:9100` | 监听地址 |
| `CLAW_HOST_PROJECT` | 空 | 设置后启用影子层模式（只读下层） |

### claw-exec

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `CLAW_EXEC_ADDR` | `:9101` | 监听地址 |
| `CLAW_EXEC_RUNTIME` | 自动检测 | `bwrap`（优先）或 `docker` |
| `CLAW_EXEC_ROOTFS` | `/opt/saki-rootfs` | bwrap 沙箱 rootfs 路径 |
| `CLAW_EXEC_IMAGE` | `saki-sandbox:latest` | Docker 沙箱镜像名 |
| `CLAW_EXEC_CONTAINER` | `saki-sandbox` | Docker 容器名 |
| `CLAW_EXEC_MEMORY` | `512m` | 容器内存限制 |
| `CLAW_EXEC_CPUS` | `2` | 容器 CPU 限制 |
| `CLAW_EXEC_PIDS` | `100` | 容器进程数限制 |
| `CLAW_DATA_DIR` | 同 `CLAW_WORKSPACE` | 持久化数据目录（agent home） |
| `HTTP_PROXY` / `HTTPS_PROXY` | 空 | 代理设置，会传入沙箱内 |

### claw-web

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `CLAW_WEB_ADDR` | `:9102` | 监听地址 |
| `BRAVE_SEARCH_API_KEY` | 空 | Brave Search API 密钥 |
| `CLAW_WEB_DISABLE_SSRF_CHECK` | `0` | 设为 `1` 禁用 SSRF 检查（仅调试） |

### claw-browser

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `CLAW_BROWSER_ADDR` | `:9103` | 监听地址 |

### CLI (`claw`)

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `CLAW_ENDPOINT` | `http://localhost:8080/v1/chat/completions` | TAG Gateway 端点 |
| `CLAW_MODEL` | `claude-opus-4-6` | 默认模型 |
| `CLAW_SESSION` | 自动生成 | 会话 key |

### TAG Gateway

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `ANTHROPIC_AUTH_TOKEN` | — | Anthropic API 密钥 |
| `ANTHROPIC_BASE_URL` | `http://192.168.190.105:8088/api` | API 基地址（本地部署默认指向内部代理） |
| `SAKI_PORT` | `8090` | TAG Gateway 监听端口 |

## 4. 健康检查

### MCP 服务器健康检查

向任意 MCP 服务器发送 `initialize` 请求：

```bash
curl -s -X POST http://127.0.0.1:9100/mcp \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"healthcheck","version":"0.1"}}}'
```

正常响应包含 `"protocolVersion":"2025-03-26"`。

### 快速检查所有服务

```bash
for port in 9100 9101 9102 9103; do
  if curl -s --max-time 2 -X POST "http://127.0.0.1:${port}/mcp" \
    -H "Content-Type: application/json" \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"check","version":"0.1"}}}' \
    | grep -q "2025-03-26"; then
    echo "端口 $port: 正常"
  else
    echo "端口 $port: 异常"
  fi
done
```

### TAG Gateway 健康检查

```bash
curl -s --max-time 5 http://127.0.0.1:8090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"ping"}]}'
```

## 5. 日志

所有服务日志写入 `.data/` 目录：

| 文件 | 服务 |
|------|------|
| `.data/claw-fs.log` | 文件系统服务器 |
| `.data/claw-exec.log` | Shell 执行服务器 |
| `.data/claw-web.log` | 网页工具服务器 |
| `.data/claw-browser.log` | 浏览器服务器 |
| `.data/tagd.log` | TAG Gateway |

```bash
# 实时查看某个服务日志
tail -f .data/claw-exec.log

# 查看所有服务日志
tail -f .data/*.log
```

MCP 服务器启动时会在 stderr 输出启动信息（格式：`MCP server name/version listening on addr`），
这些信息会被重定向到对应的 `.log` 文件。

## 6. 数据与存储

### 工作区 (`.data/workspace/`)

代理的所有文件操作都发生在此目录。影子层模式下：
- **下层（宿主项目）**：只读挂载，代理无法修改
- **上层（workspace）**：代理的所有写入都在这里，类似 overlayfs

### 会话数据 (`.data/sessions.db`)

SQLite 数据库，由 `session-hook` 管理。包含：
- `sessions` 表：会话元数据
- `messages` 表：对话历史

```bash
# 查看会话列表
sqlite3 .data/sessions.db "SELECT session_id, created_at FROM sessions;"

# 查看某个会话的消息数
sqlite3 .data/sessions.db "SELECT COUNT(*) FROM messages WHERE session_id='xxx';"

# 清理过期会话（例如超过 7 天的）
sqlite3 .data/sessions.db "DELETE FROM sessions WHERE created_at < datetime('now', '-7 days');"
sqlite3 .data/sessions.db "DELETE FROM messages WHERE session_id NOT IN (SELECT session_id FROM sessions);"
sqlite3 .data/sessions.db "VACUUM;"
```

### 沙箱 rootfs (`.data/rootfs/`)

bwrap 运行时使用的只读文件系统，由 Docker 镜像导出。约 754MB。

```bash
# 更新 rootfs（重新构建镜像后执行）
docker build -t saki-sandbox:latest -f deploy/Dockerfile.sandbox .
rm -rf .data/rootfs && mkdir -p .data/rootfs
docker create --name tmp saki-sandbox:latest
docker export tmp | tar -xf - -C .data/rootfs
docker rm tmp
```

### Agent Home (`.data/.agent-home/`)

bwrap 模式下的持久化 home 目录。`pip install`、`npm install` 等操作的缓存
和安装结果保存在这里，跨命令执行持久化。

## 7. 执行运行时

claw-exec 支持两种沙箱运行时：

### bwrap（推荐）

- 启动延迟 ~8ms（Docker 为 ~716ms）
- 不需要 Docker daemon
- 通过 `--die-with-parent` 自动清理子进程
- 需要内核支持 unprivileged user namespaces

```bash
# 检查 bwrap 可用性
which bwrap && echo "已安装"
sysctl kernel.unprivileged_userns_clone  # 应为 1
```

### docker（fallback）

- 启动一个持久容器 `saki-sandbox`，通过 `docker exec` 执行命令
- 资源限制：512MB 内存、2 CPU、100 进程
- `--security-opt no-new-privileges` 阻止提权

```bash
# 检查沙箱容器状态
docker inspect -f '{{.State.Running}}' saki-sandbox

# 手动重启沙箱容器
docker rm -f saki-sandbox
# 重启 claw-exec 即可自动重建
```

## 8. 安全注意事项

### 文件系统安全

- **路径遍历防护**：所有文件路径经 `workspace.Resolve()` 校验，阻止 `../` 越界和符号链接逃逸
- **文件黑名单**：禁止读取 `.env`、`credentials.json`、`secrets.yaml`、`id_rsa`、`id_ed25519` 等敏感文件
- **影子层隔离**：宿主项目只读挂载，代理无法修改原始文件

### 网络安全

- **SSRF 防护**（`internal/safenet`）：阻止访问私有 IP、回环地址、云元数据端点（169.254.169.254 等）
- **DNS 重绑定检测**：解析域名后检查 IP 是否在黑名单范围
- **Docker 网络隔离**：`claw-internal` 网络为纯内部网络，仅 `claw-exec` 有外网访问权限

### 输出安全

- **凭证脱敏**（`safenet.RedactSecrets`）：工具输出中的 API 密钥、PAT、PEM 私钥等自动脱敏
- **命令混淆检测**（`safenet.DetectObfuscation`）：检测 base64 解码管道、curl 管道到 shell 等危险模式
- **Unicode 同形字归一化**（`safenet.NormalizeUnicode`）：防止西里尔字母冒充拉丁字母绕过安全检查

### 沙箱安全

- bwrap：`--unshare-pid`、`--die-with-parent`、只读绑定宿主文件系统
- Docker：`--no-new-privileges`、内存/CPU/进程数限制
- 两种运行时都将 `/workspace` 设为唯一可写目录

## 9. 故障排查

### 服务无法启动

```bash
# 检查端口是否被占用
lsof -i :9100
lsof -i :9101

# 检查二进制是否存在
ls -la bin/

# 重新编译
make clean && make build
```

### claw-exec 命令执行失败

```bash
# 检查运行时
cat .data/claw-exec.log | grep "runtime="

# bwrap 模式：检查 rootfs
ls .data/rootfs/usr/bin/python3

# docker 模式：检查容器
docker ps -a | grep saki-sandbox
docker logs saki-sandbox
```

### 影子层读不到宿主文件

```bash
# 确认 CLAW_HOST_PROJECT 已设置
cat .data/claw-fs.log | grep "shadow"

# 确认宿主项目路径正确且可读
ls -la /path/to/project
```

### 会话不持久

```bash
# 确认 session-hook 在运行
cat .data/tagd.log | grep "session"

# 确认 SQLite 文件存在且可写
ls -la .data/sessions.db

# 检查请求是否带了 X-Session-Key 头
```

### 工具调用超时

MCP 工具超时配置在 `tagd.yaml` 中：

| 服务 | 默认超时 | 说明 |
|------|----------|------|
| claw-fs | 30s | 文件操作一般很快 |
| claw-exec | 120s | 编译、安装等可能较慢 |
| claw-web | 30s | 网页抓取 |
| claw-browser | 60s | 页面加载可能较慢 |

claw-exec 内部还有每条命令的超时（默认 30s，最大 300s），由工具参数 `timeout_ms` 控制。

## 10. 定期维护

### 日常

- 检查 `.data/*.log` 有无异常错误
- 监控 `.data/workspace/` 磁盘占用

### 每周

- 清理过期会话数据（见第 6 节）
- 清理 `.data/workspace/` 中不再需要的文件

### 每月

- 更新沙箱镜像：`docker build -t saki-sandbox:latest -f deploy/Dockerfile.sandbox .`
- 更新 rootfs（如果使用 bwrap）
- 运行完整测试套件验证：`make test && scripts/graytest.sh`
- 更新 Go 依赖：`go get -u ./... && make tidy && make test`
