package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/server"
)

// credentialVerifier checks a credential against its platform before it
// is stored, so a typo is refused where the user can still fix it
// rather than failing a launch an hour later.
//
// @decision(DL-008)
type credentialVerifier struct {
	env *Env
	cfg config.Config
}

// Verify contacts the platform and reports what it learned about the
// credential. The credential never appears in the error it returns.
//
// @decision(DL-008) @decision(DL-041)
func (v *credentialVerifier) Verify(ctx context.Context, platform, secret string) (server.Verified, error) {
	if v.env == nil {
		// Nothing to build a platform client with, so the credential is
		// stored on the user's word rather than refused.
		return server.Verified{}, server.ErrVerifyUnsupported
	}
	if platform == server.PlatformGitHub {
		as, err := v.verifyGitHub(ctx, secret)
		return server.Verified{As: as}, err
	}
	if kind, host, ok := server.PlatformHost(platform); ok {
		return v.verifyOnHost(ctx, kind, host, secret)
	}
	// A model provider key has no cheap identity endpoint to call, and
	// spending a model request to check one is not worth it.
	//
	// @decision(DL-008)
	return server.Verified{}, server.ErrVerifyUnsupported
}

// verifyGitHub accepts a personal access token only: the App is the
// deployment's credential, not a user's, and its private key must never
// be pasted into a hosted server.
//
// @decision(DL-008) @decision(DL-041)
func (v *credentialVerifier) verifyGitHub(ctx context.Context, secret string) (string, error) {
	if strings.HasPrefix(strings.TrimSpace(secret), "{") {
		return "", errors.New("this looks like a GitHub App credential;" +
			" a user credential is a fine-grained personal access token")
	}
	if !git.LooksLikeGitHubPAT(secret) {
		return "", errors.New("this does not look like a GitHub token" +
			" (a fine-grained one starts with github_pat_)")
	}
	// Built through the configured GitHub platform when there is one, so
	// an enterprise host is verified against its own API rather than
	// github.com, and buildPlatform's injection point is honoured.
	entry := config.Platform{Name: "github", Kind: config.KindGitHub}
	if p, ok := githubPlatformIn(v.cfg); ok {
		entry = p
	}
	platform, err := v.env.buildPlatform(ctx, entry, secret)
	if err != nil {
		return "", err
	}
	// The token must reach a repository, or it is useless for a session
	// even though it authenticates.
	if _, err := platform.ListRepos(ctx); err != nil {
		if errors.Is(err, git.ErrNoRepos) {
			return "", errors.New("the token reaches no repositories;" +
				" grant it access to the ones you want to work on")
		}
		return "", err
	}
	// The account the token proved it owns, when the platform can say.
	if pat, ok := platform.(*git.GitHubPAT); ok {
		if login, err := pat.Login(ctx); err == nil {
			return login, nil
		}
	}
	return "", nil
}

// verifyOnHost checks a Forgejo, Gitea or GitLab token against the
// platform configured on its host: what the forge says about the token
// first, then that it reaches a repository.
//
// @decision(DL-008) @decision(DL-041)
func (v *credentialVerifier) verifyOnHost(ctx context.Context, kind, host, secret string) (server.Verified, error) {
	p, ok := platformOn(v.cfg, kind, host)
	if !ok {
		// Naming the hosts that would work turns "no platform on
		// localhost" into an answer: the configured one is very often
		// the same host with a port.
		//
		// @decision(DL-008)
		if hosts := hostsOf(v.cfg, kind); len(hosts) > 0 {
			return server.Verified{}, fmt.Errorf("this server has no %s platform on %s; it has %s",
				kind, host, strings.Join(hosts, ", "))
		}
		return server.Verified{}, fmt.Errorf("this server has no %s platform on %s", kind, host)
	}
	platform, err := v.env.buildPlatform(ctx, p, secret)
	if err != nil {
		return server.Verified{}, err
	}
	info, err := git.Inspect(ctx, platform)
	if err != nil && !errors.Is(err, git.ErrUnsupported) {
		return server.Verified{}, err
	}
	if _, err := platform.ListRepos(ctx); err != nil {
		if errors.Is(err, git.ErrNoRepos) {
			return server.Verified{}, errors.New("the token reaches no repositories;" +
				" scope it to the ones you want to work on")
		}
		return server.Verified{}, err
	}
	if info.User == "" {
		info.User = p.Account
	}
	return server.Verified{As: info.User, Scopes: info.Scopes, ExpiresAt: info.ExpiresAt}, nil
}

// githubPlatformIn is the configured GitHub platform, if the deployment
// has one. Its base URL is what an enterprise host is verified against.
//
// @decision(DL-008)
func githubPlatformIn(cfg config.Config) (config.Platform, bool) {
	for _, p := range cfg.Platforms {
		if p.Kind == config.KindGitHub {
			return p, true
		}
	}
	return config.Platform{}, false
}

// hostsOf is every host this server has a platform of a kind on, as a
// `<kind>:<host>` credential would need them spelled.
//
// @decision(DL-008) @decision(DL-041)
func hostsOf(cfg config.Config, kind string) []string {
	var out []string
	for _, p := range cfg.Platforms {
		if string(p.Kind) != kind {
			continue
		}
		if h, err := platformHost(p); err == nil {
			out = append(out, h)
		}
	}
	return out
}

// platformOn finds the configured platform of a kind on a host.
//
// @decision(DL-008) @decision(DL-041)
func platformOn(cfg config.Config, kind, host string) (config.Platform, bool) {
	for _, p := range cfg.Platforms {
		if string(p.Kind) != kind {
			continue
		}
		if h, err := platformHost(p); err == nil && strings.EqualFold(h, host) {
			return p, true
		}
	}
	return config.Platform{}, false
}
