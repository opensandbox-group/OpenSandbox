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

// Package revision provides the in-memory Go coordinator for OSEP-0023.
// It has no live Vault, TLS, or IPC wiring. Callers must build and validate the
// complete policy/Vault snapshot under their mutation barrier, gate reads on
// Confirmed, and finalize their public store under that same barrier. The
// unused revisionruntime ProcessSession adapter owns transport authentication, a fresh
// receiver directory/generation, readiness, and local cleanup. Live launch,
// restart recovery, and remote connection teardown remain integration work.
// New is for a fresh generation pair, not recovery of an existing receiver.
package revision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"sync"
	"unicode/utf8"
)

var (
	ErrInvalid         = errors.New("invalid snapshot or coordinator configuration")
	ErrBusy            = errors.New("revision operation in progress")
	ErrClosed          = errors.New("revision coordinator closed")
	ErrIndeterminate   = errors.New("revision outcome indeterminate")
	ErrPrepareRejected = errors.New("snapshot prepare rejected and aborted")
)

// Identity matches the Python receiver's generation/epoch/digest identity.
// It contains no snapshot payload. Wire encoding belongs to the IPC adapter.
type Identity struct {
	ControlGeneration string `json:"controlGeneration"`
	SubjectGeneration string `json:"subjectGeneration"`
	DecisionEpoch     int64  `json:"decisionEpoch"`
	VaultRevision     int64  `json:"vaultRevision"`
	PolicyEpoch       int64  `json:"policyEpoch"`
	Digest            string `json:"digest"`
}

// Transport binds one authenticated receiver/session to this coordinator.
// Errors are conservatively ambiguous, even cancellation: work may have reached
// the receiver. Methods must honor context cancellation and return owned metadata
// values. Prepare receives its own byte copy; the caller must not concurrently
// mutate its input. Late commands must obey the receiver's exact-identity fencing.
// Transport errors are never propagated, since they may contain credentials.
type Transport interface {
	Prepare(context.Context, Identity, []byte) (Identity, error)
	Commit(context.Context, Identity) (Identity, error)
	Abort(context.Context, Identity) (Identity, error)
	Readback(context.Context) (*Identity, error)
}

// Coordinator serializes one subject's transactions without holding its mutex
// over transport calls. It retains only confirmed and uncertain metadata, never
// snapshot payloads. The zero value is not usable; construct it with New.
type Coordinator struct {
	mu                       sync.Mutex
	transport                Transport
	control, subject         string
	limit                    int
	epoch                    int64
	confirmed, pending       *Identity
	commitSent, busy, closed bool
	cancel                   context.CancelFunc
}

// New starts with unknown state. Even authoritative empty state requires Apply.
func New(control, subject string, transport Transport, maxSnapshotBytes int) (*Coordinator, error) {
	valid := func(s string) bool { return utf8.ValidString(s) && len(s) > 0 && utf8.RuneCountInString(s) <= 128 }
	if !valid(control) || !valid(subject) || transport == nil || maxSnapshotBytes <= 0 {
		return nil, ErrInvalid
	}
	return &Coordinator{transport: transport, control: control, subject: subject, limit: maxSnapshotBytes}, nil
}

func copyIdentity(r *Identity) *Identity {
	if r == nil {
		return nil
	}
	copy := *r
	return &copy
}

func same(a, b *Identity) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func (c *Coordinator) available() error {
	if c.closed {
		return ErrClosed
	}
	if c.busy {
		return ErrBusy
	}
	return nil
}

// Confirmed returns a metadata copy, or nil for unknown initial state. It refuses
// reads during a mutation or after commit may have reached the receiver. A failed
// prepare can leave an abort retry pending, but prepare is inert, so the previous
// confirmed revision remains authoritative while new mutations stay fenced.
func (c *Coordinator) Confirmed() (*Identity, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.available(); err != nil {
		return nil, err
	}
	if c.pending != nil && c.commitSent {
		return nil, ErrIndeterminate
	}
	return copyIdentity(c.confirmed), nil
}

// Apply allocates a monotonically increasing decision epoch and sends exact
// snapshot bytes. Public Vault revisions may restart after a delete/recreate.
// A failed prepare is retired by an acknowledged abort. Once commit is sent,
// only exact commit acknowledgement/readback can publish the candidate; abort
// must never be used to guess that a commit was rolled back.
func (c *Coordinator) Apply(ctx context.Context, vaultRevision, policyEpoch int64, payload []byte) (Identity, error) {
	c.mu.Lock()
	if err := c.available(); err != nil {
		c.mu.Unlock()
		return Identity{}, err
	}
	if c.pending != nil {
		c.mu.Unlock()
		return Identity{}, ErrIndeterminate
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return Identity{}, err
	}
	if vaultRevision < 0 || policyEpoch < 0 || len(payload) > c.limit || c.epoch == math.MaxInt64 {
		c.mu.Unlock()
		return Identity{}, ErrInvalid
	}
	data := append([]byte(nil), payload...)
	digest := sha256.Sum256(data)
	c.epoch++
	r := Identity{c.control, c.subject, c.epoch, vaultRevision, policyEpoch, hex.EncodeToString(digest[:])}
	c.pending, c.commitSent, c.busy = &r, false, true
	ctx, c.cancel = context.WithCancel(ctx)
	c.mu.Unlock()
	ack, err := c.transport.Prepare(ctx, r, data)
	if err != nil || ack != r {
		ack, err = c.transport.Abort(ctx, r)
		if err == nil && ack == r {
			return Identity{}, c.finish(false, ErrPrepareRejected)
		}
		return Identity{}, c.finish(false, ErrIndeterminate)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Identity{}, c.finish(false, ErrClosed)
	}
	c.commitSent = true
	c.mu.Unlock()
	ack, err = c.transport.Commit(ctx, r)
	if err != nil || ack != r {
		return Identity{}, c.finish(false, ErrIndeterminate)
	}
	if err := c.finish(true, nil); err != nil {
		return Identity{}, err
	}
	return r, nil
}

// Reconcile resolves only the outstanding operation, without resending payloads.
// After a prepare failure it retries exact abort. After a commit attempt it first
// reads back: an exact candidate confirms success; an exact previous state permits
// an idempotent commit retry, but does NOT establish rollback. Unknown foreign
// metadata or unavailable readback leaves all snapshot-affecting work blocked.
func (c *Coordinator) Reconcile(ctx context.Context) (*Identity, error) {
	c.mu.Lock()
	if err := c.available(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	if c.pending == nil {
		r := copyIdentity(c.confirmed)
		c.mu.Unlock()
		return r, nil
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	r, sent, previous := *c.pending, c.commitSent, copyIdentity(c.confirmed)
	c.busy = true
	ctx, c.cancel = context.WithCancel(ctx)
	c.mu.Unlock()
	if !sent {
		ack, err := c.transport.Abort(ctx, r)
		if err != nil || ack != r {
			return nil, c.finish(false, ErrIndeterminate)
		}
		if err := c.finish(false, nil); err != nil {
			return nil, err
		}
		return previous, nil
	}
	active, err := c.transport.Readback(ctx)
	if err != nil {
		return nil, c.finish(false, ErrIndeterminate)
	}
	if !same(active, &r) {
		if !same(active, previous) {
			return nil, c.finish(false, ErrIndeterminate)
		}
		ack, err := c.transport.Commit(ctx, r)
		if err != nil || ack != r {
			return nil, c.finish(false, ErrIndeterminate)
		}
	}
	if err := c.finish(true, nil); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Coordinator) finish(committed bool, err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.busy = false
	if c.closed {
		return ErrClosed
	}
	if committed {
		c.confirmed = copyIdentity(c.pending)
	}
	if err != ErrIndeterminate {
		c.pending = nil
	}
	return err
}

// Close permanently fences local completion and cancels in-flight transport
// without waiting. The owning adapter must also fence/close the remote receiver
// and connections; cancellation alone cannot recall a command already delivered.
func (c *Coordinator) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.confirmed, c.pending = nil, nil
	if c.cancel != nil {
		c.cancel()
	}
}
