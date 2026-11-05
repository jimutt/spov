package cli

import (
	_ "embed"
	"strings"

	"charm.land/lipgloss/v2"
)

// The storspov the tool is named after, derived from the full-size
// drawing in art/spov-source.txt by art/shrink.py.
//
// @decision(DL-024)
//
//go:embed art/spov.txt
var artSpov string

// The frame the art occupies, and the block of it reserved for status
// text. The block was measured to be clear of ink with a gutter on
// every side; artRegionIsClear guards that it stays clear if the art
// changes.
//
// @decision(DL-024)
const (
	artWidth  = 39
	artHeight = 24

	statusCol    = 26
	statusRow    = 15
	statusWidth  = 28
	statusHeight = 8
)

// viewWidth and viewHeight are the whole view: art plus status block,
// and the art plus its ground row. A terminal smaller than this
// declines the view.
//
// @decision(DL-024)
const (
	viewWidth  = statusCol + statusWidth
	viewHeight = artHeight + 1
)

// The ground the bird stands on. It is solid under the art and fades
// out under the status block, so it reads as ground rather than as a
// rule drawn under the whole layout.
//
// @decision(DL-024)
const (
	groundSolid = "─"
	groundMid   = "╌"
	groundFaint = "┈"

	// groundStart is where the line begins, a few cells left of the
	// bird's feet. Derived from the art: the feet sit at column 8.
	groundStart = 4
)

// artGutter is the clearance required between the status block and any
// ink, so text never abuts the bird.
//
// @decision(DL-024)
const artGutter = 2

// launchFrame is the art, as rows padded to artWidth.
type launchFrame []string

// spovFrame parses the embedded art.
//
// @decision(DL-024)
func spovFrame() launchFrame { return parseFrame(artSpov) }

// parseFrame pads the art into its box so every row is the same width.
//
// @decision(DL-024)
func parseFrame(raw string) launchFrame {
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	frame := make(launchFrame, artHeight)
	for i := range frame {
		if i < len(lines) {
			frame[i] = padRight(lines[i], artWidth)
			continue
		}
		frame[i] = strings.Repeat(" ", artWidth)
	}
	return frame
}

// render draws the frame with status lines composited into the reserved
// block. Lines longer than the block are truncated rather than allowed
// to overrun into the art.
//
// @decision(DL-024)
func (f launchFrame) render(status []string) string {
	return f.renderTo(status, 1)
}

// renderTo is render with the ground drawn only part-way, so it fills in
// as a launch proceeds.
//
// @decision(DL-024)
func (f launchFrame) renderTo(status []string, ground float64) string {
	return f.compose(status, ground, func(s string) string { return s })
}

// groundLine renders the tapered ground under the view.
//
// @decision(DL-024)
func groundLine() string { return groundLineTo(1) }

// groundLineTo renders the ground filled to the given fraction of its
// length, so it draws itself in as a launch proceeds.
//
// @decision(DL-024)
func groundLineTo(progress float64) string {
	full := []rune(groundLineFull())
	n := int(float64(len(full)) * progress)
	if n < 0 {
		n = 0
	}
	if n > len(full) {
		n = len(full)
	}
	return string(full[:n])
}

func groundLineFull() string {
	// The line starts a little left of the feet and fades out to the
	// right, so it reads as ground under the bird rather than as a rule
	// under the whole layout.
	start := max(0, groundStart)

	var b strings.Builder
	b.WriteString(strings.Repeat(" ", start))
	for x := start; x < viewWidth; x++ {
		switch {
		case x < artWidth-4:
			b.WriteString(groundSolid)
		case x < artWidth+4:
			b.WriteString(groundMid)
		case x < artWidth+12:
			b.WriteString(groundFaint)
		default:
			return b.String()
		}
	}
	return b.String()
}

// compose lays the status block beside the art, applying paint to the
// art portion of each row only.
//
// @decision(DL-024)
func (f launchFrame) compose(status []string, ground float64, paint func(string) string) string {
	var b strings.Builder
	// The status block can extend past the art, so the view is as tall
	// as whichever ends last.
	rows := max(len(f), statusRow+min(len(status), statusHeight))

	for y := range rows {
		var row string
		if y < len(f) {
			row = f[y]
		}

		i := y - statusRow
		if i < 0 || i >= len(status) || i >= statusHeight {
			if trimmed := strings.TrimRight(row, " "); trimmed != "" {
				b.WriteString(paint(trimmed))
			}
			b.WriteByte('\n')
			continue
		}

		// The art left of the block is painted; the status text keeps
		// the styling it was rendered with.
		left := strings.TrimRight(padRight(row, statusCol)[:statusCol], " ")
		gap := strings.Repeat(" ", statusCol-len([]rune(left)))
		painted := ""
		if left != "" {
			painted = paint(left)
		}
		b.WriteString(painted + gap + truncate(status[i], statusWidth))
		b.WriteByte('\n')
	}

	b.WriteString(paint(groundLineTo(ground)))
	b.WriteByte('\n')
	return b.String()
}

// truncate shortens text to width display cells, marking the cut.
//
// Width is measured with lipgloss rather than by counting runes: a
// styled string carries ANSI escapes that occupy no cells, and counting
// them truncates visible text that would have fitted.
//
// @decision(DL-024)
func truncate(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	if width <= 1 {
		return ""
	}
	// Truncating a styled string mid-escape would corrupt it, so the
	// cut is made on the plain text and the caller's styling is lost.
	// In practice only over-long status lines reach this.
	plain := stripStyle(s)
	if lipgloss.Width(plain) <= width {
		return plain
	}

	// The cut is measured in display cells, not runes: a CJK character
	// or an emoji occupies two cells, so a rune-counted cut overruns the
	// block and runs through the art. Note carries remote error text,
	// so this is reachable rather than theoretical.
	budget := width - 1 // room for the ellipsis
	var b strings.Builder
	used := 0
	for _, r := range plain {
		w := lipgloss.Width(string(r))
		if used+w > budget {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String() + "…"
}

// stripStyle removes ANSI escape sequences.
//
// CSI sequences (ESC[...m and friends) end at a letter, but OSC ones
// (ESC]8;;url BEL, which lipgloss emits for a hyperlink) end at BEL or
// ST and contain letters throughout — terminating on the first letter
// would spill the URL into the visible text.
//
// @decision(DL-024)
func stripStyle(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] != 0x1b {
			b.WriteRune(runes[i])
			continue
		}
		i = skipEscape(runes, i)
	}
	return b.String()
}

// skipEscape returns the index of the last rune of the escape starting
// at i, so the caller's loop increment lands past it.
func skipEscape(runes []rune, i int) int {
	if i+1 >= len(runes) {
		return i
	}
	switch runes[i+1] {
	case '[': // CSI: ends at a final byte in @-~
		for j := i + 2; j < len(runes); j++ {
			if runes[j] >= '@' && runes[j] <= '~' {
				return j
			}
		}
	case ']': // OSC: ends at BEL, or at ST (ESC \)
		for j := i + 2; j < len(runes); j++ {
			if runes[j] == 0x07 {
				return j
			}
			if runes[j] == 0x1b && j+1 < len(runes) && runes[j+1] == '\\' {
				return j + 1
			}
		}
	default:
		return i + 1
	}
	return len(runes) - 1
}

func padRight(s string, width int) string {
	runes := []rune(s)
	if len(runes) >= width {
		return string(runes[:width])
	}
	return s + strings.Repeat(" ", width-len(runes))
}

// artRegionIsClear reports whether the status block, plus its gutter, is
// free of ink in the frame. New art that would collide with the status
// text fails a test rather than shipping overlapped.
//
// @decision(DL-024)
func artRegionIsClear(f launchFrame) bool {
	top := max(0, statusRow-1)
	bottom := min(artHeight, statusRow+statusHeight+1)
	left := max(0, statusCol-artGutter)
	right := min(artWidth, statusCol+statusWidth+artGutter)

	for y := top; y < bottom; y++ {
		runes := []rune(f[y])
		for x := left; x < right && x < len(runes); x++ {
			if runes[x] != ' ' {
				return false
			}
		}
	}
	return true
}

// centre places the rendered view in the middle of a terminal of the
// given size. A zero width or height leaves that axis flush, and a
// terminal no larger than the view is left alone rather than shifted by
// a negative margin.
//
// Horizontal padding is applied per row so it survives the ANSI escapes
// the art and status carry; vertical padding is blank lines above.
//
// @decision(DL-024)
func centreRows(body string, width, height int) []string {
	rows := strings.Split(strings.TrimRight(body, "\n"), "\n")

	if left := (width - viewWidth) / 2; left > 0 {
		pad := strings.Repeat(" ", left)
		for i, row := range rows {
			// An empty row stays empty: padding it would write trailing
			// spaces across the terminal for no visible gain.
			if row != "" {
				rows[i] = pad + row
			}
		}
	}

	// Padded by the view's declared height rather than the row count:
	// the ground row is blank until a launch makes progress, and
	// centring on the shorter body would move the art as it fills in.
	//
	// @decision(DL-024)
	if top := (height - viewHeight) / 2; top > 0 {
		rows = append(make([]string, top), rows...)
	}
	return rows
}

// paintRows colours the composed view element by element rather than
// row by row. The art, the flock and the ground each get their own
// colour, and the status lines keep the styling they were rendered
// with, so the view is not one flat wash.
//
// @decision(DL-024)
func paintRows(rows []string, p launchPalette, marks flockMarks) string {
	art := lipgloss.NewStyle().Foreground(p.bird)
	flock := lipgloss.NewStyle().Foreground(p.flock)
	ground := lipgloss.NewStyle().Foreground(p.ground)

	var b strings.Builder
	for y, row := range rows {
		switch {
		case row == "":
		case marks.Ground[y]:
			row = ground.Render(row)
		default:
			row = paintRowSplit(row, marks.Flyers[y], art, flock)
		}
		b.WriteString(row)
		b.WriteByte('\n')
	}
	return b.String()
}

// flockMarks records which rows carry the flock and the ground, so
// paintRows can colour them separately without re-deriving anything.
//
// @decision(DL-024)
type flockMarks struct {
	// Flyers maps a row to the column spans the birds occupy.
	Flyers map[int][]span
	// Ground marks the ground row.
	Ground map[int]bool
}

// span is a half-open column range.
type span struct{ From, To int }

// paintRowSplit colours a row, giving the flyer spans their own colour
// and the rest the art's.
//
// @decision(DL-024)
func paintRowSplit(row string, flyers []span, art, flock lipgloss.Style) string {
	if len(flyers) == 0 {
		return art.Render(row)
	}
	runes := []rune(row)
	var b strings.Builder
	at := 0
	for _, sp := range flyers {
		from, to := min(sp.From, len(runes)), min(sp.To, len(runes))
		if from < at {
			continue
		}
		if from > at {
			b.WriteString(art.Render(string(runes[at:from])))
		}
		b.WriteString(flock.Render(string(runes[from:to])))
		at = to
	}
	if at < len(runes) {
		b.WriteString(art.Render(string(runes[at:])))
	}
	return b.String()
}

// The distant bird that crosses the sky while a launch runs. Three
// cells wide, cycling through wing positions.
//
// @decision(DL-024)
var flyerFrames = []string{`\v/`, "-v-", `/^\`, "-v-"}

// flyerWidth is how many cells the distant bird occupies.
const flyerWidth = 3

// flyerLane is one horizontal path across the sky: a row, the columns
// it spans, and how fast a bird on it travels.
//
// The lanes are measured against the art — each is a run the standing
// bird leaves clear — so re-deriving the art means re-checking them.
// flyerPath drops any position that would collide regardless.
//
// @decision(DL-024)
type flyerLane struct {
	// Row is relative to the top of the art, not to the terminal:
	// centring pushes the art down on a tall window, and an absolute
	// row would leave the flock stranded near the top of the screen.
	// Negative rows are the sky above the art.
	Row int
	// Slow is how many ticks pass before the bird advances a column.
	// Different values keep the flock from moving in lockstep.
	Slow int
	// Phase offsets the bird's start along the lane.
	Phase int
}

// flyerLanes are the paths the background birds follow. They drift
// roughly horizontally, at different heights and speeds, so the sky
// reads as a flock at distance rather than one bird on a trajectory.
//
// Columns are spans of the whole terminal, not of the view: the sky is
// as wide as the window, and confining the flock to the centred view
// wastes it. Rows are relative to the art, so the flock stays with the
// bird rather than drifting to the top of a tall window.
//
// The phases are spread around the cycle so the birds are scattered
// across the sky from the first frame rather than entering together
// from one edge.
//
// @decision(DL-024)
var flyerLanes = []flyerLane{
	{Row: -2, Slow: 6, Phase: 5},
	{Row: 1, Slow: 5, Phase: 55},
	{Row: 3, Slow: 7, Phase: 25},
	{Row: 6, Slow: 8, Phase: 80},
	{Row: 9, Slow: 4, Phase: 40},
	{Row: 12, Slow: 6, Phase: 15},
}

// overlayFlock draws the background birds across the whole terminal,
// onto rows that have already been centred. Ink is never overwritten,
// so a bird passes behind the art and the status text rather than
// through them.
//
// @decision(DL-024)
func overlayFlock(rows []string, tick, width, artTop, artLeft int) ([]string, map[int][]span) {
	marks := map[int][]span{}
	if width <= 0 {
		return rows, marks
	}
	out := make([]string, len(rows))
	copy(out, rows)
	solid := solidSpans(rows, artTop, artLeft)

	for _, lane := range flyerLanes {
		row := artTop + lane.Row
		if row < 0 || row >= len(out) {
			continue
		}
		slow := max(lane.Slow, 1)
		cycle := width + flyerWidth // fly fully off the right edge
		col := (((tick / slow) + lane.Phase) % cycle) - flyerWidth/2
		glyph := []rune(flyerFrames[(tick/2+lane.Phase)%len(flyerFrames)])

		drawn := drawBehind(out[row], col, glyph, width, solid[row])
		if drawn != out[row] {
			marks[row] = append(marks[row], span{From: max(col, 0), To: col + len(glyph)})
		}
		out[row] = drawn
	}
	return out, marks
}

// drawBehind writes glyph at col, skipping any cell the art occupies so
// the bird passes behind it.
//
// Occlusion is by the art's silhouette span, not by its ink: the
// drawing is open shading with gaps inside its outline, and testing ink
// alone let a bird show through the holes in the spov's body.
//
// @decision(DL-024)
func drawBehind(row string, col int, glyph []rune, width int, solid []span) string {
	runes := []rune(padRight(row, width))
	for i, g := range glyph {
		x := col + i
		if x < 0 || x >= len(runes) {
			continue
		}
		if runes[x] != ' ' || withinAny(solid, x) {
			continue
		}
		runes[x] = g
	}
	return strings.TrimRight(string(runes), " ")
}

// withinAny reports whether x falls inside one of the spans.
func withinAny(spans []span, x int) bool {
	for _, sp := range spans {
		if x >= sp.From && x < sp.To {
			return true
		}
	}
	return false
}

// solidSpans returns, per row, the column range the art occupies from
// its first ink to its last. Everything between is treated as the
// bird's body even where the drawing leaves it blank.
//
// @decision(DL-024)
func solidSpans(rows []string, artTop, artLeft int) map[int][]span {
	out := map[int][]span{}
	frame := spovFrame()
	for y, row := range frame {
		runes := []rune(row)
		first, last := -1, -1
		for x, r := range runes {
			if r != ' ' {
				if first < 0 {
					first = x
				}
				last = x
			}
		}
		if first < 0 {
			continue
		}
		out[artTop+y] = []span{{From: artLeft + first, To: artLeft + last + 1}}
	}
	return out
}

// artTopRow is the terminal row the art starts on once centred. The
// flock's lanes are measured from it.
//
// @decision(DL-024)
func artTopRow(height int) int {
	if top := (height - viewHeight) / 2; top > 0 {
		return top
	}
	return 0
}

// groundRow is the row the ground was drawn on: the last row carrying
// ink, since the ground is the view's final line.
//
// @decision(DL-024)
func groundRow(rows []string) int {
	for y := len(rows) - 1; y >= 0; y-- {
		if strings.TrimSpace(rows[y]) != "" {
			return y
		}
	}
	return -1
}

// artLeftCol is the terminal column the view starts at once centred,
// matching the padding centreRows applies.
//
// @decision(DL-024)
func artLeftCol(width int) int {
	if left := (width - viewWidth) / 2; left > 0 {
		return left
	}
	return 0
}
