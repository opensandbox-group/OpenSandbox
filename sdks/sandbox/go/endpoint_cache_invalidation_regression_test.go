// Copyright 2026 The OpenSandbox Authors
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
	"testing"
	"time"
)

// blockEndpointFetch starts an outstanding cache miss. Its release function
// waits for the caller to finish, so cleanup cannot leave a blocked goroutine.
func blockEndpointFetch(t *testing.T, c *EndpointCache, key endpointCacheKey, endpoint string) func() {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := c.GetOrFetch(context.Background(), key, func() (*Endpoint, error) {
			close(started)
			<-release
			return &Endpoint{Endpoint: endpoint}, nil
		})
		if err != nil {
			t.Errorf("initial fetch: %v", err)
		}
	}()
	<-started
	var once sync.Once
	unblock := func() {
		once.Do(func() { close(release) })
		<-done
	}
	t.Cleanup(unblock)
	return unblock
}

func TestEndpointCache_InvalidateDoesNotRepopulateFromInflightFetch(t *testing.T) {
	c := NewEndpointCache(10, time.Minute)
	key := endpointCacheKey{sandboxID: "sb-1", port: 8080}
	release := blockEndpointFetch(t, c, key, "old")

	c.Invalidate("sb-1")
	release()

	if ep, ok := c.Get(key); ok {
		t.Fatalf("invalidated fetch repopulated cache with %q", ep.Endpoint)
	}
	calls := 0
	ep, err := c.GetOrFetch(context.Background(), key, func() (*Endpoint, error) {
		calls++
		return &Endpoint{Endpoint: "new"}, nil
	})
	if err != nil || ep.Endpoint != "new" || calls != 1 {
		t.Fatalf("fresh lookup = (%v, %v), fetch calls = %d; want new endpoint and one fetch", ep, err, calls)
	}
}

func TestEndpointCache_InvalidateSeparatesNewCallersFromInflightFetch(t *testing.T) {
	c := NewEndpointCache(10, time.Minute)
	key := endpointCacheKey{sandboxID: "sb-1", port: 8080}
	release := blockEndpointFetch(t, c, key, "old")
	c.Invalidate("sb-1")

	// The old fetch remains blocked. A post-invalidation lookup must invoke
	// its own fetch, rather than waiting for that obsolete singleflight call.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	ep, err := c.GetOrFetch(ctx, key, func() (*Endpoint, error) {
		calls++
		return &Endpoint{Endpoint: "new"}, nil
	})
	if err != nil {
		t.Fatalf("post-invalidation lookup waited for obsolete fetch: %v", err)
	}
	if ep.Endpoint != "new" || calls != 1 {
		t.Fatalf("fresh lookup = %v, fetch calls = %d; want new endpoint and one fetch", ep, calls)
	}
	release()
	if got, ok := c.Get(key); !ok || got.Endpoint != "new" {
		t.Fatalf("old fetch overwrote fresh cache: endpoint=%v, present=%v", got, ok)
	}
}

func TestEndpointCache_InvalidatePreservesOtherSandboxEntriesAndInflightFetches(t *testing.T) {
	c := NewEndpointCache(10, time.Minute)
	cachedKey := endpointCacheKey{sandboxID: "sb-other", port: 8080}
	inflightKey := endpointCacheKey{sandboxID: "sb-other", port: 18080}
	c.Put(cachedKey, &Endpoint{Endpoint: "other-cached"})
	release := blockEndpointFetch(t, c, inflightKey, "other-fetched")

	c.mu.Lock()
	otherFetch := c.inflight[inflightKey]
	c.mu.Unlock()
	c.Invalidate("sb-1")
	c.mu.Lock()
	preserved := c.inflight[inflightKey] == otherFetch && otherFetch != nil
	c.mu.Unlock()
	if !preserved {
		t.Fatal("invalidation removed another sandbox's inflight generation")
	}
	release()

	for key, want := range map[endpointCacheKey]string{
		cachedKey:   "other-cached",
		inflightKey: "other-fetched",
	} {
		if ep, ok := c.Get(key); !ok || ep.Endpoint != want {
			t.Fatalf("unrelated sandbox entry %v = (%v, %v), want %q", key, ep, ok, want)
		}
	}
}

func TestEndpointCache_CompletedFetchDoesNotRetainInflightGeneration(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("error=%v", fail), func(t *testing.T) {
			c := NewEndpointCache(10, time.Minute)
			key := endpointCacheKey{sandboxID: "sb-1", port: 8080}
			_, err := c.GetOrFetch(context.Background(), key, func() (*Endpoint, error) {
				if fail {
					return nil, fmt.Errorf("fetch failed")
				}
				return &Endpoint{Endpoint: "fetched"}, nil
			})
			if (err != nil) != fail {
				t.Fatalf("unexpected fetch error: %v", err)
			}
			c.mu.Lock()
			retained := len(c.inflight)
			c.mu.Unlock()
			if retained != 0 {
				t.Fatalf("completed fetch retained %d inflight generations", retained)
			}
		})
	}
}
