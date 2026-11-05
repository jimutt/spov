//go:build integration

package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	modal "github.com/modal-labs/modal-client/go"
)

// TestIntegrationSidecarRecovery is spike S-I4, measured before DL-034
// point 10 was built: how an exited sidecar looks, whether one can be
// created again under the same name, whether SetTags keeps the tags it
// does not name, and whether a sandbox's name is free again as soon as
// Terminate returns. It creates billable sandboxes and terminates them.
//
// @decision(DL-034)
func TestIntegrationSidecarRecovery(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	sb, _, _ := startOpenCodeSandbox(t, ctx, p, "spov-si4", 5*time.Minute)
	image, err := p.image(ctx, testImage())
	if err != nil {
		t.Fatal(err)
	}

	// A listener on the proxy's port, as the proxy itself has.
	// perl is the one listener the image has.
	const listen = "perl -MIO::Socket -e 'my $s=IO::Socket::INET->new(LocalPort=>3128,Listen=>5,ReuseAddr=>1);" +
		"while(my $c=$s->accept){close $c}' >/dev/null 2>&1 & "
	probe := func(when string) {
		t.Helper()
		// The entrypoint's own wait for the proxy.
		code, out, _ := execIn(ctx, t, sb, "bash", "-c",
			"getent hosts proxy; (exec 3<>/dev/tcp/proxy/3128) && echo connected")
		t.Logf("MEASURED main reaches proxy:3128 %s: exit %d, %s", when, code, strings.TrimSpace(out))
		if !strings.Contains(out, "connected") {
			t.Errorf("main cannot connect to proxy:3128 %s", when)
		}
	}
	if _, err := sb.ExperimentalSidecars.Create(ctx, "proxy", image, &modal.SidecarCreateParams{
		Command: []string{"sh", "-c", listen + "sleep 12; exit 3"},
	}); err != nil {
		t.Fatalf("first sidecar: %v", err)
	}
	time.Sleep(6 * time.Second)
	probe("while the first sidecar runs")
	time.Sleep(15 * time.Second)
	h := &modalHandle{sb: sb, p: p}
	if running, err := h.SidecarRunning(ctx, "proxy"); err != nil || running {
		t.Errorf("SidecarRunning after the exit = %v, %v", running, err)
	}
	if c, err := sb.ExperimentalSidecars.Get(ctx, "proxy", nil); err != nil {
		t.Logf("MEASURED an exited sidecar: Get fails (not found: %v): %v", isNotFound(err), err)
	} else {
		code, perr := c.Poll(ctx, nil)
		if code != nil {
			t.Logf("MEASURED an exited sidecar: Get succeeds, Poll gives exit %d (err %v)", *code, perr)
		} else {
			t.Logf("MEASURED an exited sidecar: Get succeeds, Poll says running (err %v)", perr)
		}
	}
	if list, err := sb.ExperimentalSidecars.List(ctx, nil); err == nil {
		t.Logf("MEASURED sidecars listed after the exit: %d", len(list))
	}

	if err := h.StartSidecar(ctx, Sidecar{Name: "proxy", Image: testImage(),
		Command: []string{"sh", "-c", listen + "sleep infinity"}, Egress: EgressOpen()}); err != nil {
		t.Fatalf("StartSidecar under the exited one's name: %v", err)
	}
	time.Sleep(6 * time.Second)
	probe("after the recreated sidecar started")
	if running, err := h.SidecarRunning(ctx, "proxy"); err != nil || !running {
		t.Errorf("SidecarRunning after StartSidecar = %v, %v", running, err)
	}
	if res, err := h.SidecarExecInput(ctx, "proxy", []string{"echo", "by-name"}, nil); err != nil || res.Stdout != "by-name\n" {
		t.Errorf("the recreated sidecar by name: %q, %v", res.Stdout, err)
	} else {
		t.Log("MEASURED a sidecar started again under the exited one's name answers by name")
	}

	info, err := h.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tags before SetTags: %v", info.Tags)
	if err := h.AddTags(ctx, map[string]string{"spov-si4": "1"}); err != nil {
		t.Fatalf("AddTags: %v", err)
	}
	after, err := h.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Tags["spov-si4"] != "1" || after.Tags[TagSpov] != "1" || after.Tags[TagName] != info.Tags[TagName] {
		t.Errorf("AddTags lost tags: %v", after.Tags)
	}
	if err := sb.SetTags(ctx, map[string]string{"spov-si4": "1"}, nil); err != nil {
		t.Fatalf("SetTags: %v", err)
	}
	raw, err := h.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MEASURED tags after Modal's SetTags of one tag: %v", raw.Tags)

	name := info.Name
	if _, err := sb.Terminate(ctx, nil); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	base, err := p.image(ctx, testImage())
	if err != nil {
		t.Fatal(err)
	}
	reused, err := p.client.Sandboxes.ExperimentalCreate(ctx, p.app, base, &modal.SandboxCreateParams{
		Name: name, Command: []string{"sleep", "infinity"}, Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Logf("MEASURED the name %q right after Terminate: refused: %v", name, err)
		start := time.Now()
		for time.Since(start) < 3*time.Minute {
			time.Sleep(2 * time.Second)
			reused, err = p.client.Sandboxes.ExperimentalCreate(ctx, p.app, base, &modal.SandboxCreateParams{
				Name: name, Command: []string{"sleep", "infinity"}, Timeout: 5 * time.Minute,
			})
			if err == nil {
				t.Cleanup(func() { _, _ = reused.Terminate(context.WithoutCancel(ctx), nil) })
				t.Logf("MEASURED the name is free %s after Terminate returned", time.Since(start).Round(time.Second))
				return
			}
		}
		t.Logf("MEASURED the name was still taken 3m after Terminate: %v", err)
		return
	}
	t.Cleanup(func() { _, _ = reused.Terminate(context.WithoutCancel(ctx), nil) })
	t.Logf("MEASURED the name %q right after Terminate: free", name)
}
