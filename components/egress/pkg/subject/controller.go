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
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/slotsource"
	"github.com/alibaba/opensandbox/internal/safego"
	"k8s.io/apimachinery/pkg/util/wait"
)

// Controller drives the subject lifecycle from the slot source: it rescans on
// start (every live subject re-enters denying), then applies slot events to
// the registry and the platform hooks.
//
// Fail-closed rule: a hook failure (deny-first install) keeps the subject
// denying; OnRegistered is retried with backoff until it succeeds or the
// context ends. The subject can never activate without enforcement in place.
type Controller struct {
	reg   Registry
	hooks LifecycleHooks
}

// NewController wires a registry and its platform hooks.
func NewController(reg Registry, hooks LifecycleHooks) *Controller {
	return &Controller{reg: reg, hooks: hooks}
}

// Run consumes the slot source's event stream until ctx is canceled. The
// watch re-delivers every bound slot at start (restart recovery: each live
// subject re-enters denying through the same registration path), so there is
// no List-then-Watch handoff race and no separate rescan step. It returns
// nil on normal cancellation, or the first unrecoverable source error.
// Event-level parse failures (EventError) fail closed and are logged.
func (c *Controller) Run(ctx context.Context, src slotsource.Source) error {
	events, err := src.Watch(ctx)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return errors.New("slot source watch channel closed")
			}
			if err := c.applyEvent(ctx, ev); err != nil {
				return err
			}
		}
	}
}

func (c *Controller) applyEvent(ctx context.Context, ev slotsource.Event) error {
	switch ev.Type {
	case slotsource.EventBound:
		_, err := c.registerSubject(ctx, ev.Slot)
		return err
	case slotsource.EventUpdated:
		state, err := c.registerSubject(ctx, ev.Slot)
		if err != nil {
			return err
		}
		if state == StateActive && c.hooks != nil {
			// Fencing unchanged and the subject is active: dispatch-relevant
			// slot fields (veth/gateway/DNS path) may have moved. Reconcile
			// enforcement without resetting the policy.
			s := FromSandboxUID(ev.Slot.Owner.SandboxUID)
			if err := c.hooks.OnSlotUpdated(s, ev.Slot); err != nil {
				return err
			}
		}
		return nil
	case slotsource.EventDeleted:
		s := FromSandboxUID(ev.Slot.Owner.SandboxUID)
		prev := c.reg.Unregister(s)
		if prev == StateAbsent {
			return nil
		}
		if c.hooks != nil {
			if err := c.hooks.OnUnloaded(s, ev.Slot); err != nil {
				return err
			}
		}
		log.Infof("subject %s unloaded (was %s)", s, prev)
		return nil
	case slotsource.EventError:
		// Fail closed: an unparseable slot is never treated as active. The
		// source keeps delivering; we log and continue denying.
		log.Errorf("slot source event error (fail closed): %v", ev.Err)
		return nil
	default:
		return nil
	}
}

// registerSubject registers a slot (deny-first) and retries the deny-first
// hook until it succeeds. It never marks the subject active; activation only
// happens via Registry.ApplyPolicy once a policy push lands. Registration and
// enforcement are atomic with respect to ApplyPolicy (see
// Registry.RegisterAndEnforce), so a retried deny-first install can never
// clobber an already-applied policy. Returns the subject state after
// registration (StateDenying for fresh/rebound, StateActive when the subject
// was already active under the same fencing).
func (c *Controller) registerSubject(ctx context.Context, slot slotsource.Slot) (State, error) {
	if slot.Phase != slotsource.PhaseBound {
		// The source filters non-bound phases, but a stale event must not
		// open a subject either.
		return StateAbsent, nil
	}
	s := FromSandboxUID(slot.Owner.SandboxUID)
	key := SubjectKey{NetNSPath: slot.HostNetnsPath, SourceIP: slot.IP}
	fence := FromSlotOwner(slot.Owner)
	if c.hooks == nil {
		return c.reg.Register(s, key, fence), nil
	}
	// Deny-first install is the fail-closed guarantee: retry until it
	// succeeds, so a subject can never activate without enforcement.
	var state State
	if err := wait.ExponentialBackoffWithContext(ctx, wait.Backoff{
		Duration: 100 * time.Millisecond,
		Factor:   2,
		Steps:    6,
		Cap:      2 * time.Second,
	}, func(ctx context.Context) (bool, error) {
		st, err := c.reg.RegisterAndEnforce(s, key, fence, func() error {
			return c.hooks.OnRegistered(s, slot)
		})
		state = st
		if err != nil {
			log.Warnf("subject %s: deny-first install failed (retrying): %v", s, err)
			return false, nil
		}
		log.Infof("subject %s registered, state=%s", s, st)
		return true, nil
	}); err != nil {
		return StateAbsent, err
	}
	// Registry lock released: best-effort follow-up (pending push flush).
	if c.hooks != nil {
		c.hooks.OnRegisteredComplete(s, slot)
	}
	return state, nil
}

// StartWatch is a convenience wrapper running Run in the background with
// panic-guarded logging; failures are returned on the channel.
func (c *Controller) StartWatch(ctx context.Context, src slotsource.Source) <-chan error {
	errCh := make(chan error, 1)
	safego.Go(func() {
		errCh <- c.Run(ctx, src)
	})
	return errCh
}
