package auth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/mail"
	"os"
	"sort"
	"strings"

	"github.com/jimutt/spov/internal/store"
)

// SeedAllowlist imports path into the store when the org has no
// administrator, and reports whether it did. The write is one
// transaction, and CountAdmins takes the org's advisory lock inside it,
// so concurrent seeders on a rolling deploy converge rather than race.
//
// Account lines ("@login") are resolved before that transaction, under
// one SeedLookupTimeout for them all, so the lock is never held across
// a call to GitHub; one that cannot be resolved makes the file
// unusable. lookup may be nil when the file names no account.
//
// @decision(DL-007) @decision(DL-128)
func SeedAllowlist(ctx context.Context, st store.Store, orgID, path string, lookup AccountLookup, warn func(string)) (bool, error) {
	if warn == nil {
		warn = func(string) {}
	}
	// A first look outside the lock, so a deployment that has an
	// administrator reads no file and asks GitHub nothing.
	n, err := st.Allowlist().CountAdmins(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("auth: allowlist: counting administrators: %w", err)
	}
	seeded := false
	if n == 0 {
		seeded, err = seedOnce(ctx, st, orgID, path, lookup, warn)
		if err != nil {
			return false, err
		}
	}
	if seeded {
		warn("allowlist: seeded from " + path + "; from now on the allowlist lives in the database" +
			" and this file is ignored")
		// A file that allows people but names no administrator is not
		// refused, so a deployment upgrading with such a file still
		// starts, but it is worth shouting about.
		if n, err := st.Allowlist().CountAdmins(ctx, orgID); err == nil && n == 0 {
			warn("allowlist: " + path + " names no administrator, so nobody can manage the allowlist;" +
				" add \"admin\" to a line and restart, or set admin on a row directly")
		}
		return true, nil
	}
	if _, err := os.Stat(path); err == nil {
		warn("allowlist: the allowlist lives in the database; " + path +
			" is ignored (it seeds a new deployment only)")
	} else if !errors.Is(err, fs.ErrNotExist) {
		warn("allowlist: " + path + " is ignored, and could not be read anyway: " + err.Error())
	}
	return false, nil
}

// seedOnce parses and resolves the file, then writes it if the org
// still has no administrator once the lock is held.
//
// @decision(DL-007) @decision(DL-128)
func seedOnce(ctx context.Context, st store.Store, orgID, path string, lookup AccountLookup, warn func(string)) (bool, error) {
	// Whether this is a first boot or a re-seed decides how bad an
	// unusable file is. See unusableSeed below.
	existing, err := st.Allowlist().List(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("auth: allowlist: reading the allowlist: %w", err)
	}
	firstBoot := len(existing) == 0
	parsed, err := parseAllowlistFile(path)
	if err == nil && parsed.len() == 0 {
		err = fmt.Errorf("%w: %s allows nobody, so no one could log in", ErrEmptyAllowlist, path)
	}
	var accounts map[int64]seedAccount
	if err == nil {
		accounts, err = resolveSeedAccounts(ctx, st.Users(), orgID, lookup, parsed.logins)
	}
	if err != nil {
		return false, unusableSeed(err, path, firstBoot, warn)
	}
	seeded := false
	err = st.Tx(ctx, func(tx store.Store) error {
		al := tx.Allowlist()
		n, err := al.CountAdmins(ctx, orgID)
		if err != nil {
			return fmt.Errorf("auth: allowlist: counting administrators: %w", err)
		}
		if n > 0 {
			return nil
		}
		for email, admin := range parsed.emails {
			if err := al.Put(ctx, store.AllowlistEntry{OrgID: orgID, Email: email, Admin: admin}); err != nil {
				return fmt.Errorf("auth: allowlist: seeding %s: %w", email, err)
			}
		}
		for id, a := range accounts {
			if err := al.Put(ctx, store.AllowlistEntry{
				OrgID: orgID, GithubID: id, GithubLogin: a.Login, GithubName: a.Name, Admin: a.admin,
			}); err != nil {
				return fmt.Errorf("auth: allowlist: seeding @%s: %w", a.Login, err)
			}
		}
		seeded = true
		return nil
	})
	return seeded, err
}

// seedAccount is a resolved account line.
type seedAccount struct {
	GitHubAccount
	admin bool
}

// resolveSeedAccounts resolves every account line under one deadline.
// Two lines that turn out to be one account take the widest privilege,
// as a repeated address does.
//
// @decision(DL-128)
func resolveSeedAccounts(ctx context.Context, users store.UserStore, orgID string, lookup AccountLookup, logins map[string]seedLogin) (map[int64]seedAccount, error) {
	if len(logins) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, SeedLookupTimeout)
	defer cancel()
	names := make([]string, 0, len(logins))
	for name := range logins {
		names = append(names, name)
	}
	sort.Strings(names)
	out := map[int64]seedAccount{}
	for _, name := range names {
		l := logins[name]
		a, err := ResolveAccount(ctx, users, orgID, lookup, l.login)
		if err != nil {
			return nil, fmt.Errorf("auth: allowlist line %d: @%s: %w", l.line, l.login, err)
		}
		cur := out[a.ID]
		out[a.ID] = seedAccount{GitHubAccount: a, admin: cur.admin || l.admin}
	}
	return out, nil
}

// unusableSeed decides what an unreadable or empty seed file means: fatal
// on a first boot (nothing in the table at all), a loud warning on a
// re-seed (entries exist but none is an administrator).
//
// @decision(DL-007)
func unusableSeed(err error, path string, firstBoot bool, warn func(string)) error {
	if firstBoot {
		return err
	}
	warn("allowlist: this org has no administrator and " + path + " cannot seed one (" + err.Error() +
		"); starting anyway, but nobody can manage the allowlist until an admin entry is restored" +
		" — set admin on a row directly, or restore the file and restart")
	return nil
}

// seedFile is a parsed seed: addresses and logins, each with whether it
// is an admin line.
type seedFile struct {
	emails map[string]bool
	// logins is keyed by the lower-cased login, since GitHub logins
	// compare case-insensitively.
	logins map[string]seedLogin
}

// seedLogin is one account line: the login as written, its line, and
// whether any line for it said admin.
type seedLogin struct {
	login string
	line  int
	admin bool
}

func (f seedFile) len() int { return len(f.emails) + len(f.logins) }

func parseAllowlistFile(path string) (seedFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return seedFile{}, fmt.Errorf("auth: allowlist: %w", err)
	}
	defer func() { _ = f.Close() }()
	// A directory is the shape a bind mount leaves behind when its
	// source did not exist: Docker creates the source as a directory
	// and mounts that. Naming the cause saves an operator working
	// backwards from "is a directory" on a read.
	if info, err := f.Stat(); err == nil && info.IsDir() {
		return seedFile{}, fmt.Errorf("auth: allowlist: %s is a directory, not a file"+
			" (a bind mount whose source is missing creates one; remove it and create the file)", path)
	}
	return parseAllowlist(f)
}

// parseAllowlist reads one entry per line: an e-mail or a GitHub account
// written "@login", optionally followed by the word "admin". Blank lines
// and everything from a "#" are ignored. A line that is neither is an
// error naming its number.
//
// @decision(DL-007) @decision(DL-128)
func parseAllowlist(r io.Reader) (seedFile, error) {
	out := seedFile{emails: map[string]bool{}, logins: map[string]seedLogin{}}
	scanner := bufio.NewScanner(r)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if i := strings.IndexByte(text, '#'); i >= 0 {
			text = text[:i]
		}
		fields := strings.Fields(text)
		if len(fields) == 0 {
			continue
		}
		admin := false
		switch len(fields) {
		case 1:
		case 2:
			if !strings.EqualFold(fields[1], "admin") {
				return seedFile{}, fmt.Errorf("auth: allowlist line %d: %q is not \"admin\"", line, fields[1])
			}
			admin = true
		default:
			return seedFile{}, fmt.Errorf("auth: allowlist line %d: expected \"email\", \"@login\","+
				" or either followed by \"admin\"", line)
		}
		if strings.HasPrefix(fields[0], "@") {
			login, err := ParseGitHubLogin(fields[0])
			if err != nil {
				return seedFile{}, fmt.Errorf("auth: allowlist line %d: %w", line, err)
			}
			key := strings.ToLower(login)
			cur, seen := out.logins[key]
			if !seen {
				cur = seedLogin{login: login, line: line}
			}
			cur.admin = cur.admin || admin
			out.logins[key] = cur
			continue
		}
		email := normalizeEmail(fields[0])
		// The point of parsing at all is to catch an operator's typo
		// before it becomes "why can't I log in", so this is a real
		// parse rather than a check for "@". The parsed address must be
		// the whole field: ParseAddress also accepts a display-name
		// form, whose address half would not be what Lookup compares.
		if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
			return seedFile{}, fmt.Errorf("auth: allowlist line %d: %q is not an e-mail address", line, fields[0])
		}
		// A repeated address takes the widest privilege it was given
		// anywhere in the file, so an admin line cannot be silently
		// undone by a later plain one.
		out.emails[email] = out.emails[email] || admin
	}
	if err := scanner.Err(); err != nil {
		return seedFile{}, fmt.Errorf("auth: allowlist: %w", err)
	}
	return out, nil
}
