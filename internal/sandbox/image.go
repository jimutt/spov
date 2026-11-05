package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/jimutt/spov/internal/toolchain"
)

// ImageSpec describes the base image spov boots sandboxes from.
//
// @decision(DL-032)
type ImageSpec struct {
	// OpenCodeVersion is the exact OpenCode build baked in.
	OpenCodeVersion string
	// ExtraPackages are additional apt packages for a project variant.
	ExtraPackages []string
	// Addons are the curated add-ons a variant carries, as
	// toolchain.Parse gave them, each at its resolved version when it
	// has one.
	//
	// @decision(DL-033)
	Addons []toolchain.Selection
}

// basePackages are the system packages every image carries: what
// OpenCode itself needs, and the default toolchain's build tools and
// utilities. ripgrep matters most: without `rg` on PATH the server
// downloads it from GitHub at runtime, which stalls cold start and fails
// outright under restricted egress. xz-utils unpacks toolchain archives.
//
// @decision(DL-032)
var basePackages = []string{
	"bash",
	"build-essential",
	"ca-certificates",
	"curl",
	"fd-find",
	"git",
	"jq",
	"make",
	"openssh-client",
	"pkg-config",
	"ripgrep",
	"sqlite3",
	"tar",
	"unzip",
	"xz-utils",
	"zip",
}

// baseImage is the registry image the spov image is built from. Debian
// slim gives a glibc/GNU userland; OpenCode's Bun binary does not run on
// musl.
//
// @decision(DL-032)
const baseImage = "debian:trixie-slim"

// imageBinaryPath is where the image installs opencode, whatever the
// package calls its binary. The entrypoint runs it from here.
//
// @decision(DL-032) @decision(DL-035)
const imageBinaryPath = "/root/.opencode/bin/opencode"

// openCodeRelease is how one OpenCode build is published on npm: the
// scope of its platform packages, the binary's name inside them, and
// npm's dist.integrity for each target the image installs.
//
// @decision(DL-032)
type openCodeRelease struct {
	Scope     string
	Binary    string
	Integrity map[string]string
}

// The npm targets the image installs, by Debian architecture. x64 is the
// baseline build, which runs on any x86-64 host.
//
// @decision(DL-032)
var openCodeTargets = []struct{ Arch, Target string }{
	{"amd64", "linux-x64-baseline"},
	{"arm64", "linux-arm64"},
}

// openCodeReleases are the builds whose integrity was recorded when the
// pin moved to them. Print a value with
// `npm view <scope>/cli-<target>@<version> dist.integrity`.
//
// @decision(DL-032)
var openCodeReleases = map[string]openCodeRelease{
	"2.0.22": {Scope: "@opencode", Binary: "opencode", Integrity: map[string]string{
		"linux-x64-baseline": "sha512-nwVyjnug68v1A0emkDf28GUM37m5PjiFgu2Xbn3ujNHdnSd0v43Yr5G7TD1XTvAbXg+Zv46hX0CvINJorLuOFA==",
		"linux-arm64":        "sha512-t/yuu9Dqd/M44dFH95bphu7ikO6FATMQuVk/91QBnCMzsP8zVrXU7zAxD1wVLpYYUDqIonEmi80XETlFqx7chw==",
	}},
}

// unrecordedRelease is how a version with no entry is taken to be
// published. Its integrity is read from the registry when the image
// builds.
//
// @decision(DL-032)
var unrecordedRelease = openCodeRelease{Scope: "@opencode", Binary: "opencode"}

// release is how this spec's version is published.
func (s ImageSpec) release() openCodeRelease {
	if r, ok := openCodeReleases[s.OpenCodeVersion]; ok {
		return r
	}
	return unrecordedRelease
}

// The git-forge CLIs baked into the image, at pinned versions from
// release artefacts since none is in Debian. Each download is checked
// against the sha256 below before it is installed; a version bump
// changes both.
//
// @decision(DL-032)
const (
	// TeaVersion is the Forgejo/Gitea CLI.
	TeaVersion = "0.15.1"
	// GhVersion is the GitHub CLI.
	GhVersion = "2.98.0"
	// GlabVersion is the GitLab CLI.
	GlabVersion = "1.120.0"
)

// cliSHA256 is each CLI's release artefact's sha256 per Debian
// architecture, from the release's own checksums file: tea's binary,
// gh's and glab's tarball.
//
// @decision(DL-032)
var cliSHA256 = map[string]map[string]string{
	"tea": {
		"amd64": "aac99cc6e650a81ae7b5061f8c75bc0eade4509c828d97b6072e1f0a3bd24357",
		"arm64": "0db109df6696bfe01f9203402f503404692404d4ea9c16a540ecaeecc8e6bab2",
	},
	"gh": {
		"amd64": "3b8ac6b30336802fc1a858d7c084e11cdf24ac1a761ca90b68022d7d729208de",
		"arm64": "cf689084f3a3618f7eae4a2420d335d74626d65f5e594b9828d125d69f800d86",
	},
	"glab": {
		"amd64": "4e6c59de9f7ed2f304bf93aad01ea8f8a69584f0450ce90ad696ef81f69c69aa",
		"arm64": "c60ebb4cb36f276714847a118845b9b4e69f46a8080c68b21ca63f158722cdea",
	},
}

// pickSHA256 is the shell that sets $a to the architecture and $s to the
// CLI's sha256 for it, or fails the build on an architecture with none.
func pickSHA256(cli string) string {
	sums := cliSHA256[cli]
	var cases []string
	for _, arch := range slices.Sorted(maps.Keys(sums)) {
		cases = append(cases, fmt.Sprintf("%s) s=%s;;", arch, sums[arch]))
	}
	return `a=$(dpkg --print-architecture); case "$a" in ` + strings.Join(cases, " ") +
		` *) echo "spov: no ` + cli + ` build for $a" >&2; exit 1;; esac; `
}

// extraPackages are the requested packages the base does not already
// install.
func (s ImageSpec) extraPackages() []string { return ExtraPackages(s.ExtraPackages) }

// ExtraPackages trims pkgs, drops those the base image already installs,
// and returns the rest sorted and de-duplicated: the canonical form of a
// variant's apt list.
//
// @decision(DL-033)
func ExtraPackages(pkgs []string) []string {
	var out []string
	for _, p := range pkgs {
		if p = strings.TrimSpace(p); p != "" && !slices.Contains(basePackages, p) && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// ValidatePackages refuses an apt package name that is not safe to put in
// a build command.
//
// @decision(DL-033)
func ValidatePackages(pkgs []string) error {
	for _, p := range pkgs {
		if p = strings.TrimSpace(p); p != "" && !validPackageName(p) {
			return fmt.Errorf("sandbox: apt package %q is not a valid package name", p)
		}
	}
	return nil
}

// Packages returns the full sorted, de-duplicated apt package list.
func (s ImageSpec) Packages() []string {
	all := slices.Clone(basePackages)
	for _, p := range s.ExtraPackages {
		if p = strings.TrimSpace(p); p != "" && !slices.Contains(all, p) {
			all = append(all, p)
		}
	}
	slices.Sort(all)
	return all
}

// Name is the published image name. It encodes the OpenCode version so a
// version bump is an explicit new image rather than a silent rebuild, and
// hashes any extra packages so variants do not collide.
//
// @decision(DL-032)
func (s ImageSpec) Name() string {
	name := "spov-base-" + s.OpenCodeVersion
	if extra := s.extraSuffix(); extra != "" {
		name += "-" + extra
	}
	return name
}

// extraSuffix distinguishes images that differ in anything but the
// OpenCode version: extra apt packages, a bump to a baked-in CLI, or any
// other change to the recipe. It hashes the base image and every
// rendered command, so no change reuses an image built from another recipe.
//
// @decision(DL-032)
func (s ImageSpec) extraSuffix() string {
	return shortHash(baseImage + "\n" + strings.Join(s.DockerfileCommands(), "\n"))
}

// DockerfileCommands renders the image build steps, every layer's in
// order.
//
// @decision(DL-032) @decision(DL-035)
func (s ImageSpec) DockerfileCommands() []string {
	var all []string
	for _, l := range s.Layers() {
		all = append(all, l...)
	}
	return all
}

// Layers renders the build as a chain of layers, from the least to the
// most often changed, so a change rebuilds only its own layer and those
// after it, and a variant builds only its add-ons on a cached base.
//
// @decision(DL-032) @decision(DL-033)
func (s ImageSpec) Layers() [][]string {
	layers := [][]string{{
		"RUN apt-get update && apt-get install -y --no-install-recommends " +
			strings.Join(basePackages, " ") +
			" && rm -rf /var/lib/apt/lists/* && ln -sf /usr/bin/fdfind /usr/local/bin/fd && mkdir -p /workspace",
	}}
	langs := toolchain.Layers()
	langs[len(langs)-1] = append(langs[len(langs)-1], toolchain.Checks())
	layers = append(layers, langs...)
	for _, a := range s.Addons {
		layers = append(layers, a.Layer())
	}
	if extra := s.extraPackages(); len(extra) > 0 {
		layers = append(layers, []string{
			"RUN apt-get update && apt-get install -y --no-install-recommends " +
				strings.Join(extra, " ") + " && rm -rf /var/lib/apt/lists/*",
		})
	}
	layers = append(layers,
		[]string{
			teaInstallCommand(),
			ghInstallCommand(),
			glabInstallCommand(),
			// Fail the build, not the first session, if a CLI is broken.
			// glab writes a default config wherever it runs, so it runs
			// against a throwaway one and the image keeps none.
			"RUN tea --version && gh --version && GLAB_CONFIG_DIR=/tmp/glab-check glab --version && " +
				"rm -rf /tmp/glab-check /root/.config/glab-cli",
		},
		[]string{
			"RUN " + s.openCodeInstall(imageBinaryPath),
			// Fail the build, not the first sandbox, if the toolchain is
			// missing or the pinned version did not install.
			"RUN " + s.openCodeVersionCheck(imageBinaryPath) + " && rg --version | head -1",
		},
	)
	return layers
}

// openCodeInstall renders the shell that installs the pinned build at
// path: the platform package from npm, checked against its integrity
// before it is unpacked. It runs under sh, so it uses no bashisms.
//
// @decision(DL-032) @decision(DL-035)
func (s ImageSpec) openCodeInstall(bin string) string {
	r := s.release()
	var arches []string
	for _, t := range openCodeTargets {
		arches = append(arches, fmt.Sprintf("%s) t=%s; i='%s';;", t.Arch, t.Target, r.Integrity[t.Target]))
	}
	pkg := "https://registry.npmjs.org/" + r.Scope + "/cli-$t"
	steps := []string{
		"set -eu",
		`a=$(dpkg --print-architecture)`,
		`case "$a" in ` + strings.Join(arches, " ") + ` *) echo "spov: no OpenCode build for $a" >&2; exit 1;; esac`,
		// An unrecorded version takes the registry's own value.
		`if [ -z "$i" ]; then i=$(curl -fsSL "` + pkg + "/" + s.OpenCodeVersion + `" | grep -o '"integrity":"sha512-[^"]*"' | head -n 1 | cut -d '"' -f 4); ` +
			`if [ -z "$i" ]; then echo "spov: the registry gave no sha512 integrity for OpenCode ` + s.OpenCodeVersion + `" >&2; exit 1; fi; ` +
			`echo "spov: OpenCode ` + s.OpenCodeVersion + ` has no recorded integrity; the registry gives $i"; fi`,
		`tgz=$(mktemp)`,
		`curl -fsSL -o "$tgz" "` + pkg + "/-/cli-$t-" + s.OpenCodeVersion + `.tgz"`,
		// npm's integrity is base64; sha512sum prints hex.
		`want=$(printf %s "${i#sha512-}" | base64 -d | od -An -v -tx1 | tr -dc '0-9a-f')`,
		`got=$(sha512sum "$tgz" | cut -d ' ' -f 1)`,
		`if [ -z "$want" ] || [ "$got" != "$want" ]; then echo "spov: the OpenCode package does not match its integrity $i" >&2; exit 1; fi`,
		`mkdir -p "` + path.Dir(bin) + `"`,
		`tar -xzOf "$tgz" package/bin/` + r.Binary + ` > "` + bin + `"`,
		`chmod 755 "` + bin + `"`,
		`rm -f "$tgz"`,
	}
	return strings.Join(steps, "; ")
}

// openCodeVersionCheck renders a check that the binary at bin reports
// the pinned version, as `<name> v<version>` or a bare version.
//
// @decision(DL-032)
func (s ImageSpec) openCodeVersionCheck(bin string) string {
	return `v=$("` + bin + `" --version | tail -n 1); v=${v##* }; v=${v#v}; ` +
		`if [ "$v" != "` + s.OpenCodeVersion + `" ]; then echo "spov: ` + bin + ` reports $v, not ` + s.OpenCodeVersion + `" >&2; exit 1; fi`
}

// Validate checks an ImageSpec before a build is attempted.
func (s ImageSpec) Validate() error {
	if s.OpenCodeVersion == "" {
		return fmt.Errorf("sandbox: image spec needs an OpenCode version")
	}
	if !validVersion(s.OpenCodeVersion) {
		return fmt.Errorf("sandbox: OpenCode version %q contains characters that are not safe in a shell command", s.OpenCodeVersion)
	}
	if err := ValidatePackages(s.ExtraPackages); err != nil {
		return err
	}
	// A selection is interpolated into a build command, so it must be
	// one Parse would produce, not merely one that looks like it, and
	// its resolved version one Resolve accepts.
	//
	// @decision(DL-033)
	again, err := toolchain.Parse(toolchain.Strings(s.Addons))
	if err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	if !slices.Equal(again, toolchain.Unresolved(s.Addons)) {
		return fmt.Errorf("sandbox: add-ons %v are not as Parse gives them", toolchain.Strings(s.Addons))
	}
	for i, a := range s.Addons {
		if r, err := again[i].Resolve(a.Resolved); err != nil {
			return fmt.Errorf("sandbox: %w", err)
		} else if r != a {
			return fmt.Errorf("sandbox: add-on %s is not as Resolve gives it", a.Installed())
		}
	}
	return nil
}

// Requested is the spec at the add-on versions as they were asked for.
// Its name is the key the newest build of a request is recorded under.
//
// @decision(DL-016)
func (s ImageSpec) Requested() ImageSpec {
	s.Addons = toolchain.Unresolved(s.Addons)
	return s
}

// shortHash gives a stable, filename-safe suffix distinguishing image
// variants. A caller chooses a variant's recipe through sandbox.apt, so
// the suffix is long enough that no one can search for a recipe whose
// name collides with another's.
//
// @decision(DL-016)
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:32]
}

// validVersion reports whether a version string is safe to interpolate
// into the image build command.
func validVersion(v string) bool {
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return v != ""
}

// validPackageName reports whether s is a plausible apt package name. It
// is deliberately strict: the name is interpolated into a shell command,
// and a leading '-' would reach apt-get as an option.
func validPackageName(s string) bool {
	if s == "" || s[0] == '-' || s[0] == '.' || s[0] == '+' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '+':
		default:
			return false
		}
	}
	return s != ""
}

// teaInstallCommand fetches the Forgejo CLI, which ships as a plain
// static binary, and checks it before installing it.
//
// @decision(DL-032)
func teaInstallCommand() string {
	return fmt.Sprintf(
		"RUN set -eu; "+pickSHA256("tea")+
			"curl -fsSL -o /tmp/tea https://gitea.com/gitea/tea/releases/download/v%s/tea-%s-linux-$a; "+
			`echo "$s  /tmp/tea" | sha256sum -c -; `+
			"install -m 755 /tmp/tea /usr/local/bin/tea; rm -f /tmp/tea",
		TeaVersion, TeaVersion)
}

// ghInstallCommand fetches the GitHub CLI, which ships as a tarball, and
// checks the tarball before unpacking it.
//
// @decision(DL-032)
func ghInstallCommand() string {
	return fmt.Sprintf(
		"RUN set -eu; "+pickSHA256("gh")+
			"curl -fsSL -o /tmp/gh.tgz https://github.com/cli/cli/releases/download/v%s/gh_%s_linux_$a.tar.gz; "+
			`echo "$s  /tmp/gh.tgz" | sha256sum -c -; `+
			"tar -xzf /tmp/gh.tgz -C /tmp; install -m 755 /tmp/gh_%s_linux_$a/bin/gh /usr/local/bin/gh; "+
			"rm -rf /tmp/gh.tgz /tmp/gh_%s_linux_$a",
		GhVersion, GhVersion, GhVersion, GhVersion)
}

// glabInstallCommand fetches the GitLab CLI, which ships as a tarball
// from GitLab's package registry, and checks it before unpacking it.
//
// @decision(DL-032)
func glabInstallCommand() string {
	return fmt.Sprintf(
		"RUN set -eu; "+pickSHA256("glab")+
			"curl -fsSL -o /tmp/glab.tgz "+
			"https://gitlab.com/api/v4/projects/gitlab-org%%2Fcli/packages/generic/glab/%s/glab_%s_linux_$a.tar.gz; "+
			`echo "$s  /tmp/glab.tgz" | sha256sum -c -; `+
			"mkdir -p /tmp/glab && tar -xzf /tmp/glab.tgz -C /tmp/glab; "+
			"install -m 755 /tmp/glab/bin/glab /usr/local/bin/glab; rm -rf /tmp/glab.tgz /tmp/glab",
		GlabVersion, GlabVersion)
}

// proxyBaseImage is the registry image the proxy image starts from. The
// proxy is a static binary and needs only the CA roots to verify the
// forges it forwards to.
//
// @decision(DL-034)
const proxyBaseImage = "debian:bookworm-slim"

// proxyBaseCommands add the CA roots to proxyBaseImage.
//
// @decision(DL-034)
var proxyBaseCommands = []string{
	"RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*",
}

// ProxyBinaryPath is where the proxy image carries the binary. It is
// credproxy.BinaryPath, which this package does not import;
// TestTheProxyBinaryPathsAgree keeps the two equal.
//
// @decision(DL-034)
const ProxyBinaryPath = "/usr/local/bin/spov-proxy"

// ProxyImageName is the published name of the image carrying binary. It
// covers the base image too, so a change to either is a new image.
//
// @decision(DL-034)
func ProxyImageName(binary []byte) string {
	h := sha256.New()
	h.Write([]byte(proxyBaseImage + "\n" + strings.Join(proxyBaseCommands, "\n") + "\n"))
	h.Write(binary)
	return "spov-proxy-" + hex.EncodeToString(h.Sum(nil))[:16]
}
