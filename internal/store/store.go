package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// Store is what the server persists. The interface is the schema: an
// implementation that cannot answer one of these queries is not a Store.
//
// @decision(DL-003)
type Store interface {
	Sessions() SessionStore
	Mints() MintStore
	// Users, Tokens, DeviceCodes and Credentials serve hosted mode.
	// Local mode holds one fixed user and never writes the rest.
	//
	// @decision(DL-003)
	Users() UserStore
	Tokens() TokenStore
	DeviceCodes() DeviceCodeStore
	Credentials() CredentialStore
	// OrgCredentials holds the org's own shared credentials, sealed, and
	// AdminEvents the audit trail of admins changing them. Hosted only.
	//
	// @decision(DL-042)
	OrgCredentials() OrgCredentialStore
	AdminEvents() AdminEventStore
	// Allowlist is who may log in. Hosted mode only: local mode has one
	// fixed user and no allowlist at all.
	//
	// @decision(DL-007)
	Allowlist() AllowlistStore
	// Conversations is what a session said and what happened around it,
	// kept after the sandbox is gone.
	//
	// @decision(DL-017)
	Conversations() ConversationStore
	// Jobs is the supervised work a session was launched to do.
	//
	// @decision(DL-045)
	Jobs() JobStore
	// Runs is each sandbox a session had.
	//
	// @decision(DL-085)
	Runs() RunStore
	// Tx runs fn atomically where the backend can. The memory store
	// serialises transactions with a mutex so the semantics hold there
	// too. fn must not call back into the outer store, must not retain
	// the store it is given past its return, and must not nest Tx; the
	// memory store fails loudly on each.
	Tx(ctx context.Context, fn func(Store) error) error
	Ping(ctx context.Context) error
}

// SessionStore holds sessions. Every query is scoped to an org.
//
// @decision(DL-003)
type SessionStore interface {
	Create(ctx context.Context, s Session) error
	// Get resolves a session by ID or, failing that, by name among the
	// org's live sessions.
	Get(ctx context.Context, orgID, ref string) (Session, error)
	// Lock holds the session's row, by ID, until the enclosing Tx ends, so
	// a count of its mints and the insert of a new one are one step on
	// every replica. Outside Tx it only checks that the session exists.
	//
	// @decision(DL-065)
	Lock(ctx context.Context, orgID, id string) error
	// List returns the matching sessions newest first (by ID, which is
	// a ULID), the page f.Before and f.Limit name.
	//
	// @decision(DL-012)
	List(ctx context.Context, orgID string, f SessionFilter) ([]Session, error)
	// Count counts the sessions List would return without Before and
	// Limit.
	//
	// @decision(DL-012)
	Count(ctx context.Context, orgID string, f SessionFilter) (int, error)
	// Update replaces the stored session. UpdatedAt is set by the store.
	Update(ctx context.Context, s Session) error
	// BySandbox and ListLive are the reconciler's queries and the only
	// ones that cross orgs: it works from what the provider reports,
	// which has no org. Request handlers never call them.
	BySandbox(ctx context.Context, sandboxID string) (Session, error)
	ListLive(ctx context.Context) ([]Session, error)
	// RequestCancel marks a starting session as cancelled, durably, and
	// reports whether there was one to mark. A launch runs on one
	// replica but may be deleted from any, and the pre-sandbox window
	// is exactly when there is no sandbox to terminate instead — so the
	// signal cannot live in the launching process's memory.
	//
	// @decision(DL-003)
	RequestCancel(ctx context.Context, orgID, id string, at time.Time) (bool, error)
	// CountLive counts a user's starting and running sessions. Called
	// inside the create transaction it is what makes a concurrency
	// quota a limit rather than a suggestion: two creates racing on
	// different replicas serialise on the same rows.
	//
	// @decision(DL-009)
	CountLive(ctx context.Context, orgID, ownerID string) (int, error)
	// CancelRequested reports whether a cancel was recorded. Called
	// inside a transaction it takes the row lock, which is what makes
	// cancelling and committing a launch mutually exclusive across
	// replicas rather than only within one.
	//
	// @decision(DL-003)
	CancelRequested(ctx context.Context, orgID, id string) (bool, error)
	// MarkStopped moves a live session to state (stopped or failed) at
	// at, with errText, and reports whether this call made the move. The
	// same write ends the session's current run (DL-085). A
	// session no longer live, or not this org's, is left alone and
	// reports false, so of two callers stopping the same session exactly
	// one sees true.
	//
	// @decision(DL-011)
	MarkStopped(ctx context.Context, orgID, id string, state SessionState, at time.Time, errText string) (bool, error)
	// SetGitToken records a session's current renewable git token: when
	// it was minted and when it lapses. A write of its own, so a caller
	// holding an older copy of the row cannot undo a re-mint.
	//
	// @decision(DL-021)
	SetGitToken(ctx context.Context, orgID, id string, minted, expires time.Time) error
	// SetOpenCodeGrant records which generation of its owner's OpenCode
	// Console grant the session's sidecar holds. A write of its own, as
	// SetGitToken is.
	//
	// @decision(DL-015)
	SetOpenCodeGrant(ctx context.Context, orgID, id string, generation int) error
	// SetProxyCA keeps the session's proxy CA, sealed by the caller, so a
	// recreated sidecar can reuse it; a second write replaces the first,
	// and MarkStopped deletes it. ProxyCA reads it back, or ErrNotFound.
	// No listing or view reads these.
	//
	// @decision(DL-034)
	SetProxyCA(ctx context.Context, orgID, id string, ca SealedValue) error
	ProxyCA(ctx context.Context, orgID, id string) (SealedValue, error)
}

// SealedValue is a secret sealed with auth.Sealer: the ciphertext, its
// nonce and the key version that sealed it.
//
// @decision(DL-034)
type SealedValue struct {
	Ciphertext []byte
	Nonce      []byte
	KeyVersion int
}

// MintStore records every credential mint.
//
// @decision(DL-003)
type MintStore interface {
	Create(ctx context.Context, m Mint) error
	ListBySession(ctx context.Context, orgID, sessionID string) ([]Mint, error)
	// CountSince counts a session's mints at or after t, for rate limits.
	CountSince(ctx context.Context, orgID, sessionID string, t time.Time) (int, error)
	// SetExpiry records when a mint's credential stops working. It is
	// separate from Create because the row is written before the token
	// is minted, so that counting and reserving are one atomic step.
	SetExpiry(ctx context.Context, orgID, mintID string, expires time.Time) error
}

var (
	ErrNotFound = errors.New("store: not found")
	// ErrNestedTx is Tx called from inside Tx.
	ErrNestedTx = errors.New("store: nested transaction")
	// ErrConflict is a duplicate ID, or a live session with the same name
	// in the same org.
	ErrConflict = errors.New("store: conflict")
)

// SessionState is where a session is in its life.
//
// @decision(DL-003)
type SessionState string

const (
	StateStarting SessionState = "starting"
	StateRunning  SessionState = "running"
	StateStopped  SessionState = "stopped"
	StateFailed   SessionState = "failed"
)

// Live reports whether the session may still have a sandbox.
func (s SessionState) Live() bool {
	return s == StateStarting || s == StateRunning
}

// LiveStates is Live as a list, for a filter that has to name the
// states. Kept beside Live so the two cannot drift; a test asserts they
// agree.
//
// @decision(DL-012)
var LiveStates = []SessionState{StateStarting, StateRunning}

// Session is one launched (or launching) sandbox session.
//
// @decision(DL-003)
type Session struct {
	ID        string
	OrgID     string
	OwnerID   string
	Name      string
	State     SessionState
	SandboxID string
	Request   SessionRequest
	Clones    []string
	Landing   string
	// Error is the terminal failure, redacted of anything secret.
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
	StoppedAt *time.Time
	// CancelRequestedAt is set when a delete arrived while the session
	// was still starting. The launching replica reads it before it
	// commits, whichever replica recorded it.
	//
	// @decision(DL-003)
	CancelRequestedAt *time.Time
	// Credentials says what the sandbox was given for each platform.
	// Written by the launch, with Clones.
	//
	// @decision(DL-043)
	Credentials []SessionCredential
	// RoutedCredentials are the routed credentials the launch attached,
	// never a secret. Written by the launch, with Clones.
	//
	// @decision(DL-075)
	RoutedCredentials []SessionRoutedCredential
	// GitTokenMintedAt and GitTokenExpiresAt are the session's current
	// renewable git token: when it was minted and when it lapses. Never
	// the token. Written only by SetGitToken; Update keeps the stored
	// values.
	//
	// @decision(DL-021)
	GitTokenMintedAt  *time.Time
	GitTokenExpiresAt *time.Time
	// OpenCodeGrant is the generation of the owner's OpenCode Console
	// grant the session holds, or 0 for none. Written only by
	// SetOpenCodeGrant; Update keeps the stored value.
	//
	// @decision(DL-015)
	OpenCodeGrant int
	// Booted is the image the sandbox was created from and the add-ons
	// it installs. Written by the launch, with Clones.
	//
	// @decision(DL-033)
	Booted BootedImage
}

// BootedImage is what a sandbox was created from: the image's name, and
// its add-ons at the versions installed (`name@version`), which can be
// older patches than the newest when the launch used an older build.
//
// @decision(DL-033) @decision(DL-016)
type BootedImage struct {
	Image  string   `json:"image,omitempty"`
	Addons []string `json:"addons,omitempty"`
}

// SessionRoutedCredential is one routed credential a session holds: its
// name, the tier it came from, and its route as the launch resolved it.
//
// @decision(DL-075)
type SessionRoutedCredential struct {
	Name   string   `json:"name"`
	Tier   string   `json:"tier"`
	Hosts  []string `json:"hosts"`
	Paths  []string `json:"paths,omitempty"`
	Scheme string   `json:"scheme"`
	User   string   `json:"user,omitempty"`
	Header string   `json:"header,omitempty"`
	Preset string   `json:"preset,omitempty"`
}

// RoutedRequest is which routed credentials a launch asks for: its own
// list (Names, or None for none), the narrowing its configuration sent
// (Preferred, where "none" alone means none), and the hosts its project
// says it needs.
//
// @decision(DL-075)
type RoutedRequest struct {
	Names     []string `json:",omitempty"`
	None      bool     `json:",omitempty"`
	Preferred []string `json:",omitempty"`
	Needs     []string `json:",omitempty"`
}

// CredentialKind is what a sandbox holds for one platform.
//
// @decision(DL-043)
type CredentialKind string

const (
	// CredentialRenewed is a GitHub App token, narrowed to the session's
	// repositories and re-minted while the session is in use (DL-021).
	CredentialRenewed CredentialKind = "renewed"
	// CredentialStored is a stored token, handed over as it is.
	CredentialStored CredentialKind = "stored"
)

// SessionCredential is one platform's credential kind. It names the
// kind and nothing about the secret.
//
// @decision(DL-043) @decision(DL-034)
type SessionCredential struct {
	Platform string         `json:"platform"`
	Kind     CredentialKind `json:"kind"`
	// Holder is where the token is: CredentialInProxy, CredentialInSandbox,
	// or empty when not recorded.
	Holder CredentialHolder `json:"holder,omitempty"`
	// Forge is the platform's kind: "github", "forgejo", "gitea" or
	// "gitlab" (DL-041).
	Forge string `json:"forge,omitempty"`
	// Access is how far the token reaches: GitAccessRepos, GitAccessAll,
	// or empty on a session recorded before it was, which is repos.
	//
	// @decision(DL-043)
	Access GitAccess `json:"access,omitempty"`
	// Tier is the credential tier the launch resolved for this platform,
	// "user" or "org", which every later step resolves again and only;
	// empty on a session recorded before it was, which resolves
	// personal-first.
	//
	// @decision(DL-042)
	Tier string `json:"tier,omitempty"`
}

// GitAccess is how far a session's forge credentials reach: the
// session's own repositories, or every repository the credential does.
//
// @decision(DL-043)
type GitAccess string

const (
	GitAccessRepos GitAccess = "repos"
	GitAccessAll   GitAccess = "all"
)

// ParseGitAccess accepts repos, all, or empty for none named.
//
// @decision(DL-043)
func ParseGitAccess(s string) (GitAccess, error) {
	switch a := GitAccess(strings.ToLower(strings.TrimSpace(s))); a {
	case "", GitAccessRepos, GitAccessAll:
		return a, nil
	}
	return "", fmt.Errorf("git access must be %q or %q", GitAccessRepos, GitAccessAll)
}

// CredentialHolder is where a sandbox's forge token is kept.
//
// @decision(DL-034)
type CredentialHolder string

const (
	// CredentialInProxy is the proxy sidecar, out of the agent's reach.
	CredentialInProxy CredentialHolder = "proxy"
	// CredentialInSandbox is the main container, which a launch without
	// the proxy binary still uses locally.
	CredentialInSandbox CredentialHolder = "sandbox"
)

// SessionRequest is what was asked for, kept so attach and the listing
// can show it. It carries no secret.
//
// @decision(DL-003)
type SessionRequest struct {
	Name    string
	Repos   []RepoRef
	Landing string
	Image   string
	Sandbox SandboxConfig
	Egress  string
	// EgressExtraDomains are the caller's own additions to the preset.
	//
	// @decision(DL-068)
	EgressExtraDomains []string
	// GitName and GitEmail are the author of commits made in the
	// sandbox, as the client resolved them; empty falls back to the
	// server's own identity.
	//
	// @decision(DL-044)
	GitName  string
	GitEmail string
	// Model is OpenCode's own provider/model identifier. The server
	// resolves it before the row is written, so an empty one here means
	// the caller named none AND had no stored default (DL-014).
	//
	// @decision(DL-014)
	//
	// It rides in the request rather than in a column of its own: the
	// request is persisted whole, so a column beside it would be a
	// second copy of the same fact.
	Model string
	// GitAccess is what the launch asked for, resolved before the row is
	// written. GitAccessPreferred says it came from a saved preference
	// (the CLI's config or the caller's default), so a platform that
	// cannot have all falls back to repos rather than failing the launch.
	//
	// @decision(DL-043)
	GitAccess          GitAccess
	GitAccessPreferred bool
	// CredentialPreference names, per configured platform, the tier a
	// launch prefers: "org" for the deployment's shared credential. A
	// platform not named is personal-first. The browser sends none.
	//
	// @decision(DL-042)
	CredentialPreference map[string]string `json:",omitempty"`
	// Routed is which routed credentials the launch asks for.
	//
	// @decision(DL-075)
	Routed RoutedRequest `json:",omitzero"`
	// Addons and Apt make Image a variant of the base: curated add-ons
	// in canonical `name@version` form, and extra Debian packages,
	// resolved before the row is written.
	//
	// @decision(DL-033)
	Addons []string
	Apt    []string
	// Env are the session's own environment variables, the deployment's
	// base included, stored in the clear: they are not secrets.
	//
	// @decision(DL-096)
	Env map[string]string `json:",omitempty"`
	// Sync records the local branch and commit a synced session started
	// from; Bundle is passed to the launch in memory and never stored in
	// the database.
	//
	// @decision(DL-027)
	Sync *SessionSync `json:",omitempty"`
}

// SessionSync is the local branch and base commit a synced session was
// seeded from. Bundle is excluded from JSON so it is never persisted in
// the session table.
//
// @decision(DL-027)
type SessionSync struct {
	Branch     string `json:"branch"`
	BaseCommit string `json:"base_commit"`
	Bundle     []byte `json:"-"`
}

// RepoRef names a repository on a configured platform.
type RepoRef struct {
	Platform string
	Owner    string
	Name     string
}

// SandboxConfig is the resource shape requested for the sandbox.
type SandboxConfig struct {
	CPU            float64
	CPULimit       float64
	MemoryMiB      int
	MemoryLimitMiB int
	Timeout        time.Duration
	IdleTimeout    time.Duration
	Regions        []string
	Cloud          string
	Workdir        string
	InboundCIDRs   []string
	Persist        bool
}

// SessionFilter narrows a listing. Zero means everything in the org.
type SessionFilter struct {
	// States, when set, keeps only these states.
	States []SessionState
	// OwnerID, when set, keeps only this owner's sessions.
	OwnerID string
	// Name, when set, keeps only sessions of exactly this name, which a
	// stopped session shares with later ones.
	//
	// @decision(DL-087)
	Name string
	// Before, when set, keeps only sessions with an ID below it: the
	// page after the one that ended with that ID. Count ignores it.
	//
	// @decision(DL-012)
	Before string
	// Limit, when positive, returns at most this many, newest first.
	// Count ignores it.
	//
	// @decision(DL-012)
	Limit int
}

func (f SessionFilter) matches(s Session) bool {
	if f.OwnerID != "" && s.OwnerID != f.OwnerID {
		return false
	}
	if f.Name != "" && s.Name != f.Name {
		return false
	}
	if f.Before != "" && s.ID >= f.Before {
		return false
	}
	if len(f.States) == 0 {
		return true
	}
	for _, st := range f.States {
		if s.State == st {
			return true
		}
	}
	return false
}

// Mint is one credential mint against a session.
//
// @decision(DL-003)
type Mint struct {
	ID        string
	OrgID     string
	SessionID string
	UserID    string
	Port      int
	ExpiresAt time.Time
	CreatedAt time.Time
}

// NewID returns a fresh ULID. IDs sort by creation time.
//
// @decision(DL-003)
func NewID(now time.Time) string {
	return ulid.MustNew(ulid.Timestamp(now), rand.Reader).String()
}

// ValidID reports whether s is a well-formed ULID.
func ValidID(s string) bool {
	_, err := ulid.ParseStrict(s)
	return err == nil
}

// UserStore holds the people who may use a hosted server. Local mode has
// exactly one, fixed at startup.
//
// @decision(DL-003)
type UserStore interface {
	Create(ctx context.Context, u User) error
	Get(ctx context.Context, orgID, id string) (User, error)
	// ByEmail resolves the identity an OAuth login proves. E-mail is
	// compared case-insensitively.
	ByEmail(ctx context.Context, orgID, email string) (User, error)
	List(ctx context.Context, orgID string) ([]User, error)
	Update(ctx context.Context, u User) error
	Delete(ctx context.Context, orgID, id string) error
}

// TokenStore holds the API tokens the device flow issues. Only the hash
// is stored, so a database dump cannot be replayed.
//
// @decision(DL-006)
type TokenStore interface {
	Create(ctx context.Context, t APIToken) error
	// ByHash is the authenticator's lookup and crosses orgs: a bearer
	// token names no org until it is resolved.
	ByHash(ctx context.Context, hash []byte) (APIToken, error)
	// ByID resolves one token within an org, for the revoke endpoint.
	//
	// @decision(DL-006)
	ByID(ctx context.Context, orgID, id string) (APIToken, error)
	ListByUser(ctx context.Context, orgID, userID string) ([]APIToken, error)
	// Touch records use, at most once a minute, so every request does
	// not write.
	Touch(ctx context.Context, orgID, id string, at time.Time) error
	Revoke(ctx context.Context, orgID, id string, at time.Time) error
}

// DeviceCodeStore holds device-flow codes while a login is in progress.
//
// @decision(DL-006)
type DeviceCodeStore interface {
	Create(ctx context.Context, d DeviceCode) error
	// ByHash and ByUserCode cross orgs: neither the polling CLI nor the
	// approving user names one.
	ByHash(ctx context.Context, hash []byte) (DeviceCode, error)
	ByUserCode(ctx context.Context, userCode string) (DeviceCode, error)
	Update(ctx context.Context, d DeviceCode) error
	Delete(ctx context.Context, id string) error
	// DeleteExpired reaps codes past their expiry and reports how many
	// went.
	DeleteExpired(ctx context.Context, before time.Time) (int, error)
}

// CredentialStore holds per-user platform credentials, encrypted. The
// store never sees plaintext.
//
// @decision(DL-008)
type CredentialStore interface {
	Put(ctx context.Context, c UserCredential) error
	Get(ctx context.Context, orgID, userID, platform string) (UserCredential, error)
	ListByUser(ctx context.Context, orgID, userID string) ([]UserCredential, error)
	Delete(ctx context.Context, orgID, userID, platform string) error
	// Lock serialises changes to one user's credential across replicas
	// until the transaction it is called in ends. Outside a transaction
	// it holds nothing.
	//
	// @decision(DL-015)
	Lock(ctx context.Context, orgID, userID, platform string) error
}

// AllowlistStore holds the addresses that may log in. It is a table
// rather than a file reloaded on SIGHUP (DL-007): a signal reaches one
// replica, a table is read by all of them.
//
// E-mail is the key, normalised to lower case on the way in, so a
// caller may pass whatever case a person typed.
//
// @decision(DL-007)
type AllowlistStore interface {
	List(ctx context.Context, orgID string) ([]AllowlistEntry, error)
	Get(ctx context.Context, orgID, email string) (AllowlistEntry, error)
	// Put inserts or updates one entry. CreatedAt is kept from the
	// existing row, so re-granting admin does not restate when the
	// person was first allowed in.
	Put(ctx context.Context, e AllowlistEntry) error
	Delete(ctx context.Context, orgID, email string) error
	// CountAdmins counts the org's admin entries. Called inside a
	// transaction it also takes an org-scoped advisory lock, which is
	// what makes "this change would leave no admin" a decision rather
	// than a guess: count(*) takes no row locks, so two concurrent
	// demotions would otherwise both see two admins and both proceed.
	// The same lock serialises the first-boot seed across replicas.
	//
	// A transaction that changes the allowlist must therefore call it
	// **first**, before reading the entry it is about to change, so
	// that read is serialised too.
	//
	// Outside a transaction the lock would release immediately and is
	// skipped, exactly as SessionStore.CountLive does.
	CountAdmins(ctx context.Context, orgID string) (int, error)
}

// AllowlistEntry is one address that may log in, and whether it is an
// administrator. There is no ID: the address is the identity, and an
// entry exists for an address whether or not anyone has logged in under
// it yet.
//
// @decision(DL-007)
type AllowlistEntry struct {
	OrgID string
	Email string
	Admin bool
	// CreatedAt is when the address was first allowed, not when the
	// entry was last changed.
	CreatedAt time.Time
}

// User is a person who may use the server.
//
// @decision(DL-003)
type User struct {
	ID          string
	OrgID       string
	Email       string
	Name        string
	GithubLogin string
	Admin       bool
	// GitName and GitEmail author commits made in this user's sessions.
	GitName  string
	GitEmail string
	// DefaultModel is the provider/model reference this user's launches
	// run on when the request names none. Empty leaves the choice to
	// OpenCode.
	//
	// @decision(DL-014)
	DefaultModel string
	// DefaultGitAccess and DefaultJobGitAccess are the git access this
	// user's sessions and jobs get when the launch names none. Empty is
	// repos.
	//
	// @decision(DL-043)
	DefaultGitAccess    GitAccess
	DefaultJobGitAccess GitAccess
	// DefaultJobDuration is the wall clock this user's jobs get when the
	// request names none. Zero is unset, which is the built-in default.
	//
	// @decision(DL-049)
	DefaultJobDuration time.Duration
	// Quota overrides the org's defaults for this user, field by field.
	// An empty quota asserts nothing and leaves every default in place.
	//
	// @decision(DL-009)
	Quota       Quota
	LastLoginAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// APIToken is one issued CLI token. The token itself is never stored.
//
// @decision(DL-006)
type APIToken struct {
	ID     string
	OrgID  string
	UserID string
	// Hash is sha256 of the token text.
	Hash       []byte
	Name       string
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Usable reports whether the token may authenticate at t.
func (t APIToken) Usable(at time.Time) bool {
	if t.RevokedAt != nil {
		return false
	}
	return t.ExpiresAt == nil || t.ExpiresAt.After(at)
}

// DeviceCodeStatus is where a device-flow login has got to.
//
// @decision(DL-006)
type DeviceCodeStatus string

const (
	DevicePending  DeviceCodeStatus = "pending"
	DeviceApproved DeviceCodeStatus = "approved"
	DeviceDenied   DeviceCodeStatus = "denied"
)

// DeviceCode is one device-flow login in progress.
//
// @decision(DL-006)
type DeviceCode struct {
	ID    string
	OrgID string
	// Hash is sha256 of the device code; UserCode is shown to the human.
	Hash     []byte
	UserCode string
	// UserID is empty until someone approves.
	UserID    string
	Status    DeviceCodeStatus
	Name      string
	ExpiresAt time.Time
	// LastPolledAt and IntervalSeconds carry RFC 8628's slow_down.
	LastPolledAt    *time.Time
	IntervalSeconds int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// UserCredential is one platform credential belonging to a user, sealed
// by the caller. The store holds ciphertext and never a plaintext token.
//
// @decision(DL-008)
type UserCredential struct {
	ID         string
	OrgID      string
	UserID     string
	Platform   string
	Ciphertext []byte
	Nonce      []byte
	KeyVersion int
	// VerifiedAs is the account the token proved it owns, for display.
	VerifiedAs string
	// Scopes and ExpiresAt are what the forge said about the token when
	// it was stored, for display; empty and zero when it said nothing.
	//
	// @decision(DL-041)
	Scopes    []string
	ExpiresAt time.Time
	// Route is a routed credential's route, encoded, which its seal
	// binds; nil on every other credential.
	//
	// @decision(DL-073)
	Route     []byte
	CreatedAt time.Time
	UpdatedAt time.Time
}

// OrgCredential is an org's shared credential for one platform, sealed
// as a UserCredential is but under its own AAD. Meta holds what may be
// shown about it (the App id, account and installation), never a secret.
//
// @decision(DL-042)
type OrgCredential struct {
	ID         string
	OrgID      string
	Platform   string
	Ciphertext []byte
	Nonce      []byte
	KeyVersion int
	Meta       []byte
	UpdatedBy  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// OrgCredentialStore holds org credentials, one per org and platform.
// The store never sees plaintext.
//
// @decision(DL-042)
type OrgCredentialStore interface {
	// Put inserts or replaces, keeping the first row's ID and CreatedAt.
	Put(ctx context.Context, c OrgCredential) error
	Get(ctx context.Context, orgID, platform string) (OrgCredential, error)
	// List is every one the org holds, by platform.
	//
	// @decision(DL-074)
	List(ctx context.Context, orgID string) ([]OrgCredential, error)
	// Delete is ErrNotFound when there was nothing to delete.
	Delete(ctx context.Context, orgID, platform string) error
}

// AdminEvent is one change an admin made to the org's configuration. It
// names who, what and when, and never carries a secret.
//
// @decision(DL-042)
type AdminEvent struct {
	ID     string
	OrgID  string
	Actor  string
	Action string
	Detail string
	At     time.Time
}

// AdminEventStore is the org's audit trail of admin changes.
//
// @decision(DL-042)
type AdminEventStore interface {
	Append(ctx context.Context, e AdminEvent) error
	// List returns the org's newest events first, at most limit.
	List(ctx context.Context, orgID string, limit int) ([]AdminEvent, error)
}

// EventSource says who authored an event row: the sandbox's OpenCode
// server, or spov itself marking where its recorder was not listening.
//
// @decision(DL-017)
type EventSource string

const (
	SourceOpenCode EventSource = "opencode"
	SourceSpov     EventSource = "spov"
	// SourceEphemeral is a frame the sandbox sent that OpenCode's own
	// event store does not keep — a permission prompt, above all. A
	// third source rather than SourceSpov: these are the sandbox's
	// words, not spov's.
	//
	// @decision(DL-017)
	SourceEphemeral EventSource = "ephemeral"
)

// Conversation is one OpenCode session inside a spov session's sandbox.
// A sandbox may hold several.
//
// @decision(DL-017)
type Conversation struct {
	ID        string
	OrgID     string
	SessionID string
	// OpenCodeID is the session id OpenCode gave it (ses_…).
	OpenCodeID string
	Title      string
	// Info is OpenCode's Session.Info for it, whole, as the sandbox's
	// listing last gave it, or empty when it never did. A transcript
	// resume imports it.
	//
	// @decision(DL-089)
	Info []byte
	// ParentOpenCodeID is the OpenCode id of the conversation that
	// started this one as a subagent, or empty for a root, including one
	// whose parent was never learned. An upsert never clears it.
	//
	// @decision(DL-110)
	ParentOpenCodeID string
	// SeqBase and SeqAfter map a conversation imported by a transcript
	// resume onto its stored sequence: a durable frame at seq is stored
	// at SeqBase + seq - SeqAfter, and one at or before SeqAfter is the
	// import's own echo. Both are 0 for a conversation never imported.
	//
	// @decision(DL-089)
	SeqBase   int64
	SeqAfter  int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsRoot reports whether c is a conversation no other one started.
//
// @decision(DL-110)
func (c Conversation) IsRoot() bool { return c.ParentOpenCodeID == "" }

// Primary is the person's conversation among a session's: the oldest
// root, else the oldest, by CreatedAt with ListConversations' order
// breaking a tie, so a list in another order still finds it. ok is false
// only when there are none.
//
// @decision(DL-110) @decision(DL-109) @decision(DL-025)
func Primary(convs []Conversation) (Conversation, bool) {
	var best Conversation
	found := false
	for _, roots := range []bool{true, false} {
		for _, c := range convs {
			if roots && !c.IsRoot() {
				continue
			}
			if !found || c.CreatedAt.Before(best.CreatedAt) {
				best, found = c, true
			}
		}
		if found {
			return best, true
		}
	}
	return Conversation{}, false
}

// Message is one thing said in a conversation: the reply, not the parts
// it arrived in, and never a reasoning part.
//
// @decision(DL-017)
type Message struct {
	ID             string
	OrgID          string
	SessionID      string
	ConversationID string
	// OpenCodeID is the message id OpenCode gave it (msg_…).
	OpenCodeID string
	// Role is who spoke: user or assistant.
	Role string
	Text string
	// At is when OpenCode says the message was created; zero when it
	// carried no time.
	At time.Time
	// Raw is the message as received, for re-projection — the same
	// reason Event.Raw exists, and needed here so gate 3 can read tool
	// parts the current projection does not keep.
	//
	// @decision(DL-017) @decision(DL-050)
	Raw       []byte
	CreatedAt time.Time
}

// Event is one durable frame of a conversation's log, or a marker spov
// wrote into it.
//
// @decision(DL-017)
type Event struct {
	ID        string
	OrgID     string
	SessionID string
	// ConversationID is empty for a spov marker, which belongs to the
	// session rather than to one of its conversations.
	ConversationID string
	Source         EventSource
	// Seq is OpenCode's durable sequence within the conversation; 0
	// for a spov marker and for an ephemeral frame, neither of which
	// has one. It is stored as NULL rather than 0, so a seq-less row
	// is absent from the dedup key rather than colliding with every
	// other seq-less row of its conversation.
	//
	// @decision(DL-017)
	Seq int64
	// Pos is assigned by the store on insert and is strictly increasing
	// across the store, so a reader can hold it as a cursor.
	Pos  int64
	Name string
	Text string
	At   time.Time
	// Raw is the frame as received, for re-projection.
	Raw       []byte
	CreatedAt time.Time
}

// ConversationStore holds conversations, their messages and their
// events. Writes are idempotent on OpenCode's own identifiers, so a
// caller may repeat a read of the sandbox without remembering what it
// wrote last time.
//
// @decision(DL-017)
type ConversationStore interface {
	// UpsertConversation keys on (SessionID, OpenCodeID). An existing
	// row keeps its ID and CreatedAt; its title and its info change only
	// when the new one is non-empty. The stored row is returned. A session that
	// does not exist in the org is ErrNotFound.
	UpsertConversation(ctx context.Context, c Conversation) (Conversation, error)
	ListConversations(ctx context.Context, orgID, sessionID string) ([]Conversation, error)
	// PutMessages inserts or updates by (ConversationID, OpenCodeID) and
	// reports how many rows changed. A message for a conversation that
	// does not exist, or that names a different org or session than its
	// conversation, is ErrNotFound. A batch lands whole or not at all.
	PutMessages(ctx context.Context, msgs []Message) (int, error)
	// ListMessages orders by At, then ID; a message with no time sorts
	// first.
	ListMessages(ctx context.Context, orgID, sessionID string) ([]Message, error)
	// LastMessages answers the list page in one query: each session's
	// latest message, keyed by session ID. A session with none is
	// absent from the map.
	LastMessages(ctx context.Context, orgID string, sessionIDs []string) (map[string]Message, error)
	// PutEvents inserts, ignoring an OpenCode frame whose
	// (ConversationID, Seq) is already stored, and returns the rows it
	// wrote with Pos filled in. A duplicate ID is ErrConflict; an event
	// that fails Validate is ErrInvalidEvent; one naming a conversation
	// that does not exist, or a different org or session than it, is
	// ErrNotFound. A batch lands whole or not at all, and on any error
	// nothing is returned.
	PutEvents(ctx context.Context, evs []Event) ([]Event, error)
	// ListEvents returns the session's events with Pos greater than
	// after, in Pos order, at most limit of them; a non-positive limit
	// means all.
	ListEvents(ctx context.Context, orgID, sessionID string, after int64, limit int) ([]Event, error)
	// LastEvent returns the session's newest event by Pos, or
	// ErrNotFound when it has none. It answers "what has the recorder
	// said most recently" without reading the log.
	//
	// @decision(DL-018)
	LastEvent(ctx context.Context, orgID, sessionID string) (Event, error)
	// LastSeqs returns the highest stored durable sequence of each of
	// the session's conversations, keyed by conversation ID. A
	// conversation with no durable frame is absent.
	//
	// @decision(DL-017)
	LastSeqs(ctx context.Context, orgID, sessionID string) (map[string]int64, error)
	// SetSeqOffset records how an imported conversation's durable log
	// continues its stored one (Conversation.SeqBase and SeqAfter).
	//
	// @decision(DL-089)
	SetSeqOffset(ctx context.Context, orgID, conversationID string, base, after int64) error
	// LastEventExcept is LastEvent passing over the named events, so a
	// marker written after real activity cannot hide it.
	//
	// @decision(DL-021)
	LastEventExcept(ctx context.Context, orgID, sessionID string, except []string) (Event, error)
	// FindEvent returns the session's newest event with this name whose
	// raw frame contains the bytes of contains, or ErrNotFound. The
	// caller re-reads the frame: a match on bytes is a candidate, not a
	// parse.
	//
	// @decision(DL-052)
	FindEvent(ctx context.Context, orgID, sessionID, name string, contains []byte) (Event, error)
}

// ErrInvalidEvent is an Event that could not be a row: an OpenCode
// frame with no conversation or sequence, one claiming a name in spov's
// own namespace, or a source nobody defined.
//
// @decision(DL-017) @decision(DL-058)
var ErrInvalidEvent = errors.New("store: invalid event")

// SpovNamespace prefixes the events spov writes about a session's
// recording rather than about the session. Only a SourceSpov event may
// carry it.
//
// @decision(DL-058) @decision(DL-017)
const SpovNamespace = "spov."

// Validate reports whether the event may be stored.
func (e Event) Validate() error {
	switch e.Source {
	case SourceOpenCode:
		if e.ConversationID == "" || e.Seq <= 0 {
			return fmt.Errorf("%w: an opencode event needs a conversation and a positive seq", ErrInvalidEvent)
		}
		// An event's name is whatever the frame called itself, and the
		// sandbox is the untrusted side. SpovNamespace is reserved so
		// that a name in it always means spov said it — a page can then
		// draw a marker as spov's own voice without asking twice.
		//
		// @decision(DL-058)
		if strings.HasPrefix(e.Name, SpovNamespace) {
			return fmt.Errorf("%w: %q is reserved for spov's own events", ErrInvalidEvent, SpovNamespace)
		}
	case SourceEphemeral:
		// No seq: these carry no durable marker and therefore no
		// position, which is exactly why they need a source of their
		// own. A conversation is still required — an ephemeral frame
		// belongs to the session it was sent about.
		//
		// @decision(DL-017)
		if e.ConversationID == "" {
			return fmt.Errorf("%w: an ephemeral event needs a conversation", ErrInvalidEvent)
		}
		if e.Seq != 0 {
			return fmt.Errorf("%w: an ephemeral event has no sequence, got %d", ErrInvalidEvent, e.Seq)
		}
		if strings.HasPrefix(e.Name, SpovNamespace) {
			return fmt.Errorf("%w: %q is reserved for spov's own events", ErrInvalidEvent, SpovNamespace)
		}
	case SourceSpov:
	default:
		return fmt.Errorf("%w: unknown source %q", ErrInvalidEvent, e.Source)
	}
	if e.ID == "" || e.Name == "" {
		return fmt.Errorf("%w: id and name are required", ErrInvalidEvent)
	}
	if e.At.IsZero() {
		return fmt.Errorf("%w: at is required", ErrInvalidEvent)
	}
	return nil
}

// JobState is where a job is in its life.
//
// The machine is `queued → provisioning → running → delivering →
// succeeded | failed | blocked | cancelled`, plus `steered`. `queued` is
// produced by job creation, `cancelled` by cancellation and `delivering`
// by delivery.
//
// @decision(DL-045)
type JobState string

const (
	JobQueued       JobState = "queued"
	JobProvisioning JobState = "provisioning"
	JobRunning      JobState = "running"
	JobDelivering   JobState = "delivering"
	JobSucceeded    JobState = "succeeded"
	JobFailed       JobState = "failed"
	// JobBlocked is a job parked on something the server will not
	// decide for it: a permission nobody answered, or a bound it hit.
	// Recoverable by a human, and deliberately distinct from JobFailed,
	// which is not.
	JobBlocked JobState = "blocked"
	// JobSteered is a human having taken over. A suspension rather than
	// a terminal state: the loop stops continuing, the sandbox stays
	// up, and leaving is explicit — resumed, cancelled, or the agent
	// declaring the goal met (DL-048). Never a timeout or a loop that
	// merely gave out.
	JobSteered   JobState = "steered"
	JobCancelled JobState = "cancelled"
)

// Terminal reports whether a job in this state is finished. A blocked
// or steered job is not: both are waiting on a human and both can move
// again.
//
// @decision(DL-045)
func (s JobState) Terminal() bool {
	switch s {
	case JobSucceeded, JobFailed, JobCancelled:
		return true
	}
	return false
}

// Live reports whether the job still holds, or is about to hold, a
// sandbox. A blocked or steered job does: that is what makes it
// attachable, and what makes it cost money while it waits.
//
// @decision(DL-045)
func (s JobState) Live() bool {
	return !s.Terminal() && s != JobQueued
}

// Counts reports whether the job counts against its owner's concurrency
// quota. This is deliberately not Live: a queued job holds no sandbox
// yet but is committed to getting one, and leaving it uncounted would
// let a caller queue a thousand jobs against a limit of three and have
// the supervisor honour every one of them.
//
// @decision(DL-045)
func (s JobState) Counts() bool { return !s.Terminal() }

// JobCounts is Counts as a list, for a query that has to name the
// states rather than call a method. Kept beside Counts so the two
// cannot drift; a test asserts they agree.
//
// @decision(DL-045)
var JobCounts = []JobState{
	JobQueued, JobProvisioning, JobRunning, JobBlocked, JobSteered, JobDelivering,
}

// jobTransitions is the machine. A state's entry lists what may follow
// it; a terminal state has none.
//
// Written as data rather than as a switch because it is the one thing
// the supervisor and the job surface all read: DL-046 writes steered and
// blocked, DL-050 writes the verdict, DL-049 delivers a job at its wall
// clock. A table can be asserted against in one test.
//
// @decision(DL-045) @decision(DL-048) @decision(DL-049)
var jobTransitions = map[JobState][]JobState{
	JobQueued:       {JobProvisioning, JobCancelled, JobFailed},
	JobProvisioning: {JobRunning, JobFailed, JobCancelled, JobBlocked},
	// A run may finish, be taken over, park, or be stopped. It reaches
	// delivering on every path that produced something to deliver,
	// which DL-050 makes include the failure paths.
	JobRunning: {JobDelivering, JobBlocked, JobSteered, JobFailed, JobCancelled},
	// Delivery is the last thing a job does, and it is what classifies
	// the outcome.
	JobDelivering: {JobSucceeded, JobFailed, JobCancelled},
	// Both suspensions return to running when a human releases them,
	// and both can be given up on instead.
	//
	// A steered job also reaches delivering: the agent declaring the
	// goal met (DL-048), and its sandbox dying with the job never
	// released (DL-045). A blocked job reaches it only at its wall
	// clock (DL-049): the question will never be answered, and what was
	// done before it is worth collecting.
	JobBlocked: {JobRunning, JobDelivering, JobCancelled, JobFailed},
	JobSteered: {JobRunning, JobDelivering, JobCancelled, JobFailed},
}

// ErrInvalidTransition is a move the machine does not allow.
//
// @decision(DL-045)
var ErrInvalidTransition = errors.New("store: invalid job transition")

// CanTransition reports whether a job may move from one state to
// another. A move to the same state is allowed: the supervisor writes a
// job's progress as well as its state, and re-recording `running` while
// still running is not a transition.
//
// @decision(DL-045)
func CanTransition(from, to JobState) bool {
	if from == to {
		return true
	}
	return slices.Contains(jobTransitions[from], to)
}

// JobKind is what a job is meant to produce. It decides what gate 2
// verifies (DL-050) and what the permission allowlist permits (DL-040).
//
// @decision(DL-045)
type JobKind string

const (
	// JobKindPR delivers a pull request.
	JobKindPR JobKind = "pr"
	// JobKindReport delivers Markdown reports read out of the sandbox.
	JobKindReport JobKind = "report"
)

// Job is one piece of supervised work: a goal, the session running it,
// and where it has got to.
//
// The server owns this row and its states; a plugin inside the sandbox
// owns continuation. The plugin may fail without the job failing, so
// nothing on the verdict path may depend on a signal only the plugin
// produces.
//
// @decision(DL-045)
type Job struct {
	ID      string
	OrgID   string
	OwnerID string
	// SessionID is the ordinary session row the job runs in, so the
	// transcript, the sessions list and the web page work on a job for
	// free.
	SessionID string
	Kind      JobKind
	State     JobState
	// Goal is the canonical task text. The row owns it: the session's
	// first prompt is a rendering of this, and the plugin's in-memory
	// copy is derived and may be stale after a reload. Nothing reads
	// the goal back out of the transcript.
	Goal string
	// CheckCommand is the repo's own check, run in the sandbox when the
	// run ends so gate 1 of the verdict has an exit status rather than
	// the agent's account of one. Empty leaves gate 1 unverified, which
	// DL-050 classifies on the other gates rather than failing.
	//
	// It lives on the row because the launch happens later and possibly
	// on another replica: a job is created `queued` and provisioned by
	// whichever supervisor picks it up.
	//
	// @decision(DL-050)
	CheckCommand string
	// Model is OpenCode's own provider/model identifier, resolved from
	// the request and the owner's stored default before the row was
	// written (DL-014). Empty means neither named one.
	//
	// A column of its own, unlike a session's: a redelivery reads it
	// when provisioning lands on another replica, and a verdict is only
	// comparable across runs if the row says what ran them.
	//
	// @decision(DL-014)
	Model string
	// CheckProposed says the check came from spov's preparation pass
	// rather than from the caller.
	//
	// The distinction cannot be recovered from the command itself, and
	// the two are not equal evidence: a gate that passed on the
	// caller's own check says something about the repository, while
	// one that passed on a check spov chose says something about spov's
	// reading of the request. The verdict says which.
	//
	// @decision(DL-047)
	CheckProposed bool
	// Assessment is the preparation pass's reading of the goal, or
	// empty if no pass ran or it could not say anything useful.
	//
	// It is DERIVED and the goal is canonical: re-running the pass must
	// be possible without rewriting what the user asked for, so this is
	// a separate column rather than an edit to Goal. It is stored
	// because provisioning may happen on another replica, and because a
	// redelivery must not pay for a second call.
	//
	// @decision(DL-047)
	Assessment string
	// Error is the terminal failure, redacted of anything secret. It is
	// set on failure and also on a park, where it says what is being
	// waited on.
	Error string
	// Unfinished is what the agent declared it left undone when it
	// called the completion tool, written at delivery. Non-empty makes a
	// success partial. The deliverables themselves are rows of their own
	// (JobStore.Deliverables).
	//
	// @decision(DL-050) @decision(DL-051)
	Unfinished string
	// Verdict is what the gates found, one line per gate. It is kept
	// apart from Error because a SUCCEEDED job has one too: the
	// reasoning behind a verdict is worth showing whichever way it
	// went, and a client that renders Error as a failure would
	// otherwise show every success as one.
	//
	// @decision(DL-050)
	Verdict string
	// Attempt counts infrastructure retries, not iterations: a sandbox
	// timeout or a Modal error is a retry, and never burns one of the
	// plugin's continuations.
	Attempt int
	// Cursor is the highest event Pos the supervisor has already read
	// for this job. It is durable because the supervisor's conclusions
	// are not idempotent: re-reading an old `permission.asked` or a
	// human's inbox item would re-derive `blocked` or `steered` and
	// silently undo the explicit act that released the job — a legal
	// transition, so no validation catches it.
	//
	// @decision(DL-046)
	Cursor    int64
	CreatedAt time.Time
	UpdatedAt time.Time
	// StartedAt is when the job first reached running, and StoppedAt
	// when it reached a terminal state. Both nil until then.
	StartedAt *time.Time
	StoppedAt *time.Time
	// MaxDuration is how long the job may run unattended, fixed at
	// create. Zero is a job created before the clock existed, which has
	// none. The clock counts running and blocked across the job:
	// ClockUsed is the time of its finished stretches and ClockStartedAt
	// the start of the current one, nil while it is stopped. Only
	// StampJobTimes writes either.
	//
	// @decision(DL-049)
	MaxDuration    time.Duration
	ClockStartedAt *time.Time
	ClockUsed      time.Duration
}

// ClockCounts reports whether a job in this state uses its wall clock.
//
// @decision(DL-049)
func (s JobState) ClockCounts() bool { return s == JobRunning || s == JobBlocked }

// ClockElapsed is how much of its wall clock the job has used at now.
//
// @decision(DL-049)
func (j Job) ClockElapsed(now time.Time) time.Duration {
	used := j.ClockUsed
	if j.ClockStartedAt != nil && now.After(*j.ClockStartedAt) {
		used += now.Sub(*j.ClockStartedAt)
	}
	return used
}

// ErrInvalidJob is a job that may not be stored as given.
//
// @decision(DL-045)
var ErrInvalidJob = errors.New("store: invalid job")

// Validate reports whether the job may be stored.
//
// @decision(DL-045)
func (j Job) Validate() error {
	if j.ID == "" || j.OrgID == "" || j.SessionID == "" {
		return fmt.Errorf("%w: id, org and session are required", ErrInvalidJob)
	}
	if strings.TrimSpace(j.Goal) == "" {
		return fmt.Errorf("%w: a job with no goal has nothing to supervise", ErrInvalidJob)
	}
	switch j.Kind {
	case JobKindPR, JobKindReport:
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidJob, j.Kind)
	}
	if _, ok := jobStates[j.State]; !ok {
		return fmt.Errorf("%w: unknown state %q", ErrInvalidJob, j.State)
	}
	if j.Attempt < 0 {
		return fmt.Errorf("%w: attempt %d is negative", ErrInvalidJob, j.Attempt)
	}
	if j.MaxDuration < 0 {
		return fmt.Errorf("%w: max duration %s is negative", ErrInvalidJob, j.MaxDuration)
	}
	return nil
}

// jobStates is every state the machine has, terminal ones included —
// jobTransitions alone cannot serve, since a terminal state has no
// entry there.
//
// @decision(DL-045)
var jobStates = map[JobState]struct{}{
	JobQueued: {}, JobProvisioning: {}, JobRunning: {}, JobDelivering: {},
	JobSucceeded: {}, JobFailed: {}, JobBlocked: {}, JobSteered: {},
	JobCancelled: {},
}

// JobStates is every state the machine has, in a stable order.
//
// It exists so a rule that must decide something for EVERY state can be
// checked against the machine rather than against a list someone has to
// remember to extend — the same reason internal/jobs keeps signalLast.
// DL-052 is the first such rule: a state it does not name becomes
// steerable by omission, which is the direction that matters.
//
// @decision(DL-045) @decision(DL-052)
func JobStates() []JobState {
	out := make([]JobState, 0, len(jobStates))
	for s := range jobStates {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// ValidJobState reports whether s names a state the machine has. The
// API filters on state, and a typo must be refused rather than
// silently matching nothing — an empty list reads as "no such jobs"
// rather than "no such state".
//
// @decision(DL-045)
func ValidJobState(s JobState) bool {
	_, ok := jobStates[s]
	return ok
}

// JobFilter narrows a job listing.
//
// @decision(DL-045)
type JobFilter struct {
	// States keeps only jobs in one of these; empty keeps all.
	States []JobState
	// SessionID keeps only the job of one session; empty keeps all.
	SessionID string
	// OwnerID keeps only this owner's jobs; empty keeps all. The same
	// shape SessionFilter has, so a non-admin listing is a narrower
	// query rather than the whole org's history filtered in Go.
	//
	// @decision(DL-045)
	OwnerID string
	// Before, when set, keeps only jobs with an ID below it. Count
	// ignores it.
	//
	// @decision(DL-012)
	Before string
	// Limit, when positive, returns at most this many, newest first.
	// Count ignores it.
	//
	// @decision(DL-012)
	Limit int
}

func (f JobFilter) match(j Job) bool {
	if f.OwnerID != "" && j.OwnerID != f.OwnerID {
		return false
	}
	if f.Before != "" && j.ID >= f.Before {
		return false
	}
	if f.SessionID != "" && j.SessionID != f.SessionID {
		return false
	}
	if len(f.States) > 0 && !slices.Contains(f.States, j.State) {
		return false
	}
	return true
}

// JobStore holds jobs. Every query is scoped to an org, except the two
// the supervisor needs to work from what the provider reports.
//
// Only the supervisor writes a job's state, and it runs behind DL-004's
// leader lock — the same one the recorder runs behind. So the concurrent
// writers are one component's own replicas rather than two different
// components, and the store needs no version column to arbitrate
// between them: the lock already has.
//
// @decision(DL-045) @decision(DL-003)
type JobStore interface {
	Create(ctx context.Context, j Job) error
	Get(ctx context.Context, orgID, id string) (Job, error)
	// GetForUpdate is Get that, inside Tx, holds the row until the
	// transaction ends, so a decision read from the row cannot be undone
	// by another writer before this one's Update. Outside Tx it is Get.
	//
	// @decision(DL-049)
	GetForUpdate(ctx context.Context, orgID, id string) (Job, error)
	// List returns the matching jobs newest first, the page f.Before
	// and f.Limit name.
	//
	// @decision(DL-012)
	List(ctx context.Context, orgID string, f JobFilter) ([]Job, error)
	// Count counts the jobs List would return without Before and Limit.
	//
	// @decision(DL-012)
	Count(ctx context.Context, orgID string, f JobFilter) (int, error)
	// Update replaces the stored job. UpdatedAt is set by the store,
	// and the move must be one the machine allows: an invalid one is
	// ErrInvalidTransition and changes nothing.
	Update(ctx context.Context, j Job) error
	// BySession resolves the job of a session. It is how an attached
	// CLI finds the loop's controls for the session it is in.
	BySession(ctx context.Context, orgID, sessionID string) (Job, error)
	// BySessions answers the session list page in one query: the job of
	// each session, keyed by session ID. A session with no job is
	// absent from the map.
	//
	// It exists for the same reason LastMessages does — a listing must
	// cost one query rather than one per row — and is the only way the
	// list decorates rows with their jobs.
	//
	// @decision(DL-060)
	BySessions(ctx context.Context, orgID string, sessionIDs []string) (map[string]Job, error)
	// CountLive counts a user's unfinished jobs — every state in
	// JobCounts, `queued` included. Called inside the create
	// transaction it is what makes the job quota a limit rather than a
	// suggestion, exactly as SessionStore.CountLive does.
	//
	// @decision(DL-045)
	CountLive(ctx context.Context, orgID, ownerID string) (int, error)
	// ListLive lists every unfinished job across orgs. It is the
	// supervisor's own query, for finding what a restarted server was
	// running, and the only one that crosses orgs — a restarted replica
	// has no principal to scope by. Request handlers never call it.
	ListLive(ctx context.Context) ([]Job, error)

	// PutDeliverable inserts a member of a job's inventory, or updates
	// the stored one with the same key unless that one is verified: a
	// later write never makes a member worse. It reports whether it
	// wrote. JobID and Key identify the member; the store sets the times.
	//
	// @decision(DL-051)
	PutDeliverable(ctx context.Context, orgID string, d Deliverable) (bool, error)
	// Deliverables lists the inventories of several jobs in one query,
	// keyed by job ID and ordered by key. A job with none is absent.
	//
	// @decision(DL-051)
	Deliverables(ctx context.Context, orgID string, jobIDs []string) (map[string][]Deliverable, error)
	// PutJobFile stores a collected file once. A file already stored at
	// that path is kept and the call reports false.
	//
	// @decision(DL-051)
	PutJobFile(ctx context.Context, orgID string, f JobFile) (bool, error)
	// JobFile reads one collected file, body included. ErrNotFound when
	// none is stored at that path.
	JobFile(ctx context.Context, orgID, jobID, path string) (JobFile, error)
	// JobFiles lists a job's collected files without their bodies.
	JobFiles(ctx context.Context, orgID, jobID string) ([]JobFile, error)
}

// DeliverableKind is what a member of a job's inventory is.
//
// @decision(DL-051)
type DeliverableKind string

const (
	DeliverablePR     DeliverableKind = "pr"
	DeliverableReport DeliverableKind = "report"
)

// DeliverableStatus is what spov found about one member.
//
// @decision(DL-050)
type DeliverableStatus string

const (
	// DeliverableVerified: the forge answered with an open pull request
	// from the branch, or the report was collected.
	DeliverableVerified DeliverableStatus = "verified"
	// DeliverableMissing: the forge answered that no open pull request
	// comes from the branch.
	DeliverableMissing DeliverableStatus = "missing"
	// DeliverableUnverified: spov could not ask, or could not read.
	DeliverableUnverified DeliverableStatus = "unverified"
	// DeliverableOverLimit: not collected, because of the size limits.
	DeliverableOverLimit DeliverableStatus = "over_limit"
	// DeliverableWithdrawn: the agent took the pull request back after a
	// person wrote to the job, so it is not part of the set (DL-051).
	DeliverableWithdrawn DeliverableStatus = "withdrawn"
)

// DeliverableStatuses is every status, for a guard that must decide each.
var DeliverableStatuses = []DeliverableStatus{
	DeliverableVerified, DeliverableMissing, DeliverableUnverified, DeliverableOverLimit, DeliverableWithdrawn,
}

// Deliverable is one member of a job's inventory: a pull request, or a
// report the job left in its out directory.
//
// @decision(DL-051)
type Deliverable struct {
	JobID  string
	Key    string
	Kind   DeliverableKind
	Status DeliverableStatus
	// Detail is one line on the status, for a person.
	Detail string
	// Platform, Owner, Name and Branch say where a pull request is from;
	// Number and URL are filled in once the forge has answered.
	Platform string
	// Forge is the platform's kind, which decides whether the change is
	// called a pull request or a merge request. A write with none keeps
	// the stored one.
	//
	// @decision(DL-051)
	Forge  string
	Owner  string
	Name   string
	Branch string
	Number int
	URL    string
	// Title is the pull request's title, or a report's first heading.
	Title string
	// Path and Size are a report's, relative to the out directory.
	Path string
	Size int64
	// ImagesLost is a verified report some of whose linked images could
	// not be collected, which gate 2 reads as partial.
	//
	// @decision(DL-051)
	ImagesLost bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// PullRequestKey is a pull request member's key within its job.
//
// @decision(DL-051)
func PullRequestKey(platform, owner, name, branch string) string {
	return "pr:" + platform + ":" + owner + "/" + name + ":" + branch
}

// ReportKey is a report member's key within its job.
//
// @decision(DL-051)
func ReportKey(path string) string { return "report:" + path }

// JobFile is a file delivery collected from a job's out directory: a
// report, or an image one links.
//
// @decision(DL-051) @decision(DL-063)
type JobFile struct {
	JobID string
	// Path is relative to the out directory.
	Path        string
	ContentType string
	Size        int64
	// Body is empty when listed by JobFiles.
	Body      []byte
	CreatedAt time.Time
}

// StampJobTimes sets StartedAt and StoppedAt on next from the move
// being made, so a caller cannot forget to and the two stores cannot
// disagree about when a job began or ended. Both are write-once: a job
// that returns to running from blocked keeps the time it first
// started. Both stores call it, so the rule has one home.
//
// The wall clock is a ledger: entering a counting state starts a
// stretch, leaving one adds the stretch to ClockUsed, and a move between
// running and blocked leaves both alone.
//
// @decision(DL-045) @decision(DL-049)
func StampJobTimes(next *Job, cur Job, now time.Time) {
	next.StartedAt, next.StoppedAt = cur.StartedAt, cur.StoppedAt
	next.ClockStartedAt, next.ClockUsed = cur.ClockStartedAt, cur.ClockUsed
	if next.StartedAt == nil && next.State == JobRunning {
		next.StartedAt = &now
	}
	switch counts, counted := next.State.ClockCounts(), cur.State.ClockCounts(); {
	case counts && !counted:
		clock := now
		next.ClockStartedAt = &clock
	case !counts && counted:
		// Whole seconds, which is what the Postgres column holds, so
		// both stores agree.
		next.ClockUsed = cur.ClockElapsed(now).Round(time.Second)
		next.ClockStartedAt = nil
	}
	if next.StoppedAt == nil && next.State.Terminal() {
		next.StoppedAt = &now
	}
}
