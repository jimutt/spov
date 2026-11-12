package server

import (
	"testing"

	"github.com/jimutt/spov/internal/store"
)

// TestADeliverablesLinkIsServedOnlyAsHTTP: a row stored before the forge's
// answer was checked may hold any scheme, and the view never serves one.
//
// @decision(DL-135)
func TestADeliverablesLinkIsServedOnlyAsHTTP(t *testing.T) {
	for url, want := range map[string]string{
		"https://codefloe.com/a/b/pulls/1": "https://codefloe.com/a/b/pulls/1",
		"http://git.example/a/b/pulls/1":   "http://git.example/a/b/pulls/1",
		"javascript:alert(1)":              "",
		"data:text/html,x":                 "",
		"https:///no-host":                 "",
		"":                                 "",
	} {
		got := deliverableViewOf(store.Deliverable{Kind: store.DeliverablePR, URL: url}).URL
		if got != want {
			t.Errorf("a stored %q is served as %q, want %q", url, got, want)
		}
	}
}
