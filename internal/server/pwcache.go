package server

import (
	"container/list"
	"sync"
)

// DefaultPasswordCacheSize bounds how many session passwords one replica
// holds. A server that outlives thousands of sessions would otherwise
// accumulate one secret per session it ever served.
//
// @decision(DL-003)
const DefaultPasswordCacheSize = 1000

// passwordCache is a bounded LRU of session passwords, held in memory
// only. The sandbox is the durable holder (DL-025); an entry evicted
// here costs one exec to recover, never a lost session.
//
// @decision(DL-003)
type passwordCache struct {
	mu    sync.Mutex
	max   int
	order *list.List               // front is most recently used
	items map[string]*list.Element // session id -> element holding an entry
}

type pwEntry struct {
	id       string
	password string
}

func newPasswordCache(max int) *passwordCache {
	if max <= 0 {
		max = DefaultPasswordCacheSize
	}
	return &passwordCache{max: max, order: list.New(), items: map[string]*list.Element{}}
}

// get returns a session's password and marks it recently used.
func (c *passwordCache) get(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[id]
	if !ok {
		return "", false
	}
	c.order.MoveToFront(el)
	return el.Value.(*pwEntry).password, true
}

// put stores a password, evicting the least recently used entry when the
// cache is full. An empty password is not cached: it would satisfy a
// later lookup with a value that cannot authenticate.
func (c *passwordCache) put(id, password string) {
	if id == "" || password == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[id]; ok {
		el.Value.(*pwEntry).password = password
		c.order.MoveToFront(el)
		return
	}
	c.items[id] = c.order.PushFront(&pwEntry{id: id, password: password})
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		if oldest == nil {
			return
		}
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*pwEntry).id)
	}
}

// forget drops a session's password. Called when the session stops.
func (c *passwordCache) forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[id]; ok {
		c.order.Remove(el)
		delete(c.items, id)
	}
}

// clear drops everything, for shutdown.
func (c *passwordCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order.Init()
	clear(c.items)
}

// has reports whether a session's password is held, without marking it
// used. For tests of the paths that must drop it.
func (c *passwordCache) has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.items[id]
	return ok
}

// len reports how many passwords are held.
func (c *passwordCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
