package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jimutt/spov/internal/config"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/server"
)

// Every kind has an answer at each place the CLI and the server branch
// on a kind: a platform client of that kind, a user-tier credential key
// that splits back into it, and a stored secret. A new kind fails here
// until each place is decided.
//
// @decision(DL-041)
func TestEveryKindIsDecidedWhereTheCLIBranchesOnIt(t *testing.T) {
	// NewForgejo asks the server's version; nothing else is called.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"1.22.0"}`))
	}))
	defer srv.Close()
	env := envWith(nil)
	for _, kind := range git.Kinds {
		p := config.Platform{Name: "p", Kind: config.PlatformKind(kind), BaseURL: srv.URL, Account: "me"}
		token := "a-stored-token"
		if kind == git.KindGitHub {
			token = "github_pat_a-stored-token"
		}
		platform, err := newPlatform(context.Background(), p, token)
		if err != nil {
			t.Errorf("%s: newPlatform: %v", kind, err)
		} else if platform.Kind() != kind {
			t.Errorf("%s: platform kind %q", kind, platform.Kind())
		}
		key, ok := credentialPlatform(p)
		if !ok {
			t.Errorf("%s: no credential key", kind)
		}
		if kind != git.KindGitHub {
			if got, _, ok := server.PlatformHost(key); !ok || got != kind {
				t.Errorf("%s: key %q splits into %q", kind, key, got)
			}
		}
		if _, err := server.CanonicalPlatform(key); err != nil {
			t.Errorf("%s: key %q refused: %v", kind, key, err)
		}
		if hint := addCredentialHint(p); !strings.Contains(hint, key) {
			t.Errorf("%s: hint %q does not name %q", kind, hint, key)
		}
		if kind != git.KindGitHub {
			if _, err := platformSecret(env, p, addOptions{Token: token}); err != nil {
				t.Errorf("%s: platformSecret: %v", kind, err)
			}
		}
	}
}
