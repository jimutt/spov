package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
)

// The states a step of `spov init` can be in.
const (
	stepOK      = "ok"
	stepMissing = "missing"
	stepWaiting = "waiting"
)

// The steps' names, which --json carries and scripts may rely on.
const (
	stepModal  = "modal"
	stepGit    = "git"
	stepModel  = "model"
	stepClient = "client"
	stepImage  = "image"
)

// initStep is one thing a local setup needs. check reads and never
// changes anything; fix, which runs only after a question, is in
// initfix.go.
//
// @decision(DL-123)
type initStep struct {
	name, title string
	// required says the setup is not ready without it.
	required bool
	// needs names a step that must be ok before this one can be checked.
	needs string
	check func(ctx context.Context, r *initRun) stepResult
	fix   func(ctx context.Context, r *initRun) error
}

// stepResult is a step's state with a line of detail, and for a missing
// one what does not work without it and the one line that fixes it.
type stepResult struct {
	state  string
	detail string
	why    string
	fix    string
}

// StepView is one step in `spov init --json`.
//
// @decision(DL-123)
type StepView struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Required bool   `json:"required"`
	Detail   string `json:"detail,omitempty"`
	Fix      string `json:"fix,omitempty"`
}

// InitView is `spov init --json`: init's own format, not an API view.
//
// @decision(DL-123)
type InitView struct {
	Ready bool       `json:"ready"`
	Steps []StepView `json:"steps"`
}

// initRun is one run of the checklist.
type initRun struct {
	env *Env
	cfg config.Config
	// results holds each step's latest state, by name.
	results map[string]stepResult
	// changed says a fix wrote the platform or stored a key, which a
	// local server that is already running has not seen.
	changed bool
}

// initSteps is what "ready" means for a local setup, in the order each
// needs the one before.
//
// @decision(DL-123)
func initSteps() []initStep {
	return []initStep{
		{name: stepModal, title: "Modal", required: true, check: checkModal, fix: fixModal},
		{name: stepGit, title: "Git platform", required: true, check: checkGit, fix: fixGitHub},
		{name: stepModel, title: "Model", required: true, check: checkModel, fix: fixModel},
		{name: stepClient, title: "OpenCode client", check: checkClient},
		{name: stepImage, title: "Sandbox image", required: true, needs: stepModal, check: checkImage, fix: fixImage},
	}
}

// newInitCmd is `spov init`.
//
// @decision(DL-123) @decision(DL-125)
func newInitCmd(env *Env) *cobra.Command {
	var check, asJSON bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Check this machine is ready, and set up what is missing",
		Long: "Checks what a first `spov fly` needs, in order, and offers to set up each thing that is missing:\n\n" +
			"  1. Modal, where the sandboxes run\n" +
			"  2. a git platform: GitHub through the gh CLI's login\n" +
			"  3. a model: an API key, or an OpenCode Go or Zen sign-in\n" +
			"  4. the OpenCode client on this machine\n" +
			"  5. the sandbox image, built once\n\n" +
			"It asks before each change and is safe to run again: a step that is already done is left alone.\n" +
			"--check changes nothing and exits 1 when the setup is not ready, as a run without a terminal does.\n\n" +
			"Forgejo, Gitea, GitLab and a GitHub App are set up with `spov credentials add`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd.Context(), env, check || asJSON, asJSON)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only check: change nothing, and exit 1 when the setup is not ready")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the checks as JSON; implies --check")
	return cmd
}

func runInit(ctx context.Context, env *Env, checkOnly, asJSON bool) error {
	// As the local server reads it: a project file's opencode or modal
	// keys are not the server's.
	//
	// @decision(DL-123)
	env.ServerConfig, env.cachedConfig = true, nil
	res, err := env.config()
	if err != nil {
		return err
	}
	hosted, err := env.hostedServer()
	if err != nil {
		return err
	}
	if hosted {
		return initHosted(ctx, env, res.Config, asJSON)
	}
	if !checkOnly && !env.initInteractive() {
		_, _ = fmt.Fprintln(env.Err, "spov: no terminal to ask on, so this only checks (as --check does)")
		checkOnly = true
	}
	r := &initRun{env: env, cfg: res.Config, results: map[string]stepResult{}}
	steps := initSteps()
	if !asJSON {
		_, _ = fmt.Fprintln(env.Out, "Checking what a first `spov fly` needs on this machine.")
	}
	for i, step := range steps {
		// Ctrl-C ends init, at a question as anywhere else.
		if ctx.Err() != nil {
			return &ExitError{Code: 130}
		}
		res := r.run(ctx, step)
		if asJSON {
			continue
		}
		r.print(i+1, step, res)
		if res.state != stepMissing || checkOnly || step.fix == nil {
			continue
		}
		if res.why != "" {
			_, _ = fmt.Fprintln(env.Out, "     "+res.why)
		}
		err := step.fix(ctx, r)
		if ctx.Err() != nil {
			return &ExitError{Code: 130}
		}
		if err != nil {
			_, _ = fmt.Fprintf(env.Out, "     %v\n", err)
		}
		if again := r.run(ctx, step); again.state != res.state || again.detail != res.detail {
			r.print(i+1, step, again)
		}
	}
	r.settleLocalServer(ctx)
	view := r.view(steps)
	if asJSON {
		if err := writeJSON(env.Out, view); err != nil {
			return err
		}
	} else {
		r.conclude(ctx, steps, view.Ready)
	}
	if !view.Ready {
		return &ExitError{Code: 1}
	}
	return nil
}

// run checks one step, or leaves it waiting for the step it needs.
func (r *initRun) run(ctx context.Context, step initStep) stepResult {
	res := stepResult{state: stepWaiting, detail: "waits for " + titleOf(step.needs)}
	if step.needs == "" || r.results[step.needs].state == stepOK {
		res = step.check(ctx, r)
	}
	r.results[step.name] = res
	return res
}

func titleOf(name string) string {
	for _, s := range initSteps() {
		if s.name == name {
			return s.title
		}
	}
	return name
}

// print writes a step's line: a mark, and always the word beside it.
func (r *initRun) print(n int, step initStep, res stepResult) {
	mark := map[string]string{stepOK: "✓", stepMissing: "✗", stepWaiting: "–"}[res.state]
	word := res.state
	if res.state == stepMissing && !step.required {
		word = "not found"
	}
	_, _ = fmt.Fprintf(r.env.Out, "\n%d. %s  %s %s\n", n, step.title, mark, word)
	if res.detail != "" {
		_, _ = fmt.Fprintln(r.env.Out, "     "+res.detail)
	}
}

func (r *initRun) view(steps []initStep) InitView {
	v := InitView{Ready: true}
	for _, s := range steps {
		res := r.results[s.name]
		v.Steps = append(v.Steps, StepView{Name: s.name, State: res.state, Required: s.required, Detail: res.detail, Fix: res.fix})
		if s.required && res.state != stepOK {
			v.Ready = false
		}
	}
	return v
}

// conclude says what to run next, or what is still missing.
func (r *initRun) conclude(ctx context.Context, steps []initStep, ready bool) {
	out := r.env.Out
	if !ready {
		_, _ = fmt.Fprintln(out, "\nNot ready yet. Still missing:")
		for _, s := range steps {
			if res := r.results[s.name]; s.required && res.state != stepOK {
				line := "  - " + s.title
				if res.fix != "" {
					line += ": " + res.fix
				}
				_, _ = fmt.Fprintln(out, line)
			}
		}
		_, _ = fmt.Fprintln(out, "Run `spov init` again when you have them.")
		return
	}
	_, _ = fmt.Fprintln(out, "\nReady.")
	fly := "spov fly"
	// Without a local client the TUI cannot join the sandbox a launch
	// would pay for.
	if client := r.results[stepClient]; client.state != stepOK {
		fly = "spov fly --web"
		_, _ = fmt.Fprintln(out, "The OpenCode client is not installed, so use the web UI until it is:")
		if client.fix != "" {
			_, _ = fmt.Fprintln(out, "  "+client.fix)
		}
	}
	_, _ = fmt.Fprintf(out, "  cd your-repo && %s        # from a checkout: its branch, synced back when you quit\n", fly)
	_, _ = fmt.Fprintf(out, "  %s --repo owner/name      # from anywhere: the repository's default branch\n", fly)
}

// initInteractive reports whether init may ask: a terminal on both ends,
// or a test's own answers.
func (e *Env) initInteractive() bool {
	if e.Confirm != nil {
		return true
	}
	out, ok := e.Out.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(out.Fd())) && e.Stdin != nil && term.IsTerminal(int(e.Stdin.Fd()))
}

// checkModal finds Modal's credentials and uses them, so a tick means
// Modal accepted them.
//
// @decision(DL-123)
func checkModal(ctx context.Context, r *initRun) stepResult {
	env := r.env
	const why = "The sandboxes run on Modal, on your account, so nothing can launch without it."
	creds, found := credentials.ResolveModal(env.Environ.Getenv, env.Environ.UserHomeDir)
	fromEnv := env.Environ.Getenv("MODAL_TOKEN_ID") != "" || env.Environ.Getenv("MODAL_TOKEN_SECRET") != ""
	fix := "uvx modal setup"
	if fromEnv {
		fix = "correct or unset MODAL_TOKEN_ID and MODAL_TOKEN_SECRET"
	}
	if errors.Is(found, credentials.ErrNoModalCredentials) {
		return stepResult{state: stepMissing, detail: strings.TrimPrefix(found.Error(), "credentials: "), why: why, fix: fix}
	}
	// What spov found is only a hint: Modal's own client decides, and
	// it reads more than spov looks at.
	if _, err := env.provider(ctx); err != nil {
		detail := fmt.Sprintf("found credentials in %s, but could not use them with Modal: %v", creds.Source, err)
		if found != nil {
			detail = strings.TrimPrefix(found.Error(), "credentials: ")
		}
		return stepResult{state: stepMissing, why: why, fix: fix, detail: detail}
	}
	if found != nil {
		return stepResult{state: stepOK, detail: "signed in"}
	}
	return stepResult{state: stepOK, detail: "signed in, from " + creds.Source}
}

// checkGit is satisfied by any platform whose credential resolves now:
// a gh marker counts only while gh answers.
//
// @decision(DL-123)
func checkGit(ctx context.Context, r *initRun) stepResult {
	env := r.env
	missing := stepResult{state: stepMissing, fix: "spov init, or spov credentials add <platform>",
		why: "Without a git platform a session is seeded from your checkout and cannot push or open a pull request."}
	var problems []string
	for _, p := range r.cfg.Platforms {
		token, err := env.platformToken(ctx, p)
		if err != nil || token == "" {
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", p.Name, err))
			}
			continue
		}
		detail := fmt.Sprintf("%s (%s)", p.Name, p.Kind)
		if key, kerr := credentials.PlatformKey(p.Name); kerr == nil && env.Credentials != nil {
			if stored, _ := env.Credentials.Get(key); stored != "" {
				if host, ok := ghLogin(stored); ok {
					detail = fmt.Sprintf("%s, through gh's login on %s", p.Name, host)
				}
			}
		}
		return stepResult{state: stepOK, detail: detail}
	}
	switch {
	case len(problems) > 0:
		missing.detail = strings.Join(problems, "; ")
	case len(r.cfg.Platforms) == 0:
		missing.detail = "no platform is configured"
	default:
		missing.detail = "no configured platform has a credential"
	}
	return missing
}

// checkModel is satisfied by a provider key, exported or stored, or a
// usable OpenCode sign-in on this machine.
//
// @decision(DL-123)
func checkModel(_ context.Context, r *initRun) stepResult {
	env := r.env
	var exported, stored []string
	for _, name := range providerEnvVars {
		if env.Environ.Getenv(name) != "" {
			exported = append(exported, name)
		} else if v, err := env.storedProviderKey(name); err == nil && v != "" {
			stored = append(stored, name)
		}
	}
	var have []string
	if len(stored) > 0 {
		have = append(have, strings.Join(stored, ", ")+" stored")
	}
	if len(exported) > 0 {
		have = append(have, strings.Join(exported, ", ")+" exported in this shell (set it wherever you run spov)")
	}
	if server.LocalOpenCodeSignedIn(env.Credentials) {
		have = append(have, "OpenCode is signed in")
	}
	if len(have) == 0 {
		return stepResult{state: stepMissing, detail: "no provider key and no OpenCode sign-in",
			why: "The agent needs a model: an API key for a provider, or an OpenCode Go or Zen subscription.",
			fix: "spov credentials add ANTHROPIC_API_KEY, or spov login opencode"}
	}
	return stepResult{state: stepOK, detail: strings.Join(have, "; ")}
}

// checkClient looks for a local OpenCode client the pin accepts. It is
// not required: the web UI needs none.
//
// @decision(DL-123) @decision(DL-035)
func checkClient(ctx context.Context, r *initRun) stepResult {
	return clientResult(ctx, r.env, r.cfg.OpenCode.Version)
}

func clientResult(ctx context.Context, env *Env, want string) stepResult {
	found, err := env.locateBinary(ctx, want)
	if err != nil {
		first, _, _ := strings.Cut(err.Error(), "\n")
		return stepResult{state: stepMissing, detail: first,
			fix: fmt.Sprintf("curl -fsSL https://opencode.ai/v2/install | bash -s -- --version %s", want)}
	}
	return stepResult{state: stepOK, detail: fmt.Sprintf("opencode %s at %s", found.Version, found.Path)}
}

// checkImage asks Modal whether the image the local server will boot is
// published.
//
// @decision(DL-123) @decision(DL-016)
func checkImage(ctx context.Context, r *initRun) stepResult {
	name := imageName(r.cfg)
	missing := stepResult{state: stepMissing, fix: "spov serve image build",
		why: "Every sandbox boots from one image with OpenCode and the toolchain in it. It is built once per OpenCode version and takes a few minutes."}
	builder, err := r.imageBuilder(ctx)
	if err != nil {
		missing.detail = err.Error()
		return missing
	}
	ok, err := builder.HasImage(ctx, name)
	switch {
	case err != nil:
		missing.detail = fmt.Sprintf("could not ask Modal about %s: %v", name, err)
	case !ok && r.cfg.Sandbox.Image != "":
		missing.detail = fmt.Sprintf("sandbox.image names %s, which is not published", name)
		missing.fix = "publish that image, or remove sandbox.image"
	case !ok:
		missing.detail = name + " is not built yet"
	default:
		return stepResult{state: stepOK, detail: name + " is published"}
	}
	return missing
}

func (r *initRun) imageBuilder(ctx context.Context) (sandbox.ImageBuilder, error) {
	provider, err := r.env.provider(ctx)
	if err != nil {
		return nil, err
	}
	builder, ok := provider.(sandbox.ImageBuilder)
	if !ok {
		return nil, errors.New("this sandbox provider cannot build images")
	}
	return builder, nil
}

// initHosted is init with a hosted server configured: the server holds
// Modal, the image and the credentials, so only the sign-in and the
// local client are this machine's.
//
// @decision(DL-123)
func initHosted(ctx context.Context, env *Env, cfg config.Config, asJSON bool) error {
	url, _ := env.configuredServerURL()
	signIn := stepResult{state: stepMissing, detail: "not signed in to " + url, fix: "spov login"}
	c, ok := env.existingClient(ctx)
	if ok {
		me, err := c.Me(ctx)
		switch status := client.StatusOf(err); {
		case err == nil:
			signIn = stepResult{state: stepOK, detail: fmt.Sprintf("signed in to %s as %s", url, me.UserID)}
		case status != http.StatusUnauthorized && status != http.StatusForbidden:
			// Unreachable is not signed out.
			signIn = stepResult{state: stepMissing, detail: fmt.Sprintf("%s did not answer: %v", url, err),
				fix: "check server.url and that the server is up"}
		}
	}
	want := cfg.OpenCode.Version
	if ok {
		if pin, err := c.OpenCodePin(ctx); err == nil && pin.OpenCode != "" {
			want = pin.OpenCode
		}
	}
	client := clientResult(ctx, env, want)
	view := InitView{Ready: signIn.state == stepOK, Steps: []StepView{
		{Name: "server", State: signIn.state, Required: true, Detail: signIn.detail, Fix: signIn.fix},
		{Name: stepClient, State: client.state, Detail: client.detail, Fix: client.fix},
	}}
	if asJSON {
		if err := writeJSON(env.Out, view); err != nil {
			return err
		}
	} else {
		r := &initRun{env: env}
		_, _ = fmt.Fprintf(env.Out, "This CLI uses the spov server at %s, which holds Modal, the sandbox image and the credentials.\n", url)
		r.print(1, initStep{title: "Server", required: true}, signIn)
		r.print(2, initStep{title: "OpenCode client"}, client)
		if signIn.state == stepOK {
			_, _ = fmt.Fprintln(env.Out, "\nReady. Store your own tokens and keys with `spov credentials add`.")
		} else {
			_, _ = fmt.Fprintln(env.Out, "\nNot ready: "+signIn.fix+".")
		}
	}
	if !view.Ready {
		return &ExitError{Code: 1}
	}
	return nil
}

// choose asks which of several options and answers its index, or -1 for
// none.
//
// @decision(DL-123)
func (e *Env) choose(ctx context.Context, prompt string, options []string) (int, error) {
	if e.Choose != nil {
		return e.Choose(prompt, options)
	}
	return answerOrInterrupt(ctx, -1, func() (int, error) { return e.readChoice(prompt, options) })
}

func (e *Env) readChoice(prompt string, options []string) (int, error) {
	if e.Stdin == nil || !term.IsTerminal(int(e.Stdin.Fd())) {
		return -1, fmt.Errorf("no terminal to ask on")
	}
	return chooseFrom(e.Stdin, e.Err, prompt, options), nil
}

// chooseFrom lists the options and reads a number; anything else is no
// choice.
func chooseFrom(in io.Reader, out io.Writer, prompt string, options []string) int {
	_, _ = fmt.Fprintln(out, prompt)
	for i, o := range options {
		_, _ = fmt.Fprintf(out, "  %d) %s\n", i+1, o)
	}
	_, _ = fmt.Fprintf(out, "Choose 1-%d: ", len(options))
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(options) {
		return -1
	}
	return n - 1
}

// runInteractive runs another program with this terminal.
//
// @decision(DL-124)
func (e *Env) runInteractive(ctx context.Context, name string, args ...string) error {
	if e.RunInteractive != nil {
		return e.RunInteractive(ctx, name, args...)
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.Stdin, e.Out, e.Err
	return cmd.Run()
}

// answerOrInterrupt waits for an answer read from the terminal, or for
// the context to end: a terminal read is not interruptible, so Ctrl-C
// would otherwise do nothing until Enter. The abandoned read ends with
// the process.
//
// @decision(DL-123)
func answerOrInterrupt[T any](ctx context.Context, none T, read func() (T, error)) (T, error) {
	type answer struct {
		v   T
		err error
	}
	got := make(chan answer, 1)
	go func() {
		v, err := read()
		got <- answer{v, err}
	}()
	select {
	case a := <-got:
		return a.v, a.err
	case <-ctx.Done():
		return none, ctx.Err()
	}
}
