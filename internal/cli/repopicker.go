package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/sahilm/fuzzy"
)

// pickerChromeLines is the space the picker needs around the rows
// themselves: title, query line, two blank lines, and help.
const pickerChromeLines = 6

// maxPickerRows caps the visible rows so a long list still fits a
// normal terminal.
const maxPickerRows = 15

// maxScoreGap drops matches scoring far below the best one for the same
// query. Subsequence matching alone is far too permissive on a long
// list: "dot" matches "alice/java-demos" by picking one letter from
// each of three words. Those score far below a real hit, but the scores
// themselves are not comparable between queries — a legitimate match can
// be negative — so the cut is relative to the best match rather than an
// absolute floor.
//
// @decision(DL-028)
const maxScoreGap = 12

// pickerRow is one repository as the picker holds it: the text a query
// matches against, and the parts needed to render it.
//
// @decision(DL-028)
type pickerRow struct {
	repo RepoChoice
	// haystack is what a query matches: the slug and the platform name,
	// joined by a single space. Padding is applied when rendering, not
	// here: the scorer penalises the distance between matched characters,
	// so padding inside the haystack would make a wide column outrank a
	// better match.
	haystack string
	// slugPad is the spaces inserted after the slug when rendering, so a
	// matched byte offset past the slug can be shifted onto the drawn row.
	slugPad int
	// slugLen is the byte length of the slug, which is where the padding
	// is inserted.
	slugLen int
	// platformPad is the width the platform column is padded to, so the
	// detail beside it lines up.
	platformPad int
	// detail is the trailing text that helps pick but is not searched.
	detail string
}

// repoPicker is the repository picker. It owns its own model rather than
// using huh's MultiSelect, whose filter is a hardcoded substring match.
//
// @decision(DL-028)
type repoPicker struct {
	rows    []pickerRow
	query   string
	cursor  int
	visible []int
	// selected holds the index into rows of every chosen repository.
	// Selection is by identity, never by position in the filtered view,
	// so filtering and reordering cannot change what is selected.
	//
	// @decision(DL-028)
	selected map[int]bool
	// matched maps a visible row to the byte offsets a query matched, for
	// highlighting.
	matched map[int][]int

	offset    int
	height    int
	width     int
	cancelled bool
	done      bool
	err       error

	styles pickerStyles
}

// pickerStyles is the picker's palette.
type pickerStyles struct {
	title     lipgloss.Style
	count     lipgloss.Style
	cursor    lipgloss.Style
	match     lipgloss.Style
	detail    lipgloss.Style
	help      lipgloss.Style
	selected  lipgloss.Style
	emptyHint lipgloss.Style
}

func newPickerStyles(dark bool) pickerStyles {
	pick := lipgloss.LightDark(dark)
	return pickerStyles{
		title:     lipgloss.NewStyle().Bold(true),
		count:     lipgloss.NewStyle().Foreground(pick(lipgloss.Color("#5c6773"), lipgloss.Color("#9aa7b4"))),
		cursor:    lipgloss.NewStyle().Foreground(pick(lipgloss.Color("#a8761f"), lipgloss.Color("#e0b341"))),
		match:     lipgloss.NewStyle().Bold(true).Foreground(pick(lipgloss.Color("#a8761f"), lipgloss.Color("#e0b341"))),
		detail:    lipgloss.NewStyle().Foreground(pick(lipgloss.Color("#5c6773"), lipgloss.Color("#9aa7b4"))),
		help:      lipgloss.NewStyle().Foreground(pick(lipgloss.Color("#9aa7b4"), lipgloss.Color("#5c6773"))),
		selected:  lipgloss.NewStyle().Foreground(pick(lipgloss.Color("#2f7d55"), lipgloss.Color("#7fd6a3"))),
		emptyHint: lipgloss.NewStyle().Foreground(pick(lipgloss.Color("#9aa7b4"), lipgloss.Color("#5c6773"))),
	}
}

// newRepoPicker builds the picker over the repositories offered.
//
// @decision(DL-028)
func newRepoPicker(repos []RepoChoice, dark bool, now time.Time) *repoPicker {
	width := slugColumnWidth(repos)
	platformWidth := 0
	if width > 0 {
		for _, r := range repos {
			if n := lipgloss.Width(r.Platform); n > platformWidth {
				platformWidth = n
			}
		}
	}
	rows := make([]pickerRow, 0, len(repos))
	for _, r := range repos {
		slug := r.Repo.Slug()
		pad := 0
		if width > 0 {
			if pad = width - lipgloss.Width(slug); pad < 0 {
				pad = 0
			}
			pad += 2
		}
		rows = append(rows, pickerRow{
			repo:        r,
			haystack:    pickerHaystack(r, width > 0),
			slugPad:     pad,
			slugLen:     len(slug),
			platformPad: platformWidth,
			detail:      repoDetail(r, now),
		})
	}
	p := &repoPicker{
		rows:     rows,
		selected: map[int]bool{},
		matched:  map[int][]int{},
		height:   maxPickerRows,
		styles:   newPickerStyles(dark),
	}
	p.refilter()
	return p
}

// pickerHaystack is the searchable text of a row: the slug, then the
// platform when there is more than one.
//
// The slug leads because the matcher is greedy rather than optimal: with
// the platform first, a query like "docs" matches "co(d)efl(o)e" plus a
// trailing "cs" and scores far below the same repository on another
// platform, even though its name literally contains the query.
//
// @decision(DL-028)
func pickerHaystack(r RepoChoice, withPlatform bool) string {
	if !withPlatform {
		return r.Repo.Slug()
	}
	return r.Repo.Slug() + " " + r.Platform
}

// render is the row's text as drawn: the haystack with the slug column
// padded out so the platform names line up.
//
// @decision(DL-028)
func (r pickerRow) render() string {
	if r.slugPad == 0 {
		return r.haystack
	}
	platform := r.haystack[r.slugLen+1:]
	pad := r.platformPad - lipgloss.Width(platform)
	if pad < 0 {
		pad = 0
	}
	return r.haystack[:r.slugLen] + strings.Repeat(" ", r.slugPad) + platform + strings.Repeat(" ", pad)
}

// shift maps byte offsets in the haystack onto the rendered row, which
// has padding inserted after the slug.
//
// @decision(DL-028)
func (r pickerRow) shift(indexes []int) []int {
	if r.slugPad == 0 || len(indexes) == 0 {
		return indexes
	}
	out := make([]int, 0, len(indexes))
	for _, i := range indexes {
		// The separator at slugLen is itself replaced by the padding, so
		// it shifts with the rest. Either side of the boundary maps to a
		// space, so this is which padding cell lights up rather than a
		// correctness question; the one next to the platform name is
		// where the match actually sits.
		if i >= r.slugLen {
			i += r.slugPad - 1
		}
		out = append(out, i)
	}
	return out
}

// repoDetail is the trailing detail that helps pick but is not searched:
// privacy, archived state, whether the token may only read it, and
// staleness.
//
// @decision(DL-042)
func repoDetail(c RepoChoice, now time.Time) string {
	r := c.Repo
	var parts []string
	if c.MaybeReadOnly {
		parts = append(parts, "public, push only if your token was given it")
	}
	if r.Private {
		parts = append(parts, "private")
	}
	if r.Archived {
		parts = append(parts, "archived")
	}
	if !r.UpdatedAt.IsZero() {
		parts = append(parts, humanAge(now.Sub(r.UpdatedAt)))
	}
	return strings.Join(parts, "  ")
}

// refilter recomputes the visible rows for the current query. With no
// query every row is visible in its natural order; with one, rows are
// ranked best first.
//
// @decision(DL-028)
func (p *repoPicker) refilter() {
	p.matched = map[int][]int{}
	if p.query == "" {
		p.visible = make([]int, len(p.rows))
		for i := range p.rows {
			p.visible[i] = i
		}
		p.clampCursor()
		return
	}

	haystacks := make([]string, len(p.rows))
	for i, r := range p.rows {
		haystacks[i] = r.haystack
	}
	// Find sorts by score with a stable sort, so rows with equal scores
	// keep their natural order and do not swap between keystrokes.
	matches := fuzzy.Find(p.query, haystacks)
	p.visible = make([]int, 0, len(matches))
	best := 0
	if len(matches) > 0 {
		best = matches[0].Score
	}
	for _, m := range matches {
		if best-m.Score > maxScoreGap {
			continue
		}
		p.visible = append(p.visible, m.Index)
		p.matched[m.Index] = m.MatchedIndexes
	}
	p.clampCursor()
}

// clampCursor keeps the cursor and scroll offset inside the filtered
// view, which shrinks and grows as the query changes.
func (p *repoPicker) clampCursor() {
	if p.cursor >= len(p.visible) {
		p.cursor = len(p.visible) - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
	p.scrollToCursor()
}

// scrollToCursor moves the window so the cursor is inside it.
func (p *repoPicker) scrollToCursor() {
	h := p.rowsVisible()
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+h {
		p.offset = p.cursor - h + 1
	}
	if p.offset > len(p.visible)-h {
		p.offset = len(p.visible) - h
	}
	if p.offset < 0 {
		p.offset = 0
	}
}

// rowsVisible is how many rows fit, given the window and the cap.
func (p *repoPicker) rowsVisible() int {
	h := p.height
	if h > maxPickerRows {
		h = maxPickerRows
	}
	if h < 1 {
		h = 1
	}
	return h
}

func (p *repoPicker) Init() tea.Cmd { return nil }

// Update handles one key press.
//
// @decision(DL-028)
func (p *repoPicker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.width = msg.Width
		// Applied even when it goes non-positive: rowsVisible floors at
		// one row, and keeping a stale larger height renders a view
		// taller than the terminal.
		p.height = msg.Height - pickerChromeLines
		p.scrollToCursor()
		return p, nil
	case tea.KeyPressMsg:
		return p.handleKey(msg)
	}
	return p, nil
}

// handleKey applies one key press. Printable characters extend the
// query, so filtering needs no mode to enter or leave.
//
// @decision(DL-028)
func (p *repoPicker) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		p.cancelled = true
		p.done = true
		return p, tea.Quit
	case "enter":
		if len(p.selected) == 0 {
			p.err = fmt.Errorf("select at least one repository")
			return p, nil
		}
		p.done = true
		return p, tea.Quit
	case "up", "ctrl+p":
		p.moveCursor(-1)
		return p, nil
	case "down", "ctrl+n":
		p.moveCursor(1)
		return p, nil
	case "pgup":
		p.moveCursor(-p.rowsVisible())
		return p, nil
	case "pgdown":
		p.moveCursor(p.rowsVisible())
		return p, nil
	case "space":
		// The terminal decoder emits the space bar as
		// {Code: KeySpace, Text: " "}, exactly like any other printable
		// character, so a typed space and a select press are the same
		// event. Space selects; the query never receives one.
		p.toggle()
		return p, nil
	case "backspace":
		if p.query != "" {
			p.query = trimLastRune(p.query)
			p.err = nil
			p.refilter()
		}
		return p, nil
	case "ctrl+u":
		if p.query != "" {
			p.query = ""
			p.err = nil
			p.refilter()
		}
		return p, nil
	}

	// Anything printable extends the query. Key.Text is non-empty only
	// for keys that produce characters, so this cannot swallow Tab or
	// the arrow keys. Shift is the one modifier that still produces
	// query text; alt+x and friends are commands, not input.
	if text := msg.Key().Text; text != "" && msg.Mod&^tea.ModShift == 0 {
		p.query += text
		p.err = nil
		p.refilter()
	}
	return p, nil
}

// trimLastRune drops the final rune, so backspace deletes a character
// rather than a byte.
func trimLastRune(s string) string {
	r := []rune(s)
	return string(r[:len(r)-1])
}

func (p *repoPicker) moveCursor(delta int) {
	if len(p.visible) == 0 {
		return
	}
	p.cursor += delta
	p.clampCursor()
}

// toggle flips the row under the cursor, by its identity in rows.
//
// @decision(DL-028)
func (p *repoPicker) toggle() {
	if len(p.visible) == 0 {
		return
	}
	i := p.visible[p.cursor]
	if p.selected[i] {
		delete(p.selected, i)
		return
	}
	p.selected[i] = true
	p.err = nil
}

// chosen is every selected repository, in the order they were offered.
// It is not limited to rows matching the final query: a repository
// selected under an earlier query stays selected.
//
// @decision(DL-028)
func (p *repoPicker) chosen() []RepoChoice {
	idx := make([]int, 0, len(p.selected))
	for i := range p.selected {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]RepoChoice, 0, len(idx))
	for _, i := range idx {
		out = append(out, p.rows[i].repo)
	}
	return out
}

// View renders the picker.
//
// @decision(DL-028)
func (p *repoPicker) View() tea.View {
	var b strings.Builder

	b.WriteString(p.styles.title.Render("Which repositories should the sandbox start with?"))
	b.WriteString("\n")

	query := p.query
	if query == "" {
		query = p.styles.count.Render("type to search")
	}
	count := fmt.Sprintf("%d of %d", len(p.visible), len(p.rows))
	if n := len(p.selected); n > 0 {
		count += fmt.Sprintf("  %d selected", n)
	}
	b.WriteString("> " + query + "  " + p.styles.count.Render(count) + "\n\n")

	if len(p.visible) == 0 {
		b.WriteString(p.styles.emptyHint.Render("  no repositories match " + fmt.Sprintf("%q", p.query)))
		b.WriteString("\n")
	} else {
		h := p.rowsVisible()
		for n := p.offset; n < len(p.visible) && n < p.offset+h; n++ {
			b.WriteString(p.renderRow(n))
			b.WriteString("\n")
		}
	}

	if p.err != nil {
		b.WriteString(p.styles.cursor.Render("  " + p.err.Error()))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(p.styles.help.Render("  ↑↓ move · space select · enter confirm · esc cancel"))

	return tea.NewView(p.fit(b.String()))
}

// fit truncates every line to the window. A line wider than the
// terminal wraps, which pushes the rest of the view down and breaks the
// layout — for the title and the help as much as for a long row.
// Truncation drops that line's styling with it, so it is applied only to
// the lines that do not fit.
//
// @decision(DL-028)
func (p *repoPicker) fit(view string) string {
	if p.width <= 0 {
		return view
	}
	lines := strings.Split(view, "\n")
	for i, line := range lines {
		if lipgloss.Width(line) > p.width {
			lines[i] = truncate(line, p.width)
		}
	}
	return strings.Join(lines, "\n")
}

// renderRow draws one visible row, highlighting the characters a query
// matched.
//
// @decision(DL-028)
func (p *repoPicker) renderRow(n int) string {
	i := p.visible[n]
	row := p.rows[i]

	cursor := "  "
	if n == p.cursor {
		cursor = p.styles.cursor.Render("> ")
	}
	box := "[ ] "
	if p.selected[i] {
		box = p.styles.selected.Render("[x] ")
	}

	line := cursor + box + highlight(row.render(), row.shift(p.matched[i]), p.styles.match)
	if row.detail != "" {
		line += "  " + p.styles.detail.Render(row.detail)
	}
	return line
}

// highlight styles the matched byte offsets within s. The indexes come
// from the matcher and are byte offsets into the same string, so the
// runes they start are emitted whole.
//
// @decision(DL-028)
func highlight(s string, indexes []int, style lipgloss.Style) string {
	if len(indexes) == 0 {
		return s
	}
	hit := make(map[int]bool, len(indexes))
	for _, i := range indexes {
		hit[i] = true
	}
	var b strings.Builder
	for i, r := range s {
		if hit[i] {
			b.WriteString(style.Render(string(r)))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
