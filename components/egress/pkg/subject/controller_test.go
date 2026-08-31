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

package subject

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/slotsource"
)

// fakeHooks: records transitions; optionally fails deny-first installs.
type fakeHooks struct {
	mu          sync.Mutex
	registered  []slotsource.Slot
	unloaded    []Subject
	slotUpdated []Subject
	denyErr     error        // when set, every OnRegistered fails
	failFirstN  atomic.Int32 // fail while >0 (decremented per attempt)
	attempts    atomic.Int32
}

func (h *fakeHooks) OnRegistered(_ Subject, slot slotsource.Slot) error {
	h.attempts.Add(1)
	if h.denyErr != nil {
		return h.denyErr
	}
	if h.failFirstN.Load() > 0 {
		h.failFirstN.Add(-1)
		return errors.New("nft unavailable")
	}
	h.mu.Lock()
	h.registered = append(h.registered, slot)
	h.mu.Unlock()
	return nil
}

func (h *fakeHooks) OnRegisteredComplete(_ Subject, _ slotsource.Slot) {}

func (h *fakeHooks) OnSlotUpdated(s Subject, slot slotsource.Slot) error {
	h.mu.Lock()
	h.slotUpdated = append(h.slotUpdated, s)
	h.mu.Unlock()
	return nil
}

func (h *fakeHooks) OnUnloaded(s Subject, _ slotsource.Slot) error {
	h.mu.Lock()
	h.unloaded = append(h.unloaded, s)
	h.mu.Unlock()
	return nil
}

func (h *fakeHooks) countRegistered() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.registered)
}

func (h *fakeHooks) registeredAt(i int) slotsource.Slot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.registered[i]
}

func (h *fakeHooks) unloadedAt(i int) Subject {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i >= len(h.unloaded) {
		return ""
	}
	return h.unloaded[i]
}

// fakeSource: pre-seeded slots (re-delivered as Bound at watch start, per the
// Source contract) + scripted event channel.
type fakeSource struct {
	slots  []slotsource.Slot
	events chan slotsource.Event
}

func (s *fakeSource) List(context.Context) ([]slotsource.Slot, error) {
	return s.slots, nil
}

func (s *fakeSource) Watch(ctx context.Context) (<-chan slotsource.Event, error) {
	out := make(chan slotsource.Event, len(s.slots)+4)
	for _, slot := range s.slots {
		out <- slotsource.Event{Type: slotsource.EventBound, Slot: slot}
	}
	go func() {
		for {
			select {
			case ev, ok := <-s.events:
				if !ok {
					close(out)
					return
				}
				out <- ev
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func boundSlot(uid string, gen uint64, ip string) slotsource.Slot {
	return slotsource.Slot{
		ID:            "slot-" + uid,
		Phase:         slotsource.PhaseBound,
		Owner:         slotsource.Owner{SandboxUID: uid, InstanceGeneration: gen, AssignmentAttempt: 1},
		IP:            netip.MustParseAddr(ip),
		HostNetnsPath: "/var/run/netns/ns-" + uid,
		HostVeth:      "veth" + uid,
		Gateway:       netip.MustParseAddr("10.0.0.1"),
		PrivateCIDR:   netip.MustParsePrefix("10.0.0.0/24"),
		DNSPath:       "/run/fast-sandbox/network/dns/" + uid,
	}
}

func TestControllerRescanRegistersDenyFirst(t *testing.T) {
	reg := NewRegistry(nil, nil)
	hooks := &fakeHooks{}
	c := NewController(reg, hooks)
	slot := boundSlot("a", 1, "10.0.0.5")
	src := &fakeSource{slots: []slotsource.Slot{slot}, events: make(chan slotsource.Event)}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := c.StartWatch(ctx, src)
	waitFor(t, func() bool { return hooks.countRegistered() == 1 })
	require.Equal(t, slot, hooks.registeredAt(0))

	state, ok := reg.Get(FromSandboxUID("a"))
	require.True(t, ok)
	assert.Equal(t, StateDenying, state)

	cancel()
	require.NoError(t, <-errCh)
}

func TestControllerDenyFirstFailureRetriesAndFailClosed(t *testing.T) {
	reg := NewRegistry(nil, nil)
	hooks := &fakeHooks{}
	hooks.failFirstN.Store(2)

	c := NewController(reg, hooks)
	slot := boundSlot("a", 1, "10.0.0.5")
	src := &fakeSource{slots: []slotsource.Slot{slot}, events: make(chan slotsource.Event)}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errCh := c.StartWatch(ctx, src)

	waitFor(t, func() bool {
		state, _ := reg.Get(FromSandboxUID("a"))
		return state == StateDenying && hooks.attempts.Load() >= 3
	})
	assert.Equal(t, int32(3), hooks.attempts.Load())
	assert.Len(t, hooks.registered, 1)
	cancel()
	require.NoError(t, <-errCh)
}

func TestControllerEventsBoundAndDeleted(t *testing.T) {
	reg := NewRegistry(nil, nil)
	hooks := &fakeHooks{}
	c := NewController(reg, hooks)
	events := make(chan slotsource.Event, 4)
	src := &fakeSource{events: events}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errCh := c.StartWatch(ctx, src)

	events <- slotsource.Event{Type: slotsource.EventBound, Slot: boundSlot("a", 1, "10.0.0.5")}
	waitFor(t, func() bool {
		state, _ := reg.Get(FromSandboxUID("a"))
		return state == StateDenying
	})

	// policy lands -> active
	pol, err := parsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err)
	require.NoError(t, reg.ApplyPolicy(FromSandboxUID("a"), pol))
	state, _ := reg.Get(FromSandboxUID("a"))
	assert.Equal(t, StateActive, state)

	// slot gone -> unload, hook fires, absent
	events <- slotsource.Event{Type: slotsource.EventDeleted, Slot: boundSlot("a", 1, "10.0.0.5")}
	waitFor(t, func() bool { return hooks.unloadedAt(0) == FromSandboxUID("a") })
	assert.Equal(t, FromSandboxUID("a"), hooks.unloadedAt(0))
	state, ok := reg.Get(FromSandboxUID("a"))
	assert.False(t, ok)
	assert.Equal(t, StateAbsent, state)

	cancel()
	close(events)
	require.NoError(t, <-errCh)
}

func TestControllerEventErrorFailClosed(t *testing.T) {
	reg := NewRegistry(nil, nil)
	hooks := &fakeHooks{}
	c := NewController(reg, hooks)
	events := make(chan slotsource.Event, 2)
	src := &fakeSource{events: events}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errCh := c.StartWatch(ctx, src)

	events <- slotsource.Event{Type: slotsource.EventError, Err: errors.New("parse failure")}
	events <- slotsource.Event{Type: slotsource.EventBound, Slot: boundSlot("a", 1, "10.0.0.5")}
	waitFor(t, func() bool {
		state, _ := reg.Get(FromSandboxUID("a"))
		return state == StateDenying
	})
	cancel()
	close(events)
	require.NoError(t, <-errCh)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
