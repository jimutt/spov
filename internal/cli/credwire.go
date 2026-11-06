package cli

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/console"
	"github.com/jimutt/spov/internal/routedcred"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/version"
)

// wireCredentials installs the two-tier credential resolution on deps.
//
// Local mode gets no per-user resolution: there is one user, whose
// credentials are this machine's store, which `spov credentials add`
// writes directly.
//
// @decision(DL-008)
func (e *Env) wireCredentials(deps *server.Deps, cfg config.Config, localMode bool) error {
	sealer, err := e.sealer()
	if err != nil {
		return err
	}
	deps.Credentials = &server.CredentialStore{
		Store:  deps.Store.Credentials(),
		Sealer: sealer,
		Now:    e.now,
	}
	deps.Verifier = &credentialVerifier{env: e, cfg: cfg}
	// @decision(DL-073)
	dir, dirErr := e.stateDir()
	switch {
	case localMode && e.Credentials != nil && dirErr == nil:
		deps.RoutedCredentials = server.LocalRoutedCredentials{Local: &routedcred.Local{
			Creds: e.Credentials, Path: filepath.Join(dir, routedcred.IndexName)}}
	case sealer != nil:
		deps.RoutedCredentials = &server.SealedRoutedCredentials{Store: deps.Store.Credentials(), Sealer: sealer, Now: e.now}
	}
	// The OpenCode sign-in lives where the server keeps credentials: the
	// machine's own store locally, the sealed store when hosted.
	//
	// @decision(DL-015)
	deps.Console = &console.Client{UserAgent: "spov/" + version.Get().Version}
	switch {
	case localMode && e.Credentials != nil:
		deps.ConsoleGrants = &server.LocalConsoleStore{Creds: e.Credentials}
	case !localMode && sealer != nil:
		deps.ConsoleGrants = server.SealedConsoleStore{Store: deps.Store, Sealer: sealer, Now: e.now}
	}
	if localMode || sealer == nil {
		// Without a key there is nothing to read back, so every user
		// keeps the deployment's credentials and the API refuses to
		// store any.
		return nil
	}
	// Warm the config cache before any request copies the Env: the copy
	// carries the cached pointer, so a cold cache would have each
	// request goroutine racing to fill its own copy's slot.
	//
	// @decision(DL-008)
	if _, err := e.config(); err != nil {
		return err
	}
	// @decision(DL-042)
	deps.OrgApps = &server.OrgApps{Store: deps.Store, Sealer: sealer, Now: e.now}
	// @decision(DL-074)
	deps.OrgRoutedCredentials = &server.SealedOrgRoutedCredentials{Store: deps.Store, Sealer: sealer}
	hosted := &hostedCredentials{
		env:     e,
		cfg:     cfg,
		store:   deps.Credentials,
		warn:    deps.Warn,
		orgKeys: deps.ProviderKeys,
		apps:    deps.OrgApps,
	}
	deps.UserForge = hosted.forgeFor
	// @decision(DL-042)
	deps.UserRepos = hosted.reposByTier
	deps.UserProviderKeys = hosted.providerKeysFor
	// @decision(DL-042)
	deps.OrgGitHub = orgGitHub{h: hosted}
	return nil
}

// sealer builds the credential sealer from the environment. A server
// with no key configured is not an error: it simply stores no user
// credentials, which is every local run.
//
// @decision(DL-008)
func (e *Env) sealer() (*auth.Sealer, error) {
	current := e.Environ.Getenv(CredentialKeyEnvVar)
	old := e.Environ.Getenv(CredentialKeyOldEnvVar)
	if current == "" {
		if old != "" {
			return nil, fmt.Errorf("%s is set without %s;"+
				" the retiring key can only be used alongside the current one",
				CredentialKeyOldEnvVar, CredentialKeyEnvVar)
		}
		return nil, nil
	}
	// Each key's version comes from the key itself, so rows sealed
	// under the retiring key stay readable and the order the two are
	// configured in cannot brick them.
	//
	// @decision(DL-008)
	sealer, err := auth.NewSealerFor(current, old)
	if err != nil {
		if errors.Is(err, auth.ErrNoKey) {
			return nil, nil
		}
		return nil, err
	}
	return sealer, nil
}
