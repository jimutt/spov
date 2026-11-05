package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/store"
)

// The organisation's own GitHub App, stored sealed in the database for a
// configured platform. The deployment's environment, when it sets one,
// wins and this is not read.
//
// @decision(DL-042)

// OrgAppCacheTTL is how long a replica keeps a decrypted App before it
// reads the row again; the replica that changes it drops its copy at
// once.
//
// @decision(DL-042)
const OrgAppCacheTTL = 30 * time.Second

// OrgAppMeta is what may be shown about a stored App. The private key is
// never in it.
//
// @decision(DL-042)
type OrgAppMeta struct {
	AppID          string `json:"app_id"`
	Account        string `json:"account"`
	InstallationID int64  `json:"installation_id"`
}

// OrgApp is a stored App, opened.
type OrgApp struct {
	Credential git.GitHubCredential
	Meta       OrgAppMeta
	UpdatedBy  string
	UpdatedAt  time.Time
}

// OrgApps reads and writes the org's stored GitHub Apps, sealing them on
// the way in. A nil OrgApps, or one with no sealer, holds none.
//
// @decision(DL-042)
type OrgApps struct {
	Store  store.Store
	Sealer *auth.Sealer
	Now    func() time.Time

	mu    sync.Mutex
	cache map[string]cachedApp
}

type cachedApp struct {
	app   OrgApp
	found bool
	at    time.Time
}

// ErrOrgAppUnreadable is a stored App that exists and cannot be opened.
var ErrOrgAppUnreadable = errors.New("server: the stored GitHub App cannot be opened")

// Get returns the stored App for a platform, and whether there is one.
//
// @decision(DL-042)
func (o *OrgApps) Get(ctx context.Context, orgID, platform string) (OrgApp, bool, error) {
	if o == nil || o.Store == nil || o.Sealer == nil {
		return OrgApp{}, false, nil
	}
	key := orgID + "\x00" + platform
	o.mu.Lock()
	if c, ok := o.cache[key]; ok && o.now().Sub(c.at) < OrgAppCacheTTL {
		o.mu.Unlock()
		return c.app, c.found, nil
	}
	o.mu.Unlock()
	row, err := o.Store.OrgCredentials().Get(ctx, orgID, platform)
	if errors.Is(err, store.ErrNotFound) {
		o.remember(key, OrgApp{}, false)
		return OrgApp{}, false, nil
	}
	if err != nil {
		return OrgApp{}, false, err
	}
	plain, err := o.Sealer.Open(row.Ciphertext, row.Nonce, row.KeyVersion, auth.OrgCredentialAAD(orgID, platform))
	if err != nil {
		return OrgApp{}, false, fmt.Errorf("%w: %w", ErrOrgAppUnreadable, err)
	}
	cred, err := git.ParseGitHubCredential(string(plain))
	if err != nil {
		return OrgApp{}, false, fmt.Errorf("%w: %w", ErrOrgAppUnreadable, err)
	}
	var meta OrgAppMeta
	_ = json.Unmarshal(row.Meta, &meta)
	app := OrgApp{Credential: cred, Meta: meta, UpdatedBy: row.UpdatedBy, UpdatedAt: row.UpdatedAt}
	o.remember(key, app, true)
	return app, true, nil
}

// Sealed is the row storing an App: the credential sealed under the
// org's AAD, the meta in the clear. The caller writes it, in its own
// transaction, and then calls Forget.
//
// @decision(DL-042)
func (o *OrgApps) Sealed(orgID, platform, by string, cred git.GitHubCredential, meta OrgAppMeta) (store.OrgCredential, error) {
	if o == nil || o.Sealer == nil {
		return store.OrgCredential{}, auth.ErrNoKey
	}
	plain, err := json.Marshal(cred)
	if err != nil {
		return store.OrgCredential{}, err
	}
	ct, nonce, version, err := o.Sealer.Seal(plain, auth.OrgCredentialAAD(orgID, platform))
	if err != nil {
		return store.OrgCredential{}, err
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return store.OrgCredential{}, err
	}
	return store.OrgCredential{
		ID: store.NewID(o.now()), OrgID: orgID, Platform: platform,
		Ciphertext: ct, Nonce: nonce, KeyVersion: version, Meta: metaJSON, UpdatedBy: by,
	}, nil
}

// Forget drops this replica's cached copy of a platform's App.
//
// @decision(DL-042)
func (o *OrgApps) Forget(orgID, platform string) {
	if o != nil {
		o.forget(orgID + "\x00" + platform)
	}
}

func (o *OrgApps) remember(key string, app OrgApp, found bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.cache == nil {
		o.cache = map[string]cachedApp{}
	}
	o.cache[key] = cachedApp{app: app, found: found, at: o.now()}
}

func (o *OrgApps) forget(key string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.cache, key)
}

func (o *OrgApps) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}
