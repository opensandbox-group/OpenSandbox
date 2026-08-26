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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/slotsource"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// fakeNft implements fleetNftApplier with recording.
type fakeNft struct {
	mu            sync.Mutex
	denyFirst     []subject.Subject
	policyApplied []subject.Subject
	lastPolicy    *policy.NetworkPolicy
	removed       []subject.Subject
	dispatchUpd   []subject.Subject
	denyFirstErr  error
	policyErr     error
}

func (f *fakeNft) ApplyDenyFirst(_ context.Context, s subject.Subject, _ slotsource.Slot) error {
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

func (f *fakeNft) ApplyDispatchUpdate(_ context.Context, s subject.Subject, _ slotsource.Slot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatchUpd = append(f.dispatchUpd, s)
	return nil
}

func (f *fakeNft) Remove(_ context.Context, s subject.Subject) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func TestFleetServerPolicyRouting(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	s := subject.FromSandboxUID("u-1")
	reg.Register(s, subject.SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.5")},
		subject.Fencing{SandboxUID: "u-1", InstanceGeneration: 1, AssignmentAttempt: 1})

	// unknown UID -> 404 on read
	rec := doRequest(t, srv, http.MethodGet, "/policy", "ghost", "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	// push policy for a registered subject -> active
	rec = doRequest(t, srv, http.MethodPut, "/policy", uidHeader(s),
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, nft.appliedCount())
	state, ok := reg.Get(s)
	require.True(t, ok)
	assert.Equal(t, subject.StateActive, state)

	// GET reflects the subject's policy
	rec = doRequest(t, srv, http.MethodGet, "/policy", uidHeader(s), "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "example.com")

	// missing header -> 400
	rec = doRequest(t, srv, http.MethodPut, "/policy", "", `{"defaultAction":"deny"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFleetServerPolicyPendingPushFlushedOnRegistration(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	s := subject.FromSandboxUID("u-1")

	// push before the slot exists -> cached as pending (202), nothing applied
	rec := doRequest(t, srv, http.MethodPut, "/policy", uidHeader(s),
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, 0, nft.appliedCount())

	// slot appears: controller registration path flushes the pending push
	reg.Register(s, subject.SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.5")},
		subject.Fencing{SandboxUID: "u-1", InstanceGeneration: 1, AssignmentAttempt: 1})
	dir := t.TempDir()
	dnsPath := filepath.Join(dir, "resolv.conf")
	require.NoError(t, os.WriteFile(dnsPath, []byte("nameserver 10.96.0.10\n"), 0o644))
	slot := slotsource.Slot{
		ID: "slot-1", Phase: slotsource.PhaseBound,
		Owner: slotsource.Owner{SandboxUID: "u-1", InstanceGeneration: 1, AssignmentAttempt: 1},
		IP:    netip.MustParseAddr("10.0.0.5"), HostNetnsPath: "/n", HostVeth: "v",
		Gateway: netip.MustParseAddr("10.0.0.1"), DNSPath: dnsPath,
	}
	require.NoError(t, srv.OnRegistered(s, slot))
	srv.OnRegisteredComplete(s, slot)

	state, ok := reg.Get(s)
	require.True(t, ok)
	assert.Equal(t, subject.StateActive, state, "pending push must be applied on registration")
	assert.Equal(t, 1, nft.appliedCount())
	eff := reg.EffectivePolicy(s)
	require.NotNil(t, eff)
	assert.Equal(t, "allow", eff.Evaluate("example.com"), "pending push must replay the EXACT pushed policy, not default-deny")
}

func TestFleetServerPendingGenerationMismatchDropped(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	s := subject.FromSandboxUID("u-1")

	rec := doRequest(t, srv, http.MethodPut, "/policy", uidHeader(s),
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	// header generation 2 set on the cached request
	srv.mu.Lock()
	if qs, ok := srv.pending[s]; ok && len(qs) > 0 {
		qs[0].hasGen = true
		qs[0].gen = 2
	}
	srv.mu.Unlock()

	// slot appears with generation 1 -> mismatch, pending dropped. The
	// controller registers the subject (deny-first) before the flush.
	reg.Register(s, subject.SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.5")},
		subject.Fencing{SandboxUID: "u-1", InstanceGeneration: 1, AssignmentAttempt: 1})
	dir := t.TempDir()
	dnsPath := filepath.Join(dir, "resolv.conf")
	require.NoError(t, os.WriteFile(dnsPath, []byte("nameserver 10.96.0.10\n"), 0o644))
	slot := slotsource.Slot{
		ID: "slot-1", Phase: slotsource.PhaseBound,
		Owner: slotsource.Owner{SandboxUID: "u-1", InstanceGeneration: 1, AssignmentAttempt: 1},
		IP:    netip.MustParseAddr("10.0.0.5"), HostNetnsPath: "/n", HostVeth: "v",
		Gateway: netip.MustParseAddr("10.0.0.1"), DNSPath: dnsPath,
	}
	require.NoError(t, srv.OnRegistered(s, slot))
	srv.OnRegisteredComplete(s, slot)

	state, _ := reg.Get(s)
	assert.Equal(t, subject.StateDenying, state, "stale pending push must never activate a rebound sandbox")
	assert.Equal(t, 0, nft.appliedCount())
}

func TestFleetServerCredentialVaultPerSubject(t *testing.T) {
	srv, reg, _ := fleetTestServer(t)
	sA := subject.FromSandboxUID("a")
	sB := subject.FromSandboxUID("b")
	reg.Register(sA, subject.SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.5")}, subject.Fencing{SandboxUID: "a"})
	reg.Register(sB, subject.SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.6")}, subject.Fencing{SandboxUID: "b"})

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

func TestFleetServerOnRegisteredResolvRewrite(t *testing.T) {
	srv, _, nft := fleetTestServer(t)
	dir := t.TempDir()
	dnsPath := filepath.Join(dir, "resolv.conf")
	require.NoError(t, os.WriteFile(dnsPath, []byte("nameserver 10.96.0.10\nsearch svc.local\n"), 0o644))
	s := subject.FromSandboxUID("u-1")
	slot := slotsource.Slot{
		ID: "slot-1", Phase: slotsource.PhaseBound,
		Owner: slotsource.Owner{SandboxUID: "u-1", InstanceGeneration: 1, AssignmentAttempt: 1},
		IP:    netip.MustParseAddr("10.0.0.5"), HostNetnsPath: "/n", HostVeth: "v",
		Gateway: netip.MustParseAddr("10.0.0.1"), DNSPath: dnsPath,
	}
	require.NoError(t, srv.OnRegistered(s, slot))
	content, err := os.ReadFile(dnsPath)
	require.NoError(t, err)
	require.Equal(t, "nameserver 10.0.0.1\nsearch svc.local\n", string(content))
	require.Len(t, nft.denyFirst, 1)

	// resolv rewrite failure -> error (fail closed, controller retries)
	nft.denyFirstErr = nil
	slot.DNSPath = filepath.Join(dir, "missing.conf")
	require.Error(t, srv.OnRegistered(s, slot))
}

func TestFleetServerOnUnloadedDropsPending(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	s := subject.FromSandboxUID("u-1")
	rec := doRequest(t, srv, http.MethodPut, "/policy", uidHeader(s), `{"defaultAction":"deny"}`)
	require.Equal(t, http.StatusAccepted, rec.Code)

	reg.Register(s, subject.SubjectKey{}, subject.Fencing{SandboxUID: "u-1"})
	require.NoError(t, srv.OnUnloaded(s, slotsource.Slot{Owner: slotsource.Owner{SandboxUID: "u-1", InstanceGeneration: 1}}))
	require.Len(t, nft.removed, 1)

	// pending was dropped; a later registration must NOT flush stale policy
	reg.Register(s, subject.SubjectKey{}, subject.Fencing{SandboxUID: "u-1", InstanceGeneration: 2})
	srv.OnRegisteredComplete(s, slotsource.Slot{Owner: slotsource.Owner{SandboxUID: "u-1", InstanceGeneration: 2}})
	state, _ := reg.Get(s)
	assert.Equal(t, subject.StateDenying, state)
}

func TestFleetServerHealthz(t *testing.T) {
	srv, _, _ := fleetTestServer(t)
	rec := doRequest(t, srv, http.MethodGet, "/healthz", "", "")
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestFleetCreateThenConfigureEndToEnd exercises the create-then-
// configure spine: policy pushed before the slot exists is cached pending,
// and the controller applies it (deny-first -> active) once the slot appears.
func TestFleetCreateThenConfigureEndToEnd(t *testing.T) {
	dir := t.TempDir()
	dnsPath := filepath.Join(dir, "resolv.conf")
	require.NoError(t, os.WriteFile(dnsPath, []byte("nameserver 10.96.0.10\n"), 0o644))

	src := slotsource.NewFileSource(dir, 20*time.Millisecond)
	reg := subject.NewRegistry(nil, nil)
	nft := &fakeNft{}
	srv := newFleetPolicyServer(context.Background(), reg, nft, time.Minute)
	srv.dnsRedirectInstall = func(netip.Addr, int) error { return nil }
	srv.dnsRedirectRemove = func() error { return nil }
	controller := subject.NewController(reg, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	controllerErr := controller.StartWatch(ctx, src)

	uid := "e2e-1"
	rec := doRequest(t, srv, http.MethodPut, "/policy", uid,
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.Equal(t, http.StatusAccepted, rec.Code, "push before slot exists must be accepted (pending)")

	// slot bound appears; resolv file exists so deny-first install succeeds
	writeSlotFile(t, filepath.Join(dir, "e2e.json"), uid, dnsPath)
	waitForState(t, 10*time.Second, func() bool {
		state, ok := reg.Get(subject.FromSandboxUID(uid))
		return ok && state == subject.StateActive
	})

	// resolv.conf rewritten to the gateway (deny-first side effect)
	content, err := os.ReadFile(dnsPath)
	require.NoError(t, err)
	require.Contains(t, string(content), "nameserver 10.0.0.1")

	// vault push after registration goes straight through
	rec = doRequest(t, srv, http.MethodPost, "/credential-vault", uid, `{"credentials":[],"bindings":[]}`)
	require.Equal(t, http.StatusCreated, rec.Code)

	cancel()
	require.NoError(t, <-controllerErr)
}

func writeSlotFile(t *testing.T, path, uid, dnsPath string) {
	t.Helper()
	content := `{"id":"%s","phase":"Bound","owner":{"sandboxUid":"%s","instanceGeneration":1,"assignmentAttempt":1},"ip":"10.0.0.5","hostNetnsPath":"/n","hostVeth":"v","gateway":"10.0.0.1","dnsPath":"%s"}`
	require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf(content, uid, uid, dnsPath)), 0o644))
}

func waitForState(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
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
	s := subject.FromSandboxUID("u-1")
	reg.Register(s, subject.SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.5")},
		subject.Fencing{SandboxUID: "u-1", InstanceGeneration: 1, AssignmentAttempt: 1})

	rec := doRequest(t, srv, http.MethodPut, "/policy", "u-1",
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.Equal(t, http.StatusOK, rec.Code)

	nft.mu.Lock()
	applied := nft.lastPolicy
	nft.mu.Unlock()
	require.NotNil(t, applied, "nft must receive the merged policy")
	allowV4, _, denyV4, _ := applied.StaticIPSets()
	require.Contains(t, denyV4, "203.0.113.0/24", "always-deny CIDR must reach the nft deny set")
	require.Contains(t, allowV4, "198.51.100.7", "always-allow IP must reach the nft allow set")
}

// TestFleetServerNftFailureKeepsRegistryState (Fix 4): a failed nft apply
// must leave the registry (DNS/GET) on the PREVIOUS policy — nft commits
// before registry state.
func TestFleetServerNftFailureKeepsRegistryState(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	s := subject.FromSandboxUID("u-1")
	reg.Register(s, subject.SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.5")},
		subject.Fencing{SandboxUID: "u-1", InstanceGeneration: 1, AssignmentAttempt: 1})

	rec := doRequest(t, srv, http.MethodPut, "/policy", "u-1", `{"defaultAction":"deny"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateActive, mustState2(reg.Get(s)))

	// second push fails at nft: registry must stay on the FIRST policy
	nft.mu.Lock()
	nft.policyErr = errors.New("nft busy")
	nft.mu.Unlock()
	rec = doRequest(t, srv, http.MethodPut, "/policy", "u-1",
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	eff := reg.EffectivePolicy(s)
	require.NotNil(t, eff)
	assert.Equal(t, "deny", eff.Evaluate("example.com"), "failed nft apply must not publish the new policy")
}

// TestFleetServerPendingQueueReplaysBothPushes (Fix 6): policy AND vault
// pushes arriving before the slot must BOTH be replayed in order.
func TestFleetServerPendingQueueReplaysBothPushes(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	s := subject.FromSandboxUID("u-1")

	rec := doRequest(t, srv, http.MethodPut, "/policy", "u-1",
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	rec = doRequest(t, srv, http.MethodPost, "/credential-vault", "u-1", `{"credentials":[],"bindings":[]}`)
	require.Equal(t, http.StatusAccepted, rec.Code)

	// slot appears: both pushes flush in order
	dir := t.TempDir()
	dnsPath := filepath.Join(dir, "resolv.conf")
	require.NoError(t, os.WriteFile(dnsPath, []byte("nameserver 10.96.0.10\n"), 0o644))
	reg.Register(s, subject.SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.5")},
		subject.Fencing{SandboxUID: "u-1", InstanceGeneration: 1, AssignmentAttempt: 1})
	slot := slotsource.Slot{
		ID: "slot-1", Phase: slotsource.PhaseBound,
		Owner: slotsource.Owner{SandboxUID: "u-1", InstanceGeneration: 1, AssignmentAttempt: 1},
		IP:    netip.MustParseAddr("10.0.0.5"), HostNetnsPath: "/n", HostVeth: "v",
		Gateway: netip.MustParseAddr("10.0.0.1"), DNSPath: dnsPath,
	}
	require.NoError(t, srv.OnRegistered(s, slot))
	srv.OnRegisteredComplete(s, slot)

	// policy applied with its exact content, and the vault was created
	eff := reg.EffectivePolicy(s)
	require.NotNil(t, eff)
	assert.Equal(t, "allow", eff.Evaluate("example.com"))
	rec = doRequest(t, srv, http.MethodGet, "/credential-vault", "u-1", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"revision":1`)
	require.Equal(t, 1, nft.appliedCount())
}

// TestFleetServerOnSlotUpdatedReconciles (Fix 5): an EventUpdated with
// unchanged fencing must rewrite resolv and update the dispatch rule without
// resetting the policy.
func TestFleetServerOnSlotUpdatedReconciles(t *testing.T) {
	srv, reg, nft := fleetTestServer(t)
	s := subject.FromSandboxUID("u-1")
	reg.Register(s, subject.SubjectKey{SourceIP: netip.MustParseAddr("10.0.0.5")},
		subject.Fencing{SandboxUID: "u-1", InstanceGeneration: 1, AssignmentAttempt: 1})
	rec := doRequest(t, srv, http.MethodPut, "/policy", "u-1",
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)
	require.Equal(t, http.StatusOK, rec.Code)

	dir := t.TempDir()
	dnsPath := filepath.Join(dir, "resolv.conf")
	require.NoError(t, os.WriteFile(dnsPath, []byte("nameserver 10.96.0.10\n"), 0o644))
	slot := slotsource.Slot{
		ID: "slot-1", Phase: slotsource.PhaseBound,
		Owner: slotsource.Owner{SandboxUID: "u-1", InstanceGeneration: 1, AssignmentAttempt: 1},
		IP:    netip.MustParseAddr("10.0.0.5"), HostNetnsPath: "/n", HostVeth: "veth-new",
		Gateway: netip.MustParseAddr("10.0.0.1"), DNSPath: dnsPath,
	}
	require.NoError(t, srv.OnSlotUpdated(s, slot))

	content, err := os.ReadFile(dnsPath)
	require.NoError(t, err)
	require.Contains(t, string(content), "nameserver 10.0.0.1")
	nft.mu.Lock()
	require.Len(t, nft.dispatchUpd, 1)
	nft.mu.Unlock()
	// policy untouched
	eff := reg.EffectivePolicy(s)
	require.NotNil(t, eff)
	assert.Equal(t, "allow", eff.Evaluate("example.com"))
}

func mustState2(st subject.State, ok bool) subject.State {
	if !ok {
		panic("subject absent")
	}
	return st
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

	dir := t.TempDir()
	mkResolv := func(name string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte("nameserver 10.96.0.10\n"), 0o644))
		return p
	}
	slotA := slotsource.Slot{Owner: slotsource.Owner{SandboxUID: "a"}, Gateway: netip.MustParseAddr("10.10.0.1"), DNSPath: mkResolv("a.conf")}
	slotB := slotsource.Slot{Owner: slotsource.Owner{SandboxUID: "b"}, Gateway: netip.MustParseAddr("10.10.0.1"), DNSPath: mkResolv("b.conf")}
	slotC := slotsource.Slot{Owner: slotsource.Owner{SandboxUID: "c"}, Gateway: netip.MustParseAddr("10.20.0.1"), DNSPath: mkResolv("c.conf")}

	require.NoError(t, srv.OnRegistered(subject.FromSandboxUID("a"), slotA))
	require.NoError(t, srv.OnRegistered(subject.FromSandboxUID("b"), slotB))
	require.NoError(t, srv.OnRegistered(subject.FromSandboxUID("c"), slotC))
	assert.Equal(t, 2, installs, "shared gateway installs once; distinct gateway installs again")

	require.NoError(t, srv.OnUnloaded(subject.FromSandboxUID("a"), slotA))
	require.NoError(t, srv.OnUnloaded(subject.FromSandboxUID("b"), slotB))
	assert.Equal(t, 1, removes, "remove only when the LAST subject of the shared gateway unloads")

	require.NoError(t, srv.OnUnloaded(subject.FromSandboxUID("c"), slotC))
	assert.Equal(t, 2, removes)
}
