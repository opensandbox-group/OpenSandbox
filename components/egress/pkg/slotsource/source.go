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

// Package slotsource defines the egress-side consumption contract for the
// per-sandbox slot store observed by the egress control plane (fleet profile).
// The package is a source, not a store: all slot data is owned by
// fastlet, and egress never persists any state of its own.
//
// Design notes:
//
//   - The slot store is the single source of subject lifecycle (identity +
//     fencing) and dispatch-key material. It never carries policy or
//     credentials — those flow exclusively over the proxy route (see
//     "Two Control Paths").
//   - Consumers only ever see the normalized Slot/Event model, never a storage
//     format. The current backend is fastlet's internal file store
//     (/run/fast-sandbox/network/*.json); a future backend may be a read-only
//     fastlet RPC endpoint. Both must produce the same model.
//   - Fail-closed: any parse, validation, or watch failure is surfaced as an
//     error (EventError / returned error) and must cause the consumer to treat
//     the subject as denied — never as active.
//   - The slot-store contract does not decide who the "authority" is: this
//     package only defines what egress reads and guarantees about it.
package slotsource

import (
	"context"
	"net/netip"
)

// Phase mirrors the raw slot lifecycle. Only PhaseBound is acted upon; the
// Source filters Clean/Destroying records and never emits them.
type Phase string

const (
	PhaseBound      Phase = "Bound"
	PhaseClean      Phase = "Clean"
	PhaseDestroying Phase = "Destroying"
)

// Owner is the fencing triple: subject identity plus two generation counters.
// A change in either counter means the UID was rebound — all prior state for
// the subject must be discarded (a reset can never carry old policy into a
// new sandbox).
type Owner struct {
	SandboxUID         string
	InstanceGeneration uint64
	AssignmentAttempt  uint64
}

// Slot is the normalized view of a bound slot record. Exactly the fields the
// egress control plane consumes; anything else in the raw record is ignored.
//
// Required invariants enforced by Parser for a bound slot:
//   - ID, Owner.SandboxUID, HostNetnsPath, HostVeth, DNSPath are non-empty
//   - IP and Gateway are valid addresses
//   - PrivateCIDR is a valid prefix (may be the zero prefix when unused)
type Slot struct {
	ID            string
	Phase         Phase
	Owner         Owner
	IP            netip.Addr // dispatch key (ip saddr)
	HostNetnsPath string     // netns path for rule installation / defense-in-depth
	HostVeth      string     // iifname binding against UDP spoofing
	Gateway       netip.Addr // DNS proxy bind target / resolv.conf rewrite
	PrivateCIDR   netip.Prefix
	DNSPath       string // resolv.conf rewrite target file
}

// EventType is the slot lifecycle event. Event carries no Slot for
// EventDeleted and EventError.
type EventType int

const (
	// EventBound: a slot entered Bound. Register the subject (deny-first).
	EventBound EventType = iota
	// EventUpdated: identity or fencing of an existing bound slot changed.
	// The consumer must re-check fencing and discard stale state on mismatch.
	EventUpdated
	// EventDeleted: the slot is gone. Unload the subject (detach → deny → free).
	EventDeleted
	// EventError: a watch/parse failure. The consumer must fail closed.
	EventError
)

// Event is one slot lifecycle transition delivered to the subject registry.
type Event struct {
	Type EventType
	Slot Slot  // set for EventBound and EventUpdated
	Err  error // set for EventError
}

// Source delivers slot lifecycle to the consumer. Implementations are
// storage-format-specific and must normalize to the model in this package.
//
// Contract:
//   - Events are delivered in order per slot: Bound before Updated; Deleted is
//     terminal and no further events follow for that slot.
//   - Re-delivery is permitted: consumers must be idempotent (Bound twice ==
//     once; Bound after Deleted re-registers).
//   - Clean/Destroying records are never emitted as Bound/Updated.
//   - Every bound slot present when Watch starts is re-delivered as a Bound
//     event (restart recovery); a consumer that called List first must treat
//     the duplicates as no-ops.
type Source interface {
	// Watch streams slot lifecycle events until ctx is canceled or the
	// underlying store becomes unrecoverable (error, channel closed).
	Watch(ctx context.Context) (<-chan Event, error)

	// List returns a consistent snapshot of all currently bound slots
	// (egress restart: every live subject re-enters denying).
	List(ctx context.Context) ([]Slot, error)
}

// Parser converts one raw slot record into the normalized model. Kept separate
// from Source so a storage-format change is confined to the parser and fails
// closed on any unknown shape (e.g. an unsupported slot version) instead of
// mis-parsing. Parse must enforce the required invariants documented on Slot.
type Parser interface {
	Parse(raw []byte) (Slot, error)
}
