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

package slotsource

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileParserBound(t *testing.T) {
	raw := []byte(`{
		"id": "slot-1",
		"phase": "Bound",
		"owner": {"sandboxUid": "uid-1", "instanceGeneration": 3, "assignmentAttempt": 2},
		"ip": "10.0.0.5",
		"hostNetnsPath": "/var/run/netns/ns1",
		"hostVeth": "vethX",
		"gateway": "10.0.0.1",
		"privateCidr": "10.0.0.0/24",
		"dnsPath": "/run/fast-sandbox/network/dns/uid-1",
		"version": 7,
		"unrelated": {"kept": "ignored"}
	}`)
	slot, err := (FileParser{}).Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, "slot-1", slot.ID)
	assert.Equal(t, PhaseBound, slot.Phase)
	assert.Equal(t, "uid-1", slot.Owner.SandboxUID)
	assert.Equal(t, uint64(3), slot.Owner.InstanceGeneration)
	assert.Equal(t, uint64(2), slot.Owner.AssignmentAttempt)
	assert.Equal(t, "10.0.0.5", slot.IP.String())
	assert.Equal(t, "/var/run/netns/ns1", slot.HostNetnsPath)
	assert.Equal(t, "vethX", slot.HostVeth)
	assert.Equal(t, "10.0.0.1", slot.Gateway.String())
	assert.Equal(t, "10.0.0.0/24", slot.PrivateCIDR.String())
	assert.Equal(t, "/run/fast-sandbox/network/dns/uid-1", slot.DNSPath)
}

func TestFileParserStrictFailures(t *testing.T) {
	cases := map[string]string{
		"not json":         `{broken`,
		"missing id":       `{"phase":"Bound","owner":{"sandboxUid":"u"}}`,
		"missing owner":    `{"id":"s","phase":"Bound"}`,
		"unknown phase":    `{"id":"s","phase":"Running"}`,
		"bad ip":           `{"id":"s","phase":"Bound","owner":{"sandboxUid":"u"},"ip":"nope","gateway":"10.0.0.1","hostNetnsPath":"/x","hostVeth":"v","dnsPath":"/d"}`,
		"bad gateway":      `{"id":"s","phase":"Bound","owner":{"sandboxUid":"u"},"ip":"10.0.0.5","gateway":"nope","hostNetnsPath":"/x","hostVeth":"v","dnsPath":"/d"}`,
		"missing veth":     `{"id":"s","phase":"Bound","owner":{"sandboxUid":"u"},"ip":"10.0.0.5","gateway":"10.0.0.1","hostNetnsPath":"/x","dnsPath":"/d"}`,
		"missing netns":    `{"id":"s","phase":"Bound","owner":{"sandboxUid":"u"},"ip":"10.0.0.5","gateway":"10.0.0.1","hostVeth":"v","dnsPath":"/d"}`,
		"missing dns path": `{"id":"s","phase":"Bound","owner":{"sandboxUid":"u"},"ip":"10.0.0.5","gateway":"10.0.0.1","hostVeth":"v"}`,
		"bad private cidr": `{"id":"s","phase":"Bound","owner":{"sandboxUid":"u"},"ip":"10.0.0.5","gateway":"10.0.0.1","hostNetnsPath":"/x","hostVeth":"v","dnsPath":"/d","privateCidr":"oops"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := (FileParser{}).Parse([]byte(raw))
			require.Error(t, err)
		})
	}
}

func TestFileParserCleanNeedsOnlyIdentity(t *testing.T) {
	slot, err := (FileParser{}).Parse([]byte(`{"id":"s","phase":"Clean","owner":{"sandboxUid":"u"}}`))
	require.NoError(t, err)
	assert.Equal(t, PhaseClean, slot.Phase)
}

func writeSlot(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

const boundJSON = `{"id":"%s","phase":"Bound","owner":{"sandboxUid":"%s","instanceGeneration":1,"assignmentAttempt":1},"ip":"10.0.0.5","hostNetnsPath":"/var/run/netns/ns","hostVeth":"vethX","gateway":"10.0.0.1","privateCidr":"10.0.0.0/24","dnsPath":"/run/fast-sandbox/network/dns/x"}`

func TestFileSourceListBoundOnly(t *testing.T) {
	dir := t.TempDir()
	writeSlot(t, dir, "a.json", `{"id":"a","phase":"Bound","owner":{"sandboxUid":"u-a"},"ip":"10.0.0.5","hostNetnsPath":"/n","hostVeth":"v","gateway":"10.0.0.1","dnsPath":"/d"}`)
	writeSlot(t, dir, "b.json", `{"id":"b","phase":"Clean","owner":{"sandboxUid":"u-b"}}`)
	writeSlot(t, dir, "ignored.txt", `not a slot`)

	src := NewFileSource(dir, 0)
	slots, err := src.List(context.Background())
	require.NoError(t, err)
	require.Len(t, slots, 1)
	assert.Equal(t, "a", slots[0].ID)
}

func TestFileSourceListSkipsBadRecord(t *testing.T) {
	dir := t.TempDir()
	writeSlot(t, dir, "a.json", `{"id":"a","phase":"Bound","owner":{"sandboxUid":"u-a"},"ip":"10.0.0.5","hostNetnsPath":"/n","hostVeth":"v","gateway":"10.0.0.1","dnsPath":"/d"}`)
	writeSlot(t, dir, "broken.json", `{oops`)

	src := NewFileSource(dir, 0)
	slots, err := src.List(context.Background())
	require.NoError(t, err)
	require.Len(t, slots, 1, "unparseable records must be excluded, not fail the snapshot")
	assert.Equal(t, "a", slots[0].ID)
}

// TestFileSourceBadRecordFailsClosedForActiveSubject: a record that was bound
// and then becomes unparseable must be treated as absent — the controller
// unloads the subject (Deleted) instead of keeping stale enforcement alive.
func TestFileSourceBadRecordFailsClosedForActiveSubject(t *testing.T) {
	dir := t.TempDir()
	src := NewFileSource(dir, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := src.Watch(ctx)
	require.NoError(t, err)

	writeSlot(t, dir, "a.json", `{"id":"a","phase":"Bound","owner":{"sandboxUid":"u-a","instanceGeneration":1,"assignmentAttempt":1},"ip":"10.0.0.5","hostNetnsPath":"/n","hostVeth":"v","gateway":"10.0.0.1","dnsPath":"/d"}`)
	assertEvent(t, events, EventBound)

	// corrupt the record in place: the next poll must NOT emit Bound/Updated;
	// the file drops out of the view -> Deleted (fail closed), plus EventError
	writeSlot(t, dir, "a.json", `{oops`)
	ev := waitEvent(t, events)
	require.Equal(t, EventError, ev.Type, "first event after corruption is the parse failure")
	ev = waitEvent(t, events)
	require.Equal(t, EventDeleted, ev.Type, "unparseable record must be treated as gone")
	require.Equal(t, "a", ev.Slot.ID)
}

func TestFileSourceWatchLifecycle(t *testing.T) {
	dir := t.TempDir()
	src := NewFileSource(dir, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := src.Watch(ctx)
	require.NoError(t, err)

	// bound appears
	writeSlot(t, dir, "a.json", `{"id":"a","phase":"Bound","owner":{"sandboxUid":"u-a","instanceGeneration":1,"assignmentAttempt":1},"ip":"10.0.0.5","hostNetnsPath":"/n","hostVeth":"v","gateway":"10.0.0.1","dnsPath":"/d"}`)
	assertEvent(t, events, EventBound)

	// fencing changes -> updated
	writeSlot(t, dir, "a.json", `{"id":"a","phase":"Bound","owner":{"sandboxUid":"u-a","instanceGeneration":2,"assignmentAttempt":1},"ip":"10.0.0.5","hostNetnsPath":"/n","hostVeth":"v","gateway":"10.0.0.1","dnsPath":"/d"}`)
	assertEvent(t, events, EventUpdated)

	// file deleted -> deleted
	require.NoError(t, os.Remove(filepath.Join(dir, "a.json")))
	assertEvent(t, events, EventDeleted)

	// bad record -> EventError, watch keeps working
	writeSlot(t, dir, "bad.json", `{oops`)
	ev := waitEvent(t, events)
	require.Equal(t, EventError, ev.Type)
	require.Error(t, ev.Err)
	require.NoError(t, os.Remove(filepath.Join(dir, "bad.json")))

	// teardown phase in place: bound record transitions to Destroying -> Deleted
	writeSlot(t, dir, "b.json", `{"id":"b","phase":"Bound","owner":{"sandboxUid":"u-b","instanceGeneration":1,"assignmentAttempt":1},"ip":"10.0.0.6","hostNetnsPath":"/n","hostVeth":"v","gateway":"10.0.0.1","dnsPath":"/d"}`)
	assertEvent(t, events, EventBound)
	writeSlot(t, dir, "b.json", `{"id":"b","phase":"Destroying","owner":{"sandboxUid":"u-b"}}`)
	ev = waitEvent(t, events)
	require.Equal(t, EventDeleted, ev.Type)
	require.Equal(t, "b", ev.Slot.ID)

	// a fresh non-bound record never emits an event
	writeSlot(t, dir, "c.json", `{"id":"c","phase":"Clean","owner":{"sandboxUid":"u-c"}}`)
	select {
	case ev := <-events:
		t.Fatalf("unexpected event for fresh non-bound record: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func waitEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

func assertEvent(t *testing.T, ch <-chan Event, typ EventType) {
	t.Helper()
	ev := waitEvent(t, ch)
	assert.Equal(t, typ, ev.Type)
}

// TestFileSourceWatchDeliversPreExistingSlots: the List-then-Watch handoff
// race regression — a slot written before Watch starts (so List may have
// missed it) must still be re-delivered as Bound, or the subject is never
// registered and a create-then-configure push stays pending forever.
func TestFileSourceWatchDeliversPreExistingSlots(t *testing.T) {
	dir := t.TempDir()
	writeSlot(t, dir, "a.json", `{"id":"a","phase":"Bound","owner":{"sandboxUid":"u-a","instanceGeneration":1,"assignmentAttempt":1},"ip":"10.0.0.5","hostNetnsPath":"/n","hostVeth":"v","gateway":"10.0.0.1","dnsPath":"/d"}`)

	src := NewFileSource(dir, 50*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	events, err := src.Watch(ctx)
	require.NoError(t, err)

	ev := waitEvent(t, events)
	require.Equal(t, EventBound, ev.Type)
	require.Equal(t, "a", ev.Slot.ID)
}
