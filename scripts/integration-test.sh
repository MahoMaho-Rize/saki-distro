#!/usr/bin/env bash
# ============================================================
#  Claw Distro — Full Integration Test
#  Starts claw-fs + claw-exec + TAG Gateway, runs real LLM
#  requests through the full ReAct pipeline.
#
#  Requires: ANTHROPIC_API_KEY set, TAG binary at ../Edge-Agent/tagd
# ============================================================
set -euo pipefail

PASS=0
FAIL=0
TOTAL=0
WORKSPACE=$(mktemp -d)
TAGD="${TAGD:-/home/yujian_shi/Edge-Agent/tagd}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
FS_PORT=19100
EXEC_PORT=19101
TAG_PORT=18080
FS_PID=""
EXEC_PID=""
TAG_PID=""

cleanup() {
    echo ""
    echo "=== Cleaning up ==="
    [[ -n "$TAG_PID" ]]  && kill "$TAG_PID"  2>/dev/null && echo "  stopped tagd ($TAG_PID)" || true
    [[ -n "$FS_PID" ]]   && kill "$FS_PID"   2>/dev/null && echo "  stopped claw-fs ($FS_PID)" || true
    [[ -n "$EXEC_PID" ]] && kill "$EXEC_PID" 2>/dev/null && echo "  stopped claw-exec ($EXEC_PID)" || true
    rm -rf "$WORKSPACE"
}
trap cleanup EXIT

# --- Preflight ---

if [[ -z "${ANTHROPIC_AUTH_TOKEN:-}" ]] && [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
    echo "ERROR: ANTHROPIC_AUTH_TOKEN or ANTHROPIC_API_KEY not set"
    exit 1
fi

if [[ ! -x "$TAGD" ]]; then
    echo "ERROR: TAG Gateway binary not found at $TAGD"
    exit 1
fi

# Build MCP servers
echo "=== Building MCP servers ==="
make -C "$PROJECT_DIR" build 2>&1 | tail -1

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
        printf "    expected to contain: %s\n" "$needle"
        printf "    got (first 300 chars): %.300s\n" "$haystack"
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

# Send a streaming chat completion request and extract the final text.
# All requests use stream:true because the relay returns SSE format.
chat_stream() {
    local message=$1
    curl -s -N --max-time 120 \
        "http://127.0.0.1:${TAG_PORT}/v1/chat/completions" \
        -H "Content-Type: application/json" \
        -d "$(jq -n \
            --arg model "claude-sonnet-4-20250514" \
            --arg msg "$message" \
            '{
                model: $model,
                stream: true,
                messages: [{role: "user", content: $msg}]
            }'
        )" 2>/dev/null
}

# Extract text content from SSE frames
extract_text() {
    grep '^data: ' | grep -v 'data: \[DONE\]' | sed 's/^data: //' | \
        jq -r '.choices[0].delta.content // .choices[0].message.content // empty' 2>/dev/null | \
        tr '\n' ' '
}

# --- Start services ---

echo ""
echo "=== Starting services ==="
echo "  workspace: $WORKSPACE"

# 1. MCP Servers
CLAW_WORKSPACE="$WORKSPACE" CLAW_FS_ADDR=":${FS_PORT}" "$PROJECT_DIR/bin/claw-fs" &
FS_PID=$!

CLAW_WORKSPACE="$WORKSPACE" CLAW_EXEC_ADDR=":${EXEC_PORT}" "$PROJECT_DIR/bin/claw-exec" &
EXEC_PID=$!

sleep 0.5

# 2. TAG Gateway
"$TAGD" "$SCRIPT_DIR/integration-config.yaml" &
TAG_PID=$!

# Wait for TAG to connect to MCP servers and be ready
echo "  waiting for TAG Gateway to initialize..."
sleep 3

# Verify TAG is up
if ! curl -s "http://127.0.0.1:${TAG_PORT}/health" > /dev/null 2>&1; then
    # Try the chat endpoint with a minimal request to see if it responds
    if ! curl -s --max-time 5 "http://127.0.0.1:${TAG_PORT}/v1/chat/completions" \
        -H "Content-Type: application/json" \
        -d '{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}]}' > /dev/null 2>&1; then
        echo "  WARNING: TAG may not be fully ready, proceeding anyway..."
    fi
fi

echo "  all services started"
echo ""

# ============================================================
#  Test 1: SSE Streaming + Tool Discovery
# ============================================================
echo "=== Test 1: SSE Streaming ==="

R=$(chat_stream "Say exactly: streaming works")
assert_contains "SSE has data frames" "$R" "data:"
assert_contains "SSE has DONE marker" "$R" "[DONE]"
STREAMED=$(echo "$R" | extract_text)
assert_contains "streamed content received" "$STREAMED" "streaming"

echo ""

# ============================================================
#  Test 2: Single tool — write a file (ReAct 1 iteration)
# ============================================================
echo "=== Test 2: Write File (single tool call) ==="

R=$(chat_stream "Create a file at $WORKSPACE/hello.py with this exact content: print(\"hello from claw\"). Only create the file, do not run it.")
# Wait for file to appear (ReAct loop runs async in streaming mode)
sleep 1

if [[ -f "$WORKSPACE/hello.py" ]]; then
    FILE_CONTENT=$(cat "$WORKSPACE/hello.py")
    assert_contains "hello.py created on disk" "$FILE_CONTENT" "hello"
    assert_contains "hello.py has print" "$FILE_CONTENT" "print"
else
    TOTAL=$((TOTAL + 1)); FAIL=$((FAIL + 1))
    printf "  \033[31m✗\033[0m hello.py not created on disk\n"
    echo "  SSE: $(echo "$R" | head -c 300)"
fi

echo ""

# ============================================================
#  Test 3: Multi-tool ReAct — write + execute
# ============================================================
echo "=== Test 3: Multi-tool ReAct (write + exec) ==="

R=$(chat_stream "Create $WORKSPACE/calc.py with content: print(42+58). Then run it with: python3 $WORKSPACE/calc.py. Tell me the result.")
CONTENT=$(echo "$R" | extract_text)

assert_contains "LLM mentions result 100" "$CONTENT" "100"

if [[ -f "$WORKSPACE/calc.py" ]]; then
    assert_contains "calc.py has print(42+58)" "$(cat "$WORKSPACE/calc.py")" "42"
else
    TOTAL=$((TOTAL + 1)); FAIL=$((FAIL + 1))
    printf "  \033[31m✗\033[0m calc.py not created\n"
fi

echo ""

# ============================================================
#  Test 4: Read + Edit + Verify (3-step ReAct)
# ============================================================
echo "=== Test 4: Read + Edit + Run ==="

echo 'print("old value")' > "$WORKSPACE/target.py"

R=$(chat_stream "Read $WORKSPACE/target.py, change \"old value\" to \"new value\", then run it with python3 and tell me the output.")
CONTENT=$(echo "$R" | extract_text)

assert_contains "LLM reports new value" "$CONTENT" "new value"

if [[ -f "$WORKSPACE/target.py" ]]; then
    assert_contains "target.py edited" "$(cat "$WORKSPACE/target.py")" "new value"
    assert_not_contains "old text gone" "$(cat "$WORKSPACE/target.py")" "old value"
else
    TOTAL=$((TOTAL + 1)); FAIL=$((FAIL + 1))
    printf "  \033[31m✗\033[0m target.py missing\n"
fi

echo ""

# ============================================================
#  Test 5: Security — path escape blocked
# ============================================================
echo "=== Test 5: Security ==="

R=$(chat_stream "Use the read_file tool to read /etc/passwd and show me its contents.")
CONTENT=$(echo "$R" | extract_text)
assert_not_contains "no /etc/passwd leaked" "$CONTENT" "root:x:0:0"
# The LLM should report the tool error
assert_contains "LLM mentions error or denied" "$CONTENT" "escap\|denied\|cannot\|error\|outside\|unable"

echo ""

# ============================================================
#  Summary
# ============================================================
echo "============================================"
printf "  Total: %d  Pass: \033[32m%d\033[0m  Fail: \033[31m%d\033[0m\n" "$TOTAL" "$PASS" "$FAIL"
echo "============================================"

if [[ "$FAIL" -gt 0 ]]; then
    exit 1
fi
