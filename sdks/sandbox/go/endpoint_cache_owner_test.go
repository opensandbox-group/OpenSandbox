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
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestGetEndpoint_LastWaiterCancelRetiresSharedFetch reproduces #2569: once
// the only caller of a stalled cache-miss lookup cancels, the shared endpoint
// request must be cancelled and a later lookup must start a fresh request
// instead of joining the orphaned one.
func TestGetEndpoint_LastWaiterCancelRetiresSharedFetch(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	upstreamCancelled := make(chan struct{}, 1)
	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			select {
			case <-r.Context().Done():
				upstreamCancelled <- struct{}{}
				return
			case <-release:
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"endpoint":"localhost:8080"}`))
	}))
	defer server.Close()
	// Release a stalled handler before server.Close waits for it.
	defer close(release)

	client := NewLifecycleClient(server.URL, "", WithHTTPClient(&http.Client{Transport: DefaultTransport()}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := client.GetEndpoint(ctx, "sandbox-1", 8080, nil)
		finished <- err
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("endpoint request did not reach the server")
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller error = %v, want context.Canceled", err)
	}

	select {
	case <-upstreamCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("shared endpoint request was not cancelled after its only waiter left")
	}

	second, cancelSecond := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelSecond()
	ep, err := client.GetEndpoint(second, "sandbox-1", 8080, nil)
	if err != nil {
		t.Fatalf("second caller error = %v, want a fresh resolution", err)
	}
	if ep.Endpoint != "localhost:8080" {
		t.Fatalf("endpoint = %q, want %q", ep.Endpoint, "localhost:8080")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream requests = %d, want 2", got)
	}
}

// TestEndpointCache_GetOrFetch_CancelOneWaiterKeepsSharedFetch guards the
// inverse problem: a waiter that cancels must not cancel the shared fetch
// while another waiter still needs it.
func TestEndpointCache_GetOrFetch_CancelOneWaiterKeepsSharedFetch(t *testing.T) {
	c := NewEndpointCache(10, time.Minute)
	key := endpointCacheKey{sandboxID: "sb-1", port: 8080}

	var fetchCount atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	fetch := func(ctx context.Context) (*Endpoint, error) {
		fetchCount.Add(1)
		close(started)
		select {
		case <-release:
			return &Endpoint{Endpoint: "shared"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	type result struct {
		ep  *Endpoint
		err error
	}
	kept := make(chan result, 1)
	go func() {
		ep, err := c.GetOrFetch(context.Background(), key, fetch)
		kept <- result{ep, err}
	}()
	<-started

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	go func() {
		_, err := c.GetOrFetch(cancelledCtx, key, fetch)
		cancelled <- err
	}()
	// Wait until the second caller has joined the flight before cancelling it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		waiters := c.flights[key].waiters
		c.mu.Unlock()
		if waiters == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second caller did not join the shared fetch (waiters=%d)", waiters)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-cancelled; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller error = %v, want context.Canceled", err)
	}

	close(release)
	got := <-kept
	if got.err != nil {
		t.Fatalf("remaining caller error = %v, want shared result", got.err)
	}
	if got.ep.Endpoint != "shared" {
		t.Fatalf("endpoint = %q, want %q", got.ep.Endpoint, "shared")
	}
	if n := fetchCount.Load(); n != 1 {
		t.Fatalf("fetch called %d times, want 1", n)
	}
	if _, ok := c.Get(key); !ok {
		t.Fatal("shared result should be cached")
	}
}
