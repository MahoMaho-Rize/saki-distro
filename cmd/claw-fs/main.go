// claw-fs is an MCP Server that provides filesystem tools for the Claw coding agent.
// It exposes read_file, write_file, edit_file, list_dir, glob, and grep as MCP tools,
// all confined to a workspace directory.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"claw-distro/internal/mcpserver"
	"claw-distro/internal/workspace"
)

func main() {
	root := os.Getenv("CLAW_WORKSPACE")
	if root == "" {
		root = workspace.DefaultRoot
	}
	addr := os.Getenv("CLAW_FS_ADDR")
	if addr == "" {
		addr = ":9100"
	}

	ws := workspace.New(root)
	if err := ws.EnsureRoot(); err != nil {
		fmt.Fprintf(os.Stderr, "claw-fs: ensure workspace: %v\n", err)
		os.Exit(1)
	}

	srv := mcpserver.New("claw-fs", "0.1.0")
	registerTools(srv, ws)

	if err := srv.ListenAndServe(addr); err != nil {
		fmt.Fprintf(os.Stderr, "claw-fs: %v\n", err)
		os.Exit(1)
	}
}

func registerTools(srv *mcpserver.Server, ws *workspace.W) {
	srv.AddTool(mcpserver.Tool{
		Name:        "read_file",
		Description: "Read a file's contents. Supports optional byte offset and line limit.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"path"},
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "File path relative to workspace"},
				"offset": map[string]any{"type": "integer", "description": "Start line (1-indexed, default 1)"},
				"limit":  map[string]any{"type": "integer", "description": "Max lines to return (default 2000)"},
			},
		}),
	}, func(ctx context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}

		abs, err := ws.Resolve(p.Path)
		if err != nil {
			return mcpserver.ErrorResult(err.Error())
		}

		data, err := os.ReadFile(abs)
		if err != nil {
			return mcpserver.ErrorResult(err.Error())
		}

		// Line-based offset/limit
		lines := strings.Split(string(data), "\n")
		offset := max(p.Offset, 1) - 1 // convert to 0-indexed
		if offset > len(lines) {
			offset = len(lines)
		}
		limit := p.Limit
		if limit <= 0 {
			limit = 2000
		}
		end := min(offset+limit, len(lines))

		var buf strings.Builder
		for i := offset; i < end; i++ {
			fmt.Fprintf(&buf, "%d: %s\n", i+1, lines[i])
		}
		return mcpserver.SuccessResult(buf.String())
	})

	srv.AddTool(mcpserver.Tool{
		Name:        "write_file",
		Description: "Create or overwrite a file with the given content.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"path", "content"},
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "File path relative to workspace"},
				"content": map[string]any{"type": "string", "description": "File content to write"},
			},
		}),
	}, func(ctx context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}

		abs, err := ws.Resolve(p.Path)
		if err != nil {
			return mcpserver.ErrorResult(err.Error())
		}

		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return mcpserver.ErrorResult(err.Error())
		}
		if err := os.WriteFile(abs, []byte(p.Content), 0o600); err != nil {
			return mcpserver.ErrorResult(err.Error())
		}
		return mcpserver.SuccessResult("ok")
	})

	srv.AddTool(mcpserver.Tool{
		Name:        "edit_file",
		Description: "Replace an exact string in a file. Fails if old_string is not found or matches multiple times.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"path", "old_string", "new_string"},
			"properties": map[string]any{
				"path":       map[string]any{"type": "string", "description": "File path relative to workspace"},
				"old_string": map[string]any{"type": "string", "description": "Exact text to find"},
				"new_string": map[string]any{"type": "string", "description": "Replacement text"},
			},
		}),
	}, func(ctx context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			Path      string `json:"path"`
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}

		abs, err := ws.Resolve(p.Path)
		if err != nil {
			return mcpserver.ErrorResult(err.Error())
		}

		data, err := os.ReadFile(abs)
		if err != nil {
			return mcpserver.ErrorResult(err.Error())
		}

		content := string(data)
		count := strings.Count(content, p.OldString)
		switch count {
		case 0:
			return mcpserver.ErrorResult("old_string not found in file")
		case 1:
			// ok
		default:
			return mcpserver.ErrorResult(fmt.Sprintf("old_string found %d times; must be unique", count))
		}

		result := strings.Replace(content, p.OldString, p.NewString, 1)
		if err := os.WriteFile(abs, []byte(result), 0o600); err != nil {
			return mcpserver.ErrorResult(err.Error())
		}
		return mcpserver.SuccessResult("ok")
	})

	srv.AddTool(mcpserver.Tool{
		Name:        "list_dir",
		Description: "List files and directories at the given path.",
		InputSchema: jsonSchema(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":      map[string]any{"type": "string", "description": "Directory path (default: workspace root)"},
				"recursive": map[string]any{"type": "boolean", "description": "Recurse into subdirectories"},
			},
		}),
	}, func(ctx context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			Path      string `json:"path"`
			Recursive bool   `json:"recursive"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}

		abs, err := ws.Resolve(p.Path)
		if err != nil {
			return mcpserver.ErrorResult(err.Error())
		}

		var buf strings.Builder
		if p.Recursive {
			_ = filepath.WalkDir(abs, func(path string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return fs.SkipDir
				}
				rel, _ := filepath.Rel(ws.Root(), path)
				if d.IsDir() {
					fmt.Fprintf(&buf, "%s/\n", rel)
				} else {
					fmt.Fprintf(&buf, "%s\n", rel)
				}
				return nil
			})
		} else {
			entries, err := os.ReadDir(abs)
			if err != nil {
				return mcpserver.ErrorResult(err.Error())
			}
			for _, e := range entries {
				if e.IsDir() {
					fmt.Fprintf(&buf, "%s/\n", e.Name())
				} else {
					fmt.Fprintf(&buf, "%s\n", e.Name())
				}
			}
		}
		return mcpserver.SuccessResult(buf.String())
	})

	srv.AddTool(mcpserver.Tool{
		Name:        "glob",
		Description: "Find files matching a glob pattern (e.g. '**/*.go').",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"pattern"},
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Glob pattern"},
				"path":    map[string]any{"type": "string", "description": "Base directory (default: workspace root)"},
			},
		}),
	}, func(ctx context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}

		base, err := ws.Resolve(p.Path)
		if err != nil {
			return mcpserver.ErrorResult(err.Error())
		}

		var matches []string
		_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return fs.SkipDir
			}
			rel, _ := filepath.Rel(base, path)
			matched, _ := filepath.Match(p.Pattern, filepath.Base(path))
			if matched {
				matches = append(matches, rel)
			}
			return nil
		})
		return mcpserver.SuccessResult(strings.Join(matches, "\n"))
	})

	srv.AddTool(mcpserver.Tool{
		Name:        "grep",
		Description: "Search file contents for a regex pattern.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"pattern"},
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Regex pattern"},
				"path":    map[string]any{"type": "string", "description": "Directory to search (default: workspace root)"},
				"include": map[string]any{"type": "string", "description": "File glob filter (e.g. '*.go')"},
			},
		}),
	}, func(ctx context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
			Include string `json:"include"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}

		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			return mcpserver.ErrorResult("invalid regex: " + err.Error())
		}

		base, err := ws.Resolve(p.Path)
		if err != nil {
			return mcpserver.ErrorResult(err.Error())
		}

		var buf strings.Builder
		matchCount := 0
		_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return fs.SkipDir
			}
			if d.IsDir() || matchCount >= 200 {
				return nil
			}
			if p.Include != "" {
				matched, _ := filepath.Match(p.Include, d.Name())
				if !matched {
					return nil
				}
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil //nolint:nilerr // skip unreadable files
			}
			rel, _ := filepath.Rel(ws.Root(), path)
			lines := strings.Split(string(data), "\n")
			for i, line := range lines {
				if re.MatchString(line) {
					fmt.Fprintf(&buf, "%s:%d: %s\n", rel, i+1, line)
					matchCount++
					if matchCount >= 200 {
						return nil
					}
				}
			}
			return nil
		})
		if matchCount == 0 {
			return mcpserver.SuccessResult("no matches found")
		}
		return mcpserver.SuccessResult(buf.String())
	})
}

func jsonSchema(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
