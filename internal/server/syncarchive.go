package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jimutt/spov/internal/redact"
	"github.com/jimutt/spov/internal/session"
)

// archiveResponse documents a sync archive's answer.
//
// @decision(DL-176)
var archiveResponse = map[string]*huma.Response{
	"200": {
		Description: "DL-176's archive: an uncompressed tar of spov-sync.json, any spov-sync-keepalive entries, state.bundle and worktree.tar.gz as the metadata declares them, and spov-sync-end.json",
		Content:     map[string]*huma.MediaType{"application/x-tar": {Schema: &huma.Schema{Type: "string", Format: "binary"}}},
	},
}

// registerSyncArchive adds the two routes that stream a sync-back.
//
// @decision(DL-176)
func (s *Server) registerSyncArchive() {
	register(s.api, huma.Operation{
		OperationID: "export-session-sync-archive", Method: http.MethodPost, Path: "/api/v1/sessions/{ref}/sync-archive",
		Summary:   "Stream a running single-repository session's git commits and working-tree changes, up to 1 GiB",
		Tags:      []string{"sessions"},
		Responses: archiveResponse,
	}, func(ctx context.Context, in *refInput) (*huma.StreamResponse, error) {
		t, err := s.svc.sessionSyncTarget(ctx, principal(ctx), in.Ref)
		if err != nil {
			return nil, toHumaError(err)
		}
		return &huma.StreamResponse{Body: func(hc huma.Context) { s.streamSyncArchive(ctx, hc, t, toHumaError) }}, nil
	})
	register(s.api, huma.Operation{
		OperationID: "export-job-sync-archive", Method: http.MethodPost, Path: "/api/v1/jobs/{id}/sync-archive",
		Summary:   "Stream a sync job's git commits and working-tree changes from its sandbox, up to 1 GiB",
		Tags:      []string{"jobs"},
		Errors:    []int{http.StatusConflict},
		Responses: archiveResponse,
	}, func(ctx context.Context, in *jobIDInput) (*huma.StreamResponse, error) {
		t, err := s.svc.jobSyncTarget(ctx, principal(ctx), in.ID)
		if err != nil {
			return nil, toJobHumaError(err)
		}
		return &huma.StreamResponse{Body: func(hc huma.Context) { s.streamSyncArchive(ctx, hc, t, toJobHumaError) }}, nil
	})
}

// streamSyncArchive runs the export script into the response. A
// failure before the first byte is an ordinary error answer; one after
// it aborts the response, so the client never reads a short archive as
// a whole one.
//
// @decision(DL-176)
func (s *Server) streamSyncArchive(ctx context.Context, hc huma.Context, t syncTarget, mapErr func(error) error) {
	w := &archiveWriter{hc: hc}
	res, err := session.StreamSync(ctx, t.h, t.dir, t.base, w)
	if err == nil && res.ExitCode != 0 {
		err = session.ExecFailure(res)
	}
	if err == nil && !w.started {
		err = errors.New("session: the export wrote nothing")
	}
	if err == nil {
		return
	}
	if !w.started {
		status, detail := http.StatusBadGateway, redact.Error(err)
		var se huma.StatusError
		if errors.As(mapErr(err), &se) {
			status, detail = se.GetStatus(), se.Error()
		}
		_ = huma.WriteErr(s.api, hc, status, detail)
		return
	}
	s.svc.deps.Warn("sync archive of " + t.what + " aborted after it started: " + redact.Error(err))
	panic(http.ErrAbortHandler)
}

// archiveWriter sends the status and headers with its first byte and
// flushes every write, so the client sees each part as it comes.
//
// @decision(DL-176)
type archiveWriter struct {
	hc      huma.Context
	started bool
}

func (a *archiveWriter) Write(p []byte) (int, error) {
	if !a.started {
		a.started = true
		a.hc.SetHeader("Content-Type", "application/x-tar")
		a.hc.SetHeader("Cache-Control", "no-store")
		a.hc.SetHeader("X-Content-Type-Options", "nosniff")
		a.hc.SetStatus(http.StatusOK)
	}
	n, err := a.hc.BodyWriter().Write(p)
	if f, ok := a.hc.BodyWriter().(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}
