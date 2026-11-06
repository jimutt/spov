package opencode

import (
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/credproxy"
)

// Every rule of DL-097, one case each, and the names that stay allowed.
//
// @decision(DL-097)
func TestCheckUserEnv(t *testing.T) {
	spov := SpovEnvNames(nil, []string{"ANTHROPIC_API_KEY"})
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"ordinary settings", map[string]string{"NODE_ENV": "test", "DATABASE_URL": "postgres://db/test", "_X": ""}, ""},
		{"an OpenCode flag", map[string]string{"OPENCODE_LOG_LEVEL": "debug", "OPENCODE_DISABLE_AUTOUPDATE": "1"}, ""},
		{"a name spov sets", map[string]string{"PATH": "/bin"}, "spov sets itself"},
		{"a name spov sets, in another case", map[string]string{"path": "/bin"}, "spov sets itself"},
		{"a provider key", map[string]string{"ANTHROPIC_API_KEY": "k"}, "spov sets itself"},
		{"the proxy's", map[string]string{"https_proxy": "http://elsewhere"}, "spov sets itself"},
		{"the proxy's CA", map[string]string{credproxy.EnvMainCA: "x"}, "spov sets itself"},
		{"spov's prefix", map[string]string{"SPOV_ANYTHING": "x"}, "starts with SPOV_"},
		{"git's prefix", map[string]string{"GIT_SSH_COMMAND": "x"}, "starts with GIT_"},
		{"where configs live", map[string]string{"XDG_CONFIG_HOME": "/x"}, "starts with XDG_"},
		{"mise's own", map[string]string{"MISE_DATA_DIR": "/x"}, "starts with MISE_"},
		{"a JVM's options", map[string]string{"_JAVA_OPTIONS": "-Dx"}, "keeps for itself"},
		{"the loader's prefix", map[string]string{"ld_preload": "x"}, "starts with LD_"},
		{"a shell variable", map[string]string{"BASH_ENV": "/x"}, "keeps for itself"},
		{"a trust store", map[string]string{"CURL_CA_BUNDLE": "/x"}, "keeps for itself"},
		{"a forge token", map[string]string{"GITHUB_TOKEN": "x"}, "routed credential"},
		{"the Console key", map[string]string{"OPENCODE_API_KEY": "x"}, "routed credential"},
		{"OpenCode's config", map[string]string{"OPENCODE_CONFIG_CONTENT": "{}"}, "keeps for itself"},
		{"OpenCode's plumbing", map[string]string{"OPENCODE_PTY_SOCKET_DIR": "/x"}, "starts with OPENCODE_PTY_"},
		{"not a name", map[string]string{"1ABC": "x"}, "not a variable name"},
		{"a dash", map[string]string{"MY-VAR": "x"}, "not a variable name"},
		{"a long name", map[string]string{strings.Repeat("A", MaxUserEnvName+1): "x"}, "name over"},
		{"a long value", map[string]string{"BIG": strings.Repeat("v", MaxUserEnvValue+1)}, "value over"},
		{"a NUL byte", map[string]string{"NUL": "a\x00b"}, "NUL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckUserEnv(tc.env, spov)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("error %v, want one saying %q", err, tc.want)
			}
		})
	}

	t.Run("too many", func(t *testing.T) {
		env := map[string]string{}
		for i := range MaxUserEnvVars + 1 {
			env["V"+strings.Repeat("A", i)] = "x"
		}
		if err := CheckUserEnv(env, spov); err == nil || !strings.Contains(err.Error(), "the limit is") {
			t.Errorf("error %v", err)
		}
	})
	t.Run("too much in all", func(t *testing.T) {
		env := map[string]string{}
		for i := range 9 {
			env["V"+strings.Repeat("A", i)] = strings.Repeat("v", MaxUserEnvValue)
		}
		if err := CheckUserEnv(env, spov); err == nil || !strings.Contains(err.Error(), "bytes; the limit") {
			t.Errorf("error %v", err)
		}
	})
}

// The launch's own check covers everything the launch set, the secrets
// included, and adds the rest to the plain environment.
//
// @decision(DL-096) @decision(DL-097)
func TestWithUserEnvRefusesWhatTheLaunchSet(t *testing.T) {
	env := map[string]string{"HOME": "/root"}
	secrets := map[string]string{"CUSTOM_PLACEHOLDER": "spov-placeholder"}
	if err := WithUserEnv(env, secrets, map[string]string{"custom_placeholder": "x"}); err == nil {
		t.Error("a variable the secrets hold was accepted")
	}
	if err := WithUserEnv(env, secrets, map[string]string{"NODE_ENV": "test"}); err != nil {
		t.Fatal(err)
	}
	if env["NODE_ENV"] != "test" || secrets["NODE_ENV"] != "" {
		t.Errorf("env %v, secrets %v", env, secrets)
	}
}
