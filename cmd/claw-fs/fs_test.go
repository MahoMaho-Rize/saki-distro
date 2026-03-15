package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claw-distro/internal/mcpserver"
	"claw-distro/internal/workspace"
)

// --- Ported from OpenClaw pi-tools.create-openclaw-coding-tools...test.ts ---
// Tests: write→read roundtrip, edit exact match, edit non-unique, offset/limit,
//        list_dir, grep, glob, path traversal, blacklist.

func newTestFS(t *testing.T) (*mcpserver.Server, string) {
	t.Helper()
	dir := t.TempDir()
	ws := workspace.New(dir)
	srv := mcpserver.New("test-fs", "0.1.0")
	registerTools(srv, ws)
	return srv, dir
}

// TestWriteReadRoundTrip — ported from "accepts Claude Code parameter aliases for read/write/edit"
func TestWriteReadRoundTrip(t *testing.T) {
	srv, dir := newTestFS(t)

	result := toolCall(t, srv, "write_file", map[string]any{
		"path": "hello.txt", "content": "hello world",
	})
	if result.IsError {
		t.Fatalf("write_file error: %s", result.Content[0].Text)
	}

	data, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil {
		t.Fatalf("read on disk: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("disk content: got %q, want %q", string(data), "hello world")
	}

	result = toolCall(t, srv, "read_file", map[string]any{"path": "hello.txt"})
	if result.IsError {
		t.Fatalf("read_file error: %s", result.Content[0].Text)
	}
	if !strings.Contains(result.Content[0].Text, "hello world") {
		t.Errorf("read_file missing 'hello world': %s", result.Content[0].Text)
	}
}

// TestEditExactMatch — ported from "write→edit→read" sequence
func TestEditExactMatch(t *testing.T) {
	srv, _ := newTestFS(t)

	toolCall(t, srv, "write_file", map[string]any{
		"path": "edit.txt", "content": "hello world",
	})

	result := toolCall(t, srv, "edit_file", map[string]any{
		"path": "edit.txt", "old_string": "world", "new_string": "universe",
	})
	if result.IsError {
		t.Fatalf("edit_file error: %s", result.Content[0].Text)
	}

	result = toolCall(t, srv, "read_file", map[string]any{"path": "edit.txt"})
	if !strings.Contains(result.Content[0].Text, "hello universe") {
		t.Errorf("expected 'hello universe', got: %s", result.Content[0].Text)
	}
}

func TestEditFailsOnNonUniqueMatch(t *testing.T) {
	srv, _ := newTestFS(t)

	toolCall(t, srv, "write_file", map[string]any{
		"path": "dup.txt", "content": "aaa bbb aaa",
	})

	result := toolCall(t, srv, "edit_file", map[string]any{
		"path": "dup.txt", "old_string": "aaa", "new_string": "ccc",
	})
	if !result.IsError {
		t.Fatal("expected error for non-unique match")
	}
	if !strings.Contains(result.Content[0].Text, "2 times") {
		t.Errorf("error should mention count: %s", result.Content[0].Text)
	}
}

func TestEditFailsOnNoMatch(t *testing.T) {
	srv, _ := newTestFS(t)

	toolCall(t, srv, "write_file", map[string]any{
		"path": "miss.txt", "content": "hello",
	})

	result := toolCall(t, srv, "edit_file", map[string]any{
		"path": "miss.txt", "old_string": "nonexistent", "new_string": "x",
	})
	if !result.IsError {
		t.Fatal("expected error for no match")
	}
	if !strings.Contains(result.Content[0].Text, "not found") {
		t.Errorf("error should mention 'not found': %s", result.Content[0].Text)
	}
}

func TestReadWithOffsetLimit(t *testing.T) {
	srv, _ := newTestFS(t)

	toolCall(t, srv, "write_file", map[string]any{
		"path": "lines.txt", "content": "line1\nline2\nline3\nline4\nline5",
	})

	result := toolCall(t, srv, "read_file", map[string]any{
		"path": "lines.txt", "offset": 2, "limit": 2,
	})
	text := result.Content[0].Text
	if !strings.Contains(text, "line2") || !strings.Contains(text, "line3") {
		t.Errorf("expected lines 2-3, got: %s", text)
	}
	if strings.Contains(text, "line1") || strings.Contains(text, "line4") {
		t.Errorf("should not contain lines outside range: %s", text)
	}
}

func TestListDir(t *testing.T) {
	srv, dir := newTestFS(t)

	os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o600)
	os.WriteFile(filepath.Join(dir, "subdir", "b.txt"), []byte("b"), 0o600)

	result := toolCall(t, srv, "list_dir", map[string]any{})
	text := result.Content[0].Text
	if !strings.Contains(text, "a.txt") {
		t.Errorf("expected a.txt: %s", text)
	}
	if !strings.Contains(text, "subdir/") {
		t.Errorf("expected subdir/: %s", text)
	}
}

func TestGrep(t *testing.T) {
	srv, dir := newTestFS(t)

	os.WriteFile(filepath.Join(dir, "code.go"), []byte("func main() {\n\tfmt.Println(\"hello\")\n}"), 0o600)

	result := toolCall(t, srv, "grep", map[string]any{"pattern": "Println"})
	text := result.Content[0].Text
	if !strings.Contains(text, "code.go:2") {
		t.Errorf("expected match at code.go:2, got: %s", text)
	}
}

func TestGlob(t *testing.T) {
	srv, dir := newTestFS(t)

	os.WriteFile(filepath.Join(dir, "a.go"), []byte(""), 0o600)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte(""), 0o600)
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte(""), 0o600)

	result := toolCall(t, srv, "glob", map[string]any{"pattern": "*.go"})
	text := result.Content[0].Text
	if !strings.Contains(text, "a.go") || !strings.Contains(text, "b.go") {
		t.Errorf("expected .go files: %s", text)
	}
	if strings.Contains(text, "c.txt") {
		t.Errorf("should not match .txt: %s", text)
	}
}

// --- Security tests — ported from OpenClaw sandbox-paths.test.ts ---

func TestPathTraversalBlocked(t *testing.T) {
	srv, _ := newTestFS(t)

	result := toolCall(t, srv, "read_file", map[string]any{"path": "/etc/passwd"})
	if !result.IsError {
		t.Fatal("expected error for path traversal")
	}
}

func TestBlacklistBlocked(t *testing.T) {
	srv, dir := newTestFS(t)

	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=x"), 0o600)

	result := toolCall(t, srv, "read_file", map[string]any{"path": ".env"})
	if !result.IsError {
		t.Fatal("expected error for blacklisted .env")
	}
	if !strings.Contains(result.Content[0].Text, "access denied") {
		t.Errorf("error should mention access denied: %s", result.Content[0].Text)
	}
}

func TestWriteCreatesParentDirs(t *testing.T) {
	srv, dir := newTestFS(t)

	result := toolCall(t, srv, "write_file", map[string]any{
		"path": "deep/nested/file.txt", "content": "content",
	})
	if result.IsError {
		t.Fatalf("write_file error: %s", result.Content[0].Text)
	}

	data, err := os.ReadFile(filepath.Join(dir, "deep", "nested", "file.txt"))
	if err != nil {
		t.Fatalf("read on disk: %v", err)
	}
	if string(data) != "content" {
		t.Errorf("got %q, want %q", string(data), "content")
	}
}

// --- test helper: call tool via HTTP ---

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
