#!/usr/bin/env bash
# ============================================================
#  Claw Distro — Gray-scale Integration Test
#  Starts MCP servers, runs OpenClaw-equivalent test scenarios
#  via raw MCP protocol (curl), reports pass/fail.
# ============================================================
set -euo pipefail

PASS=0
FAIL=0
TOTAL=0
WORKSPACE=$(mktemp -d)
FS_PORT=19100
EXEC_PORT=19101
FS_PID=""
EXEC_PID=""

cleanup() {
    [[ -n "$FS_PID" ]]   && kill "$FS_PID"   2>/dev/null || true
    [[ -n "$EXEC_PID" ]] && kill "$EXEC_PID" 2>/dev/null || true
    rm -rf "$WORKSPACE"
}
trap cleanup EXIT

# --- Helpers ---

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

assert_contains() {
    local label=$1 haystack=$2 needle=$3
    TOTAL=$((TOTAL + 1))
    if echo "$haystack" | grep -q "$needle"; then
        PASS=$((PASS + 1))
        printf "  \033[32m✓\033[0m %s\n" "$label"
    else
        FAIL=$((FAIL + 1))
        printf "  \033[31m✗\033[0m %s\n" "$label"
        printf "    expected to contain: %s\n" "$needle"
        printf "    got: %.200s\n" "$haystack"
    fi
}

assert_not_contains() {
    local label=$1 haystack=$2 needle=$3
    TOTAL=$((TOTAL + 1))
    if ! echo "$haystack" | grep -q "$needle"; then
        PASS=$((PASS + 1))
        printf "  \033[32m✓\033[0m %s\n" "$label"
    else
        FAIL=$((FAIL + 1))
        printf "  \033[31m✗\033[0m %s\n" "$label"
        printf "    expected NOT to contain: %s\n" "$needle"
    fi
}

# --- Start servers ---

echo "=== Starting MCP servers ==="
echo "  workspace: $WORKSPACE"

CLAW_WORKSPACE="$WORKSPACE" CLAW_FS_ADDR=":${FS_PORT}" ./bin/claw-fs &
FS_PID=$!

CLAW_WORKSPACE="$WORKSPACE" CLAW_EXEC_ADDR=":${EXEC_PORT}" ./bin/claw-exec &
EXEC_PID=$!

sleep 0.5  # wait for servers to bind

echo ""

# ============================================================
#  1. MCP Protocol Compliance
# ============================================================
echo "=== 1. MCP Protocol Compliance ==="

# 1a. Initialize
R=$(mcp_call $FS_PORT "initialize" '{"protocolVersion":"2025-03-26","clientInfo":{"name":"test","version":"0.1"}}')
assert_contains "initialize returns protocol version" "$R" "2025-03-26"
assert_contains "initialize returns server name" "$R" "claw-fs"

# 1b. tools/list — claw-fs should expose 6 tools
R=$(mcp_call $FS_PORT "tools/list" '{}')
assert_contains "fs: has read_file" "$R" "read_file"
assert_contains "fs: has write_file" "$R" "write_file"
assert_contains "fs: has edit_file" "$R" "edit_file"
assert_contains "fs: has list_dir" "$R" "list_dir"
assert_contains "fs: has glob" "$R" "glob"
assert_contains "fs: has grep" "$R" "grep"

# 1c. tools/list — claw-exec should expose 4 tools
R=$(mcp_call $EXEC_PORT "tools/list" '{}')
assert_contains "exec: has exec" "$R" '"exec"'
assert_contains "exec: has process_start" "$R" "process_start"
assert_contains "exec: has process_send" "$R" "process_send"
assert_contains "exec: has process_poll" "$R" "process_poll"

echo ""

# ============================================================
#  2. Core Scenario: write → edit → read → exec (OpenClaw pi-tools)
# ============================================================
echo "=== 2. Core: write → edit → read → exec ==="

# 2a. Write a Python file
R=$(tool_call $FS_PORT "write_file" '{"path":"hello.py","content":"print(\"hello world\")"}')
assert_not_contains "write hello.py succeeds" "$R" '"isError":true'

# 2b. Read it back
R=$(tool_call $FS_PORT "read_file" '{"path":"hello.py"}')
assert_contains "read hello.py contains content" "$R" "hello world"

# 2c. Edit: change "world" to "universe"
R=$(tool_call $FS_PORT "edit_file" '{"path":"hello.py","old_string":"world","new_string":"universe"}')
assert_not_contains "edit succeeds" "$R" '"isError":true'

# 2d. Read back and verify edit
R=$(tool_call $FS_PORT "read_file" '{"path":"hello.py"}')
assert_contains "edit applied correctly" "$R" "hello universe"
assert_not_contains "old text removed" "$R" "hello world"

# 2e. Execute the script
R=$(tool_call $EXEC_PORT "exec" '{"command":"python3 '"$WORKSPACE"'/hello.py"}')
assert_contains "exec runs python" "$R" "hello universe"
assert_contains "exec exit code 0" "$R" "exit_code: 0"

echo ""

# ============================================================
#  3. File tool edge cases (OpenClaw pi-tools tests)
# ============================================================
echo "=== 3. File Tool Edge Cases ==="

# 3a. Edit fails on non-unique match
tool_call $FS_PORT "write_file" '{"path":"dup.txt","content":"aaa bbb aaa"}' > /dev/null
R=$(tool_call $FS_PORT "edit_file" '{"path":"dup.txt","old_string":"aaa","new_string":"ccc"}')
assert_contains "edit rejects non-unique match" "$R" "2 times"

# 3b. Edit fails on no match
tool_call $FS_PORT "write_file" '{"path":"miss.txt","content":"hello"}' > /dev/null
R=$(tool_call $FS_PORT "edit_file" '{"path":"miss.txt","old_string":"nonexistent","new_string":"x"}')
assert_contains "edit rejects no match" "$R" "not found"

# 3c. Read with offset/limit
tool_call $FS_PORT "write_file" '{"path":"lines.txt","content":"line1\nline2\nline3\nline4\nline5"}' > /dev/null
R=$(tool_call $FS_PORT "read_file" '{"path":"lines.txt","offset":2,"limit":2}')
assert_contains "offset/limit: has line2" "$R" "line2"
assert_contains "offset/limit: has line3" "$R" "line3"
assert_not_contains "offset/limit: no line1" "$R" "line1"

# 3d. Write creates parent dirs
R=$(tool_call $FS_PORT "write_file" '{"path":"deep/nested/file.txt","content":"deep content"}')
assert_not_contains "nested write succeeds" "$R" '"isError":true'
R=$(tool_call $FS_PORT "read_file" '{"path":"deep/nested/file.txt"}')
assert_contains "nested read works" "$R" "deep content"

# 3e. list_dir
R=$(tool_call $FS_PORT "list_dir" '{}')
assert_contains "list_dir: sees hello.py" "$R" "hello.py"
assert_contains "list_dir: sees deep/" "$R" "deep/"

# 3f. grep
R=$(tool_call $FS_PORT "grep" '{"pattern":"universe"}')
assert_contains "grep finds match" "$R" "hello.py"

# 3g. glob
tool_call $FS_PORT "write_file" '{"path":"a.go","content":"package a"}' > /dev/null
tool_call $FS_PORT "write_file" '{"path":"b.go","content":"package b"}' > /dev/null
R=$(tool_call $FS_PORT "glob" '{"pattern":"*.go"}')
assert_contains "glob finds .go files" "$R" "a.go"
assert_contains "glob finds b.go" "$R" "b.go"

echo ""

# ============================================================
#  4. Security (OpenClaw sandbox-paths.test.ts)
# ============================================================
echo "=== 4. Security ==="

# 4a. Path traversal — /etc/passwd
R=$(tool_call $FS_PORT "read_file" '{"path":"/etc/passwd"}')
assert_contains "blocks /etc/passwd" "$R" "escapes root"

# 4b. Path traversal — ../
R=$(tool_call $FS_PORT "read_file" '{"path":"../../../etc/passwd"}')
assert_contains "blocks ../ traversal" "$R" "escapes root"

# 4c. Blacklist — .env
echo "SECRET=value" > "$WORKSPACE/.env"
R=$(tool_call $FS_PORT "read_file" '{"path":".env"}')
assert_contains "blocks .env read" "$R" "access denied"

# 4d. Blacklist — credentials.json
echo '{}' > "$WORKSPACE/credentials.json"
R=$(tool_call $FS_PORT "read_file" '{"path":"credentials.json"}')
assert_contains "blocks credentials.json" "$R" "access denied"

echo ""

# ============================================================
#  5. Exec edge cases (OpenClaw bash-tools.test.ts)
# ============================================================
echo "=== 5. Exec Edge Cases ==="

# 5a. Non-zero exit code
R=$(tool_call $EXEC_PORT "exec" '{"command":"echo nope; exit 1"}')
assert_contains "non-zero: has output" "$R" "nope"
assert_contains "non-zero: exit code 1" "$R" "exit_code: 1"

# 5b. Stderr capture
R=$(tool_call $EXEC_PORT "exec" '{"command":"echo out; echo err >&2"}')
assert_contains "captures stdout" "$R" "out"
assert_contains "captures stderr" "$R" "err"

# 5c. Timeout
R=$(tool_call $EXEC_PORT "exec" '{"command":"sleep 10","timeout_ms":500}')
assert_contains "timeout kills process" "$R" "exit_code:"

# 5d. Working dir
mkdir -p "$WORKSPACE/subdir"
echo "sub content" > "$WORKSPACE/subdir/test.txt"
R=$(tool_call $EXEC_PORT "exec" "{\"command\":\"cat test.txt\",\"working_dir\":\"$WORKSPACE/subdir\"}")
assert_contains "working_dir respected" "$R" "sub content"

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
