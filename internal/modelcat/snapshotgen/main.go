// Command snapshotgen writes the model list spov ships in its binary
// (internal/modelcat/snapshot.json) from models.dev's document, read from
// a file or fetched. scripts/model-snapshot.sh runs it.
//
// @decision(DL-013)
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jimutt/spov/internal/modelcat"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run reads the document at args[0] and writes the snapshot to args[1].
func run(args []string, out io.Writer, now time.Time) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: snapshotgen <models.dev file or URL> <snapshot.json>")
	}
	raw, err := read(args[0])
	if err != nil {
		return err
	}
	l, err := modelcat.Parse(raw)
	if err != nil {
		return err
	}
	if len(l.Providers) == 0 {
		return fmt.Errorf("%s lists no model of the providers in prep.Catalogue", args[0])
	}
	// The day, not the moment, so a rerun on the same day changes nothing.
	l.AsOf = now.UTC().Truncate(24 * time.Hour)
	doc, err := json.MarshalIndent(l, "", " ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(args[1], append(doc, '\n'), 0o644); err != nil {
		return err
	}
	n := 0
	for _, ms := range l.Providers {
		n += len(ms)
	}
	_, err = fmt.Fprintf(out, "wrote %s: %d models from %d providers\n", args[1], n, len(l.Providers))
	return err
}

func read(src string) ([]byte, error) {
	if strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "http://") {
		resp, err := http.Get(src) //nolint:gosec,noctx // a developer's command with a URL they typed
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", src, resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
	return os.ReadFile(src)
}
