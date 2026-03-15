// Package mcpserver provides a minimal MCP Streamable HTTP server framework.
// It implements the server side of the MCP protocol (JSON-RPC 2.0 over HTTP/SSE)
// so that tool providers only need to register tools and their handlers.
package mcpserver

import "encoding/json"

// --- JSON-RPC 2.0 ---

// Request is a JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// --- MCP Protocol Types ---

// InitializeParams is sent by the client during handshake.
type InitializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
	ClientInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

// InitializeResult is returned by the server during handshake.
type InitializeResult struct {
	ProtocolVersion string           `json:"protocolVersion"`
	ServerInfo      ServerInfo       `json:"serverInfo"`
	Capabilities    ServerCapability `json:"capabilities"`
}

// ServerInfo identifies this MCP server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ServerCapability declares what the server supports.
type ServerCapability struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

// ToolsCapability declares tool support.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// Tool describes a single MCP tool.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"inputSchema"`
}

// CallToolParams is the params for tools/call.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CallToolResult is the result of tools/call.
type CallToolResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// ContentBlock is a single content item in a tool result.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// TextContent creates a text content block.
func TextContent(text string) ContentBlock {
	return ContentBlock{Type: "text", Text: text}
}

// ErrorResult creates an error CallToolResult.
func ErrorResult(msg string) *CallToolResult {
	return &CallToolResult{
		Content: []ContentBlock{TextContent(msg)},
		IsError: true,
	}
}

// SuccessResult creates a success CallToolResult.
func SuccessResult(text string) *CallToolResult {
	return &CallToolResult{
		Content: []ContentBlock{TextContent(text)},
	}
}
