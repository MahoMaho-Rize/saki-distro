# Claw Distro — 使用指南

## 前置条件

- TAG Gateway 二进制（`/home/agent/Edge-Agent/tagd`）
- `bubblewrap` 已安装（`/usr/bin/bwrap`）
- `saki-sandbox:latest` Docker 镜像已构建（首次部署自动导出为 rootfs）
- 环境变量 `ANTHROPIC_AUTH_TOKEN`（LLM API 密钥）

## 快速启动

```bash
cd /home/agent/claw-distro

# 构建所有二进制
make build

# 启动全栈（4 MCP 服务器 + TAG Gateway + ext_proc hooks）
./scripts/deploy-local.sh

# 挂载宿主项目（shadow layer：项目只读 + workspace 可写）
./scripts/deploy-local.sh /path/to/your/project

# 停止所有服务
./scripts/deploy-local.sh stop
```

## 服务端口

| 服务 | 端口 | 说明 |
|------|------|------|
| claw-fs | :9100 | 文件系统（读/写/编辑/glob/grep） |
| claw-exec | :9101 | 沙箱 shell 执行 |
| claw-web | :9102 | 网页抓取/搜索 |
| claw-browser | :9103 | 无头浏览器（chromedp） |
| TAG Gateway | :8090 | LLM API 入口（deploy-local.sh 默认） |

## CLI 使用

```bash
# 单次调用
./bin/claw -endpoint http://127.0.0.1:8090/v1/chat/completions "创建一个 hello world"

# 交互模式（多轮对话）
./bin/claw -endpoint http://127.0.0.1:8090/v1/chat/completions -i

# 指定会话（恢复上下文）
./bin/claw -s mysession "继续上次的任务"

# 管道输入
echo "修复这个 bug" | ./bin/claw -endpoint http://127.0.0.1:8090/v1/chat/completions

# 环境变量（避免重复指定 endpoint）
export CLAW_ENDPOINT=http://127.0.0.1:8090/v1/chat/completions
export CLAW_MODEL=claude-opus-4-6
./bin/claw "你的指令"
```

## 沙箱运行时

### 双运行时架构

claw-exec 支持两种沙箱运行时，自动检测或通过 `CLAW_EXEC_RUNTIME` 手动指定：

| | bwrap（默认） | Docker（回退） |
|---|---|---|
| 启动延迟 | ~8ms | ~716ms |
| 依赖 | bwrap + 导出的 rootfs | Docker daemon |
| 隔离 | PID namespace + ro-bind rootfs | 容器 |
| 僵尸清理 | `--die-with-parent` | `docker kill` |

**运行时选择逻辑**：`CLAW_EXEC_RUNTIME` 环境变量 → rootfs 存在且 bwrap 可用 → Docker 回退。

### rootfs 管理

首次运行 `deploy-local.sh` 时自动从 `saki-sandbox:latest` 导出：

```bash
# 手动重建 rootfs（镜像更新后）
docker build -t saki-sandbox:latest -f deploy/Dockerfile.sandbox .
rm -rf .data/rootfs
./scripts/deploy-local.sh   # 自动重新导出
```

rootfs 位于 `.data/rootfs/`（754MB），包含 Python 3.11、Node 18、Git、build-essential。

### 沙箱内环境

- **工具链**：Python 3.11、Node 18、Git 2.39、curl、jq、npm
- **可写区域**：`/workspace`（工作目录）、`/home/agent`（pip/npm 持久化）、`/tmp`
- **只读区域**：其余所有路径（整个 rootfs）
- **网络**：通过宿主代理出网（`HTTP_PROXY=socks5h://127.0.0.1:1080`）
- **宿主隔离**：宿主 `/home` 不可见，PID namespace 隔离

### 已知问题

- pip 通过 SOCKS 代理安装包会失败（rootfs 缺少 `PySocks`），去掉代理环境变量后直连可用
- 临时规避：命令中 `unset HTTP_PROXY HTTPS_PROXY ALL_PROXY && pip install ...`
- 永久修复：在 `Dockerfile.sandbox` 加 `RUN pip install pysocks` 后重建 rootfs

## Shadow Layer（影子层文件系统）

挂载宿主项目后，文件系统行为类似 OverlayFS：

- **读**：先查 workspace（upper），未命中回退宿主项目（lower，只读）
- **写**：始终写入 workspace（upper），不修改宿主文件
- **列表**：合并两层，workspace 文件优先

```bash
# 启动时挂载项目
./scripts/deploy-local.sh /home/agent/my-project

# 查看 agent 修改了哪些文件（仅 upper 层）
ls .data/workspace/
```

## 安全机制

五层纵深防御，全部在线验证通过：

1. **bwrap 沙箱**：宿主 home 不可见、根文件系统只读、PID namespace 隔离
2. **路径安全**：穿越拦截（`../`）、符号链接解析、硬链接检测（nlink>1）、空字节拦截、敏感文件黑名单
3. **SSRF 防护**：私有 IP/回环/link-local/CGNAT/云元数据端点全部阻断，DNS 解析后二次验证
4. **输出净化**：15 种凭据模式自动脱敏（OpenAI/GitHub/AWS/PEM 等）
5. **tool-guard 钩子**：prompt 注入检测 + XML 边界包裹、命令混淆检测、Unicode 同形字归一化

## 数据目录

```
.data/
├── rootfs/          # bwrap 沙箱根文件系统（754MB，只读）
├── .agent-home/     # 沙箱 /home/agent 持久化（pip/npm 缓存）
├── workspace/       # agent 工作目录（可写）
├── sessions.db      # 会话持久化（SQLite，session-hook 管理）
├── tagd.yaml        # 运行时生成的 TAG Gateway 配置
├── *.log            # 各服务日志
```

## 日志与调试

```bash
# 查看各服务日志
tail -f .data/claw-exec.log
tail -f .data/claw-fs.log
tail -f .data/tagd.log

# MCP 协议直接测试
curl -s http://127.0.0.1:9101/mcp -X POST \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call",
       "params":{"name":"exec","arguments":{"command":"echo hello"}}}'

# 服务健康检查
for port in 9100 9101 9102 9103; do
  curl -s http://127.0.0.1:$port/mcp -X POST \
    -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize",
         "params":{"protocolVersion":"2025-03-26",
                   "clientInfo":{"name":"check","version":"0.1"}}}' \
    > /dev/null 2>&1 && echo ":$port OK" || echo ":$port FAIL"
done
```

## 环境变量参考

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `CLAW_WORKSPACE` | `/workspace` | 工作目录根路径 |
| `CLAW_HOST_PROJECT` | （空） | 宿主项目路径（shadow lower layer） |
| `CLAW_EXEC_RUNTIME` | 自动检测 | `bwrap` 或 `docker` |
| `CLAW_EXEC_ROOTFS` | `/opt/saki-rootfs` | bwrap rootfs 路径 |
| `CLAW_EXEC_IMAGE` | `saki-sandbox:latest` | Docker 沙箱镜像 |
| `CLAW_EXEC_MEMORY` | `512m` | Docker 内存限制 |
| `CLAW_EXEC_CPUS` | `2` | Docker CPU 限制 |
| `CLAW_EXEC_PIDS` | `100` | Docker PID 限制 |
| `CLAW_ENDPOINT` | `http://localhost:8080/v1/chat/completions` | CLI 默认 API 端点 |
| `CLAW_MODEL` | `claude-opus-4-6` | CLI 默认模型 |
| `CLAW_SESSION` | （空） | CLI 默认会话 key |
| `ANTHROPIC_AUTH_TOKEN` | — | LLM API 密钥（必需） |
| `SAKI_PORT` | `8090` | TAG Gateway 监听端口 |
