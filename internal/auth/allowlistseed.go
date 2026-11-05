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
	"strings"

	"github.com/jimutt/spov/internal/store"
)

// SeedAllowlist imports path into the store when the org has no
// administrator, and reports whether it did. The whole check-and-import
// is one transaction, and CountAdmins takes the org's advisory lock
// inside it, so concurrent seeders on a rolling deploy converge rather
// than race.
//
// @decision(DL-007)
func SeedAllowlist(ctx context.Context, st store.Store, orgID, path string, warn func(string)) (bool, error) {
	if warn == nil {
		warn = func(string) {}
	}
	seeded := false
	err := st.Tx(ctx, func(tx store.Store) error {
		al := tx.Allowlist()
		n, err := al.CountAdmins(ctx, orgID)
		if err != nil {
			return fmt.Errorf("auth: allowlist: counting administrators: %w", err)
		}
		if n > 0 {
			return nil
		}
		// Whether this is a first boot or a re-seed decides how bad an
		// unusable file is. See unusableSeed below.
		existing, err := al.List(ctx, orgID)
		if err != nil {
			return fmt.Errorf("auth: allowlist: reading the allowlist: %w", err)
		}
		entries, err := parseAllowlistFile(path)
		if err == nil && len(entries) == 0 {
			err = fmt.Errorf("%w: %s allows nobody, so no one could log in", ErrEmptyAllowlist, path)
		}
		if err != nil {
			return unusableSeed(err, path, len(existing) == 0, warn)
		}
		for email, admin := range entries {
			if err := al.Put(ctx, store.AllowlistEntry{OrgID: orgID, Email: email, Admin: admin}); err != nil {
				return fmt.Errorf("auth: allowlist: seeding %s: %w", email, err)
			}
		}
		seeded = true
		return nil
	})
	if err != nil {
		return false, err
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

func parseAllowlistFile(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("auth: allowlist: %w", err)
	}
	defer func() { _ = f.Close() }()
	// A directory is the shape a bind mount leaves behind when its
	// source did not exist: Docker creates the source as a directory
	// and mounts that. Naming the cause saves an operator working
	// backwards from "is a directory" on a read.
	if info, err := f.Stat(); err == nil && info.IsDir() {
		return nil, fmt.Errorf("auth: allowlist: %s is a directory, not a file"+
			" (a bind mount whose source is missing creates one; remove it and create the file)", path)
	}
	return parseAllowlist(f)
}

// parseAllowlist reads one entry per line: an e-mail, optionally
// followed by the word "admin". Blank lines and everything from a "#"
// are ignored. A line that is neither is an error naming its number.
//
// @decision(DL-007)
func parseAllowlist(r io.Reader) (map[string]bool, error) {
	entries := map[string]bool{}
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
		email := normalizeEmail(fields[0])
		// The point of parsing at all is to catch an operator's typo
		// before it becomes "why can't I log in", so this is a real
		// parse rather than a check for "@". The parsed address must be
		// the whole field: ParseAddress also accepts a display-name
		// form, whose address half would not be what Lookup compares.
		if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
			return nil, fmt.Errorf("auth: allowlist line %d: %q is not an e-mail address", line, fields[0])
		}
		admin := false
		switch len(fields) {
		case 1:
		case 2:
			if !strings.EqualFold(fields[1], "admin") {
				return nil, fmt.Errorf("auth: allowlist line %d: %q is not \"admin\"", line, fields[1])
			}
			admin = true
		default:
			return nil, fmt.Errorf("auth: allowlist line %d: expected \"email\" or \"email admin\"", line)
		}
		// A repeated address takes the widest privilege it was given
		// anywhere in the file, so an admin line cannot be silently
		// undone by a later plain one.
		entries[email] = entries[email] || admin
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("auth: allowlist: %w", err)
	}
	return entries, nil
}
