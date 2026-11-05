package opencode

import (
	"strings"
	"testing"
)

// The announcement must survive rendering into the sandbox: the plugin
// the sandbox runs is the rendered source, not the file on disk, and a
// config placeholder or an escaping bug could drop it.
//
// @decision(DL-048)
func TestTheRenderedPluginCarriesTheAnnouncement(t *testing.T) {
	src, err := (&Plugin{Kind: "report", Goal: "write a report"}).Source()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`const STOP_MARKER = "` + LoopStoppedMarker + `"`,
		"async function announceStop(",
		`announceStop(ctx, sessionID, "continue-failed")`,
		"if (loop.stopped) {",
		"if (loop.announced) return",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the rendered plugin is missing %q; a job would never announce it had finished", want)
		}
	}
	// The config placeholder must be gone: an unreplaced one is a
	// ReferenceError at load and the plugin never attaches at all.
	if strings.Contains(src, "SPOV_GOAL_CONFIG") {
		t.Error("the config placeholder survived rendering; the plugin would fail to load")
	}
}
