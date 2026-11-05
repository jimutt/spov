//go:build integration

package sandbox

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"

	"github.com/jimutt/spov/internal/opencode"
)

// These are the spikes behind re-minting a GitHub token into a running
// sandbox: can the server write into it without keeping it alive, and
// can gh read a token that is not in its environment. Both create
// billable sandboxes and terminate what they create.

// execIn runs a command in a raw sandbox and returns its exit code and
// output, as modalHandle.Exec does.
func execIn(ctx context.Context, t *testing.T, sb *modal.Sandbox, command ...string) (int, string, string) {
	t.Helper()
	proc, err := sb.Exec(ctx, command, nil)
	if err != nil {
		t.Fatalf("exec %v: %v", command, err)
	}
	code, err := proc.Wait(ctx, nil)
	if err != nil {
		t.Fatalf("exec wait %v: %v", command, err)
	}
	stdout, _ := io.ReadAll(proc.Stdout)
	stderr, _ := io.ReadAll(proc.Stderr)
	return code, string(stdout), string(stderr)
}

// TestIntegrationExecAndIdleTimeout is spike S-R1. Two sandboxes with
// IdleTimeout 2m run for 4½ minutes: a no-traffic control, and one given
// a short exec every 60 s. It reports whether an exec counts as activity.
func TestIntegrationExecAndIdleTimeout(t *testing.T) {
	const (
		idle    = 2 * time.Minute
		observe = idle*2 + 30*time.Second
	)
	p := newIntegrationProvider(t)

	for _, withExec := range []bool{false, true} {
		name := "no traffic"
		if withExec {
			name = "exec every 60s"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), observe+5*time.Minute)
			defer cancel()
			sb, _, _ := startOpenCodeSandbox(t, ctx, p, "spov-execidle", idle)

			trafficCtx, stopTraffic := context.WithCancel(ctx)
			defer stopTraffic()
			if withExec {
				go func() {
					tick := time.NewTicker(60 * time.Second)
					defer tick.Stop()
					for {
						select {
						case <-trafficCtx.Done():
							return
						case <-tick.C:
							proc, err := sb.Exec(trafficCtx, []string{"true"}, nil)
							if err != nil {
								t.Logf("exec: %v", err)
								continue
							}
							_, _ = proc.Wait(trafficCtx, nil)
						}
					}
				}()
			}

			start := time.Now()
			died := time.Duration(0)
			for time.Since(start) < observe {
				time.Sleep(15 * time.Second)
				code, err := sb.Poll(ctx, nil)
				if err != nil {
					t.Fatalf("poll: %v", err)
				}
				if code != nil {
					died = time.Since(start).Round(time.Second)
					break
				}
			}
			stopTraffic()

			if died != 0 {
				t.Logf("RESULT %-16s died after %s: this shape does NOT reset the idle timeout", name, died)
			} else {
				t.Logf("RESULT %-16s alive after %s: this shape DOES reset the idle timeout", name, observe)
			}
			if !withExec && died == 0 {
				t.Errorf("the control survived %s with IdleTimeout %s: idle detection has changed and this spike measures nothing", observe, idle)
			}
		})
	}
}

// TestIntegrationGhReadsItsConfigFile is spike S-R2: with no GH_TOKEN in
// the environment, does the image's gh take its token from hosts.yml,
// and does a rewrite of that file replace it? Offline: `gh auth token`
// prints the stored token without asking GitHub.
func TestIntegrationGhReadsItsConfigFile(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	sb, _, _ := startOpenCodeSandbox(t, ctx, p, "spov-ghconfig", 5*time.Minute)

	write := func(token string) {
		t.Helper()
		// The multi-account format gh has used since 2.40, with
		// config.yml marked as migrated: the older single-account file
		// makes gh migrate it, which asks GitHub who the token belongs to.
		script := "set -e; mkdir -p /root/.config/gh; umask 077; " +
			"grep -q '^version:' /root/.config/gh/config.yml 2>/dev/null || printf 'version: \"1\"\\n' >> /root/.config/gh/config.yml; " +
			"printf 'github.com:\\n    users:\\n        x-access-token:\\n            oauth_token: %s\\n    git_protocol: https\\n    user: x-access-token\\n    oauth_token: %s\\n' \"$0\" \"$0\" " +
			"> /root/.config/gh/hosts.yml.tmp && mv /root/.config/gh/hosts.yml.tmp /root/.config/gh/hosts.yml"
		if code, out, errOut := execIn(ctx, t, sb, "bash", "-c", script, token); code != 0 {
			t.Fatalf("writing hosts.yml: exit %d\n%s%s", code, out, errOut)
		}
	}
	read := func() string {
		t.Helper()
		code, out, errOut := execIn(ctx, t, sb, "env", "-u", "GH_TOKEN", "-u", "GITHUB_TOKEN",
			"gh", "auth", "token", "--hostname", "github.com")
		if code != 0 {
			t.Fatalf("gh auth token: exit %d\n%s%s", code, out, errOut)
		}
		return strings.TrimSpace(out)
	}

	_, version, _ := execIn(ctx, t, sb, "gh", "--version")
	t.Logf("gh: %s", strings.SplitN(version, "\n", 2)[0])

	write("ghs_first0000000000000000000000000000")
	if got := read(); got != "ghs_first0000000000000000000000000000" {
		t.Errorf("RESULT gh read %q from hosts.yml, want the first token", got)
	}
	write("ghs_second000000000000000000000000000")
	if got := read(); got != "ghs_second000000000000000000000000000" {
		t.Errorf("RESULT after a rewrite gh read %q, want the second token", got)
	} else {
		t.Logf("RESULT gh reads hosts.yml without GH_TOKEN and follows a rewrite")
	}

	// And GH_TOKEN, when set, wins over the file, which is why the
	// entrypoint would have to stop exporting it.
	code, out, _ := execIn(ctx, t, sb, "env", "GH_TOKEN=ghs_fromenv", "gh", "auth", "token", "--hostname", "github.com")
	t.Logf("RESULT with GH_TOKEN set: exit %d, token %q", code, strings.TrimSpace(out))
}

// TestIntegrationRefreshThroughExecInput runs the real refresh through
// ExecInput in the real image: the token arrives on stdin, git's store
// and gh's hosts.yml are rewritten, and gh reads the new token with no
// GH_TOKEN. Re-run on an image or gh bump.
//
// @decision(DL-021)
func TestIntegrationRefreshThroughExecInput(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	sb, _, _ := startOpenCodeSandbox(t, ctx, p, "spov-refresh", 5*time.Minute)
	h := &modalHandle{sb: sb}

	for _, token := range []string{"ghs_first0000000000000000000000000000", "ghs_second000000000000000000000000000"} {
		res, err := h.ExecInput(ctx, opencode.RefreshArgs("github.com", "x-access-token", true), []byte(token+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("refresh: exit %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
		}
		code, out, errOut := execIn(ctx, t, sb, "env", "-u", "GH_TOKEN", "gh", "auth", "token", "--hostname", "github.com")
		if code != 0 || strings.TrimSpace(out) != token {
			t.Fatalf("gh auth token after a refresh: exit %d, %q\n%s", code, strings.TrimSpace(out), errOut)
		}
		_, creds, _ := execIn(ctx, t, sb, "cat", "/root/.git-credentials")
		if strings.TrimSpace(creds) != "https://x-access-token:"+token+"@github.com" {
			t.Fatalf("credential store after a refresh: %q", creds)
		}
	}
	t.Logf("RESULT a refresh through ExecInput rewrites git's store and gh's hosts.yml")
}
