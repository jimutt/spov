package modelcat

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	// Fresh is how long a fetched list is used as it is. An older one is
	// served while a refresh runs in the background.
	//
	// @decision(DL-013)
	Fresh = time.Hour
	// FailureBackoff is how long a failed fetch is not tried again.
	//
	// @decision(DL-013)
	FailureBackoff = 5 * time.Minute
	// DefaultURL is the mirror the pinned OpenCode reads.
	DefaultURL = "https://models.opencode.ai/api.json"
	// fetchTimeout bounds one fetch.
	fetchTimeout = 60 * time.Second
	// maxDocument bounds the document, decompressed. It was 5.8 MB on
	// 2026-10-19.
	maxDocument = 64 << 20
)

//go:embed snapshot.json
var snapshot []byte

// Snapshot is the list shipped in the binary, served until a fetch
// succeeds (scripts/model-snapshot.sh refreshes it).
//
// @decision(DL-013)
func Snapshot() List {
	var l List
	if err := json.Unmarshal(snapshot, &l); err != nil {
		panic("modelcat: the embedded snapshot is not a list: " + err.Error())
	}
	return l
}

// Persist keeps a fetched list beyond the process: a local server's
// state file.
//
// @decision(DL-013)
type Persist interface {
	LoadModels() (List, bool)
	SaveModels(List) error
}

// Source is the deployment's model list: the last fetched, else the
// persisted one, else the snapshot. Reading it never waits on the
// network; a stale or missing list starts a background refresh.
//
// @decision(DL-013)
type Source struct {
	// Client makes the fetch; nil is a client with no timeout of its
	// own, since the fetch carries one.
	Client *http.Client
	// URL is the document; empty is DefaultURL.
	URL     string
	Persist Persist
	Now     func() time.Time
	Warn    func(string)

	mu         sync.Mutex
	loaded     bool
	list       List
	live       bool
	refreshing bool
	failedAt   time.Time
	background sync.WaitGroup
}

// Models returns the list and whether it was fetched (true) or is the
// snapshot (false).
//
// @decision(DL-013)
func (s *Source) Models() (List, bool) {
	s.mu.Lock()
	s.load()
	l, live := s.list, s.live
	stale := !live || s.now().Sub(l.AsOf) >= Fresh
	start := stale && !s.refreshing && (s.failedAt.IsZero() || s.now().Sub(s.failedAt) >= FailureBackoff)
	if start {
		s.refreshing = true
		s.background.Add(1)
	}
	s.mu.Unlock()
	if start {
		go func() {
			defer s.background.Done()
			defer func() {
				s.mu.Lock()
				s.refreshing = false
				s.mu.Unlock()
			}()
			ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
			defer cancel()
			if err := s.Refresh(ctx); err != nil {
				s.warn(fmt.Sprintf("could not refresh the model list from %s, so the last one is kept: %v", s.url(), err))
			}
		}()
	}
	return l, live
}

// Wait waits for a background refresh, for a test or a shutdown.
func (s *Source) Wait() { s.background.Wait() }

// load reads the persisted list, else the snapshot, once. s.mu is held.
func (s *Source) load() {
	if s.loaded {
		return
	}
	s.loaded = true
	snap := Snapshot()
	// A persisted list older than this binary's snapshot is set aside:
	// the snapshot may carry a provider or a filter the old list lacks.
	if s.Persist != nil {
		if l, ok := s.Persist.LoadModels(); ok && len(l.Providers) > 0 && !l.AsOf.Before(snap.AsOf) {
			s.list, s.live = l, true
			return
		}
	}
	s.list = snap
}

// Refresh fetches the document, revalidating with the held ETag. A 304
// keeps the list and renews its age; a failure keeps it and starts the
// backoff.
//
// @decision(DL-013)
func (s *Source) Refresh(ctx context.Context) error {
	s.mu.Lock()
	s.load()
	etag := ""
	if s.live {
		etag = s.list.ETag
	}
	s.mu.Unlock()
	l, notModified, err := s.fetch(ctx, etag)
	s.mu.Lock()
	if err != nil {
		s.failedAt = s.now()
		s.mu.Unlock()
		return err
	}
	s.failedAt = time.Time{}
	if notModified {
		s.list.AsOf = s.now()
	} else {
		s.list = l
	}
	s.live = true
	saved := s.list
	s.mu.Unlock()
	// Written outside the lock, so a reader never waits on the disk.
	if s.Persist != nil {
		if err := s.Persist.SaveModels(saved); err != nil {
			s.warn(fmt.Sprintf("could not save the model list: %v", err))
		}
	}
	return nil
}

func (s *Source) fetch(ctx context.Context, etag string) (List, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url(), nil)
	if err != nil {
		return List{}, false, err
	}
	req.Header.Set("User-Agent", "spov")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return List{}, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNotModified:
		// Only an answer to a conditional request; a 304 to a plain one
		// would turn the snapshot into a "live" list.
		if etag == "" {
			return List{}, false, fmt.Errorf("%s answered 304 to a request that named no version", s.url())
		}
		return List{}, true, nil
	case http.StatusOK:
	default:
		return List{}, false, fmt.Errorf("%s answered %s", s.url(), resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument+1))
	if err != nil {
		return List{}, false, err
	}
	if len(raw) > maxDocument {
		return List{}, false, fmt.Errorf("the document is over %d bytes", maxDocument)
	}
	l, err := Parse(raw)
	if err != nil {
		return List{}, false, err
	}
	// A document that parsed but offers nothing is not a list to keep.
	if len(l.Providers) == 0 {
		return List{}, false, fmt.Errorf("the document lists no model of the providers this server carries")
	}
	l.AsOf = s.now()
	l.ETag = resp.Header.Get("ETag")
	return l, false, nil
}

func (s *Source) url() string {
	if s.URL != "" {
		return s.URL
	}
	return DefaultURL
}

func (s *Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Source) warn(msg string) {
	if s.Warn != nil {
		s.Warn(msg)
	}
}
