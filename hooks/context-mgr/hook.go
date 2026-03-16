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

// Context eviction thresholds (ported from OpenClaw context-pruning).
const (
	softTrimRatio           = 0.3   // soft trim triggers at 30% of window
	hardClearRatio          = 0.5   // hard clear triggers at 50% of window
	keepLastN               = 3     // always preserve last 3 assistant turns
	softTrimChars           = 3000  // soft trim: tool results → head(1500) + tail(1500)
	minPrunableChars        = 50000 // don't hard-clear if prunable content < 50K
	toolResultCharsPerToken = 2     // more conservative for tool output
)

// truncateIfNeeded applies three-layer context eviction:
//
//	Layer 1 (soft trim): trim large tool results to head+tail
//	Layer 2 (hard clear): replace old tool results with "[cleared]"
//	Layer 3 (drop oldest): remove oldest messages entirely
//
// Always preserves: system (first), last user (last), recent N assistant turns.
func (h *contextMgrHook) truncateIfNeeded(messages []interface{}) []interface{} {
	total := estimateTokens(messages)
	if total <= h.maxTokens {
		return messages
	}

	if len(messages) <= 2 {
		return messages
	}

	windowChars := h.maxTokens * 4 // convert back to chars
	ratio := float64(total*4) / float64(windowChars)

	// Layer 1: Soft trim — truncate large tool results (head 1500 + tail 1500)
	if ratio > softTrimRatio {
		messages = softTrimToolResults(messages, softTrimChars)
		total = estimateTokens(messages)
		if total <= h.maxTokens {
			return messages
		}
	}

	// Layer 2: Hard clear — replace old tool results with placeholder
	if ratio > hardClearRatio {
		messages = hardClearOldToolResults(messages, keepLastN, minPrunableChars)
		total = estimateTokens(messages)
		if total <= h.maxTokens {
			return messages
		}
	}

	// Layer 3: Drop oldest messages (preserve system + last user + recent N)
	system := messages[0]
	user := messages[len(messages)-1]
	history := messages[1 : len(messages)-1]

	for len(history) > 0 && estimateTokens(append(append([]interface{}{system}, history...), user)) > h.maxTokens {
		history = history[1:]
	}

	result := make([]interface{}, 0, 2+len(history))
	result = append(result, system)
	result = append(result, history...)
	result = append(result, user)
	return result
}

// softTrimToolResults trims content of assistant messages (tool results)
// to head+tail if they exceed maxChars.
func softTrimToolResults(messages []interface{}, maxChars int) []interface{} {
	for i, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		content, _ := m["content"].(string)
		if len(content) <= maxChars*2 {
			continue
		}
		// Head + tail preservation
		head := content[:maxChars/2]
		tail := content[len(content)-maxChars/2:]
		m["content"] = head + "\n[... soft trimmed ...]\n" + tail
		messages[i] = m
	}
	return messages
}

// hardClearOldToolResults replaces old assistant tool-result content
// with a short placeholder, preserving only the most recent N turns.
func hardClearOldToolResults(messages []interface{}, keepLast, minPrunable int) []interface{} {
	// Count prunable chars
	prunableChars := 0
	for _, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "assistant" {
			content, _ := m["content"].(string)
			prunableChars += len(content)
		}
	}
	if prunableChars < minPrunable {
		return messages // not enough to prune
	}

	// Find last N assistant messages to protect
	assistantIndices := []int{}
	for i, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role == "assistant" {
			assistantIndices = append(assistantIndices, i)
		}
	}

	protectedStart := len(assistantIndices) - keepLast
	if protectedStart < 0 {
		protectedStart = 0
	}
	protected := make(map[int]bool)
	for _, idx := range assistantIndices[protectedStart:] {
		protected[idx] = true
	}

	// Clear unprotected assistant messages
	for i, msg := range messages {
		if protected[i] {
			continue
		}
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role == "assistant" {
			m["content"] = "[Old tool result content cleared]"
			messages[i] = m
		}
	}
	return messages
}

// estimateTokens estimates token count using chars/4 for text,
// chars/2 for content that looks like tool output (more conservative).
func estimateTokens(messages []interface{}) int {
	b, _ := json.Marshal(messages)
	return len(b) / 4
}
