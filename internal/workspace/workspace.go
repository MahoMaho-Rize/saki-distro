// Package workspace provides path validation and shadow-layer filesystem
// for the agent's working directory.
//
// Shadow Layer mode:
//
//	lowerDir = user's project (read-only host mount)
//	upperDir = agent's changes (writable volume)
//
//	Read:  try upperDir first, fall back to lowerDir
//	Write: always to upperDir
//	List:  merge both directories (upper wins on conflict)
//
// Without a lowerDir, the workspace operates in single-layer mode
// (all operations go to root, same as v0.1).
package workspace

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// DefaultRoot is the default workspace root inside containers.
const DefaultRoot = "/workspace"

// W holds the workspace configuration.
type W struct {
	root      string // primary root (= upperDir in shadow mode)
	lowerDir  string // read-only host mount (empty = single-layer mode)
	blacklist map[string]bool
}

// New creates a single-layer workspace rooted at the given path.
func New(root string) *W {
	return &W{
		root: root,
		blacklist: map[string]bool{
			".env": true, ".env.local": true, ".env.production": true,
			"credentials.json": true, "secrets.yaml": true, "secrets.yml": true,
			"id_rsa": true, "id_ed25519": true,
		},
	}
}

// NewShadow creates a shadow-layer workspace.
// lowerDir is the read-only host project mount.
// upperDir is the writable staging area for agent changes.
func NewShadow(upperDir, lowerDir string) *W {
	w := New(upperDir)
	w.lowerDir = lowerDir
	return w
}

// Root returns the upper (writable) workspace root.
func (w *W) Root() string { return w.root }

// LowerDir returns the read-only lower layer path (empty if single-layer).
func (w *W) LowerDir() string { return w.lowerDir }

// IsShadow returns true if the workspace has a lower (read-only) layer.
func (w *W) IsShadow() bool { return w.lowerDir != "" }

// Resolve validates and resolves a relative path to an absolute path
// within the workspace root (upper layer). Used for WRITE operations.
func (w *W) Resolve(path string) (string, error) {
	return w.resolveIn(w.root, path)
}

// ResolveRead resolves a path for READ operations using shadow fallthrough:
// try upper first, then lower. Returns the absolute path to the existing file.
// For paths that don't exist in either layer, returns the upper path
// (so write_file + read_file roundtrips work).
func (w *W) ResolveRead(path string) (string, error) {
	upperPath, err := w.resolveIn(w.root, path)
	if err != nil {
		return "", err
	}

	// If file exists in upper, use it.
	if _, statErr := os.Stat(upperPath); statErr == nil {
		return upperPath, nil
	}

	// If no lower layer, return upper (may not exist — caller handles).
	if w.lowerDir == "" {
		return upperPath, nil
	}

	// Try lower layer.
	lowerPath, lowerErr := w.resolveIn(w.lowerDir, path)
	if lowerErr != nil {
		// If lower blocked it (blacklist/escape), propagate the error —
		// the file exists in lower but is denied.
		if _, statErr := os.Stat(filepath.Join(w.lowerDir, filepath.Clean(path))); statErr == nil {
			return "", lowerErr // file exists but access denied
		}
		return upperPath, nil // genuinely not in lower, fall back to upper
	}
	if _, statErr := os.Stat(lowerPath); statErr == nil {
		return lowerPath, nil
	}

	// Neither layer has it — return upper path.
	return upperPath, nil
}

// ListMerged lists a directory by merging upper and lower layers.
// Upper entries win on name conflicts. Directories from both layers
// are included. Used by list_dir and glob tools.
func (w *W) ListMerged(relPath string) ([]fs.DirEntry, error) {
	upperAbs, err := w.resolveIn(w.root, relPath)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var result []fs.DirEntry

	// Upper layer first (takes priority).
	if entries, readErr := os.ReadDir(upperAbs); readErr == nil {
		for _, e := range entries {
			seen[e.Name()] = true
			result = append(result, e)
		}
	}

	// Lower layer (only entries not in upper).
	if w.lowerDir != "" {
		lowerAbs, lowerErr := w.resolveIn(w.lowerDir, relPath)
		if lowerErr == nil {
			if entries, readErr := os.ReadDir(lowerAbs); readErr == nil {
				for _, e := range entries {
					if !seen[e.Name()] {
						result = append(result, e)
					}
				}
			}
		}
	}

	return result, nil
}

// WalkMerged walks the merged view of upper + lower layers.
// Upper files take priority. The walkFn receives paths relative to root.
func (w *W) WalkMerged(relPath string, walkFn func(relPath string, d fs.DirEntry, err error) error) error {
	upperAbs, err := w.resolveIn(w.root, relPath)
	if err != nil {
		return err
	}

	seen := make(map[string]bool)

	// Walk upper layer.
	if _, statErr := os.Stat(upperAbs); statErr == nil {
		_ = filepath.WalkDir(upperAbs, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return fs.SkipDir
			}
			rel, _ := filepath.Rel(w.root, path)
			seen[rel] = true
			return walkFn(rel, d, nil)
		})
	}

	// Walk lower layer (only unseen paths).
	if w.lowerDir != "" {
		lowerAbs, lowerErr := w.resolveIn(w.lowerDir, relPath)
		if lowerErr == nil {
			if _, statErr := os.Stat(lowerAbs); statErr == nil {
				_ = filepath.WalkDir(lowerAbs, func(path string, d fs.DirEntry, walkErr error) error {
					if walkErr != nil {
						return fs.SkipDir
					}
					rel, _ := filepath.Rel(w.lowerDir, path)
					if seen[rel] {
						return nil //nolint:nilerr // intentional: skip already-seen files
					}
					return walkFn(rel, d, nil)
				})
			}
		}
	}

	return nil
}

// DiffFiles returns a list of files that exist in the upper layer
// (agent's changes). Each entry is relative to root.
func (w *W) DiffFiles() ([]string, error) {
	var changed []string
	err := filepath.WalkDir(w.root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fs.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(w.root, path)
		changed = append(changed, rel)
		return nil
	})
	return changed, err
}

// EnsureRoot creates the workspace root directory if it doesn't exist.
func (w *W) EnsureRoot() error {
	return os.MkdirAll(w.root, 0o755)
}

// AtomicWrite writes data to a file atomically via temp file + rename.
// This prevents partial writes from corrupting files (TOCTOU safety).
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".saki-atomic-*")
	if err != nil {
		return fmt.Errorf("atomic write: create temp: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("atomic write: write: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("atomic write: chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("atomic write: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("atomic write: rename: %w", err)
	}
	return nil
}

// --- internal ---

func (w *W) resolveIn(root, path string) (string, error) {
	if path == "" {
		return root, nil
	}

	var abs string
	if filepath.IsAbs(path) {
		abs = filepath.Clean(path)
	} else {
		abs = filepath.Join(root, path)
	}

	// Resolve symlinks to detect escapes.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// File might not exist yet. Walk up to nearest existing ancestor.
		ancestor := abs
		for {
			ancestor = filepath.Dir(ancestor)
			if ancestor == "/" || ancestor == "." {
				break
			}
			resolvedAnc, ancErr := filepath.EvalSymlinks(ancestor)
			if ancErr == nil {
				if !strings.HasPrefix(resolvedAnc, root) {
					return "", fmt.Errorf("workspace: path escapes root: %s", path)
				}
				return abs, nil
			}
		}
		if !strings.HasPrefix(abs, root) {
			return "", fmt.Errorf("workspace: path escapes root: %s", path)
		}
		return abs, nil
	}

	if !strings.HasPrefix(resolved, root) {
		return "", fmt.Errorf("workspace: path escapes root: %s", path)
	}

	base := filepath.Base(resolved)
	if w.blacklist[base] {
		return "", fmt.Errorf("workspace: access denied: %s", base)
	}

	// Hardlink escape detection: nlink > 1 means the file has multiple
	// directory entries. An attacker could hardlink a file outside the
	// workspace to a path inside it, bypassing path-based checks.
	if info, statErr := os.Lstat(resolved); statErr == nil && info.Mode().IsRegular() {
		if sys, ok := info.Sys().(*syscall.Stat_t); ok && sys.Nlink > 1 {
			return "", fmt.Errorf("workspace: hardlink detected (nlink=%d): %s", sys.Nlink, path)
		}
	}

	return resolved, nil
}
