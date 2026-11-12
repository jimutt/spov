package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/console"
	"github.com/jimutt/spov/internal/credentials"
	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/prep"
	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/safego"
	"github.com/jimutt/spov/internal/store"
)

// ConsoleClient is the OpenCode Console's device flow. *console.Client
// is the real one.
//
// @decision(DL-015)
type ConsoleClient interface {
	Start(ctx context.Context, now time.Time) (console.Device, error)
	Poll(ctx context.Context, deviceCode string, now time.Time) (console.PollResult, error)
	Refresh(ctx context.Context, refresh string, now time.Time) (console.Grant, error)
}

// ConsoleStore keeps each user's OpenCode Console grant and sign-in
// attempt as opaque values, in two slots. Locked runs fn while holding
// the user's lock, and fn reads and writes through the store it is
// given, never the outer one.
//
// @decision(DL-015)
type ConsoleStore interface {
	Get(ctx context.Context, orgID, userID, slot string) (string, error)
	Put(ctx context.Context, orgID, userID, slot, value string) error
	Delete(ctx context.Context, orgID, userID, slot string) error
	Locked(ctx context.Context, orgID, userID string, fn func(ConsoleStore) error) error
}

// The two slots a user's console state is kept in.
const (
	ConsoleSlotGrant   = "grant"
	ConsoleSlotAttempt = "attempt"
	// consolePlatformPrefix names the slots in the sealed credential
	// store. CanonicalPlatform refuses it, so the credential API can
	// neither write nor delete them.
	consolePlatformPrefix = "opencode:"
	// consoleLock is the credential the lock is taken on.
	consoleLock = "opencode"
)

const (
	// ConsoleRefreshMargin is how long a grant must outlast a new
	// session for its token to be used without a refresh.
	//
	// @decision(DL-015)
	ConsoleRefreshMargin = time.Hour
	// consoleTimeout bounds one call to the console.
	consoleTimeout = 30 * time.Second
	// pushBackoff is how long a session whose token write failed waits
	// before the renewal pass tries it again.
	//
	// @decision(DL-015)
	pushBackoff = 10 * time.Minute
	// consolePollRetries is how many retryable poll answers an attempt
	// survives before it ends.
	//
	// @decision(DL-015)
	consolePollRetries = 5
)

// ConsoleGrant is a user's stored OpenCode Console sign-in.
//
// @decision(DL-015)
type ConsoleGrant struct {
	Access    string    `json:"access"`
	Refresh   string    `json:"refresh"`
	ExpiresAt time.Time `json:"expires_at"`
	OrgID     string    `json:"org_id,omitempty"`
	// Generation goes up by one with every new token, from a sign-in or
	// a refresh. A session records the generation it holds.
	Generation int `json:"generation"`
	// NeedsReconnect is set when the console refused a refresh: the
	// grant is dead and the user must sign in again.
	NeedsReconnect bool      `json:"needs_reconnect,omitempty"`
	ConnectedAt    time.Time `json:"connected_at"`
}

// usable reports whether the grant's token can still be given out.
func (g ConsoleGrant) usable(now time.Time) bool {
	return g.connected() && g.ExpiresAt.After(now)
}

// connected reports whether the grant can be used, possibly after a
// refresh. A disconnected grant keeps only its generation.
func (g ConsoleGrant) connected() bool {
	return !g.NeedsReconnect && g.Access != "" && g.Refresh != ""
}

// consoleAttempt is a sign-in in progress, or how the last one ended.
type consoleAttempt struct {
	ID              string    `json:"id"`
	DeviceCode      string    `json:"device_code"`
	UserCode        string    `json:"user_code"`
	VerificationURL string    `json:"verification_url"`
	ExpiresAt       time.Time `json:"expires_at"`
	Interval        int       `json:"interval_s"`
	NextPoll        time.Time `json:"next_poll"`
	Retries         int       `json:"retries"`
	State           string    `json:"state"`
	Reason          string    `json:"reason,omitempty"`
}

// The states an attempt reports.
const (
	AttemptPending   = "pending"
	AttemptConnected = "connected"
	AttemptFailed    = "failed"
	AttemptExpired   = "expired"
)

// OpenCodeConnectionView is the caller's OpenCode sign-in, without any
// token.
//
// @decision(DL-015)
type OpenCodeConnectionView struct {
	// Enabled is false when this server cannot hold a sign-in at all.
	Enabled   bool `json:"enabled"`
	Connected bool `json:"connected"`
	// NeedsReconnect means the console refused the stored sign-in.
	NeedsReconnect bool      `json:"needs_reconnect,omitempty"`
	OrgID          string    `json:"org_id,omitempty"`
	ConnectedAt    time.Time `json:"connected_at,omitzero"`
	ExpiresAt      time.Time `json:"expires_at,omitzero"`
}

// OpenCodeAttemptView is a sign-in attempt as the CLI and the browser
// see it. The device code is never in it.
//
// @decision(DL-015)
type OpenCodeAttemptView struct {
	ID              string    `json:"id"`
	State           string    `json:"state" enum:"pending,connected,failed,expired"`
	Reason          string    `json:"reason,omitempty"`
	VerificationURL string    `json:"verification_url"`
	UserCode        string    `json:"user_code"`
	ExpiresAt       time.Time `json:"expires_at"`
	// Interval is how often, in seconds, a client should ask again.
	Interval int `json:"interval"`
}

func (a consoleAttempt) view() OpenCodeAttemptView {
	return OpenCodeAttemptView{
		ID: a.ID, State: a.State, Reason: a.Reason, VerificationURL: a.VerificationURL,
		UserCode: a.UserCode, ExpiresAt: a.ExpiresAt, Interval: a.Interval,
	}
}

// readGrant reads a user's grant from cs.
func readGrant(ctx context.Context, cs ConsoleStore, orgID, userID string) (ConsoleGrant, bool, error) {
	raw, err := cs.Get(ctx, orgID, userID, ConsoleSlotGrant)
	if err != nil || raw == "" {
		return ConsoleGrant{}, false, err
	}
	var g ConsoleGrant
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		return ConsoleGrant{}, false, fmt.Errorf("the stored OpenCode sign-in is unreadable: %w", err)
	}
	return g, true, nil
}

func writeGrant(ctx context.Context, cs ConsoleStore, orgID, userID string, g ConsoleGrant) error {
	b, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return cs.Put(ctx, orgID, userID, ConsoleSlotGrant, string(b))
}

func readAttempt(ctx context.Context, cs ConsoleStore, orgID, userID string) (consoleAttempt, bool, error) {
	raw, err := cs.Get(ctx, orgID, userID, ConsoleSlotAttempt)
	if err != nil || raw == "" {
		return consoleAttempt{}, false, err
	}
	var a consoleAttempt
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return consoleAttempt{}, false, fmt.Errorf("the OpenCode sign-in attempt is unreadable: %w", err)
	}
	return a, true, nil
}

func writeAttempt(ctx context.Context, cs ConsoleStore, orgID, userID string, a consoleAttempt) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return cs.Put(ctx, orgID, userID, ConsoleSlotAttempt, string(b))
}

// consoleEnabled reports whether this server can hold OpenCode sign-ins.
func (s *Service) consoleEnabled() bool {
	return s.deps.Console != nil && s.deps.ConsoleGrants != nil
}

// consoleCarried reports whether this server can give a sign-in to a
// session: only through the credential proxy, never the main container.
//
// @decision(DL-015)
func (s *Service) consoleCarried() bool {
	return s.consoleEnabled() && s.deps.ProxyImage != nil
}

// consoleStanding is where a user's sign-in stands for a launch.
type consoleStanding int

const (
	consoleNone consoleStanding = iota
	consoleHeld
	consoleDead
	consoleNoProxy
)

// consoleStandingFor reads the user's grant for the model check and the
// model list. A grant whose token has lapsed is still held: the launch
// refreshes it.
//
// @decision(DL-015)
func (s *Service) consoleStandingFor(ctx context.Context, orgID, userID string) consoleStanding {
	if !s.consoleEnabled() || userID == "" {
		return consoleNone
	}
	g, ok, err := readGrant(ctx, s.deps.ConsoleGrants, orgID, userID)
	switch {
	case err != nil:
		s.deps.Warn(fmt.Sprintf("OpenCode sign-in for %s: %s", userID, redact.Error(err)))
		return consoleNone
	case !ok:
		return consoleNone
	case g.NeedsReconnect:
		return consoleDead
	case !g.connected():
		return consoleNone
	case !s.consoleCarried():
		return consoleNoProxy
	}
	return consoleHeld
}

// consoleRefusal is why a model on an OpenCode provider cannot run when
// the sign-in is the reason, or "" when it is not.
//
// @decision(DL-015)
func consoleRefusal(st consoleStanding) string {
	switch st {
	case consoleDead:
		return "OpenCode refused your stored sign-in; reconnect OpenCode with `spov login opencode` or in Settings"
	case consoleNoProxy:
		return "your OpenCode sign-in is given to sessions only through the credential proxy, which this server is running without"
	}
	return ""
}

// consoleKey is the user's usable console token and its generation, for
// the provider keys a launch or a recreated sidecar carries. It never
// refreshes.
//
// @decision(DL-015)
func (s *Service) consoleKey(ctx context.Context, orgID, userID string) (string, int) {
	if !s.consoleCarried() || userID == "" {
		return "", 0
	}
	g, ok, err := readGrant(ctx, s.deps.ConsoleGrants, orgID, userID)
	if err != nil {
		s.deps.Warn(fmt.Sprintf("OpenCode sign-in for %s: %s", userID, redact.Error(err)))
		return "", 0
	}
	if !ok || !g.usable(s.deps.Now()) {
		return "", 0
	}
	return g.Access, g.Generation
}

// withConsoleKey returns keys with the user's console token as
// OPENCODE_API_KEY, which wins over a stored or deployment key. keys is
// not modified: it may be the deployment's own map.
//
// @decision(DL-015)
func withConsoleKey(keys map[string]string, token string) map[string]string {
	if token == "" {
		return keys
	}
	out := maps.Clone(keys)
	if out == nil {
		out = map[string]string{}
	}
	out[prep.OpenCodeKeyEnv] = token
	return out
}

// freshConsoleToken is the user's console token for a launch whose
// session lasts up to need, refreshed first when it would expire before
// that plus ConsoleRefreshMargin. It returns the token and its
// generation, or none, with a note for the launch log. Only an error
// that leaves the grant lost fails the launch.
//
// @decision(DL-015)
func (s *Service) freshConsoleToken(ctx context.Context, orgID, userID string, need time.Duration) (token string, generation int, note string, err error) {
	if !s.consoleEnabled() || userID == "" {
		return "", 0, "", nil
	}
	if !s.consoleCarried() {
		if s.consoleStandingFor(ctx, orgID, userID) == consoleNoProxy {
			return "", 0, "the OpenCode sign-in was left out: this server has no credential proxy to hold it", nil
		}
		return "", 0, "", nil
	}
	now := s.deps.Now()
	g, ok, err := readGrant(ctx, s.deps.ConsoleGrants, orgID, userID)
	if err != nil {
		s.deps.Warn(fmt.Sprintf("OpenCode sign-in for %s: %s", userID, redact.Error(err)))
		return "", 0, "your OpenCode sign-in could not be read; connect OpenCode again", nil
	}
	switch {
	case !ok || (!g.NeedsReconnect && !g.connected()):
		return "", 0, "", nil
	case g.NeedsReconnect:
		return "", 0, reconnectNote, nil
	case g.ExpiresAt.After(now.Add(need + ConsoleRefreshMargin)):
		return g.Access, g.Generation, "", nil
	}
	var refreshed bool
	// Detached from the launch: once the console has rotated the tokens,
	// a cancelled launch must not leave the new grant unstored.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), consoleTimeout+15*time.Second)
	defer cancel()
	err = s.deps.ConsoleGrants.Locked(lctx, orgID, userID, func(cs ConsoleStore) error {
		cur, ok, err := readGrant(lctx, cs, orgID, userID)
		if err != nil {
			return err
		}
		// Another launch may have refreshed it while this one waited.
		if !ok || !cur.connected() || cur.ExpiresAt.After(now.Add(need+ConsoleRefreshMargin)) {
			g = cur
			return nil
		}
		rctx, cancel := context.WithTimeout(lctx, consoleTimeout)
		defer cancel()
		next, err := s.deps.Console.Refresh(rctx, cur.Refresh, now)
		if errors.Is(err, console.ErrInvalidGrant) {
			cur.NeedsReconnect = true
			g = cur
			return writeGrant(lctx, cs, orgID, userID, cur)
		}
		if err != nil {
			g = cur
			return &consoleRefreshError{err: err}
		}
		g = ConsoleGrant{
			Access: next.Access, Refresh: next.Refresh, ExpiresAt: next.ExpiresAt,
			OrgID: cur.OrgID, Generation: cur.Generation + 1, ConnectedAt: cur.ConnectedAt,
		}
		if next.OrgID != "" {
			g.OrgID = next.OrgID
		}
		// The old tokens died with the refresh, so a grant that cannot be
		// stored is lost.
		if err := writeGrant(lctx, cs, orgID, userID, g); err != nil {
			return fmt.Errorf("the refreshed OpenCode sign-in could not be stored and is lost; connect OpenCode again: %w", err)
		}
		refreshed = true
		return nil
	})
	var rerr *consoleRefreshError
	switch {
	case errors.As(err, &rerr):
		s.deps.Warn(fmt.Sprintf("OpenCode sign-in for %s: refresh: %s", userID, redact.Error(rerr.err)))
		if g.usable(now) {
			return g.Access, g.Generation, fmt.Sprintf("the OpenCode sign-in could not be refreshed; its token expires %s", g.ExpiresAt.Format(time.RFC3339)), nil
		}
		return "", 0, "the OpenCode sign-in has expired and could not be refreshed; connect OpenCode again", nil
	case err != nil:
		return "", 0, "", err
	case g.NeedsReconnect:
		return "", 0, reconnectNote, nil
	case !g.usable(now):
		return "", 0, "", nil
	}
	if refreshed {
		s.pushes.Add(1)
		// @decision(DL-135)
		go safego.Do("opencode grant push", s.deps.Warn, func() {
			defer s.pushes.Done()
			s.pushOpenCodeGrants(context.WithoutCancel(ctx), func(sess store.Session) bool {
				return sess.OrgID == orgID && sess.OwnerID == userID
			})
		})
	}
	return g.Access, g.Generation, "", nil
}

// setKeyTimeout bounds writing a refreshed grant into a sidecar.
//
// @decision(DL-135)
const setKeyTimeout = 30 * time.Second

const reconnectNote = "OpenCode refused the stored sign-in; reconnect OpenCode (`spov login opencode`, or Settings)"

// consoleRefreshError is a refresh that failed without ending the grant.
type consoleRefreshError struct{ err error }

func (e *consoleRefreshError) Error() string { return e.err.Error() }

// pushOpenCodeGrants writes the owner's current console token into the
// sidecar of every live session that holds an older one. The renewal
// pass runs it every minute, and a refresh runs it at once for its user.
//
// @decision(DL-015)
func (s *Service) pushOpenCodeGrants(ctx context.Context, only func(store.Session) bool) {
	if !s.consoleEnabled() {
		return
	}
	sessions, err := s.deps.Store.Sessions().ListLive(ctx)
	if err != nil {
		s.deps.Warn(fmt.Sprintf("OpenCode sign-in: list sessions: %s", redact.Error(err)))
		return
	}
	type owner struct{ org, user string }
	grants := map[owner]ConsoleGrant{}
	now := s.deps.Now()
	live := map[string]bool{}
	// A push leaves the session's renewal state behind. A replica that
	// does not run RenewGitTokens forgets it here instead, when a later
	// push finds the session gone.
	defer func() { s.releaseEnded(ctx, live) }()
	for _, sess := range sessions {
		live[sess.ID] = true
		if sess.OpenCodeGrant == 0 || sess.State != store.StateRunning || sess.SandboxID == "" || (only != nil && !only(sess)) {
			continue
		}
		key := owner{sess.OrgID, sess.OwnerID}
		g, seen := grants[key]
		if !seen {
			var ok bool
			g, ok, err = readGrant(ctx, s.deps.ConsoleGrants, sess.OrgID, sess.OwnerID)
			if err != nil || !ok {
				g = ConsoleGrant{}
			}
			grants[key] = g
		}
		if !g.usable(now) || g.Generation <= sess.OpenCodeGrant {
			continue
		}
		// A write resets the sandbox's idle clock, so one that failed is
		// not tried again every minute.
		s.mu.Lock()
		failed := s.renewalFor(sess.ID).pushFailed
		s.mu.Unlock()
		if !failed.IsZero() && now.Sub(failed) < pushBackoff {
			continue
		}
		if err := s.pushOpenCodeGrant(ctx, sess, g); err != nil {
			s.mu.Lock()
			s.renewalFor(sess.ID).pushFailed = now
			s.mu.Unlock()
			s.deps.Warn(fmt.Sprintf("OpenCode sign-in for %s: write the new token: %s", sess.ID, redact.Error(err)))
		}
	}
}

// pushOpenCodeGrant writes one grant's token into one session's sidecar
// and records the generation. It takes the session's renewal lock, which
// a sidecar's recreation also takes.
//
// @decision(DL-015)
func (s *Service) pushOpenCodeGrant(ctx context.Context, sess store.Session, g ConsoleGrant) error {
	ctx, cancel := context.WithTimeout(ctx, renewTimeout)
	defer cancel()
	s.mu.Lock()
	r := s.renewalFor(sess.ID)
	s.mu.Unlock()
	r.work.Lock()
	defer r.work.Unlock()
	// Reread under the lock: a recreation may have written it already.
	cur, err := s.deps.Store.Sessions().Get(ctx, sess.OrgID, sess.ID)
	if err != nil {
		return err
	}
	if cur.OpenCodeGrant >= g.Generation {
		return nil
	}
	h, err := s.handle(ctx, cur)
	if err != nil {
		return err
	}
	// @decision(DL-135)
	xctx, cancelExec := context.WithTimeout(ctx, setKeyTimeout)
	defer cancelExec()
	res, err := h.SidecarExecInput(xctx, credproxy.SidecarName, credproxy.SetKeyCommand(prep.OpenCodeKeyEnv), []byte(g.Access+"\n"))
	if err == nil && res.ExitCode != 0 && strings.Contains(res.Stderr, "no provider key") {
		// The sidecar holds no OpenCode key to replace, so it never will.
		return s.deps.Store.Sessions().SetOpenCodeGrant(ctx, sess.OrgID, sess.ID, 0)
	}
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("set-key exited %d: %s", res.ExitCode, truncateFrame(redact.Text(res.Stderr)))
	}
	if err != nil {
		return err
	}
	return s.deps.Store.Sessions().SetOpenCodeGrant(ctx, sess.OrgID, sess.ID, g.Generation)
}

// newAttemptID is a random attempt identifier.
func newAttemptID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "oca_" + hex.EncodeToString(b), nil
}

// startConsoleSignIn begins a device sign-in for the caller, replacing
// any attempt in progress.
//
// @decision(DL-015)
func (s *Service) startConsoleSignIn(ctx context.Context, orgID, userID string) (consoleAttempt, error) {
	id, err := newAttemptID()
	if err != nil {
		return consoleAttempt{}, err
	}
	now := s.deps.Now()
	cctx, cancel := context.WithTimeout(ctx, consoleTimeout)
	defer cancel()
	d, err := s.deps.Console.Start(cctx, now)
	if err != nil {
		return consoleAttempt{}, huma.Error502BadGateway("the OpenCode Console did not start a sign-in: " + redact.Error(err))
	}
	a := consoleAttempt{
		ID: id, DeviceCode: d.Code, UserCode: d.UserCode, VerificationURL: d.VerificationURL,
		ExpiresAt: d.ExpiresAt, Interval: int(d.Interval / time.Second), NextPoll: now.Add(d.Interval),
		State: AttemptPending,
	}
	if err := s.deps.ConsoleGrants.Locked(ctx, orgID, userID, func(cs ConsoleStore) error {
		return writeAttempt(ctx, cs, orgID, userID, a)
	}); err != nil {
		return consoleAttempt{}, err
	}
	return a, nil
}

// checkConsoleSignIn reports an attempt, asking the console once when a
// poll is due. Polls are made under the user's lock, so a CLI and a
// browser asking about the same attempt never both use its device code.
//
// @decision(DL-015)
func (s *Service) checkConsoleSignIn(ctx context.Context, orgID, userID, id string) (consoleAttempt, error) {
	var out consoleAttempt
	// Detached from the request: a poll that spends the device code must
	// store the grant it got, whoever hung up.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), consoleTimeout+15*time.Second)
	defer cancel()
	err := s.deps.ConsoleGrants.Locked(ctx, orgID, userID, func(cs ConsoleStore) error {
		a, ok, err := readAttempt(ctx, cs, orgID, userID)
		if err != nil {
			return err
		}
		if !ok || a.ID != id {
			return huma.Error404NotFound("no such OpenCode sign-in attempt")
		}
		out = a
		now := s.deps.Now()
		switch {
		case a.State != AttemptPending:
			return nil
		case !now.Before(a.ExpiresAt):
			a.State, a.DeviceCode = AttemptExpired, ""
			out = a
			return writeAttempt(ctx, cs, orgID, userID, a)
		case now.Before(a.NextPoll):
			return nil
		}
		pctx, cancel := context.WithTimeout(ctx, consoleTimeout)
		defer cancel()
		res, err := s.deps.Console.Poll(pctx, a.DeviceCode, now)
		if err != nil {
			res = console.PollResult{State: console.PollRetry, Reason: redact.Error(err)}
		}
		a.NextPoll = now.Add(time.Duration(a.Interval) * time.Second)
		switch res.State {
		case console.PollPending:
		case console.PollRetry:
			a.Retries++
			if res.Reason == "slow_down" {
				a.Interval += 5
				a.NextPoll = now.Add(time.Duration(a.Interval) * time.Second)
			} else if a.Retries > consolePollRetries {
				a.State, a.Reason, a.DeviceCode = AttemptFailed, "the OpenCode Console kept failing: "+res.Reason, ""
			}
		case console.PollFailed:
			a.State, a.Reason, a.DeviceCode = AttemptFailed, consoleFailure(res.Reason), ""
		case console.PollApproved:
			prev, _, err := readGrant(ctx, cs, orgID, userID)
			if err != nil {
				prev = ConsoleGrant{}
			}
			g := ConsoleGrant{
				Access: res.Grant.Access, Refresh: res.Grant.Refresh, ExpiresAt: res.Grant.ExpiresAt,
				OrgID: res.Grant.OrgID, Generation: prev.Generation + 1, ConnectedAt: now,
			}
			if err := writeGrant(ctx, cs, orgID, userID, g); err != nil {
				return err
			}
			a.State, a.DeviceCode = AttemptConnected, ""
		}
		out = a
		return writeAttempt(ctx, cs, orgID, userID, a)
	})
	return out, err
}

// consoleFailure words the console's reason for ending a sign-in.
func consoleFailure(reason string) string {
	switch reason {
	case "access_denied":
		return "the sign-in was denied in the browser"
	case "expired_token":
		return "the sign-in code expired before it was approved"
	}
	return "the OpenCode Console ended the sign-in (" + reason + ")"
}

// consoleConnection is the caller's sign-in, for display.
func (s *Service) consoleConnection(ctx context.Context, orgID, userID string) (OpenCodeConnectionView, error) {
	v := OpenCodeConnectionView{Enabled: s.consoleEnabled()}
	if !v.Enabled {
		return v, nil
	}
	// An approval nobody was waiting for is collected here, so closing
	// the tab or the CLI before it lands loses nothing.
	if a, ok, err := readAttempt(ctx, s.deps.ConsoleGrants, orgID, userID); err == nil && ok && a.State == AttemptPending {
		if _, err := s.checkConsoleSignIn(ctx, orgID, userID, a.ID); err != nil {
			s.deps.Warn(fmt.Sprintf("OpenCode sign-in for %s: %s", userID, redact.Error(err)))
		}
	}
	g, ok, err := readGrant(ctx, s.deps.ConsoleGrants, orgID, userID)
	if err != nil || !ok {
		return v, err
	}
	v.Connected = g.connected()
	v.NeedsReconnect = g.NeedsReconnect
	v.OrgID, v.ConnectedAt, v.ExpiresAt = g.OrgID, g.ConnectedAt, g.ExpiresAt
	return v, nil
}

type openCodeConnectionOutput struct {
	Body OpenCodeConnectionView
}

type openCodeAttemptOutput struct {
	Body OpenCodeAttemptView
}

type openCodeAttemptInput struct {
	ID string `path:"id"`
}

// registerOpenCodeConsole serves /me/opencode: the caller's OpenCode
// Console sign-in. Every operation is the calling principal's own.
//
// @decision(DL-015)
func (s *Server) registerOpenCodeConsole() {
	register(s.api, huma.Operation{
		OperationID: "get-opencode-connection", Method: http.MethodGet, Path: "/api/v1/me/opencode",
		Summary: "The caller's OpenCode Console sign-in", Tags: []string{"credentials"},
	}, func(ctx context.Context, _ *struct{}) (*openCodeConnectionOutput, error) {
		p := principal(ctx)
		v, err := s.svc.consoleConnection(ctx, p.OrgID, p.UserID)
		if err != nil {
			return nil, toHumaError(err)
		}
		return &openCodeConnectionOutput{Body: v}, nil
	})

	register(s.api, huma.Operation{
		OperationID: "start-opencode-connection", Method: http.MethodPost, Path: "/api/v1/me/opencode/connect",
		Summary: "Start an OpenCode Console sign-in", Tags: []string{"credentials"},
	}, func(ctx context.Context, _ *struct{}) (*openCodeAttemptOutput, error) {
		p := principal(ctx)
		if err := s.consoleEnabled(); err != nil {
			return nil, err
		}
		a, err := s.svc.startConsoleSignIn(ctx, p.OrgID, p.UserID)
		if err != nil {
			return nil, toHumaError(err)
		}
		return &openCodeAttemptOutput{Body: a.view()}, nil
	})

	register(s.api, huma.Operation{
		OperationID: "check-opencode-connection", Method: http.MethodGet, Path: "/api/v1/me/opencode/connect/{id}",
		Summary: "Where an OpenCode Console sign-in stands", Tags: []string{"credentials"},
	}, func(ctx context.Context, in *openCodeAttemptInput) (*openCodeAttemptOutput, error) {
		p := principal(ctx)
		if err := s.consoleEnabled(); err != nil {
			return nil, err
		}
		a, err := s.svc.checkConsoleSignIn(ctx, p.OrgID, p.UserID, in.ID)
		if err != nil {
			return nil, toHumaError(err)
		}
		return &openCodeAttemptOutput{Body: a.view()}, nil
	})

	register(s.api, huma.Operation{
		OperationID: "delete-opencode-connection", Method: http.MethodDelete, Path: "/api/v1/me/opencode",
		Summary: "Forget the caller's OpenCode Console sign-in", Tags: []string{"credentials"},
	}, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
		p := principal(ctx)
		if err := s.consoleEnabled(); err != nil {
			return nil, err
		}
		err := s.svc.deps.ConsoleGrants.Locked(ctx, p.OrgID, p.UserID, func(cs ConsoleStore) error {
			if err := cs.Delete(ctx, p.OrgID, p.UserID, ConsoleSlotAttempt); err != nil {
				return err
			}
			g, ok, err := readGrant(ctx, cs, p.OrgID, p.UserID)
			if err != nil || !ok {
				return cs.Delete(ctx, p.OrgID, p.UserID, ConsoleSlotGrant)
			}
			// The tokens go; the generation stays, so a later sign-in is
			// newer than anything a live session holds.
			//
			// @decision(DL-015)
			return writeGrant(ctx, cs, p.OrgID, p.UserID, ConsoleGrant{Generation: g.Generation})
		})
		if err != nil {
			return nil, toHumaError(err)
		}
		return nil, nil
	})
}

// consoleEnabled is a clear 501 when this server cannot hold a sign-in.
func (s *Server) consoleEnabled() error {
	if !s.svc.consoleEnabled() {
		return huma.Error501NotImplemented("this server cannot hold an OpenCode sign-in:" +
			" a hosted server needs SPOV_CREDENTIAL_KEY, and a local one a credential store")
	}
	return nil
}

// SealedConsoleStore keeps console state in the sealed credential store,
// one row per slot, and locks with the store's credential lock.
//
// @decision(DL-015)
type SealedConsoleStore struct {
	Store  store.Store
	Sealer *auth.Sealer
	Now    func() time.Time
	inTx   bool
}

func (c SealedConsoleStore) creds() *CredentialStore {
	return &CredentialStore{Store: c.Store.Credentials(), Sealer: c.Sealer, Now: c.Now}
}

// Get returns a slot's value, or "".
func (c SealedConsoleStore) Get(ctx context.Context, orgID, userID, slot string) (string, error) {
	return c.creds().Get(ctx, orgID, userID, consolePlatformPrefix+slot)
}

// Put seals and stores a slot's value.
func (c SealedConsoleStore) Put(ctx context.Context, orgID, userID, slot, value string) error {
	return c.creds().Put(ctx, orgID, userID, consolePlatformPrefix+slot, value, Verified{})
}

// Delete removes a slot; a missing one is not an error.
func (c SealedConsoleStore) Delete(ctx context.Context, orgID, userID, slot string) error {
	err := c.creds().Delete(ctx, orgID, userID, consolePlatformPrefix+slot)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// Locked runs fn in a transaction holding the user's credential lock.
func (c SealedConsoleStore) Locked(ctx context.Context, orgID, userID string, fn func(ConsoleStore) error) error {
	if c.inTx {
		return errors.New("console store: nested lock")
	}
	return c.Store.Tx(ctx, func(tx store.Store) error {
		if err := tx.Credentials().Lock(ctx, orgID, userID, consoleLock); err != nil {
			return err
		}
		return fn(SealedConsoleStore{Store: tx, Sealer: c.Sealer, Now: c.Now, inTx: true})
	})
}

// MemoryConsoleStore keeps console state in memory, for tests and for a
// server that has nowhere else to put it.
type MemoryConsoleStore struct {
	mu     sync.Mutex
	lock   sync.Mutex
	values map[string]string
}

func memKey(orgID, userID, slot string) string { return orgID + "\x00" + userID + "\x00" + slot }

// Get returns a slot's value, or "".
func (m *MemoryConsoleStore) Get(_ context.Context, orgID, userID, slot string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.values[memKey(orgID, userID, slot)], nil
}

// Put stores a slot's value.
func (m *MemoryConsoleStore) Put(_ context.Context, orgID, userID, slot, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.values == nil {
		m.values = map[string]string{}
	}
	m.values[memKey(orgID, userID, slot)] = value
	return nil
}

// Delete removes a slot.
func (m *MemoryConsoleStore) Delete(_ context.Context, orgID, userID, slot string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, memKey(orgID, userID, slot))
	return nil
}

// Locked runs fn holding one lock for every user.
func (m *MemoryConsoleStore) Locked(_ context.Context, _, _ string, fn func(ConsoleStore) error) error {
	m.lock.Lock()
	defer m.lock.Unlock()
	return fn(m)
}

// isConsoleSlot reports whether a credential row is console state, which
// the credential listing leaves out.
func isConsoleSlot(platform string) bool {
	return strings.HasPrefix(platform, consolePlatformPrefix)
}

// LocalConsoleStore keeps console state in this machine's credential
// store, for a local server's one user.
//
// @decision(DL-015)
type LocalConsoleStore struct {
	Creds credentials.Store
	mu    sync.Mutex
}

func localConsoleKey(slot string) (string, error) { return credentials.Key("opencode", slot) }

// Get returns a slot's value, or "".
func (l *LocalConsoleStore) Get(_ context.Context, _, _, slot string) (string, error) {
	key, err := localConsoleKey(slot)
	if err != nil {
		return "", err
	}
	v, err := l.Creds.Get(key)
	if errors.Is(err, credentials.ErrNotFound) {
		return "", nil
	}
	return v, err
}

// Put stores a slot's value.
func (l *LocalConsoleStore) Put(_ context.Context, _, _, slot, value string) error {
	key, err := localConsoleKey(slot)
	if err != nil {
		return err
	}
	return l.Creds.Set(key, value)
}

// Delete removes a slot; a missing one is not an error.
func (l *LocalConsoleStore) Delete(_ context.Context, _, _, slot string) error {
	key, err := localConsoleKey(slot)
	if err != nil {
		return err
	}
	if err := l.Creds.Delete(key); err != nil && !errors.Is(err, credentials.ErrNotFound) {
		return err
	}
	return nil
}

// Locked runs fn holding the process's one lock.
func (l *LocalConsoleStore) Locked(_ context.Context, _, _ string, fn func(ConsoleStore) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fn(lockedLocal{l})
}

// lockedLocal is a LocalConsoleStore inside its lock.
type lockedLocal struct{ *LocalConsoleStore }

func (lockedLocal) Locked(context.Context, string, string, func(ConsoleStore) error) error {
	return errors.New("console store: nested lock")
}
