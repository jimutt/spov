package auth

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Middleware authenticates every request with a and stores the principal
// in the context. A request without a valid credential is answered 401
// as application/problem+json and never reaches next.
//
// warn receives one line per rejected credential; it names the remote
// address and the reason, never the credential. Nil discards.
//
// @decision(DL-006)
func Middleware(a Authenticator, warn func(string)) func(http.Handler) http.Handler {
	if warn == nil {
		warn = func(string) {}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := a.Authenticate(r)
			if err != nil {
				if !errors.Is(err, ErrUnauthenticated) {
					warn("auth: rejected a credential from " + r.RemoteAddr + ": " + err.Error())
				}
				unauthorized(w)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

// Optional authenticates when a credential is present and passes the
// request on either way; the handler decides, by asking From whether
// there is a principal. A credential that is present and wrong is
// still not a principal, so such a request reaches the handler as
// anonymous, the same as sending none.
//
// @decision(DL-006)
func Optional(a Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := a.Authenticate(r)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

// problem is the RFC 9457 body the middleware writes. It matches the
// shape the API's own errors use, so a client parses both the same way.
type problem struct {
	Type   string `json:"type,omitempty"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("WWW-Authenticate", `Bearer realm="spov"`)
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(problem{
		Title:  "Unauthorized",
		Status: http.StatusUnauthorized,
		Detail: "a valid credential is required",
	})
}
