// claw-exec is an MCP Server that provides shell execution tools.
// All commands run inside Docker containers with workspace volume mount.
// The MCP server itself runs on the host; it spawns Docker containers
// for each command execution.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
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

// Container configuration
var (
	containerImage = envOr("CLAW_EXEC_IMAGE", "ubuntu:24.04")
	containerMem   = envOr("CLAW_EXEC_MEMORY", "512m")
	containerCPU   = envOr("CLAW_EXEC_CPUS", "2")
	containerPids  = envOr("CLAW_EXEC_PIDS", "100")
	networkMode    = envOr("CLAW_EXEC_NETWORK", "bridge") // bridge for pip/npm; "none" for full isolation
	containerSeq   int64
)

// Proxy env vars to pass into containers
var proxyEnvVars []string

func init() {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "NO_PROXY", "no_proxy"} {
		if v := os.Getenv(key); v != "" {
			proxyEnvVars = append(proxyEnvVars, key+"="+v)
		}
	}
}

// processEntry tracks a background container.
type processEntry struct {
	containerName string
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	outBuf        *limitedBuffer
	errBuf        *limitedBuffer
	done          chan struct{}
	exitCode      int
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
	if e, ok := pt.entries[id]; ok {
		// Cleanup container
		cleanup := exec.Command("docker", "rm", "-f", e.containerName)
		_ = cleanup.Run()
	}
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

	srv := mcpserver.New("claw-exec", "0.2.0")
	registerTools(srv, ws, pt)

	if err := srv.ListenAndServe(addr); err != nil {
		fmt.Fprintf(os.Stderr, "claw-exec: %v\n", err)
		os.Exit(1)
	}
}

// buildDockerArgs constructs the common docker run arguments.
func buildDockerArgs(containerName, workDir string, timeout time.Duration) []string {
	args := []string{
		"run", "--rm",
		"--name", containerName,
		"--network", networkMode,
		"--memory", containerMem,
		"--cpus", containerCPU,
		"--pids-limit", containerPids,
		"--security-opt", "no-new-privileges",
		"-v", workDir + ":/workspace",
		"-w", "/workspace",
	}
	// Pass proxy env vars into container
	for _, env := range proxyEnvVars {
		args = append(args, "-e", env)
	}
	return args
}

func nextContainerName() string {
	return fmt.Sprintf("saki-exec-%d-%d", os.Getpid(), atomic.AddInt64(&containerSeq, 1))
}

func registerTools(srv *mcpserver.Server, ws *workspace.W, pt *processTable) {
	srv.AddTool(mcpserver.Tool{
		Name:        "exec",
		Description: "Execute a shell command in an isolated Docker container. Returns stdout, stderr, and exit code.",
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

		containerName := nextContainerName()
		dockerArgs := buildDockerArgs(containerName, ws.Root(), timeout)
		dockerArgs = append(dockerArgs, containerImage, "sh", "-c", p.Command)

		execCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		// Zombie cleanup
		go func() {
			<-execCtx.Done()
			killCmd := exec.Command("docker", "kill", containerName)
			_ = killCmd.Run()
		}()

		cmd := exec.CommandContext(execCtx, "docker", dockerArgs...)

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &limitedWriter{w: &stdout, limit: maxOutputBytes}
		cmd.Stderr = &limitedWriter{w: &stderr, limit: maxOutputBytes}

		err := cmd.Run()
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else if strings.Contains(err.Error(), "signal: killed") {
				exitCode = 137 // SIGKILL from timeout
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
		Description: "Start a long-running background process in a Docker container. Returns a process ID for send/poll.",
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

		containerName := nextContainerName()
		dockerArgs := buildDockerArgs(containerName, ws.Root(), maxTimeout)
		dockerArgs = append(dockerArgs, "-i", containerImage, "sh", "-c", p.Command)

		cmd := exec.Command("docker", dockerArgs...)

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

		entry := &processEntry{
			containerName: containerName,
			cmd:           cmd,
			stdin:         stdin,
			outBuf:        outBuf,
			errBuf:        errBuf,
			done:          make(chan struct{}),
		}
		id, err := pt.add(entry)
		if err != nil {
			_ = cmd.Process.Kill()
			_ = exec.Command("docker", "rm", "-f", containerName).Run()
			return mcpserver.ErrorResult(err.Error())
		}

		go func() {
			_ = cmd.Wait()
			if cmd.ProcessState != nil {
				entry.exitCode = cmd.ProcessState.ExitCode()
			}
			close(entry.done)
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

		running := true
		select {
		case <-entry.done:
			running = false
		default:
		}

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

		if !running {
			fmt.Fprintf(&buf, "exit_code: %d\n", entry.exitCode)
			pt.remove(p.PID)
		}

		return mcpserver.SuccessResult(buf.String())
	})
}

// --- helpers ---

type limitedWriter struct {
	w     io.Writer
	limit int
	n     int
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	remaining := lw.limit - lw.n
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	n, err := lw.w.Write(p)
	lw.n += n
	return n, err
}

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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
