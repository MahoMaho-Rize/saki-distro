package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"claw-distro/internal/mcpserver"
)

// Shared across all tests. Managed by TestMain.
var (
	testBrowserInstance *browser
	testMockServer      *httptest.Server
	testMCPServer       *mcpserver.Server
)

func TestMain(m *testing.M) {
	testMockServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!DOCTYPE html>
<html><head><title>Test Page</title></head>
<body>
  <h1 id="heading">Hello Browser</h1>
  <p id="content">This is test content.</p>
  <input id="search" type="text" placeholder="Search...">
  <button id="btn" onclick="document.getElementById('content').innerText='clicked!'">Click Me</button>
</body></html>`)
	}))

	testBrowserInstance = newBrowser()
	testMCPServer = mcpserver.New("test-browser", "0.1.0")
	registerTools(testMCPServer, testBrowserInstance)

	code := m.Run()

	testBrowserInstance.close()
	testMockServer.Close()
	os.Exit(code)
}

// navigateToMock is a helper that navigates to the shared mock page.
func navigateToMock(t *testing.T) {
	t.Helper()
	result := toolCall(t, testMCPServer, "browser_navigate", map[string]any{"url": testMockServer.URL})
	if result.IsError {
		t.Fatalf("navigate error: %s", result.Content[0].Text)
	}
}

func TestBrowserNavigate(t *testing.T) {
	result := toolCall(t, testMCPServer, "browser_navigate", map[string]any{"url": testMockServer.URL})
	if result.IsError {
		t.Fatalf("navigate error: %s", result.Content[0].Text)
	}
	if !strings.Contains(result.Content[0].Text, "navigated") {
		t.Errorf("expected navigated, got: %s", result.Content[0].Text)
	}
}

func TestBrowserContent(t *testing.T) {
	navigateToMock(t)

	result := toolCall(t, testMCPServer, "browser_content", map[string]any{})
	if result.IsError {
		t.Fatalf("content error: %s", result.Content[0].Text)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "Hello Browser") {
		t.Errorf("expected heading text, got: %.200s", text)
	}
	if !strings.Contains(text, "test content") {
		t.Errorf("expected paragraph text, got: %.200s", text)
	}
}

func TestBrowserScreenshot(t *testing.T) {
	navigateToMock(t)

	result := toolCall(t, testMCPServer, "browser_screenshot", map[string]any{})
	if result.IsError {
		t.Fatalf("screenshot error: %s", result.Content[0].Text)
	}
	if !strings.HasPrefix(result.Content[0].Text, "data:image/png;base64,") {
		t.Errorf("expected base64 PNG, got prefix: %.50s", result.Content[0].Text)
	}
}

func TestBrowserClick(t *testing.T) {
	navigateToMock(t)

	result := toolCall(t, testMCPServer, "browser_click", map[string]any{"selector": "#btn"})
	if result.IsError {
		t.Fatalf("click error: %s", result.Content[0].Text)
	}

	result = toolCall(t, testMCPServer, "browser_evaluate", map[string]any{
		"expression": "document.getElementById('content').innerText",
	})
	if result.IsError {
		t.Fatalf("evaluate error: %s", result.Content[0].Text)
	}
	if !strings.Contains(result.Content[0].Text, "clicked") {
		t.Errorf("click handler did not fire, content: %s", result.Content[0].Text)
	}
}

func TestBrowserType(t *testing.T) {
	navigateToMock(t)

	result := toolCall(t, testMCPServer, "browser_type", map[string]any{
		"selector": "#search",
		"text":     "hello world",
	})
	if result.IsError {
		t.Fatalf("type error: %s", result.Content[0].Text)
	}

	result = toolCall(t, testMCPServer, "browser_evaluate", map[string]any{
		"expression": "document.getElementById('search').value",
	})
	if !strings.Contains(result.Content[0].Text, "hello world") {
		t.Errorf("typed text not found, got: %s", result.Content[0].Text)
	}
}

func TestBrowserEvaluate(t *testing.T) {
	navigateToMock(t)

	result := toolCall(t, testMCPServer, "browser_evaluate", map[string]any{
		"expression": "document.title",
	})
	if result.IsError {
		t.Fatalf("evaluate error: %s", result.Content[0].Text)
	}
	if !strings.Contains(result.Content[0].Text, "Test Page") {
		t.Errorf("expected 'Test Page', got: %s", result.Content[0].Text)
	}
}

func TestBrowserNavigate_InvalidURL(t *testing.T) {
	result := toolCall(t, testMCPServer, "browser_navigate", map[string]any{"url": ""})
	if !result.IsError {
		t.Error("expected error for empty URL")
	}
}

func TestBrowserClick_BadSelector(t *testing.T) {
	navigateToMock(t)

	result := toolCall(t, testMCPServer, "browser_click", map[string]any{"selector": "#nonexistent"})
	if !result.IsError {
		t.Error("expected error for bad selector")
	}
	if !strings.Contains(result.Content[0].Text, "not found") {
		t.Errorf("error should mention 'not found': %s", result.Content[0].Text)
	}
}

func TestBrowserType_BadSelector(t *testing.T) {
	navigateToMock(t)

	result := toolCall(t, testMCPServer, "browser_type", map[string]any{
		"selector": "#nonexistent",
		"text":     "hello",
	})
	if !result.IsError {
		t.Error("expected error for bad selector")
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
