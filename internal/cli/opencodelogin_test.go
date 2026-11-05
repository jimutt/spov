package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/console"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
)

// consoleCLIFixture is a local server whose OpenCode sign-ins go to a
// fake console and are kept in this machine's credential store.
func consoleCLIFixture(t *testing.T) (*apiFixture, *console.Fake) {
	t.Helper()
	fake := console.NewFake()
	t.Cleanup(fake.Close)
	f := newAPIFixture(t)
	f.http.Close()
	f.srv.Close()
	// The server's clock moves only when the CLI waits, so each check
	// is one poll of the console.
	var mu sync.Mutex
	now := time.Now().UTC()
	f.adjustDeps = func(d *server.Deps) {
		d.Console = &console.Client{BaseURL: fake.URL, UserAgent: "spov/test"}
		d.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	}
	f.startServer()
	f.env.Client = client.New(f.http.URL, f.token)
	old := consoleWait
	t.Cleanup(func() { consoleWait = old })
	consoleWait = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
		return nil
	}
	return f, fake
}

// onWait runs fn at every wait, after the clock moves.
func onWait(fn func()) {
	inner := consoleWait
	consoleWait = func(ctx context.Context, d time.Duration) error {
		fn()
		return inner(ctx, d)
	}
}

// `spov login opencode` shows the code, waits for approval, and leaves
// the grant in the local credential store, never on screen.
//
// @decision(DL-015)
func TestLoginOpenCodeConnectsThroughTheServer(t *testing.T) {
	f, fake := consoleCLIFixture(t)
	onWait(func() { fake.Approve("UC-0002") })
	out, errOut, err := runWith(t, f.env, "login", "opencode", "--no-browser")
	if err != nil {
		t.Fatalf("login opencode: %v\n%s", err, errOut)
	}
	if !strings.Contains(errOut, "UC-0002") || !strings.Contains(errOut, fake.URL+"/console/device") {
		t.Errorf("the code or the URL was not shown:\n%s", errOut)
	}
	if !strings.Contains(out, "Connected OpenCode") {
		t.Errorf("out = %q", out)
	}
	view, err := f.env.Client.OpenCodeConnection(context.Background())
	if err != nil || !view.Connected {
		t.Fatalf("connection = %+v, %v", view, err)
	}
	for _, s := range []string{out, errOut} {
		if strings.Contains(s, "st_") || strings.Contains(s, "rt_") || strings.Contains(s, "dc_") {
			t.Errorf("a secret reached the terminal: %q", s)
		}
	}

	out, _, err = runWith(t, f.env, "logout", "opencode")
	if err != nil || !strings.Contains(out, "Disconnected OpenCode") {
		t.Fatalf("logout opencode: %q, %v", out, err)
	}
	if view, _ := f.env.Client.OpenCodeConnection(context.Background()); view.Connected {
		t.Error("still connected after logout")
	}
}

// A denied sign-in is an error that says so.
//
// @decision(DL-015)
func TestLoginOpenCodeReportsADenial(t *testing.T) {
	f, fake := consoleCLIFixture(t)
	onWait(func() { fake.Deny("UC-0002") })
	_, _, err := runWith(t, f.env, "login", "opencode", "--no-browser")
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := runWith(t, f.env, "login", "opencode", "--name", "x"); err == nil {
		t.Error("--name, which names a server token, was accepted for OpenCode")
	}
}

// `spov credentials` says whether OpenCode is connected, on a local
// server whose machine stores no other credentials, and follows a logout.
//
// @decision(DL-015) @decision(DL-008)
func TestTheCredentialsListingShowsTheOpenCodeSignIn(t *testing.T) {
	f, fake := consoleCLIFixture(t)
	f.env.Credentials = credentials.NewMemoryStore()
	out, _, err := runWith(t, f.env, "credentials")
	if err != nil || !strings.Contains(out, "No credentials stored on this machine") ||
		!strings.Contains(out, "OpenCode: not connected (`spov login opencode`)") {
		t.Fatalf("before login: %q, %v", out, err)
	}
	onWait(func() { fake.Approve("UC-0002") })
	if _, errOut, err := runWith(t, f.env, "login", "opencode", "--no-browser"); err != nil {
		t.Fatalf("login opencode: %v\n%s", err, errOut)
	}
	out, _, err = runWith(t, f.env, "credentials")
	if err != nil || !strings.Contains(out, "OpenCode: connected  (organization org_1)  since ") {
		t.Fatalf("after login: %q, %v", out, err)
	}
	if strings.Contains(out, "st_") || strings.Contains(out, "rt_") {
		t.Errorf("the listing shows a token: %q", out)
	}
	if _, _, err := runWith(t, f.env, "logout", "opencode"); err != nil {
		t.Fatal(err)
	}
	if out, _, _ = runWith(t, f.env, "credentials"); !strings.Contains(out, "OpenCode: not connected") {
		t.Errorf("after logout: %q", out)
	}
}

// @decision(DL-015)
func TestTheOpenCodeLineSaysWhenToReconnect(t *testing.T) {
	cases := map[string]server.OpenCodeConnectionView{
		"":                        {},
		"refused by OpenCode":     {Enabled: true, NeedsReconnect: true},
		"OpenCode: connected":     {Enabled: true, Connected: true},
		"OpenCode: not connected": {Enabled: true},
	}
	for want, v := range cases {
		got := openCodeLine(v)
		if want == "" && got != "" || !strings.Contains(got, want) {
			t.Errorf("openCodeLine(%+v) = %q, want it to contain %q", v, got, want)
		}
	}
}
