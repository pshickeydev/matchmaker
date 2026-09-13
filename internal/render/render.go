// Package render implements the single terminal renderer that every
// agent-controlled string destined for an interactive terminal must pass
// through (DESIGN §5.5): CLI, TUI, report previews, errors, and
// diagnostic views. No terminal output path may bypass it.
package render

import (
	"strconv"
	"strings"
)

const (
	// esc is the escape introducer of every terminal escape sequence.
	esc byte = 0x1b
	// bel terminates OSC sequences as an alternative to ST.
	bel byte = 0x07
	// stMark is the second byte of ST (ESC \), the string terminator.
	stMark byte = '\\'
)

// isDroppedControl reports whether r is a control rune the renderer
// removes: C0 except permitted layout characters, DEL, and C1. Permitted
// layout characters are tab, line feed, and carriage return.
func isDroppedControl(r rune) bool {
	switch r {
	case '\t', '\n', '\r':
		return false
	}
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// Render sanitizes one agent-controlled string for terminal display: it
// strips C0/C1 control characters except permitted layout characters,
// removes ANSI/OSC and other terminal escape sequences, replaces invalid
// UTF-8, and preserves no agent-supplied styling.
func Render(s string) string {
	s = stripEscapes(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isDroppedControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// NormalizeField prepares an agent-controlled value for structured
// logging (DESIGN §5.5): it is emitted as a quoted, length-bounded field
// after control/escape normalization, never interpolated into message
// strings.
func NormalizeField(s string, maxLen int) string {
	normalized := Render(s)
	if maxLen > 0 {
		normalized = truncateRunes(normalized, maxLen)
	}
	return strconv.Quote(normalized)
}

// truncateRunes cuts s to at most maxLen runes.
func truncateRunes(s string, maxLen int) string {
	count := 0
	for i := range s {
		if count == maxLen {
			return s[:i]
		}
		count++
	}
	return s
}

// stripEscapes removes ANSI/OSC and other terminal escape sequences
// (ECMA-48): CSI and OSC sequences, DCS/SOS/PM/APC strings, and nF/F
// escape sequences.
func stripEscapes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != esc {
			b.WriteByte(s[i])
			i++
			continue
		}
		i = skipEscape(s, i)
	}
	return b.String()
}

// skipEscape returns the index just past the escape sequence starting at
// i, which must hold esc. Truncated sequences at end of input are
// consumed entirely.
func skipEscape(s string, i int) int {
	j := i + 1
	if j >= len(s) {
		return j
	}
	switch s[j] {
	case '[': // CSI: parameter bytes 0x30-0x3F and intermediates 0x20-0x2F, then a 0x40-0x7E final byte. An embedded escape aborts the sequence.
		j++
		for j < len(s) && ((s[j] >= 0x30 && s[j] <= 0x3f) || (s[j] >= 0x20 && s[j] <= 0x2f)) {
			j++
		}
		if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7e {
			j++
		}
		return j
	case ']', 'P', 'X', '^', '_': // string sequences: OSC, DCS, SOS, PM, APC.
		j++
		for j < len(s) {
			if s[j] == bel {
				return j + 1
			}
			if s[j] == esc && j+1 < len(s) && s[j+1] == stMark {
				return j + 2
			}
			j++
		}
		return j
	default: // nF (0x20-0x2F intermediates then a 0x30-0x7E final) or F (final only).
		for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
			j++
		}
		if j < len(s) && s[j] >= 0x30 && s[j] <= 0x7e {
			j++
		}
		return j
	}
}
