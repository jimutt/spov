package git

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"
)

// GitHub App JWTs are RS256 over two base64url JSON objects, which the
// standard library produces directly. A JWT library would add a
// dependency for one signature.
//
// @decision(DL-041)
const (
	// jwtBackdate offsets iat into the past. GitHub recommends 60s to
	// tolerate clock drift between spov's host and GitHub.
	jwtBackdate = 60 * time.Second
	// jwtLifetime is how far exp is set into the future. GitHub rejects
	// anything more than 10 minutes ahead; 9 leaves room for the drift
	// jwtBackdate already assumes.
	jwtLifetime = 9 * time.Minute
)

// parsePrivateKey reads a PEM-encoded RSA private key. GitHub issues
// PKCS#1 ("RSA PRIVATE KEY"); PKCS#8 is accepted because some tools
// convert keys on download.
//
// @decision(DL-041)
func parsePrivateKey(pemData string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("git: the GitHub App private key is not PEM-encoded; " +
			"it should start with -----BEGIN RSA PRIVATE KEY-----")
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// The error deliberately carries no key material.
		return nil, fmt.Errorf("git: the GitHub App private key is neither PKCS#1 nor PKCS#8")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("git: the GitHub App private key is %T, but GitHub Apps use RSA", parsed)
	}
	return key, nil
}

// appJWT signs a GitHub App JWT. issuer is the App ID or client ID;
// GitHub accepts either and recommends the client ID.
//
// @decision(DL-041)
func appJWT(key *rsa.PrivateKey, issuer string, now time.Time) (string, error) {
	if key == nil {
		return "", fmt.Errorf("git: no GitHub App private key")
	}
	if issuer == "" {
		return "", fmt.Errorf("git: no GitHub App id")
	}

	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("git: encode JWT header: %w", err)
	}
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": issuer,
	})
	if err != nil {
		return "", fmt.Errorf("git: encode JWT claims: %w", err)
	}

	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("git: sign JWT: %w", err)
	}
	return signing + "." + enc.EncodeToString(signature), nil
}
