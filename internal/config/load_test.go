package config

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// newFixture creates a home directory holding a user config and a separate
// working directory holding a project config. Either body may be empty to
// skip writing that file.
func newFixture(t *testing.T, userBody, projectBody string) (env MapEnv, workdir string) {
	t.Helper()
	home := t.TempDir()
	workdir = t.TempDir()

	if userBody != "" {
		dir := filepath.Join(home, ".config", "spov")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, UserFileName), []byte(userBody), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if projectBody != "" {
		if err := os.WriteFile(filepath.Join(workdir, ProjectFileName), []byte(projectBody), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return MapEnv{Home: home, Vars: map[string]string{}}, workdir
}

func TestLoadDefaultsOnly(t *testing.T) {
	env, wd := newFixture(t, "", "")
	res, err := Load(env, wd)
	if err != nil {
		t.Fatal(err)
	}
	want := Defaults()
	if !reflect.DeepEqual(res.Config.Sandbox, want.Sandbox) {
		t.Errorf("sandbox = %+v, want %+v", res.Config.Sandbox, want.Sandbox)
	}
	if res.Config.OpenCode != want.OpenCode {
		t.Errorf("opencode = %+v, want %+v", res.Config.OpenCode, want.OpenCode)
	}
	if res.Sources.UserFile != "" || res.Sources.ProjectFile != "" || len(res.Sources.EnvVars) != 0 {
		t.Errorf("sources = %+v, want empty", res.Sources)
	}
}

func TestLoadLayerPrecedence(t *testing.T) {
	userBody := `
version: 1
modal:
  environment: user-env
  app: user-app
sandbox:
  cpu: 1
  memory_mib: 4096
  timeout: 6h
opencode:
  port: 5000
`
	projectBody := `
sandbox:
  cpu: 2
  timeout: 8h
`
	env, wd := newFixture(t, userBody, projectBody)
	env.Vars["SPOV_SANDBOX_CPU"] = "4"
	env.Vars["SPOV_MODAL_ENVIRONMENT"] = "env-env"

	res, err := Load(env, wd)
	if err != nil {
		t.Fatal(err)
	}

	// env beats project beats user beats defaults.
	if res.Config.Sandbox.CPU != 4 {
		t.Errorf("cpu = %v, want 4 (environment layer)", res.Config.Sandbox.CPU)
	}
	if res.Config.Sandbox.Timeout != 8*time.Hour {
		t.Errorf("timeout = %v, want 8h (project layer)", res.Config.Sandbox.Timeout)
	}
	if res.Config.Sandbox.MemoryMiB != 4096 {
		t.Errorf("memory_mib = %v, want 4096 (user layer)", res.Config.Sandbox.MemoryMiB)
	}
	if res.Config.Sandbox.IdleTimeout != 30*time.Minute {
		t.Errorf("idle_timeout = %v, want the default", res.Config.Sandbox.IdleTimeout)
	}
	if res.Config.Modal.Environment != "env-env" {
		t.Errorf("modal.environment = %q, want env-env", res.Config.Modal.Environment)
	}
	if res.Config.Modal.App != "user-app" {
		t.Errorf("modal.app = %q, want user-app", res.Config.Modal.App)
	}
	if res.Config.OpenCode.Port != 5000 {
		t.Errorf("opencode.port = %d, want 5000", res.Config.OpenCode.Port)
	}
}

func TestLoadSourcesRecorded(t *testing.T) {
	env, wd := newFixture(t, "version: 1\n", "sandbox:\n  cpu: 1\n")
	env.Vars["SPOV_SANDBOX_CPU"] = "2"
	env.Vars["PATH"] = "/usr/bin" // wrong prefix: must not be recorded
	env.Vars["SPOV_"] = "empty"   // prefix with no key: must be ignored

	res, err := Load(env, wd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(res.Sources.UserFile, filepath.Join("spov", UserFileName)) {
		t.Errorf("user file = %q", res.Sources.UserFile)
	}
	if !strings.HasSuffix(res.Sources.ProjectFile, ProjectFileName) {
		t.Errorf("project file = %q", res.Sources.ProjectFile)
	}
	if len(res.Sources.EnvVars) != 1 || res.Sources.EnvVars[0] != "SPOV_SANDBOX_CPU" {
		t.Errorf("env vars = %v, want [SPOV_SANDBOX_CPU]", res.Sources.EnvVars)
	}
}

func TestLoadProjectFileFromAncestor(t *testing.T) {
	env, wd := newFixture(t, "platforms:\n  - name: cf\n    kind: forgejo\n    base_url: https://codefloe.com\n",
		"landing: spov\nrepos:\n  - platform: cf\n    owner: alice\n    name: spov\n")
	nested := filepath.Join(wd, "sub", "dir")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Load(env, nested)
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Landing != "spov" {
		t.Errorf("landing = %q, want spov", res.Config.Landing)
	}
	if len(res.Config.Repos) != 1 {
		t.Fatalf("repos = %+v", res.Config.Repos)
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	env, wd := newFixture(t, "", "sandbox: [this is not a map\n")
	_, err := Load(env, wd)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), ProjectFileName) {
		t.Errorf("error %q does not name the offending file", err)
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	env, wd := newFixture(t, "", "sandbox:\n  timeout: 24h\n")
	_, err := Load(env, wd)
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if !strings.Contains(err.Error(), "maximum") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadUndecodableValue(t *testing.T) {
	env, wd := newFixture(t, "", "sandbox:\n  timeout: not-a-duration\n")
	_, err := Load(env, wd)
	if err == nil {
		t.Fatal("expected a decode error")
	}
	if !strings.Contains(err.Error(), "not a duration") {
		t.Errorf("unexpected error: %v", err)
	}
	// Decode errors must read as one line, not mapstructure's multi-line block.
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error spans multiple lines: %q", err)
	}
}

func TestLoadWithoutHomeStillReadsProjectAndEnv(t *testing.T) {
	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, ProjectFileName), []byte("sandbox:\n  cpu: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := MapEnv{Vars: map[string]string{"SPOV_OPENCODE_PORT": "7000"}}
	res, err := Load(env, wd)
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Sandbox.CPU != 3 {
		t.Errorf("cpu = %v, want 3", res.Config.Sandbox.CPU)
	}
	if res.Config.OpenCode.Port != 7000 {
		t.Errorf("port = %d, want 7000", res.Config.OpenCode.Port)
	}
}

func TestLoadXDGConfigHome(t *testing.T) {
	xdg := t.TempDir()
	dir := filepath.Join(xdg, "spov")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, UserFileName), []byte("modal:\n  app: from-xdg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := MapEnv{Vars: map[string]string{"XDG_CONFIG_HOME": xdg}}
	res, err := Load(env, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Modal.App != "from-xdg" {
		t.Errorf("app = %q, want from-xdg", res.Config.Modal.App)
	}
}

func TestEnvKey(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"section field", "SPOV_SANDBOX_CPU", "sandbox.cpu"},
		{"multi-word field", "SPOV_SANDBOX_MEMORY_MIB", "sandbox.memory_mib"},
		{"duration field", "SPOV_SANDBOX_IDLE_TIMEOUT", "sandbox.idle_timeout"},
		{"modal section", "SPOV_MODAL_ENVIRONMENT", "modal.environment"},
		{"opencode section", "SPOV_OPENCODE_VERSION", "opencode.version"},
		{"top level", "SPOV_LANDING", "landing"},
		{"top level version", "SPOV_VERSION", "version"},

		{"unknown field in a known section", "SPOV_SANDBOX_CPUU", ""},
		{"unknown section", "SPOV_TOTALLY_BOGUS", ""},
		{"bare section name", "SPOV_SANDBOX", ""},
		{"list-valued key", "SPOV_PLATFORMS", ""},
		{"indexed list key", "SPOV_PLATFORMS_0_NAME", ""},
		{"prefix only", "SPOV_", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := envKey(tt.in); got != tt.want {
				t.Errorf("envKey(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestEnvKeysMatchStruct guards the env allowlist against drift: every
// scalar field in Config must be addressable, and no list may be.
func TestEnvKeysMatchStruct(t *testing.T) {
	for _, key := range []string{
		"version", "landing",
		"modal.environment", "modal.app",
		"sandbox.cpu", "sandbox.memory_mib", "sandbox.timeout",
		"sandbox.idle_timeout", "sandbox.image",
		"opencode.version", "opencode.port",
	} {
		if !envKeys[key] {
			t.Errorf("%q is not addressable through the environment", key)
		}
	}
	for _, key := range []string{"platforms", "repos"} {
		if envKeys[key] {
			t.Errorf("%q must not be addressable through the environment", key)
		}
	}
}

func TestLoadIgnoresUnknownEnvVars(t *testing.T) {
	env, wd := newFixture(t, "", "")
	env.Vars["SPOV_SANDBOX_CPU"] = "3"
	env.Vars["SPOV_SANDBOX_CPUU"] = "9"
	env.Vars["SPOV_PLATFORMS"] = "nope"
	env.Vars["SPOV_SANDBOX"] = "nope"

	res, err := Load(env, wd)
	if err != nil {
		t.Fatalf("unknown variables must not break the load: %v", err)
	}
	if res.Config.Sandbox.CPU != 3 {
		t.Errorf("cpu = %v, want 3", res.Config.Sandbox.CPU)
	}
	if res.Config.Sandbox.MemoryMiB != Defaults().Sandbox.MemoryMiB {
		t.Errorf("a bare section name clobbered the sandbox section: %+v", res.Config.Sandbox)
	}
	if len(res.Sources.EnvVars) != 1 || res.Sources.EnvVars[0] != "SPOV_SANDBOX_CPU" {
		t.Errorf("EnvVars = %v, want only the variable that applied", res.Sources.EnvVars)
	}
	want := map[string]bool{"SPOV_SANDBOX_CPUU": true, "SPOV_PLATFORMS": true, "SPOV_SANDBOX": true}
	if len(res.Sources.IgnoredEnvVars) != len(want) {
		t.Fatalf("IgnoredEnvVars = %v, want %v", res.Sources.IgnoredEnvVars, want)
	}
	for _, got := range res.Sources.IgnoredEnvVars {
		if !want[got] {
			t.Errorf("unexpected ignored variable %q", got)
		}
	}
}

func TestLoadRejectsNumericDuration(t *testing.T) {
	env, wd := newFixture(t, "", "sandbox:\n  timeout: 3600\n")
	_, err := Load(env, wd)
	if err == nil {
		t.Fatal("a bare number must not be accepted as a duration")
	}
	if !strings.Contains(err.Error(), "need a unit") {
		t.Errorf("error %q does not explain the missing unit", err)
	}
}

func TestLoadRejectsUnparsableDuration(t *testing.T) {
	env, wd := newFixture(t, "", "sandbox:\n  timeout: soon\n")
	_, err := Load(env, wd)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "is not a duration") {
		t.Errorf("error %q does not explain the bad duration", err)
	}
}

// TestLoadListsAreReplacedNotMerged documents that a later layer replaces a
// list wholesale rather than appending to it.
func TestLoadListsAreReplacedNotMerged(t *testing.T) {
	userBody := `platforms:
  - name: c
    kind: github
repos:
  - platform: c
    owner: o
    name: a
  - platform: c
    owner: o
    name: b
`
	projectBody := `repos:
  - platform: c
    owner: o
    name: r
`
	env, wd := newFixture(t, userBody, projectBody)
	res, err := Load(env, wd)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Config.Repos) != 1 || res.Config.Repos[0].Name != "r" {
		t.Errorf("repos = %+v, want only the project layer's list", res.Config.Repos)
	}
}

// A project file may not say where a credential goes: it names the key
// and where it belongs, whatever the value, and the user file's own
// value is untouched by a project that leaves the key alone.
//
// @decision(DL-026)
func TestAProjectFileCannotSayWhereCredentialsGo(t *testing.T) {
	user := "platforms:\n  - name: codefloe\n    kind: forgejo\n    base_url: https://codefloe.com\n" +
		"server:\n  url: https://spov.example.com\n"
	for _, tt := range []struct{ project, key, where string }{
		{"server:\n  url: https://evil.example\n", "server.url", "SPOV_SERVER or --server"},
		{"server:\n  url: \"\"\n", "server.url", "SPOV_SERVER or --server"},
		{"server: null\n", "server.url", "SPOV_SERVER or --server"},
		{"server:\n", "server.url", "SPOV_SERVER or --server"},
		{"server: {}\n", "server.url", "SPOV_SERVER or --server"},
		{"platforms: null\n", "platforms", "user file"},
		{"platforms:\n  - name: codefloe\n    kind: forgejo\n    base_url: https://evil.example\n", "platforms", "user file"},
		{"platforms: []\n", "platforms", "user file"},
	} {
		env, wd := newFixture(t, user, tt.project)
		_, err := Load(env, wd)
		if err == nil {
			t.Errorf("%q loaded", tt.project)
			continue
		}
		for _, want := range []string{ProjectFileName, "sets " + tt.key, tt.where} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q: %v lacks %q", tt.project, err, want)
			}
		}
	}

	env, wd := newFixture(t, user, "sandbox:\n  cpu: 2\n")
	res, err := Load(env, wd)
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Server.URL != "https://spov.example.com" || res.Config.Platforms[0].BaseURL != "https://codefloe.com" ||
		res.Config.Sandbox.CPU != 2 {
		t.Errorf("loaded %+v", res.Config)
	}
}

// A server reads the user file and the environment, never a project
// file, whatever it holds.
//
// @decision(DL-026)
func TestLoadUserSkipsTheProjectFile(t *testing.T) {
	env, wd := newFixture(t, "sandbox:\n  memory_mib: 4096\n", "sandbox:\n  memory_mib: 8192\nplatforms: []\n")
	t.Chdir(wd)
	env.Vars["SPOV_SANDBOX_CPU"] = "3"
	res, err := LoadUser(env)
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Sandbox.MemoryMiB != 4096 || res.Sources.ProjectFile != "" || res.Config.Sandbox.CPU != 3 {
		t.Errorf("LoadUser read %+v from %+v", res.Config, res.Sources)
	}
}

func TestLoadRecordsMissingHome(t *testing.T) {
	wd := t.TempDir()
	res, err := Load(MapEnv{}, wd)
	if err != nil {
		t.Fatalf("a missing home directory must not be fatal: %v", err)
	}
	if res.Sources.UserFileError == "" {
		t.Error("Sources does not record why the user layer was skipped")
	}
	if res.Sources.UserFile != "" {
		t.Errorf("UserFile = %q, want empty", res.Sources.UserFile)
	}
}

func TestLoadRejectsMalformedUserFile(t *testing.T) {
	env, wd := newFixture(t, "sandbox: [broken\n", "")
	_, err := Load(env, wd)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), UserFileName) {
		t.Errorf("error %q does not name the user config file", err)
	}
}

func TestLoadReportsUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits are not enforced")
	}
	wd := t.TempDir()
	path := filepath.Join(wd, ProjectFileName)
	if err := os.WriteFile(path, []byte("version: 1\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err := Load(MapEnv{}, wd)
	if err == nil {
		t.Fatal("expected an error for an unreadable config file")
	}
	if !strings.Contains(err.Error(), ProjectFileName) {
		t.Errorf("error %q does not name the file", err)
	}
}

func TestLoadTreatsEmptyEnvVarAsUnset(t *testing.T) {
	env, wd := newFixture(t, "modal:\n  app: configured\n", "")
	env.Vars["SPOV_MODAL_APP"] = ""

	res, err := Load(env, wd)
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Modal.App != "configured" {
		t.Errorf("modal.app = %q; an empty env var must not clobber the file", res.Config.Modal.App)
	}
	if len(res.Sources.EnvVars) != 0 {
		t.Errorf("EnvVars = %v, want empty", res.Sources.EnvVars)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"unknown field in a section", "sandbox:\n  cpuu: 4\n", "cpuu"},
		{"unknown top-level key", "bogus: 1\n", "unknown top-level keys: bogus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, wd := newFixture(t, "", tt.body)
			_, err := Load(env, wd)
			if err == nil {
				t.Fatal("expected an error for an unknown config key")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name the offending key", err)
			}
		})
	}
}

// The quota keys are documented in deploy/README.md as environment
// variables, and koanf's mapping from SPOV_QUOTAS_MAX_CONCURRENT_JOBS
// to Quotas.MaxConcurrentJobs is a naming convention rather than
// something the compiler checks — a renamed field or a changed koanf
// tag silently stops reading the variable, and the deployment quietly
// falls back to the default while the documentation still promises it
// works.
//
// @decision(DL-045) @decision(DL-009)
func TestQuotaEnvironmentVariablesReachTheConfig(t *testing.T) {
	res, err := Load(MapEnv{Vars: map[string]string{
		"SPOV_QUOTAS_MAX_CONCURRENT_JOBS":     "7",
		"SPOV_QUOTAS_MAX_CONCURRENT_SESSIONS": "5",
		"SPOV_QUOTAS_MAX_TIMEOUT":             "3h",
		"SPOV_QUOTAS_MAX_CPU":                 "2",
		"SPOV_QUOTAS_MAX_MEMORY_MIB":          "4096",
	}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q := res.Config.Quotas
	if q.MaxConcurrentJobs != 7 {
		t.Errorf("MaxConcurrentJobs = %d, want 7", q.MaxConcurrentJobs)
	}
	if q.MaxConcurrentSessions != 5 {
		t.Errorf("MaxConcurrentSessions = %d, want 5", q.MaxConcurrentSessions)
	}
	if q.MaxTimeout != 3*time.Hour {
		t.Errorf("MaxTimeout = %s, want 3h", q.MaxTimeout)
	}
	if q.MaxCPU != 2 {
		t.Errorf("MaxCPU = %g, want 2", q.MaxCPU)
	}
	if q.MaxMemoryMiB != 4096 {
		t.Errorf("MaxMemoryMiB = %d, want 4096", q.MaxMemoryMiB)
	}
}

// The preparation pass's settings reach the config through the
// environment, like every other server-side setting.
//
// @decision(DL-047)
func TestPrepEnvironmentVariablesReachTheConfig(t *testing.T) {
	res, err := Load(MapEnv{Vars: map[string]string{
		"SPOV_PREP_BASE_URL": "https://litellm.internal/v1",
		"SPOV_PREP_MODEL":    "claude-sonnet-5",
		"SPOV_PREP_KEY_ENV":  "LITELLM_API_KEY",
	}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := res.Config.Prep
	if p.BaseURL != "https://litellm.internal/v1" {
		t.Errorf("BaseURL = %q", p.BaseURL)
	}
	if p.Model != "claude-sonnet-5" {
		t.Errorf("Model = %q", p.Model)
	}
	if p.KeyEnv != "LITELLM_API_KEY" {
		t.Errorf("KeyEnv = %q", p.KeyEnv)
	}
}

// The git access keys reach the config through the environment.
//
// @decision(DL-043)
func TestGitAccessEnvironmentVariablesReachTheConfig(t *testing.T) {
	res, err := Load(MapEnv{Vars: map[string]string{
		"SPOV_GIT_ALLOW_ALL":          "true",
		"SPOV_SANDBOX_GIT_ACCESS":     "all",
		"SPOV_SANDBOX_JOB_GIT_ACCESS": "repos",
	}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := res.Config
	if !c.Git.AllowAll || c.Sandbox.GitAccess != "all" || c.Sandbox.JobGitAccess != "repos" {
		t.Errorf("git.allow_all %v, sandbox.git_access %q, sandbox.job_git_access %q",
			c.Git.AllowAll, c.Sandbox.GitAccess, c.Sandbox.JobGitAccess)
	}
}

// sandbox.sync has three states, unset among them, from a file or the
// environment.
//
// @decision(DL-095)
func TestSandboxSyncHasThreeStates(t *testing.T) {
	state := func(v *bool) string {
		if v == nil {
			return "unset"
		}
		if *v {
			return "true"
		}
		return "false"
	}
	cases := []struct {
		name, user, env, want string
	}{
		{"unset", "", "", "unset"},
		{"true in the user file", "sandbox:\n  sync: true\n", "", "true"},
		{"false in the user file", "sandbox:\n  sync: false\n", "", "false"},
		{"false from the environment", "sandbox:\n  sync: true\n", "false", "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, wd := newFixture(t, tc.user, "")
			if tc.env != "" {
				env.Vars["SPOV_SANDBOX_SYNC"] = tc.env
			}
			res, err := Load(env, wd)
			if err != nil {
				t.Fatal(err)
			}
			if got := state(res.Config.Sandbox.Sync); got != tc.want {
				t.Errorf("sandbox.sync = %s, want %s", got, tc.want)
			}
		})
	}
}

// Whether the repositories came from the project file is recorded, and
// only a list with an entry counts.
//
// @decision(DL-095)
func TestTheSourcesSayWhetherTheProjectFileListsRepos(t *testing.T) {
	const (
		platforms = "platforms:\n  - name: codefloe\n    kind: forgejo\n    base_url: https://codefloe.com\n"
		repos     = "repos:\n  - platform: codefloe\n    owner: alice\n    name: spov\n"
	)
	cases := []struct {
		name, user, project string
		want                bool
	}{
		{"the project file lists them", platforms, repos, true},
		{"only the user file lists them", platforms + repos, "sandbox:\n  cpu: 1\n", false},
		{"the project file's list is empty", platforms + repos, "repos: []\n", false},
		{"no project file", platforms + repos, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, wd := newFixture(t, tc.user, tc.project)
			res, err := Load(env, wd)
			if err != nil {
				t.Fatal(err)
			}
			if res.Sources.ProjectRepos != tc.want {
				t.Errorf("ProjectRepos = %v, want %v", res.Sources.ProjectRepos, tc.want)
			}
		})
	}
}

// The example project file loads as a project file, beside a user file
// that declares the platforms it names.
//
// @decision(DL-026)
func TestTheExampleProjectFileLoads(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", ".spov.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	env, wd := newFixture(t, `platforms:
  - name: codefloe
    kind: forgejo
    base_url: https://codefloe.com
    account: alice
  - name: github
    kind: github
    account: alice
`, string(example))
	res, err := Load(env, wd)
	if err != nil {
		t.Fatalf("the example does not load: %v", err)
	}
	if !res.Sources.ProjectRepos || len(res.Config.Repos) != 2 {
		t.Errorf("repos = %+v", res.Config.Repos)
	}
}

// sandbox.env merges by name across the layers, unlike a list.
//
// @decision(DL-096)
func TestSandboxEnvMergesByName(t *testing.T) {
	env, wd := newFixture(t, "sandbox:\n  env:\n    A: user\n    B: user\n", "sandbox:\n  env:\n    B: project\n    C: project\n")
	res, err := Load(env, wd)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "user", "B": "project", "C": "project"}
	if !maps.Equal(res.Config.Sandbox.Env, want) {
		t.Errorf("sandbox.env = %v, want %v", res.Config.Sandbox.Env, want)
	}
}

// A sandbox.env value YAML does not read as text is refused rather than
// rewritten, and SPOV_SANDBOX_ENV is not a config variable.
//
// @decision(DL-096)
func TestSandboxEnvValuesAreTextAsWritten(t *testing.T) {
	for _, value := range []string{"true", "1.10", "0755", "2024-01-01", "12"} {
		env, wd := newFixture(t, "sandbox:\n  env:\n    V: "+value+"\n", "")
		if _, err := Load(env, wd); err == nil || !strings.Contains(err.Error(), "quote the value") {
			t.Errorf("V: %s loaded, or failed otherwise: %v", value, err)
		}
	}
	env, wd := newFixture(t, "sandbox:\n  env:\n    A: \"1.10\"\n    B: off\n    C:\n", "")
	env.Vars["SPOV_SANDBOX_ENV"] = "X=1"
	res, err := Load(env, wd)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Config.Sandbox.Env; got["A"] != "1.10" || got["B"] != "off" || got["C"] != "" {
		t.Errorf("sandbox.env = %v", got)
	}
	if !slices.Contains(res.Sources.IgnoredEnvVars, "SPOV_SANDBOX_ENV") {
		t.Errorf("SPOV_SANDBOX_ENV was not ignored: %+v", res.Sources)
	}
}
