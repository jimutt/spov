//go:build integration

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// Spike S-D1 measures what collecting several reports and their images
// needs from OpenCode's file routes: GET /api/fs/list on the out
// directory (entry paths, types, subdirectories, symlinks, dotfiles), and
// GET /api/fs/read on binary files and on a file over 1 MiB. Run on an
// OpenCode version bump.
//
// It creates a billable sandbox, runs no agent turn and needs no model
// key: files are written with the sandbox's own exec.
//
//	go test -tags integration ./internal/sandbox/ -run SD1 -v
//
// @decision(DL-051) @decision(DL-063)
func TestIntegrationSD1ReportFilesShape(t *testing.T) {
	p := newIntegrationProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	t.Logf("opencode build under test: %s (image %s)", testOpenCodeVersion(), testImage())
	sb, creds, password := startOpenCodeSandboxWith(t, ctx, p, "spov-sd1", 8*time.Minute, nil)
	client := &http.Client{Timeout: 120 * time.Second}
	base := creds.URL

	const out = "/workspace/out"
	// Every byte value, then pseudo-random bytes: a reader that treats the
	// body as UTF-8 text changes the hash.
	execInSandbox(t, ctx, sb, fmt.Sprintf(`set -e
mkdir -p %[1]s/images/nested %[1]s/empty
printf '# First\n\nbody\n' > %[1]s/a.md
printf 'no heading\n' > %[1]s/b.md
printf 'hidden\n' > %[1]s/.hidden.md
printf 'x' > %[1]s/images/nested/deep.png
ln -s /etc/hostname %[1]s/link.md
ln -s /etc %[1]s/linkdir
python3 -c "import sys; sys.stdout.buffer.write(bytes(range(256))*64)" > %[1]s/images/all.bin 2>/dev/null || awk 'BEGIN{for(r=0;r<64;r++)for(i=0;i<256;i++)printf "%%c", i}' > %[1]s/images/all.bin
head -c 3000000 /dev/urandom > %[1]s/images/random.bin
head -c 12000000 /dev/urandom > %[1]s/big.md
`, out))

	sums := map[string]string{}
	for _, f := range []string{"images/all.bin", "images/random.bin", "big.md"} {
		res, err := (&modalHandle{sb: sb}).Exec(ctx, []string{"sh", "-c",
			fmt.Sprintf("sha256sum %s/%s | cut -d' ' -f1; stat -c %%s %s/%s", out, f, out, f)})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("hash %s: %v %s", f, err, res.Stderr)
		}
		sums[f] = res.Stdout
		t.Logf("%s in the sandbox: %s", f, res.Stdout)
	}

	read := func(t *testing.T, rel string) (int, []byte, http.Header) {
		t.Helper()
		u := fmt.Sprintf("%s/api/fs/read/%s?%s", base, rel,
			url.Values{"location[directory]": {out}}.Encode())
		resp, err := client.Do(sandboxHTTP(ctx, t, http.MethodGet, u, creds.Token, password))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			t.Fatalf("read %s body: %v", rel, err)
		}
		return resp.StatusCode, body, resp.Header
	}

	t.Run("binary files come back byte for byte", func(t *testing.T) {
		for _, f := range []string{"images/all.bin", "images/random.bin"} {
			code, body, h := read(t, f)
			sum := sha256.Sum256(body)
			got := hex.EncodeToString(sum[:])
			t.Logf("%s -> %d, %d bytes, Content-Type %q, sha256 %s", f, code, len(body), h.Get("Content-Type"), got)
			if code != http.StatusOK || got != firstLine(sums[f]) {
				t.Errorf("%s did not come back unchanged: want %s", f, firstLine(sums[f]))
			}
		}
	})

	t.Run("a file over 1 MiB comes back whole", func(t *testing.T) {
		start := time.Now()
		code, body, _ := read(t, "big.md")
		sum := sha256.Sum256(body)
		t.Logf("big.md -> %d, %d bytes in %s", code, len(body), time.Since(start))
		if hex.EncodeToString(sum[:]) != firstLine(sums["big.md"]) {
			t.Errorf("a 12 MB file did not come back whole")
		}
	})

	t.Run("what the listing says", func(t *testing.T) {
		for _, dir := range []string{out, out + "/images"} {
			u := fmt.Sprintf("%s/api/fs/list?%s", base, url.Values{"path": {dir}}.Encode())
			resp, err := client.Do(sandboxHTTP(ctx, t, http.MethodGet, u, creds.Token, password))
			if err != nil {
				t.Fatalf("list %s: %v", dir, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Logf("list path=%s -> %d: %s", dir, resp.StatusCode, truncate(string(body), 1500))
			var parsed struct {
				Data []struct {
					Path string `json:"path"`
					Type string `json:"type"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &parsed); err != nil {
				t.Errorf("listing did not parse: %v", err)
			}
		}
		// The form delivery sends: location[directory]=out and no path.
		// MEASURED 2026-10-15 on 2.0.22: entry paths are relative to the
		// location, a directory ends in "/", dotfiles are listed, and a
		// symlink is left out of the listing altogether.
		u := fmt.Sprintf("%s/api/fs/list?%s", base, url.Values{"location[directory]": {out}}.Encode())
		resp, err := client.Do(sandboxHTTP(ctx, t, http.MethodGet, u, creds.Token, password))
		if err != nil {
			t.Fatalf("list with location: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Logf("list location[directory]=%s -> %d: %s", out, resp.StatusCode, truncate(string(body), 1500))
		var listed struct {
			Data []struct {
				Path string `json:"path"`
				Type string `json:"type"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &listed); err != nil {
			t.Fatalf("listing did not parse: %v", err)
		}
		got := map[string]string{}
		for _, e := range listed.Data {
			got[e.Path] = e.Type
		}
		for path, typ := range map[string]string{"a.md": "file", "b.md": "file", "big.md": "file", "images/": "directory"} {
			if got[path] != typ {
				t.Errorf("entry %q: want type %q, listing has %q", path, typ, got[path])
			}
		}
		for _, path := range []string{"link.md", "linkdir", "linkdir/"} {
			if _, ok := got[path]; ok {
				t.Logf("a symlink (%s) is now listed; collection skips anything that does not read as a plain file", path)
			}
		}

		u = fmt.Sprintf("%s/api/fs/list?%s", base, url.Values{"path": {out + "/missing"}}.Encode())
		resp, err = client.Do(sandboxHTTP(ctx, t, http.MethodGet, u, creds.Token, password))
		if err != nil {
			t.Fatalf("list missing: %v", err)
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Logf("list of a missing directory -> %d: %s", resp.StatusCode, truncate(string(body), 300))
	})

	t.Run("what a symlink reads as", func(t *testing.T) {
		code, body, _ := read(t, "link.md")
		t.Logf("link.md (to /etc/hostname) -> %d: %s", code, truncate(string(body), 100))
	})
}
