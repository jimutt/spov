package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/git"
	"github.com/jimutt/spov/internal/opencode"
	"github.com/jimutt/spov/internal/session"
	"github.com/jimutt/spov/internal/store"
)

// syncToStore decodes the request's optional Sync field.
//
// @decision(DL-027)
func (r SessionRequest) syncToStore() (*store.SessionSync, error) {
	if r.Sync == nil {
		return nil, nil
	}
	var bundle []byte
	if r.Sync.Bundle != "" {
		var err error
		bundle, err = base64.StdEncoding.DecodeString(r.Sync.Bundle)
		if err != nil {
			return nil, fmt.Errorf("%w: sync bundle is not valid base64", ErrInvalidRequest)
		}
	}
	return &store.SessionSync{
		Branch:     strings.TrimSpace(r.Sync.Branch),
		BaseCommit: strings.ToLower(strings.TrimSpace(r.Sync.BaseCommit)),
		Bundle:     bundle,
	}, nil
}

type syncExportOutput struct {
	Body SyncExportView
}

func (s *Server) registerSyncExport() {
	register(s.api, huma.Operation{
		OperationID: "export-session-sync", Method: http.MethodPost, Path: "/api/v1/sessions/{ref}/sync-export",
		Summary: "Export a running single-repository session's git commits and working-tree changes", Tags: []string{"sessions"},
	}, func(ctx context.Context, in *refInput) (*syncExportOutput, error) {
		state, err := s.svc.ExportSync(ctx, principal(ctx), in.Ref)
		if err != nil {
			return nil, toHumaError(err)
		}
		out := &syncExportOutput{
			Body: SyncExportView{
				Branch:     state.Branch,
				BaseCommit: state.BaseCommit,
				HeadCommit: state.HeadCommit,
				Deleted:    state.Deleted,
			},
		}
		if len(state.Bundle) > 0 {
			out.Body.Bundle = base64.StdEncoding.EncodeToString(state.Bundle)
		}
		if len(state.WorktreeTar) > 0 {
			out.Body.WorktreeTar = base64.StdEncoding.EncodeToString(state.WorktreeTar)
		}
		return out, nil
	})
}

// ExportSync packages the git commits and uncommitted working-tree
// changes from a running single-repository session's sandbox.
//
// @decision(DL-027)
func (s *Service) ExportSync(ctx context.Context, p auth.Principal, ref string) (git.ExportedState, error) {
	sess, err := s.Get(ctx, p, ref)
	if err != nil {
		return git.ExportedState{}, err
	}
	if sess.State != store.StateRunning || sess.SandboxID == "" {
		return git.ExportedState{}, fmt.Errorf("%w (state %s)", ErrSessionNotRunning, sess.State)
	}
	owner, name, ok := singleSessionRepo(sess)
	if !ok {
		return git.ExportedState{}, fmt.Errorf("%w: sync is only supported for sessions with a single repository", ErrInvalidRequest)
	}
	if _, err := s.deps.Store.Jobs().BySession(ctx, sess.OrgID, sess.ID); err == nil {
		return git.ExportedState{}, fmt.Errorf("%w: sync is for interactive sessions, not autonomous jobs", ErrInvalidRequest)
	} else if !errors.Is(err, store.ErrNotFound) {
		return git.ExportedState{}, err
	}
	h, err := s.handle(ctx, sess)
	if err != nil {
		return git.ExportedState{}, err
	}
	dir := git.SingleRepoDir(owner, name)
	if sess.Request.Landing != "" {
		dir = sess.Request.Landing
	} else if rel := strings.TrimPrefix(sess.Landing, opencode.WorkspaceRoot+"/"); rel != "" && rel != sess.Landing {
		dir = rel
	}
	var baseCommit string
	if sess.Request.Sync != nil {
		baseCommit = sess.Request.Sync.BaseCommit
	}
	return session.ExportSync(ctx, h, dir, baseCommit)
}

func singleSessionRepo(sess store.Session) (owner, name string, ok bool) {
	if len(sess.Request.Repos) == 1 {
		return sess.Request.Repos[0].Owner, sess.Request.Repos[0].Name, true
	}
	if len(sess.Request.Repos) == 0 && len(sess.Clones) == 1 {
		o, n, err := git.SplitPath(sess.Clones[0])
		if err == nil {
			return o, n, true
		}
	}
	return "", "", false
}
