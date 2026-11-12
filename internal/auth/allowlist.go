package auth

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jimutt/spov/internal/store"
)

// AllowlistRefresh is how long a replica may go on believing a stale
// allowlist.
//
// @decision(DL-007)
const AllowlistRefresh = 10 * time.Second

// AllowlistSet is the allowlist as the cache holds it: address entries
// keyed by lower-cased e-mail and account entries keyed by GitHub ID,
// each with whether it grants administrator rights.
//
// @decision(DL-007) @decision(DL-128)
type AllowlistSet struct {
	Emails   map[string]bool
	Accounts map[int64]bool
}

// Len is how many entries the set holds.
func (s AllowlistSet) Len() int { return len(s.Emails) + len(s.Accounts) }

// Covers reports whether an entry covers the account githubID whose
// address is email, and whether any entry that covers it is an admin
// entry. A githubID of zero is a user row not yet bound to an account,
// which only an address entry can cover.
//
// @decision(DL-128)
func (s AllowlistSet) Covers(githubID int64, email string) (admin, ok bool) {
	// No entry covers a service account's address, even one stored
	// before such entries were refused.
	//
	// @decision(DL-140)
	if a, found := s.Emails[normalizeEmail(email)]; found && email != "" && !IsServiceAddress(email) {
		admin, ok = a, true
	}
	if a, found := s.Accounts[githubID]; found && githubID != 0 {
		admin, ok = admin || a, true
	}
	return admin, ok
}

// AllowlistSource loads the current allowlist.
//
// @decision(DL-007)
type AllowlistSource func(ctx context.Context) (AllowlistSet, error)

// Allowlist is who may log in: a cached copy of the store's allowlist,
// refreshed on a timer and swapped atomically, consulted on every
// authenticated request.
//
// The zero value allows nobody. Reads are lock-free and do no I/O.
//
// @decision(DL-007)
type Allowlist struct {
	load     AllowlistSource
	interval time.Duration
	entries  atomic.Pointer[AllowlistSet]
	warn     func(string)
	// failing is whether the last read failed, so a persistent outage
	// is two log lines rather than one per tick.
	failing atomic.Bool
}

// ErrEmptyAllowlist is a seed file that parsed but allows nobody. It is
// reported at startup, where it means a deployment nobody can log in to.
var ErrEmptyAllowlist = errors.New("auth: the allowlist is empty")

// NewAllowlist reads the source once and returns the cached list. The
// first read must succeed: the zero value allows nobody, so a server
// that started without one would refuse every login.
//
// @decision(DL-007)
func NewAllowlist(ctx context.Context, load AllowlistSource, interval time.Duration, warn func(string)) (*Allowlist, error) {
	if warn == nil {
		warn = func(string) {}
	}
	if interval <= 0 {
		interval = AllowlistRefresh
	}
	a := &Allowlist{load: load, interval: interval, warn: warn}
	entries, err := load(ctx)
	if err != nil {
		return nil, err
	}
	if entries.Len() == 0 {
		// Not fatal, unlike DL-007's file: an empty table on a running
		// deployment is a state an operator can fix with one API call,
		// where an unreadable file was a mount that needed a redeploy.
		// The seed is what guarantees there is normally an admin.
		warn("allowlist: nobody is on the allowlist, so no one can log in")
	}
	a.entries.Store(&entries)
	return a, nil
}

// StoreAllowlist reads one org's entries. It is the source hosted mode
// runs on.
//
// @decision(DL-007) @decision(DL-128)
func StoreAllowlist(al store.AllowlistStore, orgID string) AllowlistSource {
	return func(ctx context.Context) (AllowlistSet, error) {
		rows, err := al.List(ctx, orgID)
		if err != nil {
			return AllowlistSet{}, err
		}
		set := AllowlistSet{Emails: map[string]bool{}, Accounts: map[int64]bool{}}
		for _, e := range rows {
			switch {
			case e.GithubID != 0:
				set.Accounts[e.GithubID] = e.Admin
			case e.Email != "":
				set.Emails[normalizeEmail(e.Email)] = e.Admin
			}
		}
		return set, nil
	}
}

// Refresh re-reads the source. A failed read keeps the previous list and
// reports why. An empty result is applied, not treated as a failure —
// keeping the previous list on an empty answer would make the last
// entry impossible to remove.
//
// @decision(DL-007)
func (a *Allowlist) Refresh(ctx context.Context) error {
	entries, err := a.load(ctx)
	if err != nil {
		// Only on the way in and the way out. Run refreshes every
		// AllowlistRefresh, so warning per attempt would turn a
		// database outage into six lines a minute per replica for as
		// long as it lasts, which buries whatever else is being logged.
		if !a.failing.Swap(true) {
			a.warn("allowlist: keeping the previous list: " + err.Error())
		}
		return err
	}
	if a.failing.Swap(false) {
		a.warn("allowlist: reading the allowlist again")
	}
	a.entries.Store(&entries)
	return nil
}

// Run refreshes the list until ctx ends. It is the whole reload
// mechanism: there is no signal to send and no file to edit.
//
// @decision(DL-007)
func (a *Allowlist) Run(ctx context.Context) {
	t := time.NewTicker(a.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Refresh has already said why and kept the previous list.
			_ = a.Refresh(ctx)
		}
	}
}

// Lookup reports whether the account githubID, whose address is email,
// may use the server, and whether it is an admin. A githubID of zero is
// a user not yet bound to an account. It reads the cache, so it does no
// I/O and never blocks a request. Addresses compare case-insensitively.
//
// @decision(DL-007) @decision(DL-128)
func (a *Allowlist) Lookup(githubID int64, email string) (admin, ok bool) {
	entries := a.entries.Load()
	if entries == nil {
		return false, false
	}
	return entries.Covers(githubID, email)
}

// Live looks the account up in the source rather than the cache, and
// updates the cache with what it read. Login uses it so a just-added
// user's first login does not race the refresh interval.
//
// @decision(DL-007) @decision(DL-128)
func (a *Allowlist) Live(ctx context.Context, githubID int64, email string) (admin, ok bool, err error) {
	entries, err := a.load(ctx)
	if err != nil {
		return false, false, err
	}
	a.entries.Store(&entries)
	admin, ok = entries.Covers(githubID, email)
	return admin, ok, nil
}

// ParseAllowlistEmail normalises an address and rejects anything that is
// not one, so the admin API and the seed file agree on what may be
// stored. The address must be the whole input: mail.ParseAddress also
// accepts a display-name form, whose address half would not match what
// Lookup compares against.
//
// @decision(DL-007)
func ParseAllowlistEmail(s string) (string, error) {
	email := normalizeEmail(s)
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return "", fmt.Errorf("%q is not an e-mail address", s)
	}
	// RFC 5321's cap. Without it the address goes to the store, where
	// it is half of a btree primary key: past about 2700 bytes Postgres
	// refuses the row with an index-size error, which surfaces as a 502
	// rather than "that is not an address".
	if len(email) > MaxEmailLen {
		return "", fmt.Errorf("that address is %d characters; the limit is %d", len(email), MaxEmailLen)
	}
	return email, nil
}

// MaxEmailLen is RFC 5321's maximum forward-path length.
//
// @decision(DL-007)
const MaxEmailLen = 254

// normalizeEmail lowercases and trims. GitHub addresses are compared
// case-insensitively; the local part technically is not, but no provider
// treats it otherwise and an operator typing Bob@ must not be locked out.
func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
