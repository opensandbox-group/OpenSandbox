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
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// ---------------------------------------------------------------------------
// Relay policy surface: /policy/{sandboxUid}
// ---------------------------------------------------------------------------

const policyTestUID = "policy-u"

// policyAllow is a minimal effective policy.
func policyAllow() string {
	return `{"defaultAction":"deny","egress":[{"action":"allow","target":"api.example.com"}]}`
}

func policyAllowAlt() string {
	return `{"defaultAction":"deny","egress":[{"action":"allow","target":"other.example.com"}]}`
}

// policyPutDoc wraps a policy body in the relay document with an optional
// precondition (either spelling).
func policyPutDoc(policyBody string, revision int64, withRevision, withExpected bool) string {
	var b strings.Builder
	b.WriteString(`{"policy":`)
	b.WriteString(policyBody)
	if withRevision {
		b.WriteString(`,"revision":`)
		b.WriteString(strconv.FormatInt(revision, 10))
	}
	if withExpected {
		b.WriteString(`,"expectedRevision":`)
		b.WriteString(strconv.FormatInt(revision, 10))
	}
	b.WriteString(`}`)
	return b.String()
}

// doPolicy drives one relay request against the surface; incarnation is the
// optional expectedIncarnation precondition header.
func doPolicy(t *testing.T, srv *fastSandboxPolicyServer, method, uid, body, incarnation string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, constants.PolicyRelayPathPrefix+uid, strings.NewReader(body))
	if incarnation != "" {
		req.Header.Set(constants.EgressVaultIncarnationHeader, incarnation)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// mustPolicyRevision decodes a relay GET/PUT body's revision.
func mustPolicyRevision(t *testing.T, rec *httptest.ResponseRecorder) (int64, policyRelayDocument) {
	t.Helper()
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, rec.Code, rec.Body.String())
	var doc policyRelayDocument
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	return doc.Revision, doc
}

func TestPolicyRelayUnknownSubject404NoCache(t *testing.T) {
	srv, _, _ := fastSandboxTestServer(t)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := doPolicy(t, srv, method, "ghost", policyPutDoc(policyAllow(), 0, false, false), "")
		assert.Equal(t, http.StatusNotFound, rec.Code, method)
	}
	// No policy record was materialized for the unknown subject.
	srv.mu.Lock()
	_, stored := srv.policies[subject.FromSandboxUID("ghost")]
	srv.mu.Unlock()
	assert.False(t, stored, "the relay surface never materializes state for an unregistered subject")
}

func TestPolicyRelayPutReplacesSeededRecord(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	setBindingAndReady(t, srv, reg, policyTestUID, vaultTestIP, strPtr(policyAllow()))

	// The binding input seeded the record at revision 1: GET serves the
	// seeded policy even before any relay write.
	rec := doPolicy(t, srv, http.MethodGet, policyTestUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	require.Equal(t, int64(1), rev)
	assert.Contains(t, string(doc.Policy), "api.example.com")

	// PUT replaces at the successor revision.
	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 0, false, false), "")
	require.Equal(t, http.StatusOK, rec.Code)
	rev, doc = mustPolicyRevision(t, rec)
	assert.Equal(t, int64(2), rev)
	assert.Contains(t, string(doc.Policy), "other.example.com")

	// Enforcement followed the relay write.
	s := subject.FromSandboxUID(policyTestUID)
	assert.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	assert.Equal(t, "allow", reg.UserPolicy(s).Evaluate("other.example.com"))

	// GET echoes the recorded policy + revision.
	rec = doPolicy(t, srv, http.MethodGet, policyTestUID, "", "")
	rev, doc = mustPolicyRevision(t, rec)
	assert.Equal(t, int64(2), rev)
	assert.Contains(t, string(doc.Policy), "other.example.com")
}

// TestRelayPutReArmsEnforcement (bug report: relay PUT persists + GET but
// enforcement stays at the SET_BINDING-time deny): after activation with a
// deny bootstrap, a relay PUT(allow) must re-apply enforcement — nft gets
// the allow policy and the registry flips to it.
func TestRelayPutReArmsEnforcement(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	s := setBindingAndReady(t, srv, reg, policyTestUID, vaultTestIP, strPtr(`{"defaultAction":"deny"}`))
	nft.mu.Lock()
	require.NotNil(t, nft.lastPolicy)
	require.Equal(t, "deny", nft.lastPolicy.Evaluate("example.com"))
	denyApplies := len(nft.policyApplied)
	nft.mu.Unlock()

	// Relay PUT(allow) on the ACTIVE subject.
	rec := doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`, 1, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code)

	nft.mu.Lock()
	armed := nft.lastPolicy
	applies := len(nft.policyApplied)
	nft.mu.Unlock()
	require.NotNil(t, armed, "the relay PUT must re-arm enforcement (nft apply missing)")
	assert.Equal(t, "allow", armed.Evaluate("example.com"), "nft must be re-armed with the committed policy")
	assert.Greater(t, applies, denyApplies, "an apply must have run after the PUT")

	assert.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	assert.Equal(t, "allow", reg.UserPolicy(s).Evaluate("example.com"))
	rec = doPolicy(t, srv, http.MethodGet, policyTestUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(2), rev)
	assert.Contains(t, string(doc.Policy), "example.com")
}

// TestFirstPutAfterSeedArmsEnforcement (review discriminator: the FIRST
// relay PUT after SET_BINDING must re-arm enforcement to the committed
// record, in BOTH hook orderings).
func TestFirstPutAfterSeedArmsEnforcement(t *testing.T) {
	const allowPol = `{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`

	t.Run("put before data-plane-ready", func(t *testing.T) {
		srv, reg, nft := fastSandboxTestServer(t)
		s := subject.FromSandboxUID(policyTestUID)
		rec := doAction(t, srv, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 1, strPtr(`{"defaultAction":"deny"}`)))
		require.Equal(t, http.StatusOK, rec.Code)

		// FIRST PUT while denying (barrier hold).
		rec = doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(allowPol, 1, false, true), "")
		require.Equal(t, http.StatusOK, rec.Code)

		rec = doAction(t, srv, hookBody(t, policyTestUID, testFenceR, testFenceA, constants.HookRuntimeReady))
		require.Equal(t, http.StatusOK, rec.Code)
		rec = doAction(t, srv, hookBody(t, policyTestUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
		require.Equal(t, http.StatusOK, rec.Code)

		nft.mu.Lock()
		require.NotNil(t, nft.lastPolicy)
		assert.Equal(t, "allow", nft.lastPolicy.Evaluate("example.com"))
		nft.mu.Unlock()
		assert.Equal(t, "allow", reg.UserPolicy(s).Evaluate("example.com"))
		assert.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	})

	t.Run("put after data-plane-ready", func(t *testing.T) {
		srv, reg, nft := fastSandboxTestServer(t)
		s := setBindingAndReady(t, srv, reg, policyTestUID, vaultTestIP, strPtr(`{"defaultAction":"deny"}`))

		rec := doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(allowPol, 1, false, true), "")
		require.Equal(t, http.StatusOK, rec.Code)

		nft.mu.Lock()
		require.NotNil(t, nft.lastPolicy)
		assert.Equal(t, "allow", nft.lastPolicy.Evaluate("example.com"))
		nft.mu.Unlock()
		assert.Equal(t, "allow", reg.UserPolicy(s).Evaluate("example.com"))
		assert.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	})
}

func TestPolicyRelayPreconditions(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	setBindingAndReady(t, srv, reg, policyTestUID, vaultTestIP, strPtr(policyAllow()))

	// The binding input seeded revision 1; the first relay PUT is a replace.
	rec := doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code)

	// Stale expectedRevision is a 412.
	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllow(), 1, false, true), "")
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)

	// The GET revision acts as the precondition (round trip).
	rec = doPolicy(t, srv, http.MethodGet, policyTestUID, "", "")
	rev, _ := mustPolicyRevision(t, rec)
	require.Equal(t, int64(2), rev)
	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), rev, true, false), "")
	require.Equal(t, http.StatusOK, rec.Code)
	rev, _ = mustPolicyRevision(t, rec)
	require.Equal(t, int64(3), rev)

	// Stale round-trip revision: 412.
	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), rev-1, true, false), "")
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)

	// Both spellings agreeing is accepted.
	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllow(), rev, true, true), "")
	require.Equal(t, http.StatusOK, rec.Code)

	// Both spellings disagreeing is a 400.
	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, `{"policy":`+policyAllow()+`,"revision":1,"expectedRevision":2}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// DELETE lands a deny tombstone at the successor revision; a conditional
	// PUT against the pre-delete revision is a 412.
	rec = doPolicy(t, srv, http.MethodDelete, policyTestUID, "", "")
	require.Equal(t, http.StatusNoContent, rec.Code)
	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllow(), 4, false, true), "")
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)

	// Unconditional PUT replaces the tombstone at the successor revision.
	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllow(), 0, false, false), "")
	require.Equal(t, http.StatusOK, rec.Code)
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(6), rev)
	assert.Contains(t, string(doc.Policy), "api.example.com")
}

func TestPolicyRelayInvalidPolicyKeepsLastGood(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	setBindingAndReady(t, srv, reg, policyTestUID, vaultTestIP, strPtr(policyAllow()))

	rec := doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 0, false, false), "")
	require.Equal(t, http.StatusOK, rec.Code)

	// Invalid policy content is a 400 NACK; last-good kept. (defaultAction
	// is not action-validated by the parser; egress rule actions are.)
	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(`{"defaultAction":"deny","egress":[{"action":"bogus","target":"x.com"}]}`, 0, false, false), "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, `{`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, `{"unknownField":1}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, `{}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code)

	s := subject.FromSandboxUID(policyTestUID)
	assert.Equal(t, "allow", reg.UserPolicy(s).Evaluate("other.example.com"), "last-good policy untouched")
}

func TestPolicyRelayDeleteEnforcesDenyTombstone(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	s := setBindingAndReady(t, srv, reg, policyTestUID, vaultTestIP, strPtr(policyAllow()))
	require.Equal(t, subject.StateActive, mustState(reg.Get(s)))

	// DELETE removes the seeded policy (revision 1 → tombstone at 2) and
	// enforces default-deny IN PLACE: the subject stays active (a later
	// relay PUT re-enables immediately — the generic relay never replays a
	// data-plane-ready hook).
	rec := doPolicy(t, srv, http.MethodDelete, policyTestUID, "", "")
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	assert.Equal(t, "deny", reg_UserEvaluate(srv, s, "api.example.com"))
	nft.mu.Lock()
	require.NotNil(t, nft.lastPolicy)
	assert.Equal(t, "deny", nft.lastPolicy.Evaluate("api.example.com"), "the deny tombstone is enforced")
	nft.mu.Unlock()

	// The tombstone is authoritative: GET serves the default-deny record at
	// the successor revision, and a binding replay cannot resurrect the
	// deleted policy.
	rec = doPolicy(t, srv, http.MethodGet, policyTestUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(2), rev)
	assert.Contains(t, string(doc.Policy), `"defaultAction":"deny"`)
	assert.NotContains(t, string(doc.Policy), "api.example.com")
	rec = doAction(t, srv, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 2, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doPolicy(t, srv, http.MethodGet, policyTestUID, "", "")
	rev, doc = mustPolicyRevision(t, rec)
	assert.Equal(t, int64(2), rev, "the tombstone survives the binding replay")
	assert.NotContains(t, string(doc.Policy), "api.example.com")

	// DELETE is idempotent (the tombstone replaces itself at the successor
	// revision).
	rec = doPolicy(t, srv, http.MethodDelete, policyTestUID, "", "")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	rev, _ = mustPolicyRevision(t, doPolicy(t, srv, http.MethodGet, policyTestUID, "", ""))
	assert.Equal(t, int64(3), rev)

	// Re-add goes through the relay: the PUT replaces the tombstone and
	// re-enables immediately on the still-active subject.
	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllow(), 3, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code)
	rev, doc = mustPolicyRevision(t, rec)
	assert.Equal(t, int64(4), rev)
	assert.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	assert.Equal(t, "allow", reg_UserEvaluate(srv, s, "api.example.com"))
}

func TestPolicyRelayDenyingSubjectHoldsPending(t *testing.T) {
	srv, _, _ := fastSandboxTestServer(t)

	// SET_BINDING only: subject registered, still denying.
	rec := doAction(t, srv, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 1, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)

	// A relay PUT while denying replaces the pending policy (held to the
	// data-plane-ready barrier) at the successor revision.
	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code)
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(2), rev)
	assert.Contains(t, string(doc.Policy), "other.example.com")

	// GET serves the pending policy while held.
	rec = doPolicy(t, srv, http.MethodGet, policyTestUID, "", "")
	_, doc = mustPolicyRevision(t, rec)
	assert.Contains(t, string(doc.Policy), "other.example.com")

	// data-plane-ready activates the relayed policy.
	rec = doAction(t, srv, hookBody(t, policyTestUID, testFenceR, testFenceA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, policyTestUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	s := subject.FromSandboxUID(policyTestUID)
	assert.Equal(t, "allow", reg_UserEvaluate(srv, s, "other.example.com"))
}

// TestPolicyRelayPutReplacesStalePendingTombstone (review F4): after a
// DELETE on an active subject, the pending slot holds the deny tombstone; a
// later relay PUT must replace it, so an at-least-once data-plane-ready
// replay re-applies the REPLACEMENT, never the stale deny.
func TestPolicyRelayPutReplacesStalePendingTombstone(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	s := setBindingAndReady(t, srv, reg, policyTestUID, vaultTestIP, strPtr(policyAllow()))

	// DELETE lands the deny tombstone (active, enforced in place).
	rec := doPolicy(t, srv, http.MethodDelete, policyTestUID, "", "")
	require.Equal(t, http.StatusNoContent, rec.Code)

	// The relay PUT applies in place AND replaces the pending tombstone.
	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 2, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code)

	// A replayed data-plane-ready consumes the pending policy: it must be
	// the replacement, not the stale deny.
	rec = doAction(t, srv, hookBody(t, policyTestUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	assert.Equal(t, "allow", reg_UserEvaluate(srv, s, "other.example.com"))
	assert.Equal(t, "deny", reg_UserEvaluate(srv, s, "api.example.com"))
}

// hookNft forces an interleaving at the relay apply's enforcement step: the
// injected callback runs while the relay commit holds policyCommitMu, which
// is exactly the window a concurrent rebind would race through.
type hookNft struct {
	fakeNft
	onPolicy func()
}

func (h *hookNft) ApplyPolicy(ctx context.Context, s subject.Subject, pol *policy.NetworkPolicy) error {
	if h.onPolicy != nil {
		fn := h.onPolicy
		h.onPolicy = nil
		fn()
	}
	return h.fakeNft.ApplyPolicy(ctx, s, pol)
}

// TestRelayPutRacingRebindNeverActivatesPredecessorPolicy (review H3): a
// relay PUT for the PREDECESSOR identity whose apply lands after the
// rebind's registration must not activate the successor under the
// predecessor's policy. The rebind runs on another goroutine from inside
// the relay's enforcement step, forcing the fence flip into the apply
// window; the fixed serialization (the whole SET_BINDING mutation holds
// policyCommitMu) makes the rebind wait, the relay commit completes against
// the old instance, and the purge discards it.
func TestRelayPutRacingRebindNeverActivatesPredecessorPolicy(t *testing.T) {
	reg := subject.NewRegistry(nil, nil)
	hnft := &hookNft{}
	srv := newFastSandboxPolicyServer(context.Background(), reg, hnft)
	srv.dnsRedirectApply = func(context.Context, []netip.Addr, int) error { return nil }
	pp, err := newPolicyPersistence(t.TempDir())
	require.NoError(t, err)
	files := make(map[string]*fakeVaultFile)
	pp.newFile = func(path string) (vaultFileStore, error) {
		f, ok := files[path]
		if !ok {
			f = newFakeVaultFile(path)
			files[path] = f
		}
		return f, nil
	}
	srv.SetPolicyFiles(pp)

	const uid, ip = "race-u", "10.0.0.5"
	const oldPolicy = `{"defaultAction":"deny","egress":[{"action":"allow","target":"old.com"}]}`
	const newPolicy = `{"defaultAction":"deny","egress":[{"action":"allow","target":"new.com"}]}`

	// Generation one: old fence, active with old policy.
	rec := doAction(t, srv, bindBody(t, uid, ip, testFenceR, testFenceA, 1, strPtr(oldPolicy)))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, uid, testFenceR, testFenceA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, uid, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	s := subject.FromSandboxUID(uid)
	require.Equal(t, subject.StateActive, mustState(reg.Get(s)))

	// A queued relay PUT for the OLD identity: its apply BLOCKS inside the
	// enforcement step while the rebind (same UID, new fence) completes on
	// another goroutine — forcing the fence flip into the apply window. With
	// the serialization fix the rebind cannot proceed (policyCommitMu is
	// held by this very commit), so the block times out and the rebind runs
	// to completion afterwards; either way the successor must converge to
	// its own policy.
	started := make(chan struct{})
	done := make(chan struct{})
	hnft.onPolicy = func() {
		close(started)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			// Serialized: the rebind runs after this commit.
		}
	}
	go func() {
		<-started
		defer close(done)
		r := doAction(t, srv, bindBody(t, uid, ip, "runtime-2", "attachment-2", 2, strPtr(newPolicy)))
		assert.Equal(t, http.StatusOK, r.Code)
	}()

	// The relay write itself passes its guards (pre-read fence = old) and
	// must NOT leak its policy into the successor.
	rec = doPolicy(t, srv, http.MethodPut, uid, policyPutDoc(oldPolicy, 1, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the rebind never completed (serialization deadlock?)")
	}

	// The successor is deny-first with ITS OWN binding input — never active
	// under the predecessor's policy, whichever way the interleaving went.
	state, ok := reg.Get(s)
	require.True(t, ok)
	if state == subject.StateActive {
		// Activated only legitimately: the successor's own policy enforced.
		assert.Equal(t, "allow", reg.UserPolicy(s).Evaluate("new.com"))
		assert.Equal(t, "deny", reg.UserPolicy(s).Evaluate("old.com"))
	} else {
		assert.Equal(t, subject.StateDenying, state)
		assert.Nil(t, reg.EffectivePolicy(s), "deny-first until the successor's own data-plane-ready")
	}

	// The successor's own lifecycle completes with its own policy.
	rec = doAction(t, srv, hookBody(t, uid, "runtime-2", "attachment-2", constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, subject.StateActive, mustState(reg.Get(s)))
	assert.Equal(t, "allow", reg.UserPolicy(s).Evaluate("new.com"))
	assert.Equal(t, "deny", reg.UserPolicy(s).Evaluate("old.com"), "the predecessor's policy never reaches the successor")
}

// reg_UserEvaluate evaluates the subject's recorded user policy (test aid to
// keep assertions readable).
func reg_UserEvaluate(srv *fastSandboxPolicyServer, s subject.Subject, domain string) string {
	pol := srv.reg.UserPolicy(s)
	if pol == nil {
		return "no-policy"
	}
	return pol.Evaluate(domain)
}

func TestPolicyRelayIncarnationPrecondition(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	setBindingAndReady(t, srv, reg, policyTestUID, vaultTestIP, strPtr(policyAllow()))

	rec := doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), testFenceR)
	require.Equal(t, http.StatusOK, rec.Code)

	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllow(), 1, false, true), "runtime-999")
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)

	rec = doPolicy(t, srv, http.MethodGet, policyTestUID, "", "runtime-999")
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
	rec = doPolicy(t, srv, http.MethodGet, policyTestUID, "", testFenceR)
	require.Equal(t, http.StatusOK, rec.Code)

	rec = doPolicy(t, srv, http.MethodDelete, policyTestUID, "", "runtime-999")
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
}

func TestPolicyRelayMethodNotAllowed(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	setBindingAndReady(t, srv, reg, policyTestUID, vaultTestIP, strPtr(policyAllow()))

	rec := doPolicy(t, srv, http.MethodPost, policyTestUID, policyPutDoc(policyAllow(), 0, false, false), "")
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Contains(t, rec.Header().Get("Allow"), "PUT")

	rec = doPolicy(t, srv, http.MethodPatch, policyTestUID, "{}", "")
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	// The bare /policy path redirects into the subtree; the old UID-header
	// surface stays gone.
	rec = doRequest(t, srv, http.MethodGet, "/policy", "")
	assert.NotEqual(t, http.StatusOK, rec.Code)
}

func TestPolicyRelayRebindResetsRecord(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	s := setBindingAndReady(t, srv, reg, policyTestUID, vaultTestIP, strPtr(policyAllow()))

	rec := doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 0, false, false), "")
	require.Equal(t, http.StatusOK, rec.Code)

	// Rebind: same UID, NEW runtime instance — the recorded policy is gone.
	const newRuntime = "runtime-2"
	rec = doAction(t, srv, bindBody(t, policyTestUID, vaultTestIP, newRuntime, "attachment-2", 2, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, policyTestUID, newRuntime, "attachment-2", constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)

	// The relayed (previous instance's) policy is gone: GET serves the NEW
	// binding input at revision 1 — the successor was reseeded.
	rec = doPolicy(t, srv, http.MethodGet, policyTestUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(1), rev)
	assert.Contains(t, string(doc.Policy), "api.example.com")
	assert.NotContains(t, string(doc.Policy), "other.example.com", "the rebind discards the previous instance's recorded policy")
	assert.Equal(t, "allow", reg_UserEvaluate(srv, s, "api.example.com"), "the new binding input seeds the successor")

	// The old incarnation cannot write into the successor.
	rec = doPolicy(t, srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllow(), 1, false, true), testFenceR)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
}
