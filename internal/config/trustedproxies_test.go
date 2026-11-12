package config

import (
	"net/netip"
	"strings"
	"testing"
)

// server.trusted_proxies takes addresses and CIDR ranges, read from the
// user file; an invalid entry fails the load, which is what refuses to
// start the server.
//
// @decision(DL-133)
func TestTrustedProxiesTakeAddressesAndRangesAndRefuseAnythingElse(t *testing.T) {
	got, err := TrustedProxyPrefixes([]string{"10.0.0.0/8", " 192.0.2.10 ", "2001:db8::/32", "::ffff:198.51.100.1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.0.2.10/32"),
		netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("198.51.100.1/32"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %v, want %v", i, got[i], want[i])
		}
	}
	for entry, says := range map[string]string{
		"proxy.internal": "neither an address nor a CIDR range",
		"10.0.0.1/8":     "host bits set; the range is 10.0.0.0/8",
		"10.0.0.0/33":    "neither an address nor a CIDR range",
	} {
		if _, err := TrustedProxyPrefixes([]string{entry}); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%q: err = %v, want it to say %q", entry, err, says)
		}
	}

	env, wd := newFixture(t, "version: 1\nserver:\n  trusted_proxies: [10.0.0.1/8]\n", "")
	if _, err := LoadUser(env); err == nil || !strings.Contains(err.Error(), "server.trusted_proxies") {
		t.Fatalf("LoadUser with a bad entry: err = %v", err)
	}
	env, _ = newFixture(t, "version: 1\nserver:\n  trusted_proxies: [10.0.0.0/8]\n", "")
	env.Vars["SPOV_SERVER_TRUSTED_PROXIES"] = "0.0.0.0/0"
	res, err := Load(env, wd)
	if err != nil {
		t.Fatal(err)
	}
	if p := res.Config.Server.TrustedProxies; len(p) != 1 || p[0] != "10.0.0.0/8" {
		t.Errorf("trusted_proxies = %v, want the file's alone: a list is never set from the environment", p)
	}
}
