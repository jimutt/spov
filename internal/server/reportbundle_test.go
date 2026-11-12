package server

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jimutt/spov/internal/auth"
	"github.com/jimutt/spov/internal/store"
)

// TestAReportBundleRefusesANameAnExtractorCouldReadAsAPath: the entry
// names of a bundle are stored paths, and one with a separator, a drive,
// a dot segment or a control character is not written.
//
// @decision(DL-135)
func TestAReportBundleRefusesANameAnExtractorCouldReadAsAPath(t *testing.T) {
	f := newAPIFixture(t)
	p := auth.Principal{UserID: "u", OrgID: auth.LocalID}
	good := store.JobFile{Path: "report.md", Body: []byte("# r"), CreatedAt: time.Now()}

	var buf bytes.Buffer
	if err := f.srv.svc.WriteReportBundle(context.Background(), p, "job", good, nil, &buf); err != nil {
		t.Fatalf("a plain report: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil || len(zr.File) != 1 || zr.File[0].Name != "report.md" {
		t.Fatalf("bundle = %v, %v", zr, err)
	}

	for _, name := range []string{`..\..\x.md`, "../x.md", "/abs.md", "c:x.md", "a\x07.md", "a//b.md", "images/../x.md"} {
		bad := store.JobFile{Path: name, Body: []byte("x")}
		err := f.srv.svc.WriteReportBundle(context.Background(), p, "job", bad, nil, &bytes.Buffer{})
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("a report named %q was bundled: %v", name, err)
		}
	}
}
