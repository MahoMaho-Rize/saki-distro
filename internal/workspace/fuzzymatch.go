package workspace

import (
	"strings"
	"unicode"
)

// FuzzyFind searches for oldText in content using 4-level matching
// (ported from OpenClaw's seekSequence algorithm):
//
//	Level 1: exact match
//	Level 2: trailing whitespace ignored
//	Level 3: all whitespace ignored (trimmed)
//	Level 4: punctuation normalized + whitespace ignored
//
// Returns the index of the match in content, or -1 if not found.
// If multiple matches at any level, returns -1 (ambiguous).
func FuzzyFind(content, oldText string) int {
	normalizers := []func(string) string{
		identity,
		trimTrailing,
		strings.TrimSpace,
		func(s string) string { return normalizePunctuation(strings.TrimSpace(s)) },
	}

	for _, norm := range normalizers {
		idx := findNormalized(content, oldText, norm)
		if idx >= 0 {
			return idx
		}
	}
	return -1
}

// FuzzyFindLevel returns the match index and which level matched (1-4).
// Returns (-1, 0) if no match.
func FuzzyFindLevel(content, oldText string) (int, int) {
	normalizers := []func(string) string{
		identity,
		trimTrailing,
		strings.TrimSpace,
		func(s string) string { return normalizePunctuation(strings.TrimSpace(s)) },
	}

	for level, norm := range normalizers {
		idx := findNormalized(content, oldText, norm)
		if idx >= 0 {
			return idx, level + 1
		}
	}
	return -1, 0
}

func identity(s string) string     { return s }
func trimTrailing(s string) string { return strings.TrimRight(s, " \t\r") }

// findNormalized applies norm to both content lines and pattern lines,
// then searches for the normalized pattern as a contiguous block.
// Returns the byte offset in the original content, or -1.
func findNormalized(content, pattern string, norm func(string) string) int {
	normPattern := norm(pattern)
	if normPattern == "" {
		return -1
	}

	// For level 1 (identity), use simple string search
	if norm("x y") == "x y" {
		idx := strings.Index(content, pattern)
		if idx >= 0 {
			// Check uniqueness
			if strings.Index(content[idx+1:], pattern) >= 0 {
				return -1 // ambiguous
			}
			return idx
		}
		return -1
	}

	// For levels 2-4: line-based matching
	contentLines := strings.Split(content, "\n")
	patternLines := strings.Split(pattern, "\n")

	if len(patternLines) == 0 {
		return -1
	}

	matches := 0
	matchIdx := -1

	for i := 0; i <= len(contentLines)-len(patternLines); i++ {
		found := true
		for j, pl := range patternLines {
			if norm(contentLines[i+j]) != norm(pl) {
				found = false
				break
			}
		}
		if found {
			matches++
			if matches > 1 {
				return -1 // ambiguous
			}
			// Calculate byte offset
			offset := 0
			for k := 0; k < i; k++ {
				offset += len(contentLines[k]) + 1 // +1 for \n
			}
			matchIdx = offset
		}
	}

	return matchIdx
}

// normalizePunctuation normalizes Unicode punctuation to ASCII equivalents.
// Covers: em/en dashes, smart quotes, non-breaking spaces, fullwidth chars.
func normalizePunctuation(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		// Dashes: U+2010–U+2015, U+2212 → ASCII hyphen
		case r >= '\u2010' && r <= '\u2015' || r == '\u2212':
			b.WriteByte('-')
		// Left double quotes: U+201C, U+201D → ASCII "
		case r == '\u201C' || r == '\u201D' || r == '\u201E' || r == '\u201F':
			b.WriteByte('"')
		// Left single quotes: U+2018, U+2019, U+201A, U+201B → ASCII '
		case r == '\u2018' || r == '\u2019' || r == '\u201A' || r == '\u201B':
			b.WriteByte('\'')
		// Non-breaking space, various Unicode spaces → ASCII space
		case r == '\u00A0' || r == '\u2000' || r == '\u2001' || r == '\u2002' ||
			r == '\u2003' || r == '\u2004' || r == '\u2005' || r == '\u2006' ||
			r == '\u2007' || r == '\u2008' || r == '\u2009' || r == '\u200A' ||
			r == '\u3000' || r == '\uFEFF':
			b.WriteByte(' ')
		// Fullwidth ASCII variants: U+FF01–U+FF5E → ASCII
		case r >= '\uFF01' && r <= '\uFF5E':
			b.WriteRune(rune(r - '\uFF01' + '!'))
		default:
			if unicode.IsPrint(r) || r == '\n' || r == '\t' {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
