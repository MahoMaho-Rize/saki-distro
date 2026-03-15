package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// --- Shadow Layer tests ---

func TestShadow_ReadFallsThrough(t *testing.T) {
	lower := t.TempDir()
	upper := t.TempDir()

	// File only in lower.
	os.WriteFile(filepath.Join(lower, "host.txt"), []byte("from host"), 0o600)

	ws := NewShadow(upper, lower)

	got, err := ws.ResolveRead("host.txt")
	if err != nil {
		t.Fatalf("ResolveRead: %v", err)
	}
	data, _ := os.ReadFile(got)
	if string(data) != "from host" {
		t.Errorf("expected 'from host', got %q", string(data))
	}
}

func TestShadow_UpperTakesPriority(t *testing.T) {
	lower := t.TempDir()
	upper := t.TempDir()

	os.WriteFile(filepath.Join(lower, "file.txt"), []byte("lower"), 0o600)
	os.WriteFile(filepath.Join(upper, "file.txt"), []byte("upper"), 0o600)

	ws := NewShadow(upper, lower)

	got, err := ws.ResolveRead("file.txt")
	if err != nil {
		t.Fatalf("ResolveRead: %v", err)
	}
	data, _ := os.ReadFile(got)
	if string(data) != "upper" {
		t.Errorf("expected 'upper', got %q", string(data))
	}
}

func TestShadow_WriteGoesToUpper(t *testing.T) {
	lower := t.TempDir()
	upper := t.TempDir()

	ws := NewShadow(upper, lower)

	got, err := ws.Resolve("new.txt")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Should be in upper, not lower.
	if got != filepath.Join(upper, "new.txt") {
		t.Errorf("expected path in upper, got %s", got)
	}
}

func TestShadow_ListMerged(t *testing.T) {
	lower := t.TempDir()
	upper := t.TempDir()

	os.WriteFile(filepath.Join(lower, "host.txt"), []byte("h"), 0o600)
	os.WriteFile(filepath.Join(upper, "agent.txt"), []byte("a"), 0o600)
	// Shared name — upper wins.
	os.WriteFile(filepath.Join(lower, "shared.txt"), []byte("lower"), 0o600)
	os.WriteFile(filepath.Join(upper, "shared.txt"), []byte("upper"), 0o600)

	ws := NewShadow(upper, lower)

	entries, err := ws.ListMerged("")
	if err != nil {
		t.Fatalf("ListMerged: %v", err)
	}

	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}

	if !names["host.txt"] {
		t.Error("missing host.txt from lower")
	}
	if !names["agent.txt"] {
		t.Error("missing agent.txt from upper")
	}
	if !names["shared.txt"] {
		t.Error("missing shared.txt")
	}
	// Should not have duplicate shared.txt.
	count := 0
	for _, e := range entries {
		if e.Name() == "shared.txt" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("shared.txt appears %d times, expected 1", count)
	}
}

func TestShadow_DiffFiles(t *testing.T) {
	lower := t.TempDir()
	upper := t.TempDir()

	os.WriteFile(filepath.Join(lower, "untouched.txt"), []byte(""), 0o600)
	os.WriteFile(filepath.Join(upper, "changed.txt"), []byte(""), 0o600)
	os.MkdirAll(filepath.Join(upper, "sub"), 0o755)
	os.WriteFile(filepath.Join(upper, "sub", "new.txt"), []byte(""), 0o600)

	ws := NewShadow(upper, lower)

	diff, err := ws.DiffFiles()
	if err != nil {
		t.Fatalf("DiffFiles: %v", err)
	}

	diffMap := make(map[string]bool)
	for _, f := range diff {
		diffMap[f] = true
	}

	if !diffMap["changed.txt"] {
		t.Error("missing changed.txt in diff")
	}
	if !diffMap[filepath.Join("sub", "new.txt")] {
		t.Error("missing sub/new.txt in diff")
	}
	if diffMap["untouched.txt"] {
		t.Error("untouched.txt should NOT be in diff (it's in lower only)")
	}
}

func TestShadow_WalkMergedDeduplicates(t *testing.T) {
	lower := t.TempDir()
	upper := t.TempDir()

	os.WriteFile(filepath.Join(lower, "a.go"), []byte("package a"), 0o600)
	os.WriteFile(filepath.Join(upper, "a.go"), []byte("package a // modified"), 0o600)
	os.WriteFile(filepath.Join(lower, "b.go"), []byte("package b"), 0o600)

	ws := NewShadow(upper, lower)

	var files []string
	ws.WalkMerged("", func(rel string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, rel)
		}
		return nil
	})

	// Should have a.go (once, from upper) and b.go (from lower).
	count := 0
	for _, f := range files {
		if f == "a.go" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("a.go appears %d times, expected 1", count)
	}

	hasB := false
	for _, f := range files {
		if f == "b.go" {
			hasB = true
		}
	}
	if !hasB {
		t.Error("missing b.go from lower layer")
	}
}

func TestShadow_BlacklistStillWorks(t *testing.T) {
	lower := t.TempDir()
	upper := t.TempDir()

	os.WriteFile(filepath.Join(lower, ".env"), []byte("SECRET"), 0o600)

	ws := NewShadow(upper, lower)

	_, err := ws.ResolveRead(".env")
	if err == nil {
		t.Error("expected error for blacklisted .env in shadow mode")
	}
}

func TestShadow_PathEscapeBlocked(t *testing.T) {
	lower := t.TempDir()
	upper := t.TempDir()

	ws := NewShadow(upper, lower)

	_, err := ws.ResolveRead("/etc/passwd")
	if err == nil {
		t.Error("expected error for path escape")
	}
}

func TestShadow_IsShadow(t *testing.T) {
	single := New("/tmp/test")
	if single.IsShadow() {
		t.Error("single-layer should not be shadow")
	}

	shadow := NewShadow("/tmp/upper", "/tmp/lower")
	if !shadow.IsShadow() {
		t.Error("shadow workspace should report IsShadow=true")
	}
}
