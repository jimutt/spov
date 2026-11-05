package git

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

// TestAppJWTVerifies checks the signature the way GitHub would, so a
// broken signing path fails here rather than as a 401 against the real
// API.
//
// @decision(DL-041)
func TestAppJWTVerifies(t *testing.T) {
	key, _ := githubTestKey(t)
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	token, err := appJWT(key, "Iv23liABC", now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("the JWT has %d parts, want 3", len(parts))
	}

	signing := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signing))
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("the JWT does not verify: %v", err)
	}
}

// TestAppJWTClaims: GitHub rejects a JWT whose exp is more than ten
// minutes out, and recommends backdating iat against clock drift.
//
// @decision(DL-041)
func TestAppJWTClaims(t *testing.T) {
	key, _ := githubTestKey(t)
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	token, err := appJWT(key, "Iv23liABC", now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")

	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	decodeJWTPart(t, parts[0], &header)
	if header.Alg != "RS256" {
		t.Errorf("alg = %q, want RS256", header.Alg)
	}
	if header.Typ != "JWT" {
		t.Errorf("typ = %q", header.Typ)
	}

	var claims struct {
		IAT int64  `json:"iat"`
		EXP int64  `json:"exp"`
		ISS string `json:"iss"`
	}
	decodeJWTPart(t, parts[1], &claims)
	if claims.ISS != "Iv23liABC" {
		t.Errorf("iss = %q", claims.ISS)
	}

	iat := time.Unix(claims.IAT, 0).UTC()
	exp := time.Unix(claims.EXP, 0).UTC()
	if !iat.Before(now) {
		t.Errorf("iat = %v, want it backdated against clock drift", iat)
	}
	if got := now.Sub(iat); got != jwtBackdate {
		t.Errorf("iat is %s in the past, want %s", got, jwtBackdate)
	}
	// The hard constraint: GitHub rejects exp more than 10 minutes out.
	if ahead := exp.Sub(now); ahead > 10*time.Minute {
		t.Errorf("exp is %s ahead; GitHub rejects anything over 10m", ahead)
	}
	if !exp.After(now) {
		t.Errorf("exp = %v is not in the future", exp)
	}
}

func TestAppJWTRejectsMissingInputs(t *testing.T) {
	key, _ := githubTestKey(t)
	if _, err := appJWT(nil, "app", time.Now()); err == nil {
		t.Error("a nil key was accepted")
	}
	if _, err := appJWT(key, "", time.Now()); err == nil {
		t.Error("an empty issuer was accepted")
	}
}

// TestParsePrivateKeyFormats: GitHub hands out PKCS#1, but tools convert
// keys to PKCS#8 on the way through.
//
// @decision(DL-041)
func TestParsePrivateKeyFormats(t *testing.T) {
	key, pkcs1 := githubTestKey(t)

	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Bytes}))

	for name, data := range map[string]string{"PKCS#1": pkcs1, "PKCS#8": pkcs8} {
		t.Run(name, func(t *testing.T) {
			parsed, err := parsePrivateKey(data)
			if err != nil {
				t.Fatal(err)
			}
			if !parsed.Equal(key) {
				t.Error("the parsed key differs from the original")
			}
		})
	}
}

// TestParsePrivateKeyRejectsNonRSA covers the type-assertion branch: an
// ECDSA key is valid PKCS#8, so it parses and must then be rejected for
// being the wrong algorithm.
//
// @decision(DL-041)
func TestParsePrivateKeyRejectsNonRSA(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	_, err = parsePrivateKey(string(block))
	if err == nil {
		t.Fatal("an ECDSA key was accepted for a GitHub App")
	}
	if !strings.Contains(err.Error(), "RSA") {
		t.Errorf("err = %v; it should say GitHub Apps use RSA", err)
	}
}

func TestParsePrivateKeyRejectsGarbage(t *testing.T) {
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not a key")})
	if _, err := parsePrivateKey(string(block)); err == nil {
		t.Fatal("a malformed PKCS#8 body was accepted")
	}
	if _, err := parsePrivateKey("no pem here"); err == nil {
		t.Fatal("a non-PEM string was accepted")
	}
}

func decodeJWTPart(t *testing.T, part string, out any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatalf("decode JWT part: %v", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal JWT part: %v", err)
	}
}
