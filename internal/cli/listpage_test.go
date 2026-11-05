package cli

import "testing"

// The footer's next command keeps a size that was not the default, so
// following it shows the page after this one.
//
// @decision(DL-012)
func TestTheFooterCarriesTheSizeToTheNextPage(t *testing.T) {
	l := listing[int]{rows: []int{1, 2}, total: 9, next: "01NEXT"}
	if got := l.footer("spov jobs", "unfinished", pageFlags{page: 3, limit: 2}); got != "5–6 of 9 · next: spov jobs --page 4 -n 2" {
		t.Errorf("footer = %q", got)
	}
	if got := l.footer("spov jobs", "unfinished", pageFlags{page: 2, limit: DefaultListLimit}); got != "6–7 of 9 · next: spov jobs --page 3" {
		t.Errorf("footer = %q", got)
	}
	whole := listing[int]{rows: []int{1}, total: 1}
	if got := whole.footer("spov jobs", "unfinished", pageFlags{page: 1, limit: DefaultListLimit}); got != "" {
		t.Errorf("a listing that fit has a footer: %q", got)
	}
}
