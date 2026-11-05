//go:build integration

package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"

	"github.com/jimutt/spov/internal/opencode"
)

// TestIntegrationSpovGoalPluginDrivesTheLoop runs the plugin spov ships in
// a real sandbox and fails when that source regresses: it must load,
// continue with synthetics, apply its allowlist, and stop when told to.
// No count stops it (DL-049), so the test sends the loop's own stop once
// it has seen two continuations. It creates a billable sandbox and
// terminates what it creates.
//
// @decision(DL-039) @decision(DL-040) @decision(DL-049)
func TestIntegrationSpovGoalPluginDrivesTheLoop(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()

	t.Logf("opencode build under test: %s (image %s)", testOpenCodeVersion(), testImage())

	plugin := opencode.Plugin{
		Kind: "report",
		// A goal that can never be declared met: one the agent could
		// declare would end the run at "declared" (DL-048) before the
		// stop this test sends.
		Goal: "SPOV_GOAL_MARKER: on every turn, reply with a single word and nothing else. " +
			"This is a standing instruction with no end state: it is never complete, " +
			"so do not declare completion.",
		Permissions: opencode.PermissionsForKind("report"),
	}
	source, err := plugin.Source()
	if err != nil {
		t.Fatal(err)
	}

	sb, creds, password := startOpenCodeSandboxWith(t, ctx, p, "spov-plugin", 12*time.Minute,
		map[string]string{"/root/.config/opencode/plugins/spov-goal.js": source})
	client := &http.Client{Timeout: 0}
	base := creds.URL

	if !waitForRealPlugin(ctx, t, client, base, creds.Token, password) {
		dumpStdout(ctx, t, sb)
		t.Fatal("the spov-goal plugin never loaded")
	}

	const trace = opencode.PluginEvidencePath
	for _, event := range []string{"goal.hooked", "permission.hooked", "command.registered", "event.subscribed"} {
		if got := waitForTraceEventAt(ctx, t, sb, trace, event, 1, 60*time.Second); len(got) == 0 {
			dumpStdout(ctx, t, sb)
			t.Errorf("the plugin never recorded %q, so that primitive did not attach", event)
		}
	}
	// The goal handler attaches to both the context and the compaction
	// hook (DL-039).
	hooked := map[string]bool{}
	for _, e := range waitForTraceEventAt(ctx, t, sb, trace, "goal.hooked", 2, 60*time.Second) {
		hooked[fmt.Sprint(e["hook"])] = true
	}
	if !hooked["context"] || !hooked["compaction"] {
		t.Errorf("the goal handler attached to %v, want both context and compaction", hooked)
	}
	if failed := waitForTraceEventAt(ctx, t, sb, trace, "goal.failed", 1, time.Second); len(failed) > 0 {
		t.Errorf("a goal hook failed to attach: %v", failed)
	}

	// The run opens the way a job does, as a synthetic: a prompt is a
	// human's message, which holds the loop at once (DL-052).
	sessionID := createSession(ctx, t, client, base, creds.Token, password)
	if code, raw := post(ctx, t, client, base+"/api/session/"+sessionID+"/synthetic", creds.Token, password,
		`{"text":"Run the shell command `+"`echo spov-allowed-marker`"+`, then reply with the single word: one.","resume":true}`); code < 200 || code >= 300 {
		t.Fatalf("opening the run: %d %s", code, truncate(raw, 300))
	}

	// TestIntegrationActionVocabulary checks the allowlist names
	// statically; this checks they work in a running session. An
	// allowlist that grants nothing still lets the run finish, so assert
	// that something was allowed.
	allowed := waitForTraceEventAt(ctx, t, sb, trace, "permission.allowed", 1, 6*time.Minute)
	// Whatever denials have accumulated by now, for diagnostics only —
	// this deliberately does not wait, since a run that denies nothing
	// is the expected case.
	denied := deniedSoFar(ctx, t, sb, trace)
	if len(allowed) == 0 {
		dumpStdout(ctx, t, sb)
		for _, d := range denied {
			t.Logf("denied action: %v resources=%v", d["action"], d["resources"])
		}
		t.Fatalf("the policy allowed nothing while denying %d actions: "+
			"PermissionsForKind's action names do not match the vocabulary this build emits", len(denied))
	}
	for _, a := range allowed {
		t.Logf("MEASURED allowed action: %q", a["action"])
	}
	for _, d := range denied {
		t.Logf("MEASURED denied action: %q", d["action"])
	}

	// The loop continues itself: two continuations show it sustains
	// rather than firing once.
	sent := waitForTraceEventAt(ctx, t, sb, trace, "continue.sent", 2, 8*time.Minute)
	t.Logf("MEASURED continuations sent: %d", len(sent))
	if len(sent) < 2 {
		dumpStdout(ctx, t, sb)
		t.Fatalf("the loop sent %d continuations, want 2; it does not sustain", len(sent))
	}

	// Then the loop's own stop ends it, announced as a stop.
	post(ctx, t, client, base+"/api/session/"+sessionID+"/command", creds.Token, password,
		fmt.Sprintf(`{"name":%q,"text":"stop"}`, opencode.PluginCommand))
	stopped := waitForTraceEventAt(ctx, t, sb, trace, "loop.stopped", 1, 4*time.Minute)
	if len(stopped) == 0 {
		t.Error("the loop never recorded stopping when told to")
	} else if reason, _ := stopped[0]["reason"].(string); reason != "stopped" {
		t.Errorf("the loop stopped for %q, want stopped", reason)
	} else {
		t.Logf("MEASURED the loop stopped after %v continuations", stopped[0]["continuations"])
	}

	// Every continuation must be a synthetic. A prompt would be
	// indistinguishable from a human's and would move the job to
	// steered on its own first turn.
	messages := get(ctx, t, client, base+"/api/session/"+sessionID+"/message", creds.Token, password)
	if strings.Contains(messages, `"type":"user"`) && strings.Count(messages, `"type":"user"`) > 1 {
		t.Errorf("more than one user item: a continuation arrived as a prompt\n%s", truncate(messages, 2000))
	}
}

// TestIntegrationPluginSeesTheHumanInbox measures what DL-052 assumed:
// session.inbox.enqueued reaches a server plugin's subscription with the
// item's type, so a human's message holds the loop. The prompt is
// enqueued whether or not a model answers it. It creates one billable
// sandbox and terminates it.
//
// @decision(DL-052)
func TestIntegrationPluginSeesTheHumanInbox(t *testing.T) {
	steerRequirePin(t)
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	t.Logf("opencode build under test: %s (image %s)", testOpenCodeVersion(), testImage())

	source, err := opencode.Plugin{
		Kind: "report", Goal: "Reply with one word.",
		Permissions: opencode.PermissionsForKind("report"),
	}.Source()
	if err != nil {
		t.Fatal(err)
	}
	sb, creds, password := startOpenCodeSandboxWith(t, ctx, p, "spov-hold", 6*time.Minute,
		map[string]string{"/root/.config/opencode/plugins/spov-goal.js": source})
	client := &http.Client{Timeout: 0}

	if !waitForRealPlugin(ctx, t, client, creds.URL, creds.Token, password) {
		dumpStdout(ctx, t, sb)
		t.Fatal("the spov-goal plugin never loaded")
	}
	const trace = opencode.PluginEvidencePath
	if got := waitForTraceEventAt(ctx, t, sb, trace, "event.subscribed", 1, 60*time.Second); len(got) == 0 {
		dumpStdout(ctx, t, sb)
		t.Fatal("the plugin never subscribed to events")
	}

	sessionID := createSession(ctx, t, client, creds.URL, creds.Token, password)
	prompt(ctx, t, client, creds.URL, creds.Token, password, sessionID, "stop")

	held := waitForTraceEventAt(ctx, t, sb, trace, "loop.held", 1, 2*time.Minute)
	if len(held) == 0 {
		dumpStdout(ctx, t, sb)
		t.Fatal("a human prompt did not hold the loop: session.inbox.enqueued does not reach " +
			"the plugin in the shape isHumanInbox reads, and DL-052's hold never fires")
	}
	t.Logf("MEASURED the plugin held its loop on a human prompt: %v", held[0])
}

// waitForRealPlugin waits for the shipped plugin's id rather than the
// probes'.
func waitForRealPlugin(ctx context.Context, t *testing.T, c *http.Client, base, token, password string) bool {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(get(ctx, t, c, base+"/api/plugin", token, password), "spov.goal") {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		time.Sleep(3 * time.Second)
	}
	return false
}

// deniedSoFar reads the denials recorded up to now without waiting.
// Passing n=0 to waitForTraceEventAt would also return immediately, but
// only because len(found) >= 0 is vacuously true, which reads as a wait
// that does not wait.
func deniedSoFar(ctx context.Context, t *testing.T, sb *modal.Sandbox, path string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, e := range readTraceAt(ctx, t, sb, path) {
		if e["event"] == "permission.denied" {
			found = append(found, e)
		}
	}
	return found
}

// waitForTraceEventAt waits for n entries of one kind in a named trace.
func waitForTraceEventAt(ctx context.Context, t *testing.T, sb *modal.Sandbox, path, event string, n int, within time.Duration) []map[string]any {
	t.Helper()
	var found []map[string]any
	waitForTraceFuncAt(ctx, t, sb, path, within, func(entries []map[string]any) bool {
		found = nil
		for _, e := range entries {
			if e["event"] == event {
				found = append(found, e)
			}
		}
		return len(found) >= n
	})
	return found
}

// TestIntegrationActionVocabulary re-reads the permission actions out of
// the OpenCode build under test and fails if an allowlist names one the
// binary never asserts. It downloads the published binary and creates no
// sandbox.
//
// @decision(DL-040)
func TestIntegrationActionVocabulary(t *testing.T) {
	version := testOpenCodeVersion()
	t.Logf("reading the action vocabulary out of OpenCode %s", version)

	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode")
	downloadOpenCodeBinary(t, version, bin)

	blob, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	text := string(blob)

	// Every name an allowlist grants must appear as an action the
	// binary asserts, or it grants nothing at all.
	for _, kind := range []string{"pr", "report"} {
		for _, name := range opencode.PermissionsForKind(kind).Allow {
			if !assertsAction(text, name) {
				t.Errorf("kind %q allows %q, which OpenCode %s does not assert as a permission action",
					kind, name, version)
			}
		}
	}

	// The two that were wrong once, kept as named cases: the shell tool
	// asserts "shell" and not "bash", and there is no separate "write".
	if assertsAction(text, "bash") {
		t.Errorf(`OpenCode %s asserts "bash"; the shell action may have been renamed`, version)
	}
	if assertsAction(text, "write") {
		t.Errorf(`OpenCode %s asserts "write" as a permission; it is no longer covered by "edit" alone`, version)
	}
}

// assertsAction reports whether the binary asserts this action as a
// permission.
//
// It looks only at `assert({action:...})` call sites, never at a bare
// `action:"x"` anywhere in the binary: the minified bundle carries
// unrelated fields of that name — an audio-stream error object has
// `action:"write"` — and matching those reports permissions that do not
// exist. A call site either names the action literally or names a
// variable, so the variable's own binding is resolved.
func assertsAction(text, action string) bool {
	if strings.Contains(text, `assert({action:"`+action+`"`) {
		return true
	}
	for _, v := range assertActionVars(text) {
		if strings.Contains(text, `var `+v+`="`+action+`"`) {
			return true
		}
	}
	return false
}

// assertActionVars returns the variable names that appear as the action
// of an assert call.
var assertActionVarRE = regexp.MustCompile(`assert\(\{action:([A-Za-z_$][A-Za-z0-9_$]*)[,)]`)

func assertActionVars(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range assertActionVarRE.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// downloadOpenCodeBinary fetches the x64 baseline build. A spike
// that cannot get its binary fails rather than skips, so a bump that
// moves the package cannot pass by not running (DL-035).
//
// @decision(DL-035)
func downloadOpenCodeBinary(t *testing.T, version, dest string) {
	t.Helper()
	if err := fetchOpenCodeBinary(version, dest); err != nil {
		t.Fatalf("could not fetch OpenCode %s: %v", version, err)
	}
}

// fetchOpenCodeBinary downloads the x64 package the image installs
// (DL-032), checks it against the recorded integrity when there is one,
// and writes its binary to dest.
func fetchOpenCodeBinary(version, dest string) error {
	rel := ImageSpec{OpenCodeVersion: version}.release()
	const target = "linux-x64-baseline"
	url := "https://registry.npmjs.org/" + rel.Scope + "/cli-" + target + "/-/cli-" + target + "-" + version + ".tgz"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: registry answered %s", url, res.Status)
	}
	tgz, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if want := rel.Integrity[target]; want != "" {
		sum := sha512.Sum512(tgz)
		if got := "sha512-" + base64.StdEncoding.EncodeToString(sum[:]); got != want {
			return fmt.Errorf("%s is %s, and the recorded integrity is %s", url, got, want)
		}
	}
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return err
	}
	defer gz.Close()
	want := "package/bin/" + rel.Binary
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return fmt.Errorf("no %s in %s: %w", want, url, err)
		}
		if h.Name != want {
			continue
		}
		f, err := os.Create(dest)
		if err != nil {
			return err
		}
		//nolint:gosec // the tarball is a pinned published artifact
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	}
}
