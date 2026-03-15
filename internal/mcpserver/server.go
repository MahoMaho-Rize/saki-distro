package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// ToolHandler executes a tool call and returns the result.
type ToolHandler func(ctx context.Context, args json.RawMessage) *CallToolResult

// Server is a minimal MCP Streamable HTTP server.
// It handles initialize, tools/list, and tools/call over JSON-RPC 2.0.
type Server struct {
	info    ServerInfo
	mu      sync.RWMutex
	tools   []Tool
	handler map[string]ToolHandler
}

// New creates a new MCP server with the given name and version.
func New(name, version string) *Server {
	return &Server{
		info:    ServerInfo{Name: name, Version: version},
		handler: make(map[string]ToolHandler),
	}
}

// AddTool registers a tool with its handler.
func (s *Server) AddTool(t Tool, h ToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = append(s.tools, t)
	s.handler[t.Name] = h
}

// ServeHTTP implements http.Handler. This is the single MCP endpoint.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB max
	if err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}

	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}

	switch req.Method {
	case "initialize":
		s.handleInitialize(w, &req)
	case "notifications/initialized":
		// Client acknowledgement — no response needed for notifications.
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		s.handleToolsList(w, &req)
	case "tools/call":
		s.handleToolsCall(w, r, &req)
	default:
		writeRPCError(w, req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func (s *Server) handleInitialize(w http.ResponseWriter, req *Request) {
	result := InitializeResult{
		ProtocolVersion: "2025-03-26",
		ServerInfo:      s.info,
		Capabilities: ServerCapability{
			Tools: &ToolsCapability{},
		},
	}
	writeRPCResult(w, req.ID, result)
}

func (s *Server) handleToolsList(w http.ResponseWriter, req *Request) {
	s.mu.RLock()
	tools := make([]Tool, len(s.tools))
	copy(tools, s.tools)
	s.mu.RUnlock()

	writeRPCResult(w, req.ID, map[string]any{"tools": tools})
}

func (s *Server) handleToolsCall(w http.ResponseWriter, r *http.Request, req *Request) {
	var params CallToolParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPCError(w, req.ID, -32602, "invalid params")
		return
	}

	s.mu.RLock()
	h, ok := s.handler[params.Name]
	s.mu.RUnlock()

	if !ok {
		writeRPCResult(w, req.ID, ErrorResult(fmt.Sprintf("unknown tool: %s", params.Name)))
		return
	}

	result := h(r.Context(), params.Arguments)
	writeRPCResult(w, req.ID, result)
}

// ListenAndServe starts the MCP server on the given address.
func (s *Server) ListenAndServe(addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/mcp", s)
	log.Printf("MCP server %s/%s listening on %s", s.info.Name, s.info.Version, addr)
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 300 * time.Second, // long for tool execution
		IdleTimeout:  120 * time.Second,
	}
	return srv.ListenAndServe()
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	resp := Response{JSONRPC: "2.0", ID: id, Result: result}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	resp := Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
