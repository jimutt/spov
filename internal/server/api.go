package server

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/danielgtaylor/huma/v2/sse"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/telemetry"
	"github.com/jimutt/spov/internal/webui"
)

const (
	// APIPrefix is where every versioned, authenticated operation lives.
	//
	// @decision(DL-005)
	APIPrefix = "/api/v1/"
	// APIVersion is the document's info.version. It names the contract,
	// not the build: a build stamp here changes the document on every
	// commit, and the committed spec is diffed against a regeneration.
	//
	// @decision(DL-005)
	APIVersion = "v1"
	// MaxBodyBytes bounds every request body, except session creation,
	// which may carry a base64-encoded sync bundle (DL-027).
	MaxBodyBytes = 1 << 20
	// MaxSessionBodyBytes bounds POST /api/v1/sessions when a sync bundle
	// is attached.
	//
	// @decision(DL-027)
	MaxSessionBodyBytes = 24 << 20
	// bearerScheme names the security scheme every API operation requires.
	bearerScheme = "bearer"
)

func maxBodyBytesFor(path string) int64 {
	if path == APIPrefix+"sessions" {
		return MaxSessionBodyBytes
	}
	return MaxBodyBytes
}

// Server is the HTTP API. Build it with New, serve Handler.
//
// @decision(DL-005)
type Server struct {
	deps Deps
	svc  *Service
	mux  *http.ServeMux
	api  huma.API
	// listenAddr is what the browser guard accepts as Host; set by
	// Listen or Guard.
	guard *auth.BrowserGuard
	// web serves the built web app, and reserved is the set of path
	// prefixes that belong to the server instead. They are taken from
	// huma's own config rather than written out again, so renaming a
	// docs path cannot silently hand it to the web app.
	//
	// @decision(DL-055)
	web      http.Handler
	reserved reservedPaths
	// checks caches the App routes' status checks.
	//
	// @decision(DL-042)
	checks checkCache
	// feed backs the caller-scoped state stream, and events backs one
	// session's log stream. Both read the store: after DL-018 no read
	// path in this package reaches a sandbox.
	//
	// @decision(DL-018)
	feed   sessionFeed
	events eventFeed
	// streamLifetime and heartbeat mirror the constants below, as
	// fields only so a test can watch a stream end without waiting five
	// minutes. Nothing outside this package sets them.
	//
	// @decision(DL-018)
	streamLifetime time.Duration
	heartbeat      time.Duration
	// revokeSessions ends one address's browser sessions when its
	// allowlist entry is deleted. A field, not a call through
	// deps.Login, so a test can see the delete trigger it without
	// standing up a session manager.
	//
	// @decision(DL-007)
	revokeSessions func(ctx context.Context, orgID, email string) error
}

// reservedPaths are the non-API surfaces the server owns.
//
// @decision(DL-055)
type reservedPaths struct {
	docs    string
	openAPI string
	schemas string
}

// New builds the API over deps. addr is the address clients will use
// (host:port), which the browser guard requires as the Host header.
func New(deps Deps, addr string) (*Server, error) {
	// Defaulted before anything reads them, so the server, the guard
	// and the service it owns all see the same values.
	deps = deps.withDefaults()
	guard, err := auth.NewBrowserGuard(addr, deps.Warn)
	if err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	// Behind a proxy the Host header names the public origin, not the
	// listener, and the guard runs before authentication.
	//
	// @decision(DL-010)
	if deps.PublicURL != "" {
		host, err := publicHost(deps.PublicURL)
		if err != nil {
			return nil, err
		}
		guard.AllowPublicHost(host)
	}
	s := &Server{deps: deps, svc: newService(deps), mux: http.NewServeMux(), guard: guard}
	// Polling, behind the Subscribe seam a LISTEN/NOTIFY feed replaces.
	//
	// @decision(DL-018)
	s.feed = pollFeed{list: s.svc.StreamList, stamp: s.decorationStamp, interval: StateFeedInterval, warn: deps.Warn}
	// Bound through the server rather than to the store's method value,
	// so construction does not depend on a store being set.
	s.events = pollEvents{
		list: func(ctx context.Context, orgID, sessionID string, after int64, limit int) ([]store.Event, error) {
			return s.deps.Store.Conversations().ListEvents(ctx, orgID, sessionID, after, limit)
		},
		interval: StateFeedInterval, page: LogPageSize, warn: deps.Warn,
	}
	s.streamLifetime, s.heartbeat = StreamLifetime, HeartbeatInterval
	// @decision(DL-007)
	if deps.Login != nil {
		s.revokeSessions = deps.Login.RevokeSessions
	}

	config := huma.DefaultConfig("spov", APIVersion)
	config.Info.Description = "The spov control plane."
	config.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		bearerScheme: {Type: "http", Scheme: "bearer"},
	}
	s.api = humago.New(s.mux, config)
	// @decision(DL-011)
	if deps.Metrics != nil {
		s.api.UseMiddleware(measureRequests(deps.Metrics, deps.Now))
	}
	s.reserved = reservedPaths{docs: config.DocsPath, openAPI: config.OpenAPIPath, schemas: config.SchemasPath}
	// Static public bytes, outside the browser guard: a completed
	// login arrives as a cross-site top-level navigation the guard
	// would otherwise answer 403.
	//
	// @decision(DL-055)
	s.web = webui.Handler(deps.WebUI)

	s.registerSessions()
	// @decision(DL-087)
	s.registerResume()
	// The live read surfaces.
	//
	// @decision(DL-018)
	s.registerLive()
	s.registerAsks()
	s.registerSwitchModel()
	s.registerOrgGitHub()
	// Autonomous jobs. Registered unconditionally so the API document
	// does not depend on how a deployment is configured; a server with
	// no supervisor refuses a create with 501.
	//
	// @decision(DL-045)
	s.registerJobs()
	s.registerRepos()
	s.registerMe()
	// Per-user credentials, in any mode: local mode has one user, whose
	// stored credentials are simply their own.
	//
	// @decision(DL-008)
	s.registerCredentials()
	// @decision(DL-073)
	s.registerRoutedCredentials()
	// @decision(DL-074)
	s.registerOrgRoutedCredentials()
	s.registerOpenCodeConsole()
	s.registerModels()
	// Per-user quotas, usage and the admin view of both.
	//
	// @decision(DL-009)
	s.registerUsage()
	s.registerUsers()
	// Who may log in. Registered unconditionally so the API document
	// does not depend on how a deployment is configured; the handlers
	// refuse a server that has no allowlist.
	//
	// @decision(DL-007)
	s.registerAllowlist()
	// The browser login flow, in hosted mode only.
	//
	// @decision(DL-007)
	if deps.Login != nil {
		deps.Login.register(s.mux)
		s.registerWebSessions()
		// GitHub redirects the browser here, so it arrives as a
		// top-level cross-site navigation; the `state` nonce is what
		// guards it.
		//
		// @decision(DL-007)
		guard.AllowCrossSiteNavigation(CallbackPath)
	}
	// A user's own CLI tokens. Registered unconditionally, like the
	// allowlist above: the handlers read only the token store, so a
	// server with no device flow answers an honest empty list rather
	// than a 404, and the API document stops depending on how a
	// deployment is configured. The web app's tokens page needs the
	// operations to be in the document to call them at all.
	//
	// @decision(DL-055) @decision(DL-006)
	s.registerTokens()
	// The CLI device flow: the endpoints a terminal polls, and the page
	// a human approves on.
	//
	// @decision(DL-006)
	if deps.Device != nil {
		deps.Device.register(s.mux)
	}

	// Liveness never touches a dependency: a database blip that failed
	// this would restart every replica at once.
	//
	// @decision(DL-010)
	s.mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	s.mux.HandleFunc("GET /readyz", s.handleReady(ReadyBudget))
	// What this deployment's sandboxes will run, for a client that has
	// no way to know and was comparing against its own configuration.
	//
	// @decision(DL-035)
	s.mux.HandleFunc("GET "+OpenCodePinPath, s.handlePin)
	// Startup is readiness with a longer budget, for an orchestrator's
	// startup probe while migrations are still being applied elsewhere.
	//
	// @decision(DL-010)
	s.mux.HandleFunc("GET /startupz", s.handleReady(StartupBudget))
	return s, nil
}

const (
	// ReadyBudget bounds the dependency checks behind /readyz.
	//
	// @decision(DL-010)
	ReadyBudget = 2 * time.Second
	// StartupBudget is the same checks with room for a cold database.
	//
	// @decision(DL-010)
	StartupBudget = 10 * time.Second
)

// handleReady reports whether this replica can serve: the store answers,
// and whatever else the deployment requires (in hosted mode, migrations
// at head).
//
// @decision(DL-010)
func (s *Server) handleReady(budget time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), budget)
		defer cancel()
		// The probe is unauthenticated, so the reason is logged and
		// not returned: a connection error names the database host,
		// user and database, which is free reconnaissance.
		if err := s.deps.Store.Ping(ctx); err != nil {
			s.deps.Warn("readiness: store: " + err.Error())
			http.Error(w, "store unavailable", http.StatusServiceUnavailable)
			return
		}
		if s.deps.Ready != nil {
			if err := s.deps.Ready(ctx); err != nil {
				s.deps.Warn("readiness: " + err.Error())
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// publicHost extracts the host (with port, if given) from the public
// URL. It insists on an absolute http(s) URL: a bare hostname here would
// silently produce a guard that matches nothing.
//
// @decision(DL-010)
func publicHost(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("server: public url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("server: public url %q needs an http or https scheme", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("server: public url %q has no host", raw)
	}
	return u.Host, nil
}

// Service exposes the session service, for the reconciler and tests.
func (s *Server) Service() *Service { return s.svc }

// WebUI is the built web app this server serves, after defaulting — so a
// caller reporting on it describes what is actually being served rather
// than what this binary happens to embed.
//
// @decision(DL-055)
func (s *Server) WebUI() fs.FS { return s.deps.WebUI }

// Handler returns the whole server: liveness and readiness open, the
// docs behind the browser guard, and everything under APIPrefix behind
// the guard and authentication.
//
// @decision(DL-005) @decision(DL-006)
func (s *Server) Handler() http.Handler {
	guardedAuthed := s.guard.Wrap(auth.Middleware(s.deps.Auth, s.deps.Warn)(s.mux))
	// The login routes are how a browser gets a credential, so they run
	// behind the guard but not behind authentication.
	//
	// @decision(DL-007)
	guardedOpen := s.guard.Wrap(s.mux)
	// The approval page uses BrowserAuth, not Auth, so a CLI token
	// cannot approve a device flow.
	//
	// @decision(DL-006)
	// With no browser authenticator every visitor is sent to log in.
	browserAuth := s.deps.BrowserAuth
	if browserAuth == nil {
		browserAuth = auth.StaticAuthenticator{Err: auth.ErrUnauthenticated}
	}
	guardedOptional := s.guard.Wrap(auth.Optional(browserAuth)(s.mux))
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Login != nil && isLoginPath(r.URL.Path) {
			guardedOpen.ServeHTTP(w, r)
			return
		}
		// A CLI polling the device flow has no credential yet — that is
		// what it is polling for — so those two endpoints are open.
		//
		// @decision(DL-006)
		if s.deps.Device != nil && isDevicePath(r.URL.Path) {
			guardedOpen.ServeHTTP(w, r)
			return
		}
		// An anonymous visitor is sent to log in rather than answered
		// 401 as an API call would be.
		//
		// @decision(DL-006)
		if s.deps.Device != nil && r.URL.Path == DeviceVerifyPath {
			guardedOptional.ServeHTTP(w, r)
			return
		}
		guardedAuthed.ServeHTTP(w, r)
	})
	// The session cookie is loaded for every request that reaches the
	// server's own surfaces: the login routes write it, and the cookie
	// authenticator reads it on the API.
	//
	// @decision(DL-007)
	var owned http.Handler = inner
	if s.deps.Login != nil {
		owned = s.deps.Login.Sessions.LoadAndSave(inner)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytesFor(r.URL.Path))
		// Health is outside the guard as well as outside auth: a load
		// balancer has no credential and sends no browser headers. It
		// is also outside the session, so a probe never touches the
		// session table.
		//
		// @decision(DL-006) @decision(DL-010)
		if isHealthPath(r.URL.Path) {
			s.mux.ServeHTTP(w, r)
			return
		}
		// The pin is outside all three for the same reasons: a client
		// reads it BEFORE it has a credential, it sends no browser
		// headers, and a published upstream release number is not
		// worth a session-table read.
		//
		// @decision(DL-035)
		if r.URL.Path == OpenCodePinPath {
			s.mux.ServeHTTP(w, r)
			return
		}
		// Everything the server does not own is the web app, served
		// outside the guard, auth and session: a logged-out browser
		// must be able to load the login page. Docs and schemas stay
		// behind all three, since they describe the API's shape and
		// that is free reconnaissance for anyone who can reach the
		// port but is not this user.
		//
		// @decision(DL-006) @decision(DL-010) @decision(DL-055)
		if !s.ownsPath(r.URL.Path) {
			s.web.ServeHTTP(w, r)
			return
		}
		// @decision(DL-071)
		withCompression(owned).ServeHTTP(w, r)
	})
}

// ownsPath reports whether a path is the server's rather than the web
// app's. An unregistered path under a surface the server owns stays the
// server's: a mistyped API path must answer as the API, not return the
// web app's HTML with a 200.
//
// @decision(DL-055)
func (s *Server) ownsPath(p string) bool {
	switch {
	case p == "/api", strings.HasPrefix(p, "/api/"):
		return true
	// Each guarded on being set: huma disables one of these routes by
	// blanking its path, and HasPrefix(p, "") is true of everything —
	// an empty OpenAPIPath would hand the server every path there is
	// and make the web app unreachable.
	case s.reserved.docs != "" && p == s.reserved.docs:
		return true
	case s.reserved.openAPI != "" && strings.HasPrefix(p, s.reserved.openAPI):
		return true
	case s.reserved.schemas != "" && (p == s.reserved.schemas || strings.HasPrefix(p, s.reserved.schemas+"/")):
		return true
	case isHealthPath(p):
		return true
	// @decision(DL-035)
	case p == OpenCodePinPath:
		return true
	}
	// The login and device surfaces are reserved whether or not this
	// deployment serves them, so local and hosted mode route a path the
	// same way and only differ in the answer.
	//
	// @decision(DL-007) @decision(DL-006)
	switch {
	case isLoginPath(p), strings.HasPrefix(p, "/oauth/"):
		return true
	case isDevicePath(p), p == DeviceVerifyPath, strings.HasPrefix(p, DeviceVerifyPath+"/"):
		return true
	}
	return false
}

// isHealthPath reports whether a path is one of the probe endpoints,
// which are unauthenticated and outside the browser guard.
//
// @decision(DL-010)
func isHealthPath(p string) bool {
	switch p {
	case "/livez", "/readyz", "/startupz":
		return true
	}
	return false
}

// OpenAPI returns the API document, as OpenAPI 3.1 or downgraded to 3.0.
//
// @decision(DL-005)
func (s *Server) OpenAPI(downgrade bool) ([]byte, error) {
	if downgrade {
		return s.api.OpenAPI().DowngradeYAML()
	}
	return s.api.OpenAPI().YAML()
}

// NewHTTPServer wraps the handler with the timeouts the API needs. There
// is no WriteTimeout: SSE streams are long-lived by design.
//
// @decision(DL-005)
func (s *Server) NewHTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// Close releases what the server holds without stopping any sandbox.
func (s *Server) Close() { s.svc.Close() }

// register is the only way an operation joins the API. Every operation
// under APIPrefix is served behind authentication by Handler, so it must
// also be documented as such; register refuses one that is not, at
// startup, so the document and the enforcement cannot disagree.
//
// @decision(DL-005)
func register[I, O any](api huma.API, op huma.Operation, handler func(context.Context, *I) (*O, error)) {
	requireSecurity(&op)
	huma.Register(api, op, handler)
}

// registerSSE is register for streams.
func registerSSE[I any](api huma.API, op huma.Operation, events map[string]any, handler func(context.Context, *I, sse.Sender)) {
	requireSecurity(&op)
	// @decision(DL-011)
	if op.Metadata == nil {
		op.Metadata = map[string]any{}
	}
	op.Metadata[streamMetadata] = true
	sse.Register(api, op, events, handler)
}

func requireSecurity(op *huma.Operation) {
	if !strings.HasPrefix(op.Path, APIPrefix) {
		panic(fmt.Sprintf("server: operation %s is outside %s and cannot be registered", op.OperationID, APIPrefix))
	}
	op.Security = []map[string][]string{{bearerScheme: {}}}
	op.Errors = append(op.Errors, http.StatusUnauthorized, http.StatusForbidden)
}

// streamMetadata marks an operation as a long-lived stream, whose
// duration says nothing about latency.
const streamMetadata = "spov.stream"

// measureRequests records every API operation by its operation ID and
// status class, and its duration unless it is a stream.
//
// @decision(DL-011)
func measureRequests(m *telemetry.Metrics, now func() time.Time) func(huma.Context, func(huma.Context)) {
	if now == nil {
		now = time.Now
	}
	return func(ctx huma.Context, next func(huma.Context)) {
		start := now()
		next(ctx)
		op := ctx.Operation()
		if op == nil {
			return
		}
		took := now().Sub(start)
		if stream, _ := op.Metadata[streamMetadata].(bool); stream {
			took = -1
		}
		m.APIRequest(ctx.Context(), op.OperationID, ctx.Status(), took)
	}
}
