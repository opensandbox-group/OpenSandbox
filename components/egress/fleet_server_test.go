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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/iptables"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// fakeNft implements fleetNftApplier with recording.
type fakeNft struct {
	mu            sync.Mutex
	denyFirst     []subject.Subject
	policyApplied []subject.Subject
	lastPolicy    *policy.NetworkPolicy
	removed       []subject.Subject
	denyFirstErr  error
	policyErr     error
	removeErr     error
}

func (f *fakeNft) ApplyDenyFirst(_ context.Context, s subject.Subject, _ actionhandler.NetworkAttachment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.denyFirstErr != nil {
		return f.denyFirstErr
	}
	f.denyFirst = append(f.denyFirst, s)
	return nil
}

func (f *fakeNft) ApplyPolicy(_ context.Context, s subject.Subject, pol *policy.NetworkPolicy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.policyErr != nil {
		return f.policyErr
	}
	f.policyApplied = append(f.policyApplied, s)
	f.lastPolicy = pol
	return nil
}

func (f *fakeNft) Remove(_ context.Context, s subject.Subject) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, s)
	return nil
}

func (f *fakeNft) appliedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.policyApplied)
}

func fleetTestServer(t *testing.T) (*fleetPolicyServer, *subject.MemoryRegistry, *fakeNft) {
	t.Helper()
	reg := subject.NewRegistry(nil, nil)
	nft := &fakeNft{}
	srv := newFleetPolicyServer(context.Background(), reg, nft, time.Minute)
	// no-op the gateway DNS redirect (no iptables/nft in unit tests)
	srv.dnsRedirectInstall = func(netip.Addr, int) error { return nil }
	srv.dnsRedirectRemove = func() error { return nil }
	return srv, reg, nft
}

func uidHeader(s subject.Subject) string {
	return strings.TrimPrefix(string(s), "s-")
}

func doRequest(t *testing.T, srv *fleetPolicyServer, method, path, uid, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body == "" {
		rd = bytes.NewReader(nil)
	} else {
		rd = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, rd)
	if uid != "" {
		req.Header.Set(constants.EgressSubjectUIDHeader, uid)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// doAction posts an action envelope to the Fastlet dispatch endpoint.
func doAction(t *testing.T, srv *fleetPolicyServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, srv, http.MethodPost, constants.ActionsDispatchPath, "", body)
}

// ---------------------------------------------------------------------------
// Envelope builders (wire format via the actionhandler types)
// ---------------------------------------------------------------------------

const (
	testUID    = "u-1"
	testIP     = "10.0.0.5"
	testGW     = "10.0.0.1"
	testFenceR = "runtime-1"
	testFenceA = "attachment-1"
)

func testAttachment() *actionhandler.Attachment {
	return &actionhandler.Attachment{Network: actionhandler.NetworkAttachment{
		IP:          netip.MustParseAddr(testIP),
		Gateway:     netip.MustParseAddr(testGW),
		PrivateCIDR: netip.MustParsePrefix("10.0.0.0/24"),
		HostVeth:    "veth0",
	}}
}

// bindBody builds a SET_BINDING envelope; input nil = JSON-null removal.
func bindBody(t *testing.T, uid, ip, runtimeID, attID string, specGen uint64, input *string) string {
	t.Helper()
	env := actionhandler.Envelope{
		APIVersion:   constants.ActionsAPIVersion,
		Operation:    actionhandler.OperationSetBinding,
		InvocationID: fmt.Sprintf("inv-%s-%d", uid, specGen),
		Sandbox:      actionhandler.SandboxRef{UID: uid, Name: "sb", Namespace: "ns"},
		Revision: actionhandler.Revision{
			SpecGeneration:    specGen,
			RuntimeInstanceID: runtimeID,
			AttachmentID:      attID,
			RouteGeneration:   1,
		},
		Attachment: testAttachment(),
		Binding:    &actionhandler.Binding{},
	}
	if ip != testIP {
		env.Attachment.Network.IP = netip.MustParseAddr(ip)
	}
	if input != nil {
		quoted, err := json.Marshal(*input)
		require.NoError(t, err)
		env.Binding.Input = json.RawMessage(quoted)
	} else {
		env.Binding.Input = json.RawMessage("null")
	}
	raw, err := json.Marshal(env)
	require.NoError(t, err)
	return string(raw)
}

func hookBody(t *testing.T, uid, runtimeID, attID, name string) string {
	t.Helper()
	env := actionhandler.Envelope{
		APIVersion:   constants.ActionsAPIVersion,
		Operation:    actionhandler.OperationLifecycleHook,
		InvocationID: fmt.Sprintf("inv-%s-hook", uid),
		Sandbox:      actionhandler.SandboxRef{UID: uid, Name: "sb", Namespace: "ns"},
		Revision: actionhandler.Revision{
			SpecGeneration:    1,
			RuntimeInstanceID: runtimeID,
			AttachmentID:      attID,
			RouteGeneration:   1,
		},
		Hook: &actionhandler.Hook{Name: name, Sequence: 1},
	}
	raw, err := json.Marshal(env)
	require.NoError(t, err)
	return string(raw)
}

func removeBody(t *testing.T, uid, runtimeID, attID string) string {
	t.Helper()
	env := actionhandler.Envelope{
		APIVersion:   constants.ActionsAPIVersion,
		Operation:    actionhandler.OperationRemoveBinding,
		InvocationID: fmt.Sprintf("inv-%s-remove", uid),
		Sandbox:      actionhandler.SandboxRef{UID: uid, Name: "sb", Namespace: "ns"},
		Revision: actionhandler.Revision{
			SpecGeneration:    1,
			RuntimeInstanceID: runtimeID,
			AttachmentID:      attID,
			RouteGeneration:   1,
		},
	}
	raw, err := json.Marshal(env)
	require.NoError(t, err)
	return string(raw)
}

// setBindingAndReady drives a sandbox to active through the action protocol:
// SET_BINDING -> runtime-ready -> data-plane-ready.
func setBindingAndReady(t *testing.T, srv *fleetPolicyServer, reg *subject.MemoryRegistry, uid, ip string, input *string) subject.Subject {
	t.Helper()
	s := subject.FromSandboxUID(uid)
	rec := doAction(t, srv, bindBody(t, uid, ip, testFenceR, testFenceA, 1, input))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, uid, testFenceR, testFenceA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, uid, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	return s
}

// ---------------------------------------------------------------------------
// Status endpoint
// ---------------------------------------------------------------------------

func TestActionsStatus(t *testing.T) {
	srv, _, _ := fleetTestServer(t)
	rec := doRequest(t, srv, http.MethodGet, constants.ActionsStatusPath, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var status actionhandler.StatusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &status))
	assert.Equal(t, constants.ActionsAPIVersion, status.APIVersion)
	assert.True(t, status.Ready)
	assert.NotEmpty(t, status.InstanceID)

	// instanceId is stable for the process incarnation
	rec = doRequest(t, srv, http.MethodGet, constants.ActionsStatusPath, "", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &status))
	assert.Equal(t, srv.instanceID, status.InstanceID)
}

func TestActionsStatusMirrorsMitmGate(t *testing.T) {
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	srv, _, _ := fleetTestServer(t)
	gate := mitmproxy.NewHealthGate()
	srv.mitmGate = gate
	gate.SetReady(false)

	rec := doRequest(t, srv, http.MethodGet, constants.ActionsStatusPath, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var status actionhandler.StatusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &status))
	assert.False(t, status.Ready, "handler not ready while the MITM stack is pending")

	gate.SetReady(true)
	rec = doRequest(t, srv, http.MethodGet, constants.ActionsStatusPath, "", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &status))
	assert.True(t, status.Ready)
}

// ---------------------------------------------------------------------------
// SET_BINDING + LIFECYCLE_HOOK lifecycle
// ---------------------------------------------------------------------------

func TestActionsSetBindingDenyFirstThenHookActivates(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	s := subject.FromSandboxUID(testUID)
	input := `{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`

	// SET_BINDING: deny-first registered, policy stored pending, DNS denies
	rec := doAction(t, srv, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, &input))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))
	require.Len(t, nft.denyFirst, 1)
	assert.Nil(t, reg.EffectivePolicy(s), "DNS must keep denying while the policy is pending")
	rec = doRequest(t, srv, http.MethodGet, "/policy", uidHeader(s), "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "denying")

	// runtime-ready confirms; still denying
	rec = doAction(t, srv, hookBody(t, testUID, testFenceR, testFenceA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))

	// data-plane-ready applies the pending policy -> active
	rec = doAction(t, srv, hookBody(t, testUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	require.Equal(t, 1, nft.appliedCount())
	eff := reg.EffectivePolicy(s)
	require.NotNil(t, eff)
	assert.Equal(t, "allow", eff.Evaluate("example.com"))
}

func TestActionsSetBindingUpdateWhileActiveDoesNotReplay(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	input := `{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`
	s := setBindingAndReady(t, srv, reg, testUID, testIP, &input)
	denyFirstInstalls := len(nft.denyFirst)

	// plain input update (new spec generation, same fence): applied in place
	newInput := `{"defaultAction":"deny","egress":[{"action":"allow","target":"updated.com"}]}`
	rec := doAction(t, srv, bindBody(t, testUID, testIP, testFenceR, testFenceA, 2, &newInput))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	require.Equal(t, 2, nft.appliedCount())
	require.Equal(t, denyFirstInstalls, len(nft.denyFirst), "an update must not re-install deny-first")
	assert.Equal(t, "allow", reg.EffectivePolicy(s).Evaluate("updated.com"))
}

func TestActionsSetBindingNullRemovesPolicy(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	input := `{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`
	s := setBindingAndReady(t, srv, reg, testUID, testIP, &input)
	denyFirstInstalls := len(nft.denyFirst)

	// binding removed from a still-live sandbox: back to deny-first
	rec := doAction(t, srv, bindBody(t, testUID, testIP, testFenceR, testFenceA, 3, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))
	assert.Nil(t, reg.EffectivePolicy(s))
	assert.Nil(t, reg.UserPolicy(s))
	require.Equal(t, denyFirstInstalls+1, len(nft.denyFirst), "revert must re-install deny-first")

	// the sandbox is still registered and can be re-activated by a new binding
	reInput := `{"defaultAction":"deny"}`
	rec = doAction(t, srv, bindBody(t, testUID, testIP, testFenceR, testFenceA, 4, &reInput))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, testUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateActive, mustState(reg.Get(s)))
}

func TestActionsDataPlaneReadyWithoutPendingPolicyFailsClosed(t *testing.T) {
	srv, reg, _ := fleetTestServer(t)
	s := subject.FromSandboxUID(testUID)

	// binding removed immediately (null input): no pending policy exists
	rec := doAction(t, srv, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))

	// a (protocol-violating) data-plane-ready without a pending policy must
	// fail closed: the subject never activates
	rec = doAction(t, srv, hookBody(t, testUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))
}

func TestActionsRebindDiscardsPolicy(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	input := `{"defaultAction":"deny","egress":[{"action":"allow","target":"old.com"}]}`
	s := setBindingAndReady(t, srv, reg, testUID, testIP, &input)

	// same UID, new runtime instance: full reset to deny-first, old policy gone
	rec := doAction(t, srv, bindBody(t, testUID, testIP, "runtime-2", "attachment-2", 5, &input))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))
	assert.Nil(t, reg.EffectivePolicy(s), "old policy must not survive a rebind")

	// the new instance's data-plane-ready activates with its own policy
	newInput := `{"defaultAction":"deny","egress":[{"action":"allow","target":"new.com"}]}`
	rec = doAction(t, srv, bindBody(t, testUID, testIP, "runtime-2", "attachment-2", 6, &newInput))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, testUID, "runtime-2", "attachment-2", constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	assert.Equal(t, "allow", reg.EffectivePolicy(s).Evaluate("new.com"))
	require.Greater(t, len(nft.denyFirst), 0)
}

func TestActionsSetBindingIdempotentRetry(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	s := subject.FromSandboxUID(testUID)
	input := `{"defaultAction":"deny"}`

	// deny-first install fails: 500, subject stays absent-of-effective-policy
	nft.mu.Lock()
	nft.denyFirstErr = errors.New("nft busy")
	nft.mu.Unlock()
	rec := doAction(t, srv, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, &input))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))

	// fastlet retries the same logical operation; the retry succeeds
	nft.mu.Lock()
	nft.denyFirstErr = nil
	nft.mu.Unlock()
	rec = doAction(t, srv, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, &input))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))
	require.Len(t, nft.denyFirst, 1, "only the successful attempt installs deny-first")
}

func TestActionsInvalidPolicyInputRejected(t *testing.T) {
	srv, reg, _ := fleetTestServer(t)
	bad := "not-json"
	rec := doAction(t, srv, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, &bad))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	_, ok := reg.Get(subject.FromSandboxUID(testUID))
	assert.False(t, ok, "invalid input must not leave the subject half-registered")

	// empty input and the literal string "null" are ordinary values -> deny
	empty := ""
	rec = doAction(t, srv, bindBody(t, testUID, testIP, testFenceR, testFenceA, 2, &empty))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(subject.FromSandboxUID(testUID))))
}

// ---------------------------------------------------------------------------
// REMOVE_BINDING
// ---------------------------------------------------------------------------

func TestActionsRemoveBindingCleansUp(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	input := `{"defaultAction":"deny"}`
	s := setBindingAndReady(t, srv, reg, testUID, testIP, &input)

	// a pending vault push sits in the cache: removal must drop it
	rec := doRequest(t, srv, http.MethodPost, "/credential-vault", uidHeader(s), `{"credentials":[],"bindings":[]}`)
	require.Equal(t, http.StatusCreated, rec.Code)

	rec = doAction(t, srv, removeBody(t, testUID, testFenceR, testFenceA))
	require.Equal(t, http.StatusOK, rec.Code)
	_, ok := reg.Get(s)
	assert.False(t, ok)
	require.Len(t, nft.removed, 1)

	// a later registration must not flush the removed sandbox's state
	rec = doRequest(t, srv, http.MethodGet, "/credential-vault", uidHeader(s), "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestActionsRemoveBindingMissingStateSuccess(t *testing.T) {
	srv, _, nft := fleetTestServer(t)
	rec := doAction(t, srv, removeBody(t, "ghost", testFenceR, testFenceA))
	require.Equal(t, http.StatusOK, rec.Code, "missing Handler state is success")
	require.Len(t, nft.removed, 0)
	// cached pushes for the dead sandbox are dropped
	rec = doRequest(t, srv, http.MethodPost, "/credential-vault", "ghost", `{"credentials":[],"bindings":[]}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	rec = doAction(t, srv, removeBody(t, "ghost", testFenceR, testFenceA))
	require.Equal(t, http.StatusOK, rec.Code)
	srv.mu.Lock()
	_, ok := srv.pending[subject.FromSandboxUID("ghost")]
	srv.mu.Unlock()
	assert.False(t, ok, "remove must drop cached pushes even for an unregistered UID")
}

func TestActionsRemoveBindingStaleFenceIgnored(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	input := `{"defaultAction":"deny"}`
	s := setBindingAndReady(t, srv, reg, testUID, testIP, &input)

	// a removal from a PREVIOUS instance of the same UID must not unload the
	// current subject
	rec := doAction(t, srv, removeBody(t, testUID, "runtime-old", "attachment-old"))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	require.Len(t, nft.removed, 0)
}

func TestActionsHookForUnregisteredSubjectConflict(t *testing.T) {
	srv, _, _ := fleetTestServer(t)
	rec := doAction(t, srv, hookBody(t, "ghost", testFenceR, testFenceA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusConflict, rec.Code)
	rec = doAction(t, srv, hookBody(t, "ghost", testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusConflict, rec.Code)
}

// ---------------------------------------------------------------------------
// Envelope validation
// ---------------------------------------------------------------------------

func TestActionsInvalidEnvelopesRejected(t *testing.T) {
	srv, _, _ := fleetTestServer(t)
	cases := []struct {
		name string
		body string
	}{
		{"bad apiVersion", `{"apiVersion":"v1","operation":"SET_BINDING","sandbox":{"uid":"u"},"revision":{"runtimeInstanceId":"r","attachmentId":"a"},"attachment":{"network":{"ip":"10.0.0.5","gateway":"10.0.0.1","hostVeth":"v"}},"binding":{"input":"{}"}}`},
		{"unknown operation", `{"apiVersion":"sandbox.fast.io/actions/v1","operation":"EXPLODE","sandbox":{"uid":"u"},"revision":{"runtimeInstanceId":"r","attachmentId":"a"}}`},
		{"missing uid", `{"apiVersion":"sandbox.fast.io/actions/v1","operation":"SET_BINDING","revision":{"runtimeInstanceId":"r","attachmentId":"a"}}`},
		{"set_binding without binding", `{"apiVersion":"sandbox.fast.io/actions/v1","operation":"SET_BINDING","sandbox":{"uid":"u"},"revision":{"runtimeInstanceId":"r","attachmentId":"a"}}`},
		{"set_binding non-string input", `{"apiVersion":"sandbox.fast.io/actions/v1","operation":"SET_BINDING","sandbox":{"uid":"u"},"revision":{"runtimeInstanceId":"r","attachmentId":"a"},"attachment":{"network":{"ip":"10.0.0.5","gateway":"10.0.0.1","hostVeth":"v"}},"binding":{"input":42}}`},
		{"set_binding without attachment", `{"apiVersion":"sandbox.fast.io/actions/v1","operation":"SET_BINDING","sandbox":{"uid":"u"},"revision":{"runtimeInstanceId":"r","attachmentId":"a"},"binding":{"input":"{}"}}`},
		{"hook without hook", `{"apiVersion":"sandbox.fast.io/actions/v1","operation":"LIFECYCLE_HOOK","sandbox":{"uid":"u"},"revision":{"runtimeInstanceId":"r","attachmentId":"a"}}`},
		{"unknown hook", `{"apiVersion":"sandbox.fast.io/actions/v1","operation":"LIFECYCLE_HOOK","sandbox":{"uid":"u"},"revision":{"runtimeInstanceId":"r","attachmentId":"a"},"hook":{"name":"sandbox.before-delete"}}`},
		{"missing fence", `{"apiVersion":"sandbox.fast.io/actions/v1","operation":"SET_BINDING","sandbox":{"uid":"u"},"attachment":{"network":{"ip":"10.0.0.5","gateway":"10.0.0.1","hostVeth":"v"}},"binding":{"input":"{}"}}`},
		{"not json", `{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doAction(t, srv, tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestActionsSetBindingNullDropsPendingPushes(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	s := subject.FromSandboxUID(testUID)

	// a policy push races the binding and lands in the pending cache
	rec := doRequest(t, srv, http.MethodPut, "/policy", uidHeader(s),
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.Equal(t, http.StatusAccepted, rec.Code)

	// the binding is removed before it ever carried a policy: the cached push
	// must be dropped, never applied to a policy-less subject
	rec = doAction(t, srv, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))
	assert.Equal(t, 0, nft.appliedCount())
	assert.Nil(t, reg.EffectivePolicy(s))

	srv.mu.Lock()
	_, ok := srv.pending[s]
	srv.mu.Unlock()
	assert.False(t, ok, "binding removal must drop cached pushes")
}

// ---------------------------------------------------------------------------
// Proxy-route policy/vault surface (unchanged semantics)
// ---------------------------------------------------------------------------

func TestFleetServerPolicyRouting(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	uid := "u-1"
	s := setBindingAndReady(t, srv, reg, uid, testIP, strPtr(`{"defaultAction":"deny"}`))
	_ = s

	// unknown UID -> 404 on read
	rec := doRequest(t, srv, http.MethodGet, "/policy", "ghost", "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	// push policy for an active subject -> in-place apply
	rec = doRequest(t, srv, http.MethodPut, "/policy", uid,
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 2, nft.appliedCount())
	state, ok := reg.Get(subject.FromSandboxUID(uid))
	require.True(t, ok)
	assert.Equal(t, subject.StateActive, state)

	// GET reflects the subject's policy
	rec = doRequest(t, srv, http.MethodGet, "/policy", uid, "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "example.com")

	// missing header -> 400
	rec = doRequest(t, srv, http.MethodPut, "/policy", "", `{"defaultAction":"deny"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestFleetServerPolicyPushWhileDenyingStaysPending (review fix + binding
// authority): a runtime /policy push for a still-denying subject must neither
// activate it nor override the SET_BINDING input — data-plane-ready applies
// the binding's policy, not the pushed one.
func TestFleetServerPolicyPushWhileDenyingStaysPending(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	s := subject.FromSandboxUID("u-1")
	binding := `{"defaultAction":"deny","egress":[{"action":"allow","target":"binding.com"}]}`
	rec := doAction(t, srv, bindBody(t, "u-1", testIP, testFenceR, testFenceA, 1, &binding))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))

	// a push with a DIFFERENT policy while denying: 202, stored nowhere, the
	// binding's pending policy is untouched
	rec = doRequest(t, srv, http.MethodPut, "/policy", "u-1",
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"pushed.com"}]}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))
	require.Equal(t, 0, nft.appliedCount())

	// data-plane-ready applies the BINDING policy, not the pushed one
	rec = doAction(t, srv, hookBody(t, "u-1", testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	require.Equal(t, 1, nft.appliedCount())
	assert.Equal(t, "allow", reg.EffectivePolicy(s).Evaluate("binding.com"))
	assert.Equal(t, "deny", reg.EffectivePolicy(s).Evaluate("pushed.com"), "a denying push must not override the binding")

	// an active subject accepts the same push in place (runtime override;
	// the next SET_BINDING resets it)
	rec = doRequest(t, srv, http.MethodPut, "/policy", "u-1",
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"pushed.com"}]}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "allow", reg.EffectivePolicy(s).Evaluate("pushed.com"))
}

func TestFleetServerPolicyPendingPushFlushedOnSetBinding(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	s := subject.FromSandboxUID("u-1")

	// push before the binding exists -> cached as pending (202), nothing applied
	rec := doRequest(t, srv, http.MethodPut, "/policy", "u-1",
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, 0, nft.appliedCount())

	// SET_BINDING registers the subject deny-first and flushes the cached
	// push — but the binding input is authoritative, so the flush does not
	// change the pending policy (the binding's default-deny stays)
	rec = doAction(t, srv, bindBody(t, "u-1", testIP, testFenceR, testFenceA, 1, strPtr(`{}`)))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))
	assert.Equal(t, 0, nft.appliedCount())

	// data-plane-ready applies the BINDING input (default deny), not the
	// flushed push
	rec = doAction(t, srv, hookBody(t, "u-1", testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	assert.Equal(t, 1, nft.appliedCount())
	eff := reg.EffectivePolicy(s)
	require.NotNil(t, eff)
	assert.Equal(t, "deny", eff.Evaluate("example.com"), "the binding input is authoritative over cached pushes")
}

func TestFleetServerPendingGenerationMismatchDropped(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	s := subject.FromSandboxUID("u-1")

	rec := doRequest(t, srv, http.MethodPut, "/policy", "u-1",
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	// header generation 9 set on the cached request
	srv.mu.Lock()
	if qs, ok := srv.pending[s]; ok && len(qs) > 0 {
		qs[0].hasGen = true
		qs[0].gen = 9
	}
	srv.mu.Unlock()

	// SET_BINDING with spec generation 1 -> mismatch, pending dropped. The
	// subject is registered (deny-first) before the flush; no policy lands.
	rec = doAction(t, srv, bindBody(t, "u-1", testIP, testFenceR, testFenceA, 1, strPtr(`{"defaultAction":"deny"}`)))
	require.Equal(t, http.StatusOK, rec.Code)

	state, _ := reg.Get(s)
	assert.Equal(t, subject.StateDenying, state, "stale pending push must never activate a rebound sandbox")
	assert.Equal(t, 0, nft.appliedCount())
}

func TestFleetServerCredentialVaultPerSubject(t *testing.T) {
	srv, reg, _ := fleetTestServer(t)
	sA := subject.FromSandboxUID("a")
	sB := subject.FromSandboxUID("b")
	reg.Register(sA, subject.SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.5")}, subject.Fencing{RuntimeInstanceID: "r-a", AttachmentID: "a-a"})
	reg.Register(sB, subject.SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.6")}, subject.Fencing{RuntimeInstanceID: "r-b", AttachmentID: "a-b"})

	rec := doRequest(t, srv, http.MethodPost, "/credential-vault", uidHeader(sA),
		`{"credentials":[],"bindings":[]}`)
	require.Equal(t, http.StatusCreated, rec.Code)

	// subject B's vault does not exist yet (same as sidecar: 404); subject
	// A's vault has the revision
	rec = doRequest(t, srv, http.MethodGet, "/credential-vault", uidHeader(sB), "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	rec = doRequest(t, srv, http.MethodGet, "/credential-vault", uidHeader(sA), "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"revision":1`)
}

func TestFleetServerOnUnloadedDropsPending(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	s := subject.FromSandboxUID("u-1")
	rec := doRequest(t, srv, http.MethodPut, "/policy", uidHeader(s), `{"defaultAction":"deny"}`)
	require.Equal(t, http.StatusAccepted, rec.Code)

	reg.Register(s, subject.SubjectKey{}, subject.Fencing{RuntimeInstanceID: "r-1", AttachmentID: "a-1"})
	require.NoError(t, srv.OnUnloaded(s, testAttachment().Network))
	require.Len(t, nft.removed, 1)

	// pending was dropped; a later registration must NOT flush stale policy
	reg.Register(s, subject.SubjectKey{}, subject.Fencing{RuntimeInstanceID: "r-2", AttachmentID: "a-2"})
	srv.OnRegisteredComplete(s, testAttachment().Network, 2)
	state, _ := reg.Get(s)
	assert.Equal(t, subject.StateDenying, state)
}

func TestFleetServerHealthz(t *testing.T) {
	srv, _, _ := fleetTestServer(t)
	rec := doRequest(t, srv, http.MethodGet, "/healthz", "", "")
	require.Equal(t, http.StatusOK, rec.Code)
}

// ---------------------------------------------------------------------------
// MITM interception redirects
// ---------------------------------------------------------------------------

type fakeMitmInstaller struct {
	mu      sync.Mutex
	entries []iptables.MitmRedirectEntry
	err     error
	removed bool
}

func (f *fakeMitmInstaller) install(entries []iptables.MitmRedirectEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.entries = append([]iptables.MitmRedirectEntry(nil), entries...)
	return nil
}

func (f *fakeMitmInstaller) remove() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = true
	f.entries = nil
	return nil
}

func (f *fakeMitmInstaller) snapshot() []iptables.MitmRedirectEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]iptables.MitmRedirectEntry(nil), f.entries...)
}

func mitmAtt(t *testing.T, ip, gw string) actionhandler.NetworkAttachment {
	t.Helper()
	return actionhandler.NetworkAttachment{
		IP:          netip.MustParseAddr(ip),
		Gateway:     netip.MustParseAddr(gw),
		PrivateCIDR: netip.MustParsePrefix("10.0.0.0/24"),
		HostVeth:    "v",
	}
}

func TestFleetServerMitmRedirectInstalledOnRegistration(t *testing.T) {
	srv, _, _ := fleetTestServer(t)
	inst := &fakeMitmInstaller{}
	srv.SetMitm(nil, 18081, []int{80, 443})
	srv.mitmInstall = inst.install
	srv.mitmRemove = inst.remove

	s := subject.FromSandboxUID("u-1")
	require.NoError(t, srv.OnRegistered(s, mitmAtt(t, "10.0.0.5", "10.0.0.1")))
	require.Equal(t, []iptables.MitmRedirectEntry{{SandboxIP: netip.MustParseAddr("10.0.0.5"), Gateway: netip.MustParseAddr("10.0.0.1")}}, inst.snapshot())

	// a second subject rebuilds with both entries
	s2 := subject.FromSandboxUID("u-2")
	require.NoError(t, srv.OnRegistered(s2, mitmAtt(t, "10.0.0.6", "10.0.0.1")))
	require.ElementsMatch(t, []iptables.MitmRedirectEntry{
		{SandboxIP: netip.MustParseAddr("10.0.0.5"), Gateway: netip.MustParseAddr("10.0.0.1")},
		{SandboxIP: netip.MustParseAddr("10.0.0.6"), Gateway: netip.MustParseAddr("10.0.0.1")},
	}, inst.snapshot())

	// unload rebuilds without the subject
	require.NoError(t, srv.OnUnloaded(s, mitmAtt(t, "10.0.0.5", "10.0.0.1")))
	require.Equal(t, []iptables.MitmRedirectEntry{{SandboxIP: netip.MustParseAddr("10.0.0.6"), Gateway: netip.MustParseAddr("10.0.0.1")}}, inst.snapshot())
}

func TestFleetServerMitmRedirectFailClosesRegistration(t *testing.T) {
	srv, _, nft := fleetTestServer(t)
	inst := &fakeMitmInstaller{err: fmt.Errorf("nft unavailable")}
	srv.SetMitm(nil, 18081, []int{80, 443})
	srv.mitmInstall = inst.install

	s := subject.FromSandboxUID("u-1")
	require.Error(t, srv.OnRegistered(s, mitmAtt(t, "10.0.0.5", "10.0.0.1")))
	// the entry was rolled back: nothing left for the next rebuild
	srv.mitmMu.Lock()
	require.Len(t, srv.mitmEntries, 0)
	srv.mitmMu.Unlock()
	// the subject stays denying (deny-first ran before the failed redirect)
	require.Len(t, nft.denyFirst, 1)
}

func TestFleetServerMitmRedirectCrossFamilyRejected(t *testing.T) {
	srv, _, _ := fleetTestServer(t)
	inst := &fakeMitmInstaller{}
	srv.SetMitm(nil, 18081, []int{80, 443})
	srv.mitmInstall = inst.install

	s := subject.FromSandboxUID("u-1")
	att := mitmAtt(t, "10.0.0.5", "10.0.0.1")
	// v6 sandbox IP with a v4 gateway: an illegal nft expression that would
	// abort the whole transactional rebuild — must be rejected up front
	att.IP = netip.MustParseAddr("fd00::5")
	require.Error(t, srv.OnRegistered(s, att))
	require.Nil(t, inst.snapshot())
	// invalid gateway likewise
	att = mitmAtt(t, "10.0.0.5", "10.0.0.1")
	att.Gateway = netip.Addr{}
	require.Error(t, srv.OnRegistered(s, att))
	require.Nil(t, inst.snapshot())
}

func TestFleetServerMitmDisabledSkipsInterception(t *testing.T) {
	srv, _, _ := fleetTestServer(t)
	inst := &fakeMitmInstaller{}
	// SetMitm never called with a gate: hooks must not install anything
	srv.mitmInstall = inst.install

	s := subject.FromSandboxUID("u-1")
	require.NoError(t, srv.OnRegistered(s, mitmAtt(t, "10.0.0.5", "10.0.0.1")))
	require.NoError(t, srv.OnUnloaded(s, mitmAtt(t, "10.0.0.5", "10.0.0.1")))
	require.Nil(t, inst.snapshot())
}

func TestFleetServerHealthzMitmGate(t *testing.T) {
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	srv, _, _ := fleetTestServer(t)
	gate := mitmproxy.NewHealthGate()
	srv.mitmGate = gate

	gate.SetReady(false)
	rec := doRequest(t, srv, http.MethodGet, "/healthz", "", "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "mitmproxy not ready")

	gate.SetReady(true)
	rec = doRequest(t, srv, http.MethodGet, "/healthz", "", "")
	require.Equal(t, http.StatusOK, rec.Code)
}

// ---------------------------------------------------------------------------
// Subject-aware active vault API
// ---------------------------------------------------------------------------

func TestFleetServerActiveVaultClientIPDispatch(t *testing.T) {
	srv, reg, _ := fleetTestServer(t)
	sA := subject.FromSandboxUID("a")
	sB := subject.FromSandboxUID("b")
	reg.Register(sA, subject.SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.5")}, subject.Fencing{RuntimeInstanceID: "r-a", AttachmentID: "a-a"})
	reg.Register(sB, subject.SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.6")}, subject.Fencing{RuntimeInstanceID: "r-b", AttachmentID: "a-b"})

	// activate subject A (bindings require an effective policy that allows
	// the bound host)
	rec := doAction(t, srv, bindBody(t, "a", "10.0.0.5", "r-a", "a-a", 1,
		strPtr(`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, "a", "r-a", "a-a", constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = doRequest(t, srv, http.MethodPost, "/credential-vault", uidHeader(sA),
		`{"credentials":[{"name":"k","source":{"type":"inline","value":"v"}}],"bindings":[{"name":"b","match":{"hosts":["example.com"]},"auth":{"type":"apiKey","name":"X-API-Key","credential":"k"}}]}`)
	require.Equal(t, http.StatusCreated, rec.Code)

	// client IP -> subject A vault (the active endpoint is served on the
	// unix socket; here the handler is exercised directly)
	rec = httptest.NewRecorder()
	srv.handleCredentialVaultActive(rec, httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp=10.0.0.5", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"revision":1`)

	// subject B has no vault: 404 (the addon treats it as no-vault)
	rec = httptest.NewRecorder()
	srv.handleCredentialVaultActive(rec, httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp=10.0.0.6", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)

	// unknown IP: 404
	rec = httptest.NewRecorder()
	srv.handleCredentialVaultActive(rec, httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp=10.0.0.99", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)

	// missing / malformed clientIp: 400
	rec = httptest.NewRecorder()
	srv.handleCredentialVaultActive(rec, httptest.NewRequest(http.MethodGet, "/credential-vault/_active", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec = httptest.NewRecorder()
	srv.handleCredentialVaultActive(rec, httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp=not-an-ip", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// the HTTP mux must NOT expose the active endpoint (sidecar parity: the
	// addon is the only client, over the unix socket)
	rec = doRequest(t, srv, http.MethodGet, "/credential-vault/_active?clientIp=10.0.0.5", "", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestActionsCreateThenConfigureEndToEnd exercises the actions-driven
// create-then-configure spine: SET_BINDING installs deny-first, a vault push
// racing registration is flushed, data-plane-ready activates the policy, and
// REMOVE_BINDING tears everything down.
func TestActionsCreateThenConfigureEndToEnd(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	uid := "e2e-1"
	s := subject.FromSandboxUID(uid)

	// SET_BINDING with the policy: deny-first, policy pending
	rec := doAction(t, srv, bindBody(t, uid, testIP, testFenceR, testFenceA, 1, strPtr(`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))
	assert.Nil(t, reg.EffectivePolicy(s), "fail-closed: DNS denies while the policy is pending")

	// vault push after registration goes straight through
	rec = doRequest(t, srv, http.MethodPost, "/credential-vault", uid, `{"credentials":[],"bindings":[]}`)
	require.Equal(t, http.StatusCreated, rec.Code)

	// data-plane-ready activates the policy
	rec = doAction(t, srv, hookBody(t, uid, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	assert.Equal(t, "allow", reg.EffectivePolicy(s).Evaluate("example.com"))
	require.Equal(t, 1, nft.appliedCount())

	// REMOVE_BINDING tears everything down
	rec = doAction(t, srv, removeBody(t, uid, testFenceR, testFenceA))
	require.Equal(t, http.StatusOK, rec.Code)
	_, ok := reg.Get(s)
	assert.False(t, ok)
	require.Len(t, nft.removed, 1)
	rec = doRequest(t, srv, http.MethodGet, "/credential-vault", uid, "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestFleetServerAlwaysRulesReachNft: allow.always/deny.always must be
// enforced at the IP layer too, not only in DNS dispatch. The nft swap must
// receive the always-merged effective policy.
func TestFleetServerAlwaysRulesReachNft(t *testing.T) {
	alwaysDeny, err := policy.ParseValidatedEgressRule("deny", "203.0.113.0/24")
	require.NoError(t, err)
	alwaysAllow, err := policy.ParseValidatedEgressRule("allow", "198.51.100.7")
	require.NoError(t, err)

	reg := subject.NewRegistry([]policy.EgressRule{alwaysDeny}, []policy.EgressRule{alwaysAllow})
	nft := &fakeNft{}
	srv := newFleetPolicyServer(context.Background(), reg, nft, time.Minute)
	srv.dnsRedirectInstall = func(netip.Addr, int) error { return nil }
	srv.dnsRedirectRemove = func() error { return nil }

	// activation through the action protocol; the binding policy reaches nft
	// merged with the always overlay
	rec := doAction(t, srv, bindBody(t, "u-1", testIP, testFenceR, testFenceA, 1, strPtr(`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, "u-1", testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)

	nft.mu.Lock()
	applied := nft.lastPolicy
	nft.mu.Unlock()
	require.NotNil(t, applied, "nft must receive the merged policy")
	allowV4, _, denyV4, _ := applied.StaticIPSets()
	require.Contains(t, denyV4, "203.0.113.0/24", "always-deny CIDR must reach the nft deny set")
	require.Contains(t, allowV4, "198.51.100.7", "always-allow IP must reach the nft allow set")
}

// TestFleetServerNftFailureKeepsRegistryState: a failed nft apply must leave
// the registry (DNS/GET) on the PREVIOUS policy — nft commits before registry
// state.
func TestFleetServerNftFailureKeepsRegistryState(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	input := `{"defaultAction":"deny"}`
	s := setBindingAndReady(t, srv, reg, "u-1", testIP, &input)
	require.Equal(t, subject.StateActive, mustState(reg.Get(s)))

	// second push fails at nft: registry must stay on the FIRST policy
	nft.mu.Lock()
	nft.policyErr = errors.New("nft busy")
	nft.mu.Unlock()
	rec := doRequest(t, srv, http.MethodPut, "/policy", "u-1",
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	eff := reg.EffectivePolicy(s)
	require.NotNil(t, eff)
	assert.Equal(t, "deny", eff.Evaluate("example.com"), "failed nft apply must not publish the new policy")
}

// TestFleetServerGatewayRedirectRefcounted: the shared prerouting REDIRECT
// installs once per gateway and is removed when the last subject using it is
// unloaded.
func TestFleetServerGatewayRedirectRefcounted(t *testing.T) {
	reg := subject.NewRegistry(nil, nil)
	nft := &fakeNft{}
	srv := newFleetPolicyServer(context.Background(), reg, nft, time.Minute)
	var installs, removes int
	srv.dnsRedirectInstall = func(netip.Addr, int) error { installs++; return nil }
	srv.dnsRedirectRemove = func() error { removes++; return nil }

	attA := mitmAtt(t, "10.0.0.5", "10.10.0.1")
	attB := mitmAtt(t, "10.0.0.6", "10.10.0.1")
	attC := mitmAtt(t, "10.0.0.7", "10.20.0.1")

	require.NoError(t, srv.OnRegistered(subject.FromSandboxUID("a"), attA))
	require.NoError(t, srv.OnRegistered(subject.FromSandboxUID("b"), attB))
	require.NoError(t, srv.OnRegistered(subject.FromSandboxUID("c"), attC))
	assert.Equal(t, 2, installs, "shared gateway installs once; distinct gateway installs again")

	require.NoError(t, srv.OnUnloaded(subject.FromSandboxUID("a"), attA))
	require.NoError(t, srv.OnUnloaded(subject.FromSandboxUID("b"), attB))
	assert.Equal(t, 1, removes, "remove only when the LAST subject of the shared gateway unloads")

	require.NoError(t, srv.OnUnloaded(subject.FromSandboxUID("c"), attC))
	assert.Equal(t, 2, removes)
}

func strPtr(s string) *string { return &s }

func mustState(st subject.State, ok bool) subject.State {
	if !ok {
		panic("subject absent")
	}
	return st
}

// ---------------------------------------------------------------------------
// Review fixes: lifecycle-barrier retry semantics (PR #1678 comments)
// ---------------------------------------------------------------------------

// TestActionsDataPlaneReadyRetriesAfterNftFailure (review fix): a transient
// nft failure on data-plane-ready must NOT consume the pending policy — the
// Fastlet's retry of the same Hook succeeds instead of 409-ing forever with
// the subject permanently deny-first.
func TestActionsDataPlaneReadyRetriesAfterNftFailure(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	s := subject.FromSandboxUID("u-1")
	input := `{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`
	rec := doAction(t, srv, bindBody(t, "u-1", testIP, testFenceR, testFenceA, 1, &input))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))

	// first data-plane-ready fails at nft: 500, pending policy retained
	nft.mu.Lock()
	nft.policyErr = errors.New("nft busy")
	nft.mu.Unlock()
	rec = doAction(t, srv, hookBody(t, "u-1", testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))

	// retry of the SAME Hook succeeds with the retained pending policy
	nft.mu.Lock()
	nft.policyErr = nil
	nft.mu.Unlock()
	rec = doAction(t, srv, hookBody(t, "u-1", testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	require.Equal(t, 1, nft.appliedCount())
	assert.Equal(t, "allow", reg.EffectivePolicy(s).Evaluate("example.com"))
}

// TestActionsStaleHookFenceRejected (review fix): a delayed data-plane-ready
// from a previous instance must not consume the replacement sandbox's pending
// policy or activate it before its own data plane is ready.
func TestActionsStaleHookFenceRejected(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	s := subject.FromSandboxUID("u-1")
	input := `{"defaultAction":"deny","egress":[{"action":"allow","target":"new.com"}]}`
	rec := doAction(t, srv, bindBody(t, "u-1", testIP, "runtime-2", "attachment-2", 1, &input))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))

	// stale Hook (old runtime identity): 409, pending policy untouched, the
	// subject stays deny-first
	rec = doAction(t, srv, hookBody(t, "u-1", "runtime-old", "attachment-old", constants.HookDataPlaneReady))
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, subject.StateDenying, mustState(reg.Get(s)))
	assert.Equal(t, 0, nft.appliedCount())

	// the genuine Hook (current fence) succeeds
	rec = doAction(t, srv, hookBody(t, "u-1", "runtime-2", "attachment-2", constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	assert.Equal(t, "allow", reg.EffectivePolicy(s).Evaluate("new.com"))
}

// TestActionsRemoveBindingRetriesAfterNftFailure (review fix): a transient
// nft removal failure must keep the subject registered so the retried
// REMOVE_BINDING resumes cleanup — otherwise it would succeed with stale
// rules left for a possibly-reused IP.
func TestActionsRemoveBindingRetriesAfterNftFailure(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	input := `{"defaultAction":"deny"}`
	s := setBindingAndReady(t, srv, reg, "u-1", testIP, &input)

	// first REMOVE_BINDING fails at nft: 500, the subject STAYS registered
	nft.mu.Lock()
	nft.removeErr = errors.New("nft busy")
	nft.mu.Unlock()
	rec := doAction(t, srv, removeBody(t, "u-1", testFenceR, testFenceA))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	state, ok := reg.Get(s)
	require.True(t, ok, "subject must stay registered after a failed removal")
	assert.Equal(t, subject.StateActive, state)

	// retried REMOVE_BINDING resumes cleanup and completes (the fake only
	// records successful removes; the registry state below proves the retry
	// re-ran the enforcement removal instead of short-circuiting)
	nft.mu.Lock()
	nft.removeErr = nil
	nft.mu.Unlock()
	rec = doAction(t, srv, removeBody(t, "u-1", testFenceR, testFenceA))
	require.Equal(t, http.StatusOK, rec.Code)
	_, ok = reg.Get(s)
	assert.False(t, ok)
	require.Len(t, nft.removed, 1, "the retry must re-attempt and complete the nft removal")
}

// TestFleetServerGatewayRedirectDuplicateSetBindingIdempotent (review fix):
// at-least-once SET_BINDING delivery must not double-count the gateway — a
// duplicate registration is a no-op, and one unload fully releases it.
func TestFleetServerGatewayRedirectDuplicateSetBindingIdempotent(t *testing.T) {
	reg := subject.NewRegistry(nil, nil)
	nft := &fakeNft{}
	srv := newFleetPolicyServer(context.Background(), reg, nft, time.Minute)
	var installs, removes int
	srv.dnsRedirectInstall = func(netip.Addr, int) error { installs++; return nil }
	srv.dnsRedirectRemove = func() error { removes++; return nil }

	att := mitmAtt(t, "10.0.0.5", "10.10.0.1")
	s := subject.FromSandboxUID("a")
	require.NoError(t, srv.OnRegistered(s, att))
	require.NoError(t, srv.OnRegistered(s, att), "duplicate SET_BINDING delivery")
	assert.Equal(t, 1, installs, "duplicate registration must not re-install the redirect")

	require.NoError(t, srv.OnUnloaded(s, att))
	assert.Equal(t, 1, removes, "one unload must fully release the gateway")
}

// TestFleetServerGatewayRedirectRebindMovesGateway: a rebind that moved the
// subject to a different gateway releases the old gateway's redirect.
func TestFleetServerGatewayRedirectRebindMovesGateway(t *testing.T) {
	reg := subject.NewRegistry(nil, nil)
	nft := &fakeNft{}
	srv := newFleetPolicyServer(context.Background(), reg, nft, time.Minute)
	var installs, removes int
	srv.dnsRedirectInstall = func(netip.Addr, int) error { installs++; return nil }
	srv.dnsRedirectRemove = func() error { removes++; return nil }

	s := subject.FromSandboxUID("a")
	att1 := mitmAtt(t, "10.0.0.5", "10.10.0.1")
	att2 := mitmAtt(t, "10.0.0.5", "10.20.0.1")
	require.NoError(t, srv.OnRegistered(s, att1))
	require.NoError(t, srv.OnRegistered(s, att2), "rebind with a new gateway")
	assert.Equal(t, 2, installs)

	require.NoError(t, srv.OnUnloaded(s, att2))
	assert.Equal(t, 2, removes, "both gateways must be released")
}
