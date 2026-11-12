package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/store"
)

// A missing proxy tag on a sandbox the record says holds its token in the
// proxy fails the write: the main container is never chosen by default.
//
// @decision(DL-135) @decision(DL-034)
func TestAMissingProxyTagNeverSendsTheTokenToTheMainContainer(t *testing.T) {
	f := newProxiedRenewFixture(t)
	if got := f.row().Credentials; len(got) == 0 || got[0].Holder != store.CredentialInProxy {
		t.Fatalf("the launch did not record the proxy as the holder: %+v", got)
	}
	f.due(10 * time.Minute)
	f.act("session.next.prompted", store.SourceOpenCode)
	f.handle().DropTag(sandbox.TagProxy)

	f.srv.svc.RenewGitTokens(context.Background())

	if got := f.refreshes(); len(got) != 0 {
		t.Errorf("the token was written into the main container: %v", got)
	}
	for _, in := range f.handle().ExecInputs() {
		if len(in) > 0 {
			t.Errorf("the main container was given %q on stdin", in)
		}
	}
	if row := f.row(); row.GitTokenExpiresAt != nil && time.Until(*row.GitTokenExpiresAt) > 50*time.Minute {
		t.Error("the expiry moved on although nothing was written")
	}
}

// Where the record says the launch left the token in the sandbox, or a
// deployment without a proxy adopted a sandbox whose holder is unsaid,
// the main container is still the target.
//
// @decision(DL-135)
func TestARecordedSandboxHolderStillGetsItsRefresh(t *testing.T) {
	s := &Service{deps: Deps{}}
	info := sandbox.Info{}
	inSandbox := store.Session{Credentials: []store.SessionCredential{{Platform: "gh", Holder: store.CredentialInSandbox}}}
	if proxied, err := s.tokenInProxy(inSandbox, "gh", info); proxied || err != nil {
		t.Errorf("a recorded sandbox holder = %v, %v", proxied, err)
	}
	unknown := store.Session{Credentials: []store.SessionCredential{{Platform: "gh"}}}
	if proxied, err := s.tokenInProxy(unknown, "gh", info); proxied || err != nil {
		t.Errorf("an unrecorded holder with no proxy deployed = %v, %v", proxied, err)
	}
	s.deps.ProxyImage = func(context.Context) (string, error) { return "x", nil }
	if _, err := s.tokenInProxy(unknown, "gh", info); !errors.Is(err, ErrProxyUnknown) {
		t.Errorf("an unrecorded holder beside a proxy = %v, want ErrProxyUnknown", err)
	}
	if proxied, err := s.tokenInProxy(unknown, "gh", sandbox.Info{Tags: map[string]string{sandbox.TagProxy: "1"}}); !proxied || err != nil {
		t.Errorf("a proxy tag = %v, %v", proxied, err)
	}
}
