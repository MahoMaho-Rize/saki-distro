package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// --- Ported from OpenClaw sandbox-paths.test.ts ---

func TestResolve_AllowsPathsUnderRoot(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	ws := New(dir)

	got, err := ws.Resolve("data/file.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(dir, "data/file.txt")
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestResolve_AllowsAbsolutePathUnderRoot(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	ws := New(dir)

	abs := filepath.Join(dir, "docs/readme.md")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ws.Resolve(abs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != abs {
		t.Errorf("got %s, want %s", got, abs)
	}
}

func TestResolve_RejectsPathsOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	ws := New(dir)

	_, err := ws.Resolve("/etc/passwd")
	if err == nil {
		t.Fatal("expected error for path outside root, got nil")
	}
}

func TestResolve_RejectsTraversalViaRelative(t *testing.T) {
	dir := t.TempDir()
	ws := New(dir)

	_, err := ws.Resolve("../etc/passwd")
	if err == nil {
		t.Fatal("expected error for path traversal, got nil")
	}
}

func TestResolve_RejectsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	ws := New(dir)

	// Create a symlink inside workspace pointing outside.
	link := filepath.Join(dir, "escape")
	if err := os.Symlink("/etc", link); err != nil {
		t.Skip("symlink not supported")
	}

	_, err := ws.Resolve("escape/passwd")
	if err == nil {
		t.Fatal("expected error for symlink escape, got nil")
	}
}

func TestResolve_RejectsBlacklistedFiles(t *testing.T) {
	dir := t.TempDir()
	ws := New(dir)

	// Create the blacklisted file so Resolve reaches the blacklist check.
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("SECRET=x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ws.Resolve(".env")
	if err == nil {
		t.Fatal("expected error for blacklisted .env, got nil")
	}
}

func TestResolve_EmptyPathReturnsRoot(t *testing.T) {
	dir := t.TempDir()
	ws := New(dir)

	got, err := ws.Resolve("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != dir {
		t.Errorf("got %s, want %s", got, dir)
	}
}

func TestResolve_AllowsNewFileInExistingDir(t *testing.T) {
	dir := t.TempDir()
	ws := New(dir)

	// File doesn't exist yet, but parent dir does.
	got, err := ws.Resolve("new_file.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(dir, "new_file.txt")
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestEnsureRoot_CreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deep", "nested")
	ws := New(dir)

	if err := ws.EnsureRoot(); err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat after EnsureRoot: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected directory, got file")
	}
}
