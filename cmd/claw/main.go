// claw is a CLI for interacting with the TAG Gateway-based coding agent.
// It provides multi-turn conversation with automatic session management,
// SSE streaming output, and standard OpenAI-compatible API communication.
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
	model := flag.String("model", envOr("CLAW_MODEL", "claude-sonnet-4-20250514"), "LLM model name")
	session := flag.String("s", envOr("CLAW_SESSION", ""), "session key for multi-turn (auto-generated if empty)")
	system := flag.String("system", "", "system prompt override")
	interactive := flag.Bool("i", false, "interactive mode (multi-turn REPL)")
	flag.Parse()

	// Session key: use provided, env, or generate.
	sessionKey := *session
	if sessionKey == "" {
		sessionKey = fmt.Sprintf("claw-%d", time.Now().UnixNano()%1000000)
	}

	// Get message from args, stdin, or enter interactive mode.
	message := strings.Join(flag.Args(), " ")

	if *interactive || message == "" {
		if message != "" {
			// Run first message, then enter REPL.
			runTurn(*endpoint, *model, sessionKey, *system, message)
		}
		repl(*endpoint, *model, sessionKey, *system)
		return
	}

	// Single-shot mode.
	if !runTurn(*endpoint, *model, sessionKey, *system, message) {
		os.Exit(1)
	}
}

// repl runs an interactive multi-turn conversation loop.
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

// runTurn sends one message and streams the response.
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

// streamSSE reads SSE frames and prints content deltas to stdout.
func streamSSE(body io.Reader) bool {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			fmt.Println()
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
			continue // skip malformed frames (e.g., truncated hijack frames)
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
