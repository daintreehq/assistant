package markdown

import (
	"container/list"
	"sync"
)

// renderCache is a bounded LRU over rendered markdown. A render is PURE given its
// key (contentHash, width, expanded), so caching is safe and lets us reuse
// a block across frames and across the live→sealed transition (the scrollback
// commit re-renders at the same width, hitting the cache). The cache NEVER does
// I/O and holds only finished strings, so a hit is allocation-light.
//
// LRU because the working set is "recently visible blocks": a long transcript
// would otherwise grow the map unbounded as it scrolls. We evict the oldest.
type renderCache struct {
	mu    sync.Mutex
	max   int
	ll    *list.List // front = most-recently-used
	items map[cacheKey]*list.Element
}

// cacheKey is the pure-render identity. contentHash (not the content itself)
// keeps the key small and comparable; width/expanded change the output, while theme
// is fixed for the lifetime of the owning Renderer.
type cacheKey struct {
	contentHash uint64
	width       int
	expanded    bool
}

// cacheEntry is the stored render result.
type cacheEntry struct {
	key   cacheKey
	value Rendered
}

func newRenderCache(max int) *renderCache {
	if max < 1 {
		max = 1
	}
	return &renderCache{
		max:   max,
		ll:    list.New(),
		items: make(map[cacheKey]*list.Element, max),
	}
}

// get returns the cached render and promotes it to most-recently-used.
func (c *renderCache) get(k cacheKey) (Rendered, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*cacheEntry).value, true
	}
	return Rendered{}, false
}

// put inserts a render, evicting the least-recently-used entry past capacity.
func (c *renderCache) put(k cacheKey, v Rendered) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		el.Value.(*cacheEntry).value = v
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&cacheEntry{key: k, value: v})
	c.items[k] = el
	for c.ll.Len() > c.max {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry).key)
	}
}

// len reports the current entry count (used by tests to assert hit/evict behavior).
func (c *renderCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
