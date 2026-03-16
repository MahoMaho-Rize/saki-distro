# Saki Runtime Design — Bubblewrap + Nix 替代 Docker

## 状态：已验证，待实施

## 动机

claw-exec v0.6 使用 Docker 作为命令执行沙箱。存在三个硬伤：

| 问题 | 影响 |
|------|------|
| 启动延迟 716ms/命令 | ReAct 循环每轮都付出这个代价，5 轮 = 3.6 秒纯开销 |
| 需要 Docker daemon + docker 组 | 部署门槛高，安全审计复杂 |
| 镜像不可重现 | `ubuntu:24.04` 随时间变化，不同机器行为不一致 |

## 方案：Bubblewrap (bwrap) 沙箱

bubblewrap 是 Flatpak 使用的轻量级沙箱工具，提供与 Docker 等价的隔离能力，
但不需要 daemon、不需要 root、启动时间 6ms（120 倍快于 Docker）。

### 已验证能力（2026-03-16 在生产机器上测试）

```
✅ bubblewrap 0.6.1 已安装
✅ unprivileged user namespaces 启用 (kernel.unprivileged_userns_clone=1)
✅ Landlock LSM v1 可用
✅ cgroup v2 + memory/pids delegation 到用户级
✅ 内核 5.15.0-125-generic
```

### 性能对比（实测）

```
Docker:  real 0m0.716s  (docker run --rm ubuntu:24.04 echo hello)
bwrap:   real 0m0.006s  (bwrap --ro-bind /usr /usr ... echo hello)
```

### 隔离能力对照

| 隔离维度 | Docker | bwrap | 验证结果 |
|----------|--------|-------|---------|
| 文件系统 | mount namespace | `--ro-bind`, `--tmpfs`, `--bind` | ✅ /home/agent 不可见 |
| 网络 | network namespace | `--unshare-net` | ✅ 仅 loopback |
| PID | pid namespace | `--unshare-pid` | ✅ |
| 用户 | user namespace | `--unshare-user` | ✅ |
| 资源限制 | cgroups | cgroup v2 delegation (memory + pids) | ✅ |
| 特权提升 | `--security-opt no-new-privileges` | `--unshare-all` + no setuid | ✅ |
| 僵尸清理 | `docker kill` | `--die-with-parent` | ✅ 父进程退出自动清理 |

### 典型 bwrap 调用（等价于当前 docker run）

```bash
bwrap \
    --ro-bind /usr /usr \         # 宿主工具（python3, node, git...）
    --ro-bind /bin /bin \
    --ro-bind /lib /lib \
    --ro-bind /lib64 /lib64 \
    --ro-bind /etc/alternatives /etc/alternatives \
    --proc /proc \
    --dev /dev \
    --tmpfs /tmp \
    --tmpfs /home \
    --bind /path/to/workspace /workspace \  # 工作区（可写）
    --chdir /workspace \
    --unshare-all \               # 全隔离（mount+net+pid+user+ipc+uts）
    --die-with-parent \           # 父进程退出自动杀子进程
    -- sh -c "user command here"
```

### 网络代理传入

```bash
bwrap ... \
    --setenv HTTP_PROXY "socks5h://host.internal:1080" \
    --setenv HTTPS_PROXY "socks5h://host.internal:1080" \
    -- sh -c "pip install ..."
```

注意：`--unshare-net` 隔离网络后，代理需要通过 `--share-net` 选择性放开，
或者使用 `--ro-bind` 绑定宿主的 `/etc/resolv.conf`。

需要网络访问（pip install, npm install）时用 `--share-net` 替代 `--unshare-net`。

## 三阶段演进

### Phase A：bwrap 替代 Docker（⚡ 立即可做）

**改动**: claw-exec 的 `buildDockerArgs()` → `buildBwrapArgs()`
**代码量**: ~100 行替换
**依赖**: bwrap（已安装）
**效果**: 启动 716ms → 6ms，去掉 Docker daemon 依赖

```go
func buildBwrapArgs(workDir string, needsNetwork bool) []string {
    args := []string{
        "--ro-bind", "/usr", "/usr",
        "--ro-bind", "/bin", "/bin",
        "--ro-bind", "/lib", "/lib",
        "--ro-bind", "/lib64", "/lib64",
        "--ro-bind", "/etc/alternatives", "/etc/alternatives",
        "--proc", "/proc",
        "--dev", "/dev",
        "--tmpfs", "/tmp",
        "--tmpfs", "/home",
        "--bind", workDir, "/workspace",
        "--chdir", "/workspace",
        "--unshare-pid",
        "--unshare-ipc",
        "--unshare-uts",
        "--die-with-parent",
    }
    if !needsNetwork {
        args = append(args, "--unshare-net")
    }
    // 代理环境变量
    for _, kv := range proxyEnvVars {
        parts := strings.SplitN(kv, "=", 2)
        args = append(args, "--setenv", parts[0], parts[1])
    }
    return args
}
```

**限制**: 复用宿主已安装的工具。如果宿主没有 python3，沙箱里也没有。

### Phase B：Nix 可重现环境（需要安装 Nix）

**目标**: 精确定义 agent 可用的工具集，跨机器一致。

```nix
# saki-tools.nix
{ pkgs ? import <nixpkgs> {} }:
pkgs.buildEnv {
  name = "saki-tools";
  paths = with pkgs; [
    python312
    nodejs_22
    git
    curl
    jq
    ripgrep
  ];
}
```

bwrap 绑定 Nix store 而非宿主 /usr：

```bash
bwrap \
    --ro-bind /nix/store /nix/store \
    --ro-bind $(nix-build saki-tools.nix)/bin /usr/bin \
    --bind workspace /workspace \
    --unshare-all \
    -- sh -c "python3 --version"
```

**代码量**: +50 行配置
**依赖**: Nix (~1GB store)
**效果**: 完全可重现，版本锁定

### Phase C：Landlock 纵深防御（可选）

Landlock LSM 提供文件级权限控制，不需要 root：

```go
import "github.com/landlock-lsm/go-landlock/landlock"

// 只允许 /workspace 写入，/usr 只读，其他不可访问
err := landlock.V1.BestEffort().RestrictPaths(
    landlock.RODirs("/usr", "/bin", "/lib", "/lib64"),
    landlock.RWDirs("/workspace"),
    landlock.ROFiles("/etc/resolv.conf"),
)
```

比 bwrap 更细粒度——bwrap 隔离整个 mount namespace，Landlock 可以在
mount namespace 内部进一步限制。两者叠加 = 双保险。

**代码量**: +80 行
**效果**: 即使 bwrap 被绕过，Landlock 仍阻止文件访问

## 对 Saki 架构的影响

### claw-exec 改动

```
当前: MCP Server → docker run → ubuntu:24.04 container → command
Phase A: MCP Server → bwrap → host tools sandbox → command
Phase B: MCP Server → bwrap → nix tools sandbox → command
```

接口不变——MCP 的 `exec` 工具入参和返回值完全相同。
只有内部的命令构造从 Docker CLI 切换到 bwrap CLI。

### 不影响的组件

- claw-fs: 不使用 Docker，纯 Go 文件操作
- claw-web: HTTP 客户端，不需要沙箱
- claw-browser: chromedp 管理自己的 Chromium 进程
- context-mgr / tool-guard: ext_proc hooks，纯逻辑
- TAG 内核: 完全无关

### Shadow Layer 兼容性

bwrap 的 `--ro-bind` 和 `--bind` 完美对应 Shadow Layer 的 lower/upper 模型：

```bash
bwrap \
    --ro-bind /host-project /host-project \  # lower layer (read-only)
    --bind /workspace /workspace \           # upper layer (writable)
    ...
```

## 与 Docker 共存

Phase A 不删除 Docker 支持。通过环境变量选择运行时：

```bash
CLAW_EXEC_RUNTIME=bwrap   # 默认（如果 bwrap 可用）
CLAW_EXEC_RUNTIME=docker  # 回退
```

## 风险

| 风险 | 缓解 |
|------|------|
| bwrap 在某些云环境不可用 | 保留 Docker fallback |
| user namespace 被禁用 | 检测并回退到 Docker |
| 宿主工具缺失（Phase A） | Phase B 用 Nix 解决 |
| bwrap 不是 OCI 标准 | 我们不需要 OCI——只需要沙箱执行 |
