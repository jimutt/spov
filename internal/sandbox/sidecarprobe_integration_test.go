//go:build integration

package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"
)

// TestIntegrationSidecarIsolationProbe is spike S-I5: what a root shell
// in the main container can see or reach of the proxy sidecar, beyond
// S-I2's search of environments and files. The sidecar holds a probe in
// its environment, in files where spov-proxy and a careless program
// would put one, in a process's arguments and behind a loopback-only
// listener. Re-run it on a Modal SDK bump or a change of sandbox runtime.
//
//	go test -tags integration ./internal/sandbox/ -run SidecarIsolationProbe -v
//
// @decision(DL-034)
func TestIntegrationSidecarIsolationProbe(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	sb, _, _ := startOpenCodeSandbox(t, ctx, p, "spov-sidecar-probe", 6*time.Minute)

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
		t.Fatalf("sidecar create: %v", err)
	}
	inProxy := func(script string) (int, string, string) {
		t.Helper()
		return runIn(ctx, t, proxy, script, nil)
	}
	inMain := func(script string) (int, string, string) {
		t.Helper()
		return execIn(ctx, t, sb, "bash", "-c", script)
	}

	if code, out, errOut := inProxy("set -e; s=\"$SPOV_SPIKE_PROBE\"; " +
		"mkdir -p /run/spov-proxy /srv/open /srv/local; " +
		"for f in /run/spov-proxy/config.json /dev/shm/probe /tmp/probe /srv/local/probe; do printf %s \"$s\" > $f; done; " +
		"echo public > /srv/open/index.html; " +
		"export PATH=$PATH:/root/.local/share/mise/shims:/usr/local/bin; command -v node bun python3 nc socat; " +
		"setsid node -e \"require('http').createServer((q,r)=>r.end('public')).listen(3128,'0.0.0.0')\" >/tmp/open.log 2>&1 < /dev/null & " +
		"setsid node -e \"require('http').createServer((q,r)=>r.end(process.argv[1])).listen(9000,'127.0.0.1')\" \"$s\" >/tmp/local.log 2>&1 < /dev/null & " +
		"sleep 2; echo up"); code != 0 {
		t.Fatalf("set up the sidecar: exit %d\n%s%s", code, out, errOut)
	} else {
		t.Logf("RESULT sidecar set up: %s", strings.Join(strings.Fields(out), " "))
	}

	const view = "id -u; hostname; for n in pid net ipc mnt uts user cgroup; do printf '%s=%s ' $n \"$(readlink /proc/self/ns/$n 2>&1)\"; done; echo; " +
		"ls /proc | grep -c '^[0-9]'; (hostname -I 2>/dev/null || ip -o addr 2>/dev/null | awk '{print $4}' | tr '\\n' ' '); echo; " +
		"grep -E '^Cap(Eff|Bnd)' /proc/self/status | tr '\\n' ' '; echo; grep -c . /proc/self/mountinfo"
	_, mainView, _ := inMain(view)
	_, proxyView, _ := inProxy(view)
	t.Logf("RESULT main's view (uid, host, namespaces, processes, addresses, capabilities, mounts):\n%s", mainView)
	t.Logf("RESULT sidecar's view:\n%s", proxyView)

	// Assembled at run time, so the searching shell's own command line
	// never holds the probe.
	s := "a='" + probe[:20] + "'; b='" + probe[20:] + "'; s=\"$a$b\"; "
	for _, c := range []struct {
		name, script string
		// want is what the output must be for the wall to hold; empty
		// means the line is a measurement only.
		want string
	}{
		{"probe in any environment", s + `grep -alF -f <(printf %s "$s") /proc/[0-9]*/environ 2>/dev/null | wc -l`, "0"},
		{"probe in any command line", s + `grep -alF -f <(printf %s "$s") /proc/[0-9]*/cmdline 2>/dev/null | wc -l`, "0"},
		{"sidecar's processes visible", `cat /proc/[0-9]*/cmdline 2>/dev/null | tr '\0' ' ' | grep -c '[c]reateServer'`, "0"},
		{"probe in any file", s + `grep -rl --exclude-dir=proc --exclude-dir=sys "$s" / 2>/dev/null | wc -l`, "0"},
		{"probe through another root", s + `grep -al "$s" /proc/[0-9]*/root/run/spov-proxy/* /proc/[0-9]*/root/tmp/probe 2>/dev/null | wc -l`, "0"},
		{"sidecar's shared memory", `ls /dev/shm/probe 2>/dev/null | wc -l`, "0"},
		{"sidecar's loopback listener, by main's loopback", `curl -s --noproxy '*' --max-time 5 http://127.0.0.1:9000/probe | wc -c`, "0"},
		{"sidecar's loopback listener, by name", `curl -s --noproxy '*' --max-time 5 http://proxy:9000/probe | wc -c`, "0"},
		{"sidecar's open listener, by name", `curl -s --noproxy '*' --max-time 5 http://proxy:3128/`, "public"},
		{"other ports open on the sidecar", `n=0; for p in 22 80 443 2375 2376 4096 5000 8000 8080 8888 9000 9001 10250 50051; do (exec 3<>/dev/tcp/proxy/$p) 2>/dev/null && n=$((n+1)); done; echo $n`, "0"},
		{"unix sockets main can see", `find / -xdev -type s 2>/dev/null | head -20 | tr '\n' ' '`, ""},
		{"Modal variables in main (names)", `env | cut -d= -f1 | grep -i -E 'modal|token|secret' | sort | tr '\n' ' '`, ""},
		{"main's processes", `cat /proc/[0-9]*/cmdline 2>/dev/null | tr '\0' ' ' | cut -c1-300`, ""},
		{"mount a new proc", `mkdir -p /mnt/p && mount -t proc proc /mnt/p 2>&1 | tail -1; ls /mnt/p 2>/dev/null | grep -c '^[0-9]'`, ""},
		{"kernel", `uname -a; dmesg 2>/dev/null | head -3`, ""},
		{"metadata address", `curl -s --noproxy '*' --max-time 3 -o /dev/null -w '%{http_code}' http://169.254.169.254/ ; echo`, ""},
	} {
		code, out, errOut := inMain(c.script)
		got := strings.TrimSpace(out)
		t.Logf("RESULT %-48s exit %d: %s %s", c.name+":", code, truncate(got, 400), strings.TrimSpace(truncate(errOut, 200)))
		if c.want != "" && got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, truncate(got, 200), c.want)
		}
	}

	_, out, _ := inProxy(`printenv SPOV_SPIKE_PROBE | wc -c; curl -s --max-time 5 http://127.0.0.1:9000/probe | wc -c; ` +
		`cat /proc/[0-9]*/cmdline | tr '\0' ' ' | grep -c '[c]reateServer'`)
	t.Logf("RESULT control, in the sidecar (probe bytes in env, behind loopback, its processes): %s", strings.Join(strings.Fields(out), " "))
}
