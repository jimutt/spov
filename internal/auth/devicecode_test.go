package auth

import (
	"bytes"
	"math/big"
	"strings"
	"testing"
)

// TestNewUserCodeShapeAndAlphabet: the code is retyped by a human from a
// terminal into a browser, so the alphabet's properties are what make
// that survivable.
//
// @decision(DL-006)
func TestNewUserCodeShapeAndAlphabet(t *testing.T) {
	seen := map[string]bool{}
	for range 500 {
		code, err := NewUserCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != UserCodeLen+1 || code[UserCodeLen/2] != '-' {
			t.Fatalf("code %q is not XXXX-XXXX", code)
		}
		if seen[code] {
			t.Fatalf("a code repeated within 500 draws: %q", code)
		}
		seen[code] = true
		for _, r := range strings.ReplaceAll(code, "-", "") {
			if !strings.ContainsRune(UserCodeAlphabet, r) {
				t.Fatalf("code %q contains %q, outside the alphabet", code, r)
			}
		}
	}
}

// TestUserCodeAlphabetIsUnambiguous documents the properties the
// approval page's rate limit leans on.
//
// @decision(DL-006)
func TestUserCodeAlphabetIsUnambiguous(t *testing.T) {
	// No vowels, so a code cannot spell a word.
	if strings.ContainsAny(UserCodeAlphabet, "AEIOU") {
		t.Error("the alphabet contains vowels")
	}
	// None of the characters that pair with a member for confusion.
	for _, c := range "01IOU" {
		if strings.ContainsRune(UserCodeAlphabet, c) {
			t.Errorf("the alphabet contains %q, which is confusable", c)
		}
	}
	// No duplicates.
	seen := map[rune]bool{}
	for _, r := range UserCodeAlphabet {
		if seen[r] {
			t.Errorf("the alphabet repeats %q", r)
		}
		seen[r] = true
	}
	// The space is what makes guessing hopeless: 20^8 ≈ 2.6e10.
	space := new(big.Int).Exp(big.NewInt(int64(len(UserCodeAlphabet))), big.NewInt(UserCodeLen), nil)
	if space.Cmp(big.NewInt(1e10)) < 0 {
		t.Errorf("the code space is %s, under 10^10", space)
	}
}

// TestNormalizeUserCode accepts what a human actually types.
//
// @decision(DL-006)
func TestNormalizeUserCode(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"BCDF-GHJK", "BCDF-GHJK"},
		// Lower case, and without the dash.
		{"bcdfghjk", "BCDF-GHJK"},
		{"bcdf-ghjk", "BCDF-GHJK"},
		// Spaces anywhere.
		{"  BCDF GHJK  ", "BCDF-GHJK"},
		{"B C D F G H J K", "BCDF-GHJK"},
		// Anything outside the alphabet is dropped, so a code that
		// came back wrong simply misses rather than matching something
		// else.
		{"BCDF-GHJ", "BCDFGHJ"},
		{"", ""},
		// Characters outside the alphabet are dropped rather than
		// rejected, so what is left is looked up and simply misses.
		// "not a code" keeps N, T, C, D — which is not eight, so it
		// gets no dash and matches nothing.
		{"not a code", "NTCD"},
		{"vowels aeiou", "VWLS"},
	} {
		if got := NormalizeUserCode(tc.in); got != tc.want {
			t.Errorf("NormalizeUserCode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNormalizeUserCodeIsIdempotent, since it runs on both the query
// parameter and the submitted form.
//
// @decision(DL-006)
func TestNormalizeUserCodeIsIdempotent(t *testing.T) {
	code, err := NewUserCode()
	if err != nil {
		t.Fatal(err)
	}
	once := NormalizeUserCode(code)
	if once != code {
		t.Errorf("normalising a fresh code changed it: %q -> %q", code, once)
	}
	if twice := NormalizeUserCode(once); twice != once {
		t.Errorf("not idempotent: %q -> %q", once, twice)
	}
}

// TestNewDeviceCodeIsRandomAndHashed: the store holds the hash, so a
// database dump yields nothing exchangeable.
//
// @decision(DL-006)
func TestNewDeviceCodeIsRandomAndHashed(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		code, hash, err := NewDeviceCode()
		if err != nil {
			t.Fatal(err)
		}
		if seen[code] {
			t.Fatalf("a device code repeated: %q", code)
		}
		seen[code] = true
		if !bytes.Equal(hash, HashDeviceCode(code)) {
			t.Error("the returned hash is not the code's")
		}
		if strings.Contains(string(hash), code) {
			t.Error("the hash carries the code")
		}
		// 32 random bytes, base64url: 43 characters.
		if len(code) < 43 {
			t.Errorf("device code is %d characters, too short", len(code))
		}
	}
}

// TestHashDeviceCodeIsStableAndDistinct.
//
// @decision(DL-006)
func TestHashDeviceCodeIsStableAndDistinct(t *testing.T) {
	if !bytes.Equal(HashDeviceCode("abc"), HashDeviceCode("abc")) {
		t.Error("hashing is not stable")
	}
	if bytes.Equal(HashDeviceCode("abc"), HashDeviceCode("abd")) {
		t.Error("two codes hashed the same")
	}
	if len(HashDeviceCode("abc")) != 32 {
		t.Error("the hash is not sha256-sized")
	}
}
