// context-mgr is an ext_proc hook that implements five-layer context management.
//
// Layer 1: Context Pruning — cache-aware soft trim + hard clear of old tool results
// Layer 2: Tool Result Guard — per-call single-result cap + total budget enforcement
// Layer 3: Session Truncation — persistent truncation when layers 1+2 are insufficient
// Layer 4: Compaction — LLM-based summarization with quality audit
// Layer 5: Memory Flush — pre-compaction durable memory persistence
//
// PRE_REQ (BODY_MUTATE):
//   - Reads session history from session-hook metadata
//   - Injects history into body.messages[]
//   - Applies Layer 2 (guard) then Layer 1 (prune)
//   - Emits save_messages for the current user message
//
// POST_RESP (HEADER_ONLY):
//   - Reads react_trace from gateway metadata
//   - Extracts assistant response for persistence
//   - Triggers Layer 4 (compaction) and Layer 5 (flush) asynchronously
package main

import (
	"fmt"
	"os"

	"tag-gateway/hooklib"
)

func main() {
	maxTokens := 180000 // default context window budget (leave headroom from 200k)
	gatewayURL := envOrDefault("GATEWAY_URL", "http://localhost:8080")
	compactModel := envOrDefault("COMPACT_MODEL", "claude-sonnet-4-20250514")

	hook := newContextMgrHook(maxTokens, gatewayURL, compactModel)
	if err := hooklib.Run(hook); err != nil {
		fmt.Fprintf(os.Stderr, "context-mgr: %v\n", err)
		os.Exit(1)
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
