package cli

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/jimutt/spov/internal/git"
)

func testRepos(t *testing.T) []RepoChoice {
	t.Helper()
	codefloe := "codefloe"
	gh := "github"
	return []RepoChoice{
		{Platform: codefloe, Repo: git.Repo{Owner: "alice", Name: "spov"}},
		{Platform: codefloe, Repo: git.Repo{Owner: "alice", Name: "gadgets"}},
		{Platform: gh, Repo: git.Repo{Owner: "alice", Name: "dotfiles"}},
		{Platform: gh, Repo: git.Repo{Owner: "acme", Name: "spov-docs"}},
	}
}

func newTestPicker(t *testing.T) *repoPicker {
	t.Helper()
	return newRepoPicker(testRepos(t), true, time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))
}

// typeQuery feeds printable characters the way a terminal would.
func typeQuery(p *repoPicker, s string) {
	for _, r := range s {
		p.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func press(p *repoPicker, key string) {
	switch key {
	case "enter":
		p.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	case "esc":
		p.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	case "space":
		p.handleKey(realSpace())
	case "up":
		p.handleKey(tea.KeyPressMsg{Code: tea.KeyUp})
	case "down":
		p.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	case "backspace":
		p.handleKey(tea.KeyPressMsg{Code: tea.KeyBackspace})
	default:
		panic("unknown key " + key)
	}
}

// visibleSlugs is what the user can currently see, in order.
func visibleSlugs(p *repoPicker) []string {
	out := make([]string, 0, len(p.visible))
	for _, i := range p.visible {
		out = append(out, p.rows[i].repo.Repo.Slug())
	}
	return out
}

// TestPickerTypingFiltersImmediately: typing filters with no "/" to
// enter and no mode to leave, which is the point of DL-028.
//
// @decision(DL-028)
func TestPickerTypingFiltersImmediately(t *testing.T) {
	p := newTestPicker(t)
	if len(p.visible) != 4 {
		t.Fatalf("picker starts with %d visible rows, want all 4", len(p.visible))
	}

	typeQuery(p, "dot")
	got := visibleSlugs(p)
	if len(got) != 1 || got[0] != "alice/dotfiles" {
		t.Errorf("query %q left %v, want only alice/dotfiles", p.query, got)
	}

	// Backspace widens it again.
	press(p, "backspace")
	press(p, "backspace")
	press(p, "backspace")
	if p.query != "" || len(p.visible) != 4 {
		t.Errorf("after clearing, query = %q with %d rows, want empty and all 4", p.query, len(p.visible))
	}
}

// TestPickerMatchesSubsequences is the behaviour huh's substring filter
// could not do: "spv" must find "spov".
//
// @decision(DL-028)
func TestPickerMatchesSubsequences(t *testing.T) {
	tests := []struct {
		query string
		want  string
	}{
		{"spv", "alice/spov"},
		{"ad", "alice/dotfiles"},
		{"gdg", "alice/gadgets"},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			p := newTestPicker(t)
			typeQuery(p, tt.query)
			got := visibleSlugs(p)
			if len(got) == 0 {
				t.Fatalf("query %q matched nothing, want %s", tt.query, tt.want)
			}
			// Ranking between two genuine matches is the library's
			// business; what matters is that the intended repository is
			// found and is near the top rather than buried.
			rank := -1
			for i, s := range got {
				if s == tt.want {
					rank = i
					break
				}
			}
			if rank < 0 {
				t.Errorf("query %q left %v, which does not include %s", tt.query, got, tt.want)
			} else if rank > 1 {
				t.Errorf("query %q ranked %s at position %d in %v", tt.query, tt.want, rank, got)
			}
		})
	}
}

// TestPickerSearchesThePlatformColumn: the platform is on screen, so it
// must be searchable.
//
// @decision(DL-028)
func TestPickerSearchesThePlatformColumn(t *testing.T) {
	p := newTestPicker(t)
	typeQuery(p, "github")
	got := visibleSlugs(p)
	if len(got) != 2 {
		t.Fatalf("query %q left %v, want both github repos", p.query, got)
	}
	for _, i := range p.visible {
		if name := p.rows[i].repo.Platform; name != "github" {
			t.Errorf("query %q kept a %s row", p.query, name)
		}
	}
}

// TestPickerSelectionSurvivesFiltering is the subtle one: holding
// positions into the filtered list would select the wrong repository
// once the list reorders.
//
// @decision(DL-028)
func TestPickerSelectionSurvivesFiltering(t *testing.T) {
	p := newTestPicker(t)

	// Select dotfiles under one query.
	typeQuery(p, "dot")
	press(p, "space")
	if n := len(p.selected); n != 1 {
		t.Fatalf("selected %d rows, want 1", n)
	}

	// Change the query so the selected row is not even visible.
	for range "dot" {
		press(p, "backspace")
	}
	typeQuery(p, "gad")
	for _, i := range p.visible {
		if p.rows[i].repo.Repo.Name == "dotfiles" {
			t.Fatal("dotfiles is still visible; the test cannot prove anything")
		}
	}

	// Select a second repository under the new query.
	press(p, "space")

	chosen := p.chosen()
	if len(chosen) != 2 {
		t.Fatalf("chosen = %d repositories, want both", len(chosen))
	}
	names := map[string]bool{}
	for _, c := range chosen {
		names[c.Repo.Name] = true
	}
	if !names["dotfiles"] || !names["gadgets"] {
		t.Errorf("chosen = %v, want dotfiles and gadgets", names)
	}
}

// TestPickerTogglesTheRowUnderTheCursor: space must toggle rather than
// extend the query, and toggling twice deselects.
//
// @decision(DL-028)
func TestPickerTogglesTheRowUnderTheCursor(t *testing.T) {
	p := newTestPicker(t)
	press(p, "down")
	press(p, "space")
	if p.query != "" {
		t.Errorf("space extended the query to %q", p.query)
	}
	chosen := p.chosen()
	if len(chosen) != 1 || chosen[0].Repo.Name != "gadgets" {
		t.Fatalf("chosen = %+v, want the second row", chosen)
	}

	press(p, "space")
	if n := len(p.chosen()); n != 0 {
		t.Errorf("toggling twice left %d selected, want 0", n)
	}
}

// TestPickerRefusesAnEmptyConfirmation: confirming nothing is refused in
// the picker rather than accepted and rejected by the caller.
//
// @decision(DL-028)
func TestPickerRefusesAnEmptyConfirmation(t *testing.T) {
	p := newTestPicker(t)
	press(p, "enter")
	if p.done {
		t.Error("the picker confirmed with nothing selected")
	}
	if p.err == nil {
		t.Error("no message explains why enter did nothing")
	}

	press(p, "space")
	press(p, "enter")
	if !p.done || p.cancelled {
		t.Errorf("after selecting, enter did not confirm (done=%v cancelled=%v)", p.done, p.cancelled)
	}
}

// TestPickerCancels: esc must cancel rather than confirm a partial
// selection.
//
// @decision(DL-028)
func TestPickerCancels(t *testing.T) {
	p := newTestPicker(t)
	press(p, "space")
	press(p, "esc")
	if !p.cancelled || !p.done {
		t.Errorf("esc did not cancel (done=%v cancelled=%v)", p.done, p.cancelled)
	}
}

// TestPickerCursorStaysInRangeWhileFiltering: a query that shrinks the
// list must not leave the cursor pointing past the end.
//
// @decision(DL-028)
func TestPickerCursorStaysInRangeWhileFiltering(t *testing.T) {
	p := newTestPicker(t)
	press(p, "down")
	press(p, "down")
	press(p, "down")
	if p.cursor != 3 {
		t.Fatalf("cursor = %d, want the last row", p.cursor)
	}

	typeQuery(p, "dot") // one match
	if p.cursor >= len(p.visible) {
		t.Errorf("cursor %d is past the %d visible rows", p.cursor, len(p.visible))
	}

	// Toggling must still act on a real row rather than panicking.
	press(p, "space")
	if len(p.chosen()) != 1 {
		t.Errorf("toggle after filtering selected %d rows", len(p.chosen()))
	}
}

// TestPickerEmptyQueryKeepsNaturalOrder: with no query the offered order
// is preserved, which is what UpdatedAt ordering upstream is for.
//
// @decision(DL-028)
func TestPickerEmptyQueryKeepsNaturalOrder(t *testing.T) {
	p := newTestPicker(t)
	want := []string{"alice/spov", "alice/gadgets", "alice/dotfiles", "acme/spov-docs"}
	got := visibleSlugs(p)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// TestPickerShowsAnEmptyState: a blank list is indistinguishable from a
// broken picker.
//
// @decision(DL-028)
func TestPickerShowsAnEmptyState(t *testing.T) {
	p := newTestPicker(t)
	typeQuery(p, "zzzznotarepo")
	if len(p.visible) != 0 {
		t.Fatalf("query matched %d rows, want none", len(p.visible))
	}
	view := stripStyle(p.View().Content)
	if !strings.Contains(view, "no repositories match") {
		t.Errorf("view does not explain the empty list:\n%s", view)
	}
}

// TestPickerHighlightsMatchedCharacters: the indices come from the
// matcher as byte offsets, so a multi-byte name must not be split.
//
// @decision(DL-028)
func TestPickerHighlightsMatchedCharacters(t *testing.T) {
	style := newPickerStyles(true).match

	got := highlight("alice/spov", []int{0, 7}, style)
	if stripStyle(got) != "alice/spov" {
		t.Errorf("highlighting changed the text: %q", stripStyle(got))
	}
	if !strings.Contains(got, "\x1b[") {
		t.Error("no styling was applied to the matched characters")
	}

	// A multi-byte rune must survive: "ä" is two bytes, so a byte index
	// into it would otherwise emit half a rune.
	wide := highlight("kläder/spov", []int{0}, style)
	if stripStyle(wide) != "kläder/spov" {
		t.Errorf("highlighting corrupted a multi-byte name: %q", stripStyle(wide))
	}
}

// TestPickerScrollsAWindowOverALongList: the window must follow the
// cursor rather than clipping it.
//
// @decision(DL-028)
func TestPickerScrollsAWindowOverALongList(t *testing.T) {
	p := "codefloe"
	var repos []RepoChoice
	for i := range 40 {
		repos = append(repos, RepoChoice{
			Platform: p,
			Repo:     git.Repo{Owner: "alice", Name: string(rune('a'+i%26)) + "-repo"},
		})
	}
	picker := newRepoPicker(repos, true, time.Now())

	for range 30 {
		press(picker, "down")
	}
	if picker.cursor < picker.offset || picker.cursor >= picker.offset+picker.rowsVisible() {
		t.Errorf("cursor %d is outside the window [%d,%d)",
			picker.cursor, picker.offset, picker.offset+picker.rowsVisible())
	}

	// The rendered view must never show more rows than fit.
	view := stripStyle(picker.View().Content)
	rows := 0
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "[ ]") || strings.Contains(line, "[x]") {
			rows++
		}
	}
	if rows > picker.rowsVisible() {
		t.Errorf("view rendered %d rows, more than the %d that fit", rows, picker.rowsVisible())
	}
}

// TestPickerResizes: a window size message must change how many rows are
// drawn.
//
// @decision(DL-028)
func TestPickerResizes(t *testing.T) {
	p := newTestPicker(t)
	p.Update(tea.WindowSizeMsg{Width: 80, Height: 8})
	if got := p.rowsVisible(); got != 8-pickerChromeLines {
		t.Errorf("rowsVisible = %d in an 8-row window, want %d", got, 8-pickerChromeLines)
	}

	// A window too short for any row must still leave room for one.
	p.Update(tea.WindowSizeMsg{Width: 80, Height: 2})
	if got := p.rowsVisible(); got < 1 {
		t.Errorf("rowsVisible = %d, want at least 1", got)
	}
}

// TestPickerDropsScatteredMatches: subsequence matching alone is far too
// permissive on a long list, matching a query by taking one letter from
// each of several words.
//
// @decision(DL-028)
func TestPickerDropsScatteredMatches(t *testing.T) {
	gh := "github"
	cf := "codefloe"
	repos := []RepoChoice{
		{Platform: gh, Repo: git.Repo{Owner: "alice", Name: "dotfiles"}},
		// "old-notes" contains d, o and t, but scattered across the whole
		// name rather than clustered: a subsequence match, not a hit.
		{Platform: cf, Repo: git.Repo{Owner: "alice", Name: "old-notes"}},
	}
	p := newRepoPicker(repos, true, time.Now())
	typeQuery(p, "dot")

	got := visibleSlugs(p)
	if len(got) == 0 || got[0] != "alice/dotfiles" {
		t.Fatalf("query %q left %v, want dotfiles first", p.query, got)
	}
	for _, s := range got {
		if s == "alice/old-notes" {
			t.Errorf("query %q kept %v, which matches only by scattered letters", p.query, got)
		}
	}
}

// TestPickerFindsANameContainingTheQuery pins the slug leading the
// haystack: "docs" finds both repositories named spov-docs.
//
// @decision(DL-028)
func TestPickerFindsANameContainingTheQuery(t *testing.T) {
	cf := "codefloe"
	gh := "github"
	repos := []RepoChoice{
		{Platform: cf, Repo: git.Repo{Owner: "alice", Name: "spov-docs"}},
		{Platform: gh, Repo: git.Repo{Owner: "acme", Name: "spov-docs"}},
	}
	p := newRepoPicker(repos, true, time.Now())
	typeQuery(p, "docs")

	if len(p.visible) != 2 {
		t.Errorf("query %q left %v, want both repositories whose names contain it",
			p.query, visibleSlugs(p))
	}
}

// TestPickerMatchesUnpaddedText pins that the haystack carries no column
// padding, so "spv" ranks alice/spov above acme/spov-docs.
//
// @decision(DL-028)
func TestPickerMatchesUnpaddedText(t *testing.T) {
	cf := "codefloe"
	gh := "github"
	repos := []RepoChoice{
		{Platform: cf, Repo: git.Repo{Owner: "alice", Name: "spov"}},
		{Platform: gh, Repo: git.Repo{Owner: "acme", Name: "spov-docs"}},
	}
	p := newRepoPicker(repos, true, time.Now())
	typeQuery(p, "spv")

	got := visibleSlugs(p)
	if len(got) == 0 || got[0] != "alice/spov" {
		t.Errorf("query %q ranked %v, want the exact name first", p.query, got)
	}

	// The haystack must carry no padding, or the scoring is skewed.
	for _, row := range p.rows {
		if strings.Contains(row.haystack, "  ") {
			t.Errorf("haystack %q contains padding", row.haystack)
		}
	}
}

// TestPickerHighlightSurvivesPadding: the matcher's offsets are into the
// unpadded haystack, so they must be shifted onto the drawn row or the
// wrong characters light up.
//
// @decision(DL-028)
func TestPickerHighlightSurvivesPadding(t *testing.T) {
	cf := "codefloe"
	gh := "github"
	repos := []RepoChoice{
		{Platform: cf, Repo: git.Repo{Owner: "alice", Name: "spov"}},
		{Platform: gh, Repo: git.Repo{Owner: "acme", Name: "spov-docs"}},
	}
	p := newRepoPicker(repos, true, time.Now())
	// A query that matches inside the platform name, past the padding.
	typeQuery(p, "github")

	if len(p.visible) == 0 {
		t.Fatal("query matched nothing")
	}
	i := p.visible[0]
	row := p.rows[i]
	rendered := row.render()
	for _, at := range row.shift(p.matched[i]) {
		if at < 0 || at >= len(rendered) {
			t.Fatalf("shifted index %d is outside the rendered row %q", at, rendered)
		}
	}
	// The highlighted characters must be the platform name itself.
	var lit []byte
	for _, at := range row.shift(p.matched[i]) {
		lit = append(lit, rendered[at])
	}
	if string(lit) != "github" {
		t.Errorf("highlighted %q, want the platform name", lit)
	}
}

// TestPickerEdgeCases: the picker must not panic when the filtered list
// is empty, when the window has no room, or when a query is longer than
// anything it could match. Every one of these is reachable by typing.
//
// @decision(DL-028)
func TestPickerEdgeCases(t *testing.T) {
	t.Run("no matches leaves the cursor sane", func(t *testing.T) {
		p := newTestPicker(t)
		typeQuery(p, "zzzznope")
		if len(p.visible) != 0 {
			t.Fatalf("query %q matched %d rows, want none", p.query, len(p.visible))
		}
		if p.cursor != 0 || p.offset != 0 {
			t.Errorf("cursor=%d offset=%d on an empty list", p.cursor, p.offset)
		}
		// None of these may panic with nothing to act on.
		press(p, "down")
		press(p, "up")
		press(p, "space")
		press(p, "enter")
		if n := len(p.chosen()); n != 0 {
			t.Errorf("%d rows were selected from an empty list", n)
		}
		_ = p.View()
	})

	t.Run("a window with no room still renders", func(t *testing.T) {
		p := newTestPicker(t)
		for _, h := range []int{0, 1, 2, 3, 6, 7} {
			p.Update(tea.WindowSizeMsg{Width: 20, Height: h})
			if got := p.rowsVisible(); got < 1 {
				t.Errorf("height %d left room for %d rows", h, got)
			}
			_ = p.View()
		}
	})

	t.Run("a single row cannot be scrolled past", func(t *testing.T) {
		f := "codefloe"
		p := newRepoPicker([]RepoChoice{
			{Platform: f, Repo: git.Repo{Owner: "a", Name: "b"}},
		}, true, time.Now())
		press(p, "down")
		press(p, "down")
		if p.cursor != 0 {
			t.Errorf("cursor = %d on a one-row list", p.cursor)
		}
		press(p, "space")
		if len(p.chosen()) != 1 {
			t.Error("the only row could not be selected")
		}
	})

	t.Run("a query longer than every name clears cleanly", func(t *testing.T) {
		p := newTestPicker(t)
		long := strings.Repeat("a", 40)
		typeQuery(p, long)
		_ = p.View()
		for range long {
			press(p, "backspace")
		}
		if p.query != "" || len(p.visible) != 4 {
			t.Errorf("query=%q with %d rows after clearing", p.query, len(p.visible))
		}
	})
}

// fuzzRepos builds a picker over n synthetic repositories.
func fuzzRepos(n int) *repoPicker {
	f := "codefloe"
	repos := make([]RepoChoice, 0, n)
	for i := range n {
		repos = append(repos, RepoChoice{
			Platform: f,
			Repo:     git.Repo{Owner: "o", Name: fmt.Sprintf("r%03d", i)},
		})
	}
	return newRepoPicker(repos, true, time.Now())
}

// TestPickerInvariantsUnderRandomInput drives random key sequences and
// checks after each one that the cursor is inside the filtered list and
// the scroll window, the offset is non-negative, rendering does not panic,
// and the selection is the rows toggled an odd number of times. The
// alphabet includes space, the toggle key.
//
// @decision(DL-028)
func TestPickerInvariantsUnderRandomInput(t *testing.T) {
	chars := []rune("abcr0129-/ ")
	for seed := int64(0); seed < 300; seed++ {
		rng := rand.New(rand.NewSource(seed))
		p := fuzzRepos(1 + rng.Intn(60))
		want := map[string]bool{}

		for step := range 120 {
			switch rng.Intn(10) {
			case 0:
				p.handleKey(tea.KeyPressMsg{Code: tea.KeyUp})
			case 1:
				p.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
			case 2:
				p.handleKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
			case 3:
				p.handleKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
			case 4:
				p.handleKey(tea.KeyPressMsg{Code: tea.KeyBackspace})
			case 5:
				p.Update(tea.WindowSizeMsg{Width: 20 + rng.Intn(100), Height: rng.Intn(50)})
			case 6:
				if len(p.visible) > 0 {
					slug := p.rows[p.visible[p.cursor]].repo.Repo.Slug()
					if want[slug] {
						delete(want, slug)
					} else {
						want[slug] = true
					}
				}
				p.handleKey(realSpace())
			default:
				c := chars[rng.Intn(len(chars))]
				// A space is the toggle key wherever it comes from, so
				// the model has to account for it here too.
				if c == ' ' && len(p.visible) > 0 {
					slug := p.rows[p.visible[p.cursor]].repo.Repo.Slug()
					if want[slug] {
						delete(want, slug)
					} else {
						want[slug] = true
					}
				}
				p.handleKey(tea.KeyPressMsg{Code: c, Text: string(c)})
			}

			h := p.rowsVisible()
			if len(p.visible) == 0 {
				if p.cursor != 0 || p.offset != 0 {
					t.Fatalf("seed %d step %d: empty list left cursor=%d offset=%d",
						seed, step, p.cursor, p.offset)
				}
			} else {
				if p.cursor < 0 || p.cursor >= len(p.visible) {
					t.Fatalf("seed %d step %d: cursor %d outside %d visible rows",
						seed, step, p.cursor, len(p.visible))
				}
				if p.cursor < p.offset || p.cursor >= p.offset+h {
					t.Fatalf("seed %d step %d: cursor %d outside the window [%d,%d)",
						seed, step, p.cursor, p.offset, p.offset+h)
				}
				if p.offset < 0 {
					t.Fatalf("seed %d step %d: negative offset %d", seed, step, p.offset)
				}
			}
			_ = p.View()
		}

		got := map[string]bool{}
		for _, c := range p.chosen() {
			got[c.Repo.Slug()] = true
		}
		if len(got) != len(want) {
			t.Fatalf("seed %d: selected %v, want %v", seed, got, want)
		}
		for slug := range want {
			if !got[slug] {
				t.Fatalf("seed %d: selected %v, missing %s", seed, got, slug)
			}
		}
	}
}

// TestPickerSpaceSelects: space selects rather than reaching the query.
// It uses the decoder's real message, {Code: KeySpace, Text: " "}.
//
// @decision(DL-028)
func TestPickerSpaceSelects(t *testing.T) {
	p := newTestPicker(t)
	p.handleKey(realSpace())

	if len(p.selected) != 1 {
		t.Errorf("space selected %d rows, want 1", len(p.selected))
	}
	if p.query != "" {
		t.Errorf("space put %q in the query", p.query)
	}

	// And it toggles rather than only setting.
	p.handleKey(realSpace())
	if len(p.selected) != 0 {
		t.Errorf("a second space left %d selected, want 0", len(p.selected))
	}
}

// realSpace is the space bar exactly as the terminal decoder emits it.
// Constructing it any other way tests a message that cannot occur.
func realSpace() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
}

// TestPickerIgnoresModifiedPrintables: alt+x is a command, not query
// text, even though it carries printable Text.
//
// @decision(DL-028)
func TestPickerIgnoresModifiedPrintables(t *testing.T) {
	p := newTestPicker(t)
	p.handleKey(tea.KeyPressMsg{Code: 'x', Text: "x", Mod: tea.ModAlt})
	if p.query != "" {
		t.Errorf("alt+x put %q in the query", p.query)
	}

	// Shift is the one modifier that does produce query text.
	p.handleKey(tea.KeyPressMsg{Code: 'a', Text: "A", Mod: tea.ModShift})
	if p.query != "A" {
		t.Errorf("shift+a gave query %q, want \"A\"", p.query)
	}
}

// TestPickerAppliesAShortWindow: keeping a stale larger height renders
// a view taller than the terminal, which scrolls it.
//
// @decision(DL-028)
func TestPickerAppliesAShortWindow(t *testing.T) {
	p := newTestPicker(t)
	p.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	p.Update(tea.WindowSizeMsg{Width: 80, Height: 3})

	if got := p.rowsVisible(); got != 1 {
		t.Errorf("rowsVisible = %d in a 3-row window, want 1", got)
	}
	lines := strings.Count(p.View().Content, "\n") + 1
	if lines > 3+pickerChromeLines {
		t.Errorf("view is %d lines for a 3-row terminal", lines)
	}
}

// TestPickerTruncatesWideRows: a row wider than the window wraps, which
// breaks the layout of every row below it.
//
// @decision(DL-028)
func TestPickerTruncatesWideRows(t *testing.T) {
	f := "codefloe"
	p := newRepoPicker([]RepoChoice{
		{Platform: f, Repo: git.Repo{Owner: "alice", Name: strings.Repeat("long-", 20)}},
	}, true, time.Now())
	p.Update(tea.WindowSizeMsg{Width: 40, Height: 20})

	for _, line := range strings.Split(stripStyle(p.View().Content), "\n") {
		if w := lipgloss.Width(line); w > 40 {
			t.Errorf("line is %d cells wide in a 40-column window: %q", w, line)
		}
	}
}

// TestPickerShiftsEveryMatchOntoTheRenderedRow: every matcher offset into
// the unpadded haystack maps to the same character on the padded row,
// including a match on the slug/platform separator. The query is set
// directly, since a typed space never reaches it.
//
// @decision(DL-028)
func TestPickerShiftsEveryMatchOntoTheRenderedRow(t *testing.T) {
	cf := "codefloe"
	gh := "github"
	p := newRepoPicker([]RepoChoice{
		{Platform: cf, Repo: git.Repo{Owner: "alice", Name: "spov"}},
		{Platform: gh, Repo: git.Repo{Owner: "acme", Name: "x"}},
	}, true, time.Now())

	sawSeparator := false
	for _, q := range []string{"v c", "spov codefloe", "x g", "github", "spv"} {
		p.query = q
		p.refilter()
		for _, i := range p.visible {
			row := p.rows[i]
			rendered := row.render()
			for _, at := range p.matched[i] {
				if at == row.slugLen {
					sawSeparator = true
				}
				shifted := row.shift([]int{at})[0]
				if shifted < 0 || shifted >= len(rendered) {
					t.Fatalf("query %q: index %d shifted to %d, outside %q",
						q, at, shifted, rendered)
				}
				if row.haystack[at] != rendered[shifted] {
					t.Errorf("query %q: haystack[%d]=%q but rendered[%d]=%q",
						q, at, string(row.haystack[at]), shifted, string(rendered[shifted]))
				}
			}
		}
	}
	// A separator match maps to a space on either side of the boundary,
	// so this test cannot tell the two shifts apart — it guards the
	// invariant that every index lands on its own character, which is
	// what a wrong shift past the separator would break.
	if !sawSeparator {
		t.Error("no query matched the separator; the shift for it is untested")
	}
}

// TestPickerClearsStaleValidationWhileEditing: the "select at least one"
// message must not linger while the user is editing the query.
//
// @decision(DL-028)
func TestPickerClearsStaleValidationWhileEditing(t *testing.T) {
	p := newTestPicker(t)
	typeQuery(p, "spov")
	press(p, "enter") // nothing selected yet
	if p.err == nil {
		t.Fatal("expected a validation message")
	}
	press(p, "backspace")
	if p.err != nil {
		t.Errorf("the message survived an edit: %v", p.err)
	}
}
