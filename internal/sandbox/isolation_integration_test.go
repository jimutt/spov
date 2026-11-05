//go:build integration

package sandbox

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"
)

// TestIntegrationBubblewrapInModal is spike S-I1, the smoke test for the
// credential isolation design. One billable sandbox answers what a
// credential proxy would rest on:
//
//  1. can bubblewrap create its namespaces under Modal's gVisor;
//  2. does a network namespace take the network away, loopback included;
//  3. does a unix socket bound in from outside still get out, which is
//     how a proxy outside the namespace becomes the only way out;
//  4. does a fresh /proc hide the environment of processes outside;
//  5. can OpenCode serve from inside, reached through the reverse bridge.
//
// It installs bubblewrap, socat and tinyproxy from Debian at runtime, so
// it measures the bwrap build the image would get from apt. tinyproxy
// stands in for the proxy spov would run.
func TestIntegrationBubblewrapInModal(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	sb, _, _ := startOpenCodeSandbox(t, ctx, p, "spov-bwrap", 5*time.Minute)

	sh := func(script string) (int, string, string) {
		t.Helper()
		return execIn(ctx, t, sb, "bash", "-c", script)
	}
	must := func(what, script string) string {
		t.Helper()
		code, out, errOut := sh(script)
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s%s", what, code, out, errOut)
		}
		return out
	}

	must("install", "set -e; export DEBIAN_FRONTEND=noninteractive; apt-get update -qq && "+
		"apt-get install -y -qq --no-install-recommends bubblewrap socat tinyproxy >/dev/null")
	t.Logf("RESULT environment: %s", strings.Join(strings.Fields(must("identify",
		"bwrap --version; echo uid=$(id -u); uname -r; git --version")), " "))

	// The candidate jail. unshare(1) makes the user and network
	// namespaces, because bwrap's own --unshare-net fails here (question
	// 1 records how); bwrap inside it adds the rest, drops every
	// capability, and mounts a /proc of its own over the root filesystem
	// as it is.
	const jail = "unshare --user --map-root-user --net -- " +
		"bwrap --unshare-pid --unshare-ipc --unshare-uts --cap-drop ALL --die-with-parent " +
		"--dev-bind / / --proc /proc "

	// The bridge: a unix socket outside forwards to tinyproxy on the
	// outer loopback, and a probe secret sits in one outside process's
	// environment for question 4.
	must("start the outside half", "set -e; mkdir -p /run/spov; "+
		"install -d -o tinyproxy /run/tinyproxy /var/log/tinyproxy; tinyproxy -c /etc/tinyproxy/tinyproxy.conf; "+
		"setsid socat UNIX-LISTEN:/run/spov/egress.sock,fork,mode=600 TCP:127.0.0.1:8888 >/dev/null 2>&1 < /dev/null & "+
		"head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \\n' > /run/spov/probe; "+
		"SPOV_SPIKE_PROBE=$(cat /run/spov/probe) setsid sleep 900 >/dev/null 2>&1 < /dev/null & "+
		"for i in $(seq 50); do [ -S /run/spov/egress.sock ] && exit 0; sleep 0.1; done; exit 1")

	t.Run("1 namespaces", func(t *testing.T) {
		for _, c := range []struct {
			name, cmd string
			needed    bool
		}{
			// Without a user namespace bwrap has no CAP_SYS_ADMIN: Modal
			// does not grant it.
			{"no namespaces", "bwrap --dev-bind / / true", false},
			{"the research doc's line", "bwrap --unshare-user --unshare-pid --dev-bind / / echo ok", true},
			// gVisor gives a new network namespace a loopback that is
			// already up and addressed, and bwrap 0.8's own loopback
			// setup fails on it.
			{"bwrap --unshare-net", "bwrap --unshare-user --unshare-net --dev-bind / / true", false},
			{"the candidate jail", jail + "true", true},
		} {
			code, out, errOut := sh(c.cmd)
			t.Logf("RESULT %-24s exit %d %s%s", c.name, code, strings.TrimSpace(out), strings.TrimSpace(errOut))
			if c.needed && code != 0 {
				t.Errorf("%s failed: bubblewrap cannot build this under Modal", c.name)
			}
		}
	})

	t.Run("2 no network", func(t *testing.T) {
		const probe = "curl -sS -o /dev/null -w '%{http_code}' --max-time 10 "
		code, out, _ := sh(probe + "https://example.com")
		t.Logf("RESULT outside, example.com:       exit %d status %s", code, out)
		if code != 0 {
			t.Fatalf("the control cannot reach example.com, so the rest measures nothing")
		}
		for _, target := range []string{"https://example.com", "http://1.1.1.1", "http://127.0.0.1:4096/"} {
			code, out, errOut := sh(jail + probe + target)
			t.Logf("RESULT inside, %-22s exit %d status %s %s", target+":", code, out, strings.TrimSpace(errOut))
			if code == 0 {
				t.Errorf("the jail reached %s", target)
			}
		}
		_, out, _ = sh(jail + "cat /proc/net/dev | tail -n +3 | cut -d: -f1")
		t.Logf("RESULT inside, interfaces: %s", strings.Join(strings.Fields(out), " "))
	})

	t.Run("3 the bridge gets out", func(t *testing.T) {
		script := jail + "bash -c '" +
			"socat TCP-LISTEN:3128,bind=127.0.0.1,fork,reuseaddr UNIX-CONNECT:/run/spov/egress.sock & " +
			"for i in $(seq 50); do (exec 3<>/dev/tcp/127.0.0.1/3128) 2>/dev/null && break; sleep 0.1; done; " +
			"echo https=$(HTTPS_PROXY=http://127.0.0.1:3128 curl -sS -o /dev/null -w %{http_code} --max-time 15 https://example.com); " +
			"git -c http.proxy=http://127.0.0.1:3128 ls-remote https://github.com/git/git HEAD | cut -c1-12'"
		code, out, errOut := sh(script)
		t.Logf("RESULT through the bridge: exit %d %s %s", code, strings.Join(strings.Fields(out), " "), strings.TrimSpace(errOut))
		if code != 0 || !strings.Contains(out, "https=200") || len(strings.Fields(out)) < 2 {
			t.Errorf("the jail could not get out through the bridged proxy")
		}
	})

	t.Run("4 the environment of outside processes", func(t *testing.T) {
		const find = "s=$(cat /run/spov/probe); grep -l \"$s\" /proc/[0-9]*/environ 2>/dev/null | wc -l"
		_, outside, _ := sh(find)
		// A user namespace alone, with the outer /proc still mounted.
		_, sharedProc, _ := sh("unshare --user --map-root-user -- bash -c '" + find + "'")
		_, inside, _ := sh(jail + "bash -c '" + find + "; ls -d /proc/[0-9]* | wc -l'")
		t.Logf("RESULT processes whose environment holds the probe: outside %s, user namespace alone %s, jail %s (jail sees %s pids)",
			strings.TrimSpace(outside), strings.TrimSpace(sharedProc),
			strings.Fields(inside + " ? ?")[0], strings.Fields(inside + " ? ?")[1])
		if strings.TrimSpace(outside) == "0" {
			t.Fatalf("the control does not find the probe, so the rest measures nothing")
		}
		if strings.Fields(inside + " ?")[0] != "0" {
			t.Errorf("the jail can read the environment of a process outside it")
		}
	})

	t.Run("5 OpenCode serves from inside", func(t *testing.T) {
		// Inside, OpenCode takes the same port as the outer server, which
		// only works in a namespace of its own; the reverse bridge takes
		// a unix socket back out to an outer port.
		// Without --die-with-parent: the exec's shell exits at once.
		detached := strings.Replace(jail, "--die-with-parent ", "", 1)
		must("start OpenCode in the jail", "setsid "+detached+"bash -c '"+
			"socat UNIX-LISTEN:/run/spov/opencode.sock,fork,mode=600 TCP:127.0.0.1:4096 & "+
			"cd /workspace && exec /root/.opencode/bin/opencode serve --hostname 127.0.0.1 --port 4096' >/tmp/jailed-opencode.log 2>&1 < /dev/null & "+
			"setsid socat TCP-LISTEN:4196,bind=127.0.0.1,fork,reuseaddr UNIX-CONNECT:/run/spov/opencode.sock >/dev/null 2>&1 < /dev/null &")
		var status string
		for range 60 {
			_, out, _ := sh("curl -s -o /dev/null -w '%{http_code}' --max-time 3 http://127.0.0.1:4196/")
			if status = strings.TrimSpace(out); status != "" && status != "000" {
				break
			}
			time.Sleep(time.Second)
		}
		_, log, _ := sh("tail -c 600 /tmp/jailed-opencode.log")
		t.Logf("RESULT jailed OpenCode through the reverse bridge: status %s\n%s", status, log)
		if status == "" || status == "000" {
			t.Errorf("OpenCode inside the jail was not reachable from outside")
		}
	})
}

// TestIntegrationSidecarProxy is spike S-I2: the same question as S-I1,
// answered with a Modal sidecar instead of namespaces. The proxy runs in
// its own container, holding a probe secret in its environment; the main
// container's egress is then blocked. It measures whether the main
// container can still reach the sidecar by name, whether that is its only
// way out, and whether anything of the sidecar is visible from main.
func TestIntegrationSidecarProxy(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	sb, _, _ := startOpenCodeSandbox(t, ctx, p, "spov-sidecar", 5*time.Minute)

	image, err := p.image(ctx, testImage())
	if err != nil {
		t.Fatal(err)
	}
	probe := randomPassword(t)
	proxy, err := sb.ExperimentalSidecars.Create(ctx, "proxy", image, &modal.SidecarCreateParams{
		Command: []string{"sleep", "infinity"},
		Env:     map[string]string{"SPOV_SPIKE_PROBE": probe},
	})
	if err != nil {
		t.Fatalf("RESULT sidecar create failed: %v", err)
	}
	inProxy := func(script string) (int, string, string) {
		t.Helper()
		proc, err := proxy.Exec(ctx, []string{"bash", "-c", script}, nil)
		if err != nil {
			t.Fatalf("sidecar exec: %v", err)
		}
		code, err := proc.Wait(ctx, nil)
		if err != nil {
			t.Fatalf("sidecar exec wait: %v", err)
		}
		stdout, _ := io.ReadAll(proc.Stdout)
		stderr, _ := io.ReadAll(proc.Stderr)
		return code, string(stdout), string(stderr)
	}
	inMain := func(script string) (int, string, string) {
		t.Helper()
		return execIn(ctx, t, sb, "bash", "-c", script)
	}

	// tinyproxy stands in for spov's proxy, open to the internal network.
	if code, out, errOut := inProxy("set -e; export DEBIAN_FRONTEND=noninteractive; " +
		"apt-get update -qq && apt-get install -y -qq --no-install-recommends tinyproxy >/dev/null; " +
		"sed -i '/^Allow /d' /etc/tinyproxy/tinyproxy.conf; install -d -o tinyproxy /run/tinyproxy /var/log/tinyproxy; " +
		"tinyproxy -c /etc/tinyproxy/tinyproxy.conf; hostname; cat /etc/hosts"); code != 0 {
		t.Fatalf("set up the proxy: exit %d\n%s%s", code, out, errOut)
	} else {
		t.Logf("RESULT sidecar up; its view:\n%s", out)
	}

	if err := sb.UpdateNetworkPolicy(ctx, &modal.SandboxUpdateNetworkPolicyParams{
		OutboundCIDRAllowlist:   &modal.Allowlist{},
		OutboundDomainAllowlist: &modal.Allowlist{},
	}); err != nil {
		t.Fatalf("block main's egress: %v", err)
	}
	time.Sleep(3 * time.Second)

	const get = "curl -sS -o /dev/null -w '%{http_code}' --max-time 10 "
	_, hosts, _ := inMain("getent hosts proxy main; cat /etc/hosts")
	t.Logf("RESULT main's view of the names:\n%s", hosts)
	for _, c := range []struct {
		name, script string
		wantOK       bool
	}{
		{"main direct to example.com", get + "https://example.com", false},
		{"main direct to 1.1.1.1", get + "http://1.1.1.1", false},
		{"main via the sidecar, curl", "HTTPS_PROXY=http://proxy:8888 " + get + "https://example.com", true},
		{"main via the sidecar, git", "git -c http.proxy=http://proxy:8888 ls-remote https://github.com/git/git HEAD | cut -c1-12", true},
	} {
		code, out, errOut := inMain(c.script)
		t.Logf("RESULT %-30s exit %d %s %s", c.name+":", code, strings.TrimSpace(out), strings.TrimSpace(errOut))
		if (code == 0) != c.wantOK {
			t.Errorf("%s: exit %d, want success=%v", c.name, code, c.wantOK)
		}
	}

	// The secret: main searches every environment and file it can see.
	code, out, _ := inMain("s='" + probe + "'; grep -l \"$s\" /proc/[0-9]*/environ 2>/dev/null | wc -l; " +
		"cat /proc/[0-9]*/comm 2>/dev/null | grep -c -E 'tinyproxy|sleep'; " +
		"grep -rl --exclude-dir=proc --exclude-dir=sys \"$s\" / 2>/dev/null | head -3 | wc -l")
	t.Logf("RESULT main finds the probe in: environments %s, sees sidecar processes %s, files %s",
		strings.Fields(out + " ? ? ?")[0], strings.Fields(out + " ? ? ?")[1], strings.Fields(out + " ? ? ?")[2])
	if code != 0 || strings.Fields(out + " ?")[0] != "0" || strings.Fields(out + " ? ? ?")[2] != "0" {
		t.Errorf("main can see the sidecar's secret")
	}
	_, out, _ = inProxy("printenv SPOV_SPIKE_PROBE | wc -c")
	t.Logf("RESULT control: the sidecar holds the probe (%s bytes)", strings.TrimSpace(out))
}
