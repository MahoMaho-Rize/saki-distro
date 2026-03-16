package main

// Context management thresholds and configuration.
// All values are tunable; defaults match OpenClaw's proven settings.

const (
	// ── Token estimation ────────────────────────────────────────────
	charsPerToken              = 4    // plain text: 4 chars ≈ 1 token
	toolResultCharsPerToken    = 2    // code/JSON/paths: 2 chars ≈ 1 token (more conservative)
	imageCharEstimate          = 8000 // each image block ≈ 8000 chars ≈ 2000 tokens
	defaultContextWindowTokens = 200000

	// ── Layer 1: Context Pruning (cache-aware) ──────────────────────
	softTrimRatio        = 0.30  // soft trim triggers at 30% of window
	hardClearRatio       = 0.50  // hard clear triggers at 50% of window
	keepLastAssistants   = 3     // protect last N assistant turns
	softTrimMaxChars     = 4000  // tool results > this get head+tail trimmed
	softTrimHeadChars    = 1500  // head portion in soft trim
	softTrimTailChars    = 1500  // tail portion in soft trim
	minPrunableCharsHard = 50000 // don't hard-clear if prunable < this

	// ── Layer 2: Tool Result Context Guard ──────────────────────────
	singleResultCapRatio = 0.50 // single tool result capped at 50% of window (chars)
	totalBudgetRatio     = 0.75 // all messages capped at 75% of window (chars)

	// ── Layer 3: Session Truncation ─────────────────────────────────
	sessionTruncateRatio   = 0.30   // 30% of window for persistent truncation
	sessionTruncateHardCap = 400000 // absolute char cap for session truncation

	// ── Layer 4: Compaction ─────────────────────────────────────────
	compactTriggerRatio       = 0.80 // trigger compaction at 80% usage
	compactMaxHistoryShare    = 0.50 // summarize at most 50% of window
	compactPreservedTurns     = 3    // keep last N user turns verbatim
	compactPreservedTurnChars = 600  // each preserved turn truncated to this
	compactMaxRetries         = 3    // quality audit retries
	compactChunkCount         = 2    // split history into N chunks for parallel summarization
	compactMaxToolFailures    = 8    // max tool failures to preserve in enrichment
	compactToolFailureChars   = 240  // each tool failure entry max chars

	// ── Layer 5: Memory Flush ───────────────────────────────────────
	flushReserveTokens   = 20000           // reserve floor for flush trigger
	flushSoftThreshold   = 4000            // soft threshold for flush trigger
	flushMaxSessionBytes = 2 * 1024 * 1024 // 2MB session file trigger

	// ── Error detection patterns ────────────────────────────────────
	// Used by hasImportantTail() and overflow detection.
	importantTailCheckChars = 2000 // check last N chars for error patterns
	importantTailBudgetFrac = 0.30 // allocate 30% of budget to tail if important
	importantTailMaxChars   = 4000 // max chars allocated to tail

	// ── Cache TTL (layer 1 optimization) ────────────────────────────
	cacheTTLSeconds = 300 // 5 minutes — Anthropic prompt cache TTL
)
