package cli

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/server"
)

// sandbox.env, a project file's over the user file's by name, and --env
// over both, reach the session.
//
// @decision(DL-096)
func TestFlySendsTheMergedVariables(t *testing.T) {
	f := newAPIFixtureWithConfig(t, defaultConfig+"sandbox:\n  env:\n    NODE_ENV: production\n    LOCALE: sv_SE\n")
	writeProject(t, f.wd, "sandbox:\n  env:\n    NODE_ENV: development\n    FEATURE: \"on\"\n")
	if _, stderr, err := runWith(t, f.env, "fly", "--keep", "--name", "env", "--env", "FEATURE=off", "--env", "EMPTY="); err != nil {
		t.Fatalf("fly: %v\n%s", err, stderr)
	}
	out, _, err := runWith(t, f.env, "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Sessions []server.SessionView `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"NODE_ENV": "development", "LOCALE": "sv_SE", "FEATURE": "off", "EMPTY": ""}
	if len(body.Sessions) != 1 || !maps.Equal(body.Sessions[0].Env, want) {
		t.Errorf("env = %+v, want %v", body.Sessions, want)
	}
}

// A refused variable or a malformed --env stops the launch before the
// server is asked.
//
// @decision(DL-097)
func TestFlyRefusesABadVariableBeforeLaunching(t *testing.T) {
	// A project file, which the local server never reads: a user file's
	// bad sandbox.env stops the server itself from starting.
	cases := []struct {
		name, project string
		args          []string
		want          string
	}{
		{"a refused name in a project file", "sandbox:\n  env:\n    GITHUB_TOKEN: x\n", nil, `"GITHUB_TOKEN"`},
		{"a refused name in a flag", "", []string{"--env", "HTTPS_PROXY=http://x"}, `"HTTPS_PROXY"`},
		{"a flag with no =", "", []string{"--env", "NODE_ENV"}, "KEY=VALUE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAPIFixtureWithConfig(t, defaultConfig)
			if tc.project != "" {
				writeProject(t, f.wd, tc.project)
			}
			_, _, err := runWith(t, f.env, append([]string{"fly", "--keep", "--name", "bad"}, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one naming %s", err, tc.want)
			}
			if f.provider.Running() != 0 {
				t.Errorf("a sandbox was created")
			}
		})
	}
}

// The server takes its own sandbox.env as the base, and will not start
// on one that DL-097 refuses.
//
// @decision(DL-096) @decision(DL-097)
func TestTheServersOwnVariables(t *testing.T) {
	deps := func(t *testing.T, body string) (server.Deps, error) {
		t.Helper()
		cfg := userConfigEnv(t, defaultConfig+body).Vars["XDG_CONFIG_HOME"]
		env := &Env{
			Environ:      config.MapEnv{Vars: map[string]string{"XDG_CONFIG_HOME": cfg, "USER": "alice"}},
			Workdir:      t.TempDir(),
			ServerConfig: true,
		}
		res, err := env.config()
		if err != nil {
			t.Fatal(err)
		}
		d, _, err := env.serverDeps(context.Background(), res.Config, sandbox.NewFake(), "tok", false)
		return d, err
	}
	d, err := deps(t, "sandbox:\n  env:\n    NODE_ENV: test\n")
	if err != nil || d.Defaults.Env["NODE_ENV"] != "test" {
		t.Errorf("defaults env = %v, err %v", d.Defaults.Env, err)
	}
	if _, err := deps(t, "sandbox:\n  env:\n    SSL_CERT_FILE: /x\n"); err == nil || !strings.Contains(err.Error(), "SSL_CERT_FILE") {
		t.Errorf("a refused variable of the server's own: %v", err)
	}
	// An add-on's variable is refused even when the server's own add-ons
	// do not include it, since a request may name that add-on.
	if _, err := deps(t, "sandbox:\n  env:\n    PLAYWRIGHT_BROWSERS_PATH: /x\n"); err == nil || !strings.Contains(err.Error(), "PLAYWRIGHT_BROWSERS_PATH") {
		t.Errorf("an add-on's variable in the server's own: %v", err)
	}
}
