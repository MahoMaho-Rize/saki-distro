// Package workspace provides path validation for the agent's working directory.
// All file operations are confined to a root directory (default /workspace).
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultRoot is the default workspace root inside containers.
const DefaultRoot = "/workspace"

// W holds the workspace configuration.
type W struct {
	root      string
	blacklist map[string]bool
}

// New creates a workspace rooted at the given path.
func New(root string) *W {
	w := &W{
		root: root,
		blacklist: map[string]bool{
			".env": true, ".env.local": true, ".env.production": true,
			"credentials.json": true, "secrets.yaml": true, "secrets.yml": true,
			"id_rsa": true, "id_ed25519": true,
		},
	}
	return w
}

// Root returns the workspace root path.
func (w *W) Root() string { return w.root }

// Resolve validates and resolves a path to an absolute path within the workspace.
// It follows symlinks and ensures the resolved path is under root.
func (w *W) Resolve(path string) (string, error) {
	if path == "" {
		return w.root, nil
	}

	var abs string
	if filepath.IsAbs(path) {
		abs = filepath.Clean(path)
	} else {
		abs = filepath.Join(w.root, path)
	}

	// Resolve symlinks to detect escapes.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// File might not exist yet (write_file, nested dirs).
		// Walk up to the nearest existing ancestor and verify it's under root.
		ancestor := abs
		for {
			ancestor = filepath.Dir(ancestor)
			if ancestor == "/" || ancestor == "." {
				break
			}
			resolvedAnc, ancErr := filepath.EvalSymlinks(ancestor)
			if ancErr == nil {
				if !strings.HasPrefix(resolvedAnc, w.root) {
					return "", fmt.Errorf("workspace: path escapes root: %s", path)
				}
				return abs, nil
			}
		}
		// Clean path check: must be lexically under root.
		if !strings.HasPrefix(abs, w.root) {
			return "", fmt.Errorf("workspace: path escapes root: %s", path)
		}
		return abs, nil
	}

	if !strings.HasPrefix(resolved, w.root) {
		return "", fmt.Errorf("workspace: path escapes root: %s", path)
	}

	base := filepath.Base(resolved)
	if w.blacklist[base] {
		return "", fmt.Errorf("workspace: access denied: %s", base)
	}

	return resolved, nil
}

// EnsureRoot creates the workspace root directory if it doesn't exist.
func (w *W) EnsureRoot() error {
	return os.MkdirAll(w.root, 0o755)
}
