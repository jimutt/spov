package session

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/proxy"
	"github.com/jimutt/spov/internal/sandbox"
)

// StartProxy brings up the loopback proxy that fronts a sandbox. Every
// client reaches the session through it: the sandbox exposes no port, and
// the proxy is what holds the connect token and the server password.
//
// This is the launcher's own proxy, which mints straight from the
// provider because the launcher holds the handle. A client attaching to
// a session mints through the server instead (StartClientProxy).
//
// @decision(DL-030)
func StartProxy(ctx context.Context, h sandbox.Handle, port int, password string, warn func(string)) (*proxy.Proxy, error) {
	return start(ctx, func(ctx context.Context) (proxy.Credential, error) {
		// Minted per call rather than captured, so a token that has
		// expired mid-session is replaced by a fresh one.
		conn, err := h.ConnectToken(ctx, port, "")
		if err != nil {
			return proxy.Credential{}, err
		}
		return proxy.Credential{
			URL:      conn.URL,
			Token:    conn.Token,
			User:     opencode.BasicAuthUser,
			Password: password,
		}, nil
	}, 0, warn)
}

// Connector mints connection credentials for a session. The server's
// connect endpoint is the real one; it is an interface here so the
// session package does not depend on the API client.
//
// @decision(DL-030)
type Connector interface {
	Connect(ctx context.Context) (Connection, error)
}

// Connection is one set of minted credentials.
//
// @decision(DL-030)
type Connection struct {
	URL         string
	Token       string
	User        string
	Password    string
	IdleTimeout time.Duration
}

// ConnectorFunc adapts a function to Connector.
type ConnectorFunc func(ctx context.Context) (Connection, error)

// Connect implements Connector.
func (f ConnectorFunc) Connect(ctx context.Context) (Connection, error) { return f(ctx) }

// StartClientProxy brings up the loopback proxy a client runs, minting
// through the server rather than the provider. The client holds no
// provider credential and persists nothing the server returns.
//
// @decision(DL-030)
func StartClientProxy(ctx context.Context, c Connector, warn func(string)) (*proxy.Proxy, error) {
	// The first mint decides the heartbeat interval, so it happens here
	// rather than lazily: a proxy that cannot mint is a failure the
	// caller should see now.
	first, err := c.Connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	// The eager first credential is consumed once. A mutex rather than
	// a bare flag: the proxy calls mint from its own goroutines, and
	// this must not depend on how it happens to serialise them.
	var (
		mu      sync.Mutex
		pending = &first
	)
	return start(ctx, func(ctx context.Context) (proxy.Credential, error) {
		mu.Lock()
		conn := pending
		pending = nil
		mu.Unlock()
		if conn == nil {
			minted, err := c.Connect(ctx)
			if err != nil {
				return proxy.Credential{}, err
			}
			conn = &minted
		}
		return proxy.Credential{URL: conn.URL, Token: conn.Token, User: conn.User, Password: conn.Password}, nil
	}, first.IdleTimeout, warn)
}

func start(ctx context.Context, mint proxy.Mint, idle time.Duration, warn func(string)) (*proxy.Proxy, error) {
	p, err := proxy.Start(ctx, proxy.Options{Mint: mint, Heartbeat: proxy.HeartbeatFor(idle), Warn: warn})
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	return p, nil
}

// closeProxy stops the proxy, if there is one. Every path that ends a
// session runs it: the proxy is the only thing keeping the session
// reachable, and it holds credentials.
//
// @decision(DL-030)
func (s *Session) closeProxy() {
	if s.proxy == nil {
		return
	}
	_ = s.proxy.Close()
	s.proxy = nil
}
