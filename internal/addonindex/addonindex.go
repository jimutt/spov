// Package addonindex resolves a partial add-on version, such as node@22
// or postgres@14, to the exact release it names today, from mise's
// version index or an apt repository's Packages index. toolchain says
// where each add-on resolves and which answers it accepts; this package
// makes the calls and keeps the answers.
package addonindex

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jimutt/spov/internal/toolchain"
)

const (
	// Fresh is how long an answer is used as it is. An older one is used
	// while a lookup refreshes it in the background.
	//
	// @decision(DL-033)
	Fresh = time.Hour
	// FailureBackoff is how long a failed lookup with no answer to fall
	// back on is not tried again, so an unreachable index does not hold
	// up every launch for lookupTimeout.
	//
	// @decision(DL-033)
	FailureBackoff = 5 * time.Minute
	// DefaultMiseURL is mise's version index, one tool per path.
	DefaultMiseURL = "https://mise-versions.jdx.dev/"
	// lookupTimeout bounds one lookup.
	lookupTimeout = 20 * time.Second
	// maxIndex bounds what one index may hold, decompressed. The largest
	// measured, PGDG's, was 4.6 MB.
	maxIndex = 64 << 20
)

// Answer is a resolved version and when it was looked up.
type Answer struct {
	Version string    `json:"version"`
	At      time.Time `json:"at"`
}

// Persist keeps answers beyond the process: a local server's state file.
//
// @decision(DL-033)
type Persist interface {
	LoadAnswers() (map[string]Answer, error)
	SaveAnswers(map[string]Answer) error
}

// Resolver resolves selections, caching each answer per add-on and
// requested version. The zero value is ready to use, with answers held
// in memory.
//
// @decision(DL-033)
type Resolver struct {
	// Client makes the lookups; nil is a client with no timeout of its
	// own, since each lookup carries one.
	Client *http.Client
	// MiseURL is mise's index; empty is DefaultMiseURL.
	MiseURL string
	// Persist, when set, loads the answers once and saves them on every
	// change.
	Persist Persist
	Now     func() time.Time
	Warn    func(string)

	mu         sync.Mutex
	loaded     bool
	answers    map[string]Answer
	refreshing map[string]bool
	failed     map[string]time.Time
	background sync.WaitGroup
}

// Resolve returns sel with each partial version resolved to an exact
// release. A selection that needs no resolving, or has no answer and
// whose lookup fails, is returned unresolved. A launch never fails here.
//
// @decision(DL-033)
func (r *Resolver) Resolve(ctx context.Context, sel []toolchain.Selection) []toolchain.Selection {
	out := toolchain.Unresolved(sel)
	var wg sync.WaitGroup
	for i := range out {
		idx, ok := out[i].Index()
		if !ok {
			continue
		}
		wg.Go(func() { out[i] = r.resolveOne(ctx, out[i], idx) })
	}
	wg.Wait()
	return out
}

func (r *Resolver) resolveOne(ctx context.Context, s toolchain.Selection, idx toolchain.Index) toolchain.Selection {
	key := s.String()
	a, ok := r.cached(key)
	switch {
	case ok && r.now().Sub(a.At) < Fresh:
	case ok:
		r.refresh(s, idx)
	case r.backingOff(key):
		return s
	default:
		v, err := r.lookup(ctx, s, idx)
		if err != nil {
			r.markFailed(key)
			r.warn(fmt.Sprintf("could not resolve %s, so it is built as written: %v", key, err))
			return s
		}
		a = r.store(key, v)
	}
	got, err := s.Resolve(a.Version)
	if err != nil {
		// Only a tampered state file holds an answer lookup refused.
		r.warn(fmt.Sprintf("the cached answer for %s is not usable: %v", key, err))
		return s
	}
	return got
}

// refresh looks s up again in the background, once at a time per key. A
// failure keeps the old answer.
func (r *Resolver) refresh(s toolchain.Selection, idx toolchain.Index) {
	key := s.String()
	r.mu.Lock()
	if r.refreshing == nil {
		r.refreshing = map[string]bool{}
	}
	if r.refreshing[key] {
		r.mu.Unlock()
		return
	}
	r.refreshing[key] = true
	r.mu.Unlock()
	r.background.Go(func() {
		defer func() {
			r.mu.Lock()
			delete(r.refreshing, key)
			r.mu.Unlock()
		}()
		v, err := r.lookup(context.Background(), s, idx)
		if err != nil {
			r.warn(fmt.Sprintf("could not refresh %s, so the last answer stays in use: %v", key, err))
			return
		}
		r.store(key, v)
	})
}

// backingOff reports whether key's last lookup failed within
// FailureBackoff.
func (r *Resolver) backingOff(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.failed[key]
	return ok && r.now().Sub(at) < FailureBackoff
}

func (r *Resolver) markFailed(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed == nil {
		r.failed = map[string]time.Time{}
	}
	r.failed[key] = r.now()
}

func (r *Resolver) cached(key string) (Answer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.load()
	a, ok := r.answers[key]
	return a, ok
}

// store records v for key and saves the answers.
func (r *Resolver) store(key, v string) Answer {
	a := Answer{Version: v, At: r.now()}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.load()
	r.answers[key] = a
	delete(r.failed, key)
	if r.Persist != nil {
		if err := r.Persist.SaveAnswers(r.answers); err != nil {
			r.warn(fmt.Sprintf("could not save the resolved add-on versions: %v", err))
		}
	}
	return a
}

// load reads the persisted answers once. A missing or unreadable store
// starts empty. r.mu is held.
func (r *Resolver) load() {
	if r.loaded {
		return
	}
	r.loaded = true
	r.answers = map[string]Answer{}
	if r.Persist == nil {
		return
	}
	got, err := r.Persist.LoadAnswers()
	if err != nil {
		r.warn(fmt.Sprintf("could not read the resolved add-on versions, so they are looked up again: %v", err))
		return
	}
	for k, a := range got {
		r.answers[k] = a
	}
}

// lookup asks the index for s's newest release.
func (r *Resolver) lookup(ctx context.Context, s toolchain.Selection, idx toolchain.Index) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	var candidates []string
	var err error
	if idx.Mise != "" {
		candidates, err = r.miseVersions(ctx, idx)
	} else {
		candidates, err = r.aptVersions(ctx, idx)
	}
	if err != nil {
		return "", err
	}
	best := ""
	for _, c := range candidates {
		if got, err := s.Resolve(c); err != nil || got.Resolved == "" {
			continue
		}
		if best == "" || Newer(c, best) {
			best = c
		}
	}
	if best == "" {
		return "", fmt.Errorf("the index lists no release of %s", s)
	}
	return best, nil
}

// miseVersions reads mise's index for a tool: one version per line, each
// starting with idx.Prefix when there is one.
func (r *Resolver) miseVersions(ctx context.Context, idx toolchain.Index) ([]string, error) {
	base := r.MiseURL
	if base == "" {
		base = DefaultMiseURL
	}
	body, err := r.get(ctx, strings.TrimSuffix(base, "/")+"/"+idx.Mise)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	var out []string
	sc := bufio.NewScanner(io.LimitReader(body, maxIndex))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := strings.CutPrefix(line, idx.Prefix); ok && v != "" {
			out = append(out, v)
		}
	}
	return out, sc.Err()
}

// aptVersions reads a Packages index for the upstream versions of
// idx.Package: the version without its epoch or Debian revision.
func (r *Resolver) aptVersions(ctx context.Context, idx toolchain.Index) ([]string, error) {
	body, err := r.get(ctx, idx.Packages)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	var in io.Reader = body
	if strings.HasSuffix(idx.Packages, ".gz") {
		gz, err := gzip.NewReader(body)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", idx.Packages, err)
		}
		defer func() { _ = gz.Close() }()
		in = gz
	}
	return PackageVersions(io.LimitReader(in, maxIndex), idx.Package)
}

// PackageVersions reads a Packages index and returns the upstream
// version of every stanza for pkg.
func PackageVersions(in io.Reader, pkg string) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	name := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			name = ""
		case strings.HasPrefix(line, "Package: "):
			name = strings.TrimSpace(strings.TrimPrefix(line, "Package: "))
		case strings.HasPrefix(line, "Version: ") && name == pkg:
			out = append(out, upstream(strings.TrimSpace(strings.TrimPrefix(line, "Version: "))))
		}
	}
	return out, sc.Err()
}

// upstream strips a Debian version's epoch and revision:
// 6:7.4.6-1rl1~trixie1 is 7.4.6.
func upstream(v string) string {
	if _, rest, ok := strings.Cut(v, ":"); ok {
		v = rest
	}
	v, _, _ = strings.Cut(v, "-")
	return v
}

func (r *Resolver) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "spov")
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// Newer reports whether version a is newer than b, comparing their runs
// of digits as numbers in order: 21.0.12+101 is newer than 21.0.9+10.
func Newer(a, b string) bool {
	x, y := numbers(a), numbers(b)
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return len(x) > len(y)
}

func numbers(v string) []int {
	var out []int
	for f := range strings.FieldsFuncSeq(v, func(r rune) bool { return r < '0' || r > '9' }) {
		n, err := strconv.Atoi(f)
		if err != nil {
			n = int(^uint(0) >> 1)
		}
		out = append(out, n)
	}
	return out
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Resolver) warn(msg string) {
	if r.Warn != nil {
		r.Warn(msg)
	}
}
