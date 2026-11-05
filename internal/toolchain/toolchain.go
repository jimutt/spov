// Package toolchain is what a sandbox image carries besides OpenCode and
// the forge CLIs: the default languages, mise as the layer that serves a
// repository's own version pins, and the curated add-ons a project can
// opt into. It renders build commands and the facts the entrypoint and
// the workspace guide need, and calls nothing itself.
package toolchain

import (
	"fmt"
	"strings"
)

// Tool is one mise-managed tool at an exact version.
type Tool struct {
	Name    string
	Version string
}

// Defaults are the languages every image carries, installed through mise
// as global defaults.
//
// @decision(DL-032)
var Defaults = []Tool{
	{"python", "3.14.8"},
	{"uv", "0.12.22"},
	{"node", "24.21.0"},
	{"bun", "1.4.2"},
	{"go", "1.27.1"},
}

// CorepackVersion is installed from npm, since Node 25 and later no
// longer bundle it.
//
// @decision(DL-032)
const CorepackVersion = "0.36.0"

// MiseVersion is the pinned mise release.
//
// @decision(DL-032)
const MiseVersion = "2026.10.0"

// miseTargets maps a Debian architecture to mise's release target and
// the sha256 of that binary, from the release's SHASUMS256.txt.
//
// @decision(DL-032)
var miseTargets = []struct{ Arch, Target, SHA256 string }{
	{"amd64", "x64", "57ced973f968b8fbab07aa8e32bd7077d4a357e200a22356d98963c723c6de0a"},
	{"arm64", "arm64", "4b8cacffac83e8493fc5d1eef25f6365edba73ccbed5a1f3987b7cb3f5079656"},
}

// The directories tools live in. Everything installed globally lands in
// one of them, so nothing needs a `mise reshim`.
//
// @decision(DL-032)
const (
	MiseShims    = "/root/.local/share/mise/shims"
	npmGlobal    = "/root/.local/share/npm-global"
	NpmGlobalBin = npmGlobal + "/bin"
	GoBin        = "/root/go/bin"
	LocalBin     = "/root/.local/bin"
	CargoBin     = "/root/.cargo/bin"
	DotnetTools  = "/root/.dotnet/tools"
	miseConfig   = "/root/.config/mise/config.toml"
)

// PathDirs are the toolchain's directories in PATH order: mise's shims
// first, so a repository's pinned version wins over anything else.
//
// @decision(DL-032)
func PathDirs() []string {
	return []string{MiseShims, NpmGlobalBin, GoBin, LocalBin, CargoBin, DotnetTools}
}

// idiomaticTools are the tools whose own version files mise honours:
// .nvmrc, .python-version, rust-toolchain.toml, global.json and so on.
// mise leaves them off by default. Go is absent: GOTOOLCHAIN reads
// go.mod itself.
//
// @decision(DL-032)
var idiomaticTools = []string{"node", "python", "rust", "dotnet", "ruby", "java"}

// WorkspaceRoot is where every repository is cloned. mise trusts the
// configuration files under it.
const WorkspaceRoot = "/workspace"

// miseSettings is the global mise configuration the image writes.
//
// @decision(DL-032)
func miseSettings() []string {
	quoted := make([]string, len(idiomaticTools))
	for i, t := range idiomaticTools {
		quoted[i] = `"` + t + `"`
	}
	return []string{
		"[settings]",
		"idiomatic_version_file_enable_tools = [" + strings.Join(quoted, ", ") + "]",
		`trusted_config_paths = ["` + WorkspaceRoot + `"]`,
		"yes = true",
		// A bare `.java-version` such as 17 means Temurin, which the
		// java add-on installs, rather than mise's default vendor.
		`java.shorthand_vendor = "temurin"`,
	}
}

// withPath prefixes a build command with the toolchain's PATH. A build
// layer, like the sandbox entrypoint, sees no Dockerfile ENV.
func withPath(cmd string) string {
	return "export PATH=" + strings.Join(PathDirs(), ":") + ":$PATH && " + cmd
}

// Layers renders the image layers that install mise and the default
// languages. Each inner slice is one layer, so a change to the languages
// rebuilds only from there.
//
// @decision(DL-032)
func Layers() [][]string {
	var arches []string
	for _, t := range miseTargets {
		arches = append(arches, fmt.Sprintf("%s) t=%s; s=%s;;", t.Arch, t.Target, t.SHA256))
	}
	mise := []string{
		"RUN set -eu; a=$(dpkg --print-architecture); " +
			`case "$a" in ` + strings.Join(arches, " ") + ` *) echo "spov: no mise build for $a" >&2; exit 1;; esac; ` +
			`curl -fsSL -o /usr/local/bin/mise "https://github.com/jdx/mise/releases/download/v` + MiseVersion + `/mise-v` + MiseVersion + `-linux-$t"; ` +
			`echo "$s  /usr/local/bin/mise" | sha256sum -c -; chmod 755 /usr/local/bin/mise`,
		"RUN mkdir -p " + pathDir(miseConfig) + " && printf '%s\\n' " + shellWords(miseSettings()) + " > " + miseConfig +
			" && printf '%s\\n' 'prefix=" + npmGlobal + "' > /root/.npmrc",
	}
	var specs []string
	for _, t := range Defaults {
		specs = append(specs, t.Name+"@"+t.Version)
	}
	languages := []string{
		"RUN mise use -g " + strings.Join(specs, " "),
		"RUN " + withPath("npm install -g corepack@"+CorepackVersion+" && corepack enable --install-directory "+NpmGlobalBin),
	}
	return [][]string{mise, languages}
}

// Checks renders a build step that fails unless every default tool
// reports its pinned version.
//
// @decision(DL-032)
func Checks() string {
	checks := []string{
		`python --version | grep -qF "` + version(Defaults, "python") + `"`,
		`node --version | grep -qF "v` + version(Defaults, "node") + `"`,
		`go version | grep -qF "go` + version(Defaults, "go") + ` "`,
		`bun --version | grep -qxF "` + version(Defaults, "bun") + `"`,
		`uv --version | grep -qF "uv ` + version(Defaults, "uv") + ` "`,
		`corepack --version | grep -qxF "` + CorepackVersion + `"`,
		`mise --version | grep -qF "` + MiseVersion + ` "`,
	}
	return "RUN " + withPath(strings.Join(checks, " && "))
}

func version(tools []Tool, name string) string {
	for _, t := range tools {
		if t.Name == name {
			return t.Version
		}
	}
	return ""
}

func pathDir(p string) string {
	return p[:strings.LastIndex(p, "/")]
}

// shellWords single-quotes each word. None of the words here contains a
// single quote.
func shellWords(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = "'" + w + "'"
	}
	return strings.Join(quoted, " ")
}
