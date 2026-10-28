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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// Policy persistence: the recorded policy survives an egress restart and
// supersedes the replayed binding input. Outcomes are substituted at the
// same vaultFileStore seam the vault tests use.

// policyPersistHarness wires a fast-sandbox test server to fake-backed
// policy persistence over a real (swept) temp directory.
type policyPersistHarness struct {
	srv   *fastSandboxPolicyServer
	reg   *subject.MemoryRegistry
	nft   *fakeNft
	pp    *policyPersistence
	dir   string
	files map[string]*fakeVaultFile
}

func newPolicyPersistHarness(t *testing.T) *policyPersistHarness {
	t.Helper()
	dir := t.TempDir()
	h := &policyPersistHarness{files: make(map[string]*fakeVaultFile), dir: dir}
	h.srv, h.reg, h.nft = fastSandboxTestServer(t)
	pp, err := newPolicyPersistence(dir)
	require.NoError(t, err)
	pp.newFile = func(path string) (vaultFileStore, error) {
		f, ok := h.files[path]
		if !ok {
			f = newFakeVaultFile(path)
			h.files[path] = f
		}
		return f, nil
	}
	h.pp = pp
	h.srv.SetPolicyFiles(pp)
	// The policy clone flow also exercises the vault transfer (template
	// carry exports both states), so the harness wires vault persistence as
	// a SIBLING state directory (mirroring the production layout — same
	// volume, distinct basenames; aliasing them would clobber the shared
	// <uid>.json names).
	vp, err := newVaultPersistence(filepath.Join(h.dir, "vault"))
	require.NoError(t, err)
	vp.newFile = pp.newFile
	h.srv.SetVaultFiles(vp)
	return h
}

// readySubject drives SET_BINDING + both hooks under the default test fence.
func (h *policyPersistHarness) readySubject(t *testing.T, uid, ip string) subject.Subject {
	t.Helper()
	s := subject.FromSandboxUID(uid)
	rec := doAction(t, h.srv, bindBody(t, uid, ip, testFenceR, testFenceA, 1, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, uid, testFenceR, testFenceA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, uid, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	return s
}

func (h *policyPersistHarness) fileFor(uid string) *fakeVaultFile {
	path := filepath.Join(h.dir, uid+".json")
	f, ok := h.files[path]
	if !ok {
		f = newFakeVaultFile(path)
		h.files[path] = f
	}
	return f
}

// fakeForPath returns (creating if needed) the fake store for an arbitrary
// path — policy files and bundle staging files share the newFile seam.
func (h *policyPersistHarness) fakeForPath(path string) *fakeVaultFile {
	f, ok := h.files[path]
	if !ok {
		f = newFakeVaultFile(path)
		h.files[path] = f
	}
	return f
}

// seedPolicyFile writes simulated committed file bytes before registration.
func (h *policyPersistHarness) seedPolicyFile(t *testing.T, uid, incarnation, attachment string, revision int64, policyBody string) {
	t.Helper()
	raw, err := policy.EncodePolicyFile(incarnation, attachment, revision, json.RawMessage(policyBody))
	require.NoError(t, err)
	f := h.fileFor(uid)
	f.data = raw
	f.exists = true
}

// decodePolicyFile decodes the simulated file bytes of a subject.
func decodePolicyFile(t *testing.T, f *fakeVaultFile) (policy.PolicyFile, json.RawMessage) {
	t.Helper()
	require.True(t, f.exists, "the policy file must exist")
	pf, body, err := policy.DecodePolicyFile(f.data)
	require.NoError(t, err)
	return pf, body
}

// seedPolicyBundle writes a real bundle file carrying the OLD runtime fence
// (import reads the source with plain I/O — no injected seam).
func (h *policyPersistHarness) seedPolicyBundle(t *testing.T, path string, revision int64, policyBody string) []byte {
	t.Helper()
	raw, err := policy.EncodePolicyFile(testFenceR, testFenceA, revision, json.RawMessage(policyBody))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return raw
}

// policySnapshotBody builds a POLICY_EXPORT / POLICY_IMPORT envelope.
func policySnapshotBody(t *testing.T, uid, runtimeID, attID, op, path string, seal *bool) string {
	t.Helper()
	env := actionhandler.Envelope{
		APIVersion:   constants.ActionsAPIVersion,
		Operation:    actionhandler.Operation(op),
		InvocationID: fmt.Sprintf("inv-%s-policy-snapshot", uid),
		Sandbox:      actionhandler.SandboxRef{UID: uid, Name: "sb", Namespace: "ns"},
		Revision: actionhandler.Revision{
			SpecGeneration:    1,
			RuntimeInstanceID: runtimeID,
			AttachmentID:      attID,
			RouteGeneration:   1,
		},
		Snapshot: &actionhandler.SnapshotTransfer{Path: path, Seal: seal},
	}
	raw, err := json.Marshal(env)
	require.NoError(t, err)
	return string(raw)
}

// ---------------------------------------------------------------------------
// SET_BINDING seeds and persists the record
// ---------------------------------------------------------------------------

func TestPolicyPersistSetBindingStampsFile(t *testing.T) {
	h := newPolicyPersistHarness(t)
	h.readySubject(t, policyTestUID, vaultTestIP)

	pf, body := decodePolicyFile(t, h.fileFor(policyTestUID))
	assert.Equal(t, 1, pf.Version)
	assert.Equal(t, testFenceR, pf.Incarnation)
	assert.Equal(t, testFenceA, pf.AttachmentID)
	assert.Equal(t, int64(1), pf.Revision)
	assert.Contains(t, string(body), "api.example.com")

	// GET serves the seeded policy + revision.
	rec := doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(1), rev)
	assert.Contains(t, string(doc.Policy), "api.example.com")

	// A relay write persists the successor revision.
	rec = doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code)
	pf, body = decodePolicyFile(t, h.fileFor(policyTestUID))
	assert.Equal(t, int64(2), pf.Revision)
	assert.Contains(t, string(body), "other.example.com")
}

// ---------------------------------------------------------------------------
// Reload at SET_BINDING (egress-restart recovery): the file wins
// ---------------------------------------------------------------------------

func TestPolicyPersistReloadSupersedesBindingInput(t *testing.T) {
	h := newPolicyPersistHarness(t)

	// Committed state from the previous egress generation: revision 5
	// carrying a relay mutation (other.example.com). The replayed binding
	// input still carries the bootstrap policy (api.example.com).
	h.seedPolicyFile(t, policyTestUID, testFenceR, testFenceA, 5, policyAllowAlt())

	rec := doAction(t, h.srv, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 1, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)

	// The file is authoritative: the recorded policy serves at revision 5
	// and the binding input is superseded.
	rec = doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(5), rev)
	assert.Contains(t, string(doc.Policy), "other.example.com")
	assert.NotContains(t, string(doc.Policy), "api.example.com")

	// data-plane-ready activates the RELOADED policy.
	rec = doAction(t, h.srv, hookBody(t, policyTestUID, testFenceR, testFenceA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, policyTestUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	s := subject.FromSandboxUID(policyTestUID)
	assert.Equal(t, "allow", reg_UserEvaluate(h.srv, s, "other.example.com"))
	assert.Equal(t, "deny", reg_UserEvaluate(h.srv, s, "api.example.com"))

	// A replayed binding with the bootstrap input does not override the
	// file-backed record.
	rec = doAction(t, h.srv, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 2, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
	rev, doc = mustPolicyRevision(t, rec)
	assert.Equal(t, int64(5), rev, "a file-backed record never moves for a binding replay")
	assert.Contains(t, string(doc.Policy), "other.example.com")

	// A relay write on the file-backed record works and stays file-backed.
	rec = doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllow(), 5, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code)
	pf, _ := decodePolicyFile(t, h.fileFor(policyTestUID))
	assert.Equal(t, int64(6), pf.Revision)
	rec = doAction(t, h.srv, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 3, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
	rev, _ = mustPolicyRevision(t, rec)
	assert.Equal(t, int64(6), rev, "relay writes keep the record file-backed")
}

// TestPolicyPersistRestartRecoversRelayWrites: simulate an egress restart —
// a fresh server over the same persistence state — and verify the relayed
// policy survives the binding replay.
func TestPolicyPersistRestartRecoversRelayWrites(t *testing.T) {
	h := newPolicyPersistHarness(t)
	h.readySubject(t, policyTestUID, vaultTestIP)

	// Runtime mutation via the relay: revision 2.
	rec := doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code)

	// Fresh server over the same persistence state (the fake-backed files
	// simulate the disk and survive), then the Fastlet's binding replay.
	srv2, _, _ := fastSandboxTestServer(t)
	pp2, err := newPolicyPersistence(h.dir)
	require.NoError(t, err)
	pp2.newFile = h.pp.newFile
	srv2.SetPolicyFiles(pp2)

	rec = doAction(t, srv2, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 1, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv2, hookBody(t, policyTestUID, testFenceR, testFenceA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv2, hookBody(t, policyTestUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)

	// The relayed policy survived the restart.
	rec = doPolicy(t, srv2, http.MethodGet, policyTestUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(2), rev)
	assert.Contains(t, string(doc.Policy), "other.example.com")
	s := subject.FromSandboxUID(policyTestUID)
	assert.Equal(t, "allow", reg_UserEvaluate(srv2, s, "other.example.com"))
}

func TestPolicyPersistStaleIncarnationDiscarded(t *testing.T) {
	h := newPolicyPersistHarness(t)

	// A reset: the file belongs to a previous runtime instance.
	h.seedPolicyFile(t, policyTestUID, "runtime-0", "attachment-0", 5, policyAllowAlt())

	h.readySubject(t, policyTestUID, vaultTestIP)

	// Stale, discarded: the successor is seeded from its binding input and
	// the file is re-stamped with the successor's fence at revision 1.
	rec := doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(1), rev)
	assert.Contains(t, string(doc.Policy), "api.example.com")
	pf, _ := decodePolicyFile(t, h.fileFor(policyTestUID))
	assert.Equal(t, testFenceR, pf.Incarnation)
	assert.Equal(t, int64(1), pf.Revision)

	// The stale incarnation cannot write into the successor.
	rec = doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllow(), 1, false, true), "runtime-0")
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)
}

func TestPolicyPersistUnusableFilesDegradeFailClosed(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"corrupt", []byte("{corrupt json")},
		{"newer schema", []byte(`{"v":99,"incarnation":"x","attachmentId":"y","revision":1,"policy":{}}` + "\n")},
		{"unparseable policy", func() []byte {
			raw, _ := policy.EncodePolicyFile(testFenceR, testFenceA, 5, json.RawMessage(`{"egress":"not-a-list"}`))
			return raw
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPolicyPersistHarness(t)
			f := h.fileFor(policyTestUID)
			f.data = tc.data
			f.exists = true

			// Registration succeeds; the subject degrades FAIL CLOSED — the
			// bootstrap input must not seed (it may be more permissive than
			// the unreadable runtime change), the pending policy is
			// default-deny.
			h.readySubject(t, policyTestUID, vaultTestIP)
			rec := doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
			rev, doc := mustPolicyRevision(t, rec)
			assert.Equal(t, int64(0), rev)
			assert.Contains(t, string(doc.Policy), `"defaultAction":"deny"`)

			// The degraded mark surfaces on the actions status.
			status := mustStatus(t, h.srv)
			assert.Equal(t, []string{policyTestUID}, status.DegradedPolicies)
			assert.True(t, status.Ready, "a per-subject degradation never flips process readiness")

			// A relay PUT heals the subject: the degraded mark clears and
			// the written policy takes over at revision 1.
			// The degraded record exists at revision 0, so the healing PUT
			// is a replace (200), not a create.
			rec = doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllow(), 0, false, false), "")
			require.Equal(t, http.StatusOK, rec.Code)
			rec = doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
			rev, doc = mustPolicyRevision(t, rec)
			assert.Equal(t, int64(1), rev)
			assert.Contains(t, string(doc.Policy), "api.example.com")
			status = mustStatus(t, h.srv)
			assert.Empty(t, status.DegradedPolicies)
		})
	}
}

// ---------------------------------------------------------------------------
// Removal / rebind purge the durable record
// ---------------------------------------------------------------------------

func TestPolicyPersistRemovalPurgesRecord(t *testing.T) {
	h := newPolicyPersistHarness(t)
	h.readySubject(t, policyTestUID, vaultTestIP)
	require.True(t, h.fileFor(policyTestUID).exists, "precondition: the seeded record persisted")

	// Binding removal: deny-first is also the durable state.
	rec := doAction(t, h.srv, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 2, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, h.fileFor(policyTestUID).exists, "the removal purges the persisted policy")
	rec = doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// A later registration starts from its own input — never the removed
	// policy.
	rec = doAction(t, h.srv, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 3, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
	_, doc := mustPolicyRevision(t, rec)
	assert.Contains(t, string(doc.Policy), "api.example.com")
}

func TestPolicyPersistRebindPurgesRecord(t *testing.T) {
	h := newPolicyPersistHarness(t)
	h.readySubject(t, policyTestUID, vaultTestIP)
	require.True(t, h.fileFor(policyTestUID).exists)

	// Relay mutation at revision 2, then rebind.
	rec := doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code)

	const newRuntime = "runtime-2"
	rec = doAction(t, h.srv, bindBody(t, policyTestUID, vaultTestIP, newRuntime, "attachment-2", 2, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, policyTestUID, newRuntime, "attachment-2", constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)

	// The previous instance's relayed policy is gone; the successor is
	// seeded from its own binding input at revision 1.
	rec = doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(1), rev)
	assert.Contains(t, string(doc.Policy), "api.example.com")
	pf, _ := decodePolicyFile(t, h.fileFor(policyTestUID))
	assert.Equal(t, newRuntime, pf.Incarnation, "the file is stamped with the successor's fence")
}

// ---------------------------------------------------------------------------
// Commit protocol outcomes
// ---------------------------------------------------------------------------

func TestPolicyPersistCommitOutcomes(t *testing.T) {
	t.Run("pre-rename failure nacks without latch", func(t *testing.T) {
		h := newPolicyPersistHarness(t)
		h.readySubject(t, policyTestUID, vaultTestIP)
		f := h.fileFor(policyTestUID)
		f.saveState, f.saveErr = policy.FileUnchanged, errors.New("no space on temp create")

		rec := doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), "")
		require.Equal(t, http.StatusInternalServerError, rec.Code)

		// Last-good kept on both sides; no restore, no latch.
		assert.Equal(t, 0, f.restoreCalls)
		assert.Equal(t, int64(1), mustReadRevision(t, h, policyTestUID))
		f.saveErr, f.saveState = nil, policy.FileCommitted
		rec = doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), "")
		assert.Equal(t, http.StatusOK, rec.Code, "the next write with a working store succeeds")
	})

	t.Run("uncertain persist latches recovery and restores", func(t *testing.T) {
		h := newPolicyPersistHarness(t)
		h.readySubject(t, policyTestUID, vaultTestIP)
		goodFile := append([]byte(nil), h.fileFor(policyTestUID).data...)

		f := h.fileFor(policyTestUID)
		f.saveState, f.saveErr = policy.FileUnknown, errors.New("rename returned EIO")
		rec := doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), "")
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)

		assert.Equal(t, 1, f.restoreCalls)
		assert.Equal(t, string(goodFile), string(f.data), "the previous file image was restored")
		assert.True(t, h.srv.policyRecoveryActive(), "the recovery latch freezes policy writes until restart")

		// Frozen: relay writes 503, healthz 503, status flags recovery and
		// drops readiness.
		rec = doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), "")
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		rec = doRequest(t, h.srv, http.MethodGet, "/healthz", "")
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Contains(t, rec.Body.String(), "policy persistence recovery required")
		status := mustStatus(t, h.srv)
		assert.True(t, status.PolicyRecovery)
		assert.False(t, status.Ready)
	})

	t.Run("apply failure rolls the file back", func(t *testing.T) {
		h := newPolicyPersistHarness(t)
		h.readySubject(t, policyTestUID, vaultTestIP)
		goodFile := append([]byte(nil), h.fileFor(policyTestUID).data...)

		// The nft apply fails on an active subject: the file is rolled back
		// and the registry keeps the previous policy.
		h.nft.policyErr = errors.New("nft busy")
		rec := doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), "")
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		h.nft.policyErr = nil

		assert.Equal(t, 1, h.fileFor(policyTestUID).restoreCalls)
		assert.Equal(t, string(goodFile), string(h.fileFor(policyTestUID).data))
		assert.False(t, h.srv.policyRecoveryActive(), "a confirmed restore needs no latch")
		s := subject.FromSandboxUID(policyTestUID)
		assert.Equal(t, "allow", reg_UserEvaluate(h.srv, s, "api.example.com"), "the registry keeps the previous policy")
	})
}

// mustReadRevision reads the recorded revision through the relay GET.
func mustReadRevision(t *testing.T, h *policyPersistHarness, uid string) int64 {
	t.Helper()
	rec := doPolicy(t, h.srv, http.MethodGet, uid, "", "")
	rev, _ := mustPolicyRevision(t, rec)
	return rev
}

// TestPolicyPersistDeleteTombstoneSurvivesRestart: a relay DELETE persists
// an authoritative default-deny record, so an egress restart's binding
// replay loads the tombstone instead of resurrecting the deleted policy —
// and a replayed data-plane-ready activates the deny record.
func TestPolicyPersistDeleteTombstoneSurvivesRestart(t *testing.T) {
	h := newPolicyPersistHarness(t)
	h.readySubject(t, policyTestUID, vaultTestIP)

	// Runtime mutation to rev 2, then delete → deny tombstone at rev 3.
	rec := doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doPolicy(t, h.srv, http.MethodDelete, policyTestUID, "", "")
	require.Equal(t, http.StatusNoContent, rec.Code)
	pf, body := decodePolicyFile(t, h.fileFor(policyTestUID))
	assert.Equal(t, int64(3), pf.Revision)
	assert.Contains(t, string(body), `"defaultAction":"deny"`)

	// Fresh server over the same persistence state: the binding replay must
	// load the tombstone, not reseed the bootstrap policy.
	srv2, reg2, nft2 := fastSandboxTestServer(t)
	pp2, err := newPolicyPersistence(h.dir)
	require.NoError(t, err)
	pp2.newFile = h.pp.newFile
	srv2.SetPolicyFiles(pp2)

	rec = doAction(t, srv2, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 1, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv2, hookBody(t, policyTestUID, testFenceR, testFenceA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv2, hookBody(t, policyTestUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = doPolicy(t, srv2, http.MethodGet, policyTestUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(3), rev)
	assert.Contains(t, string(doc.Policy), `"defaultAction":"deny"`)
	assert.NotContains(t, string(doc.Policy), "api.example.com", "the deleted policy must not resurrect")

	// Enforcement is the deny tombstone, not the bootstrap policy.
	s := subject.FromSandboxUID(policyTestUID)
	assert.Equal(t, subject.StateActive, mustState(reg2.Get(s)))
	nft2.mu.Lock()
	last := nft2.lastPolicy
	nft2.mu.Unlock()
	require.NotNil(t, last)
	assert.Equal(t, "deny", last.Evaluate("api.example.com"))
	assert.Equal(t, "deny", last.Evaluate("other.example.com"))
}

// TestPolicyPersistStaleRecordNeverSupersedesFreshRegistration: a leftover
// record under a foreign fence (purge outcome uncertain) must not make
// applyBindingPolicy supersede the fresh registration's binding input.
func TestPolicyPersistStaleRecordNeverSupersedesFreshRegistration(t *testing.T) {
	h := newPolicyPersistHarness(t)

	// A stale record under a foreign fence survives on disk (its purge
	// failed) and in memory.
	h.readySubject(t, policyTestUID, vaultTestIP)
	rec := doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code)
	h.fileFor(policyTestUID).failFirstDelete = errors.New("unlink EIO")
	// Rebind with an uncertain purge outcome: the relayed policy must not
	// leak into the successor.
	const newRuntime = "runtime-2"
	rec = doAction(t, h.srv, bindBody(t, policyTestUID, vaultTestIP, newRuntime, "attachment-2", 2, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, policyTestUID, newRuntime, "attachment-2", constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)

	// The successor was seeded from its OWN binding input at revision 1.
	rec = doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(1), rev)
	assert.Contains(t, string(doc.Policy), "api.example.com")
	assert.NotContains(t, string(doc.Policy), "other.example.com", "the foreign-fence record must not supersede the fresh input")
	pf, _ := decodePolicyFile(t, h.fileFor(policyTestUID))
	assert.Equal(t, newRuntime, pf.Incarnation)
}

// TestPolicyPersistRemovalPurgeFailureLandsDenyTombstone (review F3): a
// binding removal whose durable purge fails must not leave the removed
// policy's file recoverable — a deny tombstone lands instead, so a later
// same-fence registration loads deny, never the removed policy.
func TestPolicyPersistRemovalPurgeFailureLandsDenyTombstone(t *testing.T) {
	h := newPolicyPersistHarness(t)
	h.readySubject(t, policyTestUID, vaultTestIP)

	// Runtime mutation to rev 2, then a removal whose unlink fails.
	rec := doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code)
	h.fileFor(policyTestUID).failFirstDelete = errors.New("unlink EIO")

	rec = doAction(t, h.srv, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 2, nil))
	require.Equal(t, http.StatusOK, rec.Code)

	// The tombstone replaced the removed policy durably.
	pf, body := decodePolicyFile(t, h.fileFor(policyTestUID))
	assert.Equal(t, int64(3), pf.Revision)
	assert.Contains(t, string(body), `"defaultAction":"deny"`)
	assert.NotContains(t, string(body), "other.example.com", "the removed policy must not survive the removal")

	// A later same-fence registration loads the tombstone — never the
	// removed policy.
	rec = doAction(t, h.srv, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 3, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(3), rev)
	assert.Contains(t, string(doc.Policy), `"defaultAction":"deny"`)
}

// TestPolicyPersistSealedFilelessSubjectSurvivesRestart (review C1): an
// export seals a file-less subject; the restart's binding replay must
// restore the seal WITHOUT deadlocking (the sealed branch runs under the
// reload's own s.mu), and the relay ops stay 409 with default-deny pending.
func TestPolicyPersistSealedFilelessSubjectSurvivesRestart(t *testing.T) {
	h := newPolicyPersistHarness(t)

	// Bind, then remove the policy (healthy purge → file-less), then export
	// (seals the file-less subject).
	h.readySubject(t, policyTestUID, vaultTestIP)
	rec := doAction(t, h.srv, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 2, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.False(t, h.fileFor(policyTestUID).exists, "precondition: file-less after removal")
	bundlePath := filepath.Join(t.TempDir(), "bundle", policyTestUID+".json")
	rec = doAction(t, h.srv, policySnapshotBody(t, policyTestUID, testFenceR, testFenceA, "POLICY_EXPORT", bundlePath, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	h.srv.mu.Lock()
	require.True(t, h.srv.policies[subject.FromSandboxUID(policyTestUID)].sealed)
	h.srv.mu.Unlock()

	// Restart: fresh server over the same persistence state, then the
	// binding replay (non-removal). Must not deadlock; the seal and the
	// default-deny pending come back.
	srv2, reg2, _ := fastSandboxTestServer(t)
	pp2, err := newPolicyPersistence(h.dir)
	require.NoError(t, err)
	pp2.newFile = h.pp.newFile
	srv2.SetPolicyFiles(pp2)

	done := make(chan struct{})
	go func() {
		defer close(done)
		r := doAction(t, srv2, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 1, strPtr(policyAllow())))
		assert.Equal(t, http.StatusOK, r.Code)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: the sealed file-less replay never completed")
	}

	srv2.mu.Lock()
	e := srv2.policies[subject.FromSandboxUID(policyTestUID)]
	srv2.mu.Unlock()
	require.NotNil(t, e)
	assert.True(t, e.sealed, "the seal is restored from the marker")
	rec = doPolicy(t, srv2, http.MethodPut, policyTestUID, policyPutDoc(policyAllow(), 0, false, false), "")
	assert.Equal(t, http.StatusConflict, rec.Code, "relay ops stay 409 while sealed")
	_ = reg2
}

// TestTemplateCloneAdoptsPolicyAndVault (issue #2312 acceptance 2): the
// template's policy and vault records are adopted at the clone's first
// registration — re-stamped to the clone's fence, revision preserved, vault
// validated against the LANDED template policy (not the bootstrap input),
// and enforced in-guest.
func TestTemplateCloneAdoptsPolicyAndVault(t *testing.T) {
	h := newPolicyPersistHarness(t)

	// Template source: allow a.com + b.com, with a vault binding on a.com.
	h.readySubject(t, "template-u", vaultTestIP)
	rec := doPolicy(t, h.srv, http.MethodPut, "template-u", policyPutDoc(
		`{"defaultAction":"deny","egress":[{"action":"allow","target":"a.com"},{"action":"allow","target":"api.example.com"}]}`, 1, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doVault(t, h.srv, http.MethodPost, "template-u", vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)

	// Template carry: seal-free exports of both states.
	bundle := filepath.Join(t.TempDir(), "bundle")

	require.NoError(t, os.MkdirAll(bundle, 0o700))
	sealFree := false
	rec = doAction(t, h.srv, policySnapshotBody(t, "template-u", testFenceR, testFenceA, "POLICY_EXPORT", filepath.Join(bundle, "policy.json"), &sealFree))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, policySnapshotBody(t, "template-u", testFenceR, testFenceA, "VAULT_EXPORT", filepath.Join(bundle, "vault.json"), &sealFree))
	require.Equal(t, http.StatusOK, rec.Code)

	// The source keeps full CRUD (seal-free).
	rec = doPolicy(t, h.srv, http.MethodGet, "template-u", "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doVault(t, h.srv, http.MethodGet, "template-u", "", "")
	require.Equal(t, http.StatusOK, rec.Code)

	// The clone: fresh subject on a new fence; the driver hands both
	// bundle files over strictly before SET_BINDING. The imports re-stamp
	// the records to the clone's fence for the next registration.
	const cloneUID, cloneIP = "clone-u", "10.9.2.5"
	const cloneR, cloneA = "clone-runtime", "clone-attachment"
	// Materialize the published bundle (the driver uploads the staged
	// files; the restore side pulls them back to disk).
	for _, name := range []string{"policy.json", "vault.json"} {
		staged := h.fakeForPath(filepath.Join(bundle, name))
		require.True(t, staged.exists, "the template export must have staged %s", name)
		require.NoError(t, os.WriteFile(filepath.Join(bundle, name), staged.data, 0o600))
	}
	rec = doAction(t, h.srv, policySnapshotBody(t, cloneUID, cloneR, cloneA, "POLICY_IMPORT", filepath.Join(bundle, "policy.json"), nil))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, policySnapshotBody(t, cloneUID, cloneR, cloneA, "VAULT_IMPORT", filepath.Join(bundle, "vault.json"), nil))
	require.Equal(t, http.StatusOK, rec.Code)

	// First registration: the bootstrap input (default-allow) is
	// superseded by the landed template policy; the vault reload validates
	// against that LANDED policy, so the clone adopts both.
	rec = doAction(t, h.srv, bindBody(t, cloneUID, cloneIP, cloneR, cloneA, 1, strPtr(`{"defaultAction":"allow"}`)))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, cloneUID, cloneR, cloneA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, cloneUID, cloneR, cloneA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)

	clone := subject.FromSandboxUID(cloneUID)
	assert.Equal(t, "allow", reg_UserEvaluate(h.srv, clone, "a.com"), "the template policy is enforced")
	assert.Equal(t, "deny", reg_UserEvaluate(h.srv, clone, "c.com"))

	// The clone's vault is live and injected from the adopted record.
	req := httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp="+cloneIP, nil)
	rec2 := httptest.NewRecorder()
	h.srv.handleCredentialVaultActive(rec2, req)
	require.Equal(t, http.StatusOK, rec2.Code)
	assert.Contains(t, rec2.Body.String(), "Bearer sekret")

	// The adopted record is authoritative over later binding replays.
	rec = doAction(t, h.srv, bindBody(t, cloneUID, cloneIP, cloneR, cloneA, 2, strPtr(`{"defaultAction":"allow"}`)))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doPolicy(t, h.srv, http.MethodGet, cloneUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(2), rev, "the landed revision is preserved through adoption")
	assert.Contains(t, string(doc.Policy), "a.com")
}

// ---------------------------------------------------------------------------
// Capability + healthy posture
// ---------------------------------------------------------------------------

func TestPolicyPersistCapabilityFlag(t *testing.T) {
	// Policy persistence is required in the profile: every server advertises
	// the capability.
	h := newPolicyPersistHarness(t)
	status := mustStatus(t, h.srv)
	assert.True(t, status.PolicyTransfer, "policy persistence configured: the Fastlet's transfer capability")

	srv, _, _ := fastSandboxTestServer(t)
	status = mustStatus(t, srv)
	assert.True(t, status.PolicyTransfer)
	assert.True(t, status.TemplateStateExport, "seal-free template carry + land-for-next-registration are supported")
	assert.NotContains(t, mustStatusBodyJSON(t, srv), "policyRecovery", "healthy posture omits the recovery flag")
}

// mustStatusBodyJSON returns the raw actions status body.
func mustStatusBodyJSON(t *testing.T, srv *fastSandboxPolicyServer) string {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, constants.ActionsStatusPath, "")
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}
