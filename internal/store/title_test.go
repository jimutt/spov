package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A title is one line of at most MaxTitleRunes characters: controls and
// line breaks become spaces, whitespace collapses, the bidirectional
// controls go, and the rest is cut at the bound.
//
// @decision(DL-149)
func TestCleanTitle(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"plain":            {"Fix the flaky login test", "Fix the flaky login test"},
		"trimmed":          {"  padded \t", "padded"},
		"line breaks":      {"first\nsecond\r\nthird", "first second third"},
		"controls":         {"a\x1b[31mred\x07b\x00c\u0085d", "a [31mred b c d"},
		"separators":       {"one\u2028two\u2029three", "one two three"},
		"bidi":             {"admin\u202e\u2066txt.exe\u2069\u200f\u061c", "admintxt.exe"},
		"invalid encoding": {"ok\xffay", "okay"},
		"only noise":       {"\n\t\u202e ", ""},
		"empty":            {"", ""},
		"runes not bytes":  {strings.Repeat("é", 130), strings.Repeat("é", MaxTitleRunes)},
	}
	for name, c := range cases {
		if got := CleanTitle(c.in); got != c.want {
			t.Errorf("%s: CleanTitle(%q) = %q, want %q", name, c.in, got, c.want)
		}
	}
	long := CleanTitle(strings.Repeat("word ", 100))
	if n := utf8.RuneCountInString(long); n > MaxTitleRunes || strings.HasSuffix(long, " ") {
		t.Errorf("a long title is %d characters and ends %q", n, long[len(long)-3:])
	}
	if again := CleanTitle(long); again != long {
		t.Errorf("cleaning twice changed the title: %q", again)
	}
}
