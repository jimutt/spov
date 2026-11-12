package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// AllowlistEntryView is one address that may log in.
//
// @decision(DL-007)
type AllowlistEntryView struct {
	Email string `json:"email" doc:"The address, lower-cased"`
	Admin bool   `json:"admin" doc:"Whether this address administers the org"`
	// CreatedAt is when the address was first allowed, not when the
	// entry was last changed.
	CreatedAt time.Time `json:"created_at"`
}

type allowlistOutput struct {
	Body struct {
		Entries []AllowlistEntryView `json:"entries"`
	}
}

type putAllowlistInput struct {
	Email string `path:"email" doc:"The address to allow"`
	Body  struct {
		Admin bool `json:"admin" doc:"Whether this address administers the org"`
	}
}

type allowlistEntryOutput struct {
	Body AllowlistEntryView
}

type deleteAllowlistInput struct {
	Email string `path:"email" doc:"The address to remove"`
}

// registerAllowlist serves the admin API for who may log in. Every
// operation needs an administrator, and none may leave the org without
// one.
//
// @decision(DL-007)
func (s *Server) registerAllowlist() {
	register(s.api, huma.Operation{
		OperationID: "list-allowlist", Method: http.MethodGet, Path: "/api/v1/allowlist",
		Summary: "The addresses that may log in", Tags: []string{"admin"},
	}, func(ctx context.Context, _ *struct{}) (*allowlistOutput, error) {
		p := principal(ctx)
		if err := s.requireAllowlist(p); err != nil {
			return nil, err
		}
		entries, err := s.deps.Store.Allowlist().List(ctx, p.OrgID)
		if err != nil {
			return nil, toHumaError(err)
		}
		out := &allowlistOutput{}
		out.Body.Entries = make([]AllowlistEntryView, 0, len(entries))
		for _, e := range entries {
			out.Body.Entries = append(out.Body.Entries, allowlistView(e))
		}
		return out, nil
	})

	register(s.api, huma.Operation{
		OperationID: "put-allowlist-entry", Method: http.MethodPut, Path: "/api/v1/allowlist/{email}",
		Summary: "Allow an address to log in, or change its admin rights", Tags: []string{"admin"},
		Errors: []int{http.StatusUnprocessableEntity},
	}, func(ctx context.Context, in *putAllowlistInput) (*allowlistEntryOutput, error) {
		p := principal(ctx)
		if err := s.requireAllowlist(p); err != nil {
			return nil, err
		}
		email, err := auth.ParseAllowlistEmail(in.Email)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		entry := store.AllowlistEntry{OrgID: p.OrgID, Email: email, Admin: in.Body.Admin}
		var stored store.AllowlistEntry
		err = s.deps.Store.Tx(ctx, func(tx store.Store) error {
			al := tx.Allowlist()
			// Count before reading the entry: CountAdmins takes the org's
			// advisory lock, which must also cover the read of cur. Do
			// not move it into the guard; see DL-007.
			//
			// @decision(DL-007)
			admins, err := al.CountAdmins(ctx, p.OrgID)
			if err != nil {
				return err
			}
			if err := requireLiveAdmin(ctx, al, p); err != nil {
				return err
			}
			cur, err := al.Get(ctx, p.OrgID, email)
			switch {
			case errors.Is(err, store.ErrNotFound):
				// A new address; there is nothing to demote.
			case err != nil:
				return err
			case cur.Admin && !in.Body.Admin:
				if err := guardLastAdmin(p, email, admins, "remove your own admin rights"); err != nil {
					return err
				}
			}
			if err := al.Put(ctx, entry); err != nil {
				return err
			}
			// Read back inside the transaction: created_at is the
			// store's, kept from the existing row, and a read after
			// the commit would report someone else's later change as
			// the result of this call.
			stored, err = al.Get(ctx, p.OrgID, email)
			return err
		})
		if err != nil {
			return nil, toHumaError(err)
		}
		s.allowlistChanged(ctx)
		return &allowlistEntryOutput{Body: allowlistView(stored)}, nil
	})

	register(s.api, huma.Operation{
		OperationID: "delete-allowlist-entry", Method: http.MethodDelete, Path: "/api/v1/allowlist/{email}",
		Summary: "Stop an address logging in, and end what it has running and stored",
		Description: "Removes the entry, ends the person's browser sessions, cancels their unfinished jobs," +
			" stops their live sessions (a held sync job's included), revokes their CLI tokens and deletes" +
			" their stored credentials, then writes an audit row. Their history is kept. Deleting an" +
			" address that is no longer on the allowlist, but has signed in, runs those steps again.",
		Tags:   []string{"admin"},
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}, func(ctx context.Context, in *deleteAllowlistInput) (*struct{}, error) {
		p := principal(ctx)
		if err := s.requireAllowlist(p); err != nil {
			return nil, err
		}
		email, err := auth.ParseAllowlistEmail(in.Email)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		var user *store.User
		err = s.deps.Store.Tx(ctx, func(tx store.Store) error {
			al := tx.Allowlist()
			// Count before reading the entry: CountAdmins takes the org's
			// advisory lock, which must also cover the read of cur. Do
			// not move it into the guard; see DL-007.
			//
			// @decision(DL-007)
			admins, err := al.CountAdmins(ctx, p.OrgID)
			if err != nil {
				return err
			}
			if err := requireLiveAdmin(ctx, al, p); err != nil {
				return err
			}
			cur, err := al.Get(ctx, p.OrgID, email)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			// A person who never signed in has no user row, and so
			// nothing to end.
			//
			// @decision(DL-132)
			u, uerr := tx.Users().ByEmail(ctx, p.OrgID, email)
			switch {
			case uerr == nil:
				user = &u
			case !errors.Is(uerr, store.ErrNotFound):
				return uerr
			}
			if errors.Is(err, store.ErrNotFound) {
				// Already removed: a person who signed in is off-boarded
				// again, which is how a partial removal is retried.
				//
				// @decision(DL-132)
				if user != nil {
					return nil
				}
				// Not "no such session", which is what the shared
				// mapper says for a missing row.
				return huma.Error404NotFound(email + " is not on the allowlist")
			}
			if cur.Admin {
				if err := guardLastAdmin(p, email, admins, "remove yourself"); err != nil {
					return err
				}
			}
			return al.Delete(ctx, p.OrgID, email)
		})
		if err != nil {
			return nil, toHumaError(err)
		}
		s.allowlistChanged(ctx)
		// The per-request check already locks them out; this deletes
		// their web sessions too, so a re-added address does not resume
		// an old one.
		//
		// @decision(DL-007)
		if s.revokeSessions != nil {
			if err := s.revokeSessions(ctx, p.OrgID, email); err != nil {
				// The entry is gone, which is what was asked for. A
				// session that outlived it is inert within one refresh
				// interval, so this is a warning, not a failed delete.
				s.deps.Warn("allowlist: revoking sessions for " + email + ": " + err.Error())
			}
		}
		// After the commit, so the person is locked out before anything
		// of theirs is ended. Each step runs even when one fails.
		//
		// @decision(DL-132)
		if user != nil {
			if _, err := s.svc.Offboard(ctx, p, email, *user); err != nil {
				return nil, huma.Error500InternalServerError(err.Error())
			}
		}
		return nil, nil
	})
}

// requireAllowlist refuses a caller who is not an administrator, a
// server with no login (and so no allowlist, as in local mode), and a
// caller outside the org the allowlist governs, such as the shared
// bearer token's "local" org.
//
// @decision(DL-007)
func (s *Server) requireAllowlist(p auth.Principal) error {
	if err := s.requireAdmin(p); err != nil {
		return err
	}
	if s.deps.Login == nil {
		return huma.Error501NotImplemented(
			"this server has no allowlist: it has no browser login, so there is nobody to allow")
	}
	if p.OrgID != s.deps.Login.OrgID {
		return huma.Error403Forbidden(
			"this credential is not in the org the allowlist governs, so it could only edit an allowlist" +
				" nothing reads; use a browser session or a token from `spov login`")
	}
	return nil
}

// requireLiveAdmin refuses a caller whose own entry no longer grants
// admin, reading the store rather than the cached Principal.Admin. It
// must run inside the caller's transaction, after CountAdmins has taken
// the org's lock.
//
// @decision(DL-007)
func requireLiveAdmin(ctx context.Context, al store.AllowlistStore, p auth.Principal) error {
	me, err := al.Get(ctx, p.OrgID, p.Email)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return err
	case me.Admin:
		return nil
	}
	return huma.Error403Forbidden(
		"your administrator rights have been removed; this credential is still cached as an admin" +
			" but the allowlist is not")
}

// guardLastAdmin refuses a change to the caller's own admin rights, and
// any change that would leave the org with no admin. admins is the count
// read at the top of the caller's transaction. The zero-admin rule is
// unreachable through the handlers today and is kept on purpose; it is
// tested directly.
//
// @decision(DL-007)
func guardLastAdmin(p auth.Principal, email string, admins int, what string) error {
	if sameEmail(email, p.Email) {
		return huma.Error422UnprocessableEntity("you cannot " + what + "; ask another admin")
	}
	if admins <= 1 {
		return huma.Error422UnprocessableEntity(
			"this is the org's last administrator; grant admin to someone else first")
	}
	return nil
}

// allowlistChanged refreshes this replica's cached copy, so the admin
// who made the change sees it on their next request. Other replicas
// catch up on their own refresh timer.
//
// @decision(DL-007)
func (s *Server) allowlistChanged(ctx context.Context) {
	if s.deps.Login == nil || s.deps.Login.Allowlist == nil {
		return
	}
	// Refresh has already said why and kept the previous list.
	_ = s.deps.Login.Allowlist.Refresh(ctx)
}

func sameEmail(a, b string) bool {
	return a != "" && strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func allowlistView(e store.AllowlistEntry) AllowlistEntryView {
	return AllowlistEntryView{Email: e.Email, Admin: e.Admin, CreatedAt: e.CreatedAt}
}
