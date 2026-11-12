package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/modelcat"
	"github.com/jimutt/spov/internal/server"
)

// run executes the spov command tree with the given args and returns
// stdout, stderr and the error.
func run(t *testing.T, env Env, args ...string) (string, string, error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	env.Out, env.Err = &out, &errBuf
	if env.Environ == nil {
		env.Environ = config.MapEnv{Vars: map[string]string{}}
	}
	if env.Workdir == "" {
		env.Workdir = t.TempDir()
	}
	// A serve in a test reads the shipped list, never models.dev.
	if env.Models == nil {
		env.Models = server.FixedModels(modelcat.Snapshot())
	}
	root := NewRoot(env)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errBuf.String(), err
}

func TestVersionCommand(t *testing.T) {
	out, _, err := run(t, Env{}, "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "spov ") {
		t.Errorf("output = %q", out)
	}
}

func TestVersionShort(t *testing.T) {
	out, _, err := run(t, Env{}, "version", "--short")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "spov") || strings.TrimSpace(out) == "" {
		t.Errorf("output = %q, want a bare version number", out)
	}
}

func TestVersionRejectsArgs(t *testing.T) {
	if _, _, err := run(t, Env{}, "version", "extra"); err == nil {
		t.Fatal("expected an error for unexpected arguments")
	}
}

func TestConfigWithoutSubcommandShowsHelp(t *testing.T) {
	out, _, err := run(t, Env{}, "config")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "show") {
		t.Errorf("help output does not mention the show subcommand: %q", out)
	}
}

func TestConfigShowYAML(t *testing.T) {
	wd := t.TempDir()
	writeProject(t, wd, "sandbox:\n  cpu: 1.5\n  timeout: 3h\n")
	env := Env{Workdir: wd, Environ: config.MapEnv{Vars: map[string]string{"SPOV_MODAL_APP": "acme"}}}

	out, _, err := run(t, env, "config", "show")
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid YAML: %v\n%s", err, out)
	}
	sandbox := got["sandbox"].(map[string]any)
	if sandbox["cpu"] != 1.5 {
		t.Errorf("cpu = %v, want 1.5", sandbox["cpu"])
	}
	if sandbox["timeout"] != "3h0m0s" {
		t.Errorf("timeout = %v, want a human duration", sandbox["timeout"])
	}
	if got["modal"].(map[string]any)["app"] != "acme" {
		t.Errorf("modal.app = %v, want acme", got["modal"])
	}
	sources := got["sources"].(map[string]any)
	if sources["project_file"] == nil {
		t.Error("sources does not record the project file")
	}
	if !strings.Contains(out, "SPOV_MODAL_APP") {
		t.Error("sources does not record the environment variable")
	}
}

func TestConfigShowJSON(t *testing.T) {
	out, _, err := run(t, Env{}, "config", "show", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if got["version"] != float64(config.SchemaVersion) {
		t.Errorf("version = %v", got["version"])
	}
}

func TestConfigShowUnknownFormat(t *testing.T) {
	_, _, err := run(t, Env{}, "config", "show", "--format", "toml")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "toml") {
		t.Errorf("error = %v", err)
	}
}

func TestConfigShowSurfacesLoadErrors(t *testing.T) {
	wd := t.TempDir()
	writeProject(t, wd, "sandbox:\n  timeout: 99h\n")
	_, _, err := run(t, Env{Workdir: wd}, "config", "show")
	if err == nil {
		t.Fatal("expected the validation error to reach the caller")
	}
	if !strings.Contains(err.Error(), "maximum") {
		t.Errorf("error = %v", err)
	}
}

// TestConfigShowRoundTrips asserts the rendered YAML uses the same key
// names as the config file it came from, so `spov config show` output can be
// pasted back into .spov.yaml.
func TestConfigShowRoundTrips(t *testing.T) {
	body := `platforms:
  - name: codefloe
    kind: forgejo
    base_url: https://codefloe.com
    account: alice
  - name: gh
    kind: github
repos:
  - platform: codefloe
    owner: alice
    name: spov
  - platform: gh
    owner: modal-labs
    name: modal-client
    dir: modal-sdk
landing: spov
`
	wd := t.TempDir()
	environ := userConfigEnv(t, body)

	out, _, err := run(t, Env{Workdir: wd, Environ: environ}, "config", "show")
	if err != nil {
		t.Fatal(err)
	}

	var shown struct {
		Platforms []config.Platform `yaml:"platforms"`
		Repos     []config.Repo     `yaml:"repos"`
		Landing   string            `yaml:"landing"`
	}
	if err := yaml.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatalf("output is not valid YAML: %v\n%s", err, out)
	}

	want := []config.Platform{
		{Name: "codefloe", Kind: config.KindForgejo, BaseURL: "https://codefloe.com", Account: "alice"},
		{Name: "gh", Kind: config.KindGitHub},
	}
	if !reflect.DeepEqual(shown.Platforms, want) {
		t.Errorf("platforms round-tripped as %+v, want %+v\n%s", shown.Platforms, want, out)
	}

	wantRepos := []config.Repo{
		{Platform: "codefloe", Owner: "alice", Name: "spov"},
		{Platform: "gh", Owner: "modal-labs", Name: "modal-client", Dir: "modal-sdk"},
	}
	if !reflect.DeepEqual(shown.Repos, wantRepos) {
		t.Errorf("repos round-tripped as %+v, want %+v\n%s", shown.Repos, wantRepos, out)
	}
	if shown.Landing != "spov" {
		t.Errorf("landing = %q", shown.Landing)
	}

	// Optional fields must be omitted rather than rendered empty.
	if strings.Contains(out, `base_url: ""`) || strings.Contains(out, `dir: ""`) {
		t.Errorf("empty optional fields were rendered:\n%s", out)
	}

	// Decoding into a generic map checks the literal key names: struct
	// decoding would hide a `baseurl` key by matching it case-insensitively.
	var generic struct {
		Platforms []map[string]any `yaml:"platforms"`
		Repos     []map[string]any `yaml:"repos"`
	}
	if err := yaml.Unmarshal([]byte(out), &generic); err != nil {
		t.Fatal(err)
	}
	if _, ok := generic.Platforms[0]["base_url"]; !ok {
		t.Errorf("platform key is not base_url: %v", generic.Platforms[0])
	}
	if _, ok := generic.Repos[1]["dir"]; !ok {
		t.Errorf("repo key is not dir: %v", generic.Repos[1])
	}
}

func TestRootHasNoUnexpectedCommands(t *testing.T) {
	root := NewRoot(Env{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	want := map[string]bool{
		"version": true, "config": true, "help": true, "completion": true,
		"fly": true, "ls": true, "stop": true, "attach": true, "resume": true,
		"login": true, "logout": true, "serve": true, "whoami": true,
		"credentials": true,
		// @decision(DL-045) @decision(DL-023)
		"jobs": true,
		// @decision(DL-122)
		"templates": true,
		// @decision(DL-123)
		"init": true,
	}
	for _, c := range root.Commands() {
		if !want[c.Name()] {
			t.Errorf("unexpected command %q", c.Name())
		}
	}
}

// TestTheRenamedCommandsAreGone: a removed name does not come back as an
// alias, and the aliases that stay are the ones DL-023 allows.
//
// @decision(DL-023)
func TestTheRenamedCommandsAreGone(t *testing.T) {
	root := NewRoot(Env{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	for _, gone := range []string{"down", "run", "new", "ps"} {
		if c := child(root, gone); c != nil {
			t.Errorf("%q still reaches `%s`", gone, c.CommandPath())
		}
	}
	aliases := map[string][]string{"spov ls": {"list"}, "spov jobs list": {"ls"}, "spov credentials list": {"ls"}}
	var visit func(c *cobra.Command)
	visit = func(c *cobra.Command) {
		if want := aliases[c.CommandPath()]; !slices.Equal(c.Aliases, want) {
			t.Errorf("`%s` has aliases %v, want %v", c.CommandPath(), c.Aliases, want)
		}
		for _, sub := range c.Commands() {
			visit(sub)
		}
	}
	visit(root)
}

// writeUserConfig writes body as the user file under an
// XDG_CONFIG_HOME of dir.
func writeUserConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "spov"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "spov", config.UserFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// userConfigEnv is an environment whose user file is body.
func userConfigEnv(t *testing.T, body string) config.MapEnv {
	t.Helper()
	dir := t.TempDir()
	writeUserConfig(t, dir, body)
	return config.MapEnv{Vars: map[string]string{"XDG_CONFIG_HOME": dir}}
}

func writeProject(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, config.ProjectFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// failWriter fails every write, standing in for a closed pipe.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestConfigShowWriteErrors(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			root := NewRoot(Env{
				Out:     failWriter{},
				Err:     &bytes.Buffer{},
				Environ: config.MapEnv{Vars: map[string]string{}},
				Workdir: t.TempDir(),
			})
			root.SetArgs([]string{"config", "show", "--format", format})
			if err := root.Execute(); err == nil {
				t.Fatal("expected a write error")
			}
		})
	}
}

func TestVersionWriteError(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"version", "--short"}} {
		root := NewRoot(Env{Out: failWriter{}, Err: &bytes.Buffer{}})
		root.SetArgs(args)
		if err := root.Execute(); err == nil {
			t.Errorf("%v: expected a write error", args)
		}
	}
}

// TestConfigShowReportsCredentialBackend: DL-008 requires the user to be
// able to see where their secrets actually live.
func TestConfigShowReportsCredentialBackend(t *testing.T) {
	store := credentials.NewMemoryStore()
	out, _, err := run(t, Env{Credentials: store}, "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "credentials: memory") {
		t.Errorf("output does not name the credential backend:\n%s", out)
	}
}

func TestConfigShowWithoutCredentialStore(t *testing.T) {
	out, _, err := run(t, Env{}, "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	// Omitted rather than shown empty: no store was configured.
	if strings.Contains(out, "credentials:") {
		t.Errorf("a credentials line appeared with no store configured:\n%s", out)
	}
}
