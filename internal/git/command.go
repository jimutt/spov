package git

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// nullHooksPath is the hooks directory a spov git command is given:
// the null device, which holds no hook. git accepts "/dev/null" for it
// on Linux, macOS, WSL (including a checkout under /mnt/c) and, through
// its own path translation, Git for Windows.
const nullHooksPath = "/dev/null"

// guardArgs are the options placed before the subcommand of every git
// command spov runs in a checkout: no pager, no hooks, and no
// filesystem monitor (core.fsmonitor can name a program git runs on
// each status or index refresh).
func guardArgs(keepHooksPath bool) []string {
	args := []string{"--no-pager", "-c", "core.fsmonitor=false"}
	if !keepHooksPath {
		args = append(args, "-c", "core.hooksPath="+nullHooksPath)
	}
	return args
}

// Command builds the git command spov runs in a user's checkout, with
// the checkout's hooks and filesystem monitor off and no pager. Every
// git process spov starts goes through it (`TestEveryGitCommandIsBuiltByCommand`).
// Aliases cannot redirect it: every subcommand spov uses is a built-in,
// and git does not let an alias shadow one.
//
// env, when non-nil, replaces the environment, as exec.Cmd.Env does.
//
// @decision(DL-136)
func Command(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
	return command(ctx, dir, env, false, args...)
}

// commandReadingHooksPath is Command for the one question that needs
// the checkout's real hooks directory, `rev-parse --git-path hooks`,
// which the null hooks path would otherwise answer.
func commandReadingHooksPath(ctx context.Context, dir string, args ...string) *exec.Cmd {
	return command(ctx, dir, nil, true, args...)
}

func command(ctx context.Context, dir string, env []string, keepHooksPath bool, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append(guardArgs(keepHooksPath), args...)...)
	cmd.Dir = dir
	if env == nil {
		env = os.Environ()
	}
	// The pager is never wanted: output is captured, never shown. An
	// external diff program is not spov's to run either.
	env = slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		return strings.HasPrefix(kv, "GIT_EXTERNAL_DIFF=") || strings.HasPrefix(kv, "GIT_PAGER=")
	})
	cmd.Env = append(env, "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0")
	return cmd
}
