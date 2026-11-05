//go:build integration

package git

import (
	"context"
	"os/exec"
	"path/filepath"
)

// gitClone shallow-clones url into a subdirectory of dir.
func gitClone(ctx context.Context, url, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--", url, filepath.Join(dir, "repo"))
	// Never prompt: a hanging credential prompt would look like a hang.
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GCM_INTERACTIVE=never")
	return cmd.CombinedOutput()
}
