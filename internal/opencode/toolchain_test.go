package opencode

import (
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/toolchain"
)

func addonSel(t *testing.T, names ...string) []toolchain.Selection {
	t.Helper()
	sel, err := toolchain.Parse(names)
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

// TestTheSandboxPathServesTheToolchainFirst: mise's shims lead, so a
// repository's pin wins, and the system directories still follow.
//
// @decision(DL-032)
func TestTheSandboxPathServesTheToolchainFirst(t *testing.T) {
	env, err := EnvConfig{Password: "p"}.Env()
	if err != nil {
		t.Fatal(err)
	}
	dirs := strings.Split(env["PATH"], ":")
	if dirs[0] != toolchain.MiseShims {
		t.Errorf("PATH starts with %s", dirs[0])
	}
	for _, want := range append(toolchain.PathDirs(), "/root/.opencode/bin", "/usr/bin", "/bin") {
		if !strings.Contains(":"+env["PATH"]+":", ":"+want+":") {
			t.Errorf("PATH lacks %s: %s", want, env["PATH"])
		}
	}
	if toolchain.WorkspaceRoot != WorkspaceRoot {
		t.Errorf("toolchain trusts %s, but clones land in %s", toolchain.WorkspaceRoot, WorkspaceRoot)
	}
}

// TestAddonEnvironmentReachesEveryProcess: it is the sandbox's own
// environment, and it cannot replace PATH or HOME.
//
// @decision(DL-033)
func TestAddonEnvironmentReachesEveryProcess(t *testing.T) {
	env, err := EnvConfig{Password: "p", Addons: addonSel(t, "playwright", "dotnet")}.Env()
	if err != nil {
		t.Fatal(err)
	}
	if env["PLAYWRIGHT_BROWSERS_PATH"] != toolchain.PlaywrightBrowsers || env["DOTNET_NOLOGO"] != "1" {
		t.Errorf("env = %v", env)
	}
	if env["HOME"] != "/root" || env["PATH"] != sandboxPath {
		t.Errorf("an add-on changed HOME or PATH: %v", env)
	}
}

// TestServicesStartBeforeTheClonesAndTheServer: in the background, so
// they boot while the clones run and never hold up the server.
//
// @decision(DL-033)
func TestServicesStartBeforeTheClonesAndTheServer(t *testing.T) {
	script, err := Entrypoint{
		Port:   DefaultPort,
		Clones: []Clone{{URL: "https://example.com/r.git", Dir: "r"}},
		Addons: addonSel(t, "postgres", "redis", "rust"),
	}.Script()
	if err != nil {
		t.Fatal(err)
	}
	pg := strings.Index(script, "(pg_ctlcluster 17 main start) >'/tmp/spov-postgres.log' 2>&1 &\n")
	redis := strings.Index(script, "(redis-server /etc/redis/redis.conf --daemonize yes) >'/tmp/spov-redis.log' 2>&1 &\n")
	clone := strings.Index(script, "git clone")
	exec := strings.Index(script, "exec "+imageBinary)
	if pg < 0 || redis < 0 || clone < 0 || exec < 0 {
		t.Fatalf("a step is missing:\n%s", script)
	}
	if pg > clone || redis > clone {
		t.Errorf("services start after the clones:\n%s", script)
	}
	if plain, _ := (Entrypoint{Port: DefaultPort}).Script(); strings.Contains(plain, "pg_ctlcluster") {
		t.Error("a sandbox without the add-on starts its service")
	}
}

// TestTheWorkspaceGuideCarriesTheToolchain.
//
// @decision(DL-033)
func TestTheWorkspaceGuideCarriesTheToolchain(t *testing.T) {
	script, err := Entrypoint{
		Port:      DefaultPort,
		Workspace: []WorkspaceRepo{{Dir: "r", Slug: "acme/r", Host: "github.com", CLI: CLIGh}},
		Addons:    addonSel(t, "rust"),
	}.Script()
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(script, "<<'SPOV_AGENTS_EOF'")
	end := strings.LastIndex(script, "SPOV_AGENTS_EOF\n")
	if start < 0 || end < start {
		t.Fatalf("no workspace guide:\n%s", script)
	}
	guide := script[start:end]
	if !strings.Contains(guide, toolchain.Guide(addonSel(t, "rust"))) {
		t.Errorf("the guide does not carry the toolchain section:\n%s", guide)
	}
}
