package safenet

import (
	"regexp"
	"strings"
)

// credentialPatterns matches common credential formats in tool output.
// Used to redact secrets before sending to clients.
var credentialPatterns = []*regexp.Regexp{
	// API keys with common prefixes
	regexp.MustCompile(`\b(sk-[a-zA-Z0-9]{20,})`),           // OpenAI/Anthropic
	regexp.MustCompile(`\b(ghp_[a-zA-Z0-9]{36,})`),          // GitHub PAT
	regexp.MustCompile(`\b(github_pat_[a-zA-Z0-9_]{20,})`),  // GitHub fine-grained
	regexp.MustCompile(`\b(gsk_[a-zA-Z0-9]{20,})`),          // Groq
	regexp.MustCompile(`\b(xox[baprs]-[a-zA-Z0-9\-]{10,})`), // Slack
	regexp.MustCompile(`\b(xapp-[a-zA-Z0-9\-]{10,})`),       // Slack app
	regexp.MustCompile(`\b(AIza[a-zA-Z0-9_\-]{30,})`),       // Google
	regexp.MustCompile(`\b(pplx-[a-zA-Z0-9]{20,})`),         // Perplexity
	regexp.MustCompile(`\b(npm_[a-zA-Z0-9]{20,})`),          // npm
	regexp.MustCompile(`\b(cr_[a-zA-Z0-9]{20,})`),           // custom relay

	// Generic patterns
	regexp.MustCompile(`(?i)(api[_-]?key|secret|password|token|auth)\s*[:=]\s*["']?([a-zA-Z0-9_\-./+]{16,})["']?`),
	regexp.MustCompile(`(?i)Authorization:\s*Bearer\s+([a-zA-Z0-9_\-./+]{16,})`),

	// PEM private keys
	regexp.MustCompile(`(?s)(-----BEGIN\s+(RSA\s+)?PRIVATE\s+KEY-----.*?-----END\s+(RSA\s+)?PRIVATE\s+KEY-----)`),

	// AWS
	regexp.MustCompile(`\b(AKIA[0-9A-Z]{16})`), // AWS access key
}

// RedactSecrets replaces detected credentials in text with redacted versions.
// Preserves first 6 and last 4 characters for debugging.
func RedactSecrets(text string) string {
	for _, pat := range credentialPatterns {
		text = pat.ReplaceAllStringFunc(text, func(match string) string {
			if len(match) < 12 {
				return "***REDACTED***"
			}
			return match[:6] + "***REDACTED***" + match[len(match)-4:]
		})
	}
	return text
}

// --- Command obfuscation detection ---

// obfuscationPatterns detect encoded/obfuscated command execution.
var obfuscationPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)base64\s+(-d|--decode)\s*\|.*\b(ba)?sh\b`),        // base64 | sh
	regexp.MustCompile(`(?i)\b(curl|wget)\s+.*\|\s*(ba)?sh\b`),                // curl | sh
	regexp.MustCompile(`(?i)\beval\s*\$\(`),                                   // eval $()
	regexp.MustCompile(`(?i)\bpython[23]?\s+-c\s+.*(?:exec|eval|__import__)`), // python -c exec
	regexp.MustCompile(`(?i)\bperl\s+-e\s+.*(?:system|exec|reverse)`),         // perl -e system
	regexp.MustCompile(`(?i)\bruby\s+-e\s+.*(?:system|exec|%x)`),              // ruby -e system
	regexp.MustCompile(`(?i)\\x[0-9a-f]{2}.*\\x[0-9a-f]{2}.*\\x[0-9a-f]{2}`),  // hex escapes
	regexp.MustCompile(`(?i)\$'\\'[0-9]{3}.*\$'\\'[0-9]{3}`),                  // bash octal
	regexp.MustCompile(`(?i)\bprintf\s+.*\\\\x.*\|\s*(ba)?sh\b`),              // printf hex | sh
}

// DetectObfuscation checks a command string for obfuscation patterns.
// Returns the matched pattern description, or "" if clean.
func DetectObfuscation(cmd string) string {
	descriptions := []string{
		"base64 decode piped to shell",
		"curl/wget piped to shell",
		"eval with command substitution",
		"python exec/eval injection",
		"perl system/exec injection",
		"ruby system/exec injection",
		"hex escape sequences",
		"bash octal escapes",
		"printf hex piped to shell",
	}
	for i, pat := range obfuscationPatterns {
		if pat.MatchString(cmd) {
			return descriptions[i]
		}
	}
	// Length-based heuristic
	if len(cmd) > 10000 {
		return "suspiciously long command (>10K chars)"
	}
	return ""
}

// --- Unicode homoglyph normalization ---

// NormalizeHomoglyphs strips zero-width characters and normalizes
// common Unicode homoglyphs that can be used to disguise prompt injection.
func NormalizeHomoglyphs(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		// Strip zero-width characters
		if isZeroWidth(r) {
			continue
		}
		// Normalize confusables
		if norm, ok := homoglyphMap[r]; ok {
			b.WriteRune(norm)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isZeroWidth(r rune) bool {
	switch r {
	case '\u200B', // zero-width space
		'\u200C', // zero-width non-joiner
		'\u200D', // zero-width joiner
		'\u200E', // LTR mark
		'\u200F', // RTL mark
		'\u2060', // word joiner
		'\u2061', // function application
		'\u2062', // invisible times
		'\u2063', // invisible separator
		'\u2064', // invisible plus
		'\uFEFF': // BOM / zero-width no-break space
		return true
	}
	return false
}

// homoglyphMap maps confusable Unicode characters to their ASCII lookalikes.
var homoglyphMap = map[rune]rune{
	// Cyrillic → Latin
	'\u0410': 'A', '\u0412': 'B', '\u0421': 'C', '\u0415': 'E',
	'\u041D': 'H', '\u041A': 'K', '\u041C': 'M', '\u041E': 'O',
	'\u0420': 'P', '\u0422': 'T', '\u0425': 'X',
	'\u0430': 'a', '\u0435': 'e', '\u043E': 'o', '\u0440': 'p',
	'\u0441': 'c', '\u0443': 'y', '\u0445': 'x',
	// Fullwidth → ASCII
	'\uFF21': 'A', '\uFF22': 'B', '\uFF23': 'C', '\uFF24': 'D',
	'\uFF25': 'E', '\uFF26': 'F', '\uFF41': 'a', '\uFF42': 'b',
	'\uFF43': 'c', '\uFF44': 'd', '\uFF45': 'e', '\uFF46': 'f',
	// Math/special
	'\u2013': '-',                  // en dash
	'\u2014': '-',                  // em dash
	'\u2018': '\'', '\u2019': '\'', // smart single quotes
	'\u201C': '"', '\u201D': '"', // smart double quotes
}
