package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/big"
	"strings"
)

// UserCodeAlphabet is what a human retypes from a terminal into a
// browser. It has no vowels, so a code cannot spell a word, and it
// excludes every character that pairs with another for confusion (0/O,
// 1/I/L, 5/S): L and S are kept because the characters they are misread
// as are not in the alphabet.
//
// @decision(DL-006)
const UserCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

// UserCodeLen is the number of characters, shown as XXXX-XXXX. Twenty to
// the eighth is about 2.6e10, which is what makes the approval page's
// rate limit sufficient.
//
// @decision(DL-006)
const UserCodeLen = 8

// NewUserCode returns a code as XXXX-XXXX.
//
// @decision(DL-006)
func NewUserCode() (string, error) {
	b := make([]byte, UserCodeLen)
	max := big.NewInt(int64(len(UserCodeAlphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("auth: generating a user code: %w", err)
		}
		b[i] = UserCodeAlphabet[n.Int64()]
	}
	half := UserCodeLen / 2
	return string(b[:half]) + "-" + string(b[half:]), nil
}

// NormalizeUserCode puts a code a human typed into the stored form:
// upper case, no spaces, and the dash supplied whether or not they typed
// it. Anything else is left alone so the lookup simply misses.
//
// @decision(DL-006)
func NormalizeUserCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		if strings.ContainsRune(UserCodeAlphabet, r) {
			b.WriteRune(r)
		}
	}
	code := b.String()
	if len(code) != UserCodeLen {
		return code
	}
	half := UserCodeLen / 2
	return code[:half] + "-" + code[half:]
}

// NewDeviceCode returns the secret the polling client holds, and its
// hash, which is what the store keeps.
//
// @decision(DL-006)
func NewDeviceCode() (code string, hash []byte, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("auth: generating a device code: %w", err)
	}
	code = base64.RawURLEncoding.EncodeToString(raw)
	return code, HashDeviceCode(code), nil
}

// HashDeviceCode is what the store holds.
//
// @decision(DL-006)
func HashDeviceCode(code string) []byte {
	sum := sha256.Sum256([]byte(code))
	return sum[:]
}
