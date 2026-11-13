package auth

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jimutt/spov/internal/store"
)

// ServiceAccountDomain is the domain of every service account's
// address. `.invalid` is reserved, so no forge verifies an address
// under it.
//
// @decision(DL-140)
const ServiceAccountDomain = "service.invalid"

// ServiceAccountAddress is the address a service account's row carries.
//
// @decision(DL-140)
func ServiceAccountAddress(name string) string {
	return name + "@" + ServiceAccountDomain
}

// IsServiceAddress reports whether s is an address under
// ServiceAccountDomain or one of its subdomains, in any case and with
// or without a trailing dot.
//
// @decision(DL-140)
func IsServiceAddress(s string) bool {
	at := strings.LastIndexByte(s, '@')
	if at < 0 {
		return false
	}
	domain := strings.TrimRight(normalizeEmail(s[at+1:]), ".")
	return domain == ServiceAccountDomain || strings.HasSuffix(domain, "."+ServiceAccountDomain)
}

// ErrServiceAddress is an allowlist entry or a sign-in whose address is
// a service account's.
var ErrServiceAddress = errors.New("auth: the address is under " + ServiceAccountDomain +
	", which only service accounts use")

// CheckAllowlistAddress refuses an address no allowlist entry may hold:
// one under ServiceAccountDomain.
//
// @decision(DL-140)
func CheckAllowlistAddress(email string) error {
	if IsServiceAddress(email) {
		return fmt.Errorf("%w; a service account is never on the allowlist", ErrServiceAddress)
	}
	return nil
}

// errNotAPerson is a credential whose user row is not a person's, where
// only a person may authenticate.
var errNotAPerson = errors.New("the user is not a person")

// personRow reports whether a row is a person's (store.User.Person).
//
// @decision(DL-140)
func personRow(u store.User) bool { return u.Person() }
