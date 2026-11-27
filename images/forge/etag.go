package main

import "sync"

// etagCache (#236 item 5): the sidecar remembers the ETag and body of each successful GET and asks the forge with
// If-None-Match next time. A 304 is answered from the cache as the earlier 200, so the loop sees no difference; on
// GitHub a 304 does not count against the rate limit. One sidecar serves one agent with one credential, so the
// upstream URL is the key. Bounded: at most etagMaxEntries entries and etagMaxBytes of bodies; when full, the
// oldest entries go first.
const (
	etagMaxEntries = 512
	etagMaxBytes   = 32 << 20
	etagMaxBody    = 1 << 20
)

type etagEntry struct {
	etag, ctype string
	body        []byte
}

type etagCache struct {
	mu    sync.Mutex
	m     map[string]*etagEntry
	order []string
	bytes int
}

func (c *etagCache) get(u string) *etagEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[u]
}

func (c *etagCache) drop(u string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(u)
}

func (c *etagCache) dropLocked(u string) {
	if e, ok := c.m[u]; ok {
		c.bytes -= len(e.body)
		delete(c.m, u)
		for i, k := range c.order {
			if k == u {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
	}
}

func (c *etagCache) put(u string, e *etagEntry) {
	if len(e.body) > etagMaxBody || e.etag == "" || len(e.etag) > 200 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*etagEntry{}
	}
	c.dropLocked(u)
	for len(c.order) > 0 && (len(c.order) >= etagMaxEntries || c.bytes+len(e.body) > etagMaxBytes) {
		c.dropLocked(c.order[0])
	}
	c.m[u] = e
	c.order = append(c.order, u)
	c.bytes += len(e.body)
}
