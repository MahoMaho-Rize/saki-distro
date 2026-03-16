package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

// compactor handles Layer 4: LLM-based context summarization.
type compactor struct {
	gatewayURL string
	model      string
	client     *http.Client
}

func newCompactor(gatewayURL, model string) *compactor {
	return &compactor{
		gatewayURL: gatewayURL,
		model:      model,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// compactResult holds the output of a compaction run.
type compactResult struct {
	Summary        string        // LLM-generated summary text
	PreservedTurns []interface{} // recent turns kept verbatim
	ToolFailures   []string      // extracted tool failure records
	ReadFiles      []string
	ModifiedFiles  []string
}

// run executes the full compaction pipeline on a message history.
// Steps: prune → split preserved → summarize → audit → enrich.
func (c *compactor) run(messages []interface{}, windowTokens int) (*compactResult, error) {
	if len(messages) < 4 {
		return nil, fmt.Errorf("too few messages to compact: %d", len(messages))
	}

	// Step 1: Extract tool failures and file ops before summarization.
	toolFailures := extractToolFailures(messages, compactMaxToolFailures, compactToolFailureChars)
	readFiles, modifiedFiles := extractModifiedFiles(messages)

	// Step 2: Split preserved recent turns from summarize pool.
	preserved, toSummarize := splitPreservedTurns(messages)

	if len(toSummarize) == 0 {
		return &compactResult{
			Summary:        "",
			PreservedTurns: preserved,
			ToolFailures:   toolFailures,
			ReadFiles:      readFiles,
			ModifiedFiles:  modifiedFiles,
		}, nil
	}

	// Step 3: Prune history for context share budget.
	budgetTokens := int(float64(windowTokens) * compactMaxHistoryShare)
	toSummarize = pruneForBudget(toSummarize, budgetTokens)

	// Step 4: Staged LLM summarization.
	summary, err := c.summarizeInStages(toSummarize)
	if err != nil {
		return nil, fmt.Errorf("summarize: %w", err)
	}

	// Step 5: Quality audit.
	if err := auditSummaryQuality(summary, toSummarize); err != nil {
		// Retry once with explicit fix instructions.
		fixPrompt := fmt.Sprintf(
			"The previous summary failed quality checks: %v\n\nPlease regenerate, ensuring all required sections are present.",
			err,
		)
		summary, err = c.callLLM(buildSummaryPrompt(toSummarize, fixPrompt))
		if err != nil {
			return nil, fmt.Errorf("summary retry: %w", err)
		}
	}

	return &compactResult{
		Summary:        summary,
		PreservedTurns: preserved,
		ToolFailures:   toolFailures,
		ReadFiles:      readFiles,
		ModifiedFiles:  modifiedFiles,
	}, nil
}

// buildCompactedMessages assembles the final message list after compaction.
func buildCompactedMessages(system interface{}, result *compactResult) []interface{} {
	var messages []interface{}

	// System message preserved.
	if system != nil {
		messages = append(messages, system)
	}

	// Summary as an assistant message.
	if result.Summary != "" {
		enriched := enrichSummary(result)
		messages = append(messages, map[string]interface{}{
			"role":    "assistant",
			"content": enriched,
		})
	}

	// Recent turns preserved verbatim.
	messages = append(messages, result.PreservedTurns...)

	return messages
}

// enrichSummary adds structured metadata to the summary text.
func enrichSummary(result *compactResult) string {
	var b strings.Builder
	b.WriteString(result.Summary)

	if len(result.ToolFailures) > 0 {
		b.WriteString("\n\n## Tool Failures\n")
		for _, f := range result.ToolFailures {
			b.WriteString("- ")
			b.WriteString(f)
			b.WriteString("\n")
		}
	}

	if len(result.ReadFiles) > 0 {
		b.WriteString("\n<read-files>\n")
		for _, f := range result.ReadFiles {
			b.WriteString(f)
			b.WriteString("\n")
		}
		b.WriteString("</read-files>\n")
	}

	if len(result.ModifiedFiles) > 0 {
		b.WriteString("\n<modified-files>\n")
		for _, f := range result.ModifiedFiles {
			b.WriteString(f)
			b.WriteString("\n")
		}
		b.WriteString("</modified-files>\n")
	}

	return b.String()
}

// splitPreservedTurns separates the last N user turns (with associated
// assistant replies) from the message pool.
func splitPreservedTurns(messages []interface{}) (preserved, rest []interface{}) {
	// Find last N user message indices.
	var userIndices []int
	for i, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if isUserMessage(m) {
			userIndices = append(userIndices, i)
		}
	}

	preserveFrom := len(userIndices) - compactPreservedTurns
	if preserveFrom < 0 {
		preserveFrom = 0
	}

	if preserveFrom >= len(userIndices) {
		return nil, messages
	}

	splitIdx := userIndices[preserveFrom]

	// Skip system message at index 0 when splitting.
	restStart := 0
	if len(messages) > 0 {
		m, ok := messages[0].(map[string]interface{})
		if ok && isSystemMessage(m) {
			restStart = 1
		}
	}

	rest = messages[restStart:splitIdx]
	preserved = messages[splitIdx:]

	// Truncate each preserved turn to the limit.
	for i, msg := range preserved {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		content := messageContentString(m)
		if len(content) > compactPreservedTurnChars {
			m["content"] = content[:compactPreservedTurnChars] + "..."
			preserved[i] = m
		}
	}

	return preserved, rest
}

// pruneForBudget trims messages from the front until under budget tokens.
func pruneForBudget(messages []interface{}, budgetTokens int) []interface{} {
	for len(messages) > 1 && estimateMessagesTokens(messages) > budgetTokens {
		messages = messages[1:]
	}
	return messages
}

// summarizeInStages handles single-chunk or multi-chunk summarization.
func (c *compactor) summarizeInStages(messages []interface{}) (string, error) {
	if len(messages) < 4 {
		// Single-pass summarization.
		prompt := buildSummaryPrompt(messages, "")
		return c.callLLM(prompt)
	}

	// Multi-chunk: split into N chunks, summarize each, then merge.
	chunks := splitIntoChunks(messages, compactChunkCount)
	var partials []string
	for i, chunk := range chunks {
		prompt := buildSummaryPrompt(chunk, fmt.Sprintf("This is part %d of %d.", i+1, len(chunks)))
		summary, err := c.callLLMWithRetry(prompt, compactMaxRetries)
		if err != nil {
			return "", fmt.Errorf("chunk %d: %w", i+1, err)
		}
		partials = append(partials, summary)
	}

	// Merge partial summaries.
	mergePrompt := buildMergePrompt(partials)
	return c.callLLMWithRetry(mergePrompt, compactMaxRetries)
}

// splitIntoChunks divides messages into n roughly equal chunks by token count.
func splitIntoChunks(messages []interface{}, n int) [][]interface{} {
	if n <= 1 || len(messages) <= n {
		return [][]interface{}{messages}
	}
	totalTokens := estimateMessagesTokens(messages)
	targetPerChunk := totalTokens / n
	if targetPerChunk == 0 {
		targetPerChunk = 1
	}

	var chunks [][]interface{}
	var current []interface{}
	currentTokens := 0

	for _, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		tokens := estimateMessageTokens(m)
		if currentTokens+tokens > targetPerChunk && len(current) > 0 && len(chunks) < n-1 {
			chunks = append(chunks, current)
			current = nil
			currentTokens = 0
		}
		current = append(current, msg)
		currentTokens += tokens
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}

// callLLMWithRetry calls the LLM with exponential backoff retries.
func (c *compactor) callLLMWithRetry(prompt []interface{}, maxRetries int) (string, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		result, err := c.callLLM(prompt)
		if err == nil {
			return result, nil
		}
		lastErr = err

		// Exponential backoff: 500ms, 1s, 2s, 4s...
		backoff := time.Duration(500*(1<<attempt)) * time.Millisecond
		jitter := time.Duration(rand.IntN(500)) * time.Millisecond
		if backoff > 5*time.Second {
			backoff = 5 * time.Second
		}
		time.Sleep(backoff + jitter)
	}
	return "", fmt.Errorf("all %d retries failed: %w", maxRetries, lastErr)
}

// callLLM makes a synchronous (non-streaming) chat completion request
// to the gateway's own API.
func (c *compactor) callLLM(messages []interface{}) (string, error) {
	reqBody := map[string]interface{}{
		"model":    c.model,
		"stream":   false,
		"messages": messages,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", c.gatewayURL+"/v1/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Mark as internal to prevent context-mgr from processing this request.
	req.Header.Set("X-Context-Mgr-Internal", "true")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("LLM returned %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse OpenAI-format response.
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("unmarshal response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}
	return result.Choices[0].Message.Content, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Prompts
// ──────────────────────────────────────────────────────────────────────────────

func buildSummaryPrompt(messages []interface{}, extra string) []interface{} {
	historyJSON, _ := json.Marshal(messages)

	systemPrompt := `You are a context compaction assistant. Generate a compact, factual summary of the conversation history below.

Write the summary in the same language as the majority of the conversation.
Focus on factual content: what was discussed, what decisions were made, what is the current state.
Do NOT translate or modify code, file paths, identifiers, or error messages.

Use these EXACT section headings:
## Decisions
## Open TODOs
## Constraints/Rules
## Pending user asks
## Exact identifiers

Preserve ALL opaque identifiers verbatim (UUIDs, hashes, IDs, tokens, API keys, hostnames, IPs, ports, URLs, filenames).
If a section has no content, write "None." under it.`

	if extra != "" {
		systemPrompt += "\n\n" + extra
	}

	return []interface{}{
		map[string]interface{}{
			"role":    "system",
			"content": systemPrompt,
		},
		map[string]interface{}{
			"role":    "user",
			"content": "Summarize this conversation history:\n\n" + string(historyJSON),
		},
	}
}

func buildMergePrompt(partials []string) []interface{} {
	merged := strings.Join(partials, "\n\n---\n\n")

	return []interface{}{
		map[string]interface{}{
			"role": "system",
			"content": `Merge these partial summaries into a single coherent summary.
You MUST preserve: active tasks and their status, batch operation progress,
the last user request, decisions and rationale, TODOs/issues/constraints.
Prioritize recent context. Use the same section headings:
## Decisions
## Open TODOs
## Constraints/Rules
## Pending user asks
## Exact identifiers`,
		},
		map[string]interface{}{
			"role":    "user",
			"content": "Merge these partial summaries:\n\n" + merged,
		},
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Quality Audit
// ──────────────────────────────────────────────────────────────────────────────

// auditSummaryQuality checks that the summary contains all required sections
// and preserves key identifiers from recent messages.
func auditSummaryQuality(summary string, originalMessages []interface{}) error {
	// Check required section headings.
	requiredSections := []string{
		"## Decisions",
		"## Open TODOs",
		"## Constraints/Rules",
		"## Pending user asks",
		"## Exact identifiers",
	}
	var missing []string
	for _, section := range requiredSections {
		if !strings.Contains(summary, section) {
			missing = append(missing, section)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required sections: %s", strings.Join(missing, ", "))
	}

	// Check identifier preservation from recent messages.
	identifiers := extractOpaqueIdentifiers(originalMessages)
	var lostIDs []string
	for _, id := range identifiers {
		if !strings.Contains(summary, id) {
			lostIDs = append(lostIDs, id)
		}
	}
	if len(lostIDs) > 0 {
		return fmt.Errorf("lost identifiers: %s", strings.Join(lostIDs, ", "))
	}

	return nil
}

// extractOpaqueIdentifiers extracts up to 12 opaque identifiers from the
// last 10 messages. These are hex strings, URLs, file paths, host:port, etc.
func extractOpaqueIdentifiers(messages []interface{}) []string {
	const maxMessages = 10
	const maxIDs = 12

	start := len(messages) - maxMessages
	if start < 0 {
		start = 0
	}

	var ids []string
	seen := make(map[string]bool)

	for _, msg := range messages[start:] {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		content := messageContentString(m)
		if content == "" {
			continue
		}
		// Simple heuristic extraction of identifiers.
		for _, word := range strings.Fields(content) {
			if len(ids) >= maxIDs {
				return ids
			}
			if seen[word] {
				continue
			}
			if looksLikeIdentifier(word) {
				seen[word] = true
				ids = append(ids, word)
			}
		}
	}
	return ids
}

// looksLikeIdentifier returns true if the word looks like an opaque identifier.
func looksLikeIdentifier(word string) bool {
	// Must be at least 8 chars to be interesting.
	if len(word) < 8 {
		return false
	}
	// URL-like
	if strings.HasPrefix(word, "http://") || strings.HasPrefix(word, "https://") {
		return true
	}
	// File path
	if strings.HasPrefix(word, "/") && strings.Contains(word, "/") && len(word) > 10 {
		return true
	}
	// host:port
	if strings.Contains(word, ":") && !strings.Contains(word, " ") {
		colonIdx := strings.LastIndex(word, ":")
		port := word[colonIdx+1:]
		if len(port) >= 2 && len(port) <= 5 && isDigits(port) {
			return true
		}
	}
	// Hex string (UUIDs, hashes, etc.)
	hexChars := 0
	for _, c := range word {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') || c == '-' {
			hexChars++
		}
	}
	if hexChars > len(word)*3/4 && len(word) >= 16 {
		return true
	}
	return false
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}
