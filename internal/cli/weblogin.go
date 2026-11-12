package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres"
)

// The browser login's configuration. All of it is environment-only, as
// hosted mode requires (DL-010).
//
// @decision(DL-007)
const (
	GitHubClientIDEnvVar     = "SPOV_GITHUB_OAUTH_CLIENT_ID"
	GitHubClientSecretEnvVar = "SPOV_GITHUB_OAUTH_CLIENT_SECRET"
	AllowlistFileEnvVar      = "SPOV_ALLOWLIST_FILE"
)

// DefaultAllowlistFile is where the allowlist is read from when
// SPOV_ALLOWLIST_FILE is unset. It is a path a container mounts a file
// or a ConfigMap at.
//
// @decision(DL-007)
const DefaultAllowlistFile = "/etc/spov/allowlist"

// Web session lifetimes. A login lasts a week, and goes sooner if it is
// unused for a day.
//
// @decision(DL-007)
const (
	WebSessionLifetime = 7 * 24 * time.Hour
	WebSessionIdle     = 24 * time.Hour
	// WebSessionCookie is the cookie carrying the opaque scs token on
	// plain http; a Secure cookie is named WebSessionCookieSecure.
	WebSessionCookie = "spov_session"
	// WebSessionCookieSecure is the name when the cookie is Secure. The
	// __Host- prefix makes a browser refuse one set by a sibling
	// subdomain, or with a Domain or another Path.
	//
	// @decision(DL-135)
	WebSessionCookieSecure = "__Host-spov_session"
	// WebSessionTable is where scs keeps its rows. Not the library's
	// default "sessions": that name is spov's own sessions table.
	WebSessionTable = "web_sessions"
)

// newLogin builds the browser login flow, or nil when this deployment
// has none. Login needs Postgres (the session store and the user rows)
// and a registered OAuth app; a server without them serves the API on
// its bearer token alone, which is what a single-operator deployment
// runs until it has users.
//
// @decision(DL-007)
func (e *Env) newLogin(ctx context.Context, st store.Store, publicURL string, warn func(string)) (*server.Login, func(), error) {
	clientID := e.Environ.Getenv(GitHubClientIDEnvVar)
	secret := e.Environ.Getenv(GitHubClientSecretEnvVar)
	if clientID == "" && secret == "" {
		return nil, func() {}, nil
	}
	if clientID == "" || secret == "" {
		return nil, nil, fmt.Errorf("%s and %s must be set together", GitHubClientIDEnvVar, GitHubClientSecretEnvVar)
	}
	// The callback is registered with GitHub against the public origin,
	// so a deployment behind a proxy cannot infer it from the listener.
	if publicURL == "" {
		return nil, nil, fmt.Errorf("%s is required for login: it is the origin GitHub redirects back to", PublicURLEnvVar)
	}
	pg, ok := st.(*postgres.Store)
	if !ok {
		return nil, nil, fmt.Errorf("login needs a database: set %s", DatabaseURLEnvVar)
	}
	redirect, err := url.JoinPath(strings.TrimSuffix(publicURL, "/"), server.CallbackPath)
	if err != nil {
		return nil, nil, fmt.Errorf("building the OAuth callback URL: %w", err)
	}

	// The file is the first-boot seed, not live configuration: it is
	// imported when the org has no administrator and ignored after
	// that. What enforcement reads is the table.
	//
	// @decision(DL-007)
	if _, err := auth.SeedAllowlist(ctx, st, server.DefaultOrgID, e.allowlistPath(), e.accountLookup(), warn); err != nil {
		return nil, nil, err
	}
	list, err := auth.NewAllowlist(ctx,
		auth.StoreAllowlist(st.Allowlist(), server.DefaultOrgID), auth.AllowlistRefresh, warn)
	if err != nil {
		return nil, nil, err
	}

	sessions := newSessionManager(publicURL, warn)
	pgstore := pgxstore.NewWithConfig(pg.Pool(), pgxstore.Config{
		TableName:       WebSessionTable,
		CleanUpInterval: 5 * time.Minute,
	})
	sessions.Store = pgstore

	login := &server.Login{
		OAuth:     auth.NewGitHubOAuth(clientID, secret, redirect),
		Sessions:  sessions,
		Users:     st.Users(),
		Allowlist: list,
		// @decision(DL-128)
		Entries: st.Allowlist(),
		OrgID:   server.DefaultOrgID,
		Now:     e.now,
		Warn:    warn,
	}
	return login, pgstore.StopCleanup, nil
}

// accountLookup resolves an allowlist entry's GitHub login: the test's
// fake, else GitHub's API, the same one the login reads.
//
// @decision(DL-128)
func (e *Env) accountLookup() auth.AccountLookup {
	if e.GitHubAccounts != nil {
		return e.GitHubAccounts
	}
	return auth.GitHubAccounts{}.Lookup
}

// newSessionManager applies the session and cookie policy. It is
// separate from newLogin so the policy can be checked without a
// database: the cookie is the credential, and its attributes are what
// stop a page on another site from using it.
//
// @decision(DL-007)
func newSessionManager(publicURL string, warn func(string)) *scs.SessionManager {
	sessions := scs.New()
	sessions.Lifetime = WebSessionLifetime
	sessions.IdleTimeout = WebSessionIdle
	// The store keeps a hash of the token, never the cookie's value.
	//
	// @decision(DL-127)
	sessions.HashTokenInStore = true
	sessions.Cookie.Name = WebSessionCookie
	sessions.Cookie.HttpOnly = true
	sessions.Cookie.SameSite = http.SameSiteLaxMode
	sessions.Cookie.Path = "/"
	// Secure follows the public origin: a cookie marked Secure is never
	// sent over http, so a deployment on plain http for a local trial
	// would silently never log anyone in.
	//
	// @decision(DL-007)
	sessions.Cookie.Secure = strings.HasPrefix(publicURL, "https://")
	if sessions.Cookie.Secure {
		sessions.Cookie.Name = WebSessionCookieSecure
	}
	if !sessions.Cookie.Secure && warn != nil {
		warn("login: " + PublicURLEnvVar + " is not https, so the session cookie is not marked Secure")
	}
	return sessions
}

// allowlistPath is the file a new deployment is seeded from. The allowlist
// itself is read from the store.
//
// @decision(DL-007)
func (e *Env) allowlistPath() string {
	if path := e.Environ.Getenv(AllowlistFileEnvVar); path != "" {
		return path
	}
	return DefaultAllowlistFile
}

// watchAllowlist answers SIGHUP. There is nothing to reload, but the
// trap keeps the signal from killing the process. It
// returns when ctx ends.
//
// @decision(DL-007)
func watchAllowlist(ctx context.Context, login *server.Login, warn func(string)) {
	hup, stop := trapHUP()
	defer stop()
	watchAllowlistOn(ctx, hup, login, warn)
}

// trapHUP starts delivering SIGHUP to a channel. It is separate from the
// loop so a caller can install the trap before anything can signal:
// until Notify runs, SIGHUP's default action terminates the process.
//
// @decision(DL-007)
func trapHUP() (<-chan os.Signal, func()) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	return hup, func() { signal.Stop(hup) }
}

// watchAllowlistOn is watchAllowlist over an already-installed trap.
//
// @decision(DL-007)
func watchAllowlistOn(ctx context.Context, hup <-chan os.Signal, login *server.Login, warn func(string)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
		}
		if login == nil {
			warn("allowlist: nothing to reload; this server has no login configured")
			continue
		}
		// Refreshing here would make SIGHUP look like it still means
		// something. It does not: a signal reaches one replica, which
		// is the gap DL-007 closed.
		//
		// @decision(DL-007)
		warn("allowlist: nothing to reload; the allowlist lives in the database" +
			" and every replica refreshes it every " + auth.AllowlistRefresh.String() +
			". Manage it with the /api/v1/allowlist endpoints.")
	}
}
