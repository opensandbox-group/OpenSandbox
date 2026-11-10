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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/stretchr/testify/require"
)

func attachMutationReceiver(t *testing.T, s *fastSandboxPolicyServer) *mitmproxy.SubjectRevisionIPCConfig {
	t.Helper()
	o := subjectTestOwner(t)
	launch, configs := subjectAddonLauncher(t, o)
	child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	o.attach(s, child)
	t.Cleanup(o.stopDrain)
	return (*configs)[0].SubjectRevisionIPC
}

func mutationPersistence(t *testing.T, s *fastSandboxPolicyServer) {
	t.Helper()
	s.policyFiles.newFile = func(path string) (vaultFileStore, error) { return policy.NewAtomicPolicyFile(path) }
	vp, err := newVaultPersistence(filepath.Join(t.TempDir(), "vault"))
	require.NoError(t, err)
	s.SetVaultFiles(vp)
}

// Build the expectation from committed state, independently of the producer
// mirror. Reading only that mirror would let a missed producer pass silently.
func committedMutationSnapshot(t *testing.T, s *fastSandboxPolicyServer, uid string) *fastSandboxSubjectSnapshot {
	t.Helper()
	s.policyCommitMu.Lock()
	defer s.policyCommitMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	subj := subject.FromSandboxUID(uid)
	var active credentialvault.ActiveSnapshot
	if entry := s.vaults[subj]; entry.store != nil {
		var err error
		active, err = entry.store.ActiveSnapshot()
		require.True(t, err == nil || errors.Is(err, credentialvault.ErrNotFound))
	}
	var epoch int64
	if entry := s.policies[subj]; entry != nil {
		epoch = entry.revision
	}
	payload, err := credentialvault.MarshalDecisionSnapshot(active, epoch)
	require.NoError(t, err)
	return &fastSandboxSubjectSnapshot{payload: payload, vaultRevision: active.Revision, policyEpoch: epoch}
}

func checkMutationInstalled(t *testing.T, s *fastSandboxPolicyServer, h *mitmproxy.SubjectRevisionIPCConfig, uid, runtimeID, attachmentID string) revision.Identity {
	t.Helper()
	want := committedMutationSnapshot(t, s, uid)
	requireSnapshotContent(t, want, latestSubjectSnapshot(t, s, uid))
	return waitSubjectInstalled(t, h, lifecycleTarget(t, uid, runtimeID, attachmentID), want)
}

func TestFastSandboxSubjectSnapshotMutationLegacyVault(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprint(persistent), func(t *testing.T) {
			s, _, _ := fastSandboxTestServer(t)
			if persistent {
				mutationPersistence(t, s)
			}
			h := attachMutationReceiver(t, s)
			require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
			checkMutationInstalled(t, s, h, testUID, testFenceR, testFenceA)
			for _, tc := range []struct {
				method, body string
				code         int
				revision     int64
			}{
				{http.MethodPost, vaultCredential(), 201, 1},
				{http.MethodPut, vaultPut("1"), 200, 2},
				{http.MethodDelete, "", 204, 0},
			} {
				rec := doVault(t, s, tc.method, testUID, tc.body, "")
				require.Equal(t, tc.code, rec.Code, rec.Body.String())
				active := checkMutationInstalled(t, s, h, testUID, testFenceR, testFenceA)
				require.Equal(t, tc.revision, active.VaultRevision)
			}
		})
	}
}

func TestFastSandboxSubjectSnapshotMutationActivation(t *testing.T) {
	s, _, _, _, configs := snapshotDrainHarness(t)
	h := (*configs)[0].SubjectRevisionIPC
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	first := checkMutationInstalled(t, s, h, testUID, testFenceR, testFenceA)
	waitReadiness(t, s, testUID, testFenceR, testFenceA)
	// Activation has the same digest, but still passes through the producer.
	frozen := latestSubjectSnapshot(t, s, testUID)
	require.Eventually(t, func() bool {
		transport, err := revision.NewSubjectUnixTransport(h.SocketPath, h.SessionToken, h.ControlGeneration, lifecycleTarget(t, testUID, testFenceR, testFenceA), h.MaxSnapshotBytes)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		active, err := transport.Readback(ctx)
		return err == nil && active != nil && active.DecisionEpoch > first.DecisionEpoch
	}, 5*time.Second, 10*time.Millisecond)
	requireSnapshotContent(t, frozen, committedMutationSnapshot(t, s, testUID))
}

func TestFastSandboxSubjectSnapshotMutationPolicyAndRemoval(t *testing.T) {
	s, reg, _, _, configs := snapshotDrainHarness(t)
	h := (*configs)[0].SubjectRevisionIPC
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	waitReadiness(t, s, testUID, testFenceR, testFenceA)
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		rec := doPolicy(t, s, method, testUID, policyPutDoc(policyAllowAlt(), 0, false, false), "")
		require.Contains(t, []int{200, 204}, rec.Code)
		checkMutationInstalled(t, s, h, testUID, testFenceR, testFenceA)
	}
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 2, nil)).Code)
	removed := checkMutationInstalled(t, s, h, testUID, testFenceR, testFenceA)
	require.Zero(t, removed.PolicyEpoch)
	require.Contains(t, string(latestSubjectSnapshot(t, s, testUID).payload), `"state":"active-empty"`)
	state, known := reg.Get(subject.FromSandboxUID(testUID))
	require.True(t, known)
	require.Equal(t, subject.StateDenying, state)
	require.Nil(t, reg.EffectivePolicy(subject.FromSandboxUID(testUID)))
	require.Equal(t, 409, doAction(t, s, hookBody(t, testUID, testFenceR, testFenceA, constants.HookDataPlaneReady)).Code)
}

func TestFastSandboxSubjectSnapshotMutationStrictVaultAndUnknown(t *testing.T) {
	s, owner := writerTestServer(t)
	h := attachMutationReceiver(t, s)
	uid, r, a := owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID
	// Replay SET installs the existing recorded policy before initialization.
	require.Equal(t, 200, doActionsV2(t, s, actionsV2Body(t, actionhandler.OperationSetBinding, owner, vaultAllowPolicy())).Code)
	checkMutationInstalled(t, s, h, uid, r, a)
	before := latestSubjectSnapshot(t, s, uid)
	require.Equal(t, 200, writerInit(t, s, owner, true))
	require.NotSame(t, before, latestSubjectSnapshot(t, s, uid), "fresh empty initialization is a producer")
	checkMutationInstalled(t, s, h, uid, r, a)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		state := writerState(t, s, owner)
		body := vaultCredential()
		if method == http.MethodPut {
			body = vaultPut("1")
		}
		require.Contains(t, []int{200, 201, 204}, writerMutation(t, s, owner, method, state, body))
		active := checkMutationInstalled(t, s, h, uid, r, a)
		if method == http.MethodDelete {
			require.Zero(t, active.VaultRevision, "DEV-13: deleted v2 state keeps revision zero")
		}
	}
	before = latestSubjectSnapshot(t, s, uid)
	require.Equal(t, 200, writerInit(t, s, owner, false))
	require.NotSame(t, before, latestSubjectSnapshot(t, s, uid), "verified deleted initialization is also a producer")
	checkMutationInstalled(t, s, h, uid, r, a)
	require.Equal(t, 201, writerMutation(t, s, owner, http.MethodPost, writerState(t, s, owner), vaultCredential()))
	last := checkMutationInstalled(t, s, h, uid, r, a)
	before = latestSubjectSnapshot(t, s, uid)
	require.NoError(t, os.Remove(filepath.Join(s.policyFiles.dir, uid+".json.owner")))
	rec := doActionsV2Relay(t, s, http.MethodGet, vaultStatePrefix+uid, owner, "")
	require.Equal(t, 412, rec.Code)
	require.Equal(t, []string{"vault_state_unknown"}, s.subjectLifecycleDegraded())
	require.Same(t, before, latestSubjectSnapshot(t, s, uid), "unknown authority cannot replace the last payload with empty")
	require.Equal(t, last, waitSubjectInstalled(t, h, lifecycleTarget(t, uid, r, a), before))
	s.policyCommitMu.Lock()
	s.recordBindingSnapshot(subject.FromSandboxUID(uid), subject.Fencing{RuntimeInstanceID: r, AttachmentID: a})
	s.policyCommitMu.Unlock()
	require.Same(t, before, latestSubjectSnapshot(t, s, uid), "later producers must also preserve the unknown mirror")
	require.True(t, s.actionStatusResponse().Ready)
}

func mutationBundles(t *testing.T, rev int64) (string, string) {
	t.Helper()
	dir := t.TempDir()
	vaultPath, policyPath := filepath.Join(dir, "vault.json"), filepath.Join(dir, "policy.json")
	vault, err := credentialvault.EncodeVaultFile("old-r", "old-a", rev, bundleVaultBody(t))
	require.NoError(t, err)
	pol, err := policy.EncodePolicyFile("old-r", "old-a", rev+10, []byte(policyAllow()))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(vaultPath, vault, 0600))
	require.NoError(t, os.WriteFile(policyPath, pol, 0600))
	return vaultPath, policyPath
}

func TestFastSandboxSubjectSnapshotMutationImports(t *testing.T) {
	for _, when := range []string{"before-set", "before-ready", "after-ready"} {
		t.Run(when, func(t *testing.T) {
			s, _, _ := fastSandboxTestServer(t)
			mutationPersistence(t, s)
			h := attachMutationReceiver(t, s)
			set := func() {
				require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
			}
			ready := func() {
				waitReadiness(t, s, testUID, testFenceR, testFenceA)
			}
			if when != "before-set" {
				set()
				checkMutationInstalled(t, s, h, testUID, testFenceR, testFenceA)
			}
			if when == "after-ready" {
				ready()
			}
			vaultPath, policyPath := mutationBundles(t, 3)
			for _, tc := range []struct{ op, path string }{{"POLICY_IMPORT", policyPath}, {"VAULT_IMPORT", vaultPath}} {
				rec := doAction(t, s, snapshotBody(t, testUID, testFenceR, testFenceA, tc.op, tc.path))
				require.Equal(t, 200, rec.Code, rec.Body.String())
				if when != "before-set" {
					checkMutationInstalled(t, s, h, testUID, testFenceR, testFenceA)
				}
			}
			if when == "before-set" {
				s.mu.Lock()
				require.Empty(t, s.subjectDrain.snapshots)
				require.Empty(t, s.subjectDrain.queue)
				s.mu.Unlock()
				waitSubjectKnown(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), false)
				set()
			}
			active := checkMutationInstalled(t, s, h, testUID, testFenceR, testFenceA)
			require.Equal(t, int64(3), active.VaultRevision)
			require.Equal(t, int64(13), active.PolicyEpoch)
			ready()
			checkMutationInstalled(t, s, h, testUID, testFenceR, testFenceA)
			require.Equal(t, "allow", reg_UserEvaluate(s, subject.FromSandboxUID(testUID), "api.example.com"))
		})
	}
}

func TestFastSandboxSubjectSnapshotMutationCoalesce(t *testing.T) {
	s, h, fault := snapshotFaultHarness(t, "", "")
	mutationPersistence(t, s)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	select {
	case <-fault.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("initial Commit not reached")
	}
	first := latestSubjectSnapshot(t, s, testUID)
	// No SET producer intervenes: every update here must enqueue on its own,
	// and none may wait for the unresolved IPC transaction or acquire its lock.
	require.Equal(t, 201, doVault(t, s, http.MethodPost, testUID, vaultCredential(), "").Code)
	require.Equal(t, 200, doPolicy(t, s, http.MethodPut, testUID, policyPutDoc(policyAllow(), 0, false, false), "").Code)
	vaultPath, policyPath := mutationBundles(t, 5)
	for _, tc := range []struct{ op, path string }{{"VAULT_IMPORT", vaultPath}, {"POLICY_IMPORT", policyPath}} {
		require.Equal(t, 200, doAction(t, s, snapshotBody(t, testUID, testFenceR, testFenceA, tc.op, tc.path)).Code)
	}
	requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
	want := committedMutationSnapshot(t, s, testUID)
	require.NotEqual(t, first.payload, want.payload)
	s.mu.Lock()
	queued := append([]fastSandboxSubjectIntent(nil), s.subjectDrain.queue...)
	s.mu.Unlock()
	require.Len(t, queued, 1)
	requireSnapshotContent(t, want, queued[0].snapshot)
	close(fault.release)
	active := checkMutationInstalled(t, s, h, testUID, testFenceR, testFenceA)
	require.Equal(t, int64(2), active.DecisionEpoch)
	fault.mu.Lock()
	defer fault.mu.Unlock()
	var payloads [][]byte
	for _, event := range fault.events {
		if strings.HasSuffix(event.Path, "/prepare") {
			payloads = append(payloads, event.Payload)
		}
	}
	require.Equal(t, [][]byte{first.payload, want.payload}, payloads)
}

type mutationObservation struct {
	Code  int
	Body  string
	Ready bool
	Allow string
}

func exerciseMutationHTTP(t *testing.T, s *fastSandboxPolicyServer, strict, fileMetaMismatch, waitForACK bool) []mutationObservation {
	t.Helper()
	var out []mutationObservation
	observe := func(rec *httptest.ResponseRecorder) {
		out = append(out, mutationObservation{rec.Code, rec.Body.String(), s.actionStatusResponse().Ready, reg_UserEvaluate(s, subject.FromSandboxUID(testUID), "api.example.com")})
	}
	if strict {
		s.SetActionsAPIVersion(actionhandler.APIVersionV2)
		owner := actionsV2Owner(testUID, testFenceR, testFenceA, 1)
		s.instanceID = "fixed-differential-handler"
		observe(doActionsV2(t, s, actionsV2Body(t, actionhandler.OperationSetBinding, owner, vaultAllowPolicy())))
		if waitForACK && s.subjectDrain != nil {
			waitReadiness(t, s, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID)
		}
		observe(doActionsV2(t, s, actionsV2Body(t, actionhandler.OperationLifecycleHook, owner, nil)))
		init, err := json.Marshal(map[string]any{"contract": strictVaultContract{vaultWriterVersion, "init-a", "lineage-a"}, "mode": "legacy", "fresh": true, "handlerInstanceId": s.instanceID})
		require.NoError(t, err)
		observe(doActionsV2Relay(t, s, "POST", vaultInitializationPrefix+testUID, owner, string(init)))
		for _, method := range []string{"POST", "PUT", "DELETE"} {
			state := writerState(t, s, owner)
			body := vaultCredential()
			if method == "PUT" {
				body = vaultPut("1")
			}
			payload, err := json.Marshal(map[string]any{"expectedState": state.Token, "vault": json.RawMessage(body)})
			require.NoError(t, err)
			observe(doActionsV2Relay(t, s, method, vaultStatePrefix+testUID, owner, string(payload)))
		}
		var before *fastSandboxSubjectSnapshot
		if fileMetaMismatch {
			if s.subjectDrain != nil {
				before = latestSubjectSnapshot(t, s, testUID)
			}
			vf, body, err := s.writerFile(subject.FromSandboxUID(testUID), &strictOwnerRecord{Owner: owner, VaultContract: &strictVaultContract{vaultWriterVersion, "init-a", "lineage-a"}})
			require.NoError(t, err)
			vf.StateRevision++
			payload, err := credentialvault.EncodeVaultFileV2(vf, body)
			require.NoError(t, err)
			fs, err := s.vaultFiles.storeFor(subject.FromSandboxUID(testUID))
			require.NoError(t, err)
			state, err := fs.SaveRaw(payload)
			require.NoError(t, err)
			require.Equal(t, policy.FileCommitted, state)
		} else {
			require.NoError(t, os.Remove(filepath.Join(s.policyFiles.dir, testUID+".json.owner")))
		}
		rec := doActionsV2Relay(t, s, "GET", vaultStatePrefix+testUID, owner, "")
		observe(rec)
		if fileMetaMismatch {
			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			if s.subjectDrain != nil {
				require.Contains(t, s.subjectLifecycleDegraded(), "vault_state_unknown")
				require.Same(t, before, latestSubjectSnapshot(t, s, testUID), "file metadata mismatch must preserve the last mirrored payload")
				s.mu.Lock()
				s.recordSubjectSnapshotLocked(subject.FromSandboxUID(testUID), subject.Fencing{RuntimeInstanceID: testFenceR, AttachmentID: testFenceA})
				s.mu.Unlock()
				require.Same(t, before, latestSubjectSnapshot(t, s, testUID), "later producers must preserve the unknown mirror")
			}
		}
		return out
	}
	observe(doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())))
	for _, hook := range []string{constants.HookRuntimeReady, constants.HookDataPlaneReady} {
		if waitForACK && s.subjectDrain != nil && hook == constants.HookDataPlaneReady {
			waitReadiness(t, s, testUID, testFenceR, testFenceA)
		}
		observe(doAction(t, s, hookBody(t, testUID, testFenceR, testFenceA, hook)))
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		body := vaultCredential()
		if method == "PUT" {
			body = vaultPut("1")
		}
		observe(doVault(t, s, method, testUID, body, ""))
	}
	for _, method := range []string{"PUT", "DELETE"} {
		observe(doPolicy(t, s, method, testUID, policyPutDoc(policyAllow(), 0, false, false), ""))
	}
	observe(doPolicy(t, s, "PUT", testUID, policyPutDoc(policyAllow(), 0, false, false), ""))
	vaultPath, policyPath := mutationBundles(t, 5)
	for _, tc := range []struct{ op, path string }{{"VAULT_IMPORT", vaultPath}, {"POLICY_IMPORT", policyPath}} {
		observe(doAction(t, s, snapshotBody(t, testUID, testFenceR, testFenceA, tc.op, tc.path)))
	}
	if waitForACK && s.subjectDrain != nil {
		waitReadiness(t, s, testUID, testFenceR, testFenceA)
	}
	observe(doAction(t, s, hookBody(t, testUID, testFenceR, testFenceA, constants.HookDataPlaneReady)))
	observe(doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 2, nil)))
	observe(doAction(t, s, hookBody(t, testUID, testFenceR, testFenceA, constants.HookDataPlaneReady)))
	return out
}

func TestFastSandboxSubjectSnapshotMutationHTTPDifferential(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		strict, fileMetaMismatch bool
	}{{"false", false, false}, {"true", true, false}, {"strict-file-meta-mismatch", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			strict := tc.strict
			var baseline []mutationObservation
			for _, mode := range []string{"disabled", "enabled", "rejected"} {
				s, _, _ := fastSandboxTestServer(t)
				mutationPersistence(t, s)
				if mode == "enabled" {
					attachMutationReceiver(t, s)
				} else if mode == "rejected" {
					h := attachMutationReceiver(t, s)
					_, proxy := startSnapshotFault(t, h, "revision_rejected", "prepare")
					client, err := revision.NewSubjectLifecycleClient(proxy.SocketPath, proxy.SessionToken, proxy.ControlGeneration)
					require.NoError(t, err)
					s.publishSubjectClient(client, proxy)
				}
				observed := exerciseMutationHTTP(t, s, strict, tc.fileMetaMismatch, mode != "rejected")
				if mode == "disabled" {
					baseline = observed
					require.Nil(t, s.subjectDrain)
					raw, err := json.Marshal(observed)
					require.NoError(t, err)
					t.Logf("HTTP-BASELINE %s", raw)
				} else {
					if mode == "rejected" {
						for i := range observed {
							require.Equal(t, "no-policy", observed[i].Allow, "failed installation must never activate policy")
							observed[i].Allow = baseline[i].Allow // only the expected deny-first consequence differs
							gated := (strict && i == 1) || (!strict && (i == 2 || i == 11))
							if gated {
								require.Equal(t, 503, observed[i].Code)
								require.Contains(t, observed[i].Body, "subject snapshot not acknowledged")
								observed[i].Code, observed[i].Body = baseline[i].Code, baseline[i].Body
							}
						}
						require.Eventually(t, func() bool {
							want := []string{"subject_prepare_rejected"}
							if strict {
								want = append(want, "vault_state_unknown")
							}
							return fmt.Sprint(s.subjectLifecycleDegraded()) == fmt.Sprint(want)
						}, 5*time.Second, 5*time.Millisecond)
					}
					require.Equal(t, baseline, observed, "status/body/Ready/hooks and policy evaluation: %s", mode)
					if strict && mode != "rejected" {
						require.Equal(t, []string{"vault_state_unknown"}, s.subjectLifecycleDegraded())
					}
				}
			}
		})
	}
}

// All recovery assertions use the production addon over Unix IPC. The disk
// edit remains a valid v2 file, so the 503 proves the metadata mismatch path.
func mismatchMutationVault(t *testing.T, s *fastSandboxPolicyServer, owner actionhandler.Owner) {
	t.Helper()
	subj := subject.FromSandboxUID(owner.SandboxUID)
	record, err := s.readOwnerRecord(subj)
	require.NoError(t, err)
	vf, body, err := s.writerFile(subj, record)
	require.NoError(t, err)
	vf.StateRevision++
	payload, err := credentialvault.EncodeVaultFileV2(vf, body)
	require.NoError(t, err)
	fs, err := s.vaultFiles.storeFor(subj)
	require.NoError(t, err)
	state, err := fs.SaveRaw(payload)
	require.NoError(t, err)
	require.Equal(t, policy.FileCommitted, state)
	before := latestSubjectSnapshot(t, s, owner.SandboxUID)
	require.Equal(t, 503, doActionsV2Relay(t, s, "GET", vaultStatePrefix+owner.SandboxUID, owner, "").Code)
	require.Contains(t, s.subjectLifecycleDegraded(), "vault_state_unknown")
	require.Same(t, before, latestSubjectSnapshot(t, s, owner.SandboxUID))
}

func TestFastSandboxSubjectSnapshotUnknownBootstrapRecovery(t *testing.T) {
	s, owner := writerTestServer(t)
	attachMutationReceiver(t, s)
	require.Equal(t, 200, writerInit(t, s, owner, true))
	require.Equal(t, 201, writerMutation(t, s, owner, "POST", writerState(t, s, owner), vaultCredential()))
	want := committedMutationSnapshot(t, s, owner.SandboxUID)
	// Recreate the Go server with only durable files, as after egress restart.
	restarted, _, _ := fastSandboxTestServer(t)
	restarted.SetActionsAPIVersion(actionhandler.APIVersionV2)
	restarted.SetPolicyFiles(s.policyFiles)
	restarted.SetVaultFiles(s.vaultFiles)
	h := attachMutationReceiver(t, restarted)
	require.Equal(t, 200, doActionsV2(t, restarted, actionsV2Body(t, actionhandler.OperationSetBinding, owner, vaultAllowPolicy())).Code)
	restarted.mu.Lock()
	bootstrap := restarted.subjectDrain.snapshots[subject.FromSandboxUID(owner.SandboxUID)]
	restarted.mu.Unlock()
	require.Nil(t, bootstrap, "bootstrap unknown must not publish empty")
	// A failed bootstrap verify must not turn its placeholder into a live loss.
	vp := restarted.vaultFiles
	restarted.vaultFiles = &vaultPersistence{dir: vp.dir, newFile: func(string) (vaultFileStore, error) { return nil, errors.New("injected verification failure") }}
	require.Equal(t, 503, writerInit(t, restarted, owner, false))
	restarted.vaultFiles = vp
	require.Equal(t, 200, writerInit(t, restarted, owner, false))
	checkMutationInstalled(t, restarted, h, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID)
	requireSnapshotContent(t, want, latestSubjectSnapshot(t, restarted, owner.SandboxUID))
	require.NotContains(t, restarted.subjectLifecycleDegraded(), "vault_state_unknown")
}

func TestFastSandboxSubjectSnapshotUnknownVerifiedRecovery(t *testing.T) {
	for _, cause := range []string{"file-meta", "writer-file", "owner", "verify-file", "verify-reload"} {
		t.Run(cause, func(t *testing.T) {
			s, owner := writerTestServer(t)
			h := attachMutationReceiver(t, s)
			require.Equal(t, 200, writerInit(t, s, owner, true))
			require.Equal(t, 201, writerMutation(t, s, owner, "POST", writerState(t, s, owner), vaultCredential()))
			checkMutationInstalled(t, s, h, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID)
			before := latestSubjectSnapshot(t, s, owner.SandboxUID)
			switch cause {
			case "file-meta":
				mismatchMutationVault(t, s, owner)
			case "owner":
				path := filepath.Join(s.policyFiles.dir, owner.SandboxUID+".json.owner")
				raw, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NoError(t, os.Remove(path))
				require.Equal(t, 412, doActionsV2Relay(t, s, "GET", vaultStatePrefix+owner.SandboxUID, owner, "").Code)
				require.NoError(t, os.WriteFile(path, raw, 0600))
			case "writer-file", "verify-file":
				vp := s.vaultFiles
				s.vaultFiles = &vaultPersistence{dir: vp.dir, newFile: func(string) (vaultFileStore, error) { return nil, errors.New("injected writer file failure") }}
				if cause == "writer-file" {
					require.Equal(t, 503, doActionsV2Relay(t, s, "GET", vaultStatePrefix+owner.SandboxUID, owner, "").Code)
				} else {
					require.Equal(t, 503, writerInit(t, s, owner, false))
				}
				s.vaultFiles = vp
			case "verify-reload":
				// A policy that no longer permits the committed credential rejects
				// Reload, without invalidating the durable Vault file itself.
				require.Equal(t, 200, doActionsV2Relay(t, s, "PUT", constants.ActionsV2PolicyRelayPathPrefix+owner.SandboxUID, owner, policyPutDoc(policyAllowAlt(), 0, false, false)).Code)
				before = latestSubjectSnapshot(t, s, owner.SandboxUID)
				require.Equal(t, 503, writerInit(t, s, owner, false))
				require.Equal(t, 200, doActionsV2Relay(t, s, "PUT", constants.ActionsV2PolicyRelayPathPrefix+owner.SandboxUID, owner, policyPutDoc(*vaultAllowPolicy(), 0, false, false)).Code)
			}
			require.Contains(t, s.subjectLifecycleDegraded(), "vault_state_unknown")
			require.Same(t, before, latestSubjectSnapshot(t, s, owner.SandboxUID))
			require.Equal(t, 200, writerInit(t, s, owner, false))
			checkMutationInstalled(t, s, h, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID)
			require.NotContains(t, s.subjectLifecycleDegraded(), "vault_state_unknown")
		})
	}
}

func TestFastSandboxSubjectSnapshotUnknownTwoSubjects(t *testing.T) {
	s, a := writerTestServer(t)
	h := attachMutationReceiver(t, s)
	b := actionsV2Owner("b", "runtime-b", "attachment-b", 1)
	require.Equal(t, 200, doActionsV2(t, s, actionsV2Body(t, actionhandler.OperationSetBinding, b, vaultAllowPolicy())).Code)
	for _, owner := range []actionhandler.Owner{a, b} {
		require.Equal(t, 200, writerInit(t, s, owner, true))
		checkMutationInstalled(t, s, h, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID)
		mismatchMutationVault(t, s, owner)
	}
	require.Equal(t, 200, writerInit(t, s, a, false))
	checkMutationInstalled(t, s, h, a.SandboxUID, a.RuntimeInstanceID, a.AttachmentID)
	require.Contains(t, s.subjectLifecycleDegraded(), "vault_state_unknown", "B is still unknown")
	require.Equal(t, 200, writerInit(t, s, b, false))
	checkMutationInstalled(t, s, h, b.SandboxUID, b.RuntimeInstanceID, b.AttachmentID)
	require.NotContains(t, s.subjectLifecycleDegraded(), "vault_state_unknown")
}

func TestFastSandboxSubjectSnapshotUnknownRemoval(t *testing.T) {
	s, owner := writerTestServer(t)
	h := attachMutationReceiver(t, s)
	require.Equal(t, 200, writerInit(t, s, owner, true))
	checkMutationInstalled(t, s, h, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID)
	mismatchMutationVault(t, s, owner)
	require.Equal(t, 200, doActionsV2(t, s, actionsV2Body(t, actionhandler.OperationRemoveBinding, owner, nil)).Code)
	waitSubjectKnown(t, h, lifecycleTarget(t, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID), false)
	s.mu.Lock()
	removed := s.subjectDrain.snapshots[subject.FromSandboxUID(owner.SandboxUID)]
	s.mu.Unlock()
	require.Nil(t, removed)
	require.NotContains(t, s.subjectLifecycleDegraded(), "vault_state_unknown")
}

// Compare committed inputs independently of the mirror's derived digest.
func requireSnapshotContent(t *testing.T, want, got *fastSandboxSubjectSnapshot) {
	t.Helper()
	require.Equal(t, want.payload, got.payload)
	require.Equal(t, want.vaultRevision, got.vaultRevision)
	require.Equal(t, want.policyEpoch, got.policyEpoch)
}
