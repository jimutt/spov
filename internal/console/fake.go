package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
)

// Fake is an in-process console for tests. It enforces what the spike
// measured: a refresh rotates both tokens and ends the previous access
// token, and sending a replaced refresh token revokes the whole grant.
//
// @decision(DL-015)
type Fake struct {
	*httptest.Server

	mu       sync.Mutex
	n        int
	devices  map[string]*fakeDevice
	families map[string]*fakeFamily // by every refresh token ever issued
	// Refreshes counts refresh calls, including refused ones.
	Refreshes int
	// UserAgents are the User-Agent headers seen, in order.
	UserAgents []string
	// ExpiresIn is the lifetime of issued access tokens, in seconds.
	ExpiresIn int
	// PollError, when set, is answered to the next device-token poll.
	PollError string
}

type fakeDevice struct {
	userCode string
	approved bool
	denied   bool
	used     bool
}

type fakeFamily struct {
	refresh string // the current refresh token
	access  string // the current access token
	revoked bool
}

// NewFake starts a fake console. Close it when done.
func NewFake() *Fake {
	f := &Fake{devices: map[string]*fakeDevice{}, families: map[string]*fakeFamily{}, ExpiresIn: 30 * 24 * 3600}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/device/code", f.code)
	mux.HandleFunc("POST /auth/device/token", f.token)
	f.Server = httptest.NewServer(mux)
	return f
}

// Approve approves the sign-in showing userCode.
func (f *Fake) Approve(userCode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.devices {
		if d.userCode == userCode {
			d.approved = true
		}
	}
}

// Deny denies the sign-in showing userCode.
func (f *Fake) Deny(userCode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.devices {
		if d.userCode == userCode {
			d.denied = true
		}
	}
}

// Valid reports whether an access token currently works.
func (f *Fake) Valid(access string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fam := range f.families {
		if fam.access == access && !fam.revoked {
			return true
		}
	}
	return false
}

func (f *Fake) next(prefix string) string {
	f.n++
	return fmt.Sprintf("%s%04d", prefix, f.n)
}

func (f *Fake) code(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.UserAgents = append(f.UserAgents, r.UserAgent())
	var in struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.ClientID != ClientID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client"})
		return
	}
	dc, uc := f.next("dc_"), f.next("UC-")
	f.devices[dc] = &fakeDevice{userCode: uc}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code": dc, "user_code": uc,
		"verification_uri_complete": "/console/device?user_code=" + uc,
		"expires_in":                600, "interval": 5,
	})
}

func (f *Fake) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.UserAgents = append(f.UserAgents, r.UserAgent())
	var in struct {
		GrantType    string `json:"grant_type"`
		DeviceCode   string `json:"device_code"`
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	switch in.GrantType {
	case deviceGrantType:
		if f.PollError != "" {
			code := f.PollError
			f.PollError = ""
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": code})
			return
		}
		d := f.devices[in.DeviceCode]
		switch {
		case d == nil || d.used:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		case d.denied:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "access_denied"})
		case !d.approved:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "authorization_pending"})
		default:
			d.used = true
			f.issue(w, &fakeFamily{})
		}
	case "refresh_token":
		f.Refreshes++
		fam := f.families[in.RefreshToken]
		if fam == nil || fam.revoked {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		if fam.refresh != in.RefreshToken {
			fam.revoked = true
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		f.issue(w, fam)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
	}
}

// issue gives a family new tokens. The caller holds f.mu.
func (f *Fake) issue(w http.ResponseWriter, fam *fakeFamily) {
	fam.access, fam.refresh = f.next("st_"), f.next("rt_")
	f.families[fam.refresh] = fam
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": fam.access, "refresh_token": fam.refresh,
		"expires_in": f.ExpiresIn, "token_type": "bearer", "org_id": "org_1",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
