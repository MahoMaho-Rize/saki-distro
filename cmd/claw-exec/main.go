// claw-exec is an MCP Server that provides shell execution tools for the Claw coding agent.
// It exposes exec, process_start, process_send, and process_poll as MCP tools.
// All commands run inside the container, confined to the workspace directory.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"claw-distro/internal/mcpserver"
	"claw-distro/internal/workspace"
)

const (
	maxOutputBytes  = 1 << 20 // 1MB per stream
	defaultTimeout  = 30 * time.Second
	maxTimeout      = 300 * time.Second
	maxProcessCount = 32
)

// processEntry tracks a background process.
type processEntry struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	outBuf *limitedBuffer
	errBuf *limitedBuffer
}

type processTable struct {
	mu      sync.Mutex
	entries map[int]*processEntry
	nextID  int
}

func newProcessTable() *processTable {
	return &processTable{entries: make(map[int]*processEntry)}
}

func (pt *processTable) add(e *processEntry) (int, error) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if len(pt.entries) >= maxProcessCount {
		return 0, fmt.Errorf("process limit reached (%d)", maxProcessCount)
	}
	pt.nextID++
	id := pt.nextID
	pt.entries[id] = e
	return id, nil
}

func (pt *processTable) get(id int) (*processEntry, bool) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	e, ok := pt.entries[id]
	return e, ok
}

func (pt *processTable) remove(id int) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	delete(pt.entries, id)
}

func main() {
	root := os.Getenv("CLAW_WORKSPACE")
	if root == "" {
		root = workspace.DefaultRoot
	}
	addr := os.Getenv("CLAW_EXEC_ADDR")
	if addr == "" {
		addr = ":9101"
	}

	ws := workspace.New(root)
	pt := newProcessTable()

	srv := mcpserver.New("claw-exec", "0.1.0")
	registerTools(srv, ws, pt)

	if err := srv.ListenAndServe(addr); err != nil {
		fmt.Fprintf(os.Stderr, "claw-exec: %v\n", err)
		os.Exit(1)
	}
}

func registerTools(srv *mcpserver.Server, ws *workspace.W, pt *processTable) {
	srv.AddTool(mcpserver.Tool{
		Name:        "exec",
		Description: "Execute a shell command and return stdout, stderr, and exit code. Blocks until completion or timeout.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"command"},
			"properties": map[string]any{
				"command":     map[string]any{"type": "string", "description": "Shell command to execute"},
				"timeout_ms":  map[string]any{"type": "integer", "description": "Timeout in milliseconds (default 30000, max 300000)"},
				"working_dir": map[string]any{"type": "string", "description": "Working directory relative to workspace"},
			},
		}),
	}, func(ctx context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			Command    string `json:"command"`
			TimeoutMs  int    `json:"timeout_ms"`
			WorkingDir string `json:"working_dir"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}

		timeout := defaultTimeout
		if p.TimeoutMs > 0 {
			timeout = time.Duration(p.TimeoutMs) * time.Millisecond
			if timeout > maxTimeout {
				timeout = maxTimeout
			}
		}

		dir := ws.Root()
		if p.WorkingDir != "" {
			resolved, err := ws.Resolve(p.WorkingDir)
			if err != nil {
				return mcpserver.ErrorResult(err.Error())
			}
			dir = resolved
		}

		execCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		cmd := exec.CommandContext(execCtx, "sh", "-c", p.Command)
		cmd.Dir = dir

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &limitedWriter{w: &stdout, limit: maxOutputBytes}
		cmd.Stderr = &limitedWriter{w: &stderr, limit: maxOutputBytes}

		err := cmd.Run()
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				return mcpserver.ErrorResult(err.Error())
			}
		}

		result := fmt.Sprintf("exit_code: %d\n", exitCode)
		if stdout.Len() > 0 {
			result += "--- stdout ---\n" + stdout.String()
		}
		if stderr.Len() > 0 {
			result += "--- stderr ---\n" + stderr.String()
		}
		return mcpserver.SuccessResult(result)
	})

	srv.AddTool(mcpserver.Tool{
		Name:        "process_start",
		Description: "Start a long-running background process. Returns a process ID for send/poll.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"command"},
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "Shell command"},
			},
		}),
	}, func(ctx context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}

		cmd := exec.Command("sh", "-c", p.Command)
		cmd.Dir = ws.Root()

		stdin, err := cmd.StdinPipe()
		if err != nil {
			return mcpserver.ErrorResult(err.Error())
		}

		outBuf := newLimitedBuffer(maxOutputBytes)
		errBuf := newLimitedBuffer(maxOutputBytes)
		cmd.Stdout = outBuf
		cmd.Stderr = errBuf

		if err := cmd.Start(); err != nil {
			return mcpserver.ErrorResult(err.Error())
		}

		entry := &processEntry{cmd: cmd, stdin: stdin, outBuf: outBuf, errBuf: errBuf}
		id, err := pt.add(entry)
		if err != nil {
			_ = cmd.Process.Kill()
			return mcpserver.ErrorResult(err.Error())
		}

		// Reap in background to avoid zombies.
		go func() {
			_ = cmd.Wait()
		}()

		return mcpserver.SuccessResult(fmt.Sprintf(`{"pid": %d}`, id))
	})

	srv.AddTool(mcpserver.Tool{
		Name:        "process_send",
		Description: "Send input text to a background process's stdin.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"pid", "input"},
			"properties": map[string]any{
				"pid":   map[string]any{"type": "integer", "description": "Process ID from process_start"},
				"input": map[string]any{"type": "string", "description": "Text to write to stdin"},
			},
		}),
	}, func(ctx context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			PID   int    `json:"pid"`
			Input string `json:"input"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}

		entry, ok := pt.get(p.PID)
		if !ok {
			return mcpserver.ErrorResult(fmt.Sprintf("no process with pid %d", p.PID))
		}

		if _, err := entry.stdin.Write([]byte(p.Input)); err != nil {
			return mcpserver.ErrorResult(err.Error())
		}
		return mcpserver.SuccessResult("ok")
	})

	srv.AddTool(mcpserver.Tool{
		Name:        "process_poll",
		Description: "Read new output from a background process and check if it's still running.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"pid"},
			"properties": map[string]any{
				"pid": map[string]any{"type": "integer", "description": "Process ID from process_start"},
			},
		}),
	}, func(ctx context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			PID int `json:"pid"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}

		entry, ok := pt.get(p.PID)
		if !ok {
			return mcpserver.ErrorResult(fmt.Sprintf("no process with pid %d", p.PID))
		}

		running := entry.cmd.ProcessState == nil

		stdout := entry.outBuf.Drain()
		stderr := entry.errBuf.Drain()

		var buf bytes.Buffer
		fmt.Fprintf(&buf, "running: %v\n", running)
		if len(stdout) > 0 {
			fmt.Fprintf(&buf, "--- stdout ---\n%s", stdout)
		}
		if len(stderr) > 0 {
			fmt.Fprintf(&buf, "--- stderr ---\n%s", stderr)
		}

		// Clean up finished processes.
		if !running {
			exitCode := entry.cmd.ProcessState.ExitCode()
			fmt.Fprintf(&buf, "exit_code: %d\n", exitCode)
			pt.remove(p.PID)
		}

		return mcpserver.SuccessResult(buf.String())
	})
}

// limitedWriter caps writes at a byte limit; excess is silently discarded.
type limitedWriter struct {
	w     io.Writer
	limit int
	n     int
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	remaining := lw.limit - lw.n
	if remaining <= 0 {
		return len(p), nil // discard
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	n, err := lw.w.Write(p)
	lw.n += n
	return n, err
}

// limitedBuffer is a concurrency-safe buffer with a byte cap and drain semantics.
type limitedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func newLimitedBuffer(limit int) *limitedBuffer {
	return &limitedBuffer{limit: limit}
}

func (lb *limitedBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	remaining := lb.limit - lb.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	return lb.buf.Write(p)
}

// Drain returns accumulated data and resets the buffer.
func (lb *limitedBuffer) Drain() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	s := lb.buf.String()
	lb.buf.Reset()
	return s
}

func jsonSchema(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
