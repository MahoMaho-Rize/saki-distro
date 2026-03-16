package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// memoryFlusher handles Layer 5: pre-compaction memory persistence.
// Triggers an agent run to write durable memories before compaction.
type memoryFlusher struct {
	gatewayURL string
	model      string
	client     *http.Client

	// Track flush count per compaction cycle to prevent re-entrancy.
	flushCount atomic.Int32
}

func newMemoryFlusher(gatewayURL, model string) *memoryFlusher {
	return &memoryFlusher{
		gatewayURL: gatewayURL,
		model:      model,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// shouldFlush checks whether a memory flush should be triggered.
func (f *memoryFlusher) shouldFlush(totalTokens, windowTokens int) bool {
	threshold := windowTokens - flushReserveTokens - flushSoftThreshold
	if totalTokens < threshold {
		return false
	}
	// Only flush once per compaction cycle.
	return f.flushCount.Load() == 0
}

// flush triggers an agent run to write durable memories.
// The agent decides what's worth persisting. Returns nil if nothing to flush
// or if the flush agent returns a silent reply.
func (f *memoryFlusher) flush(sessionKey string) error {
	if !f.flushCount.CompareAndSwap(0, 1) {
		return nil // already flushed this cycle
	}

	now := time.Now()
	dateStr := now.Format("2006-01-02")

	userPrompt := fmt.Sprintf(`Pre-compaction memory flush. Store durable memories only in memory/%s.md (create memory/ directory if needed).

Rules:
- Treat MEMORY.md, SOUL.md, TOOLS.md, AGENTS.md as read-only.
- If memory/%s.md exists, APPEND only. Do NOT create timestamped variants.
- Focus on: key decisions, learned patterns, important file paths, task context.
- If nothing is worth storing, reply with [SILENT_REPLY_TOKEN].`, dateStr, dateStr)

	systemAddition := "Pre-compaction memory flush turn. The session is near auto-compaction; capture durable memories to disk before context is summarized."

	reqBody := map[string]interface{}{
		"model":  f.model,
		"stream": false,
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "system",
				"content": systemAddition,
			},
			map[string]interface{}{
				"role":    "user",
				"content": userPrompt,
			},
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal flush request: %w", err)
	}

	req, err := http.NewRequest("POST", f.gatewayURL+"/v1/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create flush request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Context-Mgr-Internal", "true")
	if sessionKey != "" {
		req.Header.Set("X-Session-Key", sessionKey+"-memflush")
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("flush HTTP request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body) // drain

	if resp.StatusCode != 200 {
		return fmt.Errorf("flush returned status %d", resp.StatusCode)
	}
	return nil
}

// resetCycle resets the flush counter for a new compaction cycle.
func (f *memoryFlusher) resetCycle() {
	f.flushCount.Store(0)
}
