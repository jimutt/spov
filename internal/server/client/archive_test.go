package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAnUnservedRouteIsToldFromAMissingSession: an older server's mux
// answers an unknown path with a plain 404, a route of spov's with a
// problem document, and only the first means "ask the JSON route".
//
// @decision(DL-178)
func TestAnUnservedRouteIsToldFromAMissingSession(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/sessions/gone/sync-archive", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"status":404,"title":"Not Found","detail":"no such session"}`)
	})
	mux.HandleFunc("POST /api/v1/sessions/here/sync-archive", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = io.WriteString(w, "TAR")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, "t")

	if _, err := c.ExportJobSyncArchive(context.Background(), "j1"); !IsUnservedRoute(err) {
		t.Errorf("an unknown path = %v, want an unserved route", err)
	}
	_, err := c.ExportSessionSyncArchive(context.Background(), "gone")
	if !IsNotFound(err) || IsUnservedRoute(err) {
		t.Errorf("a missing session = %v, want a 404 of the server's own", err)
	}
	body, err := c.ExportSessionSyncArchive(context.Background(), "here")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	if b, _ := io.ReadAll(body); string(b) != "TAR" {
		t.Errorf("body = %q", b)
	}
}
