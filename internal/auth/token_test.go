package auth

import (
	"bytes"
	"errors"
	"math/big"
	"strings"
	"testing"
)

// TestNewTokenRoundTrips: a freshly minted token passes its own parse,
// which is the property every other check depends on.
//
// @decision(DL-006)
func TestNewTokenRoundTrips(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		tok, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(tok, TokenPrefix) {
			t.Fatalf("token %q has no prefix", tok)
		}
		if seen[tok] {
			t.Fatalf("a token repeated: %q", tok)
		}
		seen[tok] = true
		if _, err := ParseToken(tok); err != nil {
			t.Fatalf("a fresh token failed its own parse: %v", err)
		}
	}
}

// TestParseTokenRejectsCorruption: the checksum exists so a typo or a
// truncated paste is refused before it costs a database round trip.
//
// @decision(DL-006)
func TestParseTokenRejectsCorruption(t *testing.T) {
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}

	// Every single-character change in the body must break it.
	body := strings.TrimPrefix(tok, TokenPrefix)
	for i := range len(body) - tokenCheckLen {
		bad := []byte(tok)
		pos := len(TokenPrefix) + i
		if bad[pos] == 'a' {
			bad[pos] = 'b'
		} else {
			bad[pos] = 'a'
		}
		if _, err := ParseToken(string(bad)); !errors.Is(err, ErrMalformedToken) {
			t.Fatalf("a token corrupted at %d was accepted", i)
		}
	}
	// And a truncation.
	if _, err := ParseToken(tok[:len(tok)-1]); !errors.Is(err, ErrMalformedToken) {
		t.Error("a truncated token was accepted")
	}
}

// TestParseTokenRejectsJunk covers what actually arrives in an
// Authorization header when it is not a spov token.
//
// @decision(DL-006)
func TestParseTokenRejectsJunk(t *testing.T) {
	for _, bad := range []string{
		"",
		"nope",
		TokenPrefix,
		TokenPrefix + "short",
		TokenPrefix + "abcdef",
		// A base64 token from somewhere else.
		"ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		// The right shape with characters outside base62.
		TokenPrefix + "aaaa!!!!aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		// A local-mode bearer, which is base64url and must not parse.
		"kZ4v_9r-QwErTyUiOpAsDfGhJkLzXcVbNm1234567890",
	} {
		if _, err := ParseToken(bad); !errors.Is(err, ErrMalformedToken) {
			t.Errorf("ParseToken(%q) = %v, want ErrMalformedToken", bad, err)
		}
	}
}

// TestHashTokenIsStableAndDistinct: the store holds only the hash, so a
// database dump cannot be replayed.
//
// @decision(DL-006)
func TestHashTokenIsStableAndDistinct(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(HashToken(a), HashToken(a)) {
		t.Error("hashing is not stable")
	}
	if bytes.Equal(HashToken(a), HashToken(b)) {
		t.Error("two tokens hashed the same")
	}
	if len(HashToken(a)) != 32 {
		t.Errorf("hash is %d bytes, want 32", len(HashToken(a)))
	}
	// The hash must not contain the token: it is what gets stored.
	if strings.Contains(string(HashToken(a)), a) {
		t.Error("the hash carries the token")
	}
}

// TestTokenIsLongEnoughToResistGuessing pins that no entropy is lost
// encoding 32 random bytes into the token body.
//
// It checks by decoding rather than by measuring length: base62 is a
// big-integer base conversion with no padding, so the body's length
// varies with the value (43 characters usually, shorter about one time
// in 61), making a length assertion nondeterministic. Round-tripping
// tests the same property deterministically.
//
// @decision(DL-006)
func TestTokenIsLongEnoughToResistGuessing(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"all zero", make([]byte, tokenRandomBytes)},
		{"all ones", bytes.Repeat([]byte{0xff}, tokenRandomBytes)},
		{"high bit set", append([]byte{0x80}, make([]byte, tokenRandomBytes-1)...)},
		{"top byte clear", append([]byte{0x00}, bytes.Repeat([]byte{0xff}, tokenRandomBytes-1)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unbase62(t, base62(tc.raw)); !bytes.Equal(got, tc.raw) {
				t.Errorf("32 bytes did not survive the encoding: got %x, want %x", got, tc.raw)
			}
		})
	}
	// The largest 32-byte value needs all 43 characters, which is what
	// says the encoding carries 256 bits rather than merely looking
	// long. Deterministic: it is a fixed input, not a random one.
	if got := len(base62(bytes.Repeat([]byte{0xff}, tokenRandomBytes))); got != 43 {
		t.Errorf("the largest 32-byte value encodes to %d characters, want 43", got)
	}

	// And a real token's body is the encoding, unpadded and untouched.
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(tok, TokenPrefix)
	body = body[:len(body)-tokenCheckLen]
	raw := unbase62(t, body)
	if base62(raw) != body {
		t.Errorf("a minted token's body is not the base62 of its own value")
	}
}

// unbase62 is the inverse of base62, left-padded back to 32 bytes.
// There is no decoder in the package — nothing needs one, since a token
// is compared as text — so the test carries its own.
func unbase62(t *testing.T, s string) []byte {
	t.Helper()
	n := new(big.Int)
	for _, r := range s {
		i := strings.IndexRune(base62Alphabet, r)
		if i < 0 {
			t.Fatalf("%q is not in the alphabet", r)
		}
		n.Add(n.Mul(n, big.NewInt(62)), big.NewInt(int64(i)))
	}
	out := make([]byte, tokenRandomBytes)
	n.FillBytes(out)
	return out
}

// TestBase62UsesTheWholeAlphabet, since a bug that emitted only digits
// would still round-trip while carrying far less entropy.
//
// @decision(DL-006)
func TestBase62UsesTheWholeAlphabet(t *testing.T) {
	seen := map[rune]bool{}
	for range 500 {
		tok, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range strings.TrimPrefix(tok, TokenPrefix) {
			seen[r] = true
		}
	}
	// Not every one of the 62 need appear, but a heavily biased encoder
	// would show up as a small set.
	if len(seen) < 50 {
		t.Errorf("only %d distinct characters across 500 tokens", len(seen))
	}
	for r := range seen {
		if !strings.ContainsRune(base62Alphabet, r) {
			t.Errorf("token contains %q, which is outside the alphabet", r)
		}
	}
}
