package cli

import (
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"golang.org/x/term"
)

// initWidth is the width `spov init` wraps its prose at, or the
// terminal's when it is narrower, but never below initMinWidth.
const (
	initWidth    = 80
	initMinWidth = 40
)

// The indents of init's lines: a step's facts and conversation sit under
// its title, a line to copy two columns further in.
const (
	indentStep = 5
	indentCode = 7
)

// role is what a run of text is, which decides its style.
type role int

const (
	roleText role = iota
	roleTitle
	roleDim
	roleCode
	roleOKMark
	roleWarnMark
	roleBadMark
	roleFaintMark
	roleReady
	roleNotReady
	roleErrLabel
	roleErrText
	rolePromptMark
	rolePromptText
	roleChoiceNumber
	roleCount
)

// seg is a run of text in one role.
type seg struct {
	text string
	role role
}

// initStyle draws `spov init`'s lines. With on false every style is
// dropped and the text is the same, line for line.
//
// @decision(DL-152)
type initStyle struct {
	on      bool
	profile colorprofile.Profile
	width   int
	styles  [roleCount]lipgloss.Style
}

// initStyleFor is the style for init's stdout: styled only on a terminal,
// with NO_COLOR unset and TERM not dumb, in the colours the terminal can
// show.
//
// @decision(DL-152)
func (e *Env) initStyleFor() initStyle {
	on := e.stdoutIsTerminal() && e.Environ.Getenv("NO_COLOR") == "" && e.Environ.Getenv("TERM") != "dumb"
	return newInitStyle(on, colorprofile.Env(e.Environ.Environ()), darkBackground(e.Environ.Getenv), e.initWidthFor())
}

// initPromptStyle is the style for init's questions, which go to stderr:
// the stdout style while stderr is a terminal too, plain otherwise.
func (e *Env) initPromptStyle(st initStyle) initStyle {
	if st.on && !e.stderrIsTerminal() {
		return newInitStyle(false, colorprofile.NoTTY, true, st.width)
	}
	return st
}

// initWidthFor is initWidth, or a narrower terminal's width.
func (e *Env) initWidthFor() int {
	f, ok := e.Out.(interface{ Fd() uintptr })
	if !ok {
		return initWidth
	}
	w, _, err := term.GetSize(int(f.Fd()))
	if err != nil || w <= 0 || w >= initWidth {
		return initWidth
	}
	return max(w, initMinWidth)
}

func newInitStyle(on bool, profile colorprofile.Profile, dark bool, width int) initStyle {
	t := newTones(dark)
	plain := lipgloss.NewStyle()
	bold := plain.Bold(true)
	st := initStyle{on: on && profile > colorprofile.NoTTY, profile: profile, width: width}
	st.styles = [roleCount]lipgloss.Style{
		roleText:         plain,
		roleTitle:        bold,
		roleDim:          plain.Foreground(t.dim),
		roleCode:         plain.Foreground(t.code),
		roleOKMark:       bold.Foreground(t.ok),
		roleWarnMark:     bold.Foreground(t.warn),
		roleBadMark:      bold.Foreground(t.bad),
		roleFaintMark:    bold.Foreground(t.faint),
		roleReady:        bold.Foreground(t.ok),
		roleNotReady:     bold.Foreground(t.bad),
		roleErrLabel:     bold.Foreground(t.bad),
		roleErrText:      plain.Foreground(t.bad),
		rolePromptMark:   bold.Foreground(t.warn),
		rolePromptText:   bold,
		roleChoiceNumber: plain.Foreground(t.warn),
	}
	return st
}

// render joins the runs into one line, styled or plain. Neighbouring runs
// in one role are drawn as one.
func (s initStyle) render(segs ...seg) string {
	if !s.on {
		var b strings.Builder
		for _, g := range segs {
			b.WriteString(g.text)
		}
		return b.String()
	}
	var b strings.Builder
	for i := 0; i < len(segs); {
		j, text := i+1, segs[i].text
		for ; j < len(segs) && segs[j].role == segs[i].role; j++ {
			text += segs[j].text
		}
		if segs[i].role == roleText {
			b.WriteString(text)
		} else {
			b.WriteString(s.styles[segs[i].role].Render(text))
		}
		i = j
	}
	if s.profile == colorprofile.TrueColor {
		return b.String()
	}
	var out strings.Builder
	w := &colorprofile.Writer{Forward: &out, Profile: s.profile}
	_, _ = w.WriteString(b.String())
	return out.String()
}

// word is what wrapping never breaks: a run of text between spaces, with
// a backtick span kept whole.
type word []seg

func (w word) width() int {
	n := 0
	for _, g := range w {
		n += lipgloss.Width(g.text)
	}
	return n
}

// The words of prose drawn as code: a path, an address or an environment
// variable's name.
var (
	codePath   = regexp.MustCompile(`^(~|\.\.?)?/[^/\s]\S*$|^[\w.-]+(/[\w.@-]+)+$`)
	codeURL    = regexp.MustCompile(`^https?://\S+$`)
	codeEnvVar = regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)+$`)
)

// words splits prose at its spaces into words in base, with backtick
// spans, paths, addresses and variable names in code.
func (s initStyle) words(text string, base role) []word {
	var out []word
	for _, raw := range splitOutsideBackticks(text) {
		var w word
		for len(raw) > 0 {
			open := strings.IndexByte(raw, '`')
			closing := -1
			if open >= 0 {
				closing = strings.IndexByte(raw[open+1:], '`')
			}
			if open < 0 || closing < 0 {
				w = append(w, s.token(raw, base)...)
				break
			}
			end := open + 1 + closing + 1
			if open > 0 {
				w = append(w, s.token(raw[:open], base)...)
			}
			w = append(w, seg{raw[open:end], roleCode})
			raw = raw[end:]
		}
		out = append(out, w)
	}
	return out
}

// token is one piece of a word outside backticks, with its code core
// split from the punctuation around it.
func (s initStyle) token(t string, base role) []seg {
	core := strings.TrimLeft(t, `("'`)
	lead := t[:len(t)-len(core)]
	core = strings.TrimRight(core, `.,;:)"'`)
	trail := t[len(lead)+len(core):]
	if core == "" || !isCodeWord(core) {
		return []seg{{t, base}}
	}
	var out []seg
	if lead != "" {
		out = append(out, seg{lead, base})
	}
	out = append(out, seg{core, roleCode})
	if trail != "" {
		out = append(out, seg{trail, base})
	}
	return out
}

func isCodeWord(w string) bool {
	return codePath.MatchString(w) || codeURL.MatchString(w) || codeEnvVar.MatchString(w)
}

// splitOutsideBackticks splits at runs of whitespace that are not inside
// a closed backtick span.
func splitOutsideBackticks(text string) []string {
	var out []string
	var cur strings.Builder
	inside := false
	for i, r := range text {
		switch {
		case r == '`' && !inside && strings.IndexByte(text[i+1:], '`') >= 0:
			inside = true
			cur.WriteRune(r)
		case r == '`' && inside:
			inside = false
			cur.WriteRune(r)
		case !inside && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// wrap lays words out from column indent, and every further line from
// column hang, within the style's width. A word wider than the line is
// left whole.
func (s initStyle) wrap(indent, hang int, words []word) []string {
	var lines []string
	line := []seg{{strings.Repeat(" ", indent), roleText}}
	col, empty := indent, true
	for _, w := range words {
		n := w.width()
		if !empty && col+1+n > s.width {
			lines = append(lines, s.render(line...))
			line = []seg{{strings.Repeat(" ", hang), roleText}}
			col, empty = hang, true
		}
		if !empty {
			// A space between two runs of one role joins them.
			gap := roleText
			if last := line[len(line)-1].role; len(w) > 0 && w[0].role == last {
				gap = last
			}
			line = append(line, seg{" ", gap})
			col++
		}
		line = append(line, w...)
		col += n
		empty = false
	}
	return append(lines, s.render(line...))
}

// prose is text at indent, wrapped under itself.
func (s initStyle) prose(indent int, base role, text string) []string {
	return s.wrap(indent, indent, s.words(text, base))
}

// labelled is a label and its text at indent, the text wrapped under
// itself, past the label.
func (s initStyle) labelled(indent int, label string, labelRole, base role, text string) []string {
	words := append([]word{{{label, labelRole}}}, s.words(text, base)...)
	return s.wrap(indent, indent+lipgloss.Width(label)+1, words)
}

// codeLine is a line to copy, at indent and never wrapped.
func (s initStyle) codeLine(indent int, line string) string {
	return s.render(seg{strings.Repeat(" ", indent), roleText}, seg{line, roleCode})
}

// commandWords are the programs a fix line may start with.
var commandWords = map[string]bool{"spov": true, "uvx": true, "pipx": true, "modal": true, "gh": true, "curl": true}

// fixWords is a step's fix line: each alternative that is a command whole
// and in code, the others as prose.
func (s initStyle) fixWords(fix string, base role) []word {
	var out []word
	for i, part := range strings.Split(fix, ", or ") {
		if i > 0 {
			out[len(out)-1] = append(out[len(out)-1], seg{",", roleDim})
			out = append(out, word{{"or", roleDim}})
		}
		first, _, _ := strings.Cut(part, " ")
		if commandWords[first] {
			out = append(out, word{{part, roleCode}})
			continue
		}
		out = append(out, s.words(part, base)...)
	}
	return out
}

// question is a prompt's text at indent: a question mark, the question,
// and what follows it.
func (s initStyle) question(indent int, q, tail string) string {
	segs := []seg{{strings.Repeat(" ", indent), roleText}, {"?", rolePromptMark}, {" ", roleText}}
	for i, w := range s.words(q, rolePromptText) {
		if i > 0 {
			segs = append(segs, seg{" ", roleText})
		}
		segs = append(segs, w...)
	}
	if tail != "" {
		segs = append(segs, seg{" ", roleText}, seg{tail, roleDim})
	}
	return s.render(segs...)
}
