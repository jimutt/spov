package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// deployFile reads a file under the repository's deploy/ directory.
func deployFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", path))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestTheDeploymentFilesBindEveryInterface: a hosted server falls back to
// loopback, so each deployment file must ask for every interface itself.
//
// @decision(DL-010)
func TestTheDeploymentFilesBindEveryInterface(t *testing.T) {
	if DefaultListen != "127.0.0.1:8080" {
		t.Errorf("DefaultListen = %q, want the loopback default", DefaultListen)
	}
	sets := map[string]*regexp.Regexp{
		"compose.yaml":            regexp.MustCompile(`(?m)^\s*SPOV_LISTEN:\s*":8080"\s*$`),
		"kamal/config/deploy.yml": regexp.MustCompile(`(?m)^\s*SPOV_LISTEN:\s*":8080"\s*$`),
		"k8s/deployment.yaml":     regexp.MustCompile(`(?m)^\s*- name: SPOV_LISTEN\s*\n\s*value:\s*":8080"\s*$`),
	}
	for path, want := range sets {
		if !want.MatchString(deployFile(t, path)) {
			t.Errorf("%s does not set SPOV_LISTEN to :8080", path)
		}
	}
}

// TestKamalRunsFromTheRepositoryRoot: the config's hook and secrets paths
// are relative to the root, and the hook names the config it was run with.
//
// @decision(DL-010)
func TestKamalRunsFromTheRepositoryRoot(t *testing.T) {
	conf := deployFile(t, "kamal/config/deploy.yml")
	for _, want := range []string{
		"hooks_path: deploy/kamal/.kamal/hooks\n",
		"secrets_path: deploy/kamal/.kamal/secrets\n",
		"context: .\n",
		"dockerfile: deploy/Dockerfile\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("deploy.yml lacks %q", strings.TrimSpace(want))
		}
	}
	if hook := deployFile(t, "kamal/.kamal/hooks/pre-deploy"); !strings.Contains(hook, "-c deploy/kamal/config/deploy.yml") {
		t.Error("the pre-deploy hook does not name the config, so kamal app exec looks for config/deploy.yml at the root")
	}
}
