package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The pin is served without a credential: a client needs it before it
// has one.
//
// @decision(DL-035)
func TestThePinIsAnsweredWithoutACredential(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) { d.Defaults.OpenCodeVersion = "0.0.0-beta-19151" })
	resp, body := f.do(http.MethodGet, OpenCodePinPath, nil, func(r *http.Request) {
		r.Header.Del("Authorization")
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s = %d %s with no credential", OpenCodePinPath, resp.StatusCode, body)
	}
	var pin OpenCodePin
	if err := json.Unmarshal(body, &pin); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if pin.OpenCode != "0.0.0-beta-19151" {
		t.Errorf("pin = %+v; it must report what the deployment configured", pin)
	}
	if pin.Scheme != PinScheme {
		t.Errorf("scheme = %d, want %d", pin.Scheme, PinScheme)
	}
}

// The pin carries the version and nothing else about the deployment,
// since it answers before authentication to anyone who can reach the
// port.
//
// @decision(DL-035)
func TestThePinCarriesNothingButTheVersion(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) {
		d.Defaults.OpenCodeVersion = "0.0.0-beta-19151"
		d.Defaults.Image = "registry.test/private-image:secret-tag"
	})
	_, body := f.do(http.MethodGet, OpenCodePinPath, nil, func(r *http.Request) {
		r.Header.Del("Authorization")
	})
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	for k := range fields {
		if k != "scheme" && k != "opencode" {
			t.Errorf("the pin carries %q; it may carry the version and the scheme alone", k)
		}
	}
	if strings.Contains(string(body), "private-image") {
		t.Errorf("the pin names the deployment's image: %s", body)
	}
}

// A deployment that pins nothing answers an empty version rather than
// failing. The endpoint is a handshake and never a gate.
//
// @decision(DL-035)
func TestAPinlessDeploymentStillAnswers(t *testing.T) {
	f := newAPIFixture(t)
	resp, body := f.do(http.MethodGet, OpenCodePinPath, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s = %d %s", OpenCodePinPath, resp.StatusCode, body)
	}
	var pin OpenCodePin
	if err := json.Unmarshal(body, &pin); err != nil {
		t.Fatal(err)
	}
	if pin.OpenCode != "" {
		t.Errorf("pin = %+v, want an empty version", pin)
	}
}

// The pin is outside the browser guard as well as outside
// authentication: behind the guard this would answer 403, which a
// caller cannot tell from a deployment that has no pin.
//
// @decision(DL-035) @decision(DL-010)
func TestThePinIsAnsweredOutsideTheBrowserGuard(t *testing.T) {
	f := newAPIFixtureWith(t, func(d *Deps) { d.Defaults.OpenCodeVersion = "0.0.0-beta-19151" })
	resp, body := f.do(http.MethodGet, OpenCodePinPath, nil, func(r *http.Request) {
		r.Header.Del("Authorization")
		r.Header.Set("Origin", "https://evil.test")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s = %d %s from a cross-site request; the pin runs outside the guard", OpenCodePinPath, resp.StatusCode, body)
	}
}

// The pin is the server's path, not the web app's, or the web app's 404
// page would read as a deployment that has no pin.
//
// @decision(DL-035) @decision(DL-055)
func TestThePinPathIsTheServers(t *testing.T) {
	if !(&Server{}).ownsPath(OpenCodePinPath) {
		t.Errorf("%s is not claimed by the server, so the web app answers it", OpenCodePinPath)
	}
}
