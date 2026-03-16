// claw-exec is an MCP Server that provides shell execution tools.
//
// Two runtime modes (auto-detected, or set via CLAW_EXEC_RUNTIME):
//
//	bwrap:  bubblewrap sandbox using exported Docker rootfs.
//	        8ms startup, zero daemon, pip persists via home dir bind.
//	docker: persistent Docker container (fallback when bwrap unavailable).
//	        docker exec into saki-sandbox container.
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
	"time"

	"claw-distro/internal/mcpserver"
	"claw-distro/internal/workspace"
)

const (
	maxOutputBytes  = 1 << 20
	defaultTimeout  = 30 * time.Second
	maxTimeout      = 300 * time.Second
	maxProcessCount = 32
)

// Runtime configuration
var (
	runtime      string // "bwrap" or "docker"
	rootfsDir    = envOr("CLAW_EXEC_ROOTFS", "/opt/saki-rootfs")
	sandboxImage = envOr("CLAW_EXEC_IMAGE", "saki-sandbox:latest")
	sandboxName  = envOr("CLAW_EXEC_CONTAINER", "saki-sandbox")
	networkMode  = envOr("CLAW_EXEC_NETWORK", "bridge")
	memoryLimit  = envOr("CLAW_EXEC_MEMORY", "512m")
	cpuLimit     = envOr("CLAW_EXEC_CPUS", "2")
	pidsLimit    = envOr("CLAW_EXEC_PIDS", "100")
	agentHome    string // persistent home directory for bwrap
)

var proxyEnvVars []string

func init() {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "NO_PROXY", "no_proxy"} {
		if v := os.Getenv(key); v != "" {
			proxyEnvVars = append(proxyEnvVars, key+"="+v)
		}
	}
}

type processEntry struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	outBuf   *limitedBuffer
	errBuf   *limitedBuffer
	done     chan struct{}
	exitCode int
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
	pt.entries[pt.nextID] = e
	return pt.nextID, nil
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

	// Detect runtime
	runtime = detectRuntime()
	if err := setupRuntime(ws.Root()); err != nil {
		fmt.Fprintf(os.Stderr, "claw-exec: runtime setup failed: %v\n", err)
		os.Exit(1)
	}

	srv := mcpserver.New("claw-exec", "1.0.0")
	registerTools(srv, ws, pt)

	if err := srv.ListenAndServe(addr); err != nil {
		fmt.Fprintf(os.Stderr, "claw-exec: %v\n", err)
		os.Exit(1)
	}
}

// ─── Runtime detection and setup ───────────────────────────────────

func detectRuntime() string {
	if r := os.Getenv("CLAW_EXEC_RUNTIME"); r != "" {
		return r
	}
	// Prefer bwrap if rootfs exists and bwrap is available
	if _, err := os.Stat(rootfsDir); err == nil {
		if _, err := exec.LookPath("bwrap"); err == nil {
			return "bwrap"
		}
	}
	return "docker"
}

func setupRuntime(workDir string) error {
	switch runtime {
	case "bwrap":
		return setupBwrap(workDir)
	case "docker":
		return setupDocker(workDir)
	default:
		return fmt.Errorf("unknown runtime %q", runtime)
	}
}

func setupBwrap(workDir string) error {
	// Create persistent agent home for pip/npm state
	dataDir := os.Getenv("CLAW_DATA_DIR")
	if dataDir == "" {
		dataDir = workDir
	}
	agentHome = dataDir + "/.agent-home"
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		return fmt.Errorf("create agent home: %w", err)
	}
	fmt.Fprintf(os.Stderr, "claw-exec: runtime=bwrap rootfs=%s home=%s\n", rootfsDir, agentHome)
	return nil
}

func setupDocker(workDir string) error {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", sandboxName).Output()
	if err == nil && strings.TrimSpace(string(out)) == "true" {
		fmt.Fprintf(os.Stderr, "claw-exec: runtime=docker container=%s (reattached)\n", sandboxName)
		return nil
	}
	_ = exec.Command("docker", "rm", "-f", sandboxName).Run()

	args := []string{
		"run", "-d", "--name", sandboxName,
		"--network", networkMode,
		"--memory", memoryLimit, "--cpus", cpuLimit,
		"--pids-limit", pidsLimit,
		"--security-opt", "no-new-privileges",
		"-v", workDir + ":/workspace", "-w", "/workspace",
	}
	for _, env := range proxyEnvVars {
		args = append(args, "-e", env)
	}
	args = append(args, sandboxImage)

	cmd := exec.Command("docker", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker run: %w", err)
	}
	fmt.Fprintf(os.Stderr, "claw-exec: runtime=docker container=%s started\n", sandboxName)
	return nil
}

// ─── Command execution (runtime-agnostic) ──────────────────────────

func sandboxExec(ctx context.Context, workDir, command string, interactive bool) *exec.Cmd {
	switch runtime {
	case "bwrap":
		return bwrapExec(ctx, workDir, command, interactive)
	default:
		return dockerExec(ctx, command, interactive)
	}
}

func bwrapExec(ctx context.Context, workDir, command string, interactive bool) *exec.Cmd {
	args := []string{
		"--ro-bind", rootfsDir, "/",
		"--ro-bind", "/etc/resolv.conf", "/etc/resolv.conf",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--bind", agentHome, "/home/agent",
		"--bind", workDir, "/workspace",
		"--setenv", "HOME", "/home/agent",
		"--setenv", "PATH", "/home/agent/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"--setenv", "PYTHONDONTWRITEBYTECODE", "1",
		"--setenv", "PYTHONUNBUFFERED", "1",
		"--setenv", "DEBIAN_FRONTEND", "noninteractive",
		"--chdir", "/workspace",
		"--unshare-pid",
		"--die-with-parent",
	}
	for _, kv := range proxyEnvVars {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			args = append(args, "--setenv", parts[0], parts[1])
		}
	}
	args = append(args, "--", "sh", "-c", command)
	return exec.CommandContext(ctx, "bwrap", args...)
}

func dockerExec(ctx context.Context, command string, interactive bool) *exec.Cmd {
	args := []string{"exec"}
	if interactive {
		args = append(args, "-i")
	}
	args = append(args, sandboxName, "sh", "-c", command)
	return exec.CommandContext(ctx, "docker", args...)
}

// ─── MCP tool registration ─────────────────────────────────────────

func registerTools(srv *mcpserver.Server, ws *workspace.W, pt *processTable) {
	srv.AddTool(mcpserver.Tool{
		Name:        "exec",
		Description: "Execute a shell command in the sandbox. pip/npm installs persist across calls.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"command"},
			"properties": map[string]any{
				"command":    map[string]any{"type": "string", "description": "Shell command"},
				"timeout_ms": map[string]any{"type": "integer", "description": "Timeout ms (default 30000, max 300000)"},
			},
		}),
	}, func(ctx context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			Command   string `json:"command"`
			TimeoutMs int    `json:"timeout_ms"`
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

		execCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		cmd := sandboxExec(execCtx, ws.Root(), p.Command, false)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &limitedWriter{w: &stdout, limit: maxOutputBytes}
		cmd.Stderr = &limitedWriter{w: &stderr, limit: maxOutputBytes}

		err := cmd.Run()
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else if strings.Contains(err.Error(), "signal: killed") {
				exitCode = 137
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
		Description: "Start a long-running background process in the sandbox.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"command"},
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
			},
		}),
	}, func(ctx context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}

		cmd := sandboxExec(context.Background(), ws.Root(), p.Command, true)
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

		entry := &processEntry{cmd: cmd, stdin: stdin, outBuf: outBuf, errBuf: errBuf, done: make(chan struct{})}
		id, err := pt.add(entry)
		if err != nil {
			_ = cmd.Process.Kill()
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
		Description: "Send input to a background process's stdin.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"pid", "input"},
			"properties": map[string]any{
				"pid":   map[string]any{"type": "integer"},
				"input": map[string]any{"type": "string"},
			},
		}),
	}, func(_ context.Context, args json.RawMessage) *mcpserver.CallToolResult {
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
		Description: "Read output from a background process.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"pid"},
			"properties": map[string]any{
				"pid": map[string]any{"type": "integer"},
			},
		}),
	}, func(_ context.Context, args json.RawMessage) *mcpserver.CallToolResult {
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

// ─── helpers ───────────────────────────────────────────────────────

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
