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
// collapsed, the format characters (the bidirectional controls, zero-
// width spaces, soft hyphens, tags) removed except a zero-width joiner or
// non-joiner between two visible characters, invalid encoding dropped,
// and the rest cut at MaxTitleRunes. Cleaning a clean title changes
// nothing, and a title of nothing visible is "".
//
// @decision(DL-149)
func CleanTitle(s string) string {
	runes := []rune(strings.ToValidUTF8(s, ""))
	var b strings.Builder
	for i, r := range runes {
		switch {
		case unicode.IsControl(r):
			b.WriteByte(' ')
		case joiner(r):
			if i > 0 && i < len(runes)-1 && visible(runes[i-1]) && visible(runes[i+1]) {
				b.WriteRune(r)
			}
		case unicode.Is(unicode.Cf, r):
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if utf8.RuneCountInString(out) > MaxTitleRunes {
		out = string([]rune(out)[:MaxTitleRunes])
	}
	return strings.TrimRightFunc(out, func(r rune) bool { return unicode.IsSpace(r) || joiner(r) })
}

// joiner is the zero-width joiner or non-joiner, which shape the
// characters either side of them.
func joiner(r rune) bool { return r == 0x200d || r == 0x200c }

// visible is a character that is drawn: not a space, a control or a
// format character.
func visible(r rune) bool {
	return !unicode.IsSpace(r) && !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r)
}
