// Package opencode builds the contract between spov and the opencode
// server running inside a sandbox.
//
// @decision(DL-036)
package opencode

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/toolchain"
)

// DefaultPort is the port the server listens on inside the sandbox.
//
// @decision(DL-036)
const DefaultPort = 4096

// WorkspaceRoot is where bootstrapped repos are cloned.
const WorkspaceRoot = "/workspace"

// ScratchDir is every sandbox's scratchpad, created before the server
// starts and named in the generated AGENTS.md.
//
// @decision(DL-037)
const ScratchDir = WorkspaceRoot + "/.scratch"

// reservedDirs are the directories under WorkspaceRoot that spov owns,
// which no clone may take: a report job's output directory and the
// scratchpad. git.ReservedDirs holds the same names.
//
// @decision(DL-037)
var reservedDirs = []string{path.Base(OutDir), path.Base(ScratchDir)}

// imageBinary is where the image bakes opencode, and where the entrypoint
// runs it.
//
// @decision(DL-032) @decision(DL-035)
const imageBinary = "/root/.opencode/bin/opencode"

// Clone is one repository to bootstrap before the server starts.
type Clone struct {
	// URL is the clone URL, which may embed a short-lived credential.
	URL string
	// Dir is the directory name under WorkspaceRoot.
	Dir string
}

// Entrypoint describes the command the sandbox runs.
//
// @decision(DL-036)
type Entrypoint struct {
	// Clones are bootstrapped before the server starts. They run
	// concurrently, so completion order is unspecified.
	//
	// @decision(DL-036)
	Clones []Clone
	// Landing is the directory under WorkspaceRoot the server starts in.
	// Empty means WorkspaceRoot itself, and nothing defaults it to a
	// repository.
	//
	// @decision(DL-037)
	Landing string
	// Port the server listens on.
	Port int
	// Workspace describes the cloned repositories for the generated
	// AGENTS.md. Empty writes no file.
	Workspace []WorkspaceRepo
	// Logins authenticate the forge CLIs. Their tokens are read from the
	// environment, never written into the script.
	Logins []CLILogin
	// GitName and GitEmail are the identity commits are attributed to.
	// Either may be empty, in which case that half is left unset and git
	// falls back to its own guess.
	//
	// @decision(DL-044)
	GitName  string
	GitEmail string
	// Job, when set, makes this an autonomous job's session: the
	// spov-goal plugin is written and DL-038's external-directory grant
	// is not. The two are alternatives rather than layers.
	//
	// @decision(DL-039) @decision(DL-040)
	Job *Plugin
	// Model is OpenCode's own provider/model identifier. Empty leaves
	// the key out and OpenCode resolves its own default.
	//
	// @decision(DL-014) @decision(DL-038)
	Model string
	// ModelUnlisted declares Model in the config, for a model spov's
	// list lacks.
	//
	// @decision(DL-038)
	ModelUnlisted bool
	// Proxied means the sandbox has the credential proxy sidecar: the
	// entrypoint installs its CA and waits for it before cloning.
	//
	// @decision(DL-034)
	Proxied bool
	// Addons are the image's add-ons: their services are started, and
	// the workspace guide names them.
	//
	// @decision(DL-033)
	Addons []toolchain.Selection
}

// ProxyWaitExitCode is the entrypoint's exit code when the proxy sidecar
// never answers, so a failed launch says which part failed.
//
// @decision(DL-034)
const ProxyWaitExitCode = 97

// proxyWaitTenths bounds the wait for the proxy, in tenths of a second.
const proxyWaitTenths = 600

// CLILogin configures one forge CLI inside the sandbox.
//
// @decision(DL-032)
type CLILogin struct {
	// CLI is the tool to configure.
	CLI ForgeCLI
	// Name is the login name, matching the configured platform.
	Name string
	// URL is the platform root, e.g. https://forge.example.com.
	URL string
	// User is the account the token belongs to.
	User string
	// TokenEnv names the environment variable holding the token. The
	// token itself never appears in the script: any process in the
	// sandbox can read another's command line.
	TokenEnv string
	// BasePath is the path the forge is served under, such as "/gitlab",
	// empty at the root. Only glab is told it.
	//
	// @decision(DL-032)
	BasePath string
}

// Script renders the entrypoint shell script.
//
// @decision(DL-036)
func (e Entrypoint) Script() (string, error) {
	if err := e.Validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("set -e\n")
	e.writeAddons(&b)

	e.writeProxyWait(&b)
	e.writeGitCredentials(&b)
	e.writeGitIdentity(&b)

	e.writeClones(&b)

	// The reports' directory exists from the start, so a listing that
	// fails at delivery is a broken read and not an empty job.
	//
	// @decision(DL-037) @decision(DL-051)
	fmt.Fprintf(&b, "mkdir -p %s %s\n", shellQuote(ScratchDir), shellQuote(OutDir))
	e.writeWorkspaceGuide(&b)
	e.writeOpenCodeConfig(&b)
	if err := e.writePlugin(&b); err != nil {
		return "", err
	}
	e.writeLogins(&b)

	fmt.Fprintf(&b, "cd %s\n", shellQuote(e.WorkingDir()))
	// @decision(DL-032)
	fmt.Fprintf(&b, "exec %s serve --hostname 0.0.0.0 --port %d\n", imageBinary, e.Port)
	return b.String(), nil
}

// CloneConcurrency bounds how many clones run at once.
//
// @decision(DL-036)
const CloneConcurrency = 5

// writeClones emits the clone commands, running up to CloneConcurrency
// at a time. Each background job is waited on by its own PID; a bare
// `wait` returns 0 even when a job failed, including under `set -e`.
//
// @decision(DL-036) @decision(DL-041)
func (e Entrypoint) writeClones(b *strings.Builder) {
	if len(e.Clones) == 0 {
		return
	}

	// A single clone needs none of the job machinery, and reads better
	// in a log without it.
	if len(e.Clones) == 1 {
		b.WriteString(cloneCommand(e.Clones[0]) + "\n")
		return
	}

	b.WriteString("spov_pids=''\n")
	for i, c := range e.Clones {
		// Drain the batch before starting the next one, so no more than
		// CloneConcurrency clones compete for the sandbox's CPU.
		if i > 0 && i%CloneConcurrency == 0 {
			b.WriteString(waitForClones())
		}
		fmt.Fprintf(b, "%s & spov_pids=\"$spov_pids $!\"\n", cloneCommand(c))
	}
	b.WriteString(waitForClones())
}

// waitForClones waits on every started clone and clears the list.
//
// @decision(DL-036)
func waitForClones() string {
	return "for spov_pid in $spov_pids; do wait \"$spov_pid\"; done\n" +
		"spov_pids=''\n"
}

// cloneCommand renders one clone. "--" stops git parsing later words as
// options; shell quoting alone does not help, since a quoted
// "--upload-pack=..." is one shell word but still an option to git.
//
// @decision(DL-041)
func cloneCommand(c Clone) string {
	dir := path.Join(WorkspaceRoot, c.Dir)
	return fmt.Sprintf("git clone --depth 1 -- %s %s", shellQuote(c.URL), shellQuote(dir))
}

// writeGitCredentials configures a git credential store per host, so
// clone URLs stay free of secrets: an embedded token would be readable
// by anything running `git remote -v` and stops `tea` matching a
// repository to its login.
//
// @decision(DL-041) @decision(DL-032)
func (e Entrypoint) writeGitCredentials(b *strings.Builder) {
	if len(e.Logins) == 0 {
		return
	}
	b.WriteString("mkdir -p /root\n")
	b.WriteString("git config --global credential.helper 'store --file=/root/.git-credentials'\n")
	b.WriteString("touch /root/.git-credentials && chmod 600 /root/.git-credentials\n")
	for _, l := range e.Logins {
		// The token is expanded by the shell from the environment, so it
		// never appears in the script itself.
		fmt.Fprintf(b, "printf '%%s\\n' \"https://%s:$%s@%s\" >> /root/.git-credentials\n",
			l.User, l.TokenEnv, hostOf(l.URL))
	}
}

// writeProxyWait installs the proxy's CA, which arrives in the
// environment, into its own file and the system bundle, and waits for the
// proxy to answer. Everything after it may go out through the proxy.
//
// @decision(DL-034)
func (e Entrypoint) writeProxyWait(b *strings.Builder) {
	if !e.Proxied {
		return
	}
	fmt.Fprintf(b, "mkdir -p %s\n", shellQuote(path.Dir(credproxy.MainCAFile)))
	fmt.Fprintf(b, "printf '%%s\\n' \"$%s\" > %s\n", credproxy.EnvMainCA, shellQuote(credproxy.MainCAFile))
	fmt.Fprintf(b, "cat %s >> %s\n", shellQuote(credproxy.MainCAFile), shellQuote(credproxy.SystemBundle))
	fmt.Fprintf(b, "spov_i=0; until (exec 3<>/dev/tcp/%s/%d) 2>/dev/null; do "+
		"spov_i=$((spov_i+1)); if [ \"$spov_i\" -ge %d ]; then echo 'spov: the proxy sidecar never answered' >&2; exit %d; fi; sleep 0.1; done\n",
		credproxy.SidecarName, credproxy.Port, proxyWaitTenths, ProxyWaitExitCode)
}

// hostOf strips the scheme from a platform URL.
func hostOf(rawURL string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://"), "/")
}

// writeWorkspaceGuide emits the generated AGENTS.md via a quoted
// heredoc, so nothing in the content is expanded by the shell.
//
// @decision(DL-032) @decision(DL-037)
func (e Entrypoint) writeWorkspaceGuide(b *strings.Builder) {
	if len(e.Workspace) == 0 {
		return
	}
	kind := ""
	if e.Job != nil {
		kind = e.Job.Kind
	}
	fmt.Fprintf(b, "cat > %s <<'SPOV_AGENTS_EOF'\n", AgentsFile)
	b.WriteString(AgentsMarkdown(e.Workspace, kind))
	// @decision(DL-033)
	b.WriteString("\n" + toolchain.Guide(e.Addons))
	b.WriteString("SPOV_AGENTS_EOF\n")
}

// writeAddons starts the add-ons' services in the background without
// waiting, so they boot while the clones run. Their environment is the
// sandbox's own (session.Launch), not exported here.
//
// @decision(DL-033)
func (e Entrypoint) writeAddons(b *strings.Builder) {
	for _, svc := range toolchain.Services(e.Addons) {
		fmt.Fprintf(b, "(%s) >%s 2>&1 &\n", svc.Start, shellQuote(svc.Log))
	}
}

// writeGitIdentity configures the author for commits made in the
// sandbox.
//
// @decision(DL-044)
func (e Entrypoint) writeGitIdentity(b *strings.Builder) {
	if e.GitName != "" {
		fmt.Fprintf(b, "git config --global user.name %s\n", shellQuote(e.GitName))
	}
	if e.GitEmail != "" {
		fmt.Fprintf(b, "git config --global user.email %s\n", shellQuote(e.GitEmail))
	}
}

// writeOpenCodeConfig writes the global OpenCode config.
//
// ConfigDir is the one interpolated word here that is not shellQuoted:
// it carries a literal $HOME for the shell to expand, so it is quoted
// in the emitted script rather than escaped.
//
// A job session gets a document carrying no permission key: its policy
// is the plugin's permission hook alone.
//
// @decision(DL-038) @decision(DL-040)
func (e Entrypoint) writeOpenCodeConfig(b *strings.Builder) {
	fmt.Fprintf(b, "mkdir -p \"%s\"\n", ConfigDir)
	config := SandboxConfigJSON(e.Model, e.ModelUnlisted)
	if e.Job != nil {
		config = JobConfigJSON(e.Model, e.ModelUnlisted)
	}
	fmt.Fprintf(b, "cat > \"%s\" <<'SPOV_CONFIG_EOF'\n", ConfigFile)
	b.WriteString(config)
	b.WriteString("SPOV_CONFIG_EOF\n")
}

// writePlugin emits the spov-goal plugin for a job session. The source
// travels in the server binary rather than the image, so the plugin in a
// sandbox always matches the server that launched it.
//
// The heredoc is quoted, so nothing in the JavaScript is expanded by the
// shell.
//
// @decision(DL-039)
func (e Entrypoint) writePlugin(b *strings.Builder) error {
	if e.Job == nil {
		return nil
	}
	source, err := e.Job.Source()
	if err != nil {
		return err
	}
	if strings.Contains(source, pluginHeredocMarker) {
		return fmt.Errorf("opencode: plugin source contains the heredoc terminator %q", pluginHeredocMarker)
	}
	fmt.Fprintf(b, "mkdir -p \"%s\"\n", PluginDir)
	fmt.Fprintf(b, "cat > \"%s\" <<'%s'\n", PluginFile, pluginHeredocMarker)
	b.WriteString(source)
	if !strings.HasSuffix(source, "\n") {
		b.WriteString("\n")
	}
	fmt.Fprintf(b, "%s\n", pluginHeredocMarker)
	return nil
}

// pluginHeredocMarker terminates the plugin heredoc.
const pluginHeredocMarker = "SPOV_PLUGIN_EOF"

// writeLogins authenticates the forge CLIs. Tokens are referenced by
// environment variable, never interpolated.
//
// @decision(DL-032) @decision(DL-021)
func (e Entrypoint) writeLogins(b *strings.Builder) {
	var glab []glabHost
	for _, l := range e.Logins {
		if l.CLI == CLIGlab {
			glab = append(glab, glabHost{host: hostOf(l.URL), tokenExpr: `"$` + l.TokenEnv + `"`, subfolder: strings.Trim(l.BasePath, "/")})
		}
	}
	// glab gets one file for every GitLab host, written whole.
	//
	// @decision(DL-032)
	if len(glab) > 0 {
		writeGlabConfig(b, SandboxHome, glab)
	}
	for _, l := range e.Logins {
		switch l.CLI {
		case CLITea:
			// tea writes ~/.config/tea/config.yml; 0600 because it holds
			// the token.
			fmt.Fprintf(b, "tea login add --name %s --url %s --token \"$%s\" >/dev/null\n",
				shellQuote(l.Name), shellQuote(l.URL), l.TokenEnv)
			b.WriteString("chmod 600 /root/.config/tea/config.yml 2>/dev/null || true\n")
		case CLIGh:
			// @decision(DL-021)
			writeGhLogin(b, SandboxHome, shellQuote(hostOf(l.URL)), shellQuote(l.User), `"$`+l.TokenEnv+`"`)
		}
	}
}

// Command is the argv the sandbox is started with. It is `bash -c`, never
// `bash -lc`: a login shell sources /etc/profile and resets PATH.
//
// @decision(DL-036)
func (e Entrypoint) Command() ([]string, error) {
	script, err := e.Script()
	if err != nil {
		return nil, err
	}
	return []string{"bash", "-c", script}, nil
}

// WorkingDir is the directory the server starts in, which becomes the
// TUI's landing directory.
//
// @decision(DL-036) @decision(DL-037)
func (e Entrypoint) WorkingDir() string {
	if e.Landing == "" {
		return WorkspaceRoot
	}
	return path.Join(WorkspaceRoot, e.Landing)
}

// Validate checks the entrypoint before it is rendered.
func (e Entrypoint) Validate() error {
	if e.Port < 1 || e.Port > 65535 {
		return fmt.Errorf("opencode: port %d is out of range", e.Port)
	}
	dirs := make(map[string]bool, len(e.Clones))
	for i, c := range e.Clones {
		if c.URL == "" {
			return fmt.Errorf("opencode: clone %d has no URL", i)
		}
		if err := validCloneURL(c.URL); err != nil {
			return fmt.Errorf("opencode: clone %d: %w", i, err)
		}
		if err := validDir(c.Dir); err != nil {
			return fmt.Errorf("opencode: clone %d: %w", i, err)
		}
		if dirs[c.Dir] {
			return fmt.Errorf("opencode: two clones both target %q", c.Dir)
		}
		// @decision(DL-037)
		if slices.Contains(reservedDirs, c.Dir) {
			return fmt.Errorf("opencode: clone %d targets %q, which spov reserves", i, c.Dir)
		}
		dirs[c.Dir] = true
	}
	if e.Landing != "" {
		if err := validDir(e.Landing); err != nil {
			return fmt.Errorf("opencode: landing: %w", err)
		}
		// The landing directory only exists if something cloned it. With
		// no clones the entrypoint would die on `cd` under `set -e`, and
		// the sandbox would fail readiness with a bare exit code.
		if !dirs[e.Landing] {
			return fmt.Errorf("opencode: landing %q is not one of the cloned directories", e.Landing)
		}
	}
	return nil
}

// validCloneURL rejects URLs git would read as options rather than
// operands, and transports that let a URL name a command to run.
//
// @decision(DL-036)
func validCloneURL(u string) error {
	if strings.HasPrefix(u, "-") {
		return fmt.Errorf("clone URL %q starts with '-' and would be read as a git option", u)
	}
	// git's ext:: and file:: transports run a command or reach the local
	// filesystem; spov only ever clones over https or ssh.
	for _, prefix := range []string{"ext::", "file://", "file::"} {
		if strings.HasPrefix(strings.ToLower(u), prefix) {
			return fmt.Errorf("clone URL %q uses the %s transport, which is not allowed", u, strings.TrimSuffix(prefix, "::"))
		}
	}
	if strings.ContainsAny(u, "\n\r\x00") {
		return fmt.Errorf("clone URL contains a newline or NUL")
	}
	return nil
}

// validDir rejects directory names that would escape the workspace root.
func validDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("directory is empty")
	}
	if strings.HasPrefix(dir, "-") {
		return fmt.Errorf("directory %q starts with '-' and would be read as a git option", dir)
	}
	if path.IsAbs(dir) {
		return fmt.Errorf("directory %q must be relative to the workspace root", dir)
	}
	if dir != path.Clean(dir) {
		return fmt.Errorf("directory %q is not a clean path", dir)
	}
	for _, part := range strings.Split(dir, "/") {
		if part == ".." || part == "." {
			return fmt.Errorf("directory %q must not traverse outside the workspace root", dir)
		}
	}
	return nil
}

// shellQuote renders s as a single-quoted shell word. Clone URLs can carry
// a credential and repo names come from a remote API, so nothing is
// interpolated raw.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
