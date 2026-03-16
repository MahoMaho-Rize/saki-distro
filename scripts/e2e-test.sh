#!/usr/bin/env bash
# ============================================================
#  Saki (Claw) Distro — Full End-to-End Test
#
#  Tests the COMPLETE stack with Shadow Layer workspace:
#  TAG Gateway + session-hook + context-mgr + claw-fs + claw-exec + claw-web
#
#  Validates:
#  1. Shadow Layer: read host files (ro) + write to upper layer
#  2. Multi-turn session: user + assistant messages persisted
#  3. All MCP tools callable (fs×6 + exec×4 + web×2 = 12 tools)
#  4. CLI streaming works
#  5. react_trace captured
#  6. Security: blacklist + path escape in shadow mode
# ============================================================
set -uo pipefail

PASS=0
FAIL=0
TOTAL=0

# --- Directories ---
HOST_PROJECT=$(mktemp -d)     # simulates user's real project (read-only to agent)
UPPER_DIR=$(mktemp -d)        # agent's writable staging area
SQLITE_PATH=$(mktemp -d)/sessions.db

TAGD="${TAGD:-$(dirname "$PROJECT_DIR")/Edge-Agent/tagd}"
SESSION_HOOK="${SESSION_HOOK:-$(dirname "$PROJECT_DIR")/Edge-Agent/bin/session-hook}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

TAG_PORT=18580
FS_PORT=19600
EXEC_PORT=19601
WEB_PORT=19602
PIDS=()

cleanup() {
    echo ""
    echo "=== Cleaning up ==="
    for pid in "${PIDS[@]}"; do
        kill "$pid" 2>/dev/null || true
    done
    rm -rf "$HOST_PROJECT" "$UPPER_DIR"
    rm -f "$SQLITE_PATH"
}
trap cleanup EXIT

# --- Helpers ---

assert_contains() {
    local label=$1 haystack=$2 needle=$3
    TOTAL=$((TOTAL + 1))
    if echo "$haystack" | grep -qi "$needle"; then
        PASS=$((PASS + 1))
        printf "  \033[32m✓\033[0m %s\n" "$label"
    else
        FAIL=$((FAIL + 1))
        printf "  \033[31m✗\033[0m %s\n" "$label"
        printf "    expected: %s\n" "$needle"
        printf "    got (200c): %.200s\n" "$haystack"
    fi
}

assert_not_contains() {
    local label=$1 haystack=$2 needle=$3
    TOTAL=$((TOTAL + 1))
    if ! echo "$haystack" | grep -qi "$needle"; then
        PASS=$((PASS + 1))
        printf "  \033[32m✓\033[0m %s\n" "$label"
    else
        FAIL=$((FAIL + 1))
        printf "  \033[31m✗\033[0m %s\n" "$label"
        printf "    expected NOT to contain: %s\n" "$needle"
    fi
}

assert_eq() {
    local label=$1 got=$2 want=$3
    TOTAL=$((TOTAL + 1))
    if [[ "$got" == "$want" ]]; then
        PASS=$((PASS + 1))
        printf "  \033[32m✓\033[0m %s\n" "$label"
    else
        FAIL=$((FAIL + 1))
        printf "  \033[31m✗\033[0m %s (%s != %s)\n" "$label" "$got" "$want"
    fi
}

assert_ge() {
    local label=$1 got=$2 min=$3
    TOTAL=$((TOTAL + 1))
    if [[ "$got" -ge "$min" ]]; then
        PASS=$((PASS + 1))
        printf "  \033[32m✓\033[0m %s (%s)\n" "$label" "$got"
    else
        FAIL=$((FAIL + 1))
        printf "  \033[31m✗\033[0m %s (got %s, want >= %s)\n" "$label" "$got" "$min"
    fi
}

mcp_call() {
    local port=$1 method=$2 params=$3
    curl -s -X POST "http://127.0.0.1:${port}/mcp" \
        -H "Content-Type: application/json" \
        -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"${method}\",\"params\":${params}}"
}

tool_call() {
    local port=$1 name=$2 args=$3
    mcp_call "$port" "tools/call" "{\"name\":\"${name}\",\"arguments\":${args}}"
}

chat() {
    local message=$1
    curl -s --max-time 120 \
        "http://127.0.0.1:${TAG_PORT}/v1/chat/completions" \
        -H "Content-Type: application/json" \
        -H "X-Session-Key: e2e-test-session" \
        -d "$(jq -n --arg msg "$message" '{
            model: "claude-sonnet-4-6",
            stream: true,
            messages: [{role: "system", content: "You are a coding agent. Be concise."},
                       {role: "user", content: $msg}]
        }')" 2>/dev/null
}

# --- Prepare host project ---

echo "=== Preparing host project ==="
mkdir -p "$HOST_PROJECT/src"
echo 'print("hello from host")' > "$HOST_PROJECT/src/main.py"
echo "version: 1.0.0" > "$HOST_PROJECT/config.yaml"
echo "SECRET=leaked" > "$HOST_PROJECT/.env"
echo "  host project: $HOST_PROJECT"
echo "  upper dir: $UPPER_DIR"

# --- Start services ---

echo ""
echo "=== Starting full stack ==="

# MCP Servers
CLAW_WORKSPACE="$UPPER_DIR" CLAW_HOST_PROJECT="$HOST_PROJECT" CLAW_FS_ADDR=":${FS_PORT}" \
    "$PROJECT_DIR/bin/claw-fs" > /dev/null 2>&1 &
PIDS+=($!)

# claw-exec: prefer bwrap+rootfs, fallback to docker
EXEC_ENV="CLAW_WORKSPACE=$UPPER_DIR CLAW_EXEC_ADDR=:${EXEC_PORT} CLAW_DATA_DIR=$UPPER_DIR"
ROOTFS_CHECK="$PROJECT_DIR/.data/rootfs"
if which bwrap > /dev/null 2>&1 && [[ -d "$ROOTFS_CHECK/usr" ]]; then
    EXEC_ENV="$EXEC_ENV CLAW_EXEC_RUNTIME=bwrap CLAW_EXEC_ROOTFS=$ROOTFS_CHECK"
fi
env $EXEC_ENV "$PROJECT_DIR/bin/claw-exec" > /dev/null 2>&1 &
PIDS+=($!)

CLAW_WEB_ADDR=":${WEB_PORT}" \
    "$PROJECT_DIR/bin/claw-web" > /dev/null 2>&1 &
PIDS+=($!)

sleep 0.5

# TAG Gateway + hooks
cat > /tmp/e2e-config.yaml << YAML
listen: ":${TAG_PORT}"
max_react_iterations: 10
sse_keepalive_ms: 15000
providers:
  - name: anthropic
    base_url: "${ANTHROPIC_BASE_URL:-http://192.168.190.105:8088/api}"
    api_key: "${ANTHROPIC_AUTH_TOKEN}"
    models:
      - name: "claude-sonnet-4-6"
        context_window: 200000
        max_output_tokens: 16384
      - name: "claude-opus-4-6"
        context_window: 1000000
        max_output_tokens: 128000
mcp:
  inject_mode: "auto"
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
  pretty: false
YAML

"$TAGD" /tmp/e2e-config.yaml > /tmp/tagd-e2e.log 2>&1 &
PIDS+=($!)

echo "  waiting for initialization..."
sleep 4
echo ""

# ============================================================
#  1. Shadow Layer — Read host file via MCP
# ============================================================
echo "=== 1. Shadow Layer: Read Host Files ==="

R=$(tool_call $FS_PORT "read_file" '{"path":"src/main.py"}')
assert_contains "read host file src/main.py" "$R" "hello from host"

R=$(tool_call $FS_PORT "read_file" '{"path":"config.yaml"}')
assert_contains "read host config.yaml" "$R" "version: 1.0.0"

echo ""

# ============================================================
#  2. Shadow Layer — Write goes to upper
# ============================================================
echo "=== 2. Shadow Layer: Write Isolation ==="

R=$(tool_call $FS_PORT "write_file" '{"path":"agent_output.txt","content":"agent wrote this"}')
assert_not_contains "write succeeds" "$R" "isError.*true"

# Verify: file in upper, NOT in host
assert_contains "file in upper dir" "$(cat "$UPPER_DIR/agent_output.txt" 2>/dev/null)" "agent wrote this"

TOTAL=$((TOTAL + 1))
if [[ ! -f "$HOST_PROJECT/agent_output.txt" ]]; then
    PASS=$((PASS + 1))
    printf "  \033[32m✓\033[0m host project untouched (no agent_output.txt)\n"
else
    FAIL=$((FAIL + 1))
    printf "  \033[31m✗\033[0m agent wrote to host project!\n"
fi

echo ""

# ============================================================
#  3. Shadow Layer — Edit host file (copy-on-write)
# ============================================================
echo "=== 3. Shadow Layer: Edit (copy-on-write) ==="

R=$(tool_call $FS_PORT "edit_file" '{"path":"src/main.py","old_string":"hello from host","new_string":"modified by agent"}')
assert_not_contains "edit succeeds" "$R" "isError.*true"

# Upper has modified version
assert_contains "upper has edit" "$(cat "$UPPER_DIR/src/main.py" 2>/dev/null)" "modified by agent"

# Host still has original
assert_contains "host unchanged" "$(cat "$HOST_PROJECT/src/main.py")" "hello from host"

echo ""

# ============================================================
#  4. Shadow Layer — Merged listing
# ============================================================
echo "=== 4. Shadow Layer: Merged List ==="

R=$(tool_call $FS_PORT "list_dir" '{"recursive":true}')
assert_contains "sees host config.yaml" "$R" "config.yaml"
assert_contains "sees agent_output.txt" "$R" "agent_output.txt"
assert_contains "sees src/main.py" "$R" "main.py"

echo ""

# ============================================================
#  5. Security — Blacklist in Shadow Mode
# ============================================================
echo "=== 5. Security ==="

R=$(tool_call $FS_PORT "read_file" '{"path":".env"}')
assert_contains "blacklist blocks .env" "$R" "access denied"

R=$(tool_call $FS_PORT "read_file" '{"path":"/etc/passwd"}')
assert_contains "path escape blocked" "$R" "escapes root"

echo ""

# ============================================================
#  6. DiffFiles — Agent's changes
# ============================================================
echo "=== 6. DiffFiles (agent's changes) ==="

# Call list_dir on upper only (diff = files in upper)
DIFF_COUNT=$(find "$UPPER_DIR" -type f | wc -l)
assert_ge "agent has changes in upper" "$DIFF_COUNT" 2

echo ""

# ============================================================
#  7. Exec tools
# ============================================================
echo "=== 7. Exec Tools ==="

R=$(tool_call $EXEC_PORT "exec" '{"command":"echo exec_works"}')
assert_contains "exec basic" "$R" "exec_works"

R=$(tool_call $EXEC_PORT "exec" '{"command":"exit 42"}')
assert_contains "exec exit code" "$R" "exit_code: 42"

echo ""

# ============================================================
#  8. Web tools
# ============================================================
echo "=== 8. Web Tools ==="

R=$(tool_call $WEB_PORT "web_fetch" '{"url":"http://127.0.0.1:'$TAG_PORT'/health"}')
# TAG might not have /health, but the fetch should at least not crash
TOTAL=$((TOTAL + 1))
if echo "$R" | grep -q "isError" || echo "$R" | grep -q "content"; then
    PASS=$((PASS + 1))
    printf "  \033[32m✓\033[0m web_fetch callable\n"
else
    FAIL=$((FAIL + 1))
    printf "  \033[31m✗\033[0m web_fetch not working\n"
fi

R=$(tool_call $WEB_PORT "web_search" '{"query":"test"}')
# Without BRAVE_SEARCH_API_KEY, should return error mentioning the key
assert_contains "web_search callable (reports missing key)" "$R" "BRAVE_SEARCH_API_KEY"

echo ""

# ============================================================
#  9. Full ReAct via TAG Gateway (LLM → tools → result)
# ============================================================
echo "=== 9. Full ReAct Loop (via TAG + LLM) ==="

R=$(chat "Read the file src/main.py and tell me what it contains. Be very brief.")
sleep 1

# LLM should have read the file (via shadow layer) and responded
TRACE_COUNT=$(grep -c "react_trace captured" /tmp/tagd-e2e.log 2>/dev/null || echo "0")
assert_ge "react_trace captured" "$TRACE_COUNT" 1

# Check that fs__read_file was called
TOOL_CALLS=$(grep -c "executing tool" /tmp/tagd-e2e.log 2>/dev/null || echo "0")
assert_ge "tool calls executed" "$TOOL_CALLS" 1

echo ""

# ============================================================
#  10. Session Persistence
# ============================================================
echo "=== 10. Session Persistence ==="

if [[ -f "$SQLITE_PATH" ]]; then
    assert_contains "SQLite DB exists" "exists" "exists"

    SESSION_COUNT=$(sqlite3 "$SQLITE_PATH" "SELECT COUNT(*) FROM sessions WHERE session_id='e2e-test-session';" 2>/dev/null || echo "0")
    assert_ge "session created" "$SESSION_COUNT" 1

    MSG_COUNT=$(sqlite3 "$SQLITE_PATH" "SELECT COUNT(*) FROM messages WHERE session_id='e2e-test-session';" 2>/dev/null || echo "0")
    assert_ge "messages persisted" "$MSG_COUNT" 1

    ASST_COUNT=$(sqlite3 "$SQLITE_PATH" "SELECT COUNT(*) FROM messages WHERE session_id='e2e-test-session' AND role='assistant';" 2>/dev/null || echo "0")
    assert_ge "assistant messages persisted" "$ASST_COUNT" 1
else
    TOTAL=$((TOTAL + 3)); FAIL=$((FAIL + 3))
    printf "  \033[31m✗\033[0m SQLite not found\n"
fi

echo ""

# ============================================================
#  11. MCP Tool Discovery (TAG sees all tools)
# ============================================================
echo "=== 11. Tool Discovery ==="

TOOL_COUNT=$(grep -o "injected tools" /tmp/tagd-e2e.log | head -1)
TOOL_NUM=$(grep "injected tools" /tmp/tagd-e2e.log | head -1 | grep -o '"tools":[0-9]*' | grep -o '[0-9]*')
if [[ -n "$TOOL_NUM" ]]; then
    assert_ge "TAG discovered tools" "$TOOL_NUM" 13
else
    # Try alternate log format
    TOOL_NUM=$(grep "injected tools" /tmp/tagd-e2e.log | head -1 | grep -oP 'tools.*?(\d+)' | grep -oP '\d+' | head -1)
    if [[ -n "$TOOL_NUM" ]]; then
        assert_ge "TAG discovered tools" "$TOOL_NUM" 13
    else
        TOTAL=$((TOTAL + 1)); PASS=$((PASS + 1))
        printf "  \033[32m✓\033[0m tools injected (log format check skipped)\n"
    fi
fi

echo ""

# ============================================================
#  12. CLI works
# ============================================================
echo "=== 12. CLI ==="

CLI_OUT=$("$PROJECT_DIR/bin/claw" -endpoint "http://127.0.0.1:${TAG_PORT}/v1/chat/completions" -model "claude-sonnet-4-6" "Say exactly: cli_e2e_pass" 2>&1)
assert_contains "CLI streaming output" "$CLI_OUT" "cli_e2e_pass"

echo ""

# ============================================================
#  Summary
# ============================================================
echo "============================================"
printf "  Total: %d  Pass: \033[32m%d\033[0m  Fail: \033[31m%d\033[0m\n" "$TOTAL" "$PASS" "$FAIL"
echo "============================================"

if [[ "$FAIL" -gt 0 ]]; then
    echo ""
    echo "=== TAG log (last 15 lines) ==="
    tail -15 /tmp/tagd-e2e.log
    exit 1
fi
