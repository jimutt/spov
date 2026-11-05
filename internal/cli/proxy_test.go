package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/sandbox"
)

// A hosted server built without the proxy refuses to start unless its
// operator opts out; a local one starts and warns.
//
// @decision(DL-034)
func TestAHostedServerRefusesToRunWithoutTheProxy(t *testing.T) {
	if err := proxyCheck(false, false, ""); err == nil || !strings.Contains(err.Error(), TokensInSandboxEnvVar) {
		t.Errorf("hosted without the proxy: %v", err)
	}
	for _, c := range []struct {
		have, local bool
		optOut      string
	}{{true, false, ""}, {false, true, ""}, {false, false, "1"}} {
		if err := proxyCheck(c.have, c.local, c.optOut); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

// The proxy image is baked on first use, remembered after, and tried
// again after a failure.
//
// @decision(DL-034)
func TestTheProxyImageIsBakedOnceAndRetriedAfterAFailure(t *testing.T) {
	if proxyImageFor(sandbox.NewFake(), nil) != nil {
		t.Error("a build without the binary got a proxy image")
	}
	f := sandbox.NewFake()
	get := proxyImageFor(f, []byte("bin"))
	f.ProxyImageErr = errors.New("modal is down")
	if _, err := get(context.Background()); err == nil {
		t.Fatal("a failed bake succeeded")
	}
	f.ProxyImageErr = nil
	for range 3 {
		name, err := get(context.Background())
		if err != nil || name != sandbox.ProxyImageName([]byte("bin")) {
			t.Fatalf("name %q, err %v", name, err)
		}
	}
	if len(f.ProxyBakes) != 2 {
		t.Errorf("baked %d times, want 2 (the failure and the first success)", len(f.ProxyBakes))
	}
}

// A re-bake happens at most once per process, and what it bakes is the
// name later launches use.
//
// @decision(DL-034)
func TestTheProxyImageIsRebakedAtMostOnce(t *testing.T) {
	f := sandbox.NewFake()
	images := newProxyImages(f, []byte("bin"))
	if _, err := images.rebake(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := images.rebake(context.Background()); !errors.Is(err, errAlreadyRebaked) {
		t.Errorf("a second re-bake: %v", err)
	}
	if f.ProxyRebakes != 1 {
		t.Errorf("re-baked %d times", f.ProxyRebakes)
	}
	if name, err := images.ensure(context.Background()); err != nil || len(f.ProxyBakes) != 0 || name == "" {
		t.Errorf("after the re-bake: %q, %v, %d bakes", name, err, len(f.ProxyBakes))
	}
	if newProxyImages(f, nil) != nil {
		t.Error("no binary still made an image source")
	}
}
