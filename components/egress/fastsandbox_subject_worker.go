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
	"crypto/sha256"
	"errors"
	"math"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
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
	queue            []fastSandboxSubjectIntent
	degraded         map[string]bool
	unknownVaults    map[subject.Subject]bool // live Vaults invalidated under the current registered fence
	snapshotFailures map[subject.Subject]bool // current committed inputs could not produce a replayable snapshot
	revertPending    map[subject.Subject]bool // failed readiness rollback; retry before the next ACK gate
	deadline         time.Duration
	snapshots        map[subject.Subject]*fastSandboxSubjectSnapshot // immutable latest committed inputs, retained across child restart
	fences           map[subject.Subject]subject.Fencing             // successful registrations minus actual retirements; observation only
	acked            map[subject.Subject]fastSandboxSubjectACK       // confirmed intent, never the mirror at confirmation time
	registrations    map[subject.Subject]uint64                      // current registration instance, even when the fence is reused
	nextRegistration uint64                                          // drain-wide monotonic sequence; never reset on Unload or child replacement

	// Optional test observer, called under s.mu after acknowledged Unload.
	epochCountForTest func(int)
}

type fastSandboxSubjectEpoch struct {
	client  *revision.SubjectLifecycleClient
	handoff mitmproxy.SubjectRevisionIPCConfig
	ctx     context.Context
	cancel  context.CancelFunc
}

type fastSandboxSubjectIntent struct {
	subj            subject.Subject
	fence           subject.Fencing
	target          revision.SubjectTarget
	unload          bool
	suspend         bool
	sourceAddresses []netip.Addr // frozen attachment keys; never part of Target identity
	epoch           *fastSandboxSubjectEpoch
	snapshot        *fastSandboxSubjectSnapshot
	registration    uint64
}

type fastSandboxSubjectSnapshot struct {
	payload                    []byte
	digest                     [sha256.Size]byte
	vaultRevision, policyEpoch int64
}

type fastSandboxSubjectACK struct {
	fence        subject.Fencing
	controlEpoch string
	digest       [sha256.Size]byte
	registration uint64
}

// Call under s.mu. This is an observation of existing recovery state, not a
// change to the legacy rehydrate or relay recovery protocol.
func (s *fastSandboxPolicyServer) subjectVaultAuthoritativeLocked(subj subject.Subject) bool {
	entry := s.vaults[subj]
	return !entry.vaultStateUnknown && entry.degraded == ""
}

// Every Vault entry publication passes here under s.mu. Entry authority, not
// policy degradation, sealing or the process recovery latch, controls suspend.
// IPC remains exclusively in the drain worker, outside business locks.
func (s *fastSandboxPolicyServer) setSubjectVaultEntryLocked(subj subject.Subject, entry vaultEntry) {
	wasAuthoritative := s.subjectVaultAuthoritativeLocked(subj)
	s.vaults[subj] = entry
	d := s.subjectDrain
	if d == nil || !wasAuthoritative || s.subjectVaultAuthoritativeLocked(subj) {
		return
	}
	fence, registered := d.fences[subj]
	if !registered || !fence.Matches(entry.fence) {
		return
	}
	target := mustSubjectTarget(subj, fence)
	d.dropInstallsLocked(target) // never coalesce a recovery install ahead of suspend
	delete(d.acked, subj)
	d.queue = append(d.queue, fastSandboxSubjectIntent{subj: subj, fence: fence, target: target, suspend: true, epoch: d.current, registration: d.registrations[subj]})
	d.signal()
}

// Called under policyCommitMu -> s.mu; no IPC, registry callback or wait.
func (s *fastSandboxPolicyServer) subjectSnapshotAcknowledgedLocked(subj subject.Subject, fence subject.Fencing) bool {
	d := s.subjectDrain
	if d == nil {
		return true
	}
	if !s.subjectVaultAuthoritativeLocked(subj) || d.snapshotFailures[subj] || d.current == nil || d.snapshots[subj] == nil {
		return false
	}
	ack, ok := d.acked[subj]
	registration := d.registrations[subj]
	return ok && registration != 0 && ack.registration == registration && ack.fence.Matches(fence) && ack.controlEpoch == d.current.handoff.ControlGeneration && ack.digest == d.snapshots[subj].digest
}

func (s *fastSandboxPolicyServer) startSubjectDrain() {
	ctx, cancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	d := &fastSandboxSubjectDrain{wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel, deadline: subjectLifecycleDeadline, fences: make(map[subject.Subject]subject.Fencing), snapshots: make(map[subject.Subject]*fastSandboxSubjectSnapshot), degraded: make(map[string]bool), registrations: make(map[subject.Subject]uint64)}
	for subj, fence := range s.subjFence {
		d.fences[subj] = fence
		d.nextRegistration++
		d.registrations[subj] = d.nextRegistration
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
func (s *fastSandboxPolicyServer) publishSubjectClient(client *revision.SubjectLifecycleClient, handoff mitmproxy.SubjectRevisionIPCConfig) {
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
	d.current = &fastSandboxSubjectEpoch{client: client, handoff: handoff, ctx: ctx, cancel: cancel}
	d.acked = make(map[subject.Subject]fastSandboxSubjectACK)
	d.queue = nil // old process intents cannot apply to the new sequence owner
	d.degraded = make(map[string]bool)
	subjects := make([]subject.Subject, 0, len(d.fences))
	for subj := range d.fences {
		subjects = append(subjects, subj)
	}
	sort.Slice(subjects, func(i, j int) bool { return subjects[i] < subjects[j] })
	for _, subj := range subjects {
		s.recordSubjectIntentLocked(subj, d.fences[subj], false)
		if snapshot := d.snapshots[subj]; snapshot != nil && s.subjectVaultAuthoritativeLocked(subj) {
			d.enqueueInstallLocked(fastSandboxSubjectIntent{subj: subj, fence: d.fences[subj], target: mustSubjectTarget(subj, d.fences[subj]), epoch: d.current, snapshot: snapshot, registration: d.registrations[subj]})
		}
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
		d.acked = nil
		d.queue = nil
		d.signal()
	}
}

// Called only under mu, after local transition publication. No IPC or client
// mutex acquisition is permitted here, even for a status probe.
func (s *fastSandboxPolicyServer) recordSubjectIntentLocked(subj subject.Subject, fence subject.Fencing, unload bool) {
	d := s.subjectDrain
	// A zero fence names no registered generation: a retained rebind token
	// whose registry entry was already removed carries one, and the earlier
	// attempt already recorded the real Unload.
	if d == nil || fence == (subject.Fencing{}) {
		return
	}
	target, err := fastSandboxSubjectTarget(strings.TrimPrefix(string(subj), "s-"), fence)
	if err != nil {
		log.Warnf("subject %s: lifecycle target encoding failed", subj)
		return
	} // invalid/failed registrations have no IPC target
	registration := d.registrations[subj]
	if unload {
		d.dropInstallsLocked(target)
		if current, ok := d.fences[subj]; ok && current.Matches(fence) {
			delete(d.fences, subj)
			delete(d.registrations, subj)
			delete(d.snapshots, subj)
			delete(d.acked, subj)
			delete(d.unknownVaults, subj)
			delete(d.snapshotFailures, subj)
			delete(d.revertPending, subj)
		}
	} else {
		if old, exists := d.fences[subj]; exists && !old.Matches(fence) {
			d.dropInstallsLocked(mustSubjectTarget(subj, old))
			delete(d.snapshots, subj)
			delete(d.acked, subj)
			delete(d.unknownVaults, subj)
			delete(d.snapshotFailures, subj)
			delete(d.revertPending, subj)
		}
		d.fences[subj] = fence
		d.nextRegistration++
		registration = d.nextRegistration
		d.registrations[subj] = registration
	}
	intent := fastSandboxSubjectIntent{subj: subj, fence: fence, target: target, unload: unload, epoch: d.current, registration: registration}
	if !unload {
		if address := s.subjAtt[subj].IP; address.IsValid() {
			intent.sourceAddresses = []netip.Addr{address}
		}
	}
	d.queue = append(d.queue, intent)
	d.signal()
}

// Targets here have already passed recordSubjectIntentLocked validation.
func mustSubjectTarget(subj subject.Subject, fence subject.Fencing) revision.SubjectTarget {
	target, _ := fastSandboxSubjectTarget(strings.TrimPrefix(string(subj), "s-"), fence)
	return target
}

// Called after a local mutation commits, while policyCommitMu is still held.
// ActiveSnapshot returns an owned clone of the Store's committed render cache.
// Marshal stays under s.mu so the payload, revisions and fence freeze together;
// no registry lookup, resolver, or IPC is performed here.
func (s *fastSandboxPolicyServer) recordBindingSnapshot(subj subject.Subject, fence subject.Fencing) {
	if s.subjectDrain == nil { // set once at attach, before handlers start
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordSubjectSnapshotLocked(subj, fence)
}

// Shared producer seam for callers already holding policyCommitMu and s.mu.
// Policy validation/rendering belongs to the committing path: only the Store's
// committed snapshot and recorded policy epoch are read here. In particular,
// never call vaultValidationPolicy here (it acquires s.mu itself).
func (s *fastSandboxPolicyServer) recordSubjectSnapshotLocked(subj subject.Subject, fence subject.Fencing) {
	d := s.subjectDrain
	if d == nil {
		return
	}
	if current, ok := d.fences[subj]; !ok || !current.Matches(fence) {
		return
	}
	if !s.subjectVaultAuthoritativeLocked(subj) {
		return // retain the last payload; unrestored state is not committed empty
	}
	// The registry itself has no capacity cap. Bound the observation mirror
	// independently to the receiver's subject budget without failing SET.
	if d.snapshots[subj] == nil && len(d.snapshots) >= fastSandboxRevisionMaxSubjects {
		d.markSnapshotFailureLocked(subj, "subject_capacity_exhausted")
		return
	}
	var snapshot credentialvault.ActiveSnapshot
	var err error
	if entry, ok := s.vaults[subj]; ok && entry.store != nil {
		snapshot, err = entry.store.ActiveSnapshot()
	}
	if err != nil && !errors.Is(err, credentialvault.ErrNotFound) {
		d.markSnapshotFailureLocked(subj, "subject_snapshot_invalid")
		return
	}
	var policyEpoch int64
	if entry := s.policies[subj]; entry != nil {
		policyEpoch = entry.revision
	}
	payload, err := credentialvault.MarshalDecisionSnapshot(snapshot, policyEpoch)
	if err != nil {
		d.markSnapshotFailureLocked(subj, "subject_snapshot_invalid")
		return
	}
	if len(payload) > defaultRevisionMaxSnapshotSize {
		d.markSnapshotFailureLocked(subj, "subject_snapshot_too_large")
		return
	}
	frozen := &fastSandboxSubjectSnapshot{payload: payload, digest: sha256.Sum256(payload), vaultRevision: snapshot.Revision, policyEpoch: policyEpoch}
	d.snapshots[subj] = frozen
	delete(d.snapshotFailures, subj)
	delete(d.unknownVaults, subj)
	d.enqueueInstallLocked(fastSandboxSubjectIntent{subj: subj, fence: fence, target: mustSubjectTarget(subj, fence), epoch: d.current, snapshot: frozen, registration: d.registrations[subj]})
}

// Called under s.mu. Keep the old replay payload, but never let its ACK stand
// in for newer committed inputs that failed to produce a snapshot.
func (d *fastSandboxSubjectDrain) markSnapshotFailureLocked(subj subject.Subject, reason string) {
	if d.snapshotFailures == nil {
		d.snapshotFailures = make(map[subject.Subject]bool)
	}
	d.snapshotFailures[subj] = true
	d.degradeLocked(reason)
}

func (d *fastSandboxSubjectDrain) dropInstallsLocked(target revision.SubjectTarget) {
	kept := d.queue[:0]
	for _, intent := range d.queue {
		if intent.snapshot == nil || intent.target != target {
			kept = append(kept, intent)
		}
	}
	clear(d.queue[len(kept):])
	d.queue = kept
}

func (d *fastSandboxSubjectDrain) enqueueInstallLocked(next fastSandboxSubjectIntent) {
	for i, intent := range d.queue {
		if intent.snapshot == nil || intent.subj != next.subj {
			continue
		}
		if intent.target == next.target && intent.epoch == next.epoch && intent.registration == next.registration {
			d.queue[i] = next // same registration and child: preserve FIFO position
			d.signal()
			return
		}
		// At most one generation per subject has queued installs: a
		// generation change drops the old target first, so one drop suffices.
		d.dropInstallsLocked(intent.target)
		break // different registration/epoch: append after its Register
	}
	d.queue = append(d.queue, next)
	d.signal()
}

func (d *fastSandboxSubjectDrain) degradeLocked(reason string) {
	if !d.degraded[reason] {
		log.Warnf("subject lifecycle degraded: %s", reason)
	}
	d.degraded[reason] = true
}

// Call only at a known/live-to-unknown transition under s.mu. Registration
// checks bound the set to current subjects; bootstrap placeholders never enter.
func (s *fastSandboxPolicyServer) markSubjectVaultUnknownLocked(subj subject.Subject, fence subject.Fencing) {
	d := s.subjectDrain
	if d == nil {
		return
	}
	if current, ok := d.fences[subj]; !ok || !current.Matches(fence) {
		return
	}
	if len(d.unknownVaults) == 0 {
		log.Warnf("subject lifecycle degraded: vault_state_unknown")
	}
	if d.unknownVaults == nil {
		d.unknownVaults = make(map[subject.Subject]bool)
	}
	d.unknownVaults[subj] = true
}

func (s *fastSandboxPolicyServer) clearSubjectVaultUnknownLocked(subj subject.Subject) {
	if s.subjectDrain != nil {
		delete(s.subjectDrain.unknownVaults, subj)
	}
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
	if len(s.subjectDrain.unknownVaults) != 0 {
		reasons = append(reasons, "vault_state_unknown")
	}
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

func (d *fastSandboxSubjectDrain) recordResult(ctx context.Context, s *fastSandboxPolicyServer, epoch *fastSandboxSubjectEpoch, intent fastSandboxSubjectIntent, receipt revision.LifecycleReceipt, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Drain shutdown, like epoch replacement, cancels in-flight I/O; that is
	// not a receiver outage.
	if d.current != epoch || ctx.Err() != nil {
		return
	}
	if err == nil || receipt.LifecycleSequence != 0 {
		delete(d.degraded, "subject_ipc_unavailable")
	}
	if err == nil {
		return
	}
	if intent.suspend && receipt.LifecycleSequence != 0 && (receipt.Result.Outcome == "subject_unknown" || receipt.Result.Outcome == "subject_generation_mismatch") {
		return // confirmed no-op: there is no matching receiver left to suspend
	}
	// IPC availability is session state: a pending command whose fence became
	// stale still blocks the FIFO, so report it. Confirmed outcomes are fenced
	// per subject.
	reason := subjectLifecycleReason(receipt)
	if reason != "subject_ipc_unavailable" && !s.subjectIntentCurrentLocked(intent) {
		return
	}
	d.degradeLocked(reason)
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
	var owner *fastSandboxSubjectEpoch
	installers := make(map[revision.SubjectTarget]*fastSandboxSubjectInstaller)
	retired := make(map[revision.SubjectTarget]bool)
	epochs := make(map[subject.Subject]*int64)
	defer func() {
		for _, installer := range installers {
			installer.coordinator.Close()
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		epoch := d.current
		if epoch == nil || len(d.queue) == 0 {
			s.mu.Unlock()
			if owner != epoch {
				for _, installer := range installers {
					installer.coordinator.Close()
				}
				installers = make(map[revision.SubjectTarget]*fastSandboxSubjectInstaller)
				retired = make(map[revision.SubjectTarget]bool)
				epochs = make(map[subject.Subject]*int64)
				owner = epoch
			}
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
		valid := s.subjectIntentCurrentLocked(intent) && (intent.epoch == nil || intent.epoch == epoch)
		if !valid && !intent.unload {
			d.dropInstallsLocked(intent.target)
		}
		s.mu.Unlock()
		if owner != epoch {
			for _, installer := range installers {
				installer.coordinator.Close()
			}
			installers = make(map[revision.SubjectTarget]*fastSandboxSubjectInstaller)
			retired = make(map[revision.SubjectTarget]bool)
			epochs = make(map[subject.Subject]*int64)
			owner = epoch
		}
		if !valid {
			continue
		}
		if intent.snapshot != nil {
			if installer := installers[intent.target]; installer != nil && !retired[intent.target] {
				if !d.install(ctx, s, epoch, intent, installer) {
					installer.coordinator.Close()
					delete(installers, intent.target)
					retired[intent.target] = true
				}
			}
			continue
		}
		if intent.unload {
			if installer := installers[intent.target]; installer != nil {
				installer.coordinator.Close()
			}
			delete(installers, intent.target)
			delete(retired, intent.target)
		}
		// Cancellation on shutdown or replacement bounds even a pending old
		// command; each send/reconcile gets a fresh independent deadline.
		commandCtx, cancel := context.WithTimeout(epoch.ctx, d.deadline)
		stop := context.AfterFunc(ctx, cancel)
		var receipt revision.LifecycleReceipt
		var err error
		if intent.unload {
			receipt, err = epoch.client.Unload(commandCtx, intent.target)
		} else if intent.suspend {
			receipt, err = epoch.client.Suspend(commandCtx, intent.target)
		} else {
			receipt, err = epoch.client.Register(commandCtx, intent.target, intent.sourceAddresses...)
		}
		stop()
		cancel()
		d.recordResult(ctx, s, epoch, intent, receipt, err)
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
			err = reconcileErr
			d.recordResult(ctx, s, epoch, intent, receipt, err)
		}
		if intent.unload && err == nil {
			s.mu.Lock()
			if d.current == epoch {
				_, registered := d.fences[intent.subj]
				// Rebind can still be between old Unload and new publication.
				// Its token and queued Register also reserve the counter.
				registered = registered || s.rebinds[intent.subj] != nil
				for _, queued := range d.queue {
					if queued.subj == intent.subj && !queued.unload && queued.snapshot == nil && (queued.epoch == nil || queued.epoch == epoch) {
						registered = true
						break
					}
				}
				if !registered {
					delete(epochs, intent.subj)
				}
			}
			if d.epochCountForTest != nil {
				d.epochCountForTest(len(epochs))
			}
			s.mu.Unlock()
		}
		if !intent.unload && !intent.suspend && epoch.ctx.Err() == nil && ctx.Err() == nil {
			if err != nil {
				s.mu.Lock()
				if d.current == epoch {
					d.dropInstallsLocked(intent.target)
				}
				s.mu.Unlock()
				if installer := installers[intent.target]; installer != nil {
					installer.coordinator.Close()
				}
				delete(installers, intent.target)
				retired[intent.target] = true
			} else if installers[intent.target] == nil {
				// Only a newly confirmed Register revives a rejected target.
				delete(retired, intent.target)
				counter := epochs[intent.subj]
				if counter == nil {
					counter = new(int64)
					epochs[intent.subj] = counter
				}
				installer, makeErr := newFastSandboxSubjectInstaller(epoch.handoff, intent.target, counter)
				if makeErr == nil {
					installers[intent.target] = installer
				} else {
					s.mu.Lock()
					if d.current == epoch && s.subjectIntentCurrentLocked(intent) {
						d.degradeLocked("subject_installer_invalid")
						d.dropInstallsLocked(intent.target)
					}
					s.mu.Unlock()
					retired[intent.target] = true
				}
			}
		}
		// A confirmed non-200 receipt consumes its sequence and is never
		// automatically resubmitted. Only Reconcile can retry a pending send.
	}
}

// Only the drain goroutine uses these fields. The wrapper preserves the generic
// Coordinator's conservative sidecar semantics while exposing v2 confirmed
// rejections to this installation-only owner. After rejection, Abort is local:
// no further command may target that failed generation during this transaction.
type fastSandboxSubjectInstaller struct {
	coordinator *revision.Coordinator
	transport   *fastSandboxInstallTransport
}

type fastSandboxInstallTransport struct {
	revision.Transport
	rejection *revision.SubjectTransportRejection
	// The Coordinator owns local epochs; the wire offset preserves monotonic
	// decisionEpoch for this subject across generations in the same child.
	base int64
	last *int64
}

func (t *fastSandboxInstallTransport) observe(err error) error {
	var rejection *revision.SubjectTransportRejection
	if errors.As(err, &rejection) {
		t.rejection = rejection
	}
	return err
}

func (t *fastSandboxInstallTransport) Prepare(ctx context.Context, id revision.Identity, payload []byte) (revision.Identity, error) {
	if id.DecisionEpoch > math.MaxInt64-t.base {
		return revision.Identity{}, t.observe(&revision.SubjectTransportRejection{Outcome: "decision_epoch_exhausted"})
	}
	id.DecisionEpoch += t.base
	*t.last = id.DecisionEpoch
	ack, err := t.Transport.Prepare(ctx, id, payload)
	return t.localIdentity(ack), t.observe(err)
}
func (t *fastSandboxInstallTransport) Commit(ctx context.Context, id revision.Identity) (revision.Identity, error) {
	id.DecisionEpoch += t.base
	ack, err := t.Transport.Commit(ctx, id)
	return t.localIdentity(ack), t.observe(err)
}
func (t *fastSandboxInstallTransport) Abort(ctx context.Context, id revision.Identity) (revision.Identity, error) {
	if t.rejection != nil {
		return revision.Identity{}, t.rejection
	}
	id.DecisionEpoch += t.base
	ack, err := t.Transport.Abort(ctx, id)
	return t.localIdentity(ack), t.observe(err)
}
func (t *fastSandboxInstallTransport) Readback(ctx context.Context) (*revision.Identity, error) {
	ack, err := t.Transport.Readback(ctx)
	if ack != nil {
		local := t.localIdentity(*ack)
		ack = &local
	}
	return ack, t.observe(err)
}

func (t *fastSandboxInstallTransport) localIdentity(id revision.Identity) revision.Identity {
	if id.DecisionEpoch <= t.base {
		return revision.Identity{}
	}
	id.DecisionEpoch -= t.base
	return id
}

func newFastSandboxSubjectInstaller(h mitmproxy.SubjectRevisionIPCConfig, target revision.SubjectTarget, last *int64) (*fastSandboxSubjectInstaller, error) {
	transport, err := revision.NewSubjectUnixTransport(h.SocketPath, h.SessionToken, h.ControlGeneration, target, h.MaxSnapshotBytes)
	if err != nil {
		return nil, err
	}
	observed := &fastSandboxInstallTransport{Transport: transport, base: *last, last: last}
	coordinator, err := revision.New(h.ControlGeneration, target.SubjectGeneration, observed, h.MaxSnapshotBytes)
	if err != nil {
		return nil, err
	}
	return &fastSandboxSubjectInstaller{coordinator: coordinator, transport: observed}, nil
}

func subjectInstallReason(installer *fastSandboxSubjectInstaller, err error) string {
	if rejection := installer.transport.rejection; rejection != nil {
		switch rejection.Outcome {
		case "subject_unknown", "subject_generation_mismatch", "subject_failed", "control_generation_mismatch", "invalid_target":
			return rejection.Outcome
		case "revision_rejected":
			return "subject_prepare_rejected"
		default:
			return "subject_install_rejected"
		}
	}
	if errors.Is(err, revision.ErrInvalid) {
		return "subject_snapshot_invalid"
	}
	return "subject_ipc_unavailable"
}

// install returns false only for confirmed failure. An uncertain operation owns
// the FIFO until exact Readback/Abort reconciliation or child cancellation; it
// never resends an unconfirmed attempt. Only a confirmed Abort permits a fresh
// Apply with the same payload and a new decision epoch.
func (d *fastSandboxSubjectDrain) install(ctx context.Context, s *fastSandboxPolicyServer, epoch *fastSandboxSubjectEpoch, intent fastSandboxSubjectIntent, installer *fastSandboxSubjectInstaller) bool {
	commandCtx, cancel := context.WithTimeout(epoch.ctx, d.deadline)
	stop := context.AfterFunc(ctx, cancel)
	candidate, err := installer.coordinator.Apply(commandCtx, intent.snapshot.vaultRevision, intent.snapshot.policyEpoch, intent.snapshot.payload)
	stop()
	cancel()
	installed := err == nil
	for {
		if ctx.Err() != nil || epoch.ctx.Err() != nil {
			return true
		}
		reason := subjectInstallReason(installer, err)
		terminal := installer.transport.rejection != nil || errors.Is(err, revision.ErrInvalid)
		// Apply also returns ErrPrepareRejected after a transport failure when
		// Abort succeeds on the original context. That is confirmed non-install,
		// not a receiver rejection, and permits the same retry as Reconcile.
		aborted := errors.Is(err, revision.ErrPrepareRejected)
		s.mu.Lock()
		if d.current == epoch {
			if installed && s.subjectIntentCurrentLocked(intent) {
				// A newer mirror may already exist. Stamp only the acknowledged
				// intent, so that successor remains gated until its own ACK.
				d.acked[intent.subj] = fastSandboxSubjectACK{fence: intent.fence, controlEpoch: epoch.handoff.ControlGeneration, digest: intent.snapshot.digest, registration: intent.registration}
			}
			if installed || terminal {
				delete(d.degraded, "subject_ipc_unavailable")
			}
			if err != nil && (reason == "subject_ipc_unavailable" || s.subjectIntentCurrentLocked(intent)) {
				d.degradeLocked(reason)
			}
			if terminal {
				// A confirmed rejection revokes prior success for this registration,
				// even when the rejected payload has the same digest. A late result
				// from a retired registration must not revoke its successor's ACK.
				if d.acked[intent.subj].registration == intent.registration {
					delete(d.acked, intent.subj)
				}
				d.dropInstallsLocked(intent.target)
			}
		}
		retry := (err == nil || aborted) && !installed && d.retryInstallLocked(s, epoch, intent)
		s.mu.Unlock()
		if terminal {
			return false
		}
		if (err == nil || aborted) && !retry {
			return true
		}
		timer := time.NewTimer(subjectLifecycleRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return true
		case <-epoch.ctx.Done():
			timer.Stop()
			return true
		case <-timer.C:
		}
		reconcileCtx, cancel := context.WithTimeout(epoch.ctx, d.deadline)
		stop := context.AfterFunc(ctx, cancel)
		if retry {
			// Actions may have superseded this intent during the retry delay.
			s.mu.Lock()
			current := d.retryInstallLocked(s, epoch, intent)
			s.mu.Unlock()
			if !current {
				stop()
				cancel()
				return true
			}
			candidate, err = installer.coordinator.Apply(reconcileCtx, intent.snapshot.vaultRevision, intent.snapshot.policyEpoch, intent.snapshot.payload)
			installed = err == nil
		} else {
			var active *revision.Identity
			active, err = installer.coordinator.Reconcile(reconcileCtx)
			installed = err == nil && active != nil && *active == candidate
		}
		stop()
		cancel()
	}
}

// Called under s.mu only after the previous attempt has confirmed Abort.
// Preserve a queued successor's FIFO position and let stale intents retire
// without retiring the installer needed by that successor.
func (d *fastSandboxSubjectDrain) retryInstallLocked(s *fastSandboxPolicyServer, epoch *fastSandboxSubjectEpoch, intent fastSandboxSubjectIntent) bool {
	if d.current != epoch || !s.subjectIntentCurrentLocked(intent) {
		return false
	}
	for _, queued := range d.queue {
		if queued.snapshot != nil && queued.target == intent.target && queued.epoch == intent.epoch {
			return false
		}
	}
	return true
}
