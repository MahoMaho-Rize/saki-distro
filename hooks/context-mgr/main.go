// context-mgr is an ext_proc hook that manages multi-turn conversation context.
//
// PRE_REQ (BODY_MUTATE):
//   - Reads session history from session-hook metadata
//   - Injects history into body.messages[]
//   - Estimates token count and truncates old messages if needed
//   - Emits save_messages for the current user message
//
// POST_RESP (BODY_READ):
//   - Reads react_trace from gateway metadata
//   - Extracts assistant response and tool call records
//   - Emits save_messages for session-hook to persist
package main

import (
	"fmt"
	"os"

	"tag-gateway/hooklib"
)

func main() {
	maxTokens := 180000 // default context window budget (leave headroom from 200k)
	if err := hooklib.Run(&contextMgrHook{maxTokens: maxTokens}); err != nil {
		fmt.Fprintf(os.Stderr, "context-mgr: %v\n", err)
		os.Exit(1)
	}
}
