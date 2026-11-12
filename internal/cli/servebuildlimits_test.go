package cli

import (
	"context"
	"testing"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/sandbox"
)

// imageProvider is a fake provider that can also build images, so the
// server wires a VariantBuilder.
type imageProvider struct {
	*sandbox.Fake
}

func (imageProvider) EnsureImage(context.Context, sandbox.ImageSpec) (string, error) { return "", nil }
func (imageProvider) HasImage(context.Context, string) (bool, error)                 { return true, nil }
func (imageProvider) BuildImage(context.Context, sandbox.ImageSpec) (string, error)  { return "", nil }
func (imageProvider) ImageID(context.Context, string) (string, error)                { return "", nil }
func (imageProvider) DeleteImage(context.Context, string) error                      { return nil }

// TestTheBuildLimitsApplyOnlyToAHostedServer: a local server has one user
// on their own Modal account, so its builder is unlimited, and a shared
// server's is limited per user.
//
// @decision(DL-134)
func TestTheBuildLimitsApplyOnlyToAHostedServer(t *testing.T) {
	cfg := userConfigEnv(t, defaultConfig).Vars["XDG_CONFIG_HOME"]
	env := &Env{
		Environ:      config.MapEnv{Vars: map[string]string{"XDG_CONFIG_HOME": cfg, "USER": "alice"}},
		Workdir:      t.TempDir(),
		ServerConfig: true,
	}
	res, err := env.config()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		local, limited bool
	}{{local: true, limited: false}, {local: false, limited: true}} {
		deps, closeStore, err := env.serverDeps(context.Background(), res.Config, imageProvider{sandbox.NewFake()}, "tok", tc.local)
		if err != nil {
			t.Fatal(err)
		}
		if deps.Variants == nil {
			t.Fatalf("local=%v: no variant builder", tc.local)
		}
		if deps.Variants.PerOwnerLimits != tc.limited {
			t.Errorf("local=%v: PerOwnerLimits = %v, want %v", tc.local, deps.Variants.PerOwnerLimits, tc.limited)
		}
		closeStore()
	}
}
