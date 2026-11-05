package server

import (
	"fmt"
	"testing"
)

// TestPasswordCacheEvictsTheLeastRecentlyUsed: the cache is bounded so a
// replica that outlives thousands of sessions does not hold a secret per
// session it ever served.
//
// @decision(DL-003)
func TestPasswordCacheEvictsTheLeastRecentlyUsed(t *testing.T) {
	c := newPasswordCache(3)
	for _, id := range []string{"a", "b", "c"} {
		c.put(id, "pw-"+id)
	}
	if c.len() != 3 {
		t.Fatalf("len = %d, want 3", c.len())
	}
	// Touch a so b becomes the oldest.
	if pw, ok := c.get("a"); !ok || pw != "pw-a" {
		t.Fatalf("get(a) = %q, %v", pw, ok)
	}
	c.put("d", "pw-d")

	if c.len() != 3 {
		t.Errorf("len = %d after a fourth entry, want 3", c.len())
	}
	if _, ok := c.get("b"); ok {
		t.Error("the least recently used entry survived")
	}
	for _, id := range []string{"a", "c", "d"} {
		if _, ok := c.get(id); !ok {
			t.Errorf("%s was evicted", id)
		}
	}
}

// TestPasswordCacheDefaultsAndBounds.
//
// @decision(DL-003)
func TestPasswordCacheDefaultsAndBounds(t *testing.T) {
	for _, size := range []int{0, -1} {
		c := newPasswordCache(size)
		if c.max != DefaultPasswordCacheSize {
			t.Errorf("newPasswordCache(%d).max = %d, want %d", size, c.max, DefaultPasswordCacheSize)
		}
	}
	// The bound holds under many more entries than it has room for.
	c := newPasswordCache(10)
	for i := range 500 {
		c.put(fmt.Sprintf("s%d", i), "pw")
	}
	if c.len() != 10 {
		t.Errorf("len = %d after 500 puts into a cache of 10", c.len())
	}
}

// TestPasswordCacheRefusesEmptyValues: caching an empty password would
// satisfy a later lookup with a value that authenticates nowhere,
// turning a recoverable miss into a confusing proxy failure.
//
// @decision(DL-003)
func TestPasswordCacheRefusesEmptyValues(t *testing.T) {
	c := newPasswordCache(4)
	c.put("s1", "")
	c.put("", "pw")
	if c.len() != 0 {
		t.Errorf("len = %d, want 0", c.len())
	}
	if _, ok := c.get("s1"); ok {
		t.Error("an empty password was cached")
	}
}

// TestPasswordCacheOverwritesInPlace: a re-recovered password replaces
// the old one rather than adding a second entry for the same session.
//
// @decision(DL-003)
func TestPasswordCacheOverwritesInPlace(t *testing.T) {
	c := newPasswordCache(4)
	c.put("s1", "old")
	c.put("s1", "new")
	if c.len() != 1 {
		t.Errorf("len = %d, want 1", c.len())
	}
	if pw, _ := c.get("s1"); pw != "new" {
		t.Errorf("password = %q, want new", pw)
	}
}

// TestPasswordCacheForgetAndClear: a stopped session's password must not
// linger, and shutdown drops everything.
//
// @decision(DL-003)
func TestPasswordCacheForgetAndClear(t *testing.T) {
	c := newPasswordCache(4)
	c.put("s1", "pw")
	c.put("s2", "pw")
	if !c.has("s1") {
		t.Fatal("s1 was not cached")
	}
	c.forget("s1")
	if c.has("s1") {
		t.Error("a forgotten password is still held")
	}
	if !c.has("s2") {
		t.Error("forget dropped an unrelated session")
	}
	c.forget("nope") // must not panic
	c.clear()
	if c.len() != 0 || c.has("s2") {
		t.Error("clear left entries behind")
	}
	// The cache is usable again after a clear.
	c.put("s3", "pw")
	if !c.has("s3") {
		t.Error("the cache is unusable after clear")
	}
}

// TestPasswordCacheIsConcurrencySafe: several replicas' worth of
// requests hit one cache at once. Run with -race.
//
// @decision(DL-003)
func TestPasswordCacheIsConcurrencySafe(t *testing.T) {
	c := newPasswordCache(16)
	done := make(chan struct{})
	for w := range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range 200 {
				id := fmt.Sprintf("s%d", (w*200+i)%40)
				c.put(id, "pw")
				c.get(id)
				if i%10 == 0 {
					c.forget(id)
				}
				c.has(id)
				c.len()
			}
		}()
	}
	for range 8 {
		<-done
	}
	if c.len() > 16 {
		t.Errorf("len = %d, over the bound", c.len())
	}
}
