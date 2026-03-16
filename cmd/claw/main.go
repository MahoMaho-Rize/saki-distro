// claw is a CLI for interacting with the TAG Gateway-based coding agent.
// It provides multi-turn conversation with automatic session management,
// SSE streaming output, and real-time agent execution tree rendering.
//
// Usage:
//
//	claw "create a hello world in Python and run it"
//	claw -s mysession "read the file I created earlier"
//	echo "fix this bug" | claw
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultEndpoint = "http://localhost:8080/v1/chat/completions"

func main() {
	endpoint := flag.String("endpoint", envOr("CLAW_ENDPOINT", defaultEndpoint), "TAG Gateway endpoint")
	model := flag.String("model", envOr("CLAW_MODEL", "claude-opus-4-6"), "LLM model name")
	session := flag.String("s", envOr("CLAW_SESSION", ""), "session key for multi-turn (auto-generated if empty)")
	system := flag.String("system", "", "system prompt override")
	interactive := flag.Bool("i", false, "interactive mode (multi-turn REPL)")
	flag.Parse()

	sessionKey := *session
	if sessionKey == "" {
		sessionKey = fmt.Sprintf("claw-%d", time.Now().UnixNano()%1000000)
	}

	message := strings.Join(flag.Args(), " ")

	if *interactive || message == "" {
		if message != "" {
			runTurn(*endpoint, *model, sessionKey, *system, message)
		}
		repl(*endpoint, *model, sessionKey, *system)
		return
	}

	if !runTurn(*endpoint, *model, sessionKey, *system, message) {
		os.Exit(1)
	}
}

func repl(endpoint, model, sessionKey, system string) {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Fprintf(os.Stderr, "claw session: %s (ctrl-d to exit)\n", sessionKey)

	for {
		fmt.Fprint(os.Stderr, "\n> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "/quit" || line == "/exit" {
			break
		}
		runTurn(endpoint, model, sessionKey, system, line)
	}
	fmt.Fprintln(os.Stderr, "\nbye")
}

func runTurn(endpoint, model, sessionKey, system, message string) bool {
	messages := []map[string]string{
		{"role": "user", "content": message},
	}
	if system != "" {
		messages = append([]map[string]string{
			{"role": "system", "content": system},
		}, messages...)
	}

	body := map[string]interface{}{
		"model":    model,
		"stream":   true,
		"messages": messages,
	}

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return false
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(bodyJSON)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if sessionKey != "" {
		req.Header.Set("X-Session-Key", sessionKey)
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		fmt.Fprintf(os.Stderr, "HTTP %d: %s\n", resp.StatusCode, string(errBody))
		return false
	}

	return streamSSE(resp.Body)
}

// ─── SSE Stream Processing with Trace Event Rendering ────────────────────────

// traceEvent mirrors the kernel's stream.TraceEvent for JSON parsing.
type traceEvent struct {
	Trace      string `json:"trace"`
	Tool       string `json:"tool,omitempty"`
	Args       string `json:"args,omitempty"`
	Result     string `json:"result,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	ExitCode   int    `json:"exit_code,omitempty"`
	Depth      int    `json:"depth,omitempty"`
	Error      string `json:"error,omitempty"`
}

// treeState tracks the current execution tree for rendering.
type treeState struct {
	depth   int
	toolIdx int
	started time.Time
}

func newTreeState() *treeState {
	return &treeState{started: time.Now()}
}

// streamSSE reads SSE frames, renders content deltas to stdout,
// and renders trace events (SSE comments) as an execution tree on stderr.
func streamSSE(body io.Reader) bool {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	tree := newTreeState()

	for scanner.Scan() {
		line := scanner.Text()

		// Trace events: SSE comment lines starting with ": {"
		if strings.HasPrefix(line, ": {") {
			handleTraceEvent(line[2:], tree) // strip ": " prefix
			continue
		}

		// Content: SSE data lines
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			renderDone(tree)
			return true
		}

		var frame struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
					Role    string `json:"role"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			continue
		}
		if len(frame.Choices) > 0 && frame.Choices[0].Delta.Content != "" {
			fmt.Print(frame.Choices[0].Delta.Content)
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "\nstream error: %v\n", err)
		return false
	}
	fmt.Println()
	return true
}

func handleTraceEvent(data string, tree *treeState) {
	var ev traceEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return
	}

	prefix := treePrefix(tree.depth)

	switch ev.Trace {
	case "tool_start":
		tree.toolIdx++
		args := ev.Args
		if len(args) > 60 {
			args = args[:60] + "..."
		}
		fmt.Fprintf(os.Stderr, "%s├── ⚙ %s", prefix, ev.Tool)
		if args != "" {
			fmt.Fprintf(os.Stderr, " (%s)", compactArgs(args))
		}
		fmt.Fprintln(os.Stderr)

	case "tool_end":
		if ev.Error != "" {
			fmt.Fprintf(os.Stderr, "%s│   ✗ error: %s (%dms)\n", prefix, truncStr(ev.Error, 60), ev.DurationMs)
		} else {
			fmt.Fprintf(os.Stderr, "%s│   ✓ done (%dms)\n", prefix, ev.DurationMs)
		}

	case "delegate_start":
		tree.depth++
		args := ev.Args
		if len(args) > 80 {
			args = args[:80] + "..."
		}
		fmt.Fprintf(os.Stderr, "%s├── ⚡ delegate_task\n", prefix)
		fmt.Fprintf(os.Stderr, "%s│   → %s\n", prefix, compactArgs(args))

	case "delegate_end":
		if ev.Error != "" {
			fmt.Fprintf(os.Stderr, "%s│   ✗ sub-agent failed: %s (%dms)\n", prefix, truncStr(ev.Error, 50), ev.DurationMs)
		} else {
			fmt.Fprintf(os.Stderr, "%s│   ✓ sub-agent done (%dms)\n", prefix, ev.DurationMs)
		}
		if tree.depth > 0 {
			tree.depth--
		}
	}
}

func renderDone(tree *treeState) {
	elapsed := time.Since(tree.started)
	fmt.Println() // newline after content
	if tree.toolIdx > 0 {
		fmt.Fprintf(os.Stderr, "└── done (%d tools, %s)\n", tree.toolIdx, formatDuration(elapsed))
	}
}

func treePrefix(depth int) string {
	if depth <= 0 {
		return ""
	}
	return strings.Repeat("│   ", depth)
}

func compactArgs(args string) string {
	// Try to extract readable summary from JSON args
	args = strings.TrimSpace(args)
	if len(args) > 2 && args[0] == '{' {
		var m map[string]interface{}
		if json.Unmarshal([]byte(args), &m) == nil {
			// Show first string value as summary
			for _, v := range m {
				if s, ok := v.(string); ok && len(s) > 0 {
					return truncStr(s, 60)
				}
			}
		}
	}
	return truncStr(args, 60)
}

func truncStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
