package server

import (
	"container/list"
	"sync"
	"time"
)

const (
	// DefaultConnectionCacheSize bounds how many live connections one
	// replica holds. Smaller than DefaultPasswordCacheSize: an entry
	// here is a live unrevocable credential, not a password the sandbox
	// will hand back on request.
	//
	// @decision(DL-030)
	DefaultConnectionCacheSize = 256
	// ConnectionExpiryMargin is how long before its expiry a cached
	// connection stops being served, bounding the window in which a
	// short act (a job's open, pause, report or evidence read) is
	// handed a credential that expires mid-run. It does not bound a
	// long-held stream; the recorder's redial loop handles that.
	//
	// @decision(DL-030)
	ConnectionExpiryMargin = 5 * time.Minute
)

// connCache is a bounded LRU of live connections, held in memory only.
//
// @decision(DL-030) @decision(DL-003)
type connCache struct {
	mu    sync.Mutex
	max   int
	order *list.List               // front is most recently used
	items map[string]*list.Element // session id -> element holding an entry
}

type connEntry struct {
	id   string
	conn Connection
}

func newConnCache(max int) *connCache {
	if max <= 0 {
		max = DefaultConnectionCacheSize
	}
	return &connCache{max: max, order: list.New(), items: map[string]*list.Element{}}
}

// get returns a session's connection and marks it recently used.
//
// An entry within ConnectionExpiryMargin of its expiry is dropped
// rather than served, and dropped rather than merely skipped: what it
// holds is a credential, and one that can no longer be used has no
// reason to stay in memory.
func (c *connCache) get(id string, now time.Time) (Connection, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[id]
	if !ok {
		return Connection{}, false
	}
	e := el.Value.(*connEntry)
	if !e.conn.ExpiresAt.After(now.Add(ConnectionExpiryMargin)) {
		c.removeLocked(id)
		return Connection{}, false
	}
	c.order.MoveToFront(el)
	return e.conn, true
}

// put stores a connection, evicting the least recently used entry when
// the cache is full.
//
// A connection with no token, or with no expiry to judge it by, is not
// cached: the first cannot authenticate, and the second would be served
// forever by a get that has nothing to measure. Connect never returns
// either — it assumes AssumedTokenTTL when the token says nothing — so
// this is a guard on a shape reaching here from somewhere new.
func (c *connCache) put(id string, conn Connection) {
	if id == "" || conn.Token == "" || conn.ExpiresAt.IsZero() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[id]; ok {
		el.Value.(*connEntry).conn = conn
		c.order.MoveToFront(el)
		return
	}
	c.items[id] = c.order.PushFront(&connEntry{id: id, conn: conn})
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		if oldest == nil {
			return
		}
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*connEntry).id)
	}
}

// forget drops a session's connection. Called when the session stops.
func (c *connCache) forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(id)
}

// forgetIf drops a session's connection only if it is still the one the
// caller was refused, so two callers refused on the same stale entry
// cannot take turns discarding each other's fresh mint.
//
// @decision(DL-030)
func (c *connCache) forgetIf(id, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[id]
	if !ok || el.Value.(*connEntry).conn.Token != token {
		return
	}
	c.removeLocked(id)
}

// removeLocked drops an entry. The caller holds the lock.
func (c *connCache) removeLocked(id string) {
	if el, ok := c.items[id]; ok {
		c.order.Remove(el)
		delete(c.items, id)
	}
}

// clear drops everything, for shutdown.
func (c *connCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order.Init()
	clear(c.items)
}

// has reports whether a session's connection is held, without marking
// it used and without judging its expiry. For tests of the paths that
// must drop it.
func (c *connCache) has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.items[id]
	return ok
}

// len reports how many connections are held.
func (c *connCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
