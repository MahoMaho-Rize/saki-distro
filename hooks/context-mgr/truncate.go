package main

import (
	"encoding/json"
	"fmt"
)

// ──────────────────────────────────────────────────────────────────────────────
// Layer 2: Tool Result Context Guard (per-LLM-call)
//
// Phase A: Cap each individual tool result to singleResultCapRatio of window.
// Phase B: If total chars still exceed totalBudgetRatio of window, replace
//          oldest tool results with placeholders until under budget.
// ──────────────────────────────────────────────────────────────────────────────

// guardToolResults applies the two-phase context guard.
// Returns the (possibly modified) messages slice.
func guardToolResults(messages []interface{}, windowTokens int) []interface{} {
	// Phase A: single result cap.
	singleCap := int(float64(windowTokens) * float64(toolResultCharsPerToken) * singleResultCapRatio)
	messages = capIndividualToolResults(messages, singleCap)

	// Phase B: total budget enforcement.
	totalBudget := int(float64(windowTokens) * float64(charsPerToken) * totalBudgetRatio)
	messages = enforceTotalBudget(messages, totalBudget)

	return messages
}

// capIndividualToolResults truncates any single tool result exceeding maxChars.
func capIndividualToolResults(messages []interface{}, maxChars int) []interface{} {
	for i, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if !isToolResultMessage(m) {
			continue
		}
		content := messageContentString(m)
		if len(content) <= maxChars {
			continue
		}
		m["content"] = smartTruncate(content, maxChars)
		messages[i] = m
	}
	return messages
}

// enforceTotalBudget replaces oldest tool results with placeholders
// until total chars are under budget. Protected turns are skipped.
func enforceTotalBudget(messages []interface{}, budgetChars int) []interface{} {
	total := estimateMessagesChars(messages)
	if total <= budgetChars {
		return messages
	}

	// Find protected indices (last N assistants and their tool results).
	protected := protectedIndices(messages, keepLastAssistants)

	// Replace from oldest, skip protected.
	for i := 0; i < len(messages) && total > budgetChars; i++ {
		if protected[i] {
			continue
		}
		m, ok := messages[i].(map[string]interface{})
		if !ok {
			continue
		}
		if !isToolResultMessage(m) {
			continue
		}
		before := estimateMessageChars(m)
		m["content"] = "[compacted: tool output removed to free context]"
		messages[i] = m
		after := estimateMessageChars(m)
		total -= (before - after)
	}
	return messages
}

// ──────────────────────────────────────────────────────────────────────────────
// Layer 1: Context Pruning (soft trim + hard clear + drop oldest)
//
// Stage 1 (soft): Trim large tool results to head+tail.
// Stage 2 (hard): Replace old tool results with placeholder.
// Stage 3 (drop): Drop oldest messages entirely.
// ──────────────────────────────────────────────────────────────────────────────

// pruneContext applies three-stage context eviction.
// Always preserves: system (first), last user (last), recent N assistant turns.
func pruneContext(messages []interface{}, windowTokens int) []interface{} {
	total := estimateMessagesTokens(messages)
	if total <= windowTokens || len(messages) <= 2 {
		return messages
	}

	windowChars := windowTokens * charsPerToken
	ratio := float64(total*charsPerToken) / float64(windowChars)
	protected := protectedIndices(messages, keepLastAssistants)

	// Stage 1: Soft trim — truncate large tool results (head + tail).
	if ratio >= softTrimRatio {
		messages = softTrimMessages(messages, protected)
		total = estimateMessagesTokens(messages)
		if total <= windowTokens {
			return messages
		}
	}

	// Stage 2: Hard clear — replace old tool results with placeholder.
	ratio = float64(total*charsPerToken) / float64(windowChars)
	if ratio >= hardClearRatio {
		messages = hardClearMessages(messages, protected)
		total = estimateMessagesTokens(messages)
		if total <= windowTokens {
			return messages
		}
	}

	// Stage 3: Drop oldest messages (preserve system + last user + protected).
	messages = dropOldestMessages(messages, windowTokens, protected)
	return messages
}

// softTrimMessages trims tool results and large assistant messages.
// Images in structured content are replaced with placeholders.
func softTrimMessages(messages []interface{}, protected map[int]bool) []interface{} {
	for i, msg := range messages {
		if protected[i] {
			continue
		}
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}

		// Handle structured content with images.
		if blocks, ok := m["content"].([]interface{}); ok {
			m["content"] = replaceImages(blocks)
			messages[i] = m
			continue
		}

		content := messageContentString(m)
		if len(content) <= softTrimMaxChars {
			continue
		}
		m["content"] = smartTruncate(content, softTrimMaxChars)
		messages[i] = m
	}
	return messages
}

// hardClearMessages replaces old tool/assistant content with placeholders.
func hardClearMessages(messages []interface{}, protected map[int]bool) []interface{} {
	// Check if there's enough prunable content to justify hard clear.
	prunableChars := 0
	for i, msg := range messages {
		if protected[i] {
			continue
		}
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if isAssistantMessage(m) || isToolResultMessage(m) {
			prunableChars += estimateMessageChars(m)
		}
	}
	if prunableChars < minPrunableCharsHard {
		return messages
	}

	for i, msg := range messages {
		if protected[i] {
			continue
		}
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if isAssistantMessage(m) || isToolResultMessage(m) {
			m["content"] = "[Old tool result content cleared]"
			messages[i] = m
		}
	}
	return messages
}

// dropOldestMessages removes oldest non-system, non-protected messages
// until under token budget.
func dropOldestMessages(messages []interface{}, windowTokens int, protected map[int]bool) []interface{} {
	if len(messages) < 2 {
		return messages
	}

	// Always preserve first (system) and last (user).
	system := messages[0]
	user := messages[len(messages)-1]
	middle := make([]interface{}, len(messages)-2)
	copy(middle, messages[1:len(messages)-1])

	// Adjust protected indices for middle slice (offset by 1).
	midProtected := make(map[int]bool)
	for idx := range protected {
		if idx > 0 && idx < len(messages)-1 {
			midProtected[idx-1] = true
		}
	}

	// Drop from front of middle, skip protected.
	for len(middle) > 0 {
		all := make([]interface{}, 0, 2+len(middle))
		all = append(all, system)
		all = append(all, middle...)
		all = append(all, user)
		if estimateMessagesTokens(all) <= windowTokens {
			return all
		}
		// Find first non-protected to drop.
		dropped := false
		for j := 0; j < len(middle); j++ {
			if midProtected[j] {
				continue
			}
			middle = append(middle[:j], middle[j+1:]...)
			// Shift protected indices.
			newProt := make(map[int]bool)
			for k, v := range midProtected {
				if k > j {
					newProt[k-1] = v
				} else if k < j {
					newProt[k] = v
				}
			}
			midProtected = newProt
			dropped = true
			break
		}
		if !dropped {
			break // only protected left, stop
		}
	}

	result := make([]interface{}, 0, 2+len(middle))
	result = append(result, system)
	result = append(result, middle...)
	result = append(result, user)
	return result
}

// ──────────────────────────────────────────────────────────────────────────────
// Shared helpers
// ──────────────────────────────────────────────────────────────────────────────

// smartTruncate truncates text with error-aware head/tail preservation.
func smartTruncate(text string, maxChars int) string {
	if len(text) <= maxChars {
		return text
	}

	marker := fmt.Sprintf("\n\n⚠️ [...middle content omitted, %d chars total...]\n\n", len(text))
	budget := maxChars - len(marker)
	if budget <= 0 {
		return text[:maxChars]
	}

	if hasImportantTail(text) {
		tailBudget := int(float64(budget) * importantTailBudgetFrac)
		if tailBudget > importantTailMaxChars {
			tailBudget = importantTailMaxChars
		}
		headBudget := budget - tailBudget
		if headBudget < 0 {
			headBudget = 0
		}
		return text[:headBudget] + marker + text[len(text)-tailBudget:]
	}

	// No important tail — keep head only.
	suffix := fmt.Sprintf("\n[Truncated: kept first %d of %d chars]", budget, len(text))
	return text[:budget] + suffix
}

// replaceImages replaces image content blocks with text placeholders.
func replaceImages(blocks []interface{}) []interface{} {
	result := make([]interface{}, 0, len(blocks))
	for _, block := range blocks {
		bm, ok := block.(map[string]interface{})
		if !ok {
			result = append(result, block)
			continue
		}
		blockType, _ := bm["type"].(string)
		if blockType == "image_url" || blockType == "image" {
			result = append(result, map[string]interface{}{
				"type": "text",
				"text": "[image removed during context pruning]",
			})
			continue
		}
		result = append(result, block)
	}
	return result
}

// protectedIndices returns a set of message indices that should not be
// pruned: the last N assistant messages and their associated tool results.
func protectedIndices(messages []interface{}, keepLast int) map[int]bool {
	protected := make(map[int]bool)

	// Always protect first (system) and last (user) messages.
	if len(messages) > 0 {
		protected[0] = true
		protected[len(messages)-1] = true
	}

	// Find last N assistant messages.
	var assistantIndices []int
	for i, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if isAssistantMessage(m) {
			assistantIndices = append(assistantIndices, i)
		}
	}

	start := len(assistantIndices) - keepLast
	if start < 0 {
		start = 0
	}
	for _, idx := range assistantIndices[start:] {
		protected[idx] = true
		// Also protect adjacent tool results (i+1 if it's a tool message).
		if idx+1 < len(messages) {
			next, ok := messages[idx+1].(map[string]interface{})
			if ok && isToolResultMessage(next) {
				protected[idx+1] = true
			}
		}
		// And preceding tool results (tool_use_id matching would be ideal,
		// but adjacency heuristic is good enough for truncation protection).
		if idx-1 >= 0 {
			prev, ok := messages[idx-1].(map[string]interface{})
			if ok && isToolResultMessage(prev) {
				protected[idx-1] = true
			}
		}
	}

	return protected
}

// isOverflowError checks if an error string matches known context overflow patterns.
// Supports 18+ patterns in English and Chinese.
func isOverflowError(errMsg string) bool {
	patterns := []string{
		// English
		"context length exceeded",
		"prompt is too long",
		"maximum context length",
		"request_too_large",
		"token limit exceeded",
		"context window exceeded",
		"input too long",
		"max_tokens exceeded",
		"context_length_exceeded",
		"request size exceeds",
		// Chinese
		"上下文过长",
		"上下文超出",
		"上下文长度超",
		"超出最大上下文",
		"请压缩上下文",
		"请求过大",
		"令牌限制",
		"提示过长",
	}

	lower := toLowerASCII(errMsg)
	for _, p := range patterns {
		if containsLower(lower, p) {
			return true
		}
	}
	return false
}

// toLowerASCII lowercases ASCII chars only (fast, no allocation for pure ASCII).
func toLowerASCII(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

// containsLower checks if haystack contains needle. Both should be pre-lowered
// for ASCII patterns; Chinese patterns are matched as-is.
func containsLower(haystack, needle string) bool {
	return len(haystack) >= len(needle) && containsSubstring(haystack, needle)
}

// containsSubstring is a simple substring search.
func containsSubstring(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// extractToolFailures scans messages for failed tool results.
// Returns up to maxCount entries, each truncated to maxChars.
func extractToolFailures(messages []interface{}, maxCount, maxChars int) []string {
	var failures []string
	for _, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if !isToolResultMessage(m) {
			continue
		}
		content := messageContentString(m)
		lower := toLowerASCII(content)
		isFailure := containsSubstring(lower, "error") ||
			containsSubstring(lower, "failed") ||
			containsSubstring(lower, "exception") ||
			containsSubstring(lower, "traceback")
		if !isFailure {
			continue
		}
		entry := content
		if len(entry) > maxChars {
			entry = entry[:maxChars] + "..."
		}
		toolName := "unknown"
		if tn, ok := m["name"].(string); ok {
			toolName = tn
		}
		failures = append(failures, fmt.Sprintf("[%s] %s", toolName, entry))
		if len(failures) >= maxCount {
			break
		}
	}
	return failures
}

// extractModifiedFiles scans assistant messages for file operation references.
func extractModifiedFiles(messages []interface{}) (readFiles, modifiedFiles []string) {
	seen := make(map[string]bool)
	for _, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if !isAssistantMessage(m) {
			continue
		}
		// Scan structured content for tool_use blocks.
		blocks, ok := m["content"].([]interface{})
		if !ok {
			continue
		}
		for _, block := range blocks {
			bm, ok := block.(map[string]interface{})
			if !ok {
				continue
			}
			tool, _ := bm["tool"].(string)
			if tool == "" {
				tool, _ = bm["name"].(string)
			}

			var input map[string]interface{}
			switch v := bm["input"].(type) {
			case map[string]interface{}:
				input = v
			case json.RawMessage:
				_ = json.Unmarshal(v, &input)
			case string:
				_ = json.Unmarshal([]byte(v), &input)
			}
			if input == nil {
				continue
			}

			path, _ := input["path"].(string)
			if path == "" {
				path, _ = input["file_path"].(string)
			}
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true

			switch {
			case containsSubstring(tool, "read") || containsSubstring(tool, "glob") || containsSubstring(tool, "grep"):
				readFiles = append(readFiles, path)
			case containsSubstring(tool, "write") || containsSubstring(tool, "edit") || containsSubstring(tool, "create"):
				modifiedFiles = append(modifiedFiles, path)
			}
		}
	}
	return readFiles, modifiedFiles
}
