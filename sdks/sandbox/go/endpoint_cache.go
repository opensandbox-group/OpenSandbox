// Copyright 2026 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package opensandbox

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	DefaultEndpointCacheTTL  = 600 * time.Second
	DefaultEndpointCacheSize = 1024
)

type endpointCacheKey struct {
	sandboxID      string
	port           int
	useServerProxy bool
}

func (k endpointCacheKey) String() string {
	return fmt.Sprintf("%s:%d:%v", k.sandboxID, k.port, k.useServerProxy)
}

type endpointCacheEntry struct {
	key       endpointCacheKey
	endpoint  *Endpoint
	expiresAt time.Time
}

func cloneEndpoint(ep *Endpoint) *Endpoint {
	headers := make(map[string]string, len(ep.Headers))
	for k, v := range ep.Headers {
		headers[k] = v
	}
	return &Endpoint{Endpoint: ep.Endpoint, Headers: headers}
}

// EndpointCache is a thread-safe LRU+TTL cache for sandbox endpoints.
type EndpointCache struct {
	mu      sync.Mutex
	entries map[endpointCacheKey]*list.Element
	order   *list.List
	maxSize int
	ttl     time.Duration

	group singleflight.Group
}

// NewEndpointCache creates a cache with the given max size and TTL.
// If maxSize <= 0, DefaultEndpointCacheSize is used.
// If ttl <= 0, DefaultEndpointCacheTTL is used.
func NewEndpointCache(maxSize int, ttl time.Duration) *EndpointCache {
	if maxSize <= 0 {
		maxSize = DefaultEndpointCacheSize
	}
	if ttl <= 0 {
		ttl = DefaultEndpointCacheTTL
	}
	return &EndpointCache{
		entries: make(map[endpointCacheKey]*list.Element),
		order:   list.New(),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

// Get retrieves a cached endpoint. Returns nil, false on miss or expiry.
func (c *EndpointCache) Get(key endpointCacheKey) (*Endpoint, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	entry := elem.Value.(*endpointCacheEntry)
	if time.Now().After(entry.expiresAt) {
		c.removeLocked(elem)
		return nil, false
	}
	c.order.MoveToFront(elem)
	return cloneEndpoint(entry.endpoint), true
}

// Put stores an endpoint in the cache, evicting LRU entries if at capacity.
func (c *EndpointCache) Put(key endpointCacheKey, ep *Endpoint) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.entries[key]; ok {
		c.order.MoveToFront(elem)
		entry := elem.Value.(*endpointCacheEntry)
		entry.endpoint = cloneEndpoint(ep)
		entry.expiresAt = time.Now().Add(c.ttl)
		return
	}

	for c.order.Len() >= c.maxSize {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.removeLocked(oldest)
	}

	entry := &endpointCacheEntry{
		key:       key,
		endpoint:  cloneEndpoint(ep),
		expiresAt: time.Now().Add(c.ttl),
	}
	elem := c.order.PushFront(entry)
	c.entries[key] = elem
}

// Invalidate removes all cached and inflight entries for the given sandbox ID.
func (c *EndpointCache) Invalidate(sandboxID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for key, elem := range c.entries {
		if key.sandboxID == sandboxID {
			c.removeLocked(elem)
			c.group.Forget(key.String())
		}
	}
}

// Len returns the number of entries in the cache.
func (c *EndpointCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

func (c *EndpointCache) removeLocked(elem *list.Element) {
	entry := elem.Value.(*endpointCacheEntry)
	delete(c.entries, entry.key)
	c.order.Remove(elem)
}

// GetOrFetch checks cache, deduplicates inflight requests via singleflight,
// and calls fetch on miss. The caller's context is respected: if ctx is
// cancelled while waiting for a shared inflight request, the caller returns
// immediately with the context error (the shared fetch continues for other waiters).
func (c *EndpointCache) GetOrFetch(ctx context.Context, key endpointCacheKey, fetch func() (*Endpoint, error)) (*Endpoint, error) {
	if ep, ok := c.Get(key); ok {
		return ep, nil
	}

	ch := c.group.DoChan(key.String(), func() (interface{}, error) {
		if ep, ok := c.Get(key); ok {
			return ep, nil
		}
		ep, err := fetch()
		if err != nil {
			return nil, err
		}
		c.Put(key, ep)
		return ep, nil
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-ch:
		if result.Err != nil {
			return nil, result.Err
		}
		return cloneEndpoint(result.Val.(*Endpoint)), nil
	}
}
