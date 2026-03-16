// delegate_task — Agent-as-a-Tool sub-agent spawning.
//
// This MCP tool spawns a sub-agent by making an HTTP request back to the
// TAG Gateway. The gateway is a stateless L7 proxy — it doesn't know (or care)
// whether the request comes from an external user or an internal MCP tool.
//
// The sub-agent gets its own session (X-Session-ID), its own system prompt
// (role), and runs a full ReAct loop independently. The result is returned
// as the tool's output to the parent agent.
//
// Depth control: X-Agent-Depth header prevents infinite recursion.
// Each level increments the depth; when max depth is reached, the tool
// refuses to spawn further sub-agents.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"claw-distro/internal/mcpserver"
)

const (
	defaultGatewayURL = "http://127.0.0.1:8090/v1/chat/completions"
	defaultModel      = "claude-sonnet-4-6"
	maxAgentDepth     = 5
	delegateTimeout   = 5 * time.Minute
	maxResponseBytes  = 1 << 20 // 1 MB
)

// delegateConfig holds the resolved configuration for delegate_task.
type delegateConfig struct {
	gatewayURL string
	model      string
}

func newDelegateConfig() delegateConfig {
	return delegateConfig{
		gatewayURL: envOr("CLAW_GATEWAY_URL", defaultGatewayURL),
		model:      envOr("CLAW_MODEL", defaultModel),
	}
}

// registerDelegateTool adds the delegate_task tool to the MCP server.
func registerDelegateTool(srv *mcpserver.Server) {
	cfg := newDelegateConfig()

	srv.AddTool(mcpserver.Tool{
		Name: "delegate_task",
		Description: "Spawn a sub-agent to handle a specific task independently. " +
			"The sub-agent has its own conversation context and can use all available tools. " +
			"Use this when a task is independent and can be solved without your current context, " +
			"e.g. researching a topic, writing a self-contained module, or running a complex test. " +
			"The sub-agent's final answer is returned as this tool's output.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"task"},
			"properties": map[string]any{
				"task": map[string]any{
					"type":        "string",
					"description": "Clear, self-contained description of the task for the sub-agent.",
				},
				"role": map[string]any{
					"type":        "string",
					"description": "System prompt / persona for the sub-agent (e.g. 'You are a security researcher'). Optional.",
				},
				"model": map[string]any{
					"type":        "string",
					"description": "Model to use for the sub-agent. Defaults to " + cfg.model + ".",
				},
			},
		}),
	}, func(ctx context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			Task  string `json:"task"`
			Role  string `json:"role"`
			Model string `json:"model"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}
		if p.Task == "" {
			return mcpserver.ErrorResult("task is required")
		}

		// Depth control: read current depth from environment (set by gateway via hook)
		currentDepth := readAgentDepth()
		if currentDepth >= maxAgentDepth {
			return mcpserver.ErrorResult(fmt.Sprintf(
				"max agent depth reached (%d/%d) — cannot spawn further sub-agents",
				currentDepth, maxAgentDepth))
		}

		model := cfg.model
		if p.Model != "" {
			model = p.Model
		}

		result, err := callGateway(ctx, cfg.gatewayURL, model, p.Role, p.Task, currentDepth+1)
		if err != nil {
			return mcpserver.ErrorResult("delegate_task failed: " + err.Error())
		}

		return mcpserver.SuccessResult(result)
	})

	fmt.Fprintf(os.Stderr, "claw-exec: delegate_task registered (gateway=%s, max_depth=%d)\n",
		cfg.gatewayURL, maxAgentDepth)
}

// callGateway makes a streaming request to the TAG Gateway and collects
// all SSE chunks into the final assistant message content.
// TAG Gateway always returns SSE (ignores stream:false), so we parse
// the SSE event stream and concatenate delta.content fields.
func callGateway(ctx context.Context, gatewayURL, model, role, task string, depth int) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, delegateTimeout)
	defer cancel()

	// Build messages
	messages := []map[string]string{}
	if role != "" {
		messages = append(messages, map[string]string{"role": "system", "content": role})
	}
	messages = append(messages, map[string]string{"role": "user", "content": task})

	reqBody := map[string]any{
		"model":    model,
		"stream":   true,
		"messages": messages,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, gatewayURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Depth", strconv.Itoa(depth))

	// Unique session ID so the sub-agent gets its own context
	sessionID := fmt.Sprintf("sub-%d-%d", depth, time.Now().UnixMilli())
	req.Header.Set("X-Session-ID", sessionID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("gateway request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("gateway returned %d: %s", resp.StatusCode, truncate(string(errBody), 500))
	}

	// Parse SSE stream, collect delta.content into full message
	return collectSSE(resp.Body)
}

// collectSSE reads an SSE stream from the gateway and concatenates all
// delta.content fields into the final assistant message.
// Handles the TAG Gateway's OpenAI-compatible SSE format:
//
//	data: {"choices":[{"delta":{"content":"Hello"}}]}
//	data: [DONE]
func collectSSE(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(io.LimitReader(r, maxResponseBytes))
	var content strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		// SSE format: "data: {...}" or "data: [DONE]"
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // skip malformed chunks
		}
		if chunk.Error != nil {
			return "", fmt.Errorf("gateway error: %s", chunk.Error.Message)
		}
		if len(chunk.Choices) > 0 {
			content.WriteString(chunk.Choices[0].Delta.Content)
		}
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read SSE stream: %w", err)
	}

	result := content.String()
	if result == "" {
		return "", fmt.Errorf("empty response from sub-agent")
	}
	return result, nil
}

// readAgentDepth reads the current agent depth from the X-Agent-Depth
// environment variable or MCP request context. Defaults to 0.
func readAgentDepth() int {
	if v := os.Getenv("X_AGENT_DEPTH"); v != "" {
		if d, err := strconv.Atoi(v); err == nil {
			return d
		}
	}
	return 0
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// init registers CLAW_GATEWAY_URL validation
func init() {
	url := envOr("CLAW_GATEWAY_URL", defaultGatewayURL)
	if !strings.HasPrefix(url, "http") {
		fmt.Fprintf(os.Stderr, "claw-exec: WARNING: CLAW_GATEWAY_URL=%q doesn't look like a URL\n", url)
	}
}
