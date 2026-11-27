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

// v1 missing-file and stale-fence recovery uses an empty fallback, not a fail-closed guarantee.
// The fixture covers Go/Python Unix IPC, not mitmdump/TLS; real atomic persistence requires Linux.

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/stretchr/testify/require"
)

// recoveryGhostClient publishes an epoch whose socket cannot exist, so no
// replay can ever complete. It is the deterministic generation-change window:
// it invalidates the previous epoch exactly like a real child replacement.
func recoveryGhostClient(t *testing.T, control string) (*revision.SubjectLifecycleClient, mitmproxy.SubjectRevisionIPCConfig) {
	t.Helper()
	ghost := mitmproxy.SubjectRevisionIPCConfig{
		SocketPath:        filepath.Join(t.TempDir(), "missing.sock"),
		SessionToken:      "r6ghostsessiontoken000000000000000000",
		ControlGeneration: control,
		MaxSnapshotBytes:  defaultRevisionMaxSnapshotSize,
	}
	client, err := revision.NewSubjectLifecycleClient(ghost.SocketPath, ghost.SessionToken, ghost.ControlGeneration)
	require.NoError(t, err)
	return client, ghost
}

// AC1: A bound, B unbound, same policy hosts. During the child-generation
// window neither subject may become data-plane-ready on the old ACK; after
// replay each subject's digest, revision, policy epoch, and control
// generation match its own snapshot, and A/B never cross.
func TestFastSandboxSubjectRecoveryTwoSubjectGenerationWindow(t *testing.T) {
	s, _, o, launch, configs := snapshotDrainHarness(t)
	h1 := (*configs)[0].SubjectRevisionIPC
	targetA := lifecycleTarget(t, testUID, testFenceR, testFenceA)
	targetB := lifecycleTarget(t, "other", "r2", "a2")
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	require.Equal(t, 201, doVault(t, s, "POST", testUID, vaultCredential(), "").Code)
	require.Equal(t, 200, doAction(t, s, bindBody(t, "other", "10.0.0.9", "r2", "a2", 1, vaultAllowPolicy())).Code)
	snapA := latestSubjectSnapshot(t, s, testUID)
	snapB := latestSubjectSnapshot(t, s, "other")
	require.Equal(t, int64(1), snapA.vaultRevision)
	require.Equal(t, int64(0), snapB.vaultRevision)
	require.NotEqual(t, snapA.digest, snapB.digest, "A and B must not share a snapshot digest")
	require.Contains(t, string(snapA.payload), `"state":"active"`)
	require.Contains(t, string(snapB.payload), `"state":"active-empty"`)
	require.Contains(t, string(snapA.payload), "api.example.com")
	require.NotContains(t, string(snapB.payload), "api.example.com", "B must not inherit A's binding render")
	waitSubjectInstalled(t, h1, targetA, snapA)
	waitSubjectInstalled(t, h1, targetB, snapB)
	waitReadiness(t, s, testUID, testFenceR, testFenceA)
	waitReadiness(t, s, "other", "r2", "a2")
	subjA := subject.FromSandboxUID(testUID)
	s.mu.Lock()
	oldACK := s.subjectDrain.acked[subjA]
	require.NotEmpty(t, oldACK.controlEpoch, "the pre-restart ACK is real child-1 evidence")
	s.mu.Unlock()

	// Deterministic generation-change window: the new epoch has no receiver.
	ghost, ghostConfig := recoveryGhostClient(t, "r6-ghost-control")
	s.publishSubjectClient(ghost, ghostConfig)
	require.NotEqual(t, h1.ControlGeneration, ghostConfig.ControlGeneration, "the new epoch must use a different control generation")
	requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
	requireReadinessBlocked(t, s, "other", "r2", "a2")
	// The pre-restart ACK replanted verbatim still cannot pass: its control
	// epoch names the retired child, not the published one. This is the
	// committed old-ACK rejection evidence; see the report's fault-injection
	// run for the red direction.
	s.mu.Lock()
	s.subjectDrain.acked[subjA] = oldACK
	s.mu.Unlock()
	requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)

	// Publish a fresh production child through the same owner seam a restart
	// uses; publication replaces the ghost epoch and clears the planted ACK.
	child2, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	o.published(child2)
	h2 := (*configs)[1].SubjectRevisionIPC
	require.NotEqual(t, h1.ControlGeneration, h2.ControlGeneration)
	require.NotEqual(t, h1.SessionToken, h2.SessionToken)
	require.NotEqual(t, h1.SocketPath, h2.SocketPath)
	// Replay completes per subject against its own frozen inputs.
	replayA := waitSubjectInstalled(t, h2, targetA, latestSubjectSnapshot(t, s, testUID))
	replayB := waitSubjectInstalled(t, h2, targetB, latestSubjectSnapshot(t, s, "other"))
	require.Equal(t, snapA.payload, latestSubjectSnapshot(t, s, testUID).payload, "A's replayed payload stays its own")
	require.Equal(t, snapB.payload, latestSubjectSnapshot(t, s, "other").payload, "B's replayed payload stays its own")
	require.Equal(t, snapA.vaultRevision, replayA.VaultRevision)
	require.Equal(t, snapA.policyEpoch, replayA.PolicyEpoch)
	require.Equal(t, snapB.vaultRevision, replayB.VaultRevision)
	require.Equal(t, snapB.policyEpoch, replayB.PolicyEpoch)
	require.NotEqual(t, replayA.Digest, replayB.Digest)
	require.Equal(t, h2.ControlGeneration, replayA.ControlGeneration, "the ACK identity belongs to the new child")
	waitReadiness(t, s, testUID, testFenceR, testFenceA)
	waitReadiness(t, s, "other", "r2", "a2")
	require.Empty(t, s.subjectLifecycleDegraded())
}

// AC2: rebind and remove land inside the generation window. The old target
// is never revived by the old queue or ACK, a late detach of the previous
// epoch's client cannot clear the newly published client, and the new target
// only becomes ready through its own install ACK.
func TestFastSandboxSubjectRecoveryRebindRemoveAcrossGeneration(t *testing.T) {
	s, _, o, launch, configs := snapshotDrainHarness(t)
	h1 := (*configs)[0].SubjectRevisionIPC
	oldTarget := lifecycleTarget(t, testUID, testFenceR, testFenceA)
	removedTarget := lifecycleTarget(t, "gone", "rg", "ag")
	newTarget := lifecycleTarget(t, testUID, "r6-r", "r6-a")
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	require.Equal(t, 200, doAction(t, s, bindBody(t, "gone", "10.0.0.9", "rg", "ag", 1, vaultAllowPolicy())).Code)
	waitSubjectInstalled(t, h1, oldTarget, latestSubjectSnapshot(t, s, testUID))
	waitSubjectInstalled(t, h1, removedTarget, latestSubjectSnapshot(t, s, "gone"))
	waitReadiness(t, s, testUID, testFenceR, testFenceA)
	waitReadiness(t, s, "gone", "rg", "ag")
	s.mu.Lock()
	previousClient := s.subjectDrain.current.client
	s.mu.Unlock()

	// Window: the new epoch cannot complete anything.
	ghost, ghostConfig := recoveryGhostClient(t, "r6-ghost-control-2")
	s.publishSubjectClient(ghost, ghostConfig)
	requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
	// Rebind and remove land while the new epoch owns the sequence.
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, "r6-r", "r6-a", 2, vaultAllowPolicy())).Code)
	requireReadinessBlocked(t, s, testUID, "r6-r", "r6-a")
	require.Equal(t, 200, doAction(t, s, removeBody(t, "gone", "rg", "ag")).Code)

	child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	o.published(child)
	h2 := (*configs)[1].SubjectRevisionIPC
	require.NotEqual(t, h1.ControlGeneration, h2.ControlGeneration)
	// The old queue and ACK cannot revive the retired or removed targets.
	waitSubjectKnown(t, h2, oldTarget, false)
	waitSubjectKnown(t, h2, removedTarget, false)
	// The new target completes its own ACK before readiness opens.
	waitSubjectInstalled(t, h2, newTarget, latestSubjectSnapshot(t, s, testUID))
	waitReadiness(t, s, testUID, "r6-r", "r6-a")
	// A late detach of the previous epoch's client must not clear the
	// published client; readiness stays open with no new install.
	s.detachSubjectClient(previousClient)
	require.Equal(t, 200, readinessHook(t, s, testUID, "r6-r", "r6-a").Code)
	s.mu.Lock()
	current := s.subjectDrain.current
	s.mu.Unlock()
	require.NotNil(t, current)
	// Pointer identity only: deep comparison would race with the worker
	// still holding the previous client.
	require.NotSame(t, previousClient, current.client)
	require.Empty(t, s.subjectLifecycleDegraded())
}

// recoveryRestart reopens both managers as startup does, sharing only paths
// with the seed. In particular, no cached per-subject file store survives.
func recoveryRestart(t *testing.T, seed *fastSandboxPolicyServer) *fastSandboxPolicyServer {
	t.Helper()
	pp, err := newPolicyPersistence(seed.policyFiles.dir)
	require.NoError(t, err)
	vp, err := newVaultPersistence(seed.vaultFiles.dir)
	require.NoError(t, err)
	require.NotSame(t, seed.policyFiles, pp)
	require.NotSame(t, seed.vaultFiles, vp)
	require.Empty(t, pp.files)
	require.Empty(t, vp.files)
	s, _, _ := fastSandboxTestServer(t)
	s.SetPolicyFiles(pp)
	s.SetVaultFiles(vp)
	readinessDrain(t, s)
	return s
}

// AC3: control-plane rebuild. A new server object shares only the persisted
// files with the seed server: no drain state, snapshot mirror, or in-memory
// vault is reused. After the SET_BINDING replay, same-fence vault content is
// recovered exactly (A bound, B explicitly empty), and readiness opens only
// after a real install ACK. This is control-plane rebuild evidence over real
// files, not a full Linux egress process restart.
func TestFastSandboxSubjectRecoveryControlPlaneRebuildTwoSubjects(t *testing.T) {
	seed, _, _ := fastSandboxTestServer(t)
	mutationPersistence(t, seed)
	readinessDrain(t, seed)
	require.Equal(t, 200, doAction(t, seed, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	require.Equal(t, 201, doVault(t, seed, "POST", testUID, vaultCredential(), "").Code)
	require.Equal(t, 200, doAction(t, seed, bindBody(t, "other", "10.0.0.9", "r2", "a2", 1, vaultAllowPolicy())).Code)
	h := readinessReceiver(t, seed)
	snapA := latestSubjectSnapshot(t, seed, testUID)
	snapB := latestSubjectSnapshot(t, seed, "other")
	waitSubjectInstalled(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), snapA)
	waitSubjectInstalled(t, h, lifecycleTarget(t, "other", "r2", "a2"), snapB)
	waitReadiness(t, seed, testUID, testFenceR, testFenceA)
	waitReadiness(t, seed, "other", "r2", "a2")

	rebuilt := recoveryRestart(t, seed)
	require.Equal(t, 200, doAction(t, rebuilt, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	require.Equal(t, 200, doAction(t, rebuilt, bindBody(t, "other", "10.0.0.9", "r2", "a2", 1, vaultAllowPolicy())).Code)
	// Nothing is acknowledged before a receiver exists.
	requireReadinessBlocked(t, rebuilt, testUID, testFenceR, testFenceA)
	requireReadinessBlocked(t, rebuilt, "other", "r2", "a2")
	h2 := readinessReceiver(t, rebuilt)
	recoveredA := waitSubjectInstalled(t, h2, lifecycleTarget(t, testUID, testFenceR, testFenceA), latestSubjectSnapshot(t, rebuilt, testUID))
	recoveredB := waitSubjectInstalled(t, h2, lifecycleTarget(t, "other", "r2", "a2"), latestSubjectSnapshot(t, rebuilt, "other"))
	require.Equal(t, snapA.payload, latestSubjectSnapshot(t, rebuilt, testUID).payload, "same-fence vault content must recover exactly")
	require.Equal(t, snapB.payload, latestSubjectSnapshot(t, rebuilt, "other").payload)
	require.Equal(t, snapA.vaultRevision, recoveredA.VaultRevision)
	require.Equal(t, snapA.policyEpoch, recoveredA.PolicyEpoch)
	require.Equal(t, snapB.vaultRevision, recoveredB.VaultRevision)
	require.Equal(t, snapB.policyEpoch, recoveredB.PolicyEpoch)
	require.NotEqual(t, recoveredA.Digest, recoveredB.Digest)
	require.Contains(t, string(latestSubjectSnapshot(t, rebuilt, testUID).payload), "api.example.com")
	require.NotContains(t, string(latestSubjectSnapshot(t, rebuilt, "other").payload), "api.example.com")
	waitReadiness(t, rebuilt, testUID, testFenceR, testFenceA)
	waitReadiness(t, rebuilt, "other", "r2", "a2")
	require.Empty(t, rebuilt.subjectLifecycleDegraded())
}

// AC4: a v1 file that cannot be read keeps the subject unknown across a
// control-plane rebuild AND across a child generation change; it is not
// washed into an empty ready state. The existing explicit recovery path
// (restore the file, re-register) reinstalls and opens readiness, while a
// healthy subject recovers independently.
func TestFastSandboxSubjectRecoveryUnreadableNotWashedByChildGeneration(t *testing.T) {
	seed, _, _ := fastSandboxTestServer(t)
	mutationPersistence(t, seed)
	readinessDrain(t, seed)
	require.Equal(t, 200, doAction(t, seed, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	require.Equal(t, 200, doAction(t, seed, bindBody(t, "other", "10.0.0.9", "r2", "a2", 1, vaultAllowPolicy())).Code)
	require.Equal(t, 201, doVault(t, seed, "POST", testUID, vaultCredential(), "").Code)
	require.Equal(t, 201, doVault(t, seed, "POST", "other", vaultCredential(), "").Code)
	h := readinessReceiver(t, seed)
	waitSubjectInstalled(t, h, lifecycleTarget(t, testUID, testFenceR, testFenceA), latestSubjectSnapshot(t, seed, testUID))
	waitSubjectInstalled(t, h, lifecycleTarget(t, "other", "r2", "a2"), latestSubjectSnapshot(t, seed, "other"))
	waitReadiness(t, seed, testUID, testFenceR, testFenceA)
	waitReadiness(t, seed, "other", "r2", "a2")
	path := filepath.Join(seed.vaultFiles.dir, testUID+".json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	// Real file fault: a directory replaces the file, so reads fail without
	// chmod tricks.
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0700))

	s := recoveryRestart(t, seed)
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
	require.Equal(t, 200, doAction(t, s, bindBody(t, "other", "10.0.0.9", "r2", "a2", 1, vaultAllowPolicy())).Code)
	subjA := subject.FromSandboxUID(testUID)
	s.mu.Lock()
	entry := s.vaults[subjA]
	frozen := s.subjectDrain.snapshots[subjA]
	s.mu.Unlock()
	require.True(t, entry.vaultStateUnknown, "unreadable v1 state stays unknown")
	require.Nil(t, frozen, "unknown state must not be serialized as an empty snapshot")
	requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
	// A healthy subject still recovers and becomes ready.
	h2 := readinessReceiver(t, s)
	waitSubjectInstalled(t, h2, lifecycleTarget(t, "other", "r2", "a2"), latestSubjectSnapshot(t, s, "other"))
	waitReadiness(t, s, "other", "r2", "a2")

	// A child generation change must not wash the unknown state either.
	ghost, ghostConfig := recoveryGhostClient(t, "r6-ghost-control-4")
	s.publishSubjectClient(ghost, ghostConfig)
	requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)
	h3 := readinessReceiver(t, s)
	waitReadiness(t, s, "other", "r2", "a2")
	s.mu.Lock()
	entry = s.vaults[subjA]
	s.mu.Unlock()
	require.True(t, entry.vaultStateUnknown, "the new child generation did not heal the unreadable state")
	requireReadinessBlocked(t, s, testUID, testFenceR, testFenceA)

	// Existing explicit recovery path: restore the file, re-register.
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, raw, 0600))
	require.Equal(t, 200, doAction(t, s, bindBody(t, testUID, testIP, testFenceR, testFenceA, 2, vaultAllowPolicy())).Code)
	waitSubjectInstalled(t, h3, lifecycleTarget(t, testUID, testFenceR, testFenceA), latestSubjectSnapshot(t, s, testUID))
	waitReadiness(t, s, testUID, testFenceR, testFenceA)
	require.Empty(t, s.subjectLifecycleDegraded())
}

// AC5: three vault histories under the default Actions v1 contract. This
// locks in the CURRENT compatibility behavior — a missing v1 file (never
// created, or created and lost) and a stale-fence v1 file all resolve to the
// authoritative empty state. That is a recorded v1 limitation, not the
// fail-closed contract from the original task table; these tests assert it
// honestly instead of claiming rejection.
func TestFastSandboxSubjectRecoveryVaultHistoryDifferential(t *testing.T) {
	for _, history := range []string{"never-created", "created-then-missing", "stale-fence"} {
		t.Run(history, func(t *testing.T) {
			uid := "r6-" + history
			seed, _, _ := fastSandboxTestServer(t)
			mutationPersistence(t, seed)
			readinessDrain(t, seed)
			require.Equal(t, 200, doAction(t, seed, bindBody(t, uid, testIP, "rh-r", "rh-a", 1, vaultAllowPolicy())).Code)
			path := filepath.Join(seed.vaultFiles.dir, uid+".json")
			var raw []byte
			if history != "never-created" {
				require.Equal(t, 201, doVault(t, seed, "POST", uid, vaultCredential(), "").Code)
				h := readinessReceiver(t, seed)
				waitSubjectInstalled(t, h, lifecycleTarget(t, uid, "rh-r", "rh-a"), latestSubjectSnapshot(t, seed, uid))
				waitReadiness(t, seed, uid, "rh-r", "rh-a")
				var err error
				raw, err = os.ReadFile(path)
				require.NoError(t, err)
			}
			switch history {
			case "never-created":
				_, err := os.Stat(path)
				require.True(t, os.IsNotExist(err), "never-created must have no vault file before rebuild; stat error: %v", err)
			case "created-then-missing":
				require.NoError(t, os.Remove(path))
			case "stale-fence":
				vf, body, err := credentialvault.DecodeVaultFile(raw)
				require.NoError(t, err)
				vf.Incarnation = "r6-stale"
				b, err := credentialvault.EncodeVaultFile(vf.Incarnation, vf.AttachmentID, vf.Revision, body)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, b, 0600))
			}

			s := recoveryRestart(t, seed)
			require.Equal(t, 200, doAction(t, s, bindBody(t, uid, testIP, "rh-r", "rh-a", 1, vaultAllowPolicy())).Code)
			h := readinessReceiver(t, s)
			snap := latestSubjectSnapshot(t, s, uid)
			// Current v1 compatibility behavior: authoritative empty.
			require.Contains(t, string(snap.payload), `"state":"active-empty"`)
			require.Equal(t, int64(0), snap.vaultRevision)
			waitSubjectInstalled(t, h, lifecycleTarget(t, uid, "rh-r", "rh-a"), snap)
			waitReadiness(t, s, uid, "rh-r", "rh-a")
			if history == "stale-fence" {
				require.Eventually(t, func() bool {
					_, err := os.Stat(path)
					return os.IsNotExist(err)
				}, 5*time.Second, 10*time.Millisecond, "the stale v1 file is discarded by the legacy cleanup")
			}
		})
	}
}

// AC9 test-layer isolation: the recovery fixtures add no process environment
// and keep the production drain deadline on the servers they touch.
func TestFastSandboxSubjectRecoveryTestLayerIsolation(t *testing.T) {
	before := recoveryEnvironSnapshot()
	s, _, o, launch, configs := snapshotDrainHarness(t)
	require.Equal(t, subjectLifecycleDeadline, s.subjectDrain.deadline, "fixtures keep the production drain deadline")
	child, err := o.launch(context.Background(), mitmproxy.Config{}, launch)
	require.NoError(t, err)
	o.published(child)
	require.Len(t, *configs, 2)
	o.stopDrain()
	require.Equal(t, before, recoveryEnvironSnapshot(), "no environment leaked into the test process")
}

func recoveryEnvironSnapshot() map[string]string {
	out := make(map[string]string)
	for _, entry := range os.Environ() {
		for i := 0; i < len(entry); i++ {
			if entry[i] == '=' {
				out[entry[:i]] = entry[i+1:]
				break
			}
		}
	}
	return out
}
