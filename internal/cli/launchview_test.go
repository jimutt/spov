package cli

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/session"
)

// TestArtRegionIsClear is the guard the decision asks for: new art that
// would collide with the status text fails here rather than shipping
// overlapped.
//
// @decision(DL-024)
func TestArtRegionIsClear(t *testing.T) {
	if !artRegionIsClear(spovFrame()) {
		t.Error("the status block collides with the art; " +
			"re-measure statusCol/statusRow, or re-derive the art with art/shrink.py")
	}
}

// TestArtRegionGuardCatchesInk proves the guard can fail, since a guard
// that always passes protects nothing.
func TestArtRegionGuardCatchesInk(t *testing.T) {
	frame := spovFrame()
	row := []rune(frame[statusRow+1])
	row[statusCol+2] = '@'
	frame[statusRow+1] = string(row)

	if artRegionIsClear(frame) {
		t.Error("the guard passed a frame with ink inside the status block")
	}
}

// TestFrameFillsItsBox: every row is padded to the same width, so the
// status block always lands at the same column.
//
// @decision(DL-024)
func TestFrameFillsItsBox(t *testing.T) {
	frame := spovFrame()
	if len(frame) != artHeight {
		t.Errorf("art has %d rows, want %d", len(frame), artHeight)
	}
	for i, row := range frame {
		if got := len([]rune(row)); got != artWidth {
			t.Errorf("row %d is %d wide, want %d", i, got, artWidth)
		}
	}
}

// TestTruncateMeasuresDisplayWidth: a styled string carries ANSI escapes
// that occupy no cells, and counting them truncates text that fits.
//
// @decision(DL-024)
func TestTruncateMeasuresDisplayWidth(t *testing.T) {
	styled := lipgloss.NewStyle().Foreground(lipgloss.Color("#5c8fd6")).Render("✓ preparing tokens")
	if lipgloss.Width(styled) > statusWidth {
		t.Fatalf("fixture is wider than the block: %d", lipgloss.Width(styled))
	}

	got := truncate(styled, statusWidth)
	if got != styled {
		t.Errorf("a styled string that fits was truncated:\n got %q\nwant %q", got, styled)
	}
	if !strings.Contains(stripStyle(got), "preparing tokens") {
		t.Errorf("visible text was lost: %q", stripStyle(got))
	}
}

func TestTruncateShortensOverlongText(t *testing.T) {
	got := truncate(strings.Repeat("x", statusWidth+10), statusWidth)
	if lipgloss.Width(got) > statusWidth {
		t.Errorf("truncated to %d cells, want at most %d", lipgloss.Width(got), statusWidth)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated line does not mark the cut: %q", got)
	}
}

// TestTruncateMeasuresWideRunes: a CJK character or emoji occupies two
// display cells, so a rune-counted cut overruns the block and runs
// through the art. Note carries remote error text, so this is
// reachable rather than theoretical.
//
// @decision(DL-024)
func TestTruncateMeasuresWideRunes(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("日", 40),
		strings.Repeat("🐦", 30),
		strings.Repeat("x", 60),
		"café " + strings.Repeat("é", 40),
	} {
		got := truncate(in, statusWidth)
		if w := lipgloss.Width(got); w > statusWidth {
			t.Errorf("truncate(%.10q…) = %d cells, want at most %d", in, w, statusWidth)
		}
	}
}

// TestRenderKeepsWideStatusInsideTheBlock is the same property through
// the renderer, which is where an overrun would actually damage the art.
//
// @decision(DL-024)
func TestRenderKeepsWideStatusInsideTheBlock(t *testing.T) {
	out := strings.Split(spovFrame().render([]string{strings.Repeat("日", 60)}), "\n")
	if w := lipgloss.Width(out[statusRow]); w > statusCol+statusWidth {
		t.Errorf("row is %d cells, overrunning the block that ends at %d", w, statusCol+statusWidth)
	}
}

// TestRenderKeepsStatusInsideTheBlock: text must never overrun into the
// art, whatever it is asked to draw.
//
// @decision(DL-024)
func TestRenderKeepsStatusInsideTheBlock(t *testing.T) {
	long := strings.Repeat("A", 200)
	out := strings.Split(spovFrame().render([]string{long}), "\n")
	row := out[statusRow]
	if got := len([]rune(row)); got > statusCol+statusWidth {
		t.Errorf("row is %d cells, overrunning the block that ends at %d", got, statusCol+statusWidth)
	}
}

// stepRecorder is what a launcher would drive.
func newTestView(t *testing.T) (*launchView, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	n := 0
	v := newLaunchView(&buf, true, func() time.Time {
		n++
		return time.Unix(0, 0).Add(time.Duration(n) * time.Second)
	})
	return v, &buf
}

// TestViewRecordsStepTimings: a sandbox that takes 40s instead of 4s is
// the user's cost, and the step list is where that becomes visible.
//
// @decision(DL-024) @decision(DL-029)
func TestViewRecordsStepTimings(t *testing.T) {
	v, buf := newTestView(t)
	v.Start(session.StepTokens)
	v.Done(session.StepTokens)

	buf.Reset()
	v.draw()
	out := stripStyle(buf.String())
	if !strings.Contains(out, "1.0s") {
		t.Errorf("no elapsed time for a completed step:\n%s", out)
	}
	if !strings.Contains(out, "✓ preparing tokens") {
		t.Errorf("completed step not marked done:\n%s", out)
	}
}

func TestViewShowsPendingAndRunningSteps(t *testing.T) {
	v, buf := newTestView(t)
	v.Start(session.StepSandbox)

	buf.Reset()
	v.draw()
	out := stripStyle(buf.String())

	// The running step gets a spinner, the untouched ones a dot.
	if !strings.Contains(out, "· preparing tokens") {
		t.Errorf("a step that never started is not shown as pending:\n%s", out)
	}
	if !strings.ContainsAny(out, strings.Join(spinnerFrames, "")) {
		t.Errorf("the running step has no spinner:\n%s", out)
	}
}

// TestViewCloseIsIdempotent: every exit path from a launch closes the
// view, so closing twice must not panic or double-write.
//
// @decision(DL-024)
func TestViewCloseIsIdempotent(t *testing.T) {
	v, buf := newTestView(t)
	v.Open()
	v.Close()
	first := buf.String()
	v.Close()

	if buf.String() != first {
		t.Error("closing twice wrote the restore sequence twice")
	}
	if !strings.Contains(first, exitAltScreen) {
		t.Error("closing did not leave the alternate screen")
	}
	if !strings.Contains(first, showCursor) {
		t.Error("closing did not restore the cursor")
	}
}

// TestViewRestoresTheTerminal: the alternate screen and hidden cursor
// are global side effects, so a launch that fails must still undo them.
//
// @decision(DL-024)
func TestViewRestoresTheTerminal(t *testing.T) {
	v, buf := newTestView(t)
	v.Open()
	if !strings.Contains(buf.String(), enterAltScreen) {
		t.Fatal("opening did not enter the alternate screen")
	}
	v.Close()

	out := buf.String()
	if strings.LastIndex(out, exitAltScreen) < strings.LastIndex(out, enterAltScreen) {
		t.Error("the alternate screen was entered after it was left")
	}
}

// TestViewDrawsNothingAfterClose: a late draw from the animation
// goroutine must not write over the restored terminal.
//
// @decision(DL-024)
func TestViewDrawsNothingAfterClose(t *testing.T) {
	v, buf := newTestView(t)
	v.Open()
	v.Close()

	buf.Reset()
	v.draw()
	v.Start(session.StepServer)
	v.Note("something happened")

	if buf.Len() != 0 {
		t.Errorf("the view wrote %d bytes after being closed: %q", buf.Len(), buf.String())
	}
}

// TestFormatElapsed: the timing must fit beside the longest step name
// inside the block, so it is always five cells or fewer.
//
// @decision(DL-024)
func TestFormatElapsed(t *testing.T) {
	for _, tt := range []struct {
		d    time.Duration
		want string
	}{
		{250 * time.Millisecond, "0.2s"},
		{1500 * time.Millisecond, "1.5s"},
		{42 * time.Second, "42.0s"},
	} {
		if got := formatElapsed(tt.d); got != tt.want {
			t.Errorf("formatElapsed(%s) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestTerminalIsDark(t *testing.T) {
	t.Setenv("COLORFGBG", "0;15")
	if terminalIsDark() {
		t.Error("COLORFGBG with a light background reported dark")
	}
	t.Setenv("COLORFGBG", "15;0")
	if !terminalIsDark() {
		t.Error("COLORFGBG with a dark background reported light")
	}
	t.Setenv("COLORFGBG", "")
	if !terminalIsDark() {
		t.Error("an unset COLORFGBG should default to dark")
	}
}

// TestStepLineFitsTheBlock: the longest step name with a timing must
// not overflow, or the drawn line is silently truncated.
//
// @decision(DL-024)
func TestStepLineFitsTheBlock(t *testing.T) {
	v, _ := newTestView(t)
	for _, step := range session.LaunchSteps() {
		line := v.renderStep(stepState{step: step, done: true, elapsed: 99 * time.Second})
		if w := lipgloss.Width(line); w > statusWidth {
			t.Errorf("step %q renders %d cells, want at most %d: %q",
				step, w, statusWidth, stripStyle(line))
		}
		// The rendered line fills the block exactly by design, so the
		// name is what carries the headroom. Checked here as well as in
		// the session package, since this is where it is drawn.
		if n := len([]rune(string(step))); n > session.MaxStepNameWidth {
			t.Errorf("step %q is %d cells, over the %d budget", step, n, session.MaxStepNameWidth)
		}
	}
}

// pausingWriter blocks mid-frame so Close can interleave with a draw.
type pausingWriter struct {
	mu      sync.Mutex
	written strings.Builder
	pause   chan struct{}
	armed   bool
}

func (w *pausingWriter) Write(p []byte) (int, error) {
	if w.armed && strings.Contains(string(p), clearScreen) {
		w.armed = false
		<-w.pause // hold this frame until released
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.written.Write(p)
	return len(p), nil
}

// TestNoPaintAfterClose is the worst failure this view can have: a
// frame that passed the stopped check and is parked in its write lands
// after Close restored the terminal, painting the art and a
// clear-screen over the user's shell.
//
// @decision(DL-024)
func TestNoPaintAfterClose(t *testing.T) {
	w := &pausingWriter{pause: make(chan struct{})}
	v := newLaunchView(w, true, time.Now)

	// The race is a frame that passed the stopped check and is parked in
	// its write when Close runs. Drive draw() directly from another
	// goroutine so the parked write is mid-flight.
	w.armed = true
	drawing := make(chan struct{})
	go func() { close(drawing); v.draw() }()
	<-drawing
	time.Sleep(100 * time.Millisecond) // let it reach the parked write

	go func() {
		time.Sleep(100 * time.Millisecond)
		close(w.pause)
	}()
	v.Close()
	time.Sleep(200 * time.Millisecond) // let the parked frame land

	w.mu.Lock()
	out := w.written.String()
	w.mu.Unlock()

	exit := strings.LastIndex(out, exitAltScreen)
	if exit < 0 {
		t.Fatal("no restore sequence written")
	}
	after := out[exit+len(exitAltScreen):]
	t.Logf("%d bytes written AFTER the terminal was restored", len(after))
	if len(after) > 0 {
		t.Errorf("LEAK: a frame was painted onto the restored screen (%d bytes)", len(after))
	}
}

// TestStripStyleHandlesOSC: lipgloss emits OSC 8 for a hyperlink, and
// those end at BEL rather than at a letter. Terminating on the first
// letter spilled the URL into the visible text.
//
// @decision(DL-024)
func TestStripStyleHandlesOSC(t *testing.T) {
	for _, tt := range []struct {
		name, in, want string
	}{
		{"sgr", "\x1b[38;5;33mblue\x1b[0m", "blue"},
		{"osc bel", "\x1b]8;;http://example.com\x07label\x1b]8;;\x07", "label"},
		{"osc st", "\x1b]8;;http://example.com\x1b\\label\x1b]8;;\x1b\\", "label"},
		{"plain", "nothing to strip", "nothing to strip"},
		{"trailing escape", "text\x1b", "text"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripStyle(tt.in); got != tt.want {
				t.Errorf("stripStyle(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestNoLaunchViewDisablesTheView keeps the escape hatch honest: it is
// wired to a flag, so a user on a terminal that renders the art badly
// can turn it off.
//
// @decision(DL-024)
func TestNoLaunchViewDisablesTheView(t *testing.T) {
	env := &Env{Err: &bytes.Buffer{}, NoLaunchView: true}
	if env.launchView() != nil {
		t.Error("NoLaunchView did not disable the view")
	}
}

// TestLaunchViewDeclinesANonTerminal: a redirected log must not receive
// alternate-screen escapes.
//
// @decision(DL-024)
func TestLaunchViewDeclinesANonTerminal(t *testing.T) {
	env := &Env{Err: &bytes.Buffer{}}
	if env.launchView() != nil {
		t.Error("the view engaged for a non-terminal writer")
	}
	if (&Env{}).launchView() != nil {
		t.Error("the view engaged with no writer at all")
	}
}

// TestViewIsCentred: on a terminal larger than the view, the content is
// centred rather than stranded in the top-left corner.
//
// @decision(DL-024)
func TestViewIsCentred(t *testing.T) {
	var buf bytes.Buffer
	v := newLaunchView(&buf, true, func() time.Time { return time.Unix(0, 0) })
	v.termWidth, v.termHeight = 100, 40
	v.draw()

	out := stripStyle(buf.String())
	rows := strings.Split(strings.TrimRight(out, "\n"), "\n")

	// Measured against the art, not the first inked row: the flock
	// flies across the whole terminal, so a bird is usually above and
	// left of the art (DL-024).
	// The art's glyphs are ". * @"; the flock's are "\\ v / - ^".
	// Find the first row carrying art ink specifically.
	artTop, artLeft := -1, -1
	for y, r := range rows {
		if i := strings.IndexAny(r, ".*@"); i >= 0 {
			artTop, artLeft = y, i
			break
		}
	}
	if artTop < 0 {
		t.Fatalf("no art found in the output:\n%s", out)
	}

	if want := artTopRow(40); artTop != want {
		t.Errorf("the art starts on row %d, want %d", artTop, want)
	}
	if want := artLeftCol(100); artLeft < want {
		t.Errorf("the art is indented %d, want at least %d", artLeft, want)
	}
}

// TestViewIsFlushOnASmallTerminal: the margins are only applied when
// positive, so a terminal no larger than the view is left alone rather
// than shifted by a negative offset.
//
// @decision(DL-024)
func TestViewIsFlushOnASmallTerminal(t *testing.T) {
	var buf bytes.Buffer
	v := newLaunchView(&buf, true, func() time.Time { return time.Unix(0, 0) })
	v.termWidth, v.termHeight = viewWidth, viewHeight
	v.draw()

	out := stripStyle(buf.String())
	rows := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if strings.TrimSpace(rows[0]) == "" {
		t.Error("a terminal exactly the view's size still got vertical padding")
	}
}

// TestGroundIsUnderTheFeet: the ground is positioned from the art, so a
// re-derived drawing that moves the feet must be caught.
//
// @decision(DL-024)
func TestGroundIsUnderTheFeet(t *testing.T) {
	frame := spovFrame()
	feet := []rune(frame[len(frame)-1])

	first, last := -1, -1
	for i, r := range feet {
		if r != ' ' {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		t.Fatal("the art's last row has no ink; the bird has no feet to stand on")
	}

	ground := []rune(groundLine())
	if len(ground) <= last {
		t.Fatalf("the ground is %d cells, but the feet reach column %d", len(ground), last)
	}
	for x := first; x <= last; x++ {
		if ground[x] == ' ' {
			t.Errorf("the ground has a gap at column %d, under the bird's feet", x)
		}
	}
}

// TestGroundTapers: a solid rule across the full width reads as a
// border rather than as ground.
//
// @decision(DL-024)
func TestGroundTapers(t *testing.T) {
	ground := groundLine()
	if !strings.Contains(ground, groundSolid) {
		t.Error("the ground has no solid section")
	}
	if !strings.Contains(ground, groundFaint) {
		t.Error("the ground does not taper")
	}
	if lipgloss.Width(ground) > viewWidth {
		t.Errorf("the ground is %d cells, wider than the %d-cell view", lipgloss.Width(ground), viewWidth)
	}
}

// TestRenderIncludesTheGround.
//
// @decision(DL-024)
func TestRenderIncludesTheGround(t *testing.T) {
	out := spovFrame().render(nil)
	rows := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(rows) != viewHeight {
		t.Errorf("rendered %d rows, want %d (art plus ground)", len(rows), viewHeight)
	}
	if !strings.Contains(rows[len(rows)-1], groundSolid) {
		t.Errorf("the last row is not the ground: %q", rows[len(rows)-1])
	}
}

// TestFlockStaysOffTheArt: the flock passes behind the spov, and the
// drawing is open shading with gaps inside its outline, so occlusion is
// by silhouette span rather than by ink.
//
// @decision(DL-024)
func TestFlockStaysOffTheArt(t *testing.T) {
	frame := spovFrame()
	rows := make([]string, len(frame))
	copy(rows, frame)

	// The silhouette: first ink to last ink, per row.
	type bounds struct{ from, to int }
	silhouette := map[int]bounds{}
	for y, row := range frame {
		first, last := -1, -1
		for x, r := range row {
			if r != ' ' {
				if first < 0 {
					first = x
				}
				last = x
			}
		}
		if first >= 0 {
			silhouette[y] = bounds{first, last}
		}
	}

	for tick := range 400 {
		out, _ := overlayFlock(rows, tick, 80, 0, 0)
		for y, row := range out {
			b, ok := silhouette[y]
			if !ok {
				continue
			}
			for _, glyph := range flyerFrames {
				for _, at := range indexesOf(row, glyph) {
					if at >= b.from && at <= b.to {
						t.Fatalf("tick %d: a bird at row %d col %d shows through the spov (silhouette %d..%d)",
							tick, y, at, b.from, b.to)
					}
				}
			}
		}
	}
}

func indexesOf(s, sub string) []int {
	var out []int
	for i := 0; ; {
		n := strings.Index(s[i:], sub)
		if n < 0 {
			return out
		}
		out = append(out, i+n)
		i += n + 1
	}
}

// TestFlockUsesTheWholeTerminal: the sky is as wide as the window, not
// as wide as the centred view.
//
// @decision(DL-024)
func TestFlockUsesTheWholeTerminal(t *testing.T) {
	rows := make([]string, 30)
	seen := map[int]bool{}
	for tick := range 600 {
		out, marks := overlayFlock(rows, tick, 120, 0, 0)
		_ = out
		for _, spans := range marks {
			for _, sp := range spans {
				seen[sp.From/20] = true
			}
		}
	}
	// Birds should reach the far left and the far right of a 120-column
	// terminal, not just the middle 54.
	if !seen[0] {
		t.Error("no bird reached the left edge of the terminal")
	}
	if !seen[5] {
		t.Error("no bird reached the right edge of the terminal")
	}
}

// TestFlockTracksTheArt: lanes are relative to the art, so a taller
// terminal must not strand the flock at the top of the screen.
//
// @decision(DL-024)
func TestFlockTracksTheArt(t *testing.T) {
	for _, height := range []int{30, 50} {
		top := artTopRow(height)
		rows := make([]string, height)
		_, marks := overlayFlock(rows, 0, 100, top, 0)
		if len(marks) == 0 {
			t.Fatalf("height %d: no birds drawn", height)
		}
		for row := range marks {
			if d := row - top; d < -4 || d > viewHeight {
				t.Errorf("height %d: a bird on row %d is %d rows from the art at %d", height, row, d, top)
			}
		}
	}
}

// TestSummaryPrintsAfterTheTUI: the summary follows the TUI rather than
// printing between the launch view closing and OpenCode drawing, which
// would leave a bare terminal between the two screens. It is printed
// where the attach and stop commands are useful.
//
// @decision(DL-024)
func TestSummaryPrintsAfterTheTUI(t *testing.T) {
	f := newAPIFixture(t)
	var order []string
	f.env.RunTUI = func(_ context.Context, _ opencode.TUI) (int, error) {
		order = append(order, "tui")
		return 0, nil
	}
	f.env.LocateBinary = func(context.Context, string) (opencode.Located, error) {
		order = append(order, "locate")
		return opencode.Located{Path: "/fake/opencode"}, nil
	}

	_, stderr, err := runWith(t, f.env, "fly")
	if err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	if len(order) != 2 || order[0] != "locate" || order[1] != "tui" {
		t.Errorf("order = %v, want the binary located before the handover", order)
	}
	// The summary is still produced, just later.
	if !strings.Contains(stderr, "is ready") {
		t.Errorf("no session summary:\n%s", stderr)
	}
}

// TestWarningsAreHeldWhileTheViewIsUp: writing to the terminal under
// the alternate screen paints onto a frame about to be discarded.
//
// @decision(DL-024)
func TestWarningsAreHeldWhileTheViewIsUp(t *testing.T) {
	var buf bytes.Buffer
	env := &Env{Err: &buf}

	// With no view, a warning prints immediately.
	env.deferWarning("first")
	if !strings.Contains(buf.String(), "first") {
		t.Error("a warning was swallowed when no view was active")
	}

	// With a view, it is held until the view closes.
	buf.Reset()
	env.activeView = newLaunchView(&bytes.Buffer{}, true, nil)
	env.deferWarning("second")
	if strings.Contains(buf.String(), "second") {
		t.Error("a warning was printed while the view was up")
	}
	env.flushWarnings()
	if !strings.Contains(buf.String(), "second") {
		t.Error("a held warning was never flushed")
	}
}
