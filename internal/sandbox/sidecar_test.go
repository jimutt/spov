package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func proxySidecar() *Sidecar {
	return &Sidecar{Name: "proxy", Image: "spov-proxy-x", Command: []string{"/usr/local/bin/spov-proxy"}, Egress: EgressOpen()}
}

// A sidecar is checked before a billable create, like the rest of a spec.
//
// @decision(DL-034)
func TestASidecarIsValidatedWithItsSpec(t *testing.T) {
	spec := validSpec()
	spec.Sidecar = proxySidecar()
	if err := spec.Validate(); err != nil {
		t.Fatalf("a valid sidecar was refused: %v", err)
	}
	for name, mutate := range map[string]func(*Sidecar){
		"no name":       func(s *Sidecar) { s.Name = "" },
		"reserved name": func(s *Sidecar) { s.Name = "main" },
		"no image":      func(s *Sidecar) { s.Image = "" },
		"no command":    func(s *Sidecar) { s.Command = nil },
		"no egress":     func(s *Sidecar) { s.Egress = EgressPolicy{} },
	} {
		spec := validSpec()
		spec.Sidecar = proxySidecar()
		mutate(spec.Sidecar)
		if err := spec.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The proxy image's name follows its binary, so a new proxy is a new
// image and the same proxy is never baked twice.
//
// @decision(DL-034)
func TestTheProxyImageIsNamedByItsBinary(t *testing.T) {
	a, b := ProxyImageName([]byte("one")), ProxyImageName([]byte("two"))
	if a == b || a != ProxyImageName([]byte("one")) {
		t.Errorf("names %q and %q", a, b)
	}
	if !strings.HasPrefix(a, "spov-proxy-") || len(a) != len("spov-proxy-")+16 {
		t.Errorf("name %q", a)
	}
}

// The fake runs a command only in a sidecar the sandbox was created with.
func TestTheFakeExecsOnlyInItsOwnSidecar(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	spec := validSpec()
	spec.Sidecar = proxySidecar()
	h, err := f.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.SidecarExecInput(ctx, "proxy", []string{"set-token", "gh"}, []byte("tok")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SidecarExecInput(ctx, "other", nil, nil); !errors.Is(err, ErrNoSidecar) {
		t.Errorf("an unknown sidecar: %v", err)
	}
	got := h.(*FakeHandle).SidecarExecs()
	if len(got) != 1 || got[0].Sidecar != "proxy" || string(got[0].Stdin) != "tok" {
		t.Errorf("recorded %+v", got)
	}
	if name, err := f.EnsureProxyImage(ctx, []byte("bin")); err != nil || name != ProxyImageName([]byte("bin")) || len(f.ProxyBakes) != 1 {
		t.Errorf("EnsureProxyImage = %q, %v", name, err)
	}
}
