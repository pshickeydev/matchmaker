// Package render implements the single terminal renderer that every
// agent-controlled string destined for an interactive terminal must pass
// through (DESIGN §5.5): CLI, TUI, report previews, errors, and
// diagnostic views. No terminal output path may bypass it.
package render

const notImplemented = "not implemented"

// Render sanitizes one agent-controlled string for terminal display: it
// strips C0/C1 control characters except permitted layout characters,
// removes ANSI/OSC and other terminal escape sequences, replaces invalid
// UTF-8, and preserves no agent-supplied styling.
func Render(s string) string { panic(notImplemented) }

// NormalizeField prepares an agent-controlled value for structured
// logging (DESIGN §5.5): it is emitted as a quoted, length-bounded field
// after control/escape normalization, never interpolated into message
// strings.
func NormalizeField(s string, maxLen int) string { panic(notImplemented) }
