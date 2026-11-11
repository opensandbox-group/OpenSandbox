// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/alibaba/opensandbox/egress/pkg/revisionruntime"
	"github.com/stretchr/testify/require"
)

// Real Go owner -> ProcessSession -> Unix HTTP -> Python LiveReceiver. nft is
// the explicit local-effect seam; these are not packet or mitmdump evidence.
func revisionPolicyIPCFixture(t *testing.T, bound bool) (*policyServer, *mitmTransparent, *revisionIPCChild, *stubNft) {
	t.Helper()
	t.Setenv(constants.EnvExperimentalRevisionRuntime, "true")
	t.Setenv(constants.EnvEgressMode, constants.PolicyDnsNft)
	s := revisionIntegrationServer(t)
	if bound {
		revisionIntegrationCreateRendered(t, s, "private-policy-ipc-token")
	}
	m, child, _ := revisionIntegrationLaunchConfig(t, s, func(cfg *revisionruntime.ProcessSessionConfig) {
		cfg.LiveAdmission = true
		cfg.TLSCapacity, cfg.RequestCapacity, cfg.DrainTimeoutSeconds = 64, 256, 30
	})
	s.mitm = m
	nft := &stubNft{}
	s.nft = nft
	return s, m, child, nft
}

func assertRevisionPolicyIPCIdentity(t *testing.T, child *revisionIPCChild, snapshot credentialvault.ActiveSnapshot, epoch int64) revisionChildReport {
	t.Helper()
	report := child.exchange(t, map[string]string{"command": "inspect"})
	payload, err := credentialvault.MarshalDecisionSnapshot(snapshot, epoch)
	require.NoError(t, err)
	sum := sha256.Sum256(payload)
	require.NotNil(t, report.Active)
	require.Equal(t, report.Active, report.View, "active and selector view must publish jointly")
	require.Equal(t, epoch, report.Active.PolicyEpoch)
	require.Equal(t, snapshot.Revision, report.Active.VaultRevision)
	require.Equal(t, hex.EncodeToString(sum[:]), report.Digest)
	require.Equal(t, report.Digest, report.Active.Digest)
	require.False(t, report.AdmissionDisabled)
	return report
}

func TestRevisionPolicyIPCRealHandlersAndEpochContinuity(t *testing.T) {
	s, _, child, nft := revisionPolicyIPCFixture(t, true)
	s.policyFile = filepath.Join(t.TempDir(), "policy.json")
	before, err := s.credentialVault.FreezeForPolicy(s.proxy.CurrentPolicy())
	require.NoError(t, err)
	initial := assertRevisionPolicyIPCIdentity(t, child, before.Snapshot(), 0)
	for index, step := range []struct{ method, body string }{
		{http.MethodPost, `{"defaultAction":"deny","egress":[{"action":"allow","target":"api.example.com"}]}`},
		{http.MethodPut, `{"defaultAction":"deny","egress":[{"action":"allow","target":"api.example.com"},{"action":"allow","target":"spare.example.com"}]}`},
		{http.MethodPatch, `[{"action":"allow","target":"new.example.com"}]`},
		{http.MethodDelete, `["spare.example.com"]`},
	} {
		response := revisionPolicyDo(t, s, step.method, step.body)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		epoch := int64(index + 1)
		report := assertRevisionPolicyIPCIdentity(t, child, before.Snapshot(), epoch)
		require.Equal(t, initial.Active.DecisionEpoch+epoch, report.Active.DecisionEpoch)
		require.Equal(t, index+2, report.Counts.Prepare)
		require.Equal(t, index+2, report.Counts.Commit)
		require.Equal(t, epoch, s.revisionRecovery.current.epoch)
		require.NoError(t, s.credentialVault.ValidatePolicySnapshot(before))
		raw, err := os.ReadFile(s.policyFile)
		require.NoError(t, err)
		persisted, err := policy.ParsePolicy(string(raw))
		require.NoError(t, err)
		require.Equal(t, s.proxy.CurrentPolicy().Egress, persisted.Egress)
	}
	patch := func(expected int64, token string) credentialvault.ActiveSnapshot {
		body, err := json.Marshal(credentialvault.MutationRequest{ExpectedRevision: &expected, Credentials: &credentialvault.CredentialMutationSet{Replace: integrationVaultRequest(token).Credentials}})
		require.NoError(t, err)
		response := liveWriteDo(t, s, http.MethodPatch, string(body))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		snapshot, err := s.credentialVault.ActiveSnapshot()
		require.NoError(t, err)
		return snapshot
	}
	rotated := patch(1, "private-policy-ipc-second")
	afterVault := assertRevisionPolicyIPCIdentity(t, child, rotated, 4)
	fileBeforeReload, err := os.ReadFile(s.policyFile)
	require.NoError(t, err)
	loader := &stagedTestAlwaysLoader{candidateAllow: []policy.EgressRule{mustRule(t, policy.ActionAllow, "reload.example.com")}, pending: true}
	s.alwaysLoader = loader
	changed, err := s.reloadAlwaysRules()
	require.NoError(t, err)
	require.True(t, changed)
	afterReload := assertRevisionPolicyIPCIdentity(t, child, rotated, 5)
	require.Equal(t, afterVault.Active.DecisionEpoch+1, afterReload.Active.DecisionEpoch)
	require.Equal(t, loader.allow, s.proxy.(*stubProxy).allow)
	require.Equal(t, policy.ActionAllow, nft.applied.Evaluate("reload.example.com"))
	fileAfterReload, err := os.ReadFile(s.policyFile)
	require.NoError(t, err)
	require.Equal(t, fileBeforeReload, fileAfterReload, "operator reload must not persist user policy")
	rotated = patch(2, "private-policy-ipc-third")
	final := assertRevisionPolicyIPCIdentity(t, child, rotated, 5)
	require.Equal(t, afterReload.Active.DecisionEpoch+1, final.Active.DecisionEpoch)
	snapshot, epoch, _, err := s.captureRevisionBootstrap(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(5), epoch)
	require.Equal(t, rotated, snapshot)
}

func TestRevisionPolicyIPCResetAndNoOps(t *testing.T) {
	s, _, child, nft := revisionPolicyIPCFixture(t, false)
	response := revisionPolicyDo(t, s, http.MethodPost, "")
	require.Equal(t, http.StatusOK, response.Code)
	report := assertRevisionPolicyIPCIdentity(t, child, credentialvault.ActiveSnapshot{}, 1)
	response = revisionPolicyDo(t, s, http.MethodDelete, `["missing.example.com"]`)
	require.Equal(t, http.StatusOK, response.Code)
	s.alwaysLoader = &stagedTestAlwaysLoader{}
	changed, err := s.reloadAlwaysRules()
	require.NoError(t, err)
	require.False(t, changed)
	after := assertRevisionPolicyIPCIdentity(t, child, credentialvault.ActiveSnapshot{}, 1)
	require.Equal(t, report.Counts, after.Counts)
	require.Equal(t, 1, nft.calls)
}

func TestRevisionPolicyIPCLostCommitReplyHoldsPublicationBarrier(t *testing.T) {
	s, m, child, nft := revisionPolicyIPCFixture(t, true)
	old := s.revisionRecovery.current
	frozen, err := s.credentialVault.FreezeForPolicy(s.proxy.CurrentPolicy())
	require.NoError(t, err)
	require.True(t, child.exchange(t, map[string]string{"command": "fault", "mode": "drop-commit"}).Ready)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan int, 1)
	finished := make(chan struct{})
	t.Cleanup(func() { cancel(); <-finished })
	go func() {
		defer close(finished)
		done <- revisionPolicyDoContext(t, ctx, s, http.MethodPatch, `[{"action":"allow","target":"new.example.com"}]`).Code
	}()
	require.True(t, child.exchange(t, map[string]string{"command": "await-readback"}).Ready)
	beforeACK := assertRevisionPolicyIPCIdentity(t, child, frozen.Snapshot(), 1)
	require.Equal(t, 1, beforeACK.Counts.Dropped)
	// The child has already committed, but neither public Go state nor a
	// competing mutation may pass the owner's unconfirmed attempt.
	require.Same(t, old, s.revisionRecovery.current)
	require.Equal(t, policy.ActionDeny, s.proxy.CurrentPolicy().Evaluate("new.example.com"))
	for name, tryLock := range map[string]func() bool{"policy": s.mu.TryLock, "process": m.mu.TryLock} {
		locked := tryLock()
		if locked {
			if name == "policy" {
				s.mu.Unlock()
			} else {
				m.mu.Unlock()
			}
		}
		require.False(t, locked, "%s barrier released before exact acknowledgement", name)
	}
	require.True(t, child.exchange(t, map[string]string{"command": "release"}).Ready)
	require.Equal(t, http.StatusOK, <-done)
	after := assertRevisionPolicyIPCIdentity(t, child, frozen.Snapshot(), 1)
	require.Equal(t, 2, after.Counts.Prepare)
	require.Equal(t, 2, after.Counts.Commit)
	require.Equal(t, 1, nft.calls)
	require.Equal(t, int64(1), s.revisionRecovery.current.epoch)
	require.NoError(t, s.credentialVault.ValidatePolicySnapshot(frozen))
	require.Zero(t, child.stops)
}

func TestRevisionPolicyIPCDroppedPrepareUsesLocalEffectClassification(t *testing.T) {
	for _, withNft := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-network-effect", true: "committed-nft"}[withNft], func(t *testing.T) {
			s, m, child, nft := revisionPolicyIPCFixture(t, true)
			if !withNft {
				s.nft = nil
			}
			s.policyFile = filepath.Join(t.TempDir(), "policy.json")
			original := []byte("  {\"defaultAction\": \"deny\"} \n")
			require.NoError(t, os.WriteFile(s.policyFile, original, 0600))
			old := s.revisionRecovery.current
			before := child.exchange(t, map[string]string{"command": "inspect"})
			require.True(t, child.exchange(t, map[string]string{"command": "fault", "mode": "drop-prepare"}).Ready)
			var final revisionChildReport
			stop := m.revisionOwner.stop
			m.revisionOwner.stop = func(r *mitmproxy.Running) {
				final = child.exchange(t, map[string]string{"command": "inspect"})
				stop(r)
			}
			response := revisionPolicyDo(t, s, http.MethodPatch, `[{"action":"allow","target":"new.example.com"}]`)
			require.Equal(t, http.StatusServiceUnavailable, response.Code)
			require.Same(t, old, s.revisionRecovery.current)
			if !withNft {
				final = child.exchange(t, map[string]string{"command": "inspect"})
				actual, err := os.ReadFile(s.policyFile)
				require.NoError(t, err)
				require.Equal(t, original, actual)
				require.Zero(t, child.stops)
				require.False(t, s.revisionRecoveryRequired.Load())
				require.Equal(t, http.StatusOK, revisionPolicyDo(t, s, http.MethodPatch, `[{"action":"allow","target":"retry.example.com"}]`).Code)
			} else {
				require.True(t, s.revisionRecoveryRequired.Load())
				require.Equal(t, 1, nft.calls)
				require.Equal(t, 1, child.stops)
				require.NotNil(t, child.running.Cmd.ProcessState)
			}
			require.Equal(t, before.Active, final.Active)
			require.Equal(t, before.Counts.Commit, final.Counts.Commit)
			require.Equal(t, before.Counts.Prepare+1, final.Counts.Prepare)
			require.Equal(t, 1, final.Counts.Abort)
			require.Equal(t, 1, final.Counts.Dropped)
		})
	}
}

func TestRevisionPolicyIPCUnresolvedDeadlineStopsExactChildAndBlocksBootstrap(t *testing.T) {
	s, m, child, nft := revisionPolicyIPCFixture(t, true)
	oldBase, oldSession, oldRunning := s.revisionRecovery.current, m.revisionSession, m.running
	config, err := oldSession.MitmproxyConfig()
	require.NoError(t, err)
	_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
	require.NoError(t, err)
	require.True(t, child.exchange(t, map[string]string{"command": "fault", "mode": "unresolved"}).Ready)
	closedAfterReap := false
	oldSession.(*revisionObservedSession).beforeClose = func() { closedAfterReap = oldRunning.Cmd.ProcessState != nil }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan int, 1)
	finished := make(chan struct{})
	t.Cleanup(func() { cancel(); <-finished })
	go func() {
		defer close(finished)
		done <- revisionPolicyDoContext(t, ctx, s, http.MethodPatch, `[{"action":"allow","target":"new.example.com"}]`).Code
	}()
	require.True(t, child.exchange(t, map[string]string{"command": "await-readback"}).Ready)
	require.True(t, child.exchange(t, map[string]string{"command": "release"}).Ready)
	require.Equal(t, http.StatusServiceUnavailable, <-done)
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	require.Same(t, oldBase, s.revisionRecovery.current)
	require.Equal(t, 1, nft.calls)
	require.Equal(t, 1, nft.quiesces)
	require.True(t, closedAfterReap)
	require.Equal(t, 1, child.stops)
	require.Nil(t, m.running)
	require.Nil(t, m.revisionSession)
	require.NoDirExists(t, filepath.Dir(config.SocketPath))
	_, err = oldSession.MitmproxyConfig()
	require.ErrorIs(t, err, revision.ErrClosed)
	require.ErrorIs(t, validateRecoveryTicket(s, ticket), errRevisionRecoveryRequired)
	_, _, fresh, err := s.captureRevisionBootstrap(context.Background())
	require.ErrorIs(t, err, errRevisionRecoveryRequired)
	require.Nil(t, fresh)
	require.Equal(t, http.StatusServiceUnavailable, revisionPolicyDo(t, s, http.MethodDelete, `["missing.example.com"]`).Code)
	m.shutdown(time.Millisecond)
	require.Equal(t, 1, child.stops)
}
