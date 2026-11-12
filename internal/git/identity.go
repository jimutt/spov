package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Identity is the author commits made inside a sandbox are attributed to.
//
// @decision(DL-044)
type Identity struct {
	Name  string
	Email string
}

// Complete reports whether git could commit with this identity. git needs
// both halves, so one alone is not enough.
func (i Identity) Complete() bool { return i.Name != "" && i.Email != "" }

// Empty reports whether nothing at all was resolved.
func (i Identity) Empty() bool { return i.Name == "" && i.Email == "" }

// LocalIdentityIn reads the identity git itself would use, by asking git
// rather than parsing a config file. It runs git in dir, which is what
// decides whether an includeIf.gitdir rule applies; an empty dir inherits
// the process working directory.
//
// A machine with no git, or no identity configured, yields an empty
// Identity. The error is returned only when git ran and failed for some
// other reason, typically a malformed config file.
//
// @decision(DL-044)
func LocalIdentityIn(ctx context.Context, dir string) (Identity, error) {
	name, err := gitConfigValue(ctx, dir, "user.name")
	if err != nil {
		return Identity{}, err
	}
	email, err := gitConfigValue(ctx, dir, "user.email")
	if err != nil {
		return Identity{}, err
	}
	return Identity{Name: name, Email: email}, nil
}

// LocalIdentity reads the identity from the process working directory.
//
// @decision(DL-044)
func LocalIdentity(ctx context.Context) Identity {
	id, _ := LocalIdentityIn(ctx, "")
	return id
}

// gitConfigUnset is git's exit status for a key that is simply not set,
// which is the ordinary case and not an error.
const gitConfigUnset = 1

// gitConfigValue asks git for one setting.
//
// @decision(DL-044)
func gitConfigValue(ctx context.Context, dir, key string) (string, error) {
	cmd := Command(ctx, dir, nil, "config", "--get", key)
	out, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		// git is not installed, or could not be started. Detection is
		// best-effort, so this is silently no identity.
		return "", nil
	}
	if exit.ExitCode() == gitConfigUnset {
		return "", nil
	}
	// Anything else means git ran and objected — a malformed config file
	// exits 128. Its own message says which file and line.
	msg := strings.TrimSpace(string(exit.Stderr))
	if msg == "" {
		msg = err.Error()
	}
	return "", fmt.Errorf("git: reading %s: %s", key, msg)
}

// ResolveIdentity layers an explicit identity over the fallbacks, field by
// field and in order, so configuring only an email does not discard a
// detected name. Each fallback is consulted only while a field is still
// missing, so a source costing a database read or a subprocess is not
// paid for once the answer is complete.
//
// @decision(DL-044) @decision(DL-042)
func ResolveIdentity(explicit Identity, fallbacks ...func() Identity) Identity {
	got := explicit
	for _, next := range fallbacks {
		if got.Complete() {
			return got
		}
		if next == nil {
			continue
		}
		from := next()
		if got.Name == "" {
			got.Name = from.Name
		}
		if got.Email == "" {
			got.Email = from.Email
		}
	}
	return got
}
