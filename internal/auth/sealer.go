package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// KeyBytes is the key length AES-256-GCM requires.
//
// @decision(DL-008)
const KeyBytes = 32

// ErrNoKey is a server asked to seal or open without a key configured.
var ErrNoKey = errors.New("auth: no credential key is configured")

// Sealer encrypts user credentials for storage. Keys are held by
// version so a rotation can open what the previous key sealed while
// writing everything new under the current one.
//
// @decision(DL-008)
type Sealer struct {
	current int
	keys    map[int]cipher.AEAD
}

// NewSealer builds a sealer from base64 keys, keyed by version. The
// highest version present is the one new writes use. Prefer
// NewSealerFor, which derives each key's version from the key itself.
//
// @decision(DL-008)
func NewSealer(keys map[int]string) (*Sealer, error) {
	if len(keys) == 0 {
		return nil, ErrNoKey
	}
	s := &Sealer{keys: make(map[int]cipher.AEAD, len(keys))}
	versions := make([]int, 0, len(keys))
	for v := range keys {
		versions = append(versions, v)
	}
	sort.Ints(versions)
	for _, v := range versions {
		aead, err := aeadFromKey(keys[v], v)
		if err != nil {
			return nil, err
		}
		s.keys[v] = aead
		s.current = v
	}
	return s, nil
}

// KeyVersion is the version a key seals under: the first four bytes of
// SHA-256 over the key, as a positive integer.
//
// @decision(DL-008)
func KeyVersion(key []byte) int {
	sum := sha256.Sum256(key)
	// Masked to 31 bits so the version is positive in an int32 column.
	return int(binary.BigEndian.Uint32(sum[:4]) & 0x7fffffff)
}

// NewSealerFor builds a sealer from the current key and any number of
// retired keys that rows may still be sealed under. Each key's version
// is derived from the key, so the order they are given in does not
// change what any of them can open.
//
// @decision(DL-008)
func NewSealerFor(current string, retired ...string) (*Sealer, error) {
	if current == "" {
		return nil, ErrNoKey
	}
	s := &Sealer{keys: map[int]cipher.AEAD{}}
	for i, encoded := range append([]string{current}, retired...) {
		if encoded == "" {
			continue
		}
		raw, err := decodeKey(encoded)
		if err != nil {
			return nil, fmt.Errorf("auth: credential key: %w", err)
		}
		version := KeyVersion(raw)
		aead, err := aeadFromKey(encoded, version)
		if err != nil {
			return nil, err
		}
		if _, taken := s.keys[version]; taken {
			// The same key given twice is harmless; two different keys
			// deriving the same version is not, and must not silently
			// shadow one another.
			continue
		}
		s.keys[version] = aead
		if i == 0 {
			s.current = version
		}
	}
	return s, nil
}

// aeadFromKey decodes one base64 key and builds its cipher. The key
// itself never appears in an error: a decode failure names the length
// it got, which is what an operator needs, and nothing more.
//
// @decision(DL-008)
func aeadFromKey(encoded string, version int) (cipher.AEAD, error) {
	key, err := decodeKey(encoded)
	if err != nil {
		return nil, fmt.Errorf("auth: credential key version %d: %w", version, err)
	}
	if len(key) != KeyBytes {
		return nil, fmt.Errorf("auth: credential key version %d: got %d bytes, need %d"+
			" (generate one with: openssl rand -base64 32)", version, len(key), KeyBytes)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("auth: credential key version %d: %w", version, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("auth: credential key version %d: %w", version, err)
	}
	return aead, nil
}

// decodeKey accepts standard or raw (unpadded) base64, since an
// operator pasting from `openssl rand -base64 32` gets padding and one
// pasting from a secret manager may not.
func decodeKey(s string) ([]byte, error) {
	if key, err := base64.StdEncoding.DecodeString(s); err == nil {
		return key, nil
	}
	key, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("not valid base64")
	}
	return key, nil
}

// Version is the key version new writes are sealed under.
func (s *Sealer) Version() int { return s.current }

// Seal encrypts plaintext under the current key, returning the
// ciphertext and the nonce it used. Each call draws a fresh nonce;
// reusing one under GCM with the same key breaks confidentiality.
//
// @decision(DL-008)
func (s *Sealer) Seal(plaintext []byte, aad []byte) (ciphertext, nonce []byte, version int, err error) {
	aead, ok := s.keys[s.current]
	if !ok {
		return nil, nil, 0, ErrNoKey
	}
	nonce = make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, 0, fmt.Errorf("auth: nonce: %w", err)
	}
	return aead.Seal(nil, nonce, plaintext, aad), nonce, s.current, nil
}

// Open decrypts what Seal produced. A version this sealer has no key
// for is named, since that is the recoverable case — the operator
// dropped a key that rows are still sealed under.
//
// @decision(DL-008)
func (s *Sealer) Open(ciphertext, nonce []byte, version int, aad []byte) ([]byte, error) {
	aead, ok := s.keys[version]
	if !ok {
		return nil, fmt.Errorf("auth: no key for credential version %d;"+
			" set it alongside the current key to read rows sealed under it", version)
	}
	if len(nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("auth: credential nonce is %d bytes, want %d", len(nonce), aead.NonceSize())
	}
	// GCM authenticates as it decrypts, so a wrong key, a tampered
	// ciphertext and a record moved to another user all fail here
	// rather than returning something plausible.
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, errors.New("auth: the stored credential could not be decrypted;" +
			" the key may have changed, or the row may belong to another user")
	}
	return plaintext, nil
}

// CredentialAAD binds a sealed credential to the row that holds it, so
// ciphertext copied to another user's row or another platform fails to
// open rather than decrypting into the wrong session.
//
// @decision(DL-008)
func CredentialAAD(orgID, userID, platform string) []byte {
	return []byte(orgID + "\x00" + userID + "\x00" + platform)
}

// OrgCredentialAAD binds a sealed org credential to its row. It starts
// with a byte no org ID starts with, so an org row can never be opened as
// a user's or the reverse.
//
// @decision(DL-042)
func OrgCredentialAAD(orgID, platform string) []byte {
	return []byte("\x01org-credential\x00" + orgID + "\x00" + platform)
}
