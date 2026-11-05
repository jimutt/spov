package credproxy

import (
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

// The CA can only vouch for the hosts it was made for: a certificate it
// signs for any other host fails verification by its name constraints.
//
// @decision(DL-034)
func TestTheCAVouchesOnlyForItsHosts(t *testing.T) {
	ca, pool := testCA(t, "github.com", "codefloe.com")
	for host, want := range map[string]bool{
		"github.com":      true,
		"api.github.com":  true,
		"codefloe.com":    true,
		"example.com":     false,
		"github.com.evil": false,
	} {
		leaf, err := ca.Leaf(host)
		if err != nil {
			t.Fatal(err)
		}
		_, err = leaf.Leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: pool})
		if (err == nil) != want {
			t.Errorf("%s: verify error %v, want valid=%v", host, err, want)
		}
	}
}

// A host's certificate names that host only, and is reused.
func TestALeafNamesOneHostAndIsCached(t *testing.T) {
	ca, _ := testCA(t, "github.com")
	a, err := ca.Leaf("API.GitHub.com")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ca.Leaf("api.github.com")
	if a != b {
		t.Error("the leaf was not cached")
	}
	if names := a.Leaf.DNSNames; len(names) != 1 || names[0] != "api.github.com" {
		t.Errorf("DNS names %v", names)
	}
	if a.Leaf.NotAfter.After(ca.cert.NotAfter) {
		t.Error("the leaf outlives its CA")
	}
}

func TestNewCARefusesABadHostList(t *testing.T) {
	for _, hosts := range [][]string{nil, {""}, {"*.github.com"}, {"github.com:443"}, {"https://github.com"}} {
		if _, _, err := NewCA(hosts, time.Now()); err == nil {
			t.Errorf("NewCA(%q) succeeded", hosts)
		}
	}
}

func TestLoadCARefusesWhatItCannotUse(t *testing.T) {
	certA, keyA, err := NewCA([]string{"github.com"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, keyB, _ := NewCA([]string{"github.com"}, time.Now())
	ca, _ := testCA(t, "github.com")
	leaf, _ := ca.Leaf("github.com")
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Certificate[0]})
	for name, c := range map[string]struct{ cert, key []byte }{
		"not PEM":        {[]byte("nope"), keyA},
		"key not PEM":    {certA, []byte("nope")},
		"mismatched key": {certA, keyB},
		"not a CA":       {leafPEM, keyA},
		"garbage cert":   {pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")}), keyA},
		"garbage key":    {certA, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("x")})},
	} {
		if _, err := LoadCA(c.cert, c.key); err == nil || !strings.HasPrefix(err.Error(), "credproxy:") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A CA covers a host list only while it is valid and its name
// constraints permit every host on it.
//
// @decision(DL-034)
func TestACACoversOnlyItsHostsWhileValid(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	cert, _, err := NewCA([]string{"github.com", "openrouter.ai"}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		hosts []string
		at    time.Time
		want  bool
	}{
		{[]string{"github.com"}, now, true},
		{[]string{"openrouter.ai", "github.com"}, now, true},
		{[]string{"api.github.com"}, now, true},
		{[]string{"github.com", "api.anthropic.com"}, now, false},
		{[]string{"evilgithub.com"}, now, false},
		{nil, now, false},
		{[]string{"github.com"}, now.Add(CAValidity), false},
		{[]string{"github.com"}, now.Add(-2 * time.Hour), false},
	} {
		if got := CACovers(cert, c.hosts, c.at); got != c.want {
			t.Errorf("CACovers(%v at %s) = %v, want %v", c.hosts, c.at.Sub(now), got, c.want)
		}
	}
	if CACovers([]byte("not a cert"), []string{"github.com"}, now) {
		t.Error("garbage covers")
	}
}
