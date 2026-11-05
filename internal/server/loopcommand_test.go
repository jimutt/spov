package server

import (
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/opencode"
)

// The loop command arguments are the server's, so this pins them from
// here.
//
// @decision(DL-054) @decision(DL-039) @decision(DL-046)

// The pause and resume arguments the server sends are the ones the
// plugin's command switch matches. An argument the switch does not know
// falls through to the default case, which reports status, and the
// endpoint answers 204 either way — a pause that paused nothing looks
// like one that worked.
//
// @decision(DL-054) @decision(DL-039)
func TestTheLoopCommandArgumentsMatchThePlugin(t *testing.T) {
	src, err := (&opencode.Plugin{Kind: "report", Goal: "g"}).Source()
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{LoopPause, LoopResume} {
		want := `case "` + arg + `":`
		if !strings.Contains(src, want) {
			t.Errorf("the plugin's command switch has no %s;"+
				" the server would send an argument that falls through to status,"+
				" and the 204 would look like it worked", want)
		}
	}
}
