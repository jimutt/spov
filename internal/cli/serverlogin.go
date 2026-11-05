package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/oauth2"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
	"github.com/jimutt/spov/internal/version"
)

type serverLoginOptions struct {
	Name      string
	NoBrowser bool
}

// runServerLogin drives the device flow.
//
// @decision(DL-006)
func runServerLogin(ctx context.Context, env *Env, opts serverLoginOptions) error {
	serverURL, err := env.hostedServerURL()
	if err != nil {
		return err
	}
	if env.Credentials == nil {
		return fmt.Errorf("no credential store configured, so there is nowhere to keep the token")
	}
	key, err := credentials.ServerKey(serverURL)
	if err != nil {
		return err
	}

	cfg := deviceConfig(serverURL)
	name := opts.Name
	if name == "" {
		name = defaultTokenName()
	}
	auth, err := cfg.DeviceAuth(ctx, oauth2.SetAuthURLParam("name", name))
	if err != nil {
		return fmt.Errorf("starting the login: %w", redactURL(err))
	}

	// The code is what a human retypes, so it is the loudest thing on
	// screen; the URL goes to stderr with it, since a piped run still
	// needs both.
	_, _ = fmt.Fprintf(env.Err, "\nTo sign in, visit:\n\n    %s\n\nand enter the code:\n\n    %s\n\n",
		auth.VerificationURI, auth.UserCode)
	// The complete URI carries the code, so the human need not retype
	// it. Opening a browser is best-effort: spov's audience is headless
	// first, and the code is already on screen.
	if !opts.NoBrowser {
		openBrowser(auth.VerificationURIComplete, env)
	}
	_, _ = fmt.Fprintln(env.Err, "Waiting for approval...")

	token, err := cfg.DeviceAccessToken(ctx, auth)
	if err != nil {
		return deviceError(err)
	}
	if err := env.Credentials.Set(key, token.AccessToken); err != nil {
		return fmt.Errorf("storing the token: %w", err)
	}

	// Confirm against the server rather than announcing success on the
	// strength of a token nobody has used yet.
	c := client.New(serverURL, token.AccessToken)
	c.UserAgent = "spov/" + version.Get().Version
	me, err := c.Me(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(env.Out, "Signed in to %s. (Could not read your identity back: %v)\n", serverURL, err)
		return nil
	}
	who := me.Email
	if who == "" {
		who = me.UserID
	}
	_, _ = fmt.Fprintf(env.Out, "Signed in to %s as %s.\n", serverURL, who)
	return nil
}

// deviceConfig is the oauth2 client for spov's own device flow.
// AuthStyle is explicit; left unset, x/oauth2 probes and retries on any
// error, which replaces the server's error with the retry's and hides
// access_denied until the code expires.
//
// @decision(DL-006)
func deviceConfig(serverURL string) *oauth2.Config {
	base := strings.TrimSuffix(serverURL, "/")
	return &oauth2.Config{
		ClientID: server.DeviceClientID,
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: base + server.DeviceCodePath,
			TokenURL:      base + "/device/token",
			AuthStyle:     oauth2.AuthStyleInParams,
		},
	}
}

// defaultTokenName is what the approval page shows, so a person can tell
// which terminal is asking.
//
// @decision(DL-006)
func defaultTokenName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown host"
	}
	return host + " · spov " + version.Get().Version
}

// deviceError turns the flow's failures into something actionable. The
// RFC's codes are machine-readable and mean nothing to a person.
//
// @decision(DL-006)
func deviceError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		switch re.ErrorCode {
		case "access_denied":
			return fmt.Errorf("the sign-in was denied")
		case "expired_token":
			return fmt.Errorf("the code expired before it was approved; run the command again")
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("gave up waiting for approval")
	}
	return fmt.Errorf("the login did not complete: %w", redactURL(err))
}

// redactURL keeps a device code out of an error. x/oauth2 renders the
// request it made, and for the token endpoint that body holds the
// device_code.
//
// @decision(DL-006)
func redactURL(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if i := strings.Index(msg, "device_code="); i >= 0 {
		end := strings.IndexAny(msg[i:], "& \"")
		if end < 0 {
			msg = msg[:i] + "device_code=REDACTED"
		} else {
			msg = msg[:i] + "device_code=REDACTED" + msg[i+end:]
		}
		return errors.New(msg)
	}
	return err
}

// runServerLogout revokes server-side, then deletes locally. A failed
// revoke still deletes: leaving the token on disk after the user asked
// to be signed out is the worse failure, and it expires on its own.
//
// @decision(DL-006)
func runServerLogout(ctx context.Context, env *Env) error {
	serverURL, err := env.hostedServerURL()
	if err != nil {
		return err
	}
	if env.Credentials == nil {
		return fmt.Errorf("no credential store configured")
	}
	key, err := credentials.ServerKey(serverURL)
	if err != nil {
		return err
	}
	token, err := env.Credentials.Get(key)
	if errors.Is(err, credentials.ErrNotFound) || token == "" {
		_, _ = fmt.Fprintf(env.Out, "Not signed in to %s.\n", serverURL)
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the stored token: %w", err)
	}

	c := client.New(serverURL, token)
	c.UserAgent = "spov/" + version.Get().Version
	revokeErr := c.RevokeCurrentToken(ctx)

	if err := env.Credentials.Delete(key); err != nil && !errors.Is(err, credentials.ErrNotFound) {
		return fmt.Errorf("deleting the stored token: %w", err)
	}
	if revokeErr != nil {
		_, _ = fmt.Fprintf(env.Err,
			"spov: the token was deleted locally, but the server did not revoke it: %v\n"+
				"      it stops working when it expires, or revoke it from another signed-in machine.\n", revokeErr)
	}
	_, _ = fmt.Fprintf(env.Out, "Signed out of %s.\n", serverURL)
	return nil
}

// newWhoamiCmd prints who the server thinks you are.
//
// @decision(DL-006) @decision(DL-023)
func newWhoamiCmd(env *Env) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Print the identity the server sees",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := env.client(cmd.Context())
			if err != nil {
				return err
			}
			me, err := c.Me(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(env.Out, me)
			}
			if me.Email != "" {
				_, _ = fmt.Fprintf(env.Out, "%s\n", me.Email)
			}
			_, _ = fmt.Fprintf(env.Out, "user %s in org %s", me.UserID, me.OrgID)
			if me.Admin {
				_, _ = fmt.Fprint(env.Out, " (admin)")
			}
			_, _ = fmt.Fprintln(env.Out)
			return nil
		},
	}
	jsonFlag(cmd, &asJSON)
	return cmd
}

// hostedServerURL resolves the server a login targets. Local mode has no
// login: its credential is the state file the CLI wrote itself.
//
// @decision(DL-006)
func (e *Env) hostedServerURL() (string, error) {
	url, err := e.configuredServerURL()
	if err != nil {
		return "", err
	}
	if url == "" {
		return "", fmt.Errorf("no server configured: pass --server, set %s, or set server.url in config.\n"+
			"A local server needs no login — it authenticates with the token in its own state file", ServerEnvVar)
	}
	if err := config.ValidServerURL(url); err != nil {
		return "", fmt.Errorf("server URL: %w", err)
	}
	return url, nil
}

// configuredServerURL is the hosted server the CLI is pointed at: the
// --server flag, then SPOV_SERVER, then server.url in config. Empty means
// the CLI's own local server.
//
// @decision(DL-002)
func (e *Env) configuredServerURL() (string, error) {
	if e.ServerURL != "" {
		return e.ServerURL, nil
	}
	if url := e.Environ.Getenv(ServerEnvVar); url != "" {
		return url, nil
	}
	res, err := e.config()
	if err != nil {
		return "", err
	}
	return res.Config.Server.URL, nil
}

// storedServerToken is the token `spov login` kept for this server, or
// "" when there is none. A missing credential is not an error here: the
// request goes out unauthenticated and the 401 explains what to do,
// which is a better message than one about the keyring.
//
// @decision(DL-006)
func (e *Env) storedServerToken(serverURL string) string {
	if e.Credentials == nil {
		return ""
	}
	key, err := credentials.ServerKey(serverURL)
	if err != nil {
		return ""
	}
	token, err := e.Credentials.Get(key)
	if err != nil {
		return ""
	}
	return token
}

// ExplainAuth turns a hosted server's 401 into an instruction. Every
// command reaches the server through Env.client, so the advice belongs
// wherever the error surfaces rather than in each command.
//
// @decision(DL-006)
func (e *Env) ExplainAuth(err error) error {
	if err == nil || client.StatusOf(err) != http.StatusUnauthorized {
		return err
	}
	serverURL, urlErr := e.hostedServerURL()
	if urlErr != nil {
		return err
	}
	if e.Environ.Getenv(ServerTokenEnvVar) != "" {
		return fmt.Errorf("%w\nThe token in %s was refused; it may be revoked or expired", err, ServerTokenEnvVar)
	}
	return fmt.Errorf("%w\nRun: spov login --server %s", err, serverURL)
}
