#!/usr/bin/env bash
# ============================================================
#  Saki Distro — Local Deployment
#  Starts the full agent stack on localhost.
#
#  Usage:
#    ./scripts/deploy-local.sh                    # default workspace
#    ./scripts/deploy-local.sh /path/to/project   # shadow mount project
#    ./scripts/deploy-local.sh stop               # stop all services
# ============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
TAGD="/home/agent/Edge-Agent/tagd"
SESSION_HOOK="/home/agent/Edge-Agent/bin/session-hook"
PID_DIR="$PROJECT_DIR/.run"
DATA_DIR="$PROJECT_DIR/.data"

TAG_PORT=${SAKI_PORT:-8090}
FS_PORT=9100
EXEC_PORT=9101
WEB_PORT=9102
BROWSER_PORT=9103

# --- Stop command ---
if [[ "${1:-}" == "stop" ]]; then
    echo "Stopping Saki services..."
    for f in "$PID_DIR"/*.pid; do
        [[ -f "$f" ]] || continue
        pid=$(cat "$f")
        name=$(basename "$f" .pid)
        if kill "$pid" 2>/dev/null; then
            echo "  stopped $name ($pid)"
        fi
        rm -f "$f"
    done
    echo "Done."
    exit 0
fi

# --- Setup ---
mkdir -p "$PID_DIR" "$DATA_DIR"
WORKSPACE="$DATA_DIR/workspace"
SQLITE_PATH="$DATA_DIR/sessions.db"
mkdir -p "$WORKSPACE"

HOST_PROJECT="${1:-}"

# --- Config ---
cat > "$DATA_DIR/tagd.yaml" << YAML
listen: ":${TAG_PORT}"
max_react_iterations: 10
sse_keepalive_ms: 15000

providers:
  - name: anthropic
    base_url: "${ANTHROPIC_BASE_URL:-http://192.168.190.105:8088/api}"
    api_key: "${ANTHROPIC_AUTH_TOKEN}"
    models:
      - name: "claude-opus-4-6"
        context_window: 1000000
        max_output_tokens: 128000
      - name: "claude-sonnet-4-6"
        context_window: 1000000
        max_output_tokens: 128000

mcp:
  inject_mode: "auto"
  refresh_interval_sec: 300
  servers:
    - name: fs
      transport: http
      url: "http://127.0.0.1:${FS_PORT}/mcp"
      timeout_sec: 30
    - name: exec
      transport: http
      url: "http://127.0.0.1:${EXEC_PORT}/mcp"
      timeout_sec: 120
    - name: web
      transport: http
      url: "http://127.0.0.1:${WEB_PORT}/mcp"
      timeout_sec: 30
    - name: browser
      transport: http
      url: "http://127.0.0.1:${BROWSER_PORT}/mcp"
      timeout_sec: 60

ext_proc:
  enabled: true
  default_timeout_ms: 5000
  hooks:
    - command: ${SESSION_HOOK}
      order: 10
      env:
        SQLITE_PATH: ${SQLITE_PATH}
    - command: ${PROJECT_DIR}/bin/context-mgr
      order: 20

log:
  level: "info"
  pretty: true
YAML

# --- Start services ---
echo "=== Saki Distro — Local Deployment ==="
echo ""

start_service() {
    local name=$1 cmd=$2
    shift 2
    echo -n "  starting $name..."
    $cmd "$@" > "$DATA_DIR/${name}.log" 2>&1 &
    local pid=$!
    echo "$pid" > "$PID_DIR/${name}.pid"
    echo " pid=$pid"
}

# MCP Servers
FS_ENV="CLAW_WORKSPACE=$WORKSPACE CLAW_FS_ADDR=:${FS_PORT}"
if [[ -n "$HOST_PROJECT" ]]; then
    FS_ENV="$FS_ENV CLAW_HOST_PROJECT=$HOST_PROJECT"
    echo "  shadow layer: $HOST_PROJECT (read-only) → $WORKSPACE (writable)"
else
    echo "  workspace: $WORKSPACE (standalone, no shadow)"
fi
echo ""

env $FS_ENV "$PROJECT_DIR/bin/claw-fs" > "$DATA_DIR/claw-fs.log" 2>&1 &
echo "$!" > "$PID_DIR/claw-fs.pid"
echo "  starting claw-fs... pid=$!"

start_service "claw-exec" env CLAW_WORKSPACE="$WORKSPACE" CLAW_EXEC_ADDR=":${EXEC_PORT}" \
    HTTP_PROXY="socks5h://127.0.0.1:1080" HTTPS_PROXY="socks5h://127.0.0.1:1080" ALL_PROXY="socks5h://127.0.0.1:1080" \
    "$PROJECT_DIR/bin/claw-exec"
start_service "claw-web" env CLAW_WEB_ADDR=":${WEB_PORT}" "$PROJECT_DIR/bin/claw-web"
start_service "claw-browser" env CLAW_BROWSER_ADDR=":${BROWSER_PORT}" "$PROJECT_DIR/bin/claw-browser"

sleep 0.5

# TAG Gateway + hooks
start_service "tagd" "$TAGD" "$DATA_DIR/tagd.yaml"

echo ""
echo "  waiting for initialization..."
sleep 3

# --- Verify ---
echo ""
echo "=== Service Status ==="

check_service() {
    local name=$1 port=$2
    if curl -s --max-time 2 "http://127.0.0.1:${port}/mcp" -X POST \
        -H "Content-Type: application/json" \
        -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"check","version":"0.1"}}}' \
        > /dev/null 2>&1; then
        printf "  \033[32m●\033[0m %-15s port %s\n" "$name" "$port"
    else
        printf "  \033[31m●\033[0m %-15s port %s (not responding)\n" "$name" "$port"
    fi
}

check_service "claw-fs" "$FS_PORT"
check_service "claw-exec" "$EXEC_PORT"
check_service "claw-web" "$WEB_PORT"
check_service "claw-browser" "$BROWSER_PORT"

# TAG health check via a simple request
if curl -s --max-time 5 "http://127.0.0.1:${TAG_PORT}/v1/chat/completions" \
    -H "Content-Type: application/json" \
    -d '{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"ping"}]}' \
    > /dev/null 2>&1; then
    printf "  \033[32m●\033[0m %-15s port %s\n" "tagd" "$TAG_PORT"
else
    printf "  \033[33m●\033[0m %-15s port %s (starting...)\n" "tagd" "$TAG_PORT"
fi

echo ""
echo "=== Ready ==="
echo ""
echo "  API:   http://127.0.0.1:${TAG_PORT}/v1/chat/completions"
echo "  CLI:   ./bin/claw -endpoint http://127.0.0.1:${TAG_PORT}/v1/chat/completions \"your message\""
echo "  REPL:  ./bin/claw -endpoint http://127.0.0.1:${TAG_PORT}/v1/chat/completions -i"
echo "  Stop:  ./scripts/deploy-local.sh stop"
echo "  Logs:  $DATA_DIR/*.log"
echo "  Data:  $SQLITE_PATH"
echo ""
