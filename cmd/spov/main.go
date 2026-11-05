// Command spov launches OpenCode sessions in on-demand Modal sandboxes.
//
// @decision(DL-001)
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/jimutt/spov/internal/cli"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run wires the command tree to the real process and returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if err := execute(args, stdout, stderr); err != nil {
		_, _ = fmt.Fprintln(stderr, "spov: "+err.Error())
		return 1
	}
	return 0
}

// interactiveSelector returns the repository picker only when there is a
// terminal to drive it. The picker reads /dev/tty rather than stdin, so
// stdin being a character device is not enough: a run under `< /dev/null`
// would otherwise fail with an opaque TTY error instead of spov's own
// message.
//
// @decision(DL-023)
func interactiveSelector() func([]cli.RepoChoice) ([]cli.RepoChoice, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil
	}
	_ = tty.Close()
	return cli.SelectReposInteractively
}

func execute(args []string, stdout, stderr io.Writer) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}

	// A sandbox bills for its whole lifetime, so an interrupt must unwind
	// the launch rather than kill the process: cancelling the context
	// lets `fly` terminate whatever it already created. A second
	// interrupt restores the default behaviour, so the user is never
	// stuck waiting on a cleanup that is itself hanging.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	env := cli.Env{
		Out:     stdout,
		Err:     stderr,
		Environ: config.OSEnv{},
		Workdir: wd,
		Stdin:   os.Stdin,
		// Only offer the picker on a real terminal: piped or scripted
		// runs must fail with a clear message rather than hang on a
		// prompt nobody can answer.
		SelectRepos: interactiveSelector(),
	}
	// The store is created but not probed here: FallbackStore only talks
	// to the keyring on first use, so commands needing no secrets pay
	// nothing.
	if userPath, err := config.UserConfigPath(config.OSEnv{}); err == nil {
		env.Credentials = credentials.Default(credentials.ConfigDir(userPath), stderr)
	}

	root := cli.NewRoot(env)
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}
