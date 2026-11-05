package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/routedcred"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/server/client"
)

// routeOptions are the flags that make `credentials add` store a routed
// credential.
//
// @decision(DL-073)
type routeOptions struct {
	Hosts      []string
	Paths      []string
	Bearer     bool
	Basic      string
	Header     string
	Attach     string
	SecretFile string
}

// set reports whether any routed-credential flag was given, which is
// what makes `add` store one.
func (o routeOptions) set() bool {
	return len(o.Hosts) > 0 || len(o.Paths) > 0 || o.Bearer || o.Basic != "" || o.Header != "" ||
		o.Attach != "" || o.SecretFile != ""
}

// route is the route the flags describe.
func (o routeOptions) route(name string) (routedcred.Route, error) {
	r := routedcred.Route{Name: name, Hosts: o.Hosts, Paths: o.Paths, Attach: o.Attach}
	schemes := 0
	if o.Bearer {
		r.Scheme, schemes = credproxy.SchemeBearer, schemes+1
	}
	if o.Basic != "" {
		r.Scheme, r.User, schemes = credproxy.SchemeBasic, o.Basic, schemes+1
	}
	if o.Header != "" {
		r.Scheme, r.Header, schemes = credproxy.SchemeHeader, o.Header, schemes+1
	}
	switch {
	case len(o.Hosts) == 0:
		return r, errors.New("a routed credential needs --host, the host it is set on")
	case schemes != 1:
		return r, errors.New("a routed credential needs exactly one of --bearer, --basic <user> and --header <name>")
	}
	r = r.Normal()
	return r, r.Validate()
}

// addRouteFlags adds the routed-credential flags to `credentials add`.
//
// @decision(DL-073)
func addRouteFlags(cmd *cobra.Command, o *routeOptions) {
	f := cmd.Flags()
	f.StringArrayVar(&o.Hosts, "host", nil, "store a routed credential set on this exact host (repeatable)")
	f.StringArrayVar(&o.Paths, "path", nil, "set it only below this path prefix on its hosts, as /repo/ (repeatable)")
	f.BoolVar(&o.Bearer, "bearer", false, "send it as Authorization: Bearer")
	f.StringVar(&o.Basic, "basic", "", "send it as Basic auth with this username")
	f.StringVar(&o.Header, "header", "", "send it as the value of this header")
	f.StringVar(&o.Attach, "attach", "", "always (default): every session gets it; asked: only a launch that names it")
	f.StringVar(&o.SecretFile, "secret-file", "", "read the secret from this file, or - for standard input; prompted for if omitted")
}

// routeName is a routed credential's name as typed: route:<name>, or a
// bare name.
func routeName(arg string) (string, bool) {
	if name, ok := cutPrefixFold(strings.TrimSpace(arg), routedcred.KeyPrefix); ok {
		return name, true
	}
	return strings.TrimSpace(arg), false
}

// shadowsAPlatform reports whether a bare name could also name a
// platform: github, a per-host key, or a platform in this machine's
// config.
//
// @decision(DL-073)
func shadowsAPlatform(env *Env, name string) bool {
	if strings.EqualFold(name, server.PlatformGitHub) || strings.Contains(name, ":") {
		return true
	}
	res, err := env.config()
	if err != nil {
		return false
	}
	_, err = findPlatform(res.Config, name)
	return err == nil
}

// addRoutedCredential stores a routed credential through the server,
// which keeps it sealed when hosted and in this machine's store locally.
//
// @decision(DL-073)
func addRoutedCredential(ctx context.Context, env *Env, arg string, opts routeOptions, token string) error {
	name, _ := routeName(arg)
	if !credproxy.ValidRouteName(name) {
		return fmt.Errorf("%q cannot name a routed credential: use lowercase letters, digits and -, starting with a letter, at most %d characters", name, credproxy.MaxRouteName)
	}
	// @decision(DL-075)
	if name == server.NoCredentials {
		return fmt.Errorf("%q is reserved: `credentials: [none]` in config attaches no routed credential", name)
	}
	if shadowsAPlatform(env, name) {
		return fmt.Errorf("%q already names a platform; give the routed credential another name", name)
	}
	if token != "" {
		return errors.New("a routed credential's secret comes from the prompt or --secret-file, never a flag")
	}
	r, err := opts.route(name)
	if err != nil {
		return err
	}
	secret, err := routeSecret(env, name, opts.SecretFile)
	if err != nil {
		return err
	}
	src, err := routeSource(ctx, env)
	if err != nil {
		return err
	}
	if err := src.Put(ctx, "", "", r, secret); err != nil {
		return fmt.Errorf("%s", redactSecret(err.Error(), secret))
	}
	_, _ = fmt.Fprintf(env.Out, "Stored %s %s, unchecked: %s.\n", name, whereRoutesAre(env), routeLine(r))
	return nil
}

// routeSecret reads the secret from a file, standard input or a prompt.
func routeSecret(env *Env, name, file string) (string, error) {
	var secret string
	switch file {
	case "":
		s, err := env.promptSecret("Secret for " + name + ": ")
		if err != nil {
			return "", err
		}
		secret = s
	case "-":
		if env.Stdin == nil {
			return "", errors.New("no standard input to read the secret from")
		}
		b, err := io.ReadAll(io.LimitReader(env.Stdin, credproxy.MaxRouteSecret+1))
		if err != nil {
			return "", err
		}
		secret = string(b)
	default:
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		secret = string(b)
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", errors.New("the secret is empty")
	}
	return secret, nil
}

// whereRoutesAre says where `add` put a routed credential.
func whereRoutesAre(env *Env) string {
	if hosted, err := env.hostedServer(); err == nil && hosted {
		return "on the server"
	}
	return "on this machine"
}

// removeRoutedCredential removes one through the server, reporting
// whether there was one to remove.
//
// @decision(DL-073)
func removeRoutedCredential(ctx context.Context, env *Env, name string) (bool, error) {
	held, err := listRoutedCredentials(ctx, env)
	if err != nil {
		return false, err
	}
	found := false
	for _, h := range held {
		found = found || h.Route.Name == name
	}
	if !found {
		return false, nil
	}
	src, err := routeSource(ctx, env)
	if err != nil {
		return false, err
	}
	if _, err := src.Delete(ctx, "", "", name); err != nil {
		return false, err
	}
	_, _ = fmt.Fprintf(env.Out, "Removed %s %s.\n", name, strings.Replace(whereRoutesAre(env), "on", "from", 1))
	return true, nil
}

// isRoutedName reports whether a name `remove` was given is a routed
// credential's: route:<name>, or a bare name that cannot be a platform or
// a provider key.
//
// @decision(DL-073)
func isRoutedName(env *Env, arg string) (string, bool) {
	name, prefixed := routeName(arg)
	if prefixed {
		return name, true
	}
	n := parseCredentialName(arg)
	return name, n.Provider == "" && credproxy.ValidRouteName(name) && !shadowsAPlatform(env, name)
}

// listRoutedCredentials reads the routed credentials, or none from a
// server that cannot hold them.
//
// @decision(DL-073)
func listRoutedCredentials(ctx context.Context, env *Env) ([]server.RoutedCredentialView, error) {
	src, err := routeSource(ctx, env)
	if err != nil {
		return nil, err
	}
	views, err := src.List(ctx, "", "")
	var apiErr *client.Error
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotImplemented || apiErr.Status == http.StatusNotFound) {
		return nil, nil
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Route.Name < views[j].Route.Name })
	return views, err
}

// routeSource is where this CLI's routed credentials are kept: the
// hosted server, through its API, or this machine's store and index,
// which the local server reads, written directly as other local
// credentials are.
//
// @decision(DL-073) @decision(DL-008)
func routeSource(ctx context.Context, env *Env) (server.RoutedCredentialSource, error) {
	hosted, err := env.hostedServer()
	if err != nil {
		return nil, err
	}
	if !hosted {
		if env.Credentials == nil {
			return nil, errors.New("no credential store configured")
		}
		dir, err := env.stateDir()
		if err != nil {
			return nil, err
		}
		return server.LocalRoutedCredentials{Local: &routedcred.Local{
			Creds: env.Credentials, Path: filepath.Join(dir, routedcred.IndexName)}}, nil
	}
	c, err := env.client(ctx)
	if err != nil {
		return nil, err
	}
	return hostedRoutes{c}, nil
}

// hostedRoutes is a hosted server's routed credentials, through its API.
type hostedRoutes struct{ c *client.Client }

func (h hostedRoutes) List(ctx context.Context, _, _ string) ([]server.RoutedCredentialView, error) {
	return h.c.ListRoutedCredentials(ctx)
}

func (h hostedRoutes) Put(ctx context.Context, _, _ string, r routedcred.Route, secret string) error {
	_, err := h.c.PutRoutedCredential(ctx, r, secret)
	return err
}

func (h hostedRoutes) Delete(ctx context.Context, _, _, name string) (bool, error) {
	return true, h.c.DeleteRoutedCredential(ctx, name)
}

// Open is the server's alone: no secret comes back from the API.
func (hostedRoutes) Open(context.Context, string, string) ([]server.HeldRoute, error) {
	return nil, errors.New("a hosted server's routed credentials are opened only by the server")
}

// printRoutedCredentials ends a listing with the routed credentials.
//
// @decision(DL-073)
func printRoutedCredentials(env *Env, views []server.RoutedCredentialView) {
	if len(views) == 0 {
		return
	}
	_, _ = fmt.Fprintln(env.Out, "Routed credentials:")
	for _, v := range views {
		line := "  " + v.Route.Name + "  " + routeLine(v.Route)
		if v.Missing {
			line += "  (missing: add it again)"
		}
		_, _ = fmt.Fprintln(env.Out, line)
	}
}

// routeLine is a route as a listing shows it: hosts and paths, the
// scheme, and whether it attaches only when asked.
func routeLine(r routedcred.Route) string {
	where := strings.Join(r.Hosts, ", ")
	if len(r.Paths) > 0 {
		where += " " + strings.Join(r.Paths, ", ")
	}
	scheme := r.Scheme
	switch r.Scheme {
	case credproxy.SchemeBasic:
		scheme = "basic (" + r.User + ")"
	case credproxy.SchemeHeader:
		scheme = "header " + r.Header
	}
	line := where + "  " + scheme
	if r.Preset != "" {
		line += "  preset " + r.Preset
	}
	if r.Attach == routedcred.AttachAsked {
		line += "  only when asked"
	}
	return line
}

// routedFlags are a launch's own choice of routed credentials.
//
// @decision(DL-075)
type routedFlags struct {
	Names []string
	None  bool
}

// addRoutedFlags adds --credentials and --no-credentials to a launch
// command; key is the configuration key they override.
//
// @decision(DL-075)
func addRoutedFlags(cmd *cobra.Command, r *routedFlags, key string) {
	cmd.Flags().StringSliceVar(&r.Names, "credentials", nil,
		"attach exactly these routed credentials, asked-only ones included; "+key+", then all you hold, when unset")
	cmd.Flags().BoolVar(&r.None, "no-credentials", false, "attach no routed credential")
	cmd.MarkFlagsMutuallyExclusive("credentials", "no-credentials")
}
