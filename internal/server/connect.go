package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// Connection is what a client needs to reach a session: the edge URL and
// token, and the server password its proxy injects upstream. It is
// returned to the session's owner and never stored anywhere.
//
// @decision(DL-030)
type Connection struct {
	URL       string
	Token     string
	User      string
	Password  string
	ExpiresAt time.Time
	// IdleTimeout is the sandbox's idle timeout, so the client's proxy
	// can pace its heartbeat.
	//
	// @decision(DL-030)
	IdleTimeout time.Duration
	// sandbox is the sandbox the connection reaches, so a cached one is
	// never used for a resumed session's next sandbox.
	//
	// @decision(DL-085)
	sandbox string
}

const (
	// MintsPerMinute caps how often one session's credentials may be
	// minted. The 401s that drive re-minting come from the sandbox,
	// which is the untrusted side.
	//
	// @decision(DL-030)
	MintsPerMinute = 10
	// AssumedTokenTTL is used when a token carries no readable expiry.
	// The observed value is 24 h; connect_integration_test.go is the
	// standing proof.
	//
	// @decision(DL-030)
	AssumedTokenTTL = 24 * time.Hour
)

// ErrTooManyMints means the session's mint rate limit is exhausted.
var ErrTooManyMints = errors.New("server: too many connection requests for this session; try again shortly")

// ErrPasswordUnavailable means the session's password could not be
// recovered from its sandbox. The caller gets a 502 rather than a
// fabricated value, which would fail confusingly at the proxy instead.
//
// @decision(DL-003)
var ErrPasswordUnavailable = errors.New("server: password unavailable")

// Connect mints connection credentials for a running session.
//
// @decision(DL-030)
func (s *Service) Connect(ctx context.Context, p auth.Principal, ref string) (Connection, error) {
	sess, err := s.Get(ctx, p, ref)
	if err != nil {
		return Connection{}, err
	}
	if sess.State != store.StateRunning || sess.SandboxID == "" {
		return Connection{}, fmt.Errorf("%w (state %s)", ErrSessionNotRunning, sess.State)
	}

	now := s.deps.Now()
	// Counting and reserving are one atomic step, in one transaction that
	// holds the session's row, so the limit is exact across replicas.
	//
	// @decision(DL-030) @decision(DL-065)
	mintID := store.NewID(now)
	err = s.deps.Store.Tx(ctx, func(tx store.Store) error {
		if err := tx.Sessions().Lock(ctx, sess.OrgID, sess.ID); err != nil {
			return err
		}
		n, err := tx.Mints().CountSince(ctx, sess.OrgID, sess.ID, now.Add(-time.Minute))
		if err != nil {
			return err
		}
		if n >= MintsPerMinute {
			return ErrTooManyMints
		}
		return tx.Mints().Create(ctx, store.Mint{
			ID: mintID, OrgID: sess.OrgID, SessionID: sess.ID, UserID: p.UserID,
			Port: s.deps.Defaults.Port, CreatedAt: now,
		})
	})
	if err != nil {
		return Connection{}, err
	}

	h, err := s.handle(ctx, sess)
	if err != nil {
		return Connection{}, err
	}
	password, err := s.password(ctx, sess, h)
	if err != nil {
		return Connection{}, err
	}

	meta, err := connectMetadata(p.UserID, sess.ID)
	if err != nil {
		return Connection{}, err
	}
	port := s.deps.Defaults.Port
	conn, err := h.ConnectToken(ctx, port, meta)
	if err != nil {
		return Connection{}, err
	}

	expires := tokenExpiry(conn.Token)
	switch {
	case expires.IsZero():
		s.deps.Warn(fmt.Sprintf("connect %s: the token carries no readable expiry; assuming %s", sess.ID, AssumedTokenTTL))
		expires = now.Add(AssumedTokenTTL)
	case expires.After(now.Add(AssumedTokenTTL)):
		// The claim is the edge's to set, but an absurd one is clamped.
		s.deps.Warn(fmt.Sprintf("connect %s: the token claims to expire at %s; recording %s", sess.ID, expires, AssumedTokenTTL))
		expires = now.Add(AssumedTokenTTL)
	}
	if err := s.setMintExpiry(ctx, sess, mintID, expires); err != nil {
		s.deps.Warn(fmt.Sprintf("connect %s: record expiry: %v", sess.ID, err))
	}
	return Connection{
		URL: conn.URL, Token: conn.Token, User: opencode.BasicAuthUser, Password: password,
		ExpiresAt: expires, IdleTimeout: sess.Request.Sandbox.IdleTimeout, sandbox: sess.SandboxID,
	}, nil
}

// connectReusing is Connect for the server's OWN sandbox requests: it
// serves a cached connection when there is a usable one and mints only
// on a miss.
//
// It takes a session rather than a ref because the cache is keyed by
// session id and every server-side caller already holds the row. A ref
// would have to be resolved before the key existed, which is the store
// read the cache is there to sit in front of.
//
// A miss is always correct, so a caller that wants a fresh credential
// calls Connect.
//
// @decision(DL-030)
func (s *Service) connectReusing(ctx context.Context, p auth.Principal, sess store.Session) (Connection, error) {
	conn, ok := s.conns.get(sess.ID, s.deps.Now())
	if !ok {
		fresh, err := s.Connect(ctx, p, sess.ID)
		if err != nil {
			return Connection{}, err
		}
		s.conns.put(sess.ID, fresh)
		return fresh, nil
	}
	// A hit must still be authorized and the session still running; only
	// the mint is what the cache saves.
	cur, err := s.Get(ctx, p, sess.ID)
	if err != nil {
		return Connection{}, err
	}
	if cur.State != store.StateRunning || cur.SandboxID == "" {
		return Connection{}, fmt.Errorf("%w (state %s)", ErrSessionNotRunning, cur.State)
	}
	// A connection to an earlier run's sandbox reaches nothing.
	//
	// @decision(DL-085)
	if conn.sandbox != "" && conn.sandbox != cur.SandboxID {
		s.conns.forgetIf(sess.ID, conn.Token)
		fresh, err := s.Connect(ctx, p, sess.ID)
		if err != nil {
			return Connection{}, err
		}
		s.conns.put(sess.ID, fresh)
		return fresh, nil
	}
	return conn, nil
}

// withConnection runs one sandbox act over a reusable connection,
// minting again and retrying once if the sandbox refuses the
// credential. The retry is bounded to ErrSandboxUnauthorized, since a
// refused request did not take effect; no other failure is retried,
// since a 500 may well have already done the work and repeating it
// risks doing it twice. Every act wrapped here must be safe to run
// again from the top, which the caller — not this helper — guarantees.
// Both the connection and the password are dropped on a refusal, since
// a 401 does not say which was refused.
//
// @decision(DL-030) @decision(DL-003)
func (s *Service) withConnection(ctx context.Context, p auth.Principal, sess store.Session, fn func(Connection) error) error {
	conn, err := s.connectReusing(ctx, p, sess)
	if err != nil {
		return fmt.Errorf("connecting to the session's sandbox: %w", err)
	}
	err = fn(conn)
	if !errors.Is(err, ErrSandboxUnauthorized) {
		return err
	}
	// Conditional, so a peer that has already minted again keeps its
	// fresh entry rather than having it thrown away by this refusal.
	s.conns.forgetIf(sess.ID, conn.Token)
	s.passwords.forget(sess.ID)
	fresh, mintErr := s.Connect(ctx, p, sess.ID)
	if mintErr != nil {
		// The refusal is what the caller needs to see; the failed
		// re-mint is why nothing was done about it.
		return fmt.Errorf("%w (and minting again failed: %w)", err, mintErr)
	}
	s.conns.put(sess.ID, fresh)
	if err := fn(fresh); err != nil {
		// A second refusal is not retried, and the credential that
		// earned it must not stay cached: the next act would take it
		// and repeat this whole dance.
		if errors.Is(err, ErrSandboxUnauthorized) {
			s.conns.forgetIf(sess.ID, fresh.Token)
		}
		return err
	}
	return nil
}

// forgetRefusedConnection drops a session's cached connection when the
// sandbox refused it, so the next attempt mints rather than presenting
// the same token. The token is named so a peer's fresh mint survives.
//
// @decision(DL-030)
func (s *Service) forgetRefusedConnection(id, token string) {
	s.conns.forgetIf(id, token)
	s.passwords.forget(id)
}

// hasCachedConnection reports whether a session's connection is held in
// memory, and therefore whether the next act on it would MINT.
//
// Read by the tests of the paths that must drop it, and by Interrupt,
// which takes the recorder's mint reserve only when it would spend one
// (DL-053).
func (s *Service) hasCachedConnection(id string) bool { return s.conns.has(id) }

// setMintExpiry fills in the expiry once the token exists. The row was
// written before the mint to make the rate limit atomic.
func (s *Service) setMintExpiry(ctx context.Context, sess store.Session, mintID string, expires time.Time) error {
	return s.deps.Store.Mints().SetExpiry(ctx, sess.OrgID, mintID, expires)
}

// handle returns the sandbox handle for a session. The live handle is a
// cache of the launching replica's, not a requirement: any replica
// obtains one from the sandbox ID in the store.
//
// @decision(DL-003)
func (s *Service) handle(ctx context.Context, sess store.Session) (sandbox.Handle, error) {
	s.mu.Lock()
	live := s.live[sess.ID]
	s.mu.Unlock()
	if live != nil && live.Handle != nil {
		return live.Handle, nil
	}
	return s.deps.Provider.Get(ctx, sess.SandboxID)
}

// password returns the session's server password, recovering it from the
// sandbox once when this replica did not launch the session. It is held
// in memory only, in a bounded cache; the sandbox is the durable holder,
// so an evicted entry costs one exec.
//
// @decision(DL-030) @decision(DL-025) @decision(DL-003)
func (s *Service) password(ctx context.Context, sess store.Session, h sandbox.Handle) (string, error) {
	s.mu.Lock()
	live := s.live[sess.ID]
	s.mu.Unlock()
	if live != nil && live.Password != "" {
		return live.Password, nil
	}
	if pw, ok := s.passwords.getFor(sess.ID, sess.SandboxID); ok {
		return pw, nil
	}

	pw, err := session.PasswordFromSandbox(ctx, h)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrPasswordUnavailable, err)
	}
	s.passwords.putFor(sess.ID, sess.SandboxID, pw)
	return pw, nil
}

// connectMetadata is what the sandbox receives as a verified header. It
// names who the token was minted for, which is the hook for per-user
// audit inside the sandbox.
//
// @decision(DL-030)
func connectMetadata(userID, sessionID string) (string, error) {
	b, err := json.Marshal(struct {
		UserID    string `json:"user_id"`
		SessionID string `json:"session_id"`
	}{userID, sessionID})
	if err != nil {
		return "", err
	}
	if len(b) > sandbox.MaxConnectMetadata {
		return "", fmt.Errorf("server: connect metadata is %d bytes, over the %d limit", len(b), sandbox.MaxConnectMetadata)
	}
	return string(b), nil
}

// tokenExpiry reads the exp claim from a JWT without verifying it: the
// signature is the edge's business, and spov only needs the number to
// record what it handed out. A token it cannot read yields the zero
// time, and the caller assumes the observed default.
//
// @decision(DL-030)
func tokenExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0).UTC()
}

// ForgetSession drops what the server cached for a session. The
// reconciler calls it for every session it stops.
//
// @decision(DL-030)
func (s *Service) ForgetSession(id string) { s.forgetSession(id) }

// forgetSession drops a session's cached password, connection and
// proxy CA. Called when it stops: each serves a sandbox that is no
// longer there.
//
// @decision(DL-030) @decision(DL-034)
func (s *Service) forgetSession(id string) {
	s.passwords.forget(id)
	s.conns.forget(id)
	s.mu.Lock()
	delete(s.proxyCAs, id)
	s.mu.Unlock()
}

// hasCachedPassword reports whether a session's password is held in
// memory. For tests of the paths that must drop it.
func (s *Service) hasCachedPassword(id string) bool { return s.passwords.has(id) }
