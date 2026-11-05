//go:build integration

package sandbox

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The forge CLIs' install commands, run as the image runs them, in the
// base image under local Docker: each downloads, passes its checksum and
// reports its pinned version, and a wrong checksum fails the build. No
// Modal. Opt in with SPOV_CLI_INSTALL=1; it needs Docker and the network.
//
// @decision(DL-032)
func TestIntegrationForgeCLIsInstallAndCheckTheirChecksums(t *testing.T) {
	if os.Getenv("SPOV_CLI_INSTALL") != "1" {
		t.Skip("set SPOV_CLI_INSTALL=1 to run the forge CLI installs in Docker")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	run := func(script string) (string, error) {
		cmd := exec.Command("docker", "run", "--rm", baseImage, "bash", "-c",
			"apt-get update -qq >/dev/null && apt-get install -y -qq --no-install-recommends curl ca-certificates >/dev/null && "+script)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	var cmds []string
	for _, c := range []string{teaInstallCommand(), ghInstallCommand(), glabInstallCommand()} {
		cmds = append(cmds, "("+strings.TrimPrefix(c, "RUN ")+")")
	}
	out, err := run(strings.Join(cmds, " && ") + " && tea --version && gh --version && glab --version")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	for _, want := range []string{TeaVersion, GhVersion, GlabVersion} {
		if !strings.Contains(out, want) {
			t.Errorf("no %s in the versions:\n%s", want, out)
		}
	}
	t.Logf("versions:\n%s", out)

	// glab reads the config shape the entrypoint writes (opencode's
	// writeGlabConfig): the image leaves no config of its own, and the
	// placeholder, the default host and the switches read back as set.
	cfg := `check_update: false\ntelemetry: false\nshow_whats_new: false\nno_prompt: true\ngit_protocol: https\n` +
		`host: '"'"'gitlab.com'"'"'\nhosts:\n    '"'"'gitlab.com'"'"':\n        token: '"'"'glpat-spovplaceholder01'"'"'\n` +
		`        api_host: '"'"'gitlab.com'"'"'\n        api_protocol: https\n        git_protocol: https\n`
	out, err = run(strings.TrimPrefix(glabInstallCommand(), "RUN ") + " && " +
		"test ! -e /root/.config/glab-cli/config.yml && mkdir -p /root/.config/glab-cli && " +
		"printf '" + cfg + "' > /root/.config/glab-cli/config.yml && chmod 600 /root/.config/glab-cli/config.yml && " +
		"echo token=$(glab config get token --host gitlab.com) && echo host=$(glab config get host) && " +
		"echo check=$(glab config get check_update) && echo telemetry=$(glab config get telemetry)")
	if err != nil {
		t.Fatalf("glab config: %v\n%s", err, out)
	}
	for _, want := range []string{"token=glpat-spovplaceholder01", "host=gitlab.com", "check=false", "telemetry=false"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}

	saved := cliSHA256["glab"]
	defer func() { cliSHA256["glab"] = saved }()
	cliSHA256["glab"] = map[string]string{"amd64": strings.Repeat("0", 64), "arm64": strings.Repeat("0", 64)}
	if out, err := run(strings.TrimPrefix(glabInstallCommand(), "RUN ")); err == nil {
		t.Errorf("a wrong checksum installed:\n%s", out)
	}
}
