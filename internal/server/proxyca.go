package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// A sandbox keeps its launch's proxy CA for its whole life: the server
// holds it, sealed with the credential key where there is one and in
// this process's memory where there is not (or the store write failed),
// and a recreated sidecar reuses it.
//
// @decision(DL-034)

// proxyCAAAD binds a sealed proxy CA to its org and session, so a row
// copied to another session does not open.
func proxyCAAAD(orgID, sessionID string) []byte {
	return []byte("spov-proxy-ca\x00" + orgID + "\x00" + sessionID)
}

// keepProxyCA holds a session's proxy CA; a failure to store it only
// warns, since a recreation then makes a new CA.
//
// @decision(DL-034)
func (s *Service) keepProxyCA(ctx context.Context, sess store.Session, ca *session.ProxyCA) {
	if ca == nil || ca.Cert == "" || ca.Key == "" {
		return
	}
	if err := s.sealProxyCA(ctx, sess, ca); err == nil {
		return
	} else if !errors.Is(err, errNoSealer) {
		s.deps.Warn(fmt.Sprintf("session %s: keeping the proxy CA: %s", sess.ID, redact.Error(err)))
	}
	s.mu.Lock()
	if s.proxyCAs == nil {
		s.proxyCAs = map[string]session.ProxyCA{}
	}
	s.proxyCAs[sess.ID] = *ca
	s.mu.Unlock()
}

// errNoSealer is a server with no credential key to seal with.
var errNoSealer = errors.New("no credential key")

// sealProxyCA stores ca sealed, bound to its org and session.
//
// @decision(DL-034)
func (s *Service) sealProxyCA(ctx context.Context, sess store.Session, ca *session.ProxyCA) error {
	creds := s.deps.Credentials
	if creds == nil || creds.Sealer == nil {
		return errNoSealer
	}
	plain, err := json.Marshal(ca)
	if err != nil {
		return err
	}
	ciphertext, nonce, version, err := creds.Sealer.Seal(plain, proxyCAAAD(sess.OrgID, sess.ID))
	if err != nil {
		return err
	}
	return s.deps.Store.Sessions().SetProxyCA(ctx, sess.OrgID, sess.ID,
		store.SealedValue{Ciphertext: ciphertext, Nonce: nonce, KeyVersion: version})
}

// proxyCAFor is the session's kept proxy CA, or nil when there is none
// this server can open.
//
// @decision(DL-034)
func (s *Service) proxyCAFor(ctx context.Context, sess store.Session) *session.ProxyCA {
	s.mu.Lock()
	ca, ok := s.proxyCAs[sess.ID]
	s.mu.Unlock()
	if ok {
		return &ca
	}
	creds := s.deps.Credentials
	if creds == nil || creds.Sealer == nil {
		return nil
	}
	sealed, err := s.deps.Store.Sessions().ProxyCA(ctx, sess.OrgID, sess.ID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.deps.Warn(fmt.Sprintf("session %s: reading the proxy CA: %s", sess.ID, redact.Error(err)))
		}
		return nil
	}
	plain, err := creds.Sealer.Open(sealed.Ciphertext, sealed.Nonce, sealed.KeyVersion, proxyCAAAD(sess.OrgID, sess.ID))
	if err == nil {
		err = json.Unmarshal(plain, &ca)
	}
	if err != nil {
		s.deps.Warn(fmt.Sprintf("session %s: opening the proxy CA: %s", sess.ID, redact.Error(err)))
		return nil
	}
	return &ca
}
