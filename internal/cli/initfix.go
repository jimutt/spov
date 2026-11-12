package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/prep"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/version"
)

// The fixes `spov init` offers. Each runs only for a missing step, only
// after a yes, and calls what the matching command calls.
//
// @decision(DL-124)

// say writes a line of a step's conversation.
func (r *initRun) say(format string, a ...any) {
	_, _ = fmt.Fprintf(r.env.Out, "     "+format+"\n", a...)
}

// ask puts a yes-or-no question. Ctrl-C answers no and ends init.
func (r *initRun) ask(ctx context.Context, question string) bool {
	ok, err := answerOrInterrupt(ctx, false, func() (bool, error) {
		return r.env.confirm("     " + question + " [y/N] ")
	})
	return err == nil && ok
}

// modalSetups are the ways to run Modal's own sign-in, by the program
// each needs on PATH.
var modalSetups = [][]string{
	{"modal", "setup"},
	{"uvx", "modal", "setup"},
	{"pipx", "run", "modal", "setup"},
}

// fixModal runs Modal's sign-in. spov never reads or asks for a Modal
// token itself.
//
// @decision(DL-124)
func fixModal(ctx context.Context, r *initRun) error {
	env := r.env
	id, secret := env.Environ.Getenv("MODAL_TOKEN_ID"), env.Environ.Getenv("MODAL_TOKEN_SECRET")
	if (id == "") != (secret == "") {
		r.say("Set both MODAL_TOKEN_ID and MODAL_TOKEN_SECRET, or unset the one that is set: the environment wins over a sign-in.")
		return nil
	}
	if id != "" {
		r.say("They come from MODAL_TOKEN_ID and MODAL_TOKEN_SECRET, which win over a sign-in. If Modal refused them, correct or unset them.")
		return nil
	}
	var setup []string
	for _, c := range modalSetups {
		if lookPath(env, c[0]) != "" {
			setup = c
			break
		}
	}
	if setup == nil {
		r.say("Sign in to Modal with its own tool, then run `spov init` again:")
		r.say("  uvx modal setup        # or: pipx run modal setup, or pip install modal && python3 -m modal setup")
		r.say("MODAL_TOKEN_ID and MODAL_TOKEN_SECRET in the environment work too.")
		return nil
	}
	cmd := strings.Join(setup, " ")
	r.say("`%s` signs you in through your browser and writes ~/.modal.toml.", cmd)
	r.say("Where no browser opens (WSL, ssh), open the address it prints by hand.")
	if !r.ask(ctx, "Run `"+cmd+"` now?") {
		return nil
	}
	if err := env.runInteractive(ctx, setup[0], setup[1:]...); err != nil {
		return fmt.Errorf("`%s` did not finish: %w", cmd, err)
	}
	// A provider that failed to build is tried again with the new
	// credentials.
	return nil
}

// fixGitHub sets up GitHub on github.com through gh's login: the
// platform in the user file, then DL-083's marker.
//
// @decision(DL-124) @decision(DL-083)
func fixGitHub(ctx context.Context, r *initRun) error {
	env := r.env
	const host = "github.com"
	if len(r.cfg.Platforms) > 0 {
		r.say("This sets up GitHub through the gh CLI. Other platforms are set up with `spov credentials add <platform>`.")
	}
	token, err := env.ghToken(ctx, host)
	switch {
	case errors.Is(err, errGhMissing):
		r.say("spov uses the GitHub CLI's login. Install gh from https://cli.github.com, run `gh auth login`, then `spov init` again.")
		r.say("A GitHub App works without gh: see docs/credentials.md.")
		return nil
	case errors.Is(err, errGhSignedOut):
		r.say("The GitHub CLI is installed but not signed in to %s.", host)
		if !r.ask(ctx, "Run `gh auth login` now?") {
			return nil
		}
		if err := env.runInteractive(ctx, "gh", "auth", "login", "--hostname", host, "--git-protocol", "https"); err != nil {
			return fmt.Errorf("`gh auth login` did not finish: %w", err)
		}
		if token, err = env.ghToken(ctx, host); err != nil {
			return err
		}
	case err != nil:
		return err
	}

	platform := config.Platform{Name: "github", Kind: config.KindGitHub}
	// Whose login it is, read from GitHub with the token, which is also
	// the check that the token works: nothing is stored without it.
	built, err := env.buildPlatform(ctx, platform, token)
	if err != nil {
		return fmt.Errorf("could not check gh's login with GitHub: %s", redactSecret(err.Error(), token))
	}
	info, err := git.Inspect(ctx, built)
	if err != nil || info.User == "" {
		reason := "GitHub did not say whose it is"
		if err != nil {
			reason = redactSecret(err.Error(), token)
		}
		return fmt.Errorf("could not check gh's login with GitHub (%s); nothing was changed", reason)
	}
	platform.Account = info.User
	who := "gh's login, as " + platform.Account
	// What one yes means depends on where the token will be held.
	//
	// @decision(DL-034)
	if env.proxyBinary() != nil {
		r.say("spov can use %s. The token stays outside the sandbox, is read from gh at each launch,", who)
		r.say("and is used only for a session's own repositories. (`gh auth switch` changes the account; a GitHub App narrows further.)")
		if !r.ask(ctx, "Use gh's login for GitHub?") {
			return nil
		}
	} else {
		r.say("spov can use %s, read from gh at each launch.", who)
		if !r.ask(ctx, "Use gh's login for GitHub?") {
			return nil
		}
		r.say("This build has no credential proxy, so the token would be put INSIDE every sandbox, where the agent can read it.")
		r.say("gh's token usually reaches every repository you can.")
		if !r.ask(ctx, "Put gh's token inside the sandboxes?") {
			return nil
		}
	}

	path, err := config.UserConfigPath(env.Environ)
	if err != nil {
		return fmt.Errorf("cannot find your config directory: %w", err)
	}
	// byHand prints what to add to a file init will not or cannot edit.
	byHand := func(lines []string) {
		for _, line := range lines {
			r.say("  %s", line)
		}
	}
	snippet := strings.Split(strings.TrimRight(config.PlatformSnippet(platform, "\n"), "\n"), "\n")
	out, err := config.AddPlatform(path, platform)
	if err != nil {
		r.say("%s could not be changed (%v). Add this to it by hand, then run `spov init` again:", path, err)
		byHand(snippet)
		return nil
	}
	name := platform.Name
	switch {
	case out.Existing != "":
		name = out.Existing
	case out.NotEditable && out.HasList:
		r.say("%s already has a platforms list, which is yours and is not rewritten. Add this entry to it, then run `spov init` again:", path)
		byHand(snippet[1:])
		return nil
	case out.NotEditable:
		r.say("%s is laid out in a way init does not append to. Add this to it by hand, then run `spov init` again:", path)
		byHand(snippet)
		return nil
	case out.Added:
		// The cached configuration predates the write. The file must
		// load again and hold the platform, or it is put back.
		env.cachedConfig = nil
		res, err := env.config()
		if err == nil && !hasPlatform(res.Config, name) {
			err = errors.New("the platform did not take effect")
		}
		if err != nil {
			env.cachedConfig = nil
			if rerr := out.Restore(); rerr != nil {
				return fmt.Errorf("%s no longer loads (%w) and could not be restored: %w", path, err, rerr)
			}
			r.say("%s did not load with the platform added (%v), so it was put back. Add this by hand:", path, err)
			byHand(snippet)
			return nil
		}
		r.cfg = res.Config
		r.say("Added the github platform to %s.", path)
	}
	// The token was just checked by reading its account, so the
	// listing-based check is skipped: a new account with no repository
	// yet is exactly who runs init.
	if err := addLocalPlatform(ctx, env, name, addOptions{FromGh: true, NoVerify: true}); err != nil {
		return err
	}
	r.changed = true
	return nil
}

// fixModel offers a pasted API key, the OpenCode sign-in, or neither.
//
// @decision(DL-124)
func fixModel(ctx context.Context, r *initRun) error {
	env := r.env
	options := []string{"Paste an API key for a model provider"}
	const signIn = "Sign in to OpenCode Go or Zen"
	modalReady := r.results[stepModal].state == stepOK
	if modalReady {
		options = append(options, signIn)
	} else {
		r.say("(Signing in to OpenCode Go or Zen is offered once Modal is connected: it goes through spov's local server.)")
	}
	options = append(options, "Skip for now")
	choice, err := env.choose(ctx, "     How should the agent reach a model?", options)
	if err != nil || choice < 0 || options[choice] == "Skip for now" {
		r.say("Skipped. The agent cannot reach a model until you add one.")
		return nil
	}
	if options[choice] == signIn {
		// The sign-in is read at each launch, so a running server needs
		// no restart for it.
		return runOpenCodeLogin(ctx, env, false)
	}
	// One row per key variable: OpenCode Go and Zen share one.
	var names, vars []string
	for _, p := range prep.Catalogue() {
		if i := indexOf(vars, p.KeyEnv); i >= 0 {
			names[i] += " / " + p.Name
			continue
		}
		names, vars = append(names, p.Name), append(vars, p.KeyEnv)
	}
	rows := make([]string, len(names))
	for i := range names {
		rows[i] = fmt.Sprintf("%s (%s)", names[i], vars[i])
	}
	which, err := env.choose(ctx, "     Which provider is the key for?", rows)
	if err != nil || which < 0 {
		r.say("Skipped. The agent cannot reach a model until you add one.")
		return nil
	}
	if env.Credentials != nil {
		r.say("The key is kept on this machine (%s) and is not checked.", env.Credentials.Name())
	}
	if err := addLocalProvider(env, vars[which], addOptions{}); err != nil {
		return err
	}
	r.changed = true
	return nil
}

func hasPlatform(cfg config.Config, name string) bool {
	for _, p := range cfg.Platforms {
		if p.Name == name {
			return true
		}
	}
	return false
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// fixImage builds the base image, as `spov serve image build` does.
//
// @decision(DL-124)
func fixImage(ctx context.Context, r *initRun) error {
	if r.cfg.Sandbox.Image != "" {
		return nil
	}
	builder, err := r.imageBuilder(ctx)
	if err != nil {
		return err
	}
	if !r.ask(ctx, "Build it now? It takes a few minutes.") {
		return nil
	}
	spec := sandbox.ImageSpec{OpenCodeVersion: r.cfg.OpenCode.Version}
	if err := spec.Validate(); err != nil {
		return err
	}
	if err := publishImage(ctx, r.env, builder, spec, nil, false); err != nil {
		return fmt.Errorf("the build did not finish (run `spov serve image build` to try again): %w", err)
	}
	return nil
}

// settleLocalServer deals with a local server that started before a fix
// wrote the platform or a key: it reads both once, at its start. An
// idle one is stopped, so the next command starts one that sees the
// change; one with a live session is left alone.
//
// @decision(DL-124)
func (r *initRun) settleLocalServer(ctx context.Context) {
	if !r.changed {
		return
	}
	env := r.env
	path, err := env.stateFilePath()
	if err != nil {
		return
	}
	st, err := readState(path)
	if err != nil || st == nil {
		return
	}
	c, ok := env.liveLocalReadOnly(ctx, st)
	if !ok {
		return
	}
	page, err := c.ListSessions(ctx, client.Page{States: liveStates(), Limit: 1})
	if err != nil || len(page.Sessions) > 0 {
		_, _ = fmt.Fprintln(env.Out, "\nA spov server is already running on this machine and was left alone. It reads its configuration"+
			" when it starts, so what you just set up applies once it has exited, which it does a minute after its last session ends.")
		return
	}
	env.stopProcess(st.PID)
	_, _ = fmt.Fprintln(env.Out, "\nStopped the idle spov server on this machine, so the next command starts one that sees what you just set up.")
}

// liveLocalReadOnly is the local server the state file names, if it is
// this build's and answers. It stops nothing: a server of another build
// is replaced by the next command anyway.
func (e *Env) liveLocalReadOnly(ctx context.Context, st *localState) (*client.Client, bool) {
	if st.PID <= 0 || st.Token == "" || !loopbackURL(st.URL) || !e.processAlive(st.PID) || st.Version != version.Get().String() {
		return nil, false
	}
	c := client.New(st.URL, st.Token)
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if _, err := c.Me(probeCtx); err != nil {
		return nil, false
	}
	return c, true
}
