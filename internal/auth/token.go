package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/crc32"
	"math/big"
	"strings"
)

// TokenPrefix marks a spov CLI token. It is fixed: a secret scanner
// matches on it, and changing the format after tokens are issued means
// supporting both forever.
//
// @decision(DL-006)
const TokenPrefix = "spov_pat_"

// Token layout: the prefix, 32 random bytes in base62, then a
// 6-character base62 CRC32 of that body. The checksum is not a security
// property — anyone can compute it — it exists so the server rejects a
// typo or a truncated paste before it costs a database round trip, and
// so a scanner can match without false positives.
//
// @decision(DL-006)
const (
	tokenRandomBytes = 32
	tokenCheckLen    = 6
	base62Alphabet   = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// ErrMalformedToken is a token that is not a spov token at all, or whose
// checksum does not match. It never means "unknown token": that answer
// requires the database.
var ErrMalformedToken = errors.New("auth: malformed token")

// NewToken mints a CLI token. The caller shows it to the user once and
// stores only HashToken of it.
//
// @decision(DL-006)
func NewToken() (string, error) {
	raw := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: generating a token: %w", err)
	}
	body := base62(raw)
	return TokenPrefix + body + checksum(body), nil
}

// ParseToken checks the shape and the checksum, returning the token
// unchanged when it is well formed. It never touches storage, so a
// malformed credential costs nothing.
//
// @decision(DL-006)
func ParseToken(token string) (string, error) {
	rest, ok := strings.CutPrefix(token, TokenPrefix)
	if !ok {
		return "", ErrMalformedToken
	}
	if len(rest) <= tokenCheckLen {
		return "", ErrMalformedToken
	}
	body, check := rest[:len(rest)-tokenCheckLen], rest[len(rest)-tokenCheckLen:]
	if strings.TrimLeft(body, base62Alphabet) != "" {
		return "", ErrMalformedToken
	}
	if check != checksum(body) {
		return "", ErrMalformedToken
	}
	return token, nil
}

// HashToken is what the store holds. A database dump therefore cannot be
// replayed as credentials.
//
// @decision(DL-006)
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// checksum is the 6-character base62 CRC32 of the token body, left
// padded so its length is fixed.
func checksum(body string) string {
	sum := crc32.ChecksumIEEE([]byte(body))
	check := base62([]byte{byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum)})
	for len(check) < tokenCheckLen {
		check = "0" + check
	}
	return check[:tokenCheckLen]
}

// base62 renders bytes as a big-endian base62 number.
func base62(b []byte) string {
	n := new(big.Int).SetBytes(b)
	if n.Sign() == 0 {
		return "0"
	}
	base := big.NewInt(62)
	mod := new(big.Int)
	var out []byte
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		out = append(out, base62Alphabet[mod.Int64()])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}
