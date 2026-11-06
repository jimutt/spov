package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/config"
)

// ghLoginPrefix starts the value a GitHub platform stores in this
// machine's credential store when its credential is gh's own login: the
// marker and gh's host, never a token.
//
// @decision(DL-083)
const ghLoginPrefix = "gh:"

// ghTimeout bounds `gh auth token`, which a launch waits on.
const ghTimeout = 5 * time.Second

// ghHost is the host gh knows a GitHub platform by.
func ghHost(p config.Platform) string {
	if p.BaseURL == "" {
		return "github.com"
	}
	if u, err := url.Parse(p.BaseURL); err == nil && u.Host != "" {
		return strings.ToLower(u.Hostname())
	}
	return "github.com"
}

// ghLogin reports whether a stored value is gh's login, and its host.
func ghLogin(stored string) (string, bool) {
	host, ok := strings.CutPrefix(stored, ghLoginPrefix)
	return host, ok && host != ""
}

// ghToken reads gh's token for host, now: a logout or re-login in gh
// takes effect at the next read. A gh that is missing, logged out or
// slow is an error, never a fallback to another credential.
//
// @decision(DL-083)
func (e *Env) ghToken(ctx context.Context, host string) (string, error) {
	if e.GhToken != nil {
		return e.GhToken(ctx, host)
	}
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", host).Output()
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return "", errors.New("this platform uses gh's login, and gh is not installed")
	case ctx.Err() != nil:
		return "", fmt.Errorf("gh auth token did not answer within %s", ghTimeout)
	case err != nil:
		return "", fmt.Errorf("gh has no login for %s; run `gh auth login --hostname %s`", host, host)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("gh has no login for %s; run `gh auth login --hostname %s`", host, host)
	}
	return token, nil
}
