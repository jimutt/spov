package cli

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Printable is s made safe to print to a terminal: ESC and every other
// C0 or C1 control character, and the bidirectional overrides that can
// reorder a line, are shown as a visible escape such as `\x1b`, so
// text an agent or a sandbox wrote cannot recolour or erase the screen,
// set the window title or write the clipboard. Newline and tab stay, and
// CRLF reads as a newline. Text that holds none of these is returned
// as it is. Every site that prints such text outside --json goes
// through here (or through printableLine).
//
// @decision(DL-136)
func Printable(s string) string { return escapeControls(strings.ReplaceAll(s, "\r\n", "\n"), true) }

// printableLine is Printable for a one-line field (a path, a title):
// newline and tab are shown as escapes too.
//
// @decision(DL-136)
func printableLine(s string) string { return escapeControls(s, false) }

func escapeControls(s string, keepLayout bool) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f || s[i] >= 0x80 {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			// A byte that is not UTF-8 may be read as a C1 control by a
			// terminal in an 8-bit mode.
			fmt.Fprintf(&b, "\\x%02x", s[i])
		case keepLayout && (r == '\n' || r == '\t'):
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x80 && unicode.IsControl(r):
			fmt.Fprintf(&b, "\\x%02x", r)
		case unicode.IsControl(r), isFormatSpoof(r):
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// isFormatSpoof is a character that is not a control but reorders or
// splits what is drawn: the bidirectional embeddings, overrides and
// isolates, and the line and paragraph separators.
func isFormatSpoof(r rune) bool {
	switch {
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0x2028, r == 0x2029, r == 0x200e, r == 0x200f, r == 0x061c:
		return true
	}
	return false
}
