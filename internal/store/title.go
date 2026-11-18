package store

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxTitleRunes bounds a session's title and a job's, in characters.
//
// @decision(DL-149)
const MaxTitleRunes = 120

// CleanTitle is a title as it is stored and served: one line, with
// control characters and line breaks as spaces, runs of whitespace
// collapsed, the bidirectional controls removed, invalid encoding
// dropped, and the rest cut at MaxTitleRunes. Cleaning a clean title
// changes nothing.
//
// @decision(DL-149)
func CleanTitle(s string) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "") {
		switch {
		case bidiControl(r):
		case unicode.IsControl(r):
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if utf8.RuneCountInString(out) <= MaxTitleRunes {
		return out
	}
	runes := []rune(out)[:MaxTitleRunes]
	return strings.TrimRightFunc(string(runes), unicode.IsSpace)
}

// bidiControl is a character that reorders what is drawn: the
// embeddings, overrides and isolates and the directional marks.
func bidiControl(r rune) bool {
	switch {
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0x200e, r == 0x200f, r == 0x061c:
		return true
	}
	return false
}
