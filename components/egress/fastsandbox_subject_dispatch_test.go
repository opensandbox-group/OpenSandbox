// Copyright 2026 The OpenSandbox Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/stretchr/testify/require"
)

// Reflection keeps this regression executable on the unmodified production
// baseline; a missing suspend intent is a behavioral assertion, not a build error.
func dispatchSuspendCount(s *fastSandboxPolicyServer) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, intent := range s.subjectDrain.queue {
		field := reflect.ValueOf(intent).FieldByName("suspend")
		if field.IsValid() && field.Bool() {
			n++
		}
	}
	return n
}

func dispatchPause(t *testing.T, s *fastSandboxPolicyServer, uid, r, a string) {
	t.Helper()
	readinessDrain(t, s)
	s.recordBindingSnapshot(subject.FromSandboxUID(uid), subject.Fencing{RuntimeInstanceID: r, AttachmentID: a})
	h := readinessReceiver(t, s)
	waitSubjectInstalled(t, h, lifecycleTarget(t, uid, r, a), latestSubjectSnapshot(t, s, uid))
	s.mu.Lock()
	c := s.subjectDrain.current.client
	s.mu.Unlock()
	s.detachSubjectClient(c)
}

func TestFastSandboxSubjectDispatchRehydrateMatrix(t *testing.T) {
	for _, cell := range []string{"store-unavailable", "unreadable", "corrupt", "unsupported", "policy-held-v1", "reload-rejected-v1", "empty-v2", "policy-held-v2", "reload-rejected-v2", "stale-v2", "protected-missing", "protected-downgrade", "strict-owner", "strict-verify"} {
		t.Run(cell, func(t *testing.T) {
			s := readinessLegacySeed(t)
			subj := subject.FromSandboxUID(testUID)
			fence := subject.Fencing{RuntimeInstanceID: testFenceR, AttachmentID: testFenceA}
			dispatchPause(t, s, testUID, testFenceR, testFenceA)
			path := filepath.Join(s.vaultFiles.dir, testUID+".json")
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			vf, body, err := credentialvault.DecodeVaultFile(raw)
			require.NoError(t, err)
			pol := mustReadinessPolicy(t, *vaultAllowPolicy())
			if cell == "reload-rejected-v1" || cell == "reload-rejected-v2" {
				pol = mustReadinessPolicy(t, policyAllowAlt())
			}
			s.mu.Lock()
			// Approved test precondition: the registered mirror survives loss of
			// its local slice. Healthy same-fence SET still takes the early return.
			delete(s.vaults, subj)
			s.mu.Unlock()
			switch cell {
			case "store-unavailable":
				require.NoError(t, os.Rename(s.vaultFiles.dir, s.vaultFiles.dir+".saved"))
				require.NoError(t, os.WriteFile(s.vaultFiles.dir, []byte("file"), 0600))
				s.vaultFiles.files = make(map[subject.Subject]vaultFileStore)
			case "unreadable":
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0700))
			case "corrupt":
				require.NoError(t, os.WriteFile(path, []byte("{bad"), 0600))
			case "unsupported":
				require.NoError(t, os.WriteFile(path, []byte(`{"version":99}`), 0600))
			case "strict-owner":
				s.SetActionsAPIVersion(actionhandler.APIVersionV2)
				require.NoError(t, os.WriteFile(filepath.Join(s.policyFiles.dir, testUID+".json.owner"), []byte("{bad"), 0600))
			case "strict-verify":
				strict, owner := writerTestServer(t)
				require.Equal(t, 200, writerInit(t, strict, owner, true))
				dispatchPause(t, strict, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID)
				strict.mu.Lock()
				delete(strict.vaults, subject.FromSandboxUID(owner.SandboxUID))
				strict.mu.Unlock()
				strict.rehydrateVaultReload(subject.FromSandboxUID(owner.SandboxUID), subject.Fencing{RuntimeInstanceID: owner.RuntimeInstanceID, AttachmentID: owner.AttachmentID}, pol, false)
				require.Equal(t, 1, dispatchSuspendCount(strict))
				return
			case "protected-missing", "protected-downgrade":
				// First discover real protected bytes through the lazy relay seam.
				require.NoError(t, os.WriteFile(path, []byte(`{"version":99}`), 0600))
				s.mu.Lock()
				s.vaultForLocked(subj, fence)
				s.mu.Unlock()
				require.Equal(t, 1, dispatchSuspendCount(s))
				if cell == "protected-missing" {
					require.NoError(t, os.Remove(path))
				} else {
					require.NoError(t, os.WriteFile(path, raw, 0600))
				}
			case "empty-v2", "policy-held-v2", "reload-rejected-v2", "stale-v2":
				vf.StateLineage, vf.StateRevision, vf.InitializationID = "lineage", 1, "init"
				vf.State = credentialvault.FileStateActive
				if cell == "empty-v2" {
					vf.State = credentialvault.FileStateEmpty
					vf.Revision = 0
					body = credentialvault.VaultFileBody{}
				}
				if cell == "stale-v2" {
					vf.Incarnation = "other"
				}
				data, e := credentialvault.EncodeVaultFileV2(vf, body)
				require.NoError(t, e)
				require.NoError(t, os.WriteFile(path, data, 0600))
			}
			s.policyCommitMu.Lock()
			s.rehydrateVaultReload(subj, fence, pol, cell == "policy-held-v1" || cell == "policy-held-v2")
			s.policyCommitMu.Unlock()
			require.Equal(t, 1, dispatchSuspendCount(s), "exactly one authority loss, including false-to-false recovery")
			s.mu.Lock()
			authoritative := s.subjectVaultAuthoritativeLocked(subj)
			s.mu.Unlock()
			require.False(t, authoritative)
		})
	}
}

func TestFastSandboxSubjectDispatchLazyMatrix(t *testing.T) {
	for _, cell := range []string{"store-unavailable", "unreadable", "protected", "v2"} {
		t.Run(cell, func(t *testing.T) {
			s := readinessLegacySeed(t)
			dispatchPause(t, s, testUID, testFenceR, testFenceA)
			subj := subject.FromSandboxUID(testUID)
			path := filepath.Join(s.vaultFiles.dir, testUID+".json")
			s.mu.Lock()
			delete(s.vaults, subj)
			s.mu.Unlock()
			switch cell {
			case "store-unavailable":
				require.NoError(t, os.Rename(s.vaultFiles.dir, s.vaultFiles.dir+".saved"))
				require.NoError(t, os.WriteFile(s.vaultFiles.dir, []byte("file"), 0600))
				s.vaultFiles.files = make(map[subject.Subject]vaultFileStore)
			case "unreadable":
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0700))
			case "protected":
				require.NoError(t, os.WriteFile(path, []byte(`{"version":99}`), 0600))
			case "v2":
				data, e := credentialvault.EncodeVaultFileV2(credentialvault.VaultFile{Incarnation: testFenceR, AttachmentID: testFenceA, State: credentialvault.FileStateEmpty, StateLineage: "lineage", StateRevision: 1, InitializationID: "init"}, credentialvault.VaultFileBody{})
				require.NoError(t, e)
				require.NoError(t, os.WriteFile(path, data, 0600))
			}
			rec := doVault(t, s, "GET", testUID, "", "")
			require.Equal(t, 503, rec.Code)
			require.Equal(t, 1, dispatchSuspendCount(s))
		})
	}
}

func TestFastSandboxSubjectDispatchWriterMatrix(t *testing.T) {
	for _, cell := range []string{"owner-lost", "state-mismatch", "verify-missing", "rollback-restored-entry", "rollback-restored-absence", "rollback-unpublished", "rollback-uncertain", "vault-uncertain"} {
		t.Run(cell, func(t *testing.T) {
			s, owner := writerTestServer(t)
			subj := subject.FromSandboxUID(owner.SandboxUID)
			if cell == "owner-lost" || cell == "state-mismatch" || cell == "verify-missing" {
				require.Equal(t, 200, writerInit(t, s, owner, true))
			}
			dispatchPause(t, s, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID)
			switch cell {
			case "owner-lost":
				require.NoError(t, os.Remove(filepath.Join(s.policyFiles.dir, "a.json.owner")))
				require.Equal(t, 412, doActionsV2Relay(t, s, "GET", vaultStatePrefix+"a", owner, "").Code)
			case "state-mismatch":
				mismatchMutationVault(t, s, owner)
				require.Equal(t, 503, doActionsV2Relay(t, s, "GET", vaultStatePrefix+"a", owner, "").Code)
			case "verify-missing":
				require.NoError(t, os.Remove(filepath.Join(s.vaultFiles.dir, "a.json")))
				require.Equal(t, 503, writerInit(t, s, owner, false))
			default:
				if cell == "rollback-restored-absence" {
					s.mu.Lock()
					delete(s.vaults, subj)
					s.mu.Unlock()
				}
				before, e := os.ReadFile(filepath.Join(s.policyFiles.dir, "a.json.owner"))
				require.NoError(t, e)
				ownerFile := newFakeVaultFile("owner")
				ownerFile.exists, ownerFile.data = true, before
				s.policyFiles.newFile = func(string) (vaultFileStore, error) { return ownerFile, nil }
				file := newFakeVaultFile("vault")
				file.saveErr = errors.New("injected persistence failure")
				file.saveState = policy.FileUnchanged
				if cell == "vault-uncertain" {
					file.saveState = policy.FileUnknown
				}
				file.onSave = func() {
					if cell == "rollback-unpublished" || cell == "rollback-uncertain" {
						ownerFile.saveErr = errors.New("injected rollback failure")
						ownerFile.saveState = policy.FileUnchanged
						if cell == "rollback-uncertain" {
							ownerFile.saveState = policy.FileUnknown
						}
					}
				}
				s.vaultFiles.files[subj] = file
				require.Equal(t, 503, writerInit(t, s, owner, true))
				if cell == "rollback-restored-entry" || cell == "rollback-restored-absence" {
					s.mu.Lock()
					authoritative := s.subjectVaultAuthoritativeLocked(subj)
					q := append([]fastSandboxSubjectIntent(nil), s.subjectDrain.queue...)
					s.mu.Unlock()
					require.True(t, authoritative)
					require.GreaterOrEqual(t, len(q), 2)
					require.NotNil(t, q[len(q)-1].snapshot, "confirmed rollback must restore installation after suspend")
				}
			}
			require.Equal(t, 1, dispatchSuspendCount(s))
		})
	}
}

func TestFastSandboxSubjectDispatchNonAuthorityReplay(t *testing.T) {
	for _, degraded := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "degraded"}[degraded], func(t *testing.T) {
			s := readinessLegacySeed(t)
			dispatchPause(t, s, testUID, testFenceR, testFenceA)
			subj := subject.FromSandboxUID(testUID)
			s.mu.Lock()
			e := s.vaults[subj]
			if degraded {
				e.degraded = "held"
			} else {
				e.vaultStateUnknown = true
			}
			s.vaults[subj] = e
			s.mu.Unlock()
			h := readinessReceiver(t, s)
			target := lifecycleTarget(t, testUID, testFenceR, testFenceA)
			transport, err := revision.NewSubjectUnixTransport(h.SocketPath, h.SessionToken, h.ControlGeneration, target, h.MaxSnapshotBytes)
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_, e := transport.Readback(ctx)
				return e == nil
			}, 5*time.Second, time.Millisecond)
			// A second subject's acknowledged install is a normal-worker FIFO barrier.
			require.Equal(t, 200, doAction(t, s, bindBody(t, "barrier", "10.0.0.98", "r2", "a2", 1, vaultAllowPolicy())).Code)
			waitSubjectInstalled(t, h, lifecycleTarget(t, "barrier", "r2", "a2"), latestSubjectSnapshot(t, s, "barrier"))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			active, err := transport.Readback(ctx)
			require.NoError(t, err)
			require.Nil(t, active, "non-authoritative replay must not reinstall retained payload")
		})
	}
}

func TestFastSandboxSubjectDispatchExclusions(t *testing.T) {
	s := readinessLegacySeed(t)
	dispatchPause(t, s, testUID, testFenceR, testFenceA)
	subj := subject.FromSandboxUID(testUID)
	s.mu.Lock()
	s.policies[subj].degraded = "policy only"
	s.requireVaultRecoveryLocked("test latch")
	s.mu.Unlock()
	// Sealing is observed by the real lazy lookup, which writes the entry.
	require.NoError(t, os.WriteFile(filepath.Join(s.vaultFiles.dir, testUID+".json.sealed"), nil, 0600))
	s.vaultFor(subj)
	require.Zero(t, dispatchSuspendCount(s))
}

func TestFastSandboxSubjectDispatchRegisterSource(t *testing.T) {
	s, _, _ := fastSandboxTestServer(t)
	readinessDrain(t, s)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	s.mu.Lock()
	defer s.mu.Unlock()
	var address reflect.Value
	for _, intent := range s.subjectDrain.queue {
		if intent.snapshot == nil && !intent.unload {
			field := reflect.ValueOf(intent).FieldByName("sourceAddresses")
			if field.IsValid() {
				address = field
			}
		}
	}
	require.True(t, address.IsValid(), "missing source address dispatch")
	require.Equal(t, 1, address.Len())
	require.True(t, address.Index(0).Equal(reflect.ValueOf(netip.MustParseAddr(testIP))), "freeze the attachment registry key in the Register intent")
}

func TestSubjectDispatchIPC(t *testing.T) {
	c := launchSubjectIPCChild(t)
	target := revision.SubjectTarget{SubjectKey: "s-a", SubjectGeneration: "a"}
	post := func(seq int, op string, target revision.SubjectTarget, extra map[string]any) revision.LifecycleReceipt {
		body := map[string]any{"controlGeneration": "subject-test-control", "lifecycleSequence": seq, "target": target}
		for k, v := range extra {
			body[k] = v
		}
		raw, e := json.Marshal(body)
		require.NoError(t, e)
		_, data := c.response(t, "/v2/subjects/"+op, raw, c.token, false)
		var receipt revision.LifecycleReceipt
		require.NoError(t, json.Unmarshal(data, &receipt))
		return receipt
	}
	require.Equal(t, "registered", post(1, "register", target, map[string]any{"sourceAddresses": []string{"2001:0db8::1"}}).Result.Outcome)
	other := revision.SubjectTarget{SubjectKey: "s-b", SubjectGeneration: "b"}
	require.Equal(t, "subject_conflict", post(2, "register", other, map[string]any{"sourceAddresses": []string{"2001:db8::1"}}).Result.Outcome)
	transport := subjectTransport(t, c, target)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	payload := subjectPayload(t, true, 1)
	id := subjectIdentity(target, 1, 1, payload)
	_, e := transport.Prepare(ctx, id, payload)
	require.NoError(t, e)
	_, e = transport.Commit(ctx, id)
	require.NoError(t, e)
	require.Equal(t, "suspended", post(3, "suspend", target, nil).Result.Outcome)
	require.Nil(t, c.inspect(t, target).Revision)
	post(4, "unload", target, nil)
	require.Equal(t, "registered", post(5, "register", other, map[string]any{"sourceAddresses": []string{"2001:db8::1"}}).Result.Outcome)
}

func TestSubjectDispatchLifecycleClient(t *testing.T) {
	c := launchSubjectIPCChild(t)
	client, e := revision.NewSubjectLifecycleClient(c.socket, c.token, "subject-test-control")
	require.NoError(t, e)
	register, ok := any(client).(interface {
		Register(context.Context, revision.SubjectTarget, ...netip.Addr) (revision.LifecycleReceipt, error)
	})
	require.True(t, ok, "missing sourceAddresses client")
	suspend, ok := any(client).(interface {
		Suspend(context.Context, revision.SubjectTarget) (revision.LifecycleReceipt, error)
	})
	require.True(t, ok, "missing suspend client")
	target := revision.SubjectTarget{SubjectKey: "s-a", SubjectGeneration: "a"}
	other := revision.SubjectTarget{SubjectKey: "s-b", SubjectGeneration: "b"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	address := netip.MustParseAddr("::ffff:192.0.2.1")
	_, e = register.Register(ctx, target, address, netip.MustParseAddr("192.0.2.3"), netip.MustParseAddr("2001:0db8::1"))
	require.NoError(t, e)
	for _, address := range []string{"192.0.2.3", "2001:DB8:0::1"} {
		require.Equal(t, &target, c.exchange(t, map[string]any{"command": "lookup", "address": address}).Target)
	}
	view := c.exchange(t, map[string]any{"command": "lookup", "address": "::ffff:c000:201"})
	require.Equal(t, &target, view.Target)
	_, e = register.Register(ctx, target, netip.MustParseAddr("fe80::1%eth0"))
	require.Error(t, e)
	require.False(t, client.Pending())
	_, e = register.Register(ctx, other, address)
	require.ErrorIs(t, e, revision.ErrTransportRejected)
	transport := subjectTransport(t, c, target)
	payload := subjectPayload(t, true, 1)
	id := subjectIdentity(target, 1, 1, payload)
	_, e = transport.Prepare(ctx, id, payload)
	require.NoError(t, e)
	_, e = transport.Commit(ctx, id)
	require.NoError(t, e)
	pending := subjectIdentity(target, 2, 1, payload)
	_, e = transport.Prepare(ctx, pending, payload)
	require.NoError(t, e)
	// Lose both the successful operation reply and its immediate status reply.
	c.exchange(t, map[string]any{"command": "fault", "operations": []string{"suspend", "lifecycle-status"}})
	_, e = suspend.Suspend(ctx, target)
	require.ErrorIs(t, e, revision.ErrIndeterminate)
	require.True(t, client.Pending())
	receipt, e := client.Reconcile(ctx)
	require.NoError(t, e)
	require.Equal(t, "suspend", receipt.Operation)
	require.False(t, client.Pending())
	view = c.exchange(t, map[string]any{"command": "lookup", "address": address.String()})
	require.Nil(t, view.Revision)
	_, e = transport.Commit(ctx, pending)
	require.NoError(t, e)
	require.Nil(t, c.exchange(t, map[string]any{"command": "lookup", "address": address.String()}).Revision)
	_, e = suspend.Suspend(ctx, target)
	require.NoError(t, e)
	wrong := target
	wrong.SubjectGeneration = "wrong"
	r, e := suspend.Suspend(ctx, wrong)
	require.ErrorIs(t, e, revision.ErrTransportRejected)
	require.Equal(t, "subject_generation_mismatch", r.Result.Outcome)
	require.False(t, client.Pending())
	r, e = suspend.Suspend(ctx, other)
	require.ErrorIs(t, e, revision.ErrTransportRejected)
	require.Equal(t, "subject_unknown", r.Result.Outcome)
	require.False(t, client.Pending())
	id = subjectIdentity(target, 3, 1, payload)
	_, e = transport.Prepare(ctx, id, payload)
	require.NoError(t, e)
	_, e = transport.Commit(ctx, id)
	require.NoError(t, e)
	view = c.exchange(t, map[string]any{"command": "lookup", "address": address.String()})
	require.NotNil(t, view.Revision)
	c.exchange(t, map[string]any{"command": "fail-close", "subjectKey": target.SubjectKey})
	c.exchange(t, map[string]any{"command": "fault", "operations": []string{"lifecycle-status"}})
	r, e = client.Unload(ctx, target)
	require.ErrorIs(t, e, revision.ErrIndeterminate)
	receipt, e = client.Reconcile(ctx)
	require.ErrorIs(t, e, revision.ErrTransportRejected)
	require.Equal(t, "fenced", receipt.Result.Unload.Completion)
	view = c.exchange(t, map[string]any{"command": "lookup", "address": address.String()})
	require.Nil(t, view.Target)
	_, e = register.Register(ctx, target, address)
	require.NoError(t, e)
	_, e = register.Register(ctx, other, netip.MustParseAddr("192.0.2.2"))
	require.ErrorIs(t, e, revision.ErrTransportRejected, "quarantine still consumes the second slot")
}

func TestFastSandboxSubjectDispatchWorkerSuspend(t *testing.T) {
	s, owner := writerTestServer(t)
	require.Equal(t, 200, writerInit(t, s, owner, true))
	readinessDrain(t, s)
	s.recordBindingSnapshot(subject.FromSandboxUID(owner.SandboxUID), subject.Fencing{RuntimeInstanceID: owner.RuntimeInstanceID, AttachmentID: owner.AttachmentID})
	h := readinessReceiver(t, s)
	waitReadiness(t, s, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID)
	mismatchMutationVault(t, s, owner)
	require.Equal(t, 503, doActionsV2Relay(t, s, "GET", vaultStatePrefix+owner.SandboxUID, owner, "").Code)
	waitLifecycleIdle(t, s, h, 2)
	probe := &subjectIPCChild{socket: h.SocketPath, token: h.SessionToken}
	raw, _ := json.Marshal(map[string]string{"controlGeneration": h.ControlGeneration})
	_, body := probe.response(t, "/v2/subjects/lifecycle-status", raw, h.SessionToken, false)
	var receipt struct {
		Operation string
		Result    revision.LifecycleResult
	}
	require.NoError(t, json.Unmarshal(body, &receipt))
	require.Equal(t, "suspend", receipt.Operation)
	require.Equal(t, "suspended", receipt.Result.Outcome)
	requireReadinessBlocked(t, s, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID)
	require.Equal(t, 200, writerInit(t, s, owner, false))
	waitReadiness(t, s, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID)
}

func TestFastSandboxSubjectDispatchWorkerFailedUnload(t *testing.T) {
	c := launchSubjectIPCChild(t)
	s, _, _ := fastSandboxTestServer(t)
	readinessDrain(t, s)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	client, e := revision.NewSubjectLifecycleClient(c.socket, c.token, "subject-test-control")
	require.NoError(t, e)
	h := mitmproxy.SubjectRevisionIPCConfig{SocketPath: c.socket, SessionToken: c.token, ControlGeneration: "subject-test-control", MaxSnapshotBytes: 65536}
	s.publishSubjectClient(client, h)
	waitReadiness(t, s, testUID, testFenceR, testFenceA)
	target := lifecycleTarget(t, testUID, testFenceR, testFenceA)
	c.exchange(t, map[string]any{"command": "fail-close", "subjectKey": target.SubjectKey})
	require.Equal(t, 200, doAction(t, s, removeBody(t, testUID, testFenceR, testFenceA)).Code)
	require.Equal(t, 200, doAction(t, s, bindBody(t, "successor", testIP, "r2", "a2", 1, vaultAllowPolicy())).Code)
	waitReadiness(t, s, "successor", "r2", "a2")
	require.Contains(t, s.subjectLifecycleDegraded(), "subject_failed")
	view := c.exchange(t, map[string]any{"command": "lookup", "address": testIP})
	next := lifecycleTarget(t, "successor", "r2", "a2")
	require.Equal(t, &next, view.Target)
	require.NotNil(t, view.Revision)
}
