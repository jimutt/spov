package cli

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
)

// tones are the colours the CLI draws in, each resolved once for the
// terminal's background: the launch view, the repository picker and
// `spov init` pick from the same set.
//
// @decision(DL-024) @decision(DL-152)
type tones struct {
	title color.Color
	// ok is done, warn is in progress or waiting on someone, bad is a
	// failure.
	ok, warn, bad color.Color
	// dim is secondary text; faint is text further back still.
	dim, faint color.Color
	// code is a command, a path or an address to copy.
	code color.Color
}

func newTones(dark bool) tones {
	pick := lipgloss.LightDark(dark)
	return tones{
		title: pick(lipgloss.Color("#2b2b2b"), lipgloss.Color("#e6e6e6")),
		ok:    pick(lipgloss.Color("#2f7d55"), lipgloss.Color("#7fd6a3")),
		warn:  pick(lipgloss.Color("#a8761f"), lipgloss.Color("#e0b341")),
		bad:   pick(lipgloss.Color("#b3412f"), lipgloss.Color("#f2857a")),
		dim:   pick(lipgloss.Color("#5c6773"), lipgloss.Color("#9aa7b4")),
		faint: pick(lipgloss.Color("#9aa7b4"), lipgloss.Color("#5c6773")),
		code:  pick(lipgloss.Color("#2b6a99"), lipgloss.Color("#8ec5ec")),
	}
}

// darkBackground guesses the terminal background from COLORFGBG ("fg;bg",
// where a bg of 15 or 7 is light) and defaults to dark.
//
// @decision(DL-024)
func darkBackground(getenv func(string) string) bool {
	if v := getenv("COLORFGBG"); v != "" {
		if i := strings.LastIndex(v, ";"); i >= 0 {
			switch v[i+1:] {
			case "15", "7":
				return false
			}
		}
	}
	return true
}
