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
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// transitionNft pauses only the targeted kernel operation; requests still use
// the real HTTP, registry, and vault handlers.
type transitionNft struct {
	*fakeNft
	remove func(subject.Subject) error
	deny   func(subject.Subject, actionhandler.NetworkAttachment) error
}

func (n *transitionNft) Remove(ctx context.Context, s subject.Subject) error {
	if n.remove != nil {
		if err := n.remove(s); err != nil {
			return err
		}
	}
	return n.fakeNft.Remove(ctx, s)
}
func (n *transitionNft) ApplyDenyFirst(ctx context.Context, s subject.Subject, a actionhandler.NetworkAttachment) error {
	if n.deny != nil {
		if err := n.deny(s, a); err != nil {
			return err
		}
	}
	return n.fakeNft.ApplyDenyFirst(ctx, s, a)
}
func transitionRequest(s *fastSandboxPolicyServer, ctx context.Context, method, path, body string) <-chan *httptest.ResponseRecorder {
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		result <- w
	}()
	return result
}
func transitionResult(t *testing.T, c <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case r := <-c:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP request blocked on subject transition")
		return nil
	}
}

func TestTransitionTakeoverRelayLockChain(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	victim := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
	entered, release := make(chan struct{}), make(chan struct{})
	srv.nft = &transitionNft{fakeNft: nft, remove: func(s subject.Subject) error {
		if s == victim {
			close(entered)
			<-release
		}
		return nil
	}}
	takeover := transitionRequest(srv, context.Background(), http.MethodPost, constants.ActionsDispatchPath, bindBody(t, "successor", testIP, "new-r", "new-a", 1, vaultAllowPolicy()))
	<-entered // callback owns registry write lock, with kernel removal paused
	relay := transitionRequest(srv, context.Background(), http.MethodGet, constants.VaultRelayPathPrefix+testUID, "")
	// A relay waiting behind kernel I/O must not pin the shared state lock.
	transitionWaitForMutex(t, "readRelayVault")
	unlocked := srv.mu.TryLock()
	if unlocked {
		srv.mu.Unlock()
	}
	assert.True(t, unlocked, "registry wait must not hold s.mu")
	close(release)
	require.Equal(t, http.StatusNotFound, transitionResult(t, relay).Code)
	require.Equal(t, http.StatusOK, transitionResult(t, takeover).Code)
	_, ok := reg.Fence(victim)
	require.False(t, ok)
}

func TestTransitionSameFenceNetworkChangeHasNoEffects(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, changeGateway := range []bool{false, true} {
			name := "denying"
			if active {
				name = "active"
			}
			if changeGateway {
				name += "/gateway"
			} else {
				name += "/ip"
			}
			t.Run(name, func(t *testing.T) {
				srv, reg, nft := fastSandboxTestServer(t)
				initial := bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())
				require.Equal(t, http.StatusOK, doAction(t, srv, initial).Code)
				if active {
					require.Equal(t, http.StatusOK, doAction(t, srv, hookBody(t, testUID, testFenceR, testFenceA, constants.HookDataPlaneReady)).Code)
				}
				denied, applied, removed := len(nft.denyFirst), len(nft.policyApplied), len(nft.removed)
				changed := bindBody(t, testUID, "10.0.0.6", testFenceR, testFenceA, 2, vaultAllowPolicy())
				if changeGateway {
					changed = strings.Replace(initial, `"gateway":"10.0.0.1"`, `"gateway":"10.0.0.2"`, 1)
				}
				require.Equal(t, http.StatusConflict, doAction(t, srv, changed).Code)
				assert.Len(t, nft.denyFirst, denied)
				assert.Len(t, nft.policyApplied, applied)
				assert.Len(t, nft.removed, removed)
				resolved, ok := reg.Resolve(subject.SubjectKey{SourceIP: testAttachment().Network.IP})
				require.True(t, ok)
				assert.Equal(t, subject.FromSandboxUID(testUID), resolved)
			})
		}
	}
}

func TestTransitionRebindRemoveExpectedNewBeforePublication(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	subj := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
	nft.removeErr = errors.New("injected removal failure")
	newBody := bindBody(t, testUID, "10.0.0.6", "new-r", "new-a", 2, vaultAllowPolicy())
	require.Equal(t, http.StatusInternalServerError, doAction(t, srv, newBody).Code)
	old, _ := reg.Fence(subj)
	require.Equal(t, testFenceR, old.RuntimeInstanceID)
	require.Equal(t, http.StatusOK, doAction(t, srv, removeBody(t, testUID, testFenceR, testFenceA)).Code)
	_, ok := reg.Fence(subj)
	require.True(t, ok, "old REMOVE must not cancel newer intent")
	nft.removeErr = nil
	require.Equal(t, http.StatusOK, doAction(t, srv, removeBody(t, testUID, "new-r", "new-a")).Code)
	_, ok = reg.Fence(subj)
	require.False(t, ok, "expected-new REMOVE cancels actual old residual registration")
}

func TestTransitionGateWaitCancellationAndValidation(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	subj := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
	entered, release := make(chan struct{}), make(chan struct{})
	srv.nft = &transitionNft{fakeNft: nft, remove: func(s subject.Subject) error {
		if s == subj {
			close(entered)
			<-release
		}
		return nil
	}}
	first := transitionRequest(srv, context.Background(), http.MethodPost, constants.ActionsDispatchPath, removeBody(t, testUID, testFenceR, testFenceA))
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	pending := transitionRequest(srv, ctx, http.MethodPost, constants.ActionsDispatchPath, bindBody(t, "waiting", "10.0.0.6", "r", "a", 1, vaultAllowPolicy()))
	cancel()
	require.Equal(t, http.StatusServiceUnavailable, transitionResult(t, pending).Code)
	malformed := "not a policy"
	invalid := transitionRequest(srv, context.Background(), http.MethodPost, constants.ActionsDispatchPath, bindBody(t, "invalid", "10.0.0.7", "r", "a", 1, &malformed))
	require.Equal(t, http.StatusBadRequest, transitionResult(t, invalid).Code, "effect-free validation must not queue behind kernel work")
	close(release)
	require.Equal(t, http.StatusOK, transitionResult(t, first).Code)
	_, ok := reg.Fence(subject.FromSandboxUID("waiting"))
	require.False(t, ok, "canceled waiter cannot acquire effects later")
	require.Equal(t, http.StatusOK, doAction(t, srv, bindBody(t, "next", "10.0.0.6", "r", "a", 1, vaultAllowPolicy())).Code, "canceled waiter must not leak gate ownership")
}

func transitionCurrentToken(s *fastSandboxPolicyServer, subj subject.Subject) *subjectRebind {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rebinds[subj]
}

func TestTransitionRebindWindowRejectsOldVault(t *testing.T) {
	h := newVaultPersistHarness(t)
	subj := h.readySubject(t, testUID, testIP)
	require.Equal(t, http.StatusCreated, doVault(t, h.srv, http.MethodPost, testUID, vaultCredential(), testFenceR).Code)
	entered, release := make(chan struct{}), make(chan struct{})
	h.srv.nft = &transitionNft{fakeNft: h.nft, remove: func(subject.Subject) error { close(entered); <-release; return nil }}
	result := transitionRequest(h.srv, context.Background(), http.MethodPost, constants.ActionsDispatchPath, bindBody(t, testUID, "10.0.0.6", "new-r", "new-a", 2, vaultAllowPolicy()))
	<-entered // token and purge are committed; registry still selects old fence
	require.NotNil(t, transitionCurrentToken(h.srv, subj))
	old, _ := h.reg.Fence(subj)
	require.Equal(t, testFenceR, old.RuntimeInstanceID)
	require.False(t, h.fileFor(testUID).exists)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		body := ""
		switch method {
		case http.MethodPost:
			body = vaultCredential()
		case http.MethodPut:
			body = vaultPut("1")
		case http.MethodDelete:
			body = `{"expectedRevision":1}`
		}
		rec := doVault(t, h.srv, method, testUID, body, testFenceR)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, method)
	}
	active := httptest.NewRecorder()
	h.srv.handleCredentialVaultActive(active, httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp="+testIP, nil))
	assert.Equal(t, http.StatusServiceUnavailable, active.Code)
	h.srv.mu.Lock()
	_, materialized := h.srv.vaults[subj]
	h.srv.mu.Unlock()
	assert.False(t, materialized, "rejected old relay must not recreate purged store")
	assert.False(t, h.fileFor(testUID).exists)
	close(release)
	require.Equal(t, http.StatusOK, transitionResult(t, result).Code)
	require.Nil(t, transitionCurrentToken(h.srv, subj))
}

func TestTransitionPartialPublicationRetryDoesNotPurgeNewGeneration(t *testing.T) {
	h := newVaultPersistHarness(t)
	subj := h.readySubject(t, testUID, testIP)
	require.Equal(t, http.StatusCreated, doVault(t, h.srv, http.MethodPost, testUID, vaultCredential(), testFenceR).Code)
	h.nft.denyFirstErr = errors.New("deny-first failed after registry publication")
	body := bindBody(t, testUID, "10.0.0.6", "new-r", "new-a", 2, vaultAllowPolicy())
	require.Equal(t, http.StatusInternalServerError, doAction(t, h.srv, body).Code)
	fence, _ := h.reg.Fence(subj)
	require.Equal(t, "new-r", fence.RuntimeInstanceID)
	token := transitionCurrentToken(h.srv, subj)
	require.NotNil(t, token)
	require.True(t, token.oldRemoved)
	require.True(t, token.purged)
	entry := h.srv.vaults[subj]
	require.Equal(t, fence, entry.fence)
	deletes, removes := h.fileFor(testUID).deleteCalls, len(h.nft.removed)
	require.Equal(t, http.StatusServiceUnavailable, doAction(t, h.srv, hookBody(t, testUID, "new-r", "new-a", constants.HookDataPlaneReady)).Code)
	require.Equal(t, http.StatusOK, doAction(t, h.srv, removeBody(t, testUID, testFenceR, testFenceA)).Code)
	require.Same(t, token, transitionCurrentToken(h.srv, subj))
	h.nft.denyFirstErr = nil
	require.Equal(t, http.StatusOK, doAction(t, h.srv, body).Code)
	require.Same(t, entry.store, h.srv.vaults[subj].store, "retry must not purge already rehydrated new-generation store")
	require.Equal(t, deletes, h.fileFor(testUID).deleteCalls)
	require.Len(t, h.nft.removed, removes, "retry must not remove already installed new network")
	require.NotNil(t, h.srv.pendingPolicy(subj))
	require.Nil(t, transitionCurrentToken(h.srv, subj))
	require.Equal(t, http.StatusOK, doAction(t, h.srv, hookBody(t, testUID, "new-r", "new-a", constants.HookDataPlaneReady)).Code)
}

func TestTransitionSupersedeResetFailureKeepsLatestIntent(t *testing.T) {
	for _, partialB := range []bool{false, true} {
		for _, finish := range []string{"retry", "remove"} {
			name := "before-publication/" + finish
			if partialB {
				name = "after-publication/" + finish
			}
			t.Run(name, func(t *testing.T) {
				h := newVaultPersistHarness(t)
				subj := h.readySubject(t, testUID, testIP)
				if partialB {
					h.nft.denyFirstErr = errors.New("B enforcement failed")
				} else {
					h.nft.removeErr = errors.New("B old-network removal failed")
				}
				require.Equal(t, http.StatusInternalServerError, doAction(t, h.srv, bindBody(t, testUID, "10.0.0.6", "B", "B-att", 2, vaultAllowPolicy())).Code)
				tokenB := transitionCurrentToken(h.srv, subj)
				require.NotNil(t, tokenB)
				actual, _ := h.reg.Fence(subj)
				h.nft.denyFirstErr = nil
				h.nft.removeErr = errors.New("C full reset failed")
				deletes := h.fileFor(testUID).deleteCalls
				cBody := bindBody(t, testUID, "10.0.0.7", "C", "C-att", 3, vaultAllowPolicy())
				require.Equal(t, http.StatusInternalServerError, doAction(t, h.srv, cBody).Code)
				tokenC := transitionCurrentToken(h.srv, subj)
				require.NotSame(t, tokenB, tokenC)
				require.Equal(t, "C", tokenC.expectedNew.RuntimeInstanceID)
				require.True(t, tokenC.needsReset)
				remaining, _ := h.reg.Fence(subj)
				require.Equal(t, actual, remaining, "failed reset retains actual registry generation")
				require.Equal(t, deletes, h.fileFor(testUID).deleteCalls, "failed reset cannot claim local cleanup complete")
				for _, f := range []struct{ r, a string }{{testFenceR, testFenceA}, {"B", "B-att"}} {
					require.Equal(t, http.StatusOK, doAction(t, h.srv, removeBody(t, testUID, f.r, f.a)).Code)
					require.Same(t, tokenC, transitionCurrentToken(h.srv, subj))
				}
				require.Equal(t, http.StatusInternalServerError, doAction(t, h.srv, removeBody(t, testUID, "C", "C-att")).Code)
				require.Same(t, tokenC, transitionCurrentToken(h.srv, subj), "failed expected-new cleanup retains current intent")
				h.nft.removeErr = nil
				if finish == "retry" {
					require.Equal(t, http.StatusOK, doAction(t, h.srv, cBody).Code)
					f, ok := h.reg.Fence(subj)
					require.True(t, ok)
					require.Equal(t, "C", f.RuntimeInstanceID)
					require.NotNil(t, h.srv.pendingPolicy(subj))
				} else {
					require.Equal(t, http.StatusOK, doAction(t, h.srv, removeBody(t, testUID, "C", "C-att")).Code)
					_, ok := h.reg.Fence(subj)
					require.False(t, ok)
				}
				require.Nil(t, transitionCurrentToken(h.srv, subj))
			})
		}
	}
}

func TestTransitionSupersedePartialCUsesFreshStages(t *testing.T) {
	for _, partialB := range []bool{false, true} {
		name := "before-B-publication"
		if partialB {
			name = "after-B-publication"
		}
		t.Run(name, func(t *testing.T) {
			h := newVaultPersistHarness(t)
			subj := h.readySubject(t, testUID, testIP)
			if partialB {
				h.nft.denyFirstErr = errors.New("B enforcement failure")
			} else {
				h.nft.removeErr = errors.New("B removal failure")
			}
			require.Equal(t, http.StatusInternalServerError, doAction(t, h.srv, bindBody(t, testUID, "10.0.0.6", "B", "B-att", 2, vaultAllowPolicy())).Code)
			tokenB := transitionCurrentToken(h.srv, subj)
			h.nft.removeErr = nil
			h.nft.denyFirstErr = errors.New("C enforcement failure")
			body := bindBody(t, testUID, "10.0.0.7", "C", "C-att", 3, vaultAllowPolicy())
			require.Equal(t, http.StatusInternalServerError, doAction(t, h.srv, body).Code)
			tokenC := transitionCurrentToken(h.srv, subj)
			require.NotSame(t, tokenB, tokenC)
			require.False(t, tokenC.needsReset)
			require.True(t, tokenC.purged)
			f, ok := h.reg.Fence(subj)
			require.True(t, ok)
			require.Equal(t, "C", f.RuntimeInstanceID)
			entry := h.srv.vaults[subj]
			require.Equal(t, f, entry.fence)
			removes, deletes := len(h.nft.removed), h.fileFor(testUID).deleteCalls
			h.nft.denyFirstErr = nil
			require.Equal(t, http.StatusOK, doAction(t, h.srv, body).Code)
			require.Len(t, h.nft.removed, removes)
			require.Equal(t, deletes, h.fileFor(testUID).deleteCalls)
			require.Same(t, entry.store, h.srv.vaults[subj].store)
			require.Nil(t, transitionCurrentToken(h.srv, subj))
		})
	}
}

func TestTransitionCanceledSupersedeDoesNotReplaceIntent(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	subj := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
	nft.removeErr = errors.New("B cleanup failure")
	require.Equal(t, http.StatusInternalServerError, doAction(t, srv, bindBody(t, testUID, "10.0.0.6", "B", "B-att", 2, vaultAllowPolicy())).Code)
	token := transitionCurrentToken(srv, subj)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := transitionRequest(srv, ctx, http.MethodPost, constants.ActionsDispatchPath, bindBody(t, testUID, "10.0.0.7", "C", "C-att", 3, vaultAllowPolicy()))
	require.Equal(t, http.StatusServiceUnavailable, transitionResult(t, c).Code)
	require.Same(t, token, transitionCurrentToken(srv, subj))
	// Invalid new intent is also rejected before any token/effect changes.
	bad := "malformed policy"
	require.Equal(t, http.StatusBadRequest, doAction(t, srv, bindBody(t, testUID, "10.0.0.7", "C", "C-att", 3, &bad)).Code)
	require.Same(t, token, transitionCurrentToken(srv, subj))
}

func TestTransitionFailedFirstInstallRecordsNetwork(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	nft.denyFirstErr = errors.New("first deny-first failed")
	body := bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy())
	require.Equal(t, http.StatusInternalServerError, doAction(t, srv, body).Code)
	subj := subject.FromSandboxUID(testUID)
	att, ok := srv.attachment(subj)
	require.True(t, ok)
	require.Equal(t, testAttachment().Network, att)
	require.Equal(t, http.StatusConflict, doAction(t, srv, bindBody(t, testUID, "10.0.0.6", testFenceR, testFenceA, 2, vaultAllowPolicy())).Code)
	nft.denyFirstErr = nil
	require.Equal(t, http.StatusOK, doAction(t, srv, body).Code, "valid same-fence retry is not rejected for missing successful metadata")
	require.Empty(t, nft.removed)
	// Simulate an older/unknown registration without reliable attempt metadata.
	srv.mu.Lock()
	delete(srv.subjAtt, subj)
	srv.mu.Unlock()
	require.Equal(t, http.StatusOK, doAction(t, srv, bindBody(t, testUID, "10.0.0.6", "B", "B-att", 3, vaultAllowPolicy())).Code)
	require.Equal(t, []subject.Subject{subj}, nft.removed, "unknown old network requires conservative removal")
	f, _ := reg.Fence(subj)
	require.Equal(t, "B", f.RuntimeInstanceID)
}

func TestTransitionTakeoverPurgesOnlyActualEviction(t *testing.T) {
	for _, victimFailure := range []bool{false, true} {
		name := "successor-fails"
		if victimFailure {
			name = "victim-fails"
		}
		t.Run(name, func(t *testing.T) {
			h := newVaultPersistHarness(t)
			victim := h.readySubject(t, testUID, testIP)
			require.Equal(t, http.StatusCreated, doVault(t, h.srv, http.MethodPost, testUID, vaultCredential(), testFenceR).Code)
			old := h.srv.vaults[victim]
			if victimFailure {
				h.nft.removeErr = errors.New("victim teardown failed")
			} else {
				h.nft.denyFirstErr = errors.New("successor install failed")
			}
			require.Equal(t, http.StatusInternalServerError, doAction(t, h.srv, bindBody(t, "successor", testIP, "new-r", "new-a", 1, vaultAllowPolicy())).Code)
			_, registered := h.reg.Fence(victim)
			if victimFailure {
				require.True(t, registered)
				require.True(t, h.fileFor(testUID).exists)
				require.Same(t, old.store, h.srv.vaults[victim].store)
			} else {
				require.False(t, registered)
				require.False(t, h.fileFor(testUID).exists)
				require.NotContains(t, h.srv.vaults, victim)
				require.NotContains(t, h.srv.subjAtt, victim)
			}
		})
	}
}

func TestTransitionRemoveWaitsForStartedRelayThenPurges(t *testing.T) {
	h := newVaultPersistHarness(t)
	subj := h.readySubject(t, testUID, testIP)
	entered, release := make(chan struct{}), make(chan struct{})
	h.srv.nft = &transitionNft{fakeNft: h.nft, remove: func(subject.Subject) error { close(entered); <-release; return nil }}
	removed := transitionRequest(h.srv, context.Background(), http.MethodPost, constants.ActionsDispatchPath, removeBody(t, testUID, testFenceR, testFenceA))
	<-entered
	require.Equal(t, http.StatusCreated, doVault(t, h.srv, http.MethodPost, testUID, vaultCredential(), testFenceR).Code, "existing identity permits a write already entering before Unregister")
	require.True(t, h.fileFor(testUID).exists)
	close(release)
	require.Equal(t, http.StatusOK, transitionResult(t, removed).Code)
	require.False(t, h.fileFor(testUID).exists)
	require.NotContains(t, h.srv.vaults, subj)
	require.Equal(t, http.StatusNotFound, doVault(t, h.srv, http.MethodGet, testUID, "", "").Code)
}

func TestTransitionGateServerCancellation(t *testing.T) {
	srv, _, _ := fastSandboxTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	srv.ctx = ctx
	// A held gate represents any in-flight SET/HOOK/REMOVE kernel operation.
	srv.actionGate <- struct{}{}
	result := transitionRequest(srv, context.Background(), http.MethodPost, constants.ActionsDispatchPath, removeBody(t, testUID, testFenceR, testFenceA))
	cancel()
	require.Equal(t, http.StatusServiceUnavailable, transitionResult(t, result).Code)
	<-srv.actionGate
	require.Empty(t, srv.rebinds)
}

func TestTransitionNullBindingIdentityAndPolicyAreSeparate(t *testing.T) {
	t.Run("same-fence-omitted-attachment", func(t *testing.T) {
		srv, reg, nft := fastSandboxTestServer(t)
		subj := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
		var envelope actionhandler.Envelope
		require.NoError(t, json.Unmarshal([]byte(bindBody(t, testUID, testIP, testFenceR, testFenceA, 2, nil)), &envelope))
		envelope.Attachment = nil
		body, err := json.Marshal(envelope)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, doAction(t, srv, string(body)).Code)
		require.Nil(t, transitionCurrentToken(srv, subj))
		att, ok := srv.attachment(subj)
		require.True(t, ok)
		require.Equal(t, testAttachment().Network, att)
		require.Empty(t, nft.removed)
		require.Equal(t, subject.StateDenying, mustState(reg.Get(subj)))
		require.Nil(t, srv.pendingPolicy(subj))
	})
	t.Run("new-fence-still-rebinds", func(t *testing.T) {
		srv, reg, nft := fastSandboxTestServer(t)
		subj := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
		nft.removeErr = errors.New("new-fence null binding old-IP cleanup failed")
		require.Equal(t, http.StatusInternalServerError, doAction(t, srv, bindBody(t, testUID, "10.0.0.6", "B", "B-att", 2, nil)).Code)
		token := transitionCurrentToken(srv, subj)
		require.NotNil(t, token)
		require.Equal(t, "B", token.expectedNew.RuntimeInstanceID)
		require.Equal(t, http.StatusOK, doAction(t, srv, removeBody(t, testUID, testFenceR, testFenceA)).Code)
		require.Same(t, token, transitionCurrentToken(srv, subj))
		nft.removeErr = nil
		require.Equal(t, http.StatusOK, doAction(t, srv, removeBody(t, testUID, "B", "B-att")).Code)
		_, ok := reg.Fence(subj)
		require.False(t, ok)
		require.Nil(t, transitionCurrentToken(srv, subj))
	})
}

func TestTransitionExpectedNewRemoveCleansAbsentRegistryResidual(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	subj := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
	victim := setBindingAndReady(t, srv, reg, "victim", "10.0.0.6", vaultAllowPolicy())
	srv.nft = &transitionNft{fakeNft: nft, remove: func(s subject.Subject) error {
		if s == victim {
			return errors.New("claimed IP victim teardown failed")
		}
		return nil
	}}
	require.Equal(t, http.StatusInternalServerError, doAction(t, srv, bindBody(t, testUID, "10.0.0.6", "B", "B-att", 2, vaultAllowPolicy())).Code)
	_, exists := reg.Fence(subj)
	require.False(t, exists, "registry drops old same-UID entry before conflicting-key takeover")
	require.NotNil(t, transitionCurrentToken(srv, subj))
	require.Equal(t, http.StatusOK, doAction(t, srv, removeBody(t, testUID, "B", "B-att")).Code)
	require.Nil(t, transitionCurrentToken(srv, subj))
	_, exists = reg.Fence(victim)
	require.True(t, exists, "failed victim must retain its own identity")
	require.Equal(t, []subject.Subject{subj, subj}, nft.removed, "terminal intent cleanup cannot assume absent registry means no kernel effects")
}

func TestTransitionSupersedeKeepsGuardThroughNewRegistration(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	subj := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
	nft.denyFirstErr = errors.New("B partial registration")
	require.Equal(t, http.StatusInternalServerError, doAction(t, srv, bindBody(t, testUID, "10.0.0.6", "B", "B-att", 2, vaultAllowPolicy())).Code)
	tokenB := transitionCurrentToken(srv, subj)
	nft.denyFirstErr = nil
	entered, release := make(chan struct{}), make(chan struct{})
	srv.nft = &transitionNft{fakeNft: nft, deny: func(subject.Subject, actionhandler.NetworkAttachment) error { close(entered); <-release; return nil }}
	pending := transitionRequest(srv, context.Background(), http.MethodPost, constants.ActionsDispatchPath, bindBody(t, testUID, "10.0.0.7", "C", "C-att", 3, vaultAllowPolicy()))
	<-entered // full reset and Unregister/purge happened; C fence is publishing
	tokenC := transitionCurrentToken(srv, subj)
	require.NotSame(t, tokenB, tokenC)
	require.Equal(t, "C", tokenC.expectedNew.RuntimeInstanceID)
	relay := transitionRequest(srv, context.Background(), http.MethodGet, constants.VaultRelayPathPrefix+testUID, "")
	require.Equal(t, http.StatusServiceUnavailable, transitionResult(t, relay).Code, "purge must preserve C token even under registry write lock")
	close(release)
	require.Equal(t, http.StatusOK, transitionResult(t, pending).Code)
	require.Nil(t, transitionCurrentToken(srv, subj))
}

func TestTransitionTakeoverClearsVictimRebindToken(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	subj := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
	nft.removeErr = errors.New("pending B removal failure")
	require.Equal(t, http.StatusInternalServerError, doAction(t, srv, bindBody(t, testUID, "10.0.0.6", "B", "B-att", 2, vaultAllowPolicy())).Code)
	require.NotNil(t, transitionCurrentToken(srv, subj))
	nft.removeErr = nil
	require.Equal(t, http.StatusOK, doAction(t, srv, bindBody(t, "successor", testIP, "C", "C-att", 1, vaultAllowPolicy())).Code)
	require.Nil(t, transitionCurrentToken(srv, subj))
	_, exists := reg.Fence(subj)
	require.False(t, exists)
}

// This bounded contention experiment measures handler wait/cancel behavior,
// not a production latency SLO. Its sample count is intentionally not used
// to report a p99, and canceled terminal requests retain their live subjects.
func TestTransitionRemoveGateContentionMeasurements(t *testing.T) {
	const n = 32
	srv, reg, nft := fastSandboxTestServer(t)
	blocker := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
	for i := 0; i < n; i++ {
		uid, ip := fmt.Sprintf("queued-%d", i), fmt.Sprintf("10.0.1.%d", i+1)
		require.Equal(t, http.StatusOK, doAction(t, srv, bindBody(t, uid, ip, "r", "a", 1, vaultAllowPolicy())).Code)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	srv.nft = &transitionNft{fakeNft: nft, remove: func(s subject.Subject) error {
		if s == blocker {
			close(entered)
			<-release
		}
		return nil
	}}
	first := transitionRequest(srv, context.Background(), http.MethodPost, constants.ActionsDispatchPath, removeBody(t, testUID, testFenceR, testFenceA))
	<-entered
	type observation struct {
		status  int
		elapsed time.Duration
	}
	results := make([]chan observation, n)
	cancels := make([]context.CancelFunc, n)
	for i := 0; i < n; i++ {
		timeout := 2 * time.Second
		if i%2 == 0 {
			timeout = 15 * time.Millisecond
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		cancels[i] = cancel
		results[i] = make(chan observation, 1)
		body := removeBody(t, fmt.Sprintf("queued-%d", i), "r", "a")
		go func(c chan observation, ctx context.Context, body string) {
			start := time.Now()
			r := httptest.NewRequest(http.MethodPost, constants.ActionsDispatchPath, strings.NewReader(body)).WithContext(ctx)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			c <- observation{w.Code, time.Since(start)}
		}(results[i], ctx, body)
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	var canceledTotal, completedTotal time.Duration
	for i := 0; i < n; i += 2 {
		select {
		case got := <-results[i]:
			require.Equal(t, http.StatusServiceUnavailable, got.status)
			canceledTotal += got.elapsed
		case <-time.After(2 * time.Second):
			t.Fatal("canceled terminal request remained queued")
		}
	}
	close(release)
	require.Equal(t, http.StatusOK, transitionResult(t, first).Code)
	for i := 1; i < n; i += 2 {
		select {
		case got := <-results[i]:
			require.Equal(t, http.StatusOK, got.status)
			completedTotal += got.elapsed
		case <-time.After(2 * time.Second):
			t.Fatal("live terminal request did not resume")
		}
	}
	residual := 0
	for i := 0; i < n; i++ {
		_, ok := reg.Fence(subject.FromSandboxUID(fmt.Sprintf("queued-%d", i)))
		require.Equal(t, i%2 == 0, ok)
		if ok {
			residual++
		}
	}
	t.Logf("controlled contention: REMOVE samples=%d, timed out=%d/%d, residual subjects=%d, canceled mean=%s, completed mean=%s; no production p99/SLO claim", n, n/2, n, residual, canceledTotal/(n/2), completedTotal/(n/2))
}

func TestTransitionCanceledWhileWaitingForVaultCannotSupersede(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	subj := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
	nft.removeErr = errors.New("B cleanup failed")
	require.Equal(t, http.StatusInternalServerError, doAction(t, srv, bindBody(t, testUID, "10.0.0.6", "B", "B-att", 2, vaultAllowPolicy())).Code)
	token := transitionCurrentToken(srv, subj)
	srv.mu.Lock() // another Vault transaction owns the lock before token setup
	ctx, cancel := context.WithCancel(context.Background())
	pending := transitionRequest(srv, ctx, http.MethodPost, constants.ActionsDispatchPath, bindBody(t, testUID, "10.0.0.7", "C", "C-att", 3, vaultAllowPolicy()))
	deadline := time.Now().Add(time.Second)
	for len(srv.actionGate) == 0 {
		if time.Now().After(deadline) {
			srv.mu.Unlock()
			cancel()
			t.Fatal("SET did not acquire action gate")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	srv.mu.Unlock()
	require.Equal(t, http.StatusServiceUnavailable, transitionResult(t, pending).Code)
	require.Same(t, token, transitionCurrentToken(srv, subj))
}

func TestTransitionSameFenceNetworkCheckUsesPurgedAttemptToken(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	subj := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
	nft.removeErr = errors.New("B cleanup failed after local purge")
	require.Equal(t, http.StatusInternalServerError, doAction(t, srv, bindBody(t, testUID, "10.0.0.6", "B", "B-att", 2, vaultAllowPolicy())).Code)
	token := transitionCurrentToken(srv, subj)
	_, known := srv.attachment(subj)
	require.False(t, known)
	require.Equal(t, http.StatusConflict, doAction(t, srv, bindBody(t, testUID, "10.0.0.7", testFenceR, testFenceA, 3, vaultAllowPolicy())).Code)
	require.Same(t, token, transitionCurrentToken(srv, subj), "same actual fence with changed network is rejected before superseding intent")
}

func TestTransitionActiveUnknownNetworkIsEffectFreeInternalError(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	subj := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
	oldPolicy := reg.UserPolicy(subj)
	oldFence, _ := reg.Fence(subj)
	denied, applied, removed := len(nft.denyFirst), len(nft.policyApplied), len(nft.removed)
	// Production paths cannot produce this active, token-free combination:
	// recordBindingState precedes registration, and removal purges after
	// unregistering. Corrupt it explicitly to test the defensive boundary.
	srv.mu.Lock()
	delete(srv.subjAtt, subj)
	srv.mu.Unlock()
	body := bindBody(t, testUID, testIP, testFenceR, testFenceA, 2, vaultAllowPolicy())
	require.Equal(t, http.StatusInternalServerError, doAction(t, srv, body).Code)
	require.Len(t, nft.denyFirst, denied)
	require.Len(t, nft.policyApplied, applied)
	require.Len(t, nft.removed, removed)
	require.Same(t, oldPolicy, reg.UserPolicy(subj))
	f, _ := reg.Fence(subj)
	require.Equal(t, oldFence, f)
	require.Equal(t, subject.StateActive, mustState(reg.Get(subj)))
	require.Nil(t, transitionCurrentToken(srv, subj))
}

func TestTransitionInitiallyNullUnknownNetworkCanReceiveBinding(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	var env actionhandler.Envelope
	require.NoError(t, json.Unmarshal([]byte(bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, nil)), &env))
	env.Attachment = nil
	body, err := json.Marshal(env)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, doAction(t, srv, string(body)).Code)
	require.Equal(t, http.StatusOK, doAction(t, srv, bindBody(t, testUID, testIP, testFenceR, testFenceA, 2, vaultAllowPolicy())).Code)
	require.Equal(t, http.StatusOK, doAction(t, srv, hookBody(t, testUID, testFenceR, testFenceA, constants.HookDataPlaneReady)).Code)
	require.Equal(t, subject.StateActive, mustState(reg.Get(subject.FromSandboxUID(testUID))))
}

func omittedNullBinding(t *testing.T, runtime, attachment string) string {
	t.Helper()
	var env actionhandler.Envelope
	require.NoError(t, json.Unmarshal([]byte(bindBody(t, testUID, testIP, runtime, attachment, 3, nil)), &env))
	env.Attachment = nil
	body, err := json.Marshal(env)
	require.NoError(t, err)
	return string(body)
}

func TestTransitionNullOmissionRejectsUnprovenNewNetwork(t *testing.T) {
	for _, stage := range []string{"registered", "pending-before-publication", "pending-after-publication", "pending-absent-registry"} {
		t.Run(stage, func(t *testing.T) {
			h := newVaultPersistHarness(t)
			subj := h.readySubject(t, testUID, testIP)
			require.Equal(t, http.StatusCreated, doVault(t, h.srv, http.MethodPost, testUID, vaultCredential(), testFenceR).Code)
			if stage != "registered" {
				if stage == "pending-before-publication" {
					h.nft.removeErr = errors.New("blocked remove")
				} else {
					h.nft.denyFirstErr = errors.New("blocked install")
				}
				require.Equal(t, http.StatusInternalServerError, doAction(t, h.srv, bindBody(t, testUID, "10.0.0.6", "B", "B-att", 2, nil)).Code)
				if stage == "pending-absent-registry" {
					h.reg.Unregister(subj)
				}
			}
			token := transitionCurrentToken(h.srv, subj)
			fence, registered := h.reg.Fence(subj)
			att, known := h.srv.attachment(subj)
			store := h.srv.vaults[subj]
			purges := h.fileFor(testUID).deleteCalls
			// Trap attempted effects even when the prior fake failed before recording them.
			h.srv.nft = &transitionNft{fakeNft: h.nft, remove: func(subject.Subject) error { t.Error("unexpected Remove"); return nil }, deny: func(subject.Subject, actionhandler.NetworkAttachment) error {
				t.Error("unexpected install")
				return nil
			}}
			require.Equal(t, http.StatusBadRequest, doAction(t, h.srv, omittedNullBinding(t, "C", "C-att")).Code)
			if token != nil {
				// Old A is also a superseding intent, even before B publication
				// while the registry still contains A and its known network.
				require.Equal(t, http.StatusBadRequest, doAction(t, h.srv, omittedNullBinding(t, testFenceR, testFenceA)).Code)
			}
			if token == nil {
				require.Nil(t, transitionCurrentToken(h.srv, subj))
			} else {
				require.Same(t, token, transitionCurrentToken(h.srv, subj))
			}
			gotFence, gotRegistered := h.reg.Fence(subj)
			require.Equal(t, registered, gotRegistered)
			require.Equal(t, fence, gotFence)
			gotAtt, gotKnown := h.srv.attachment(subj)
			require.Equal(t, known, gotKnown)
			require.Equal(t, att, gotAtt)
			require.Equal(t, store, h.srv.vaults[subj])
			require.Equal(t, purges, h.fileFor(testUID).deleteCalls)
		})
	}
}

func TestTransitionNullOmissionRetriesOnlyExpectedNetwork(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("published=%v", partial), func(t *testing.T) {
			srv, reg, nft := fastSandboxTestServer(t)
			subj := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
			if partial {
				nft.denyFirstErr = errors.New("install failed")
			} else {
				nft.removeErr = errors.New("remove failed")
			}
			require.Equal(t, http.StatusInternalServerError, doAction(t, srv, bindBody(t, testUID, "10.0.0.6", "B", "B-att", 2, nil)).Code)
			token := transitionCurrentToken(srv, subj)
			require.Equal(t, "10.0.0.6", token.newNetwork.IP.String())
			nft.denyFirstErr = nil
			nft.removeErr = nil
			require.Equal(t, http.StatusOK, doAction(t, srv, omittedNullBinding(t, "B", "B-att")).Code)
			att, _ := srv.attachment(subj)
			require.Equal(t, token.newNetwork, att)
			owner, found := reg.Resolve(subject.SubjectKey{SourceIP: att.IP})
			require.True(t, found)
			require.Equal(t, subj, owner)
			_, found = reg.Resolve(subject.SubjectKey{SourceIP: netip.MustParseAddr(testIP)})
			require.False(t, found)
			require.Nil(t, transitionCurrentToken(srv, subj))
		})
	}
}

func TestTransitionNullOmissionCannotFallbackFromInvalidExpectedNetwork(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	subj := setBindingAndReady(t, srv, reg, testUID, testIP, vaultAllowPolicy())
	fence, _ := reg.Fence(subj)
	// Defensive inconsistent token: even a matching registered fence and known
	// attachment cannot substitute for this intent's missing attempted network.
	token := &subjectRebind{expectedNew: fence}
	srv.rebinds[subj] = token
	before := len(nft.denyFirst)
	require.Equal(t, http.StatusBadRequest, doAction(t, srv, omittedNullBinding(t, testFenceR, testFenceA)).Code)
	require.Same(t, token, transitionCurrentToken(srv, subj))
	require.Len(t, nft.denyFirst, before)
	require.Empty(t, nft.removed)
}

func TestTransitionInitialNullOmissionRemainsReplayable(t *testing.T) {
	srv, _, _ := fastSandboxTestServer(t)
	for i := 0; i < 2; i++ {
		require.Equal(t, http.StatusOK, doAction(t, srv, omittedNullBinding(t, testFenceR, testFenceA)).Code)
	}
}
