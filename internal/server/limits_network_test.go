package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/telemetry/telemetrytest"
)

// v6In48 is an address in its own /64 of the /48 2001:db8:7::/48.
func v6In48(i int) string {
	return fmt.Sprintf("[2001:db8:7:%x::1]:4000", i)
}

// One /48 holds 65,536 /64s, each with a bucket of its own, so a burst
// from one allocation could fill the replica's cap and leave every
// other client at 503. The network's bucket and its share stop it, and
// a client elsewhere still signs in.
//
// @decision(DL-133)
func TestOneNetworkCannotFillTheSignInCap(t *testing.T) {
	f := newDeviceFixtureWith(t, "alice@example.com\n", func(d *Deps) {
		d.Limits = limitsWith(newLimitClock(), nil)
	})
	send := deviceHit(f)
	admitted := 0
	for i := range MaxPendingLogins {
		if rec := send(http.MethodGet, LoginPath, v6In48(i/SignInBurst), nil); rec.Code == http.StatusSeeOther {
			admitted++
		} else if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("start %d from the /48 = %d, want admitted or 429", i+1, rec.Code)
		}
	}
	if admitted > 300 {
		t.Errorf("one /48 started %d sign-ins at once, want at most its network's burst of 300", admitted)
	}
	if rec := send(http.MethodGet, LoginPath, "198.51.100.9:4000", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("a client elsewhere = %d %s, want it to sign in", rec.Code, rec.Body)
	}
}

// A sign-in start the browser guard refuses spends neither its
// address's token nor a place in the cap.
//
// @decision(DL-133)
func TestASignInTheGuardRefusesSpendsNothing(t *testing.T) {
	f := newDeviceFixtureWith(t, "alice@example.com\n", func(d *Deps) {
		d.Limits = limitsWith(newLimitClock(), func(l *Limits) {
			l.SignIns = Rate{1, 1}
			l.PendingLogins = 1
		})
	})
	h, host := f.server.Handler(), f.http.Listener.Addr().String()
	login := func(site string) int {
		req := httptest.NewRequest(http.MethodGet, LoginPath, nil)
		req.Host, req.RemoteAddr = host, "203.0.113.5:4000"
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for range 5 {
		if got := login("cross-site"); got != http.StatusForbidden {
			t.Fatalf("a cross-site start = %d, want the guard's 403", got)
		}
	}
	if got := login("same-origin"); got != http.StatusSeeOther {
		t.Errorf("a start after the guard refused five = %d, want it admitted on the one token and the one place", got)
	}
}

// One IPv4 /24 or IPv6 /48 starts 300 sign-ins a minute however many
// addresses it spreads them over, and polls 1,200 times; another
// network has its own bucket, and a refusal names the network.
//
// @decision(DL-133)
func TestOneNetworkStartsThreeHundredSignInsAMinute(t *testing.T) {
	clock := newLimitClock()
	f := newDeviceFixtureWith(t, "alice@example.com\n", func(d *Deps) {
		d.Limits = limitsWith(clock, nil)
	})
	send := deviceHit(f)
	for i := range 300 {
		if rec := send(http.MethodGet, LoginPath, fmt.Sprintf("203.0.113.%d:4000", i/30), nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("start %d from the /24 = %d", i+1, rec.Code)
		}
	}
	refused := send(http.MethodGet, LoginPath, "203.0.113.200:4000", nil)
	mustRefuse(t, refused, http.StatusTooManyRequests, "the /24's 301st start from a fresh address")
	if !strings.Contains(refused.Body.String(), "from your network") {
		t.Errorf("the refusal = %s, want it to name the network", refused.Body)
	}
	if rec := send(http.MethodGet, LoginPath, "203.0.114.1:4000", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("the next /24 = %d, want its own bucket", rec.Code)
	}
	// The network's refusals gave the address's tokens back, so after
	// a second only the network refuses that address.
	clock.advance(time.Second)
	for range 30 {
		rec := send(http.MethodGet, LoginPath, "203.0.113.200:4000", nil)
		if rec.Code != http.StatusSeeOther && !strings.Contains(rec.Body.String(), "from your network") {
			t.Fatalf("a start after a second = %d %s, want only the network to refuse", rec.Code, rec.Body)
		}
	}

	form := url.Values{"grant_type": {DeviceGrantType}, "device_code": {"unknown"}, "client_id": {DeviceClientID}}.Encode()
	for i := range 1_200 {
		if rec := send(http.MethodPost, deviceTokenPath, v6In48(i/120), strings.NewReader(form)); rec.Code != http.StatusBadRequest {
			t.Fatalf("poll %d from the /48 = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	poll := send(http.MethodPost, deviceTokenPath, v6In48(500), strings.NewReader(form))
	mustRefuse(t, poll, http.StatusTooManyRequests, "the /48's 1,201st poll")
	var body oauthBody
	if err := json.Unmarshal(poll.Body.Bytes(), &body); err != nil || body.Error != "slow_down" || !strings.Contains(body.Description, "network") {
		t.Errorf("the refused poll = %s, want slow_down naming the network", poll.Body)
	}
	if rec := send(http.MethodPost, deviceTokenPath, "[2001:db8:8::1]:4000", strings.NewReader(form)); rec.Code != http.StatusBadRequest {
		t.Errorf("another /48's poll = %d, want it served", rec.Code)
	}
}

// One network holds at most its share of each cap; past it that
// network gets a 429 with its own wait, and the replica goes on
// admitting other networks.
//
// @decision(DL-133)
func TestOneNetworkHoldsAtMostItsShareOfACap(t *testing.T) {
	clock := newLimitClock()
	r := telemetrytest.New(t)
	f := newDeviceFixtureWith(t, "alice@example.com\n", func(d *Deps) {
		d.Limits = limitsWith(clock, func(l *Limits) { l.PendingLogins, l.PendingDeviceCode, l.PendingPerNetwork = 10, 10, 2 })
		d.Metrics = r.Metrics
	})
	send := deviceHit(f)
	for i := range 2 {
		if rec := send(http.MethodGet, LoginPath, v6In48(i), nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("login %d = %d", i+1, rec.Code)
		}
		clock.advance(time.Minute)
	}
	refused := send(http.MethodGet, LoginPath, v6In48(9), nil)
	mustRefuse(t, refused, http.StatusTooManyRequests, "a third login from the /48")
	if got, want := refused.Header().Get("Retry-After"), strconv.Itoa(int((FlowLifetime - 2*time.Minute).Seconds())); got != want {
		t.Errorf("Retry-After = %s, want the %s s until the network's oldest start leaves the window", got, want)
	}
	if rec := send(http.MethodGet, LoginPath, "198.51.100.9:4000", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("another network's login = %d, want the cap to have room", rec.Code)
	}
	for i := range 2 {
		if rec := send(http.MethodPost, DeviceCodePath, fmt.Sprintf("192.0.2.%d:4000", i+1), strings.NewReader("client_id=spov-cli")); rec.Code != http.StatusOK {
			t.Fatalf("device code %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	device := send(http.MethodPost, DeviceCodePath, "192.0.2.77:4000", strings.NewReader("client_id=spov-cli"))
	mustRefuse(t, device, http.StatusTooManyRequests, "a third device code from the /24")
	var body oauthBody
	if err := json.Unmarshal(device.Body.Bytes(), &body); err != nil || body.Error != "slow_down" {
		t.Errorf("the device refusal = %s, want slow_down", device.Body)
	}
	clock.advance(FlowLifetime - 2*time.Minute)
	if rec := send(http.MethodGet, LoginPath, v6In48(9), nil); rec.Code != http.StatusSeeOther {
		t.Errorf("a login once the network's oldest left the window = %d", rec.Code)
	}
	if r.Count("spov_requests_limited", "limit", "sign_in_share") != 1 || r.Count("spov_requests_limited", "limit", "device_code_share") != 1 {
		t.Errorf("share refusals were not recorded by limit")
	}
}

// Many networks, each inside its share, still fill the replica's cap,
// and then every start is the 503.
//
// @decision(DL-133)
func TestManyNetworksStillFillTheCap(t *testing.T) {
	f := newDeviceFixtureWith(t, "alice@example.com\n", func(d *Deps) {
		d.Limits = limitsWith(newLimitClock(), func(l *Limits) { l.PendingLogins, l.PendingPerNetwork = 6, 2 })
	})
	send := deviceHit(f)
	for i := range 6 {
		if rec := send(http.MethodGet, LoginPath, fmt.Sprintf("203.0.%d.1:4000", i/2), nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("login %d = %d", i+1, rec.Code)
		}
	}
	mustRefuse(t, send(http.MethodGet, LoginPath, "198.51.100.9:4000", nil), http.StatusServiceUnavailable, "a fresh network's login once the cap is full")
}

// A window forgets a network once its last start leaves, so the map of
// networks is bounded by the starts in the window.
//
// @decision(DL-133)
func TestAWindowForgetsANetworkWhoseStartsHaveLeft(t *testing.T) {
	w := newWindow(4, 2, time.Minute)
	t0 := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	for i, n := range []string{"a", "a", "b"} {
		if ok, _, _ := w.admit(n, t0.Add(time.Duration(i)*time.Second)); !ok {
			t.Fatalf("start %d refused", i+1)
		}
	}
	if ok, wait, share := w.admit("a", t0.Add(3*time.Second)); ok || !share || wait != 57*time.Second {
		t.Errorf("a third start from a = %v, %s, share %v; want the share's refusal until its oldest leaves", ok, wait, share)
	}
	if ok, _, _ := w.admit("c", t0.Add(60*time.Second+500*time.Millisecond)); !ok {
		t.Fatal("a start after the first left was refused")
	}
	if got := w.networks(); got != 3 {
		t.Errorf("networks = %d, want a, b and c", got)
	}
	w.admit("d", t0.Add(10*time.Minute))
	if got := w.networks(); got != 1 || len(w.at) != 1 {
		t.Errorf("networks %d and starts %d after the window passed, want only the new one", got, len(w.at))
	}
}
