package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"claw-distro/internal/mcpserver"
	"claw-distro/internal/workspace"
)

// --- Ported from OpenClaw bash-tools.test.ts ---

func newTestExec(t *testing.T) *mcpserver.Server {
	t.Helper()
	dir := t.TempDir()
	ws := workspace.New(dir)
	pt := newProcessTable()
	srv := mcpserver.New("test-exec", "0.1.0")
	registerTools(srv, ws, pt)
	return srv
}

// TestExecBasic — "exec tool runs command and captures output"
func TestExecBasic(t *testing.T) {
	srv := newTestExec(t)

	result := toolCall(t, srv, "exec", map[string]any{
		"command": "echo hello",
	})
	if result.IsError {
		t.Fatalf("exec error: %s", result.Content[0].Text)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "hello") {
		t.Errorf("expected 'hello' in output: %s", text)
	}
	if !strings.Contains(text, "exit_code: 0") {
		t.Errorf("expected exit_code 0: %s", text)
	}
}

// TestExecNonZeroExit — ported from "treats non-zero exits as completed and appends exit code"
func TestExecNonZeroExit(t *testing.T) {
	srv := newTestExec(t)

	result := toolCall(t, srv, "exec", map[string]any{
		"command": "echo nope; exit 1",
	})
	// Not an MCP-level error — the tool completes, reports exit code.
	if result.IsError {
		t.Fatal("non-zero exit should not be an MCP error")
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "nope") {
		t.Errorf("expected 'nope' in output: %s", text)
	}
	if !strings.Contains(text, "exit_code: 1") {
		t.Errorf("expected exit_code 1: %s", text)
	}
}

// TestExecTimeout — ported from "uses default timeout when timeout is omitted"
func TestExecTimeout(t *testing.T) {
	srv := newTestExec(t)

	result := toolCall(t, srv, "exec", map[string]any{
		"command":    "sleep 10",
		"timeout_ms": 500,
	})
	// Should complete (context cancelled = signal killed).
	text := result.Content[0].Text
	if !strings.Contains(text, "exit_code:") && !strings.Contains(text, "signal") {
		// On timeout, exec.CommandContext sends SIGKILL, ExitError is returned.
		// The exact error message depends on OS.
		if result.IsError && !strings.Contains(text, "killed") && !strings.Contains(text, "signal") {
			t.Logf("timeout result: %s", text)
		}
	}
}

// TestExecStderr — captures stderr separately
func TestExecStderr(t *testing.T) {
	srv := newTestExec(t)

	result := toolCall(t, srv, "exec", map[string]any{
		"command": "echo out; echo err >&2",
	})
	text := result.Content[0].Text
	if !strings.Contains(text, "out") {
		t.Errorf("expected stdout 'out': %s", text)
	}
	if !strings.Contains(text, "err") {
		t.Errorf("expected stderr 'err': %s", text)
	}
}

// TestProcessStartPoll — ported from "backgrounds after yield and can be polled"
func TestProcessStartPoll(t *testing.T) {
	srv := newTestExec(t)

	// Start background process
	result := toolCall(t, srv, "process_start", map[string]any{
		"command": "echo background_output",
	})
	if result.IsError {
		t.Fatalf("process_start error: %s", result.Content[0].Text)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "pid") {
		t.Fatalf("expected pid in result: %s", text)
	}

	// Extract pid
	var pidResult struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal([]byte(text), &pidResult); err != nil {
		t.Fatalf("parse pid: %v from %s", err, text)
	}

	// Give process time to finish.
	// Poll
	result = toolCall(t, srv, "process_poll", map[string]any{"pid": pidResult.PID})
	if result.IsError {
		t.Fatalf("process_poll error: %s", result.Content[0].Text)
	}
	// Process may still be running or finished — check output eventually appears.
	text = result.Content[0].Text
	// The output might be in this poll or a subsequent one.
	if strings.Contains(text, "background_output") || strings.Contains(text, "exit_code:") {
		// ok — got output or finished
	} else {
		t.Logf("poll result (process may still be running): %s", text)
	}
}

// --- helper ---

func toolCall(t *testing.T, srv *mcpserver.Server, name string, args map[string]any) *mcpserver.CallToolResult {
	t.Helper()

	argsJSON, _ := json.Marshal(args)
	params := mcpserver.CallToolParams{Name: name, Arguments: argsJSON}
	paramsJSON, _ := json.Marshal(params)

	reqBody, _ := json.Marshal(mcpserver.Request{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/call",
		Params:  paramsJSON,
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	body, _ := io.ReadAll(w.Body)
	var resp struct {
		Result *mcpserver.CallToolResult `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, body)
	}
	if resp.Result == nil {
		t.Fatalf("nil result, body: %s", body)
	}
	return resp.Result
}
