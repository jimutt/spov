package postgres

import (
	"context"
	"time"

	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/store/postgres/sqlcgen"
)

type pgMints struct{ s *Store }

func (p pgMints) Create(ctx context.Context, m store.Mint) error {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = p.s.now()
	}
	return mapErr("create mint", p.s.q.CreateMint(ctx, sqlcgen.CreateMintParams{
		ID:        m.ID,
		OrgID:     m.OrgID,
		SessionID: m.SessionID,
		UserID:    m.UserID,
		Port:      int32(m.Port),
		ExpiresAt: ts(m.ExpiresAt),
		CreatedAt: ts(m.CreatedAt),
	}))
}

func (p pgMints) ListBySession(ctx context.Context, orgID, sessionID string) ([]store.Mint, error) {
	rows, err := p.s.q.ListMintsBySession(ctx, sqlcgen.ListMintsBySessionParams{OrgID: orgID, SessionID: sessionID})
	if err != nil {
		return nil, mapErr("list mints", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]store.Mint, 0, len(rows))
	for _, r := range rows {
		out = append(out, mintFromRow(r))
	}
	return out, nil
}

func (p pgMints) CountSince(ctx context.Context, orgID, sessionID string, t time.Time) (int, error) {
	n, err := p.s.q.CountMintsSince(ctx, sqlcgen.CountMintsSinceParams{
		OrgID: orgID, SessionID: sessionID, CreatedAt: ts(t),
	})
	if err != nil {
		return 0, mapErr("count mints", err)
	}
	return int(n), nil
}

func (p pgMints) SetExpiry(ctx context.Context, orgID, mintID string, expires time.Time) error {
	n, err := p.s.q.SetMintExpiry(ctx, sqlcgen.SetMintExpiryParams{ID: mintID, OrgID: orgID, ExpiresAt: ts(expires)})
	if err != nil {
		return mapErr("set mint expiry", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

type pgUsers struct{ s *Store }

func (p pgUsers) Create(ctx context.Context, u store.User) error {
	now := p.s.now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	quota, err := quotaJSON(u.Quota)
	if err != nil {
		return err
	}
	return mapErr("create user", p.s.q.CreateUser(ctx, sqlcgen.CreateUserParams{
		ID:          u.ID,
		OrgID:       u.OrgID,
		Email:       u.Email,
		Name:        u.Name,
		GithubLogin: u.GithubLogin,
		Admin:       u.Admin,
		GitName:     u.GitName,
		GitEmail:    u.GitEmail,
		// @decision(DL-014)
		DefaultModel: u.DefaultModel,
		// @decision(DL-043)
		DefaultGitAccess:    string(u.DefaultGitAccess),
		DefaultJobGitAccess: string(u.DefaultJobGitAccess),
		// @decision(DL-049)
		DefaultJobDurationSeconds: int64(u.DefaultJobDuration / time.Second),
		Quota:                     quota,
		LastLoginAt:               tsPtr(u.LastLoginAt),
		CreatedAt:                 ts(u.CreatedAt),
		UpdatedAt:                 ts(u.UpdatedAt),
	}))
}

func (p pgUsers) Get(ctx context.Context, orgID, id string) (store.User, error) {
	row, err := p.s.q.GetUser(ctx, sqlcgen.GetUserParams{ID: id, OrgID: orgID})
	if err != nil {
		return store.User{}, mapErr("get user", err)
	}
	return userFromRow(row), nil
}

func (p pgUsers) ByEmail(ctx context.Context, orgID, email string) (store.User, error) {
	if email == "" {
		return store.User{}, store.ErrNotFound
	}
	row, err := p.s.q.GetUserByEmail(ctx, sqlcgen.GetUserByEmailParams{OrgID: orgID, Lower: email})
	if err != nil {
		return store.User{}, mapErr("get user by email", err)
	}
	return userFromRow(row), nil
}

func (p pgUsers) List(ctx context.Context, orgID string) ([]store.User, error) {
	rows, err := p.s.q.ListUsers(ctx, orgID)
	if err != nil {
		return nil, mapErr("list users", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]store.User, 0, len(rows))
	for _, r := range rows {
		out = append(out, userFromRow(r))
	}
	return out, nil
}

func (p pgUsers) Update(ctx context.Context, u store.User) error {
	quota, err := quotaJSON(u.Quota)
	if err != nil {
		return err
	}
	n, err := p.s.q.UpdateUser(ctx, sqlcgen.UpdateUserParams{
		ID:          u.ID,
		OrgID:       u.OrgID,
		Email:       u.Email,
		Name:        u.Name,
		GithubLogin: u.GithubLogin,
		Admin:       u.Admin,
		GitName:     u.GitName,
		GitEmail:    u.GitEmail,
		// @decision(DL-014)
		DefaultModel: u.DefaultModel,
		// @decision(DL-043)
		DefaultGitAccess:    string(u.DefaultGitAccess),
		DefaultJobGitAccess: string(u.DefaultJobGitAccess),
		// @decision(DL-049)
		DefaultJobDurationSeconds: int64(u.DefaultJobDuration / time.Second),
		Quota:                     quota,
		LastLoginAt:               tsPtr(u.LastLoginAt),
		UpdatedAt:                 ts(p.s.now()),
	})
	if err != nil {
		return mapErr("update user", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (p pgUsers) Delete(ctx context.Context, orgID, id string) error {
	n, err := p.s.q.DeleteUser(ctx, sqlcgen.DeleteUserParams{ID: id, OrgID: orgID})
	if err != nil {
		return mapErr("delete user", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

type pgTokens struct{ s *Store }

func (p pgTokens) Create(ctx context.Context, t store.APIToken) error {
	if len(t.Hash) == 0 {
		return store.ErrConflict
	}
	now := p.s.now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	return mapErr("create token", p.s.q.CreateAPIToken(ctx, sqlcgen.CreateAPITokenParams{
		ID:         t.ID,
		OrgID:      t.OrgID,
		UserID:     t.UserID,
		TokenHash:  t.Hash,
		Name:       t.Name,
		LastUsedAt: tsPtr(t.LastUsedAt),
		ExpiresAt:  tsPtr(t.ExpiresAt),
		RevokedAt:  tsPtr(t.RevokedAt),
		CreatedAt:  ts(t.CreatedAt),
		UpdatedAt:  ts(t.UpdatedAt),
	}))
}

func (p pgTokens) ByHash(ctx context.Context, hash []byte) (store.APIToken, error) {
	if len(hash) == 0 {
		return store.APIToken{}, store.ErrNotFound
	}
	row, err := p.s.q.GetAPITokenByHash(ctx, hash)
	if err != nil {
		return store.APIToken{}, mapErr("token by hash", err)
	}
	return tokenFromRow(row), nil
}

// ByID implements store.TokenStore.
//
// @decision(DL-006)
func (p pgTokens) ByID(ctx context.Context, orgID, id string) (store.APIToken, error) {
	row, err := p.s.q.GetAPIToken(ctx, sqlcgen.GetAPITokenParams{ID: id, OrgID: orgID})
	if err != nil {
		return store.APIToken{}, mapErr("get token", err)
	}
	return tokenFromRow(row), nil
}

func (p pgTokens) ListByUser(ctx context.Context, orgID, userID string) ([]store.APIToken, error) {
	rows, err := p.s.q.ListAPITokensByUser(ctx, sqlcgen.ListAPITokensByUserParams{OrgID: orgID, UserID: userID})
	if err != nil {
		return nil, mapErr("list tokens", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]store.APIToken, 0, len(rows))
	for _, r := range rows {
		out = append(out, tokenFromRow(r))
	}
	return out, nil
}

func (p pgTokens) Touch(ctx context.Context, orgID, id string, at time.Time) error {
	n, err := p.s.q.TouchAPIToken(ctx, sqlcgen.TouchAPITokenParams{ID: id, OrgID: orgID, LastUsedAt: ts(at)})
	if err != nil {
		return mapErr("touch token", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (p pgTokens) Revoke(ctx context.Context, orgID, id string, at time.Time) error {
	n, err := p.s.q.RevokeAPIToken(ctx, sqlcgen.RevokeAPITokenParams{
		ID: id, OrgID: orgID, RevokedAt: ts(at), UpdatedAt: ts(p.s.now()),
	})
	if err != nil {
		return mapErr("revoke token", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

type pgDeviceCodes struct{ s *Store }

func (p pgDeviceCodes) Create(ctx context.Context, d store.DeviceCode) error {
	if len(d.Hash) == 0 || d.UserCode == "" {
		return store.ErrConflict
	}
	now := p.s.now()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	d.UpdatedAt = now
	if d.Status == "" {
		d.Status = store.DevicePending
	}
	return mapErr("create device code", p.s.q.CreateDeviceCode(ctx, sqlcgen.CreateDeviceCodeParams{
		ID:             d.ID,
		OrgID:          d.OrgID,
		DeviceCodeHash: d.Hash,
		UserCode:       d.UserCode,
		UserID:         d.UserID,
		Status:         string(d.Status),
		Name:           d.Name,
		ExpiresAt:      ts(d.ExpiresAt),
		LastPolledAt:   tsPtr(d.LastPolledAt),
		IntervalS:      int32(d.IntervalSeconds),
		CreatedAt:      ts(d.CreatedAt),
		UpdatedAt:      ts(d.UpdatedAt),
	}))
}

func (p pgDeviceCodes) ByHash(ctx context.Context, hash []byte) (store.DeviceCode, error) {
	if len(hash) == 0 {
		return store.DeviceCode{}, store.ErrNotFound
	}
	row, err := p.s.q.GetDeviceCodeByHash(ctx, hash)
	if err != nil {
		return store.DeviceCode{}, mapErr("device code by hash", err)
	}
	return deviceCodeFromRow(row), nil
}

func (p pgDeviceCodes) ByUserCode(ctx context.Context, userCode string) (store.DeviceCode, error) {
	if userCode == "" {
		return store.DeviceCode{}, store.ErrNotFound
	}
	row, err := p.s.q.GetDeviceCodeByUserCode(ctx, userCode)
	if err != nil {
		return store.DeviceCode{}, mapErr("device code by user code", err)
	}
	return deviceCodeFromRow(row), nil
}

func (p pgDeviceCodes) Update(ctx context.Context, d store.DeviceCode) error {
	n, err := p.s.q.UpdateDeviceCode(ctx, sqlcgen.UpdateDeviceCodeParams{
		ID:           d.ID,
		OrgID:        d.OrgID,
		UserID:       d.UserID,
		Status:       string(d.Status),
		Name:         d.Name,
		ExpiresAt:    ts(d.ExpiresAt),
		LastPolledAt: tsPtr(d.LastPolledAt),
		IntervalS:    int32(d.IntervalSeconds),
		UpdatedAt:    ts(p.s.now()),
	})
	if err != nil {
		return mapErr("update device code", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (p pgDeviceCodes) Delete(ctx context.Context, id string) error {
	n, err := p.s.q.DeleteDeviceCode(ctx, id)
	if err != nil {
		return mapErr("delete device code", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (p pgDeviceCodes) DeleteExpired(ctx context.Context, before time.Time) (int, error) {
	n, err := p.s.q.DeleteExpiredDeviceCodes(ctx, ts(before))
	if err != nil {
		return 0, mapErr("reap device codes", err)
	}
	return int(n), nil
}

type pgCredentials struct{ s *Store }

func (p pgCredentials) Put(ctx context.Context, c store.UserCredential) error {
	now := p.s.now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	return mapErr("put credential", p.s.q.PutUserCredential(ctx, sqlcgen.PutUserCredentialParams{
		ID:         c.ID,
		OrgID:      c.OrgID,
		UserID:     c.UserID,
		Platform:   c.Platform,
		Ciphertext: c.Ciphertext,
		Nonce:      c.Nonce,
		KeyVersion: int32(c.KeyVersion),
		VerifiedAs: c.VerifiedAs,
		CreatedAt:  ts(c.CreatedAt),
		UpdatedAt:  ts(c.UpdatedAt),
		Scopes:     nonNilStrings(c.Scopes),
		ExpiresAt:  ts(c.ExpiresAt),
	}))
}

func (p pgCredentials) Get(ctx context.Context, orgID, userID, platform string) (store.UserCredential, error) {
	row, err := p.s.q.GetUserCredential(ctx, sqlcgen.GetUserCredentialParams{
		OrgID: orgID, UserID: userID, Platform: platform,
	})
	if err != nil {
		return store.UserCredential{}, mapErr("get credential", err)
	}
	return credentialFromRow(row), nil
}

func (p pgCredentials) ListByUser(ctx context.Context, orgID, userID string) ([]store.UserCredential, error) {
	rows, err := p.s.q.ListUserCredentials(ctx, sqlcgen.ListUserCredentialsParams{OrgID: orgID, UserID: userID})
	if err != nil {
		return nil, mapErr("list credentials", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]store.UserCredential, 0, len(rows))
	for _, r := range rows {
		out = append(out, credentialFromRow(r))
	}
	return out, nil
}

// Lock takes a transaction-scoped advisory lock on one user's credential,
// so two replicas refreshing it take turns.
//
// @decision(DL-015)
func (p pgCredentials) Lock(ctx context.Context, orgID, userID, platform string) error {
	return mapErr("lock credential", p.s.q.LockUserCredential(ctx, sqlcgen.LockUserCredentialParams{
		OrgID: orgID, UserID: userID, Platform: platform,
	}))
}

func (p pgCredentials) Delete(ctx context.Context, orgID, userID, platform string) error {
	n, err := p.s.q.DeleteUserCredential(ctx, sqlcgen.DeleteUserCredentialParams{
		OrgID: orgID, UserID: userID, Platform: platform,
	})
	if err != nil {
		return mapErr("delete credential", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}
