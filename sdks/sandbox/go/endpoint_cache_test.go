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
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEndpointCache_GetPut(t *testing.T) {
	c := NewEndpointCache(10, time.Minute)

	key := endpointCacheKey{sandboxID: "sb-1", port: 8080, useServerProxy: false}
	ep := &Endpoint{Endpoint: "localhost:8080"}

	if _, ok := c.Get(key); ok {
		t.Fatal("expected miss on empty cache")
	}

	c.Put(key, ep)

	got, ok := c.Get(key)
	if !ok {
		t.Fatal("expected hit after put")
	}
	if got.Endpoint != ep.Endpoint {
		t.Fatalf("got %q, want %q", got.Endpoint, ep.Endpoint)
	}
}

func TestEndpointCache_TTLExpiry(t *testing.T) {
	c := NewEndpointCache(10, 50*time.Millisecond)

	key := endpointCacheKey{sandboxID: "sb-1", port: 8080}
	c.Put(key, &Endpoint{Endpoint: "localhost:8080"})

	if _, ok := c.Get(key); !ok {
		t.Fatal("expected hit before TTL")
	}

	time.Sleep(60 * time.Millisecond)

	if _, ok := c.Get(key); ok {
		t.Fatal("expected miss after TTL")
	}
	if c.Len() != 0 {
		t.Fatalf("expected 0 entries after expiry, got %d", c.Len())
	}
}

func TestEndpointCache_LRUEviction(t *testing.T) {
	c := NewEndpointCache(3, time.Minute)

	for i := 0; i < 3; i++ {
		key := endpointCacheKey{sandboxID: fmt.Sprintf("sb-%d", i), port: 8080}
		c.Put(key, &Endpoint{Endpoint: fmt.Sprintf("host-%d:8080", i)})
	}

	if c.Len() != 3 {
		t.Fatalf("expected 3 entries, got %d", c.Len())
	}

	// Access sb-0 to make it recently used.
	c.Get(endpointCacheKey{sandboxID: "sb-0", port: 8080})

	// Insert 4th — should evict sb-1 (least recently used).
	c.Put(endpointCacheKey{sandboxID: "sb-3", port: 8080}, &Endpoint{Endpoint: "host-3:8080"})

	if c.Len() != 3 {
		t.Fatalf("expected 3 entries after eviction, got %d", c.Len())
	}

	if _, ok := c.Get(endpointCacheKey{sandboxID: "sb-1", port: 8080}); ok {
		t.Fatal("sb-1 should have been evicted")
	}
	if _, ok := c.Get(endpointCacheKey{sandboxID: "sb-0", port: 8080}); !ok {
		t.Fatal("sb-0 should still be cached")
	}
}

func TestEndpointCache_Invalidate(t *testing.T) {
	c := NewEndpointCache(10, time.Minute)

	c.Put(endpointCacheKey{sandboxID: "sb-1", port: 8080}, &Endpoint{Endpoint: "a"})
	c.Put(endpointCacheKey{sandboxID: "sb-1", port: 18080}, &Endpoint{Endpoint: "b"})
	c.Put(endpointCacheKey{sandboxID: "sb-2", port: 8080}, &Endpoint{Endpoint: "c"})

	c.Invalidate("sb-1")

	if c.Len() != 1 {
		t.Fatalf("expected 1 entry after invalidate, got %d", c.Len())
	}
	if _, ok := c.Get(endpointCacheKey{sandboxID: "sb-2", port: 8080}); !ok {
		t.Fatal("sb-2 should still be cached")
	}
}

func TestEndpointCache_GetOrFetch_Dedup(t *testing.T) {
	c := NewEndpointCache(10, time.Minute)
	key := endpointCacheKey{sandboxID: "sb-1", port: 8080}

	var fetchCount int64
	fetch := func() (*Endpoint, error) {
		atomic.AddInt64(&fetchCount, 1)
		time.Sleep(50 * time.Millisecond)
		return &Endpoint{Endpoint: "result"}, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ep, err := c.GetOrFetch(context.Background(), key, fetch)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if ep.Endpoint != "result" {
				t.Errorf("got %q, want %q", ep.Endpoint, "result")
			}
		}()
	}
	wg.Wait()

	count := atomic.LoadInt64(&fetchCount)
	if count != 1 {
		t.Fatalf("fetch called %d times, want 1 (singleflight dedup)", count)
	}
}

func TestEndpointCache_GetOrFetch_CacheHit(t *testing.T) {
	c := NewEndpointCache(10, time.Minute)
	key := endpointCacheKey{sandboxID: "sb-1", port: 8080}

	c.Put(key, &Endpoint{Endpoint: "cached"})

	var fetchCount int64
	ep, err := c.GetOrFetch(context.Background(), key, func() (*Endpoint, error) {
		atomic.AddInt64(&fetchCount, 1)
		return &Endpoint{Endpoint: "fetched"}, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.Endpoint != "cached" {
		t.Fatalf("got %q, want %q", ep.Endpoint, "cached")
	}
	if atomic.LoadInt64(&fetchCount) != 0 {
		t.Fatal("fetch should not be called on cache hit")
	}
}

func TestEndpointCache_GetOrFetch_Error(t *testing.T) {
	c := NewEndpointCache(10, time.Minute)
	key := endpointCacheKey{sandboxID: "sb-1", port: 8080}

	_, err := c.GetOrFetch(context.Background(), key, func() (*Endpoint, error) {
		return nil, fmt.Errorf("network error")
	})
	if err == nil {
		t.Fatal("expected error")
	}

	if c.Len() != 0 {
		t.Fatal("failed fetch should not populate cache")
	}
}
