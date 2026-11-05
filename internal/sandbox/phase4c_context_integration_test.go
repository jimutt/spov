//go:build integration

// Spike S8 reads, out of the published OpenCode binary, what
// session.hook("context") hands a plugin in `system[]`: strings or parts,
// and if parts, which key carries the text. spov-goal.js pushes the job's
// goal onto that array on every request (DL-039), so its entry must match.
//
//	go test -tags integration ./internal/sandbox/ -run S8 -v
//
// It creates no sandbox and needs no Modal credential or model key.
package sandbox

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestIntegrationS8ContextSystemShape reads how system[] is constructed
// in the build under test.
func TestIntegrationS8ContextSystemShape(t *testing.T) {
	version := testOpenCodeVersion()
	t.Logf("reading the context hook's system[] shape out of OpenCode %s", version)

	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode")
	downloadOpenCodeBinary(t, version, bin)
	blob, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	text := string(blob)

	// 1. The hook is dispatched with a `system` member. Find those
	//    call sites so the rest is read from the right place rather
	//    than from anywhere the word "system" appears.
	dispatch := regexp.MustCompile(`["']context["']\s*,\s*\{[^}]{0,400}`)
	sites := dispatch.FindAllString(text, -1)
	t.Logf("found %d candidate context-hook dispatch sites", len(sites))
	for i, s := range sites {
		if len(s) > 300 {
			s = s[:300]
		}
		t.Logf("  site %d: %s", i, oneLine(s))
	}
	if len(sites) == 0 {
		t.Error("no context-hook dispatch site found: the hook name or its payload has changed, " +
			"and spov-goal.js pushes the goal through exactly this")
	}

	// 2. What goes into system[]: a plain-string array or an array of
	//    parts. The bundle is minified, so these fragment matches are
	//    evidence to read in the log, not assertions.
	for _, probe := range []struct {
		name string
		re   string
	}{
		{"system.push of a bare string", `system\.push\(["'\x60]`},
		{"system.push of an object", `system\.push\(\{`},
		{"a system array literal", `system\s*[:=]\s*\[`},
		{"system built from map", `system\s*[:=][^;]{0,80}\.map\(`},
		{"a text-keyed part", `\{\s*type\s*:\s*["']text["']\s*,\s*text\s*:`},
	} {
		re := regexp.MustCompile(probe.re)
		if m := re.FindAllString(text, 3); len(m) > 0 {
			t.Logf("PRESENT  %-30s %d match(es), e.g. %s", probe.name, len(m), oneLine(m[0]))
		} else {
			t.Logf("absent   %-30s", probe.name)
		}
	}

	// 3. system[] is assigned from a function's return value
	//    (`system:Cje.content(y)` on 0.0.0-beta-18684), so the question is
	//    whether that function returns strings or objects. Its name is
	//    only logged: `content` collides with unrelated code in the bundle.
	sysFrom := regexp.MustCompile(`system:([A-Za-z_$][A-Za-z0-9_$]*)\.([A-Za-z_$][A-Za-z0-9_$]*)\(`)
	for _, m := range sysFrom.FindAllStringSubmatch(text, 5) {
		t.Logf("system[] comes from %s.%s(...)", m[1], m[2])
	}

	// The direct question, asked of the whole bundle: does anything
	// ever treat a system entry as a STRING? A .join on it, or a
	// string method, settles it either way.
	for _, probe := range []struct {
		name string
		re   string
	}{
		{"system joined as strings", `system\.join\(`},
		{"system spread into a string array", `\.\.\.system\b`},
		{"system[0] read as a value", `system\[0\]`},
		{"a system entry's .text read", `system\[[^\]]*\]\.text`},
		{"system elements mapped to parts", `system\.map\(`},
	} {
		re := regexp.MustCompile(probe.re)
		if m := re.FindAllString(text, 2); len(m) > 0 {
			t.Logf("PRESENT  %-34s %d match(es)", probe.name, len(m))
		} else {
			t.Logf("absent   %-34s", probe.name)
		}
	}

	// The most direct evidence available without a running server:
	// where the prompt is assembled, is the system content a string?
	// `system` reaching an AI SDK call as `system:` (singular string)
	// rather than as an array of parts would decide it outright.
	for _, frag := range regexp.MustCompile(`system:[^,;}]{0,60}`).FindAllString(text, 8) {
		t.Logf("system: %s", oneLine(frag))
	}

	// The finding, asserted: one of the two shapes is evident in the
	// bundle. A live probe found {type: "text", text} objects on both the
	// beta and GA (DL-039), which spov-goal.js handles.
	mapped := regexp.MustCompile(`system\.map\(`).MatchString(text)
	readsText := regexp.MustCompile(`system\[[^\]]*\]\.text`).MatchString(text)
	t.Logf("system.map present: %v; system[..].text read anywhere: %v", mapped, readsText)
	if readsText {
		t.Log("something reads .text off a system entry: the array holds parts, " +
			"which spov-goal.js shapes its entry from.")
	}
	if !mapped && !readsText {
		t.Error("neither shape is evident: the bundle changed and this spike can no longer " +
			"tell strings from parts. Do not guess — capture the hook's argument from a " +
			"running sandbox before touching spov-goal.js.")
	}
}

// oneLine collapses a bundle fragment so it reads in test output.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.Join(strings.Fields(s), " ")
}
