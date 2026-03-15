package main

import (
	"context"
	"encoding/json"
	"tag-gateway/hooklib"
)

// contextMgrHook implements the context management ext_proc hook.
type contextMgrHook struct {
	maxTokens int // context window budget (chars/4 heuristic)
}

// Compile-time interface checks.
var (
	_ hooklib.Hook      = (*contextMgrHook)(nil)
	_ hooklib.Processor = (*contextMgrHook)(nil)
	_ hooklib.Notifier  = (*contextMgrHook)(nil)
)

func (h *contextMgrHook) Init(_ *hooklib.InitParams) *hooklib.InitResult {
	return &hooklib.InitResult{
		Name:    "context-mgr",
		Version: "0.1.0",
		Phases: map[hooklib.Phase]hooklib.PhaseConfig{
			hooklib.PhasePreReq:   {Mode: hooklib.ModeBodyMutate},
			hooklib.PhasePostResp: {Mode: hooklib.ModeHeaderOnly},
		},
		MetadataNeeds: []string{
			"session_key",
			"model",
			"hook.session-hook.history",
			"hook.session-hook.turn_count",
			"react_trace",
		},
		MetadataProvides: []string{
			"save_messages",
		},
	}
}

// Process handles PRE_REQ: inject history into body, save user message.
func (h *contextMgrHook) Process(_ context.Context, params *hooklib.ProcessParams) *hooklib.ProcessResult {
	if params.Phase != hooklib.PhasePreReq {
		return hooklib.Pass()
	}

	// Parse request body.
	var body map[string]interface{}
	if err := json.Unmarshal(params.Body, &body); err != nil {
		return hooklib.Pass()
	}

	// Extract current messages from body.
	rawMessages, _ := body["messages"].([]interface{})
	if len(rawMessages) == 0 {
		return hooklib.Pass()
	}

	// Load history from session-hook.
	history := extractHistory(params.Metadata)

	// Extract the user's new message (last message in body).
	userMsg := rawMessages[len(rawMessages)-1]

	// Build full messages: system (if any) + history + user message.
	var newMessages []interface{}

	// Preserve system message from original request.
	if len(rawMessages) > 1 {
		first, _ := rawMessages[0].(map[string]interface{})
		if role, _ := first["role"].(string); role == "system" {
			newMessages = append(newMessages, first)
		}
	}

	// Append history (oldest first).
	newMessages = append(newMessages, history...)

	// Append current user message.
	newMessages = append(newMessages, userMsg)

	// Token estimation and truncation.
	newMessages = h.truncateIfNeeded(newMessages)

	body["messages"] = newMessages

	newBody, err := json.Marshal(body)
	if err != nil {
		return hooklib.Pass()
	}

	// Save user message for session-hook persistence.
	userMsgJSON, _ := json.Marshal(userMsg)
	saveMessages := []json.RawMessage{userMsgJSON}

	return hooklib.ContinueWithBody(
		map[string]interface{}{
			"save_messages": saveMessages,
		},
		newBody,
	)
}

// Notify handles POST_RESP: extract assistant response from react_trace,
// emit save_messages for session-hook to persist.
func (h *contextMgrHook) Notify(params *hooklib.NotifyParams) *hooklib.NotifyResult {
	if params.Phase != hooklib.PhasePostResp {
		return nil
	}

	// Extract react_trace from gateway metadata.
	trace := extractReactTrace(params.Metadata)

	// Build assistant messages to save.
	// If there's a react_trace, the assistant made tool calls —
	// we save a summary as the assistant message.
	// If no trace, it was a pure text response — we can't capture it
	// from POST_RESP HEADER_ONLY mode. Known limitation.
	if trace == nil {
		return nil
	}

	var assistantParts []interface{}
	for _, step := range trace.Steps {
		assistantParts = append(assistantParts, map[string]interface{}{
			"type":     "tool_use",
			"tool":     step.Tool,
			"tool_id":  step.ToolUseID,
			"input":    step.Input,
			"output":   step.Output,
			"status":   step.Status,
			"turn":     step.Turn,
			"duration": step.DurationMs,
		})
	}

	if len(assistantParts) == 0 {
		return nil
	}

	contentJSON, _ := json.Marshal(assistantParts)
	assistantMsg := map[string]interface{}{
		"role":    "assistant",
		"content": json.RawMessage(contentJSON),
	}
	msgJSON, _ := json.Marshal(assistantMsg)

	// Emit save_messages via metadata_patch. NotifyChained runs POST_RESP
	// hooks in reverse order (onion model), so context-mgr (order 20) runs
	// before session-hook (order 10). session-hook will see this patch.
	return &hooklib.NotifyResult{
		Action: hooklib.ActionContinue,
		MetadataPatch: map[string]interface{}{
			"save_messages": []json.RawMessage{json.RawMessage(msgJSON)},
		},
	}
}

// --- helpers ---

// extractHistory retrieves the conversation history from session-hook metadata.
// Gateway delivers hook metadata as flat dot-notation keys:
//
//	meta["hook.session-hook.history"] = [...]
func extractHistory(meta map[string]interface{}) []interface{} {
	historyRaw, ok := meta["hook.session-hook.history"]
	if !ok {
		return nil
	}

	// history can be a JSON-encoded string or already-parsed array.
	switch v := historyRaw.(type) {
	case []interface{}:
		return v
	case string:
		var msgs []interface{}
		if err := json.Unmarshal([]byte(v), &msgs); err == nil {
			return msgs
		}
	case json.RawMessage:
		var msgs []interface{}
		if err := json.Unmarshal(v, &msgs); err == nil {
			return msgs
		}
	}
	return nil
}

// traceStep mirrors react.TraceStep for JSON unmarshalling.
type traceStep struct {
	Turn       int             `json:"turn"`
	Type       string          `json:"type"`
	Tool       string          `json:"tool"`
	ToolUseID  string          `json:"tool_use_id"`
	Input      json.RawMessage `json:"input"`
	Output     string          `json:"output"`
	DurationMs int64           `json:"duration_ms"`
	Status     string          `json:"status"`
}

type reactTrace struct {
	Turns           int         `json:"turns"`
	Steps           []traceStep `json:"steps"`
	TotalDurationMs int64       `json:"total_duration_ms"`
}

// extractReactTrace retrieves react_trace from gateway metadata.
func extractReactTrace(meta map[string]interface{}) *reactTrace {
	raw, ok := meta["react_trace"]
	if !ok {
		return nil
	}

	// react_trace may arrive as a struct (from direct metadata) or as
	// a map[string]interface{} (from JSON round-trip). Marshal and re-parse.
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var trace reactTrace
	if err := json.Unmarshal(b, &trace); err != nil {
		return nil
	}
	if trace.Turns == 0 && len(trace.Steps) == 0 {
		return nil
	}
	return &trace
}

// truncateIfNeeded removes old messages from the middle (between system
// and the last user message) if the total token estimate exceeds budget.
func (h *contextMgrHook) truncateIfNeeded(messages []interface{}) []interface{} {
	total := estimateTokens(messages)
	if total <= h.maxTokens {
		return messages
	}

	// Strategy: keep system (first) and user (last), truncate from oldest history.
	if len(messages) <= 2 {
		return messages // can't truncate further
	}

	system := messages[0]
	user := messages[len(messages)-1]
	history := messages[1 : len(messages)-1]

	// Remove oldest messages until under budget.
	for len(history) > 0 && estimateTokens(append(append([]interface{}{system}, history...), user)) > h.maxTokens {
		history = history[1:]
	}

	result := make([]interface{}, 0, 2+len(history))
	result = append(result, system)
	result = append(result, history...)
	result = append(result, user)
	return result
}

// estimateTokens estimates token count using the chars/4 heuristic.
func estimateTokens(messages []interface{}) int {
	b, _ := json.Marshal(messages)
	return len(b) / 4
}
