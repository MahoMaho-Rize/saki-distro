package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInitialize(t *testing.T) {
	srv := New("test-server", "0.1.0")
	resp := rpcCall(t, srv, "initialize", nil)

	var result InitializeResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.ProtocolVersion != "2025-03-26" {
		t.Errorf("protocol version: got %s, want 2025-03-26", result.ProtocolVersion)
	}
	if result.ServerInfo.Name != "test-server" {
		t.Errorf("server name: got %s, want test-server", result.ServerInfo.Name)
	}
}

func TestToolsList_Empty(t *testing.T) {
	srv := New("test-server", "0.1.0")
	resp := rpcCall(t, srv, "tools/list", nil)

	var result struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(result.Tools))
	}
}

func TestToolsList_WithTools(t *testing.T) {
	srv := New("test-server", "0.1.0")
	srv.AddTool(Tool{Name: "echo", Description: "echo text"}, nil)
	srv.AddTool(Tool{Name: "ping", Description: "ping"}, nil)

	resp := rpcCall(t, srv, "tools/list", nil)

	var result struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(result.Tools))
	}
	if result.Tools[0].Name != "echo" {
		t.Errorf("tool[0] name: got %s, want echo", result.Tools[0].Name)
	}
}

func TestToolsCall_Success(t *testing.T) {
	srv := New("test-server", "0.1.0")
	srv.AddTool(
		Tool{Name: "greet", Description: "say hi"},
		func(ctx context.Context, args json.RawMessage) *CallToolResult {
			var p struct {
				Name string `json:"name"`
			}
			json.Unmarshal(args, &p)
			return SuccessResult("hello " + p.Name)
		},
	)

	params := CallToolParams{Name: "greet", Arguments: json.RawMessage(`{"name":"world"}`)}
	resp := rpcCall(t, srv, "tools/call", params)

	var result CallToolResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.IsError {
		t.Error("expected success, got error")
	}
	if len(result.Content) != 1 || result.Content[0].Text != "hello world" {
		t.Errorf("unexpected content: %+v", result.Content)
	}
}

func TestToolsCall_UnknownTool(t *testing.T) {
	srv := New("test-server", "0.1.0")

	params := CallToolParams{Name: "nonexistent"}
	resp := rpcCall(t, srv, "tools/call", params)

	var result CallToolResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for unknown tool")
	}
}

func TestMethodNotFound(t *testing.T) {
	srv := New("test-server", "0.1.0")
	resp := rpcCall(t, srv, "bogus/method", nil)

	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("error code: got %d, want -32601", resp.Error.Code)
	}
}

func TestNotificationsInitialized_Returns202(t *testing.T) {
	srv := New("test-server", "0.1.0")

	body, _ := json.Marshal(Request{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "notifications/initialized",
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusAccepted)
	}
}

// --- helpers ---

type rpcResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

func rpcCall(t *testing.T, srv *Server, method string, params any) rpcResponse {
	t.Helper()

	var paramsRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		paramsRaw = b
	}

	reqBody, _ := json.Marshal(Request{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  method,
		Params:  paramsRaw,
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	respBody, err := io.ReadAll(w.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	var resp rpcResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nbody: %s", err, respBody)
	}
	return resp
}
