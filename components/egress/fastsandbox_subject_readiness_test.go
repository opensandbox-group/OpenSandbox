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
	"net/http/httptest"
	"os"
	"path/filepath"
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

func readinessDrain(t *testing.T, s *fastSandboxPolicyServer) {
	t.Helper()
	s.startSubjectDrain()
	t.Cleanup(func() { s.subjectDrain.cancel(); <-s.subjectDrain.done })
}

// Publish only after the test has asserted the no-receiver window. The addon
// and Unix transports are the production implementations; no fake ACK exists.
func readinessReceiver(t *testing.T, s *fastSandboxPolicyServer) *mitmproxy.SubjectRevisionIPCConfig {
	t.Helper()
	o := subjectTestOwner(t)
	launch, configs := subjectAddonLauncher(t, o)
	child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	session := o.sessions[child]
	s.publishSubjectClient(session.client, session.handoff)
	return (*configs)[0].SubjectRevisionIPC
}

func readinessHook(t *testing.T, s *fastSandboxPolicyServer, uid, r, a string) *httptest.ResponseRecorder {
	t.Helper()
	if s.actionsAPIVersion == actionhandler.APIVersionV2 {
		record, err := s.readOwnerRecord(subject.FromSandboxUID(uid))
		require.NoError(t, err)
		require.NotNil(t, record)
		return doActionsV2(t, s, actionsV2Body(t, actionhandler.OperationLifecycleHook, record.Owner, nil))
	}
	return doAction(t, s, hookBody(t, uid, r, a, constants.HookDataPlaneReady))
}

func requireReadinessBlocked(t *testing.T, s *fastSandboxPolicyServer, uid, r, a string) {
	t.Helper()
	rec := readinessHook(t, s, uid, r, a)
	require.Equal(t, 503, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "subject snapshot not acknowledged")
	require.True(t, s.actionStatusResponse().Ready, "readiness is per subject")
}

func waitReadiness(t *testing.T, s *fastSandboxPolicyServer, uid, r, a string) {
	t.Helper()
	require.Eventually(t, func() bool { return readinessHook(t, s, uid, r, a).Code == 200 }, 5*time.Second, 5*time.Millisecond, "current snapshot ACK must release the hook")
}

func TestFastSandboxSubjectReadinessFirstSETAndCurrentPayload(t *testing.T) {
	s, reg, _ := fastSandboxTestServer(t)
	mutationPersistence(t, s)
	readinessDrain(t, s)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	require.Equal(t, 200, doAction(t, s, hookBody(t, testUID, testFenceR, testFenceA, constants.HookRuntimeReady)).Code)
	first := latestSubjectSnapshot(t, s, testUID)
	requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
	state, ok := reg.Get(subject.FromSandboxUID(testUID))
	require.True(t, ok)
	require.Equal(t, subject.StateDenying, state)
	require.Nil(t, reg.EffectivePolicy(subject.FromSandboxUID(testUID)))
	require.Equal(t, 201, doVault(t, s, "POST", testUID, vaultCredential(), "").Code)
	require.Equal(t, 200, doPolicy(t, s, "PUT", testUID, policyPutDoc(*vaultAllowPolicy(), 0, false, false), "").Code)
	latest := latestSubjectSnapshot(t, s, testUID)
	require.NotEqual(t, string(first.payload), string(latest.payload))
	requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
	h := readinessReceiver(t, s)
	waitSubjectInstalled(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), latest)
	waitReadiness(t, s, testUID, testFenceR, testFenceA)
	state, _ = reg.Get(subject.FromSandboxUID(testUID))
	require.Equal(t, subject.StateActive, state)
}

func TestFastSandboxSubjectReadinessConfirmedIntentAndSlowIPC(t *testing.T) {
	s, h, fault := snapshotFaultHarness(t, "", "readiness")
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	select {
	case <-fault.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("Commit not reached")
	}
	requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
	// While the first Commit result is uncertain, all handlers finish and the
	// newer payload supersedes the mirror. The FIFO is still owned by Readback.
	done := make(chan struct{})
	go func() {
		defer close(done)
		require.Equal(t, 200, doAction(t, s, bindBody(t, "other", "10.0.0.9", "r2", "a2", 1, vaultAllowPolicy())).Code)
		require.Equal(t, 201, doVault(t, s, "POST", "other", vaultCredential(), "").Code)
		require.Equal(t, 201, doVault(t, s, "POST", testUID, vaultCredential(), "").Code)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handlers waited for IPC")
	}
	latest := latestSubjectSnapshot(t, s, testUID)
	close(fault.release)
	select {
	case <-fault.retryReached:
	case <-time.After(5 * time.Second):
		t.Fatal("successor Prepare not reached")
	}
	// Readback confirmed only the first intent. Stamping the current mirror
	// here would incorrectly open this gate while its Prepare is still blocked.
	requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
	close(fault.retryRelease)
	waitSubjectInstalled(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), latest)
	waitReadiness(t, s, testUID, testFenceR, testFenceA)
	waitReadiness(t, s, "other", "r2", "a2")
}

func TestFastSandboxSubjectReadinessRegistrationInstance(t *testing.T) {
	for _, variant := range []string{"same-fence-same-payload", "same-fence-different-payload", "different-fence-same-payload"} {
		t.Run(variant, func(t *testing.T) {
			s, h, fault := snapshotFaultHarness(t, "", "readiness")
			mutationPersistence(t, s)
			require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
			select {
			case <-fault.reached:
			case <-time.After(5 * time.Second):
				t.Fatal("old registration Commit not reached")
			}
			old := latestSubjectSnapshot(t, s, testUID)
			requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
			require.Equal(t, 200, doAction(t, s, removeBody(t, testUID, testFenceR, testFenceA)).Code)
			r, a := testFenceR, testFenceA
			if variant == "different-fence-same-payload" {
				r, a = "next-r", "next-a"
			}
			require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, r, a, 2, vaultAllowPolicy())).Code)
			if variant == "same-fence-different-payload" {
				require.Equal(t, 201, doVault(t, s, "POST", testUID, vaultCredential(), "").Code)
			}
			latest := latestSubjectSnapshot(t, s, testUID)
			if variant == "same-fence-different-payload" {
				require.NotEqual(t, old.payload, latest.payload)
			} else {
				require.Equal(t, old.payload, latest.payload, "the regression requires byte-identical payloads")
			}
			requireReadinessBlocked(t, s, testUID, r, a)
			// Release only the old registration's real Readback confirmation.
			// The next Prepare barrier proves the worker stamped that result,
			// while the new registration's install cannot yet be acknowledged.
			close(fault.release)
			select {
			case <-fault.retryReached:
			case <-time.After(5 * time.Second):
				t.Fatal("new registration Prepare not reached")
			}
			requireReadinessBlocked(t, s, testUID, r, a)
			close(fault.retryRelease)
			waitSubjectInstalled(t, h, lifecycleTarget(t, testUID, r, a), latest)
			waitReadiness(t, s, testUID, r, a)
		})
	}
}

func TestFastSandboxSubjectReadinessReconcileAndPermanentFailure(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncertain-commit", true: "rejected"}[terminal], func(t *testing.T) {
			outcome, operation := "internal_error", "commit-after"
			if terminal {
				outcome, operation = "revision_rejected", "prepare"
			}
			s, _, fault := snapshotFaultHarness(t, outcome, operation)
			require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
			select {
			case <-fault.reached:
			case <-time.After(5 * time.Second):
				t.Fatal("fault not reached")
			}
			requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
			if terminal {
				require.Eventually(t, func() bool { return len(s.subjectLifecycleDegraded()) > 0 }, 5*time.Second, 5*time.Millisecond)
				for i := 0; i < 3; i++ {
					requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
				}
				state, _ := s.reg.Get(subject.FromSandboxUID(testUID))
				require.Equal(t, subject.StateDenying, state)
			} else {
				close(fault.release)
				waitReadiness(t, s, testUID, testFenceR, testFenceA)
			}
		})
	}
}

func TestFastSandboxSubjectReadinessChildEpochDetachAndFence(t *testing.T) {
	s, _, _ := fastSandboxTestServer(t)
	readinessDrain(t, s)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	readinessReceiver(t, s)
	waitReadiness(t, s, testUID, testFenceR, testFenceA)
	s.mu.Lock()
	old := s.subjectDrain.current.client
	s.mu.Unlock()
	s.detachSubjectClient(old)
	requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
	// Publication with no listening socket deterministically invalidates the
	// previous epoch. A fresh real child then replays the immutable snapshot.
	ghost := mitmproxy.SubjectRevisionIPCConfig{SocketPath: filepath.Join(t.TempDir(), "missing.sock"), SessionToken: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ControlGeneration: "next-control", MaxSnapshotBytes: defaultRevisionMaxSnapshotSize}
	client, err := revision.NewSubjectLifecycleClient(ghost.SocketPath, ghost.SessionToken, ghost.ControlGeneration)
	require.NoError(t, err)
	s.publishSubjectClient(client, ghost)
	requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
	readinessReceiver(t, s)
	waitReadiness(t, s, testUID, testFenceR, testFenceA)
	s.mu.Lock()
	old = s.subjectDrain.current.client
	s.mu.Unlock()
	s.detachSubjectClient(old)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, "next-r", "next-a", 2, vaultAllowPolicy())).Code)
	requireReadinessBlocked(t, s, testUID, "next-r", "next-a")
	require.Equal(t, 409, readinessHook(t, s, testUID, testFenceR, testFenceA).Code)
	readinessReceiver(t, s)
	waitReadiness(t, s, testUID, "next-r", "next-a")
	require.Equal(t, 200, doAction(t, s, removeBody(t, testUID, "next-r", "next-a")).Code)
	require.Equal(t, 409, readinessHook(t, s, testUID, "next-r", "next-a").Code)
}

// Durable files are produced by real relay operations, then edited on disk to
// select each recovery branch. No persistence interface is mocked.
func readinessLegacySeed(t *testing.T) *fastSandboxPolicyServer {
	t.Helper()
	s, _, _ := fastSandboxTestServer(t)
	mutationPersistence(t, s)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	require.Equal(t, 200, readinessHook(t, s, testUID, testFenceR, testFenceA).Code)
	require.Equal(t, 201, doVault(t, s, "POST", testUID, vaultCredential(), "").Code)
	return s
}

func readinessRestart(t *testing.T, seed *fastSandboxPolicyServer, strict bool) *fastSandboxPolicyServer {
	t.Helper()
	s, _, _ := fastSandboxTestServer(t)
	s.SetPolicyFiles(seed.policyFiles)
	s.SetVaultFiles(seed.vaultFiles)
	if strict {
		s.SetActionsAPIVersion(actionhandler.APIVersionV2)
	}
	readinessDrain(t, s)
	return s
}

func TestFastSandboxSubjectReadinessAuthoritativeEmptyLegacyMatrix(t *testing.T) {
	for _, cell := range []string{"first-no-file", "replay-same-fence", "corrupt-v1", "policy-held", "reload-rejected", "unreadable", "store-unavailable", "stale-v1", "post-install-degraded", "post-install-unknown"} {
		t.Run(cell, func(t *testing.T) {
			seed := readinessLegacySeed(t)
			path := filepath.Join(seed.vaultFiles.dir, testUID+".json")
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			polPath := filepath.Join(seed.policyFiles.dir, testUID+".json")
			input := vaultAllowPolicy()
			authoritative := cell == "first-no-file" || cell == "replay-same-fence" || cell == "stale-v1"
			switch cell {
			case "first-no-file":
				require.NoError(t, os.Remove(path))
			case "corrupt-v1":
				require.NoError(t, os.WriteFile(path, []byte("{bad"), 0600))
			case "policy-held":
				require.NoError(t, os.WriteFile(polPath, []byte("{bad"), 0600))
			case "reload-rejected":
				require.NoError(t, os.Remove(polPath))
				text := policyAllowAlt()
				input = &text
			case "unreadable":
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0700))
			case "store-unavailable":
				require.NoError(t, os.Rename(seed.vaultFiles.dir, seed.vaultFiles.dir+".saved"))
				require.NoError(t, os.WriteFile(seed.vaultFiles.dir, []byte("not a directory"), 0600))
				seed.vaultFiles.files = make(map[subject.Subject]vaultFileStore)
			case "stale-v1":
				vf, body, e := credentialvault.DecodeVaultFile(raw)
				require.NoError(t, e)
				vf.Incarnation = "stale"
				b, e := credentialvault.EncodeVaultFile(vf.Incarnation, vf.AttachmentID, vf.Revision, body)
				require.NoError(t, e)
				require.NoError(t, os.WriteFile(path, b, 0600))
			}
			s := readinessRestart(t, seed, false)
			require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, input)).Code)
			subj := subject.FromSandboxUID(testUID)
			if cell == "post-install-degraded" || cell == "post-install-unknown" {
				h := readinessReceiver(t, s)
				waitSubjectInstalled(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), latestSubjectSnapshot(t, s, testUID))
				waitReadiness(t, s, testUID, testFenceR, testFenceA)
				s.policyCommitMu.Lock()
				s.mu.Lock()
				entry := s.vaults[subj]
				if cell == "post-install-unknown" {
					entry.vaultStateUnknown = true
				} else {
					entry.degraded = "held for recovery"
				}
				s.vaults[subj] = entry
				s.mu.Unlock()
				s.policyCommitMu.Unlock()
			}
			s.mu.Lock()
			frozen := s.subjectDrain.snapshots[subj]
			entry := s.vaults[subj]
			s.mu.Unlock()
			if authoritative {
				require.NotNil(t, frozen)
				wantRevision := int64(1)
				if cell != "replay-same-fence" {
					wantRevision = 0
					require.Contains(t, string(frozen.payload), `"state":"active-empty"`)
				}
				require.Equal(t, wantRevision, frozen.vaultRevision)
			} else {
				require.True(t, entry.vaultStateUnknown || entry.degraded != "")
				if cell != "post-install-degraded" && cell != "post-install-unknown" {
					require.Nil(t, frozen, "unrestored state must not be serialized as empty")
				}
			}
			requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
			if !authoritative {
				switch cell {
				case "unreadable":
					require.NoError(t, os.Remove(path))
					require.NoError(t, os.WriteFile(path, raw, 0600))
					require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 2, vaultAllowPolicy())).Code)
				case "store-unavailable":
					require.NoError(t, os.Remove(seed.vaultFiles.dir))
					require.NoError(t, os.Rename(seed.vaultFiles.dir+".saved", seed.vaultFiles.dir))
					require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 2, vaultAllowPolicy())).Code)
				case "post-install-unknown":
					s.policyCommitMu.Lock()
					s.rehydrateVaultReload(subj, subject.Fencing{RuntimeInstanceID: testFenceR, AttachmentID: testFenceA}, mustReadinessPolicy(t, *vaultAllowPolicy()), false)
					s.recordBindingSnapshot(subj, subject.Fencing{RuntimeInstanceID: testFenceR, AttachmentID: testFenceA})
					s.policyCommitMu.Unlock()
				default:
					// A policy relay alone does NOT clear the held Vault's degraded flag.
					// Heal the validation policy first, then commit a real Vault relay write.
					require.Equal(t, 200, doPolicy(t, s, "PUT", testUID, policyPutDoc(*vaultAllowPolicy(), 0, false, false), "").Code)
					requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
					method, body := "POST", vaultCredential()
					if cell == "post-install-degraded" {
						method, body = "PUT", vaultPut("1")
					}
					rec := doVault(t, s, method, testUID, body, "")
					require.Contains(t, []int{200, 201}, rec.Code, rec.Body.String())
				}
			}
			h := readinessReceiver(t, s)
			waitSubjectInstalled(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), latestSubjectSnapshot(t, s, testUID))
			waitReadiness(t, s, testUID, testFenceR, testFenceA)
		})
	}
}

func TestFastSandboxSubjectReadinessStrictAuthorityMatrix(t *testing.T) {
	for _, cell := range []string{"fresh", "deleted-v2", "pending-verify", "contract-file-missing", "unknown-version", "protected-disappeared", "stale-v2", "owner-unavailable", "post-install-unknown"} {
		t.Run(cell, func(t *testing.T) {
			seed, owner := writerTestServer(t)
			require.Equal(t, 200, writerInit(t, seed, owner, true))
			if cell == "deleted-v2" {
				require.Equal(t, 201, writerMutation(t, seed, owner, "POST", writerState(t, seed, owner), vaultCredential()))
				require.Equal(t, 204, writerMutation(t, seed, owner, "DELETE", writerState(t, seed, owner), ""))
			}
			path := filepath.Join(seed.vaultFiles.dir, owner.SandboxUID+".json")
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			s := seed
			if cell == "fresh" {
				// Start a new strict subject with no contract; fresh explicitly commits empty.
				other := actionsV2Owner("b", "runtime-b", "attachment-b", 1)
				readinessDrain(t, s)
				require.Equal(t, 200, doActionsV2(t, s, actionsV2Body(t, actionhandler.OperationSetBinding, other, vaultAllowPolicy())).Code)
				require.Equal(t, 200, writerInit(t, s, other, true))
				owner = other
			} else {
				if cell == "contract-file-missing" {
					require.NoError(t, os.Remove(path))
				}
				if cell == "unknown-version" {
					require.NoError(t, os.WriteFile(path, []byte(`{"version":99}`), 0600))
				}
				if cell == "stale-v2" {
					vf, body, e := credentialvault.DecodeVaultFile(raw)
					require.NoError(t, e)
					vf.Incarnation = "stale"
					b, e := credentialvault.EncodeVaultFileV2(vf, body)
					require.NoError(t, e)
					require.NoError(t, os.WriteFile(path, b, 0600))
				}
				s = readinessRestart(t, seed, true)
				if cell == "owner-unavailable" {
					ownerPath := filepath.Join(seed.policyFiles.dir, owner.SandboxUID+".json.owner")
					ownerRaw, e := os.ReadFile(ownerPath)
					require.NoError(t, e)
					require.NoError(t, os.Remove(ownerPath))
					// Exercise the actual rehydrate owner failure before restoring authority.
					s.rehydrateVaultReload(subject.FromSandboxUID(owner.SandboxUID), subject.Fencing{RuntimeInstanceID: owner.RuntimeInstanceID, AttachmentID: owner.AttachmentID}, mustReadinessPolicy(t, *vaultAllowPolicy()), false)
					require.NoError(t, os.WriteFile(ownerPath, ownerRaw, 0600))
				}
				require.Equal(t, 200, doActionsV2(t, s, actionsV2Body(t, actionhandler.OperationSetBinding, owner, vaultAllowPolicy())).Code)
				if cell == "post-install-unknown" || cell == "protected-disappeared" {
					require.Equal(t, 200, writerInit(t, s, owner, false))
					h := readinessReceiver(t, s)
					checkMutationInstalled(t, s, h, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID)
					waitReadiness(t, s, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID)
					if cell == "post-install-unknown" {
						mismatchMutationVault(t, s, owner)
					} else {
						require.NoError(t, os.Remove(path))
						require.Equal(t, 503, doActionsV2Relay(t, s, "GET", vaultStatePrefix+owner.SandboxUID, owner, "").Code)
					}
				}
				requireReadinessBlocked(t, s, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID)
				if cell == "contract-file-missing" || cell == "unknown-version" || cell == "stale-v2" || cell == "protected-disappeared" {
					require.NotEqual(t, 200, writerInit(t, s, owner, false))
					require.NoError(t, os.WriteFile(path, raw, 0600))
				}
				require.Equal(t, 200, writerInit(t, s, owner, false))
			}
			h := readinessReceiver(t, s)
			checkMutationInstalled(t, s, h, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID)
			waitReadiness(t, s, owner.SandboxUID, owner.RuntimeInstanceID, owner.AttachmentID)
			require.Contains(t, string(latestSubjectSnapshot(t, s, owner.SandboxUID).payload), `"state":"active-empty"`)
		})
	}
}

func TestFastSandboxSubjectReadinessUnknownRebindAndChild(t *testing.T) {
	seed := readinessLegacySeed(t)
	s := readinessRestart(t, seed, false)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	readinessReceiver(t, s)
	waitReadiness(t, s, testUID, testFenceR, testFenceA)
	subj := subject.FromSandboxUID(testUID)
	fence := subject.Fencing{RuntimeInstanceID: testFenceR, AttachmentID: testFenceA}
	s.policyCommitMu.Lock()
	s.mu.Lock()
	entry := s.vaults[subj]
	entry.vaultStateUnknown = true
	s.vaults[subj] = entry
	s.markSubjectVaultUnknownLocked(subj, fence)
	s.mu.Unlock()
	s.policyCommitMu.Unlock()
	requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
	require.Equal(t, 503, doAction(t, s, bindBody(t, testUID, testIP, "next-r", "next-a", 2, vaultAllowPolicy())).Code)
	current, ok := s.reg.Fence(subj)
	require.True(t, ok)
	require.Equal(t, fence, current)
	readinessReceiver(t, s)
	require.Contains(t, s.subjectLifecycleDegraded(), "vault_state_unknown", "child publication retains Go authority state")
	requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 2, vaultAllowPolicy())).Code)
	waitReadiness(t, s, testUID, testFenceR, testFenceA)
	require.NotContains(t, s.subjectLifecycleDegraded(), "vault_state_unknown")
	s.mu.Lock()
	client := s.subjectDrain.current.client
	s.mu.Unlock()
	s.detachSubjectClient(client)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, "next-r", "next-a", 3, vaultAllowPolicy())).Code)
	requireReadinessBlocked(t, s, testUID, "next-r", "next-a")
	require.NotContains(t, s.subjectLifecycleDegraded(), "vault_state_unknown", "successful rebind clears old bookkeeping")
	readinessReceiver(t, s)
	waitReadiness(t, s, testUID, "next-r", "next-a")
}

// Compare these complete records between the baseline archive and final tree.
// The drain stays absent, as it does for flag-off and sidecar assembly.
func TestFastSandboxSubjectReadinessDisabledRecoveryDifferential(t *testing.T) {
	for _, cell := range []string{"corrupt-v1", "policy-held", "reload-rejected", "unreadable"} {
		t.Run(cell, func(t *testing.T) {
			seed := readinessLegacySeed(t)
			path := filepath.Join(seed.vaultFiles.dir, testUID+".json")
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			input := vaultAllowPolicy()
			switch cell {
			case "corrupt-v1":
				require.NoError(t, os.WriteFile(path, []byte("{bad"), 0600))
			case "policy-held":
				require.NoError(t, os.WriteFile(filepath.Join(seed.policyFiles.dir, testUID+".json"), []byte("{bad"), 0600))
			case "reload-rejected":
				require.NoError(t, os.Remove(filepath.Join(seed.policyFiles.dir, testUID+".json")))
				text := policyAllowAlt()
				input = &text
			case "unreadable":
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0700))
			}
			s, _, _ := fastSandboxTestServer(t)
			s.SetPolicyFiles(seed.policyFiles)
			s.SetVaultFiles(seed.vaultFiles)
			var observations []mutationObservation
			observe := func(rec *httptest.ResponseRecorder) {
				observations = append(observations, mutationObservation{rec.Code, rec.Body.String(), s.actionStatusResponse().Ready, reg_UserEvaluate(s, subject.FromSandboxUID(testUID), "api.example.com")})
			}
			observe(doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, input)))
			s.mu.Lock()
			entry := s.vaults[subject.FromSandboxUID(testUID)]
			s.mu.Unlock()
			observe(doAction(t, s, hookBody(t, testUID, testFenceR, testFenceA, constants.HookRuntimeReady)))
			observe(readinessHook(t, s, testUID, testFenceR, testFenceA))
			if cell == "unreadable" {
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.WriteFile(path, raw, 0600))
				observe(doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 2, vaultAllowPolicy())))
			}
			observe(doPolicy(t, s, "PUT", testUID, policyPutDoc(*vaultAllowPolicy(), 0, false, false), ""))
			method, body := "POST", vaultCredential()
			if cell == "unreadable" {
				method, body = "PUT", vaultPut("1")
			}
			observe(doVault(t, s, method, testUID, body, ""))
			require.Nil(t, s.subjectDrain)
			s.mu.Lock()
			healed := s.vaults[subject.FromSandboxUID(testUID)]
			s.mu.Unlock()
			require.Empty(t, healed.degraded)
			require.False(t, healed.vaultStateUnknown)
			payload, err := json.Marshal(struct {
				Cell              string
				Unknown, Degraded bool
				HTTP              []mutationObservation
			}{cell, entry.vaultStateUnknown, entry.degraded != "", observations})
			require.NoError(t, err)
			t.Logf("DISABLED-RECOVERY %s", payload)
		})
	}
}

func mustReadinessPolicy(t *testing.T, raw string) *policy.NetworkPolicy {
	t.Helper()
	p, err := policy.ParsePolicy(raw)
	require.NoError(t, err)
	return p
}

func TestFastSandboxSubjectReadinessHookPriorities(t *testing.T) {
	s, _, _ := fastSandboxTestServer(t)
	readinessDrain(t, s)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, nil)).Code)
	require.Equal(t, 409, readinessHook(t, s, testUID, testFenceR, testFenceA).Code, "missing pending policy precedes ACK gate")
	require.Equal(t, 409, readinessHook(t, s, testUID, "stale", testFenceA).Code, "fence rejection precedes ACK gate")
	require.Equal(t, 200, doAction(t, s, hookBody(t, testUID, testFenceR, testFenceA, constants.HookRuntimeReady)).Code)
}

func TestFastSandboxSubjectReadinessRebindPurgeRealState(t *testing.T) {
	for _, cause := range []string{"unknown-entry", "v2-file"} {
		t.Run(cause, func(t *testing.T) {
			seed := readinessLegacySeed(t)
			s := readinessRestart(t, seed, false)
			require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
			h := readinessReceiver(t, s)
			waitReadiness(t, s, testUID, testFenceR, testFenceA)
			subj := subject.FromSandboxUID(testUID)
			target := lifecycleTarget(t, testUID, testFenceR, testFenceA)
			old := latestSubjectSnapshot(t, s, testUID)
			path := filepath.Join(s.vaultFiles.dir, testUID+".json")
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			if cause == "unknown-entry" {
				s.policyCommitMu.Lock()
				s.mu.Lock()
				entry := s.vaults[subj]
				entry.vaultStateUnknown = true
				s.vaults[subj] = entry
				s.markSubjectVaultUnknownLocked(subj, entry.fence)
				s.mu.Unlock()
				s.policyCommitMu.Unlock()
				requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
			} else {
				// A valid v2 file is protected even when the cached v1 entry is known.
				vf, body, e := credentialvault.DecodeVaultFile(raw)
				require.NoError(t, e)
				vf.Version = credentialvault.VaultFileVersion2
				vf.State = credentialvault.FileStateActive
				vf.StateLineage = "lineage"
				vf.StateRevision = 1
				vf.InitializationID = "init"
				b, e := credentialvault.EncodeVaultFileV2(vf, body)
				require.NoError(t, e)
				require.NoError(t, os.WriteFile(path, b, 0600))
			}
			response := doAction(t, s, bindBody(t, testUID, testIP, "next-r", "next-a", 2, vaultAllowPolicy()))
			want := 503
			if cause == "v2-file" {
				want = 409
			}
			require.Equal(t, want, response.Code, response.Body.String())
			current, ok := s.reg.Fence(subj)
			require.True(t, ok)
			require.Equal(t, subject.Fencing{RuntimeInstanceID: testFenceR, AttachmentID: testFenceA}, current)
			waitSubjectKnown(t, h, target, true)
			waitLifecycleIdle(t, s, h, 1)
			require.Same(t, old, latestSubjectSnapshot(t, s, testUID))
			s.mu.Lock()
			require.Empty(t, s.subjectDrain.queue, "a refused purge queues neither Unload nor compensating Register")
			s.mu.Unlock()
			require.NoError(t, os.WriteFile(path, raw, 0600))
			require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 2, vaultAllowPolicy())).Code)
			waitReadiness(t, s, testUID, testFenceR, testFenceA)
			require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, "next-r", "next-a", 3, vaultAllowPolicy())).Code)
			waitReadiness(t, s, testUID, "next-r", "next-a")
			waitSubjectKnown(t, h, target, false)
			waitLifecycleIdle(t, s, h, 4)
		})
	}
}
