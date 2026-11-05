// Package credproxy is the proxy that runs in a sandbox's sidecar and
// adds credentials to forge requests, so that no forge token is ever in
// the sandbox's main container. It is built on the standard library.
//
// @decision(DL-034)
package credproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"
	"time"
)

// CAValidity is how long a sandbox's CA lives. A sandbox lives at most
// 24 hours, and a clock skewed by an hour must not fail a handshake.
const CAValidity = 26 * time.Hour

// leafValidity bounds each host certificate the proxy issues. It is cut
// to the CA's own expiry.
const leafValidity = 24 * time.Hour

// NewCA generates a CA for one sandbox. Its name constraints permit only
// the given hosts, so its key could not sign for any other host even if
// it leaked from the sidecar. It returns the certificate and key as PEM.
//
// @decision(DL-034)
func NewCA(hosts []string, now time.Time) (certPEM, keyPEM []byte, err error) {
	if len(hosts) == 0 {
		return nil, nil, errors.New("credproxy: a CA needs at least one host")
	}
	permitted := make([]string, 0, len(hosts))
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || strings.ContainsAny(h, "*:/ ") {
			return nil, nil, fmt.Errorf("credproxy: %q is not a host name", h)
		}
		permitted = append(permitted, h)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("credproxy: generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:                serial,
		Subject:                     pkix.Name{CommonName: "spov sandbox proxy CA"},
		NotBefore:                   now.Add(-time.Hour),
		NotAfter:                    now.Add(CAValidity),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         permitted,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("credproxy: sign CA: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("credproxy: encode CA key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// CACovers reports whether certPEM is a CA that is valid at now, for at
// least another hour, and whose name constraints permit every host, so a
// recreated sidecar can reuse it.
//
// @decision(DL-034)
func CACovers(certPEM []byte, hosts []string, now time.Time) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !cert.IsCA || now.Before(cert.NotBefore) || now.Add(time.Hour).After(cert.NotAfter) {
		return false
	}
	for _, h := range hosts {
		h = strings.ToLower(h)
		if !slices.ContainsFunc(cert.PermittedDNSDomains, func(d string) bool {
			return h == d || strings.HasSuffix(h, "."+d)
		}) {
			return false
		}
	}
	return len(hosts) > 0
}

// CA issues the proxy's host certificates, one per host, and caches
// them for the proxy's life.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	now  func() time.Time

	mu    sync.Mutex
	leafs map[string]*tls.Certificate
}

// LoadCA parses a CA that NewCA produced.
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, errors.New("credproxy: the CA certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("credproxy: parse CA certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, errors.New("credproxy: the CA certificate is not a CA")
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, errors.New("credproxy: the CA key is not PEM")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("credproxy: parse CA key: %w", err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("credproxy: the CA key does not match its certificate")
	}
	return &CA{cert: cert, key: key, now: time.Now, leafs: map[string]*tls.Certificate{}}, nil
}

// Leaf returns the certificate the proxy presents for host, with host as
// its only name.
//
// @decision(DL-034)
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	host = strings.ToLower(host)
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.leafs[host]; ok {
		return l, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("credproxy: generate key for %s: %w", host, err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := c.now()
	notAfter := now.Add(leafValidity)
	if notAfter.After(c.cert.NotAfter) {
		notAfter = c.cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("credproxy: sign certificate for %s: %w", host, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	l := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	c.leafs[host] = l
	return l, nil
}

func randomSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("credproxy: serial: %w", err)
	}
	return n, nil
}
