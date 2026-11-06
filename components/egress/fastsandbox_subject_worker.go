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

package main

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

const subjectLifecycleDeadline = 2 * time.Second
const subjectLifecycleRetryDelay = 100 * time.Millisecond

// All fields except done and immutable configuration are guarded by s.mu.
// Only run uses client methods, including Pending: handlers never wait on its
// mutex. Neither registry callbacks nor the worker acquire an action gate.
type fastSandboxSubjectDrain struct {
	wake    chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc
	current *fastSandboxSubjectEpoch
	// Unbounded: action intents are recorded only inside the serial actionGate,
	// so growth is limited by action rate; stale intents are dropped before send.
	// Child publication replaces the queue with the current replay set.
	queue    []fastSandboxSubjectIntent
	degraded map[string]bool
	deadline time.Duration
	fences   map[subject.Subject]subject.Fencing // successful registrations minus actual retirements; observation only
}

type fastSandboxSubjectEpoch struct {
	client *revision.SubjectLifecycleClient
	ctx    context.Context
	cancel context.CancelFunc
}

type fastSandboxSubjectIntent struct {
	subj   subject.Subject
	fence  subject.Fencing
	target revision.SubjectTarget
	unload bool
}

func (s *fastSandboxPolicyServer) startSubjectDrain() {
	ctx, cancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	d := &fastSandboxSubjectDrain{wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel, deadline: subjectLifecycleDeadline, fences: make(map[subject.Subject]subject.Fencing)}
	for subj, fence := range s.subjFence {
		d.fences[subj] = fence
	}
	s.subjectDrain = d
	s.mu.Unlock()
	go d.run(ctx, s)
}

func (d *fastSandboxSubjectDrain) signal() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Publication and replay are one mu transaction, so later SETs follow replay.
// fences mirrors successful registrations and actual Unregister/eviction,
// independently of best-effort file purges. No registry lock is needed here.
func (s *fastSandboxPolicyServer) publishSubjectClient(client *revision.SubjectLifecycleClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.subjectDrain
	if d == nil {
		return
	}
	if d.current != nil {
		d.current.cancel()
	}
	ctx, cancel := context.WithCancel(s.ctx)
	d.current = &fastSandboxSubjectEpoch{client: client, ctx: ctx, cancel: cancel}
	d.queue = nil // old process intents cannot apply to the new sequence owner
	d.degraded = make(map[string]bool)
	subjects := make([]subject.Subject, 0, len(d.fences))
	for subj := range d.fences {
		subjects = append(subjects, subj)
	}
	sort.Slice(subjects, func(i, j int) bool { return subjects[i] < subjects[j] })
	for _, subj := range subjects {
		s.recordSubjectIntentLocked(subj, d.fences[subj], false)
	}
	d.signal()
}

func (s *fastSandboxPolicyServer) detachSubjectClient(client *revision.SubjectLifecycleClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.subjectDrain
	if d != nil && d.current != nil && d.current.client == client {
		d.current.cancel()
		d.current = nil
		d.queue = nil
		d.signal()
	}
}

// Called only under mu, after local transition publication. No IPC or client
// mutex acquisition is permitted here, even for a status probe.
func (s *fastSandboxPolicyServer) recordSubjectIntentLocked(subj subject.Subject, fence subject.Fencing, unload bool) {
	d := s.subjectDrain
	if d == nil {
		return
	}
	target, err := fastSandboxSubjectTarget(strings.TrimPrefix(string(subj), "s-"), fence)
	if err != nil {
		log.Warnf("subject lifecycle target encoding failed")
		return
	} // invalid/failed registrations have no IPC target
	if unload {
		if current, ok := d.fences[subj]; ok && current.Matches(fence) {
			delete(d.fences, subj)
		}
	} else {
		d.fences[subj] = fence
	}
	d.queue = append(d.queue, fastSandboxSubjectIntent{subj: subj, fence: fence, target: target, unload: unload})
	d.signal()
}

func (s *fastSandboxPolicyServer) subjectIntentCurrentLocked(intent fastSandboxSubjectIntent) bool {
	fence, exists := s.subjectDrain.fences[intent.subj]
	if intent.unload {
		return !exists || !fence.Matches(intent.fence)
	}
	return exists && fence.Matches(intent.fence)
}

func (s *fastSandboxPolicyServer) subjectLifecycleDegraded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subjectDrain == nil {
		return nil
	}
	var reasons []string
	for reason := range s.subjectDrain.degraded {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	return reasons
}

// The vocabulary is closed; never expose targets, socket paths, response bodies
// or error text. Confirmed failures remain visible until child reconstruction.
func subjectLifecycleReason(receipt revision.LifecycleReceipt) string {
	switch receipt.Result.Outcome {
	case "subject_conflict", "subject_failed", "subject_capacity_exhausted", "subject_generation_mismatch", "invalid_target":
		return receipt.Result.Outcome
	default:
		return "subject_ipc_unavailable"
	}
}

func (d *fastSandboxSubjectDrain) recordResult(s *fastSandboxPolicyServer, epoch *fastSandboxSubjectEpoch, intent fastSandboxSubjectIntent, receipt revision.LifecycleReceipt, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.current != epoch || !s.subjectIntentCurrentLocked(intent) {
		return
	}
	if err == nil || receipt.LifecycleSequence != 0 {
		delete(d.degraded, "subject_ipc_unavailable")
	}
	if err != nil {
		reason := subjectLifecycleReason(receipt)
		if !d.degraded[reason] {
			log.Warnf("subject lifecycle degraded: %s", reason)
		}
		d.degraded[reason] = true
	}
}

func (d *fastSandboxSubjectDrain) run(ctx context.Context, s *fastSandboxPolicyServer) {
	defer close(d.done)
	defer func() {
		s.mu.Lock()
		if d.current != nil {
			d.current.cancel()
		}
		s.mu.Unlock()
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		epoch := d.current
		if epoch == nil || len(d.queue) == 0 {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-d.wake:
			}
			continue
		}
		intent := d.queue[0]
		d.queue[0] = fastSandboxSubjectIntent{}
		d.queue = d.queue[1:]
		valid := s.subjectIntentCurrentLocked(intent)
		s.mu.Unlock()
		if !valid {
			continue
		}
		// Cancellation on shutdown or replacement bounds even a pending old
		// command; each send/reconcile gets a fresh independent deadline.
		commandCtx, cancel := context.WithTimeout(epoch.ctx, d.deadline)
		stop := context.AfterFunc(ctx, cancel)
		var receipt revision.LifecycleReceipt
		var err error
		if intent.unload {
			receipt, err = epoch.client.Unload(commandCtx, intent.target)
		} else {
			receipt, err = epoch.client.Register(commandCtx, intent.target)
		}
		stop()
		cancel()
		d.recordResult(s, epoch, intent, receipt, err)
		for epoch.client.Pending() && ctx.Err() == nil && epoch.ctx.Err() == nil {
			// Keep the exact frozen command, even if its fence became stale
			// after send: allocating a successor sequence would be unsafe.
			timer := time.NewTimer(subjectLifecycleRetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-epoch.ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
			if epoch.ctx.Err() != nil {
				break
			}
			reconcileCtx, cancel := context.WithTimeout(epoch.ctx, d.deadline)
			stop := context.AfterFunc(ctx, cancel)
			confirmed, reconcileErr := epoch.client.Reconcile(reconcileCtx)
			stop()
			cancel()
			receipt = revision.LifecycleReceipt{}
			if confirmed != nil {
				receipt = *confirmed
			}
			d.recordResult(s, epoch, intent, receipt, reconcileErr)
		}
		// A confirmed non-200 receipt consumes its sequence and is never
		// automatically resubmitted. Only Reconcile can retry a pending send.
	}
}
