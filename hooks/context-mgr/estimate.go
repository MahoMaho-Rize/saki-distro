package main

import (
	"encoding/json"
	"strings"
)

// estimateMessageTokens estimates token count for a single message,
// using differentiated ratios for text vs tool output vs images.
func estimateMessageTokens(msg map[string]interface{}) int {
	role, _ := msg["role"].(string)

	// Determine char-per-token ratio based on role.
	ratio := charsPerToken
	if role == "tool" || role == "function" {
		ratio = toolResultCharsPerToken
	}

	chars := estimateMessageChars(msg)
	return chars / ratio
}

// estimateMessageChars estimates the character count for a message,
// accounting for images in structured content blocks.
func estimateMessageChars(msg map[string]interface{}) int {
	content := msg["content"]

	switch v := content.(type) {
	case string:
		return len(v)
	case json.RawMessage:
		return len(v)
	case []interface{}:
		// Structured content blocks (e.g., Anthropic multi-part content).
		total := 0
		for _, block := range v {
			bm, ok := block.(map[string]interface{})
			if !ok {
				continue
			}
			blockType, _ := bm["type"].(string)
			switch blockType {
			case "image_url", "image":
				total += imageCharEstimate
			case "text":
				text, _ := bm["text"].(string)
				total += len(text)
			default:
				// tool_use, tool_result, etc. — serialize for estimation.
				b, _ := json.Marshal(bm)
				total += len(b)
			}
		}
		return total
	default:
		// Fallback: serialize and measure.
		b, _ := json.Marshal(content)
		return len(b)
	}
}

// estimateMessagesTokens estimates total token count for a slice of messages.
func estimateMessagesTokens(messages []interface{}) int {
	total := 0
	for _, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		total += estimateMessageTokens(m)
	}
	return total
}

// estimateMessagesChars estimates total character count for a slice of messages.
func estimateMessagesChars(messages []interface{}) int {
	total := 0
	for _, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		total += estimateMessageChars(m)
	}
	return total
}

// resolveContextWindow extracts the context window token budget from metadata,
// falling back to the configured maxTokens.
func resolveContextWindow(meta map[string]interface{}, fallback int) int {
	// Prefer model-specific context window if available in metadata.
	if cw, ok := meta["context_window"]; ok {
		switch v := cw.(type) {
		case float64:
			if int(v) > 0 {
				return int(v)
			}
		case int:
			if v > 0 {
				return v
			}
		}
	}
	return fallback
}

// isToolResultMessage returns true if the message is a tool/function result.
func isToolResultMessage(msg map[string]interface{}) bool {
	role, _ := msg["role"].(string)
	return role == "tool" || role == "function"
}

// isAssistantMessage returns true if the message has role "assistant".
func isAssistantMessage(msg map[string]interface{}) bool {
	role, _ := msg["role"].(string)
	return role == "assistant"
}

// isSystemMessage returns true if the message has role "system".
func isSystemMessage(msg map[string]interface{}) bool {
	role, _ := msg["role"].(string)
	return role == "system"
}

// isUserMessage returns true if the message has role "user".
func isUserMessage(msg map[string]interface{}) bool {
	role, _ := msg["role"].(string)
	return role == "user"
}

// messageContentString extracts content as a plain string.
// Returns empty string for non-string content.
func messageContentString(msg map[string]interface{}) string {
	s, _ := msg["content"].(string)
	return s
}

// hasImportantTail checks if the last N characters of text contain
// error indicators, JSON closures, or summary markers that suggest
// the tail is more valuable than the head.
func hasImportantTail(text string) bool {
	if len(text) < importantTailCheckChars {
		return false
	}
	tail := text[len(text)-importantTailCheckChars:]
	lower := strings.ToLower(tail)

	// Error/exception patterns.
	errorPatterns := []string{
		"error", "exception", "failed", "fatal", "traceback",
		"panic", "errno", "exit code", "segfault", "assertion",
	}
	for _, p := range errorPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}

	// JSON closure — suggests structured output ending.
	if strings.Contains(tail, "}") {
		return true
	}

	// Summary/completion markers.
	summaryPatterns := []string{
		"total", "summary", "result", "complete", "finished", "done",
	}
	for _, p := range summaryPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}

	return false
}
