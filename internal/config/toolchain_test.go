package config

import (
	"slices"
	"strings"
	"testing"
)

// TestAddonsAreValidatedAtLoad: an add-on spov has no recipe for, a bad
// package name, or add-ons on a custom image fail at load, not at launch.
//
// @decision(DL-033)
func TestAddonsAreValidatedAtLoad(t *testing.T) {
	ok := valid()
	ok.Sandbox.Addons = []string{"rust", "java@17"}
	ok.Sandbox.Apt = []string{"libvips-dev"}
	if err := Validate(ok); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, tt := range []struct {
		name   string
		break_ func(*Config)
		want   string
	}{
		{"an unknown add-on", func(c *Config) { c.Sandbox.Addons = []string{"docker"} }, "sandbox.addons"},
		{"a bad version", func(c *Config) { c.Sandbox.Addons = []string{"postgres@9"} }, "does not offer"},
		{"a bad package", func(c *Config) { c.Sandbox.Apt = []string{"foo;rm"} }, "sandbox.apt"},
		{"add-ons on a custom image", func(c *Config) { c.Sandbox.Image = "mine"; c.Sandbox.Addons = []string{"rust"} }, "sandbox.image"},
		{"packages on a custom image", func(c *Config) { c.Sandbox.Image = "mine"; c.Sandbox.Apt = []string{"jq"} }, "sandbox.image"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.break_(&c)
			err := Validate(c)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

// TestAddonsLayerAndBuildOnDemandDefaultsOn: a project's list replaces
// the user's, and on-demand builds are on unless switched off.
//
// @decision(DL-033) @decision(DL-016)
func TestAddonsLayerAndBuildOnDemandDefaultsOn(t *testing.T) {
	env, wd := newFixture(t, "sandbox:\n  addons: [rust, java]\n", "sandbox:\n  addons: [postgres]\n  apt: [jq]\n")
	res, err := Load(env, wd)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Config.Sandbox.Addons, []string{"postgres"}) || !slices.Equal(res.Config.Sandbox.Apt, []string{"jq"}) {
		t.Errorf("addons = %v, apt = %v", res.Config.Sandbox.Addons, res.Config.Sandbox.Apt)
	}
	if !res.Config.Modal.BuildOnDemand {
		t.Error("on-demand builds are off by default")
	}

	env, wd = newFixture(t, "", "")
	env.Vars["SPOV_MODAL_BUILD_ON_DEMAND"] = "false"
	if res, err = Load(env, wd); err != nil {
		t.Fatal(err)
	} else if res.Config.Modal.BuildOnDemand {
		t.Error("SPOV_MODAL_BUILD_ON_DEMAND=false did not switch it off")
	}
}
