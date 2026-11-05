package sandbox

import (
	"slices"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/toolchain"
)

func addons(t *testing.T, names ...string) []toolchain.Selection {
	t.Helper()
	sel, err := toolchain.Parse(names)
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

// TestTheBuildIsLayeredLeastChangedFirst: system packages, then mise and
// the languages, then add-ons and extra packages, then the forge CLIs,
// with OpenCode last, so a pin bump or a variant rebuilds only its own
// layers.
//
// @decision(DL-032)
func TestTheBuildIsLayeredLeastChangedFirst(t *testing.T) {
	spec := ImageSpec{OpenCodeVersion: "2.0.22", Addons: addons(t, "rust", "postgres"), ExtraPackages: []string{"libvips-dev"}}
	layers := spec.Layers()
	index := func(needle string) int {
		for i, l := range layers {
			if strings.Contains(strings.Join(l, "\n"), needle) {
				return i
			}
		}
		t.Fatalf("no layer contains %q", needle)
		return -1
	}
	order := []int{
		index("ripgrep"),
		index("mise-v" + toolchain.MiseVersion),
		index("mise use -g python@"),
		index("postgresql"),
		index("mise use -g rust@"),
		index("libvips-dev"),
		index("tea --version"),
		index("cli-$t-2.0.22.tgz"),
	}
	if !slices.IsSorted(order) || order[len(order)-1] != len(layers)-1 {
		t.Errorf("layer order = %v of %d layers", order, len(layers))
	}
	if baseImage != "debian:trixie-slim" {
		t.Errorf("base image = %s", baseImage)
	}
	for _, c := range spec.DockerfileCommands() {
		if strings.Contains(c, "\n") {
			t.Errorf("a command spans lines:\n%s", c)
		}
	}
}

// TestAVariantIsItsOwnImage: add-ons change the name, the same set in
// another order does not, and a different version does.
//
// @decision(DL-033)
func TestAVariantIsItsOwnImage(t *testing.T) {
	base := ImageSpec{OpenCodeVersion: "2.0.22"}.Name()
	rust := ImageSpec{OpenCodeVersion: "2.0.22", Addons: addons(t, "rust", "java")}.Name()
	same := ImageSpec{OpenCodeVersion: "2.0.22", Addons: addons(t, "java", "rust")}.Name()
	older := ImageSpec{OpenCodeVersion: "2.0.22", Addons: addons(t, "rust@1.98.0", "java")}.Name()
	if rust == base {
		t.Error("an add-on variant has the base image's name")
	}
	if rust != same {
		t.Errorf("the same add-ons in another order are another image: %s vs %s", rust, same)
	}
	if rust == older {
		t.Error("another add-on version is the same image")
	}
}

// TestExtraPackagesTheBaseHasAddNoLayer.
func TestExtraPackagesTheBaseHasAddNoLayer(t *testing.T) {
	plain := ImageSpec{OpenCodeVersion: "v1"}
	if got := len(ImageSpec{OpenCodeVersion: "v1", ExtraPackages: []string{"git", " jq "}}.Layers()); got != len(plain.Layers()) {
		t.Errorf("re-listing base packages added a layer: %d vs %d", got, len(plain.Layers()))
	}
}

// TestValidateRefusesASelectionParseWouldNotProduce: a hand-built
// selection is interpolated into a build command, so it must be one
// Parse could have returned, in Parse's order.
//
// @decision(DL-033)
func TestValidateRefusesASelectionParseWouldNotProduce(t *testing.T) {
	for _, sel := range [][]toolchain.Selection{
		{{Name: "rust", Version: "1.0; rm -rf /"}},
		{{Name: "docker", Version: "1"}},
		{{Name: "rust", Version: "1.99.0"}, {Name: "java", Version: "17"}},
	} {
		if err := (ImageSpec{OpenCodeVersion: "v1", Addons: sel}).Validate(); err == nil {
			t.Errorf("Validate accepted %v", sel)
		}
	}
	if err := (ImageSpec{OpenCodeVersion: "v1", Addons: addons(t, "java", "rust")}).Validate(); err != nil {
		t.Errorf("Validate refused a parsed selection: %v", err)
	}
}

// TestTheGuideNamesOnlyBasePackages: the workspace guide's list of system
// tools is written by hand, so each must be one the image installs.
//
// @decision(DL-033)
func TestTheGuideNamesOnlyBasePackages(t *testing.T) {
	guide := toolchain.Guide(nil)
	start := strings.Index(guide, "plus ")
	end := strings.Index(guide[start:], ".\n")
	if start < 0 || end < 0 {
		t.Fatalf("cannot find the system tools in the guide:\n%s", guide)
	}
	list := strings.NewReplacer("plus ", "", " and ", ", ").Replace(guide[start : start+end])
	alias := map[string]string{"fd": "fd-find"}
	for _, name := range strings.Split(list, ", ") {
		pkg := strings.TrimSpace(name)
		if a, ok := alias[pkg]; ok {
			pkg = a
		}
		if !slices.Contains(basePackages, pkg) {
			t.Errorf("the guide names %q, which the image does not install", name)
		}
	}
}

// The upstream apt repositories are asked for the base image's own
// Debian release.
//
// @decision(DL-033)
func TestTheAddonRepositoriesFollowTheBaseRelease(t *testing.T) {
	if !strings.HasPrefix(baseImage, "debian:"+toolchain.DebianSuite) {
		t.Errorf("the base image is %s, but the add-on repositories are asked for %s", baseImage, toolchain.DebianSuite)
	}
}
