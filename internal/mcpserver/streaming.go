package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// StreamingToolHandler executes a tool call with streaming progress output.
// The handler receives a ProgressWriter to emit progress lines during execution,
// and returns the final CallToolResult when done.
type StreamingToolHandler func(ctx context.Context, args json.RawMessage, pw *ProgressWriter) *CallToolResult

// ProgressWriter allows a tool handler to emit progress lines during execution.
// Each line is sent as an SSE event to the client in real time.
type ProgressWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	reqID   json.RawMessage
}

// WriteLine sends a progress line to the client as a JSON-RPC notification
// embedded in an SSE event. The client (TAG Gateway) receives this through
// its Streamable HTTP transport's readSSEStream path.
func (pw *ProgressWriter) WriteLine(line string) {
	notif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/progress",
		"params": map[string]any{
			"progressToken": string(pw.reqID),
			"data":          line,
		},
	}
	data, err := json.Marshal(notif)
	if err != nil {
		return
	}
	fmt.Fprintf(pw.w, "event: message\ndata: %s\n\n", data)
	pw.flusher.Flush()
}

// AddStreamingTool registers a tool with a streaming handler.
// The tool call response is sent as SSE: progress notifications first,
// then the final result as the last event.
func (s *Server) AddStreamingTool(t Tool, h StreamingToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = append(s.tools, t)
	// Store a wrapper that uses the streaming path
	s.handler[t.Name] = nil // placeholder — streaming tools are handled specially
	if s.streamHandler == nil {
		s.streamHandler = make(map[string]StreamingToolHandler)
	}
	s.streamHandler[t.Name] = h
}

// handleStreamingToolsCall handles a tools/call for a streaming tool.
// Response is text/event-stream with progress notifications followed by the result.
func (s *Server) handleStreamingToolsCall(w http.ResponseWriter, r *http.Request, req *Request, handler StreamingToolHandler) {
	var params CallToolParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPCError(w, req.ID, -32602, "invalid params")
		return
	}

	// Assert Flusher support
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Fallback: non-streaming execution
		result := handler(r.Context(), params.Arguments, nil)
		writeRPCResult(w, req.ID, result)
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	pw := &ProgressWriter{w: w, flusher: flusher, reqID: req.ID}

	// Execute tool with progress streaming
	result := handler(r.Context(), params.Arguments, pw)

	// Send final result as the last SSE event
	resp := Response{JSONRPC: "2.0", ID: req.ID, Result: result}
	data, _ := json.Marshal(resp)
	fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
	flusher.Flush()
}
