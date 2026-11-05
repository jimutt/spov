package opencode

import (
	"fmt"
	"strings"
)

// GhConfigDir is where gh keeps its configuration in the sandbox. Its
// hosts.yml holds tokens, so it is on the deny list.
//
// @decision(DL-021)
const GhConfigDir = SandboxHome + "/.config/gh"

// writeGhLogin authenticates gh through its config file rather than
// GH_TOKEN: the file can be rewritten in a running sandbox and an
// environment variable cannot. Each host has its own fragment under
// hosts.d, and hosts.yml is their concatenation, so rewriting one host
// leaves the others alone. hostExpr, userExpr and tokenExpr are shell
// words, expanded when the script runs.
//
// @decision(DL-021) @decision(DL-032)
func writeGhLogin(b *strings.Builder, home, hostExpr, userExpr, tokenExpr string) {
	dir := home + "/.config/gh"
	hosts := dir + "/hosts.d"
	fmt.Fprintf(b, "mkdir -p %s && chmod 700 %s %s\n", hosts, dir, hosts)
	// Marked as migrated: the older single-account hosts.yml makes gh
	// migrate it on first use, which asks GitHub who the token belongs to.
	fmt.Fprintf(b, "grep -qs '^version:' %[1]s/config.yml || printf 'version: \"1\"\\n' >> %[1]s/config.yml\n", dir)
	fmt.Fprintf(b, "chmod 600 %s/config.yml\n", dir)
	// The multi-account format, with the active account's token at the
	// top level as gh writes it for insecure storage.
	fmt.Fprintf(b, "(umask 077; printf '%%s:\\n    users:\\n        %%s:\\n            oauth_token: %%s\\n"+
		"    git_protocol: https\\n    user: %%s\\n    oauth_token: %%s\\n' "+
		"%[2]s %[3]s %[4]s %[3]s %[4]s > %[1]s/%[2]s.yml.tmp && mv %[1]s/%[2]s.yml.tmp %[1]s/%[2]s.yml)\n",
		hosts, hostExpr, userExpr, tokenExpr)
	fmt.Fprintf(b, "(umask 077; cat %[1]s/*.yml > %[2]s/hosts.yml.tmp && mv %[2]s/hosts.yml.tmp %[2]s/hosts.yml)\n",
		hosts, dir)
}

// GlabConfigDir is where glab keeps its configuration in the sandbox.
// Its config.yml holds the token, so it is on the deny list.
//
// @decision(DL-032)
const GlabConfigDir = SandboxHome + "/.config/glab-cli"

// glabHost is one GitLab host glab is configured for: its host, the
// shell word that expands to its token, and the path it is served under
// without slashes, empty at the root.
type glabHost struct {
	host, tokenExpr, subfolder string
}

// writeGlabConfig authenticates glab through its config file, as gh's is,
// rather than GITLAB_TOKEN. The file is written whole, every host at
// once, to a temporary name and moved into place, so a default config
// glab wrote earlier, or a second run, never leaves a stale or broken
// entry. The first host is glab's default, the update check, telemetry,
// the what's-new banner and prompts are off, and the file is 0600, which
// glab requires. Values are single-quoted YAML; a placeholder has no
// quote.
//
// @decision(DL-032)
func writeGlabConfig(b *strings.Builder, home string, hosts []glabHost) {
	dir := home + "/.config/glab-cli"
	fmt.Fprintf(b, "mkdir -p %[1]s && chmod 700 %[1]s\n", dir)
	fmt.Fprintf(b, "(umask 077; {\n")
	fmt.Fprintf(b, "printf 'check_update: false\\ntelemetry: false\\nshow_whats_new: false\\nno_prompt: true\\n"+
		"git_protocol: https\\nhost: %s\\nhosts:\\n'\n", yamlQuote(hosts[0].host))
	for _, h := range hosts {
		fmt.Fprintf(b, "printf '    %s:\\n        token: '\"'\"'%%s'\"'\"'\\n        api_host: %s\\n"+
			"        api_protocol: https\\n        git_protocol: https\\n' %s\n",
			yamlQuote(h.host), yamlQuote(h.host), h.tokenExpr)
		if h.subfolder != "" {
			fmt.Fprintf(b, "printf '        subfolder: %s\\n'\n", yamlQuote(h.subfolder))
		}
	}
	fmt.Fprintf(b, "} > %[1]s/config.yml.tmp && mv %[1]s/config.yml.tmp %[1]s/config.yml)\n", dir)
	fmt.Fprintf(b, "chmod 600 %s/config.yml\n", dir)
}

// yamlQuote is a single-quoted YAML scalar, safe inside a single-quoted
// printf format: a host or path has no quote, backslash or percent sign
// (url.Parse refuses them in a host), and anything that does is dropped.
func yamlQuote(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\'' || r == '\\' || r == '%' || r < 0x20 {
			return -1
		}
		return r
	}, s)
	return `'"'"'` + s + `'"'"'`
}

// RefreshArgs is the argv of a credential refresh: the script, then the
// host, the git user and whether gh is configured for that host. The
// token is not an argument; the exec's standard input carries it.
//
// @decision(DL-021)
func RefreshArgs(host, user string, gh bool) []string {
	flag := "0"
	if gh {
		flag = "1"
	}
	return []string{"bash", "-c", refreshScript(SandboxHome), "spov-refresh", host, user, flag}
}

// refreshScript replaces one host's token in the git credential store
// and, when gh is configured for it, in gh's hosts.yml. The store is
// written by git's own credential-store, which takes git's lock and keeps
// every other host's entry; gh's files are written to a temporary name
// and moved into place.
//
// @decision(DL-021)
func refreshScript(home string) string {
	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString("IFS= read -r token\n")
	b.WriteString(`[ -n "$token" ] || { echo "no token on stdin" >&2; exit 2; }` + "\n")
	b.WriteString("host=$1 user=$2 gh=$3\n")
	fmt.Fprintf(&b, "(umask 077; printf 'protocol=https\\nhost=%%s\\nusername=%%s\\npassword=%%s\\n\\n' \"$host\" \"$user\" \"$token\" "+
		"| git credential-store --file %s store)\n", home+"/.git-credentials")
	b.WriteString(`if [ "$gh" = 1 ]; then` + "\n")
	writeGhLogin(&b, home, `"$host"`, `"$user"`, `"$token"`)
	b.WriteString("fi\n")
	return b.String()
}
