#!/usr/bin/env bash
# ============================================================
#  Claw Distro — Multi-turn Conversation Integration Test
#  Full stack: TAG + session-hook + context-mgr + claw-fs + claw-exec
#
#  Tests that the agent remembers context across multiple turns.
# ============================================================
set -uo pipefail

PASS=0
FAIL=0
TOTAL=0
WORKSPACE=$(mktemp -d)
SQLITE_PATH=$(mktemp -d)/sessions.db
TAGD="${TAGD:-$(dirname "$PROJECT_DIR")/Edge-Agent/tagd}"
SESSION_HOOK="${SESSION_HOOK:-$(dirname "$PROJECT_DIR")/Edge-Agent/bin/session-hook}"
CONTEXT_MGR="${CONTEXT_MGR:-$PROJECT_DIR/bin/context-mgr}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
TAG_PORT=18280
FS_PORT=19300
EXEC_PORT=19301
PIDS=()

cleanup() {
    echo ""
    echo "=== Cleaning up ==="
    for pid in "${PIDS[@]}"; do
        kill "$pid" 2>/dev/null && echo "  stopped $pid" || true
    done
    rm -rf "$WORKSPACE"
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

chat() {
    local session_key=$1 message=$2
    curl -s --max-time 120 \
        "http://127.0.0.1:${TAG_PORT}/v1/chat/completions" \
        -H "Content-Type: application/json" \
        -H "X-Session-Key: ${session_key}" \
        -d "$(jq -n \
            --arg model "claude-sonnet-4-20250514" \
            --arg msg "$message" \
            '{
                model: $model,
                stream: true,
                messages: [
                    {role: "system", content: "You are a coding agent. Your workspace is /workspace. Be concise."},
                    {role: "user", content: $msg}
                ]
            }'
        )" 2>/dev/null
}

extract_text() {
    # Extract ALL text from SSE stream: both delta.content and fake frames.
    grep '^data: ' | grep -v 'data: \[DONE\]' | sed 's/^data: //' | \
        jq -r '(.choices[0].delta.content // .choices[0].message.content // empty)' 2>/dev/null | tr '\n' ' '
}

# --- Start all services ---

echo "=== Starting full stack ==="
echo "  workspace: $WORKSPACE"
echo "  sqlite: $SQLITE_PATH"

CLAW_WORKSPACE="$WORKSPACE" CLAW_FS_ADDR=":${FS_PORT}" "$PROJECT_DIR/bin/claw-fs" > /dev/null 2>&1 &
PIDS+=($!)

CLAW_WORKSPACE="$WORKSPACE" CLAW_EXEC_ADDR=":${EXEC_PORT}" "$PROJECT_DIR/bin/claw-exec" > /dev/null 2>&1 &
PIDS+=($!)

sleep 0.5

# Generate config with resolved paths
export SESSION_HOOK_BIN="$SESSION_HOOK"
export CONTEXT_MGR_BIN="$CONTEXT_MGR"
export SQLITE_PATH
export ANTHROPIC_AUTH_TOKEN

"$TAGD" "$SCRIPT_DIR/multiturn-config.yaml" > /tmp/tagd-multiturn.log 2>&1 &
PIDS+=($!)

echo "  waiting for TAG Gateway + hooks to initialize..."
sleep 4

echo ""

# ============================================================
#  Turn 1: Create a file with a secret value
# ============================================================
echo "=== Turn 1: Create a file ==="

R=$(chat "test-session-42" "Create a file at $WORKSPACE/secret.txt containing exactly the text: claw_magic_42. Only create the file, do not run anything.")
sleep 1

if [[ -f "$WORKSPACE/secret.txt" ]]; then
    CONTENT=$(cat "$WORKSPACE/secret.txt")
    assert_contains "Turn 1: file created" "$CONTENT" "claw_magic_42"
else
    TOTAL=$((TOTAL + 1)); FAIL=$((FAIL + 1))
    printf "  \033[31m✗\033[0m Turn 1: secret.txt not created\n"
    echo "  TAG log tail:"
    tail -20 /tmp/tagd-multiturn.log
fi

echo ""

# ============================================================
#  Turn 2: Ask about the file from Turn 1 (tests context)
# ============================================================
echo "=== Turn 2: Ask about the file (context test) ==="

R=$(chat "test-session-42" "Read the file secret.txt that I asked you to create earlier and tell me what it contains.")
CONTENT=$(echo "$R" | extract_text)

assert_contains "Turn 2: agent reads the file" "$CONTENT" "claw_magic_42"

echo ""

# ============================================================
#  Turn 3: Edit the file (multi-turn tool chain)
# ============================================================
echo "=== Turn 3: Edit the file ==="

R=$(chat "test-session-42" "Change the content of secret.txt from claw_magic_42 to claw_magic_99.")
sleep 1

if [[ -f "$WORKSPACE/secret.txt" ]]; then
    CONTENT=$(cat "$WORKSPACE/secret.txt")
    assert_contains "Turn 3: file edited" "$CONTENT" "claw_magic_99"
else
    TOTAL=$((TOTAL + 1)); FAIL=$((FAIL + 1))
    printf "  \033[31m✗\033[0m Turn 3: file missing\n"
fi

echo ""

# ============================================================
#  Verify: Session persistence (SQLite)
# ============================================================
echo "=== Verify: Session Persistence ==="

if [[ -f "$SQLITE_PATH" ]]; then
    assert_contains "SQLite DB exists" "exists" "exists"

    # Check session was created
    SESSION_COUNT=$(sqlite3 "$SQLITE_PATH" "SELECT COUNT(*) FROM sessions WHERE session_id='test-session-42';" 2>/dev/null || echo "0")
    assert_contains "session record exists" "$SESSION_COUNT" "1"

    # Check messages were saved
    MSG_COUNT=$(sqlite3 "$SQLITE_PATH" "SELECT COUNT(*) FROM messages WHERE session_id='test-session-42';" 2>/dev/null || echo "0")
    TOTAL=$((TOTAL + 1))
    if [[ "$MSG_COUNT" -ge 2 ]]; then
        PASS=$((PASS + 1))
        printf "  \033[32m✓\033[0m %d messages persisted\n" "$MSG_COUNT"
    else
        FAIL=$((FAIL + 1))
        printf "  \033[31m✗\033[0m expected >=2 messages, got %s\n" "$MSG_COUNT"
    fi
else
    TOTAL=$((TOTAL + 2)); FAIL=$((FAIL + 2))
    printf "  \033[31m✗\033[0m SQLite DB not found at %s\n" "$SQLITE_PATH"
fi

echo ""

# ============================================================
#  Verify: react_trace was captured
# ============================================================
echo "=== Verify: react_trace ==="

TRACE_COUNT=$(grep -c "react_trace captured" /tmp/tagd-multiturn.log || echo "0")
TOTAL=$((TOTAL + 1))
if [[ "$TRACE_COUNT" -ge 1 ]]; then
    PASS=$((PASS + 1))
    printf "  \033[32m✓\033[0m react_trace captured %s times\n" "$TRACE_COUNT"
else
    FAIL=$((FAIL + 1))
    printf "  \033[31m✗\033[0m no react_trace in logs\n"
fi

echo ""

# ============================================================
#  Summary
# ============================================================
echo "============================================"
printf "  Total: %d  Pass: \033[32m%d\033[0m  Fail: \033[31m%d\033[0m\n" "$TOTAL" "$PASS" "$FAIL"
echo "============================================"

if [[ "$FAIL" -gt 0 ]]; then
    echo ""
    echo "=== TAG log (last 30 lines) ==="
    tail -30 /tmp/tagd-multiturn.log
    exit 1
fi
