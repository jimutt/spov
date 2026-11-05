package cli

import (
	"context"
	"strings"
	"testing"
	"time"
)

// webFixture wires a --web run: the browser opener and the interrupt
// wait are both recorded rather than real.
type webFixture struct {
	*apiFixture
	opened   []string
	waited   int
	waitErr  error
	waitHook func()
}

func newWebFixture(t *testing.T) *webFixture {
	f := &webFixture{apiFixture: newAPIFixture(t)}
	f.env.OpenBrowser = func(_ context.Context, url string) { f.opened = append(f.opened, url) }
	f.env.WaitForInterrupt = func(context.Context) error {
		f.waited++
		if f.waitHook != nil {
			f.waitHook()
		}
		return f.waitErr
	}
	return f
}

// TestFlyWebOpensTheBrowserAndTerminates: the browser gets the loopback
// URL, spov waits, and the interrupt tears the sandbox down like a TUI
// exit would.
//
// @decision(DL-025)
func TestFlyWebOpensTheBrowserAndTerminates(t *testing.T) {
	f := newWebFixture(t)
	_, stderr, err := runWith(t, f.env, "fly", "--web")
	if err != nil {
		t.Fatalf("fly --web: %v\n%s", err, stderr)
	}
	if len(f.opened) != 1 || !strings.HasPrefix(f.opened[0], "http://127.0.0.1:") {
		t.Errorf("opened = %v, want the loopback URL", f.opened)
	}
	if f.waited != 1 {
		t.Errorf("waited %d times", f.waited)
	}
	if f.tuiRuns != 0 {
		t.Error("the TUI ran in web mode")
	}
	if f.provider.Running() != 0 {
		t.Error("the sandbox is still running after the interrupt")
	}
	if !strings.Contains(stderr, "Web UI: http://127.0.0.1:") {
		t.Errorf("the URL was not printed:\n%s", stderr)
	}
}

// TestFlyWebNeverPrintsThePassword: the proxy authenticates; there is
// nothing for the user to copy.
//
// @decision(DL-030)
func TestFlyWebNeverPrintsThePassword(t *testing.T) {
	f := newWebFixture(t)
	_, stderr, err := runWith(t, f.env, "fly", "--web")
	if err != nil {
		t.Fatal(err)
	}
	if pw := f.launchedPassword(); strings.Contains(stderr, pw) {
		t.Error("the password was printed")
	}
	if strings.Contains(stderr, "connect.example") || strings.Contains(stderr, "fake-token") {
		t.Errorf("a connect URL or token was printed:\n%s", stderr)
	}
}

func TestFlyWebKeepsTheSandboxWithKeep(t *testing.T) {
	f := newWebFixture(t)
	_, stderr, err := runWith(t, f.env, "fly", "--web", "--keep", "--name", "w")
	if err != nil {
		t.Fatal(err)
	}
	if f.provider.Running() != 1 || !strings.Contains(stderr, "spov attach w --web") {
		t.Errorf("the sandbox must stay with a hint:\n%s", stderr)
	}
}

func TestFlyWebRejectsNoAttach(t *testing.T) {
	f := newWebFixture(t)
	if _, _, err := runWith(t, f.env, "fly", "--web", "--no-attach"); err == nil {
		t.Fatal("expected the flags to be refused together")
	}
	if len(f.provider.Created) != 0 {
		t.Error("a sandbox was created")
	}
}

// TestFlyWebSurvivesABrowserThatWillNotOpen: the URL is printed, so a
// user with no opener can paste it.
//
// @decision(DL-025)
func TestFlyWebSurvivesABrowserThatWillNotOpen(t *testing.T) {
	f := newWebFixture(t)
	f.env.OpenBrowser = nil
	f.env.LookPath = func(string) (string, error) { return "", context.Canceled }
	f.env.ReadFile = func(string) ([]byte, error) { return []byte("Linux"), nil }
	_, stderr, err := runWith(t, f.env, "fly", "--web")
	if err != nil {
		t.Fatalf("fly --web: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "Web UI: http://127.0.0.1:") {
		t.Errorf("the URL was not printed:\n%s", stderr)
	}
}

func TestAttachWebOpensTheBrowserAndLeavesTheSandbox(t *testing.T) {
	f := newWebFixture(t)
	f.seedSession("s", time.Minute)
	_, stderr, err := runWith(t, f.env, "attach", "s", "--web")
	if err != nil {
		t.Fatalf("attach --web: %v\n%s", err, stderr)
	}
	if len(f.opened) != 1 {
		t.Errorf("opened = %v", f.opened)
	}
	if f.provider.Running() != 1 {
		t.Error("attach --web terminated the sandbox")
	}
}

func TestAttachWebRejectsNoTUI(t *testing.T) {
	f := newWebFixture(t)
	if _, _, err := runWith(t, f.env, "attach", "s", "--web", "--no-tui"); err == nil {
		t.Fatal("expected the flags to be refused together")
	}
}

// TestNoPublicURLIsEverPrinted: nothing in any output names the edge
// endpoint or a token.
//
// @decision(DL-030)
func TestNoPublicURLIsEverPrinted(t *testing.T) {
	f := newWebFixture(t)
	f.seedSession("s", time.Minute)
	for _, args := range [][]string{{"fly", "--keep"}, {"attach", "s", "--no-tui"}, {"attach", "s", "--web"}, {"ls"}} {
		out, stderr, err := runWith(t, f.env, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, stderr)
		}
		for _, leak := range []string{"connect.example", "fake-token", "_modal_connect_token"} {
			if strings.Contains(out+stderr, leak) {
				t.Errorf("%v printed %q:\n%s%s", args, leak, out, stderr)
			}
		}
	}
}
