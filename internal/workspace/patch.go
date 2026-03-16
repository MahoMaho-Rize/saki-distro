package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PatchOp represents a single file operation in a multi-file patch.
type PatchOp struct {
	Action string // "update", "add", "delete"
	Path   string
	Lines  []PatchLine
}

// PatchLine is a single line in a patch with its prefix (+/-/context).
type PatchLine struct {
	Prefix byte // '+', '-', or ' '
	Text   string
}

// ParsePatch parses the OpenClaw-style multi-file patch format:
//
//	*** Begin Patch
//	*** Update File: path
//	@@ optional context hint
//	-old line
//	+new line
//	 context line
//	*** Add File: path
//	+new content
//	*** Delete File: path
//	*** End Patch
func ParsePatch(input string) ([]PatchOp, error) {
	lines := strings.Split(input, "\n")

	// Find *** Begin Patch
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "*** Begin Patch" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("patch: missing '*** Begin Patch' marker")
	}

	var ops []PatchOp
	var current *PatchOp

	for i := start; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if trimmed == "*** End Patch" {
			if current != nil {
				ops = append(ops, *current)
			}
			return ops, nil
		}

		if strings.HasPrefix(trimmed, "*** Update File: ") {
			if current != nil {
				ops = append(ops, *current)
			}
			current = &PatchOp{Action: "update", Path: strings.TrimPrefix(trimmed, "*** Update File: ")}
			continue
		}
		if strings.HasPrefix(trimmed, "*** Add File: ") {
			if current != nil {
				ops = append(ops, *current)
			}
			current = &PatchOp{Action: "add", Path: strings.TrimPrefix(trimmed, "*** Add File: ")}
			continue
		}
		if strings.HasPrefix(trimmed, "*** Delete File: ") {
			if current != nil {
				ops = append(ops, *current)
			}
			current = &PatchOp{Action: "delete", Path: strings.TrimPrefix(trimmed, "*** Delete File: ")}
			continue
		}

		// @@ context hints — skip
		if strings.HasPrefix(trimmed, "@@") {
			continue
		}

		// Patch content lines
		if current == nil {
			continue
		}
		if len(line) == 0 {
			current.Lines = append(current.Lines, PatchLine{Prefix: ' ', Text: ""})
			continue
		}
		prefix := line[0]
		text := ""
		if len(line) > 1 {
			text = line[1:]
		}
		switch prefix {
		case '+', '-', ' ':
			current.Lines = append(current.Lines, PatchLine{Prefix: prefix, Text: text})
		default:
			// Treat as context line
			current.Lines = append(current.Lines, PatchLine{Prefix: ' ', Text: line})
		}
	}

	if current != nil {
		ops = append(ops, *current)
	}
	return ops, fmt.Errorf("patch: missing '*** End Patch' marker")
}

// ApplyPatch applies a parsed patch to the workspace.
// For each op: update uses fuzzy line matching, add creates the file,
// delete removes it. All writes are atomic.
func (w *W) ApplyPatch(ops []PatchOp) ([]string, error) {
	var applied []string

	for _, op := range ops {
		switch op.Action {
		case "delete":
			abs, err := w.Resolve(op.Path)
			if err != nil {
				return applied, fmt.Errorf("patch delete %s: %w", op.Path, err)
			}
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return applied, fmt.Errorf("patch delete %s: %w", op.Path, err)
			}
			applied = append(applied, fmt.Sprintf("deleted %s", op.Path))

		case "add":
			abs, err := w.Resolve(op.Path)
			if err != nil {
				return applied, fmt.Errorf("patch add %s: %w", op.Path, err)
			}
			var content strings.Builder
			for _, l := range op.Lines {
				if l.Prefix == '+' {
					content.WriteString(l.Text)
					content.WriteByte('\n')
				}
			}
			if err := AtomicWrite(abs, []byte(content.String()), 0o600); err != nil {
				return applied, fmt.Errorf("patch add %s: %w", op.Path, err)
			}
			applied = append(applied, fmt.Sprintf("added %s", op.Path))

		case "update":
			readAbs, err := w.ResolveRead(op.Path)
			if err != nil {
				return applied, fmt.Errorf("patch update %s: %w", op.Path, err)
			}
			data, err := os.ReadFile(readAbs)
			if err != nil {
				return applied, fmt.Errorf("patch update %s: read: %w", op.Path, err)
			}

			result, err := applyHunks(string(data), op.Lines)
			if err != nil {
				return applied, fmt.Errorf("patch update %s: %w", op.Path, err)
			}

			writeAbs, werr := w.Resolve(op.Path)
			if werr != nil {
				return applied, fmt.Errorf("patch update %s: %w", op.Path, werr)
			}
			if err := AtomicWrite(writeAbs, []byte(result), 0o600); err != nil {
				return applied, fmt.Errorf("patch update %s: write: %w", op.Path, err)
			}
			applied = append(applied, fmt.Sprintf("updated %s", op.Path))
		}
	}

	return applied, nil
}

// applyHunks applies diff lines to content using line-based matching.
// Context lines and '-' lines are matched against the file; '+' lines
// are inserted. Uses reverse application to avoid index shifting.
func applyHunks(content string, lines []PatchLine) (string, error) {
	fileLines := strings.Split(content, "\n")

	// Collect context/remove/add sequences
	type replacement struct {
		start    int // line index in file
		oldLen   int // lines to remove
		newLines []string
	}

	var replacements []replacement
	pos := 0 // current position in fileLines

	i := 0
	for i < len(lines) {
		l := lines[i]
		switch l.Prefix {
		case ' ':
			// Context: advance position using fuzzy match
			found := false
			for j := pos; j < len(fileLines); j++ {
				if FuzzyFind(fileLines[j], l.Text) >= 0 || strings.TrimSpace(fileLines[j]) == strings.TrimSpace(l.Text) {
					pos = j + 1
					found = true
					break
				}
			}
			if !found {
				pos++ // best effort
			}
			i++

		case '-':
			// Collect contiguous -/+ block
			removeStart := pos
			var removes, adds []string
			for i < len(lines) && lines[i].Prefix == '-' {
				removes = append(removes, lines[i].Text)
				i++
			}
			for i < len(lines) && lines[i].Prefix == '+' {
				adds = append(adds, lines[i].Text)
				i++
			}
			replacements = append(replacements, replacement{
				start:    removeStart,
				oldLen:   len(removes),
				newLines: adds,
			})
			pos = removeStart + len(removes)

		case '+':
			// Standalone add (no preceding -)
			var adds []string
			for i < len(lines) && lines[i].Prefix == '+' {
				adds = append(adds, lines[i].Text)
				i++
			}
			replacements = append(replacements, replacement{
				start:    pos,
				oldLen:   0,
				newLines: adds,
			})
		}
	}

	// Apply replacements in reverse order to avoid index shifting
	for j := len(replacements) - 1; j >= 0; j-- {
		r := replacements[j]
		end := r.start + r.oldLen
		if end > len(fileLines) {
			end = len(fileLines)
		}
		tail := make([]string, len(fileLines[end:]))
		copy(tail, fileLines[end:])
		fileLines = append(append(fileLines[:r.start], r.newLines...), tail...)
	}

	result := strings.Join(fileLines, "\n")
	// Preserve trailing newline if original had one
	if strings.HasSuffix(content, "\n") && !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return result, nil
}

// EnsureDir creates the directory for the given file path if it doesn't exist.
func EnsureDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o755)
}
