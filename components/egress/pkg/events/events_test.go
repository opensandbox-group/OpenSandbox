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

package events

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/stretchr/testify/require"
)

type captureSubscriber struct {
	recv chan BlockedEvent
}

func (c *captureSubscriber) HandleBlocked(_ context.Context, ev BlockedEvent) {
	c.recv <- ev
}

type blockingSubscriber struct {
	entered chan struct{}
	block   chan struct{}
	recv    chan BlockedEvent
}

func (b *blockingSubscriber) HandleBlocked(_ context.Context, ev BlockedEvent) {
	// Signal the test that the handler is busy, then block until the channel
	// is closed to simulate a slow consumer and trigger backpressure.
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.block
	select {
	case b.recv <- ev:
	default:
	}
}

func TestBroadcasterFanout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := NewBroadcaster(ctx, BroadcasterConfig{QueueSize: 2})

	sub1 := &captureSubscriber{recv: make(chan BlockedEvent, 1)}
	sub2 := &captureSubscriber{recv: make(chan BlockedEvent, 1)}
	b.AddSubscriber(sub1)
	b.AddSubscriber(sub2)

	ev := BlockedEvent{Hostname: "example.com.", Timestamp: time.Now()}
	b.Publish(ev)

	select {
	case got := <-sub1.recv:
		require.Equal(t, ev.Hostname, got.Hostname, "sub1 expected hostname")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "sub1 did not receive event")
	}

	select {
	case got := <-sub2.recv:
		require.Equal(t, ev.Hostname, got.Hostname, "sub2 expected hostname")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "sub2 did not receive event")
	}

	b.Close()
}

func TestBroadcasterDropsWhenSubscriberBackedUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Small queue; blocking subscriber holds the first event it handles.
	b := NewBroadcaster(ctx, BroadcasterConfig{QueueSize: 1})
	entered := make(chan struct{})
	block := make(chan struct{})
	sub := &blockingSubscriber{entered: entered, block: block, recv: make(chan BlockedEvent, 8)}
	b.AddSubscriber(sub)

	ev1 := BlockedEvent{Hostname: "first.example", Timestamp: time.Now()}
	ev2 := BlockedEvent{Hostname: "second.example", Timestamp: time.Now()}
	ev3 := BlockedEvent{Hostname: "third.example", Timestamp: time.Now()}

	// ev1 is handed directly to the subscriber, which then blocks in its handler.
	b.Publish(ev1)
	<-entered
	// Subscriber is provably busy: ev2 fills the one-slot queue, ev3 is dropped.
	b.Publish(ev2)
	b.Publish(ev3)

	// Allow subscriber to drain whatever was queued.
	close(block)

	for _, want := range []BlockedEvent{ev1, ev2} {
		select {
		case got := <-sub.recv:
			require.Equal(t, want.Hostname, got.Hostname, "expected %s to be delivered", want.Hostname)
		case <-time.After(2 * time.Second):
			require.FailNow(t, "subscriber did not receive %s", want.Hostname)
		}
	}
	select {
	case dropped := <-sub.recv:
		require.FailNow(t, "third event should have been dropped, got %s", dropped.Hostname)
	case <-time.After(200 * time.Millisecond):
	}

	b.Close()
}

func TestWebhookSubscriberSendsPayload(t *testing.T) {
	var (
		gotMethod  string
		gotPayload webhookPayload
	)
	const (
		sandboxIDInitial = "sandbox-test"
		sandboxIDLater   = "sandbox-updated"
	)
	t.Setenv(constants.EnvSandboxID, sandboxIDInitial)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		_ = json.Unmarshal(body, &gotPayload)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sub := NewWebhookSubscriber(server.URL)
	require.NotNil(t, sub, "webhook subscriber should not be nil")
	t.Setenv(constants.EnvSandboxID, sandboxIDLater)

	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ev := BlockedEvent{Hostname: "Example.com.", Timestamp: ts}
	sub.HandleBlocked(context.Background(), ev)

	require.Equal(t, http.MethodPost, gotMethod, "expected POST")
	require.Equal(t, ev.Hostname, gotPayload.Hostname, "expected hostname")
	require.Equal(t, webhookSource, gotPayload.Source, "expected source")
	require.Equal(t, sandboxIDInitial, gotPayload.SandboxID, "expected sandboxId captured at init")
	require.NotEmpty(t, gotPayload.Timestamp, "expected timestamp to be set")
}
