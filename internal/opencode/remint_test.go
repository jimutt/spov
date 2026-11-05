package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// runRefresh runs the refresh script against a temporary home, with the
// token on standard input as the server sends it.
func runRefresh(t *testing.T, home, host, user, gh, token string) {
	t.Helper()
	cmd := exec.Command("bash", "-c", refreshScript(home), "spov-refresh", host, user, gh)
	cmd.Stdin = strings.NewReader(token + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("refresh %s: %v\n%s", host, err, out)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A refresh replaces one host's token in the credential store and in
// gh's hosts.yml, and leaves every other host's alone.
//
// @decision(DL-021)
func TestARefreshReplacesOnlyItsOwnHost(t *testing.T) {
	home := t.TempDir()
	creds := filepath.Join(home, ".git-credentials")
	if err := os.WriteFile(creds, []byte(
		"https://alice:forgejo-token@codefloe.com\n"+
			"https://x-access-token:ghs_old@github.com\n"+
			"https://x-access-token:ghs_other@github.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRefresh(t, home, "github.example.com", "x-access-token", "1", "ghs_enterprise")
	runRefresh(t, home, "github.com", "x-access-token", "1", "ghs_new")

	lines := strings.Split(strings.TrimSpace(readFile(t, creds)), "\n")
	slices.Sort(lines)
	want := []string{
		"https://alice:forgejo-token@codefloe.com",
		"https://x-access-token:ghs_enterprise@github.example.com",
		"https://x-access-token:ghs_new@github.com",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("credential store:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}

	hosts := readFile(t, filepath.Join(home, ".config/gh/hosts.yml"))
	for _, want := range []string{"github.com:\n", "github.example.com:\n", "oauth_token: ghs_new\n", "oauth_token: ghs_enterprise\n"} {
		if !strings.Contains(hosts, want) {
			t.Errorf("hosts.yml lacks %q:\n%s", want, hosts)
		}
	}
	if strings.Contains(hosts, "ghs_old") {
		t.Errorf("hosts.yml still holds the old token:\n%s", hosts)
	}
	for _, name := range []string{".git-credentials", ".config/gh/hosts.yml", ".config/gh/config.yml"} {
		info, err := os.Stat(filepath.Join(home, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v, want 0600", name, info.Mode().Perm())
		}
	}
	if strings.Count(readFile(t, filepath.Join(home, ".config/gh/config.yml")), "version:") != 1 {
		t.Error("config.yml gained a second version line")
	}
}

// Without gh for the host, a refresh leaves gh's configuration alone.
//
// @decision(DL-021)
func TestARefreshWithoutGhTouchesOnlyTheCredentialStore(t *testing.T) {
	home := t.TempDir()
	runRefresh(t, home, "github.com", "x-access-token", "0", "ghs_new")
	if got := readFile(t, filepath.Join(home, ".git-credentials")); got != "https://x-access-token:ghs_new@github.com\n" {
		t.Errorf("credential store = %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/gh")); !os.IsNotExist(err) {
		t.Errorf("gh's configuration was written: %v", err)
	}
}

// A refresh with nothing on standard input fails rather than writing an
// empty token over a working one.
//
// @decision(DL-021)
func TestARefreshWithNoTokenChangesNothing(t *testing.T) {
	home := t.TempDir()
	creds := filepath.Join(home, ".git-credentials")
	if err := os.WriteFile(creds, []byte("https://x-access-token:ghs_old@github.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", refreshScript(home), "spov-refresh", "github.com", "x-access-token", "1")
	cmd.Stdin = strings.NewReader("")
	if err := cmd.Run(); err == nil {
		t.Fatal("a refresh with no token succeeded")
	}
	if got := readFile(t, creds); !strings.Contains(got, "ghs_old") {
		t.Errorf("the credential store changed: %q", got)
	}
}

// The token reaches a refresh on standard input only.
//
// @decision(DL-021)
func TestTheRefreshArgvCarriesNoToken(t *testing.T) {
	args := RefreshArgs("github.com", "x-access-token", true)
	if args[len(args)-3] != "github.com" || args[len(args)-2] != "x-access-token" || args[len(args)-1] != "1" {
		t.Errorf("argv tail = %v", args[len(args)-3:])
	}
	if !strings.Contains(args[2], "read -r token") {
		t.Error("the script does not read its token from standard input")
	}
}

// The entrypoint writes gh's configuration in the same shape a refresh
// does, the multi-account format measured in spike S-R2, and exports no
// GH_TOKEN, which would override the file.
//
// @decision(DL-021) @decision(DL-032)
func TestTheEntrypointAndARefreshWriteTheSameGhConfig(t *testing.T) {
	fromEntrypoint := t.TempDir()
	var b strings.Builder
	b.WriteString("set -eu\n")
	writeGhLogin(&b, fromEntrypoint, shellQuote("github.com"), shellQuote("x-access-token"), `"$SPOV_GIT_TOKEN_GH"`)
	cmd := exec.Command("bash", "-c", b.String())
	cmd.Env = append(os.Environ(), "SPOV_GIT_TOKEN_GH=ghs_same")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	fromRefresh := t.TempDir()
	runRefresh(t, fromRefresh, "github.com", "x-access-token", "1", "ghs_same")

	for _, name := range []string{".config/gh/hosts.yml", ".config/gh/config.yml"} {
		if a, b := readFile(t, filepath.Join(fromEntrypoint, name)), readFile(t, filepath.Join(fromRefresh, name)); a != b {
			t.Errorf("%s differs:\nentrypoint:\n%s\nrefresh:\n%s", name, a, b)
		}
	}
	want := "github.com:\n    users:\n        x-access-token:\n            oauth_token: ghs_same\n" +
		"    git_protocol: https\n    user: x-access-token\n    oauth_token: ghs_same\n"
	if got := readFile(t, filepath.Join(fromRefresh, ".config/gh/hosts.yml")); got != want {
		t.Errorf("hosts.yml is not the measured format:\n%s", got)
	}

	script, err := Entrypoint{
		Clones:  []Clone{{URL: "https://github.com/o/r.git", Dir: "r"}},
		Landing: "r",
		Port:    DefaultPort,
		Logins:  []CLILogin{{CLI: CLIGh, Name: "gh", URL: "https://github.com", User: "x-access-token", TokenEnv: "SPOV_GIT_TOKEN_GH"}},
	}.Script()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "GH_TOKEN=") {
		t.Error("the entrypoint exports GH_TOKEN, which overrides the file a refresh rewrites")
	}
	if !strings.Contains(script, GhConfigDir+"/hosts.d") {
		t.Errorf("the entrypoint does not write gh's per-host configuration:\n%s", script)
	}
}

// gh's configuration holds tokens, so a job's agent is steered away from it.
//
// @decision(DL-021) @decision(DL-040)
func TestGhConfigurationIsDenied(t *testing.T) {
	for _, kind := range []string{"pr", "report"} {
		if !slices.Contains(PermissionsForKind(kind).Deny, GhConfigDir) {
			t.Errorf("%s jobs may read %s", kind, GhConfigDir)
		}
	}
}

// glab is configured from a file the agent may not read: written whole
// over any default config glab left, the same on a second run, one host
// entry per GitLab platform holding the placeholder, the first host the
// default, the update check, telemetry and banner off, mode 0600.
//
// @decision(DL-032)
func TestGlabIsConfiguredFromAFile(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config/glab-cli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// What glab writes on its first run, in part.
	stale := "check_update: true\ntelemetry: true\ngit_protocol: ssh\nhost: gitlab.com\nhosts:\n    gitlab.com:\n        token:\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("set -eu\n")
	hosts := []glabHost{
		{host: "gitlab.com", tokenExpr: `"$SPOV_GIT_TOKEN_GL"`},
		{host: "git.example.com", tokenExpr: `"$SPOV_GIT_TOKEN_SELF"`, subfolder: "gitlab"},
	}
	writeGlabConfig(&b, home, hosts)
	writeGlabConfig(&b, home, hosts)
	cmd := exec.Command("bash", "-c", b.String())
	cmd.Env = append(os.Environ(), "SPOV_GIT_TOKEN_GL=glpat-spovplaceholder01", "SPOV_GIT_TOKEN_SELF=glpat-spovplaceholder02")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	path := filepath.Join(dir, "config.yml")
	var cfg struct {
		CheckUpdate  bool   `yaml:"check_update"`
		Telemetry    bool   `yaml:"telemetry"`
		ShowWhatsNew bool   `yaml:"show_whats_new"`
		GitProtocol  string `yaml:"git_protocol"`
		Host         string `yaml:"host"`
		Hosts        map[string]struct {
			Token       string `yaml:"token"`
			APIHost     string `yaml:"api_host"`
			APIProtocol string `yaml:"api_protocol"`
			Subfolder   string `yaml:"subfolder"`
		} `yaml:"hosts"`
	}
	raw := readFile(t, path)
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("config.yml is not YAML: %v\n%s", err, raw)
	}
	if cfg.CheckUpdate || cfg.Telemetry || cfg.ShowWhatsNew || cfg.GitProtocol != "https" || cfg.Host != "gitlab.com" || len(cfg.Hosts) != 2 {
		t.Errorf("config:\n%s", raw)
	}
	if h := cfg.Hosts["gitlab.com"]; h.Token != "glpat-spovplaceholder01" || h.APIHost != "gitlab.com" || h.APIProtocol != "https" || h.Subfolder != "" {
		t.Errorf("gitlab.com = %+v", h)
	}
	if h := cfg.Hosts["git.example.com"]; h.Token != "glpat-spovplaceholder02" || h.Subfolder != "gitlab" {
		t.Errorf("git.example.com = %+v", h)
	}
	if strings.Count(raw, "'gitlab.com':") != 1 {
		t.Errorf("a host is listed twice:\n%s", raw)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v", info.Mode().Perm(), err)
	}
	for _, kind := range []string{"pr", "report"} {
		if !slices.Contains(PermissionsForKind(kind).Deny, GlabConfigDir) {
			t.Errorf("%s jobs may read %s", kind, GlabConfigDir)
		}
	}
	script, err := Entrypoint{
		Clones:  []Clone{{URL: "https://gitlab.com/g/s/r.git", Dir: "r"}},
		Landing: "r",
		Port:    DefaultPort,
		Logins:  []CLILogin{{CLI: CLIGlab, Name: "gl", URL: "https://gitlab.com", User: "oauth2", TokenEnv: "SPOV_GIT_TOKEN_GL"}},
	}.Script()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "GITLAB_TOKEN") || !strings.Contains(script, GlabConfigDir) {
		t.Errorf("the entrypoint does not configure glab from its file:\n%s", script)
	}
}
