package cli

import (
	"fmt"
	"image/color"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"charm.land/lipgloss/v2"
	"golang.org/x/term"

	"github.com/jimutt/spov/internal/session"
)

// spinnerFrames is the braille spinner OpenCode uses, so the two look
// like the same family of tool.
//
// @decision(DL-024)
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerInterval is how often the spinner advances.
const spinnerInterval = 100 * time.Millisecond

// launchPalette is the blue set the view draws in, resolved once for the
// terminal's background.
//
// @decision(DL-024)
type launchPalette struct {
	bird    color.Color
	flock   color.Color
	ground  color.Color
	title   color.Color
	done    color.Color
	running color.Color
	pending color.Color
	detail  color.Color
}

func newPalette(darkBackground bool) launchPalette {
	pick := lipgloss.LightDark(darkBackground)
	return launchPalette{
		// The bird is warm against a cool sky; the distant flock is
		// paler, as distance washes colour out; the ground is earth.
		//
		// @decision(DL-024)
		bird:   pick(lipgloss.Color("#a87d47"), lipgloss.Color("#ecd071")),
		flock:  pick(lipgloss.Color("#7d93a8"), lipgloss.Color("#6b8299")),
		ground: pick(lipgloss.Color("#6b5a45"), lipgloss.Color("#8a7a5f")),
		// The status column is deliberately near-neutral: it is text to
		// read, and the colour it does carry marks state — done, in
		// progress, not started — rather than decorating.
		title:   pick(lipgloss.Color("#2b2b2b"), lipgloss.Color("#e6e6e6")),
		done:    pick(lipgloss.Color("#2f7d55"), lipgloss.Color("#7fd6a3")),
		running: pick(lipgloss.Color("#a8761f"), lipgloss.Color("#e0b341")),
		pending: pick(lipgloss.Color("#9aa7b4"), lipgloss.Color("#5c6773")),
		detail:  pick(lipgloss.Color("#5c6773"), lipgloss.Color("#9aa7b4")),
	}
}

// stepState is one step's progress through a launch.
type stepState struct {
	step    session.Step
	started time.Time
	elapsed time.Duration
	done    bool
}

// launchView draws the launch progress over the storspov art. It
// implements session.StepReporter, so the launcher drives it without
// knowing whether progress is drawn or printed.
//
// @decision(DL-024)
type launchView struct {
	out     io.Writer
	palette launchPalette
	now     func() time.Time

	// termWidth and termHeight centre the view in the window. Zero
	// leaves it flush at the top left, which is what a test wants.
	//
	// @decision(DL-024)
	termWidth  int
	termHeight int

	// mu guards the view's state. writeMu serialises terminal writes
	// separately, so a status update never waits on a slow terminal.
	mu      sync.Mutex
	writeMu sync.Mutex
	steps   []stepState
	notes   []string
	frame   int
	stopped bool

	done chan struct{}
	once sync.Once
}

// newLaunchView builds a view writing to out.
//
// @decision(DL-024)
func newLaunchView(out io.Writer, darkBackground bool, now func() time.Time) *launchView {
	if now == nil {
		now = time.Now
	}
	steps := make([]stepState, 0, len(session.LaunchSteps()))
	for _, s := range session.LaunchSteps() {
		steps = append(steps, stepState{step: s})
	}
	return &launchView{
		out:     out,
		palette: newPalette(darkBackground),
		now:     now,
		steps:   steps,
		done:    make(chan struct{}),
	}
}

// Open enters the alternate screen and begins animating. It is not named
// Start so it cannot be confused with StepReporter.Start. Callers must
// defer Close immediately, before anything that can fail: Close restores
// the terminal and stops the animation goroutine.
//
// @decision(DL-024)
func (v *launchView) Open() {
	_, _ = io.WriteString(v.out, enterAltScreen+hideCursor)
	v.draw()
	go v.animate()
}

// Close leaves the alternate screen. It is safe to call more than once,
// which matters because every exit path from a launch calls it.
//
// @decision(DL-024)
func (v *launchView) Close() {
	v.once.Do(func() {
		close(v.done)

		// Marked stopped before taking writeMu: a frame already waiting
		// on that lock re-checks the flag and discards itself rather
		// than painting over the restored terminal.
		v.mu.Lock()
		v.stopped = true
		v.mu.Unlock()

		v.writeMu.Lock()
		defer v.writeMu.Unlock()
		_, _ = io.WriteString(v.out, showCursor+exitAltScreen)
	})
}

// animate advances the spinner until the view is stopped.
func (v *launchView) animate() {
	ticker := time.NewTicker(spinnerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-v.done:
			return
		case <-ticker.C:
			v.mu.Lock()
			v.frame++
			v.mu.Unlock()
			v.draw()
		}
	}
}

// Start marks a step as running.
func (v *launchView) Start(step session.Step) {
	v.mu.Lock()
	for i := range v.steps {
		if v.steps[i].step == step {
			v.steps[i].started = v.now()
		}
	}
	v.mu.Unlock()
	v.draw()
}

// Done marks a step complete and records how long it took.
func (v *launchView) Done(step session.Step) {
	v.mu.Lock()
	for i := range v.steps {
		if v.steps[i].step == step && !v.steps[i].done {
			v.steps[i].done = true
			if !v.steps[i].started.IsZero() {
				v.steps[i].elapsed = v.now().Sub(v.steps[i].started)
			}
		}
	}
	v.mu.Unlock()
	v.draw()
}

// Note records a message outside the step sequence.
func (v *launchView) Note(msg string) {
	v.mu.Lock()
	v.notes = append(v.notes, msg)
	v.mu.Unlock()
	v.draw()
}

// draw renders one frame. The write happens under writeMu with stopped
// re-checked inside it, so a frame in flight cannot land after Close has
// restored the screen and paint over the user's shell.
//
// @decision(DL-024)
func (v *launchView) draw() {
	v.mu.Lock()
	if v.stopped {
		v.mu.Unlock()
		return
	}
	frame, status := v.snapshot()
	palette := v.palette
	groundProgress := v.progressFraction()
	tick := v.frame
	v.mu.Unlock()

	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	v.mu.Lock()
	stopped := v.stopped
	v.mu.Unlock()
	if stopped {
		return
	}
	// Rendered unstyled, so the flock can be drawn in terminal
	// coordinates onto plain rows; colour is applied at the end. The
	// sky is as wide as the window, not as wide as the view.
	//
	// @decision(DL-024)
	body := frame.renderTo(status, groundProgress)
	rows := centreRows(body, v.termWidth, v.termHeight)
	rows, flyers := overlayFlock(rows, tick, v.termWidth, artTopRow(v.termHeight), artLeftCol(v.termWidth))
	marks := flockMarks{Flyers: flyers, Ground: map[int]bool{groundRow(rows): true}}
	_, _ = io.WriteString(v.out, clearScreen+home+paintRows(rows, palette, marks))
}

// snapshot renders the status block. The caller must hold the lock.
func (v *launchView) snapshot() (launchFrame, []string) {
	art := spovFrame()

	status := []string{
		lipgloss.NewStyle().Foreground(v.palette.title).Bold(true).Render("s p o v"),
		"",
	}
	for _, s := range v.steps {
		status = append(status, v.renderStep(s))
	}
	if note := v.lastNote(); note != "" {
		status = append(status, "", lipgloss.NewStyle().Foreground(v.palette.detail).Render(note))
	}

	return art, status
}

// renderStep draws one step line: marker, name, and elapsed time.
func (v *launchView) renderStep(s stepState) string {
	style := lipgloss.NewStyle()
	var marker string

	switch {
	case s.done:
		marker = "✓"
		style = style.Foreground(v.palette.done)
	case !s.started.IsZero():
		marker = spinnerFrames[v.frame%len(spinnerFrames)]
		style = style.Foreground(v.palette.running)
	default:
		marker = "·"
		style = style.Foreground(v.palette.pending)
	}

	line := marker + " " + string(s.step)
	if s.elapsed > 0 {
		// Padding is computed on the unstyled text, then the whole line
		// is styled once, so the escapes never affect the alignment.
		timing := formatElapsed(s.elapsed)
		pad := statusWidth - len([]rune(line)) - len([]rune(timing))
		if pad < 1 {
			pad = 1
		}
		line += strings.Repeat(" ", pad) + timing
	}
	return style.Render(line)
}

// lastNote is the most recent note, since the block has room for one.
func (v *launchView) lastNote() string {
	if len(v.notes) == 0 {
		return ""
	}
	return v.notes[len(v.notes)-1]
}

// formatElapsed renders a duration in seconds, even below one second:
// the longest step name plus a five-cell timing is the block's full width.
//
// @decision(DL-024)
func formatElapsed(d time.Duration) string {
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// Terminal control sequences, written directly rather than through a TUI
// framework: the view is torn down before OpenCode takes the terminal.
//
// @decision(DL-024)
const (
	enterAltScreen = "\x1b[?1049h"
	exitAltScreen  = "\x1b[?1049l"
	hideCursor     = "\x1b[?25l"
	showCursor     = "\x1b[?25h"
	clearScreen    = "\x1b[2J"
	home           = "\x1b[H"
)

// launchView builds the drawn view, or returns nil to keep the plain
// progress lines when stderr is not a terminal or is too small for it.
//
// @decision(DL-024)
func (e *Env) launchView() *launchView {
	if e.NoLaunchView {
		return nil
	}
	f, ok := e.Err.(interface{ Fd() uintptr })
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return nil
	}
	width, height, err := term.GetSize(int(f.Fd()))
	if err != nil || width < viewWidth || height < viewHeight {
		return nil
	}
	v := newLaunchView(e.Err, terminalIsDark(), e.now)
	v.termWidth, v.termHeight = width, height
	return v
}

// terminalIsDark guesses the terminal background from COLORFGBG and
// defaults to dark, since querying the terminal needs a round trip before
// the first frame.
//
// @decision(DL-024)
func terminalIsDark() bool {
	// COLORFGBG is set by several terminals as "fg;bg"; a bg of 15 or 7
	// means light. Anything else falls through to dark.
	if v := os.Getenv("COLORFGBG"); v != "" {
		if i := strings.LastIndex(v, ";"); i >= 0 {
			switch v[i+1:] {
			case "15", "7":
				return false
			}
		}
	}
	return true
}

// progressFraction is the share of steps completed, which draws the
// ground in as the launch proceeds. The caller must hold the lock.
//
// @decision(DL-024)
func (v *launchView) progressFraction() float64 {
	if len(v.steps) == 0 {
		return 1
	}
	done := 0
	for _, s := range v.steps {
		if s.done {
			done++
		}
	}
	// The running step counts as half, so the line moves when a step
	// starts rather than only when it finishes.
	for _, s := range v.steps {
		if !s.done && !s.started.IsZero() {
			return (float64(done) + 0.5) / float64(len(v.steps))
		}
	}
	return float64(done) / float64(len(v.steps))
}
