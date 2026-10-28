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

	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// Policy snapshot transfer across pause/resume. The driver side is platform
// work — these tests drive the egress hooks via the actions protocol. The
// shared snapshot fence constants (snapshotResumeR/A) live in the vault
// snapshot tests.

// ---------------------------------------------------------------------------
// Export: copy + seal
// ---------------------------------------------------------------------------

func TestPolicySnapshotExportCopiesAndSeals(t *testing.T) {
	h := newPolicyPersistHarness(t)
	h.readySubject(t, policyTestUID, vaultTestIP)
	bundlePath := filepath.Join(t.TempDir(), "bundle", policyTestUID+".json")

	rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, testFenceR, testFenceA, "POLICY_EXPORT", bundlePath, nil))
	require.Equal(t, http.StatusOK, rec.Code)

	// The bundle copy is byte-identical to the subject's policy file.
	bundle := h.fakeForPath(bundlePath)
	policyFile := h.fileFor(policyTestUID)
	require.True(t, bundle.exists, "export must publish the bundle file")
	require.True(t, policyFile.exists)
	assert.Equal(t, policyFile.data, bundle.data)

	// Sealed: every relay op is a 409 — the bundle copy is already taken.
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := doPolicy(t, h.srv, method, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), "")
		assert.Equal(t, http.StatusConflict, rec.Code, method)
	}
	// Policy enforcement is unchanged while the sandbox is paused.
	state, ok := h.reg.Get(subject.FromSandboxUID(policyTestUID))
	require.True(t, ok)
	assert.Equal(t, subject.StateActive, state)

	// A retried export (lost ack) re-copies and re-seals idempotently.
	rec = doAction(t, h.srv, policySnapshotBody(t, policyTestUID, testFenceR, testFenceA, "POLICY_EXPORT", bundlePath, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, policyFile.data, h.fakeForPath(bundlePath).data)
}

func TestPolicySnapshotExportWithoutPolicyFileStillSeals(t *testing.T) {
	h := newPolicyPersistHarness(t)

	// A registered subject with NO recorded policy (binding removed): the
	// removal purged the file, so the export carries nothing but still seals.
	rec := doAction(t, h.srv, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 1, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 2, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.False(t, h.fileFor(policyTestUID).exists, "precondition: the removal purged the file")

	bundlePath := filepath.Join(t.TempDir(), "bundle", policyTestUID+".json")
	rec = doAction(t, h.srv, policySnapshotBody(t, policyTestUID, testFenceR, testFenceA, "POLICY_EXPORT", bundlePath, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, h.fakeForPath(bundlePath).exists, "nothing to carry: no policy file, no bundle write")
}

// An unregistered subject has no policy to carry: the export is a 200 no-op.
func TestPolicySnapshotExportUnregisteredSubjectIsNoOp(t *testing.T) {
	h := newPolicyPersistHarness(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", "unregistered.json")

	rec := doAction(t, h.srv, policySnapshotBody(t, "unregistered-sandbox", testFenceR, testFenceA, "POLICY_EXPORT", bundlePath, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, h.fakeForPath(bundlePath).exists, "nothing to carry: no subject, no bundle write")
}

func TestPolicySnapshotExportGuards(t *testing.T) {
	t.Run("stale fence", func(t *testing.T) {
		h := newPolicyPersistHarness(t)
		h.readySubject(t, policyTestUID, vaultTestIP)
		rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, "old-runtime", testFenceA, "POLICY_EXPORT", "/tmp/bundle.json", nil))
		assert.Equal(t, http.StatusConflict, rec.Code)
		h.srv.mu.Lock()
		e := h.srv.policies[subject.FromSandboxUID(policyTestUID)]
		h.srv.mu.Unlock()
		assert.False(t, e.sealed)
	})
	t.Run("recovery latch rejects the export", func(t *testing.T) {
		h := newPolicyPersistHarness(t)
		h.readySubject(t, policyTestUID, vaultTestIP)
		h.srv.requirePolicyRecoveryLocked("simulated latch")
		bundlePath := filepath.Join(t.TempDir(), "bundle", policyTestUID+".json")

		rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, testFenceR, testFenceA, "POLICY_EXPORT", bundlePath, nil))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		h.srv.mu.Lock()
		e := h.srv.policies[subject.FromSandboxUID(policyTestUID)]
		h.srv.mu.Unlock()
		assert.False(t, e.sealed)
		assert.False(t, h.fakeForPath(bundlePath).exists, "no bundle copy while the latch holds")
	})
	t.Run("target under the policy persistence directory", func(t *testing.T) {
		h := newPolicyPersistHarness(t)
		h.readySubject(t, policyTestUID, vaultTestIP)
		inside := filepath.Join(h.dir, "other.json") // another subject's file

		rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, testFenceR, testFenceA, "POLICY_EXPORT", inside, nil))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		h.srv.mu.Lock()
		e := h.srv.policies[subject.FromSandboxUID(policyTestUID)]
		h.srv.mu.Unlock()
		assert.False(t, e.sealed)
	})
	t.Run("concurrent relay write abandons the seal", func(t *testing.T) {
		h := newPolicyPersistHarness(t)
		h.readySubject(t, policyTestUID, vaultTestIP)
		bundlePath := filepath.Join(t.TempDir(), "bundle", policyTestUID+".json")
		copied := h.fileFor(policyTestUID).data

		// A relay write lands mid-copy: the copy is stale, phase 3 refuses
		// to seal.
		newer, err := policy.EncodePolicyFile(testFenceR, testFenceA, 2, json.RawMessage(policyAllowAlt()))
		require.NoError(t, err)
		bundle := h.fakeForPath(bundlePath)
		bundle.onSave = func() {
			f := h.fileFor(policyTestUID)
			f.data = newer
			f.exists = true
		}

		rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, testFenceR, testFenceA, "POLICY_EXPORT", bundlePath, nil))
		assert.Equal(t, http.StatusConflict, rec.Code)
		h.srv.mu.Lock()
		e := h.srv.policies[subject.FromSandboxUID(policyTestUID)]
		h.srv.mu.Unlock()
		assert.False(t, e.sealed)
		assert.Equal(t, copied, bundle.data, "the stale copy exists but is not sealed — the driver retries against the newer state")
	})
}

// TestPolicySnapshotExportSealFree (issue #2312, Seal=false): the template
// carry copies the record verbatim and the source keeps full relay CRUD —
// no seal latch, revision bookkeeping unchanged.
func TestPolicySnapshotExportSealFree(t *testing.T) {
	h := newPolicyPersistHarness(t)
	h.readySubject(t, policyTestUID, vaultTestIP)
	bundlePath := filepath.Join(t.TempDir(), "bundle", policyTestUID+".json")
	sealFree := false

	before := h.fakeForPath(filepath.Join(h.dir, policyTestUID+".json")).data
	rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, testFenceR, testFenceA, "POLICY_EXPORT", bundlePath, &sealFree))
	require.Equal(t, http.StatusOK, rec.Code)

	// Byte-identical copy of the persisted record.
	bundle := h.fakeForPath(bundlePath)
	require.True(t, bundle.exists)
	assert.Equal(t, before, bundle.data)

	// The source keeps full relay CRUD — no 409, no seal latch.
	rec = doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(1), rev)
	assert.Contains(t, string(doc.Policy), "api.example.com")
	rec = doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code, "the source keeps CRUD after a seal-free export")
	rec = doPolicy(t, h.srv, http.MethodDelete, policyTestUID, "", "")
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// ---------------------------------------------------------------------------
// Import: re-stamp + commit protocol
// ---------------------------------------------------------------------------

func TestPolicySnapshotImportLandsFileBeforeRegistration(t *testing.T) {
	h := newPolicyPersistHarness(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", policyTestUID+".json")
	oldBytes := h.seedPolicyBundle(t, bundlePath, 5, policyAllowAlt())

	// Resume on a fresh Pod: no subject registered yet; the driver imports
	// before SET_BINDING.
	rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, snapshotResumeR, snapshotResumeA, "POLICY_IMPORT", bundlePath, nil))
	require.Equal(t, http.StatusOK, rec.Code)

	// Landed via the commit protocol, re-stamped with the restored fence —
	// the only authorized incarnation transfer.
	pf, body := decodePolicyFile(t, h.fileFor(policyTestUID))
	assert.Equal(t, 1, pf.Version)
	assert.Equal(t, snapshotResumeR, pf.Incarnation)
	assert.Equal(t, snapshotResumeA, pf.AttachmentID)
	assert.Equal(t, int64(5), pf.Revision)
	assert.Contains(t, string(body), "other.example.com")
	carried, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	assert.Equal(t, oldBytes, carried, "the bundle source is read, never consumed")

	// No memory record before registration (the reload happens at
	// SET_BINDING, fenced by the same identity).
	h.srv.mu.Lock()
	_, live := h.srv.policies[subject.FromSandboxUID(policyTestUID)]
	h.srv.mu.Unlock()
	assert.False(t, live)

	// At registration the file reloads under the restored fence and
	// supersedes the binding input; data-plane-ready activates it.
	rec = doAction(t, h.srv, bindBody(t, policyTestUID, vaultTestIP, snapshotResumeR, snapshotResumeA, 1, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, policyTestUID, snapshotResumeR, snapshotResumeA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(5), rev)
	assert.Contains(t, string(doc.Policy), "other.example.com")
	rec = doAction(t, h.srv, hookBody(t, policyTestUID, snapshotResumeR, snapshotResumeA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	s := subject.FromSandboxUID(policyTestUID)
	assert.Equal(t, "allow", reg_UserEvaluate(h.srv, s, "other.example.com"))
	assert.Equal(t, "deny", reg_UserEvaluate(h.srv, s, "api.example.com"), "the bundle policy, not the bootstrap input, is enforced")
}

func TestPolicySnapshotImportAfterSetBindingInstallsAndUnseals(t *testing.T) {
	h := newPolicyPersistHarness(t)
	h.readySubject(t, policyTestUID, vaultTestIP)
	bundlePath := filepath.Join(t.TempDir(), "bundle", policyTestUID+".json")
	h.seedPolicyBundle(t, bundlePath, 5, policyAllowAlt())

	// Export seals the subject (simulated pause), then resume on the same
	// fence: the import lands, installs, and unseals.
	rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, testFenceR, testFenceA, "POLICY_EXPORT", bundlePath, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	h.srv.mu.Lock()
	e := h.srv.policies[subject.FromSandboxUID(policyTestUID)]
	require.True(t, e.sealed)
	h.srv.mu.Unlock()

	// The bundle carries revision 1 (seeded state) — rewrite it to revision
	// 5 to prove the import wins over the live revision 1.
	h.seedPolicyBundle(t, bundlePath, 5, policyAllowAlt())
	rec = doAction(t, h.srv, policySnapshotBody(t, policyTestUID, testFenceR, testFenceA, "POLICY_IMPORT", bundlePath, nil))
	require.Equal(t, http.StatusOK, rec.Code)

	// The imported policy is live and unsealed.
	rec = doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(5), rev)
	assert.Contains(t, string(doc.Policy), "other.example.com")
	h.srv.mu.Lock()
	e = h.srv.policies[subject.FromSandboxUID(policyTestUID)]
	h.srv.mu.Unlock()
	assert.False(t, e.sealed, "the import unseals the subject")

	// A relay write on top lands the successor revision.
	rec = doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllow(), 5, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code)
	pf, _ := decodePolicyFile(t, h.fileFor(policyTestUID))
	assert.Equal(t, int64(6), pf.Revision)
}

func TestPolicySnapshotImportRejects(t *testing.T) {
	newHarness := func(t *testing.T) (*policyPersistHarness, string) {
		h := newPolicyPersistHarness(t)
		return h, filepath.Join(t.TempDir(), "bundle", policyTestUID+".json")
	}
	t.Run("missing bundle file", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, snapshotResumeR, snapshotResumeA, "POLICY_IMPORT", bundlePath, nil))
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
	t.Run("corrupt bundle", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(bundlePath), 0o700))
		require.NoError(t, os.WriteFile(bundlePath, []byte(`{"v":1,`), 0o600))
		rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, snapshotResumeR, snapshotResumeA, "POLICY_IMPORT", bundlePath, nil))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("invalid policy content", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		raw, err := policy.EncodePolicyFile(testFenceR, testFenceA, 5, json.RawMessage(`{"egress":"not-a-list"}`))
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(bundlePath), 0o700))
		require.NoError(t, os.WriteFile(bundlePath, raw, 0o600))
		rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, snapshotResumeR, snapshotResumeA, "POLICY_IMPORT", bundlePath, nil))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("oversized bundle", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(bundlePath), 0o700))
		require.NoError(t, os.WriteFile(bundlePath, make([]byte, maxPolicySnapshotBytes+1), 0o600))
		rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, snapshotResumeR, snapshotResumeA, "POLICY_IMPORT", bundlePath, nil))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("source under the policy persistence directory", func(t *testing.T) {
		h, _ := newHarness(t)
		inside := filepath.Join(h.dir, "staged.json")
		rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, snapshotResumeR, snapshotResumeA, "POLICY_IMPORT", inside, nil))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("stale fence while registered", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		h.readySubject(t, policyTestUID, vaultTestIP)
		h.seedPolicyBundle(t, bundlePath, 5, policyAllowAlt())
		rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, snapshotResumeR, snapshotResumeA, "POLICY_IMPORT", bundlePath, nil))
		assert.Equal(t, http.StatusConflict, rec.Code)
	})
	t.Run("recovery latch rejects the import", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		h.seedPolicyBundle(t, bundlePath, 5, policyAllowAlt())
		h.srv.requirePolicyRecoveryLocked("simulated latch")
		rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, snapshotResumeR, snapshotResumeA, "POLICY_IMPORT", bundlePath, nil))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.False(t, h.fileFor(policyTestUID).exists, "no policy file mutation while latched")
	})
}

func TestPolicySnapshotDuplicateImportDoesNotRollBack(t *testing.T) {
	h := newPolicyPersistHarness(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", policyTestUID+".json")
	h.seedPolicyBundle(t, bundlePath, 5, policyAllowAlt())

	// First import lands revision 5 (registered, denying).
	rec := doAction(t, h.srv, bindBody(t, policyTestUID, vaultTestIP, snapshotResumeR, snapshotResumeA, 1, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, policySnapshotBody(t, policyTestUID, snapshotResumeR, snapshotResumeA, "POLICY_IMPORT", bundlePath, nil))
	require.Equal(t, http.StatusOK, rec.Code)

	// Relay writes resume: revision 6 lands.
	rec = doPolicy(t, h.srv, http.MethodPut, policyTestUID, policyPutDoc(policyAllow(), 5, false, true), "")
	require.Equal(t, http.StatusOK, rec.Code)

	// A lost-ack retry must not roll revision 6 back to the bundle's 5.
	rec = doAction(t, h.srv, policySnapshotBody(t, policyTestUID, snapshotResumeR, snapshotResumeA, "POLICY_IMPORT", bundlePath, nil))
	require.Equal(t, http.StatusOK, rec.Code)

	pf, _ := decodePolicyFile(t, h.fileFor(policyTestUID))
	assert.Equal(t, int64(6), pf.Revision, "the newer relay write survives the retried import")
	rec = doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
	rev, doc := mustPolicyRevision(t, rec)
	assert.Equal(t, int64(6), rev)
	assert.Contains(t, string(doc.Policy), "api.example.com")
}

// TestPolicySnapshotRemoveBindingPurgesSealed: terminal cleanup removes the
// record and the seal marker; the bundle copy survives (export strictly
// precedes the teardown).
func TestPolicySnapshotRemoveBindingPurgesSealed(t *testing.T) {
	h := newPolicyPersistHarness(t)
	h.readySubject(t, policyTestUID, vaultTestIP)
	bundlePath := filepath.Join(t.TempDir(), "bundle", policyTestUID+".json")

	rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, testFenceR, testFenceA, "POLICY_EXPORT", bundlePath, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, h.fakeForPath(bundlePath).exists, "precondition: the bundle copy is intact")

	rec = doAction(t, h.srv, removeBody(t, policyTestUID, testFenceR, testFenceA))
	require.Equal(t, http.StatusOK, rec.Code)

	h.srv.mu.Lock()
	_, live := h.srv.policies[subject.FromSandboxUID(policyTestUID)]
	h.srv.mu.Unlock()
	assert.False(t, live, "REMOVE_BINDING purges the record")
	assert.False(t, h.fileFor(policyTestUID).exists, "REMOVE_BINDING purges the policy file")
	assert.True(t, h.fakeForPath(bundlePath).exists, "the bundle copy is untouched by the teardown")

	// The seal marker is gone with the purge.
	subj := subject.FromSandboxUID(policyTestUID)
	marker, err := h.pp.markerFor(subj)
	require.NoError(t, err)
	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr), "the purge removes the seal marker")

	// A later registration comes back unsealed from its own input.
	rec = doAction(t, h.srv, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 2, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	h.srv.mu.Lock()
	e := h.srv.policies[subj]
	h.srv.mu.Unlock()
	assert.False(t, e.sealed)
	rec = doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "")
	_, doc := mustPolicyRevision(t, rec)
	assert.Contains(t, string(doc.Policy), "api.example.com")
}

// TestPolicySnapshotSealSurvivesRestart: a handler restart between export
// and REMOVE_BINDING restores the sealed record from the durable marker.
func TestPolicySnapshotSealSurvivesRestart(t *testing.T) {
	h := newPolicyPersistHarness(t)
	h.readySubject(t, policyTestUID, vaultTestIP)
	bundlePath := filepath.Join(t.TempDir(), "bundle", policyTestUID+".json")

	rec := doAction(t, h.srv, policySnapshotBody(t, policyTestUID, testFenceR, testFenceA, "POLICY_EXPORT", bundlePath, nil))
	require.Equal(t, http.StatusOK, rec.Code)

	// Fresh server over the same persistence state + binding replay.
	srv2, _, _ := fastSandboxTestServer(t)
	pp2, err := newPolicyPersistence(h.dir)
	require.NoError(t, err)
	pp2.newFile = h.pp.newFile
	srv2.SetPolicyFiles(pp2)
	subj := subject.FromSandboxUID(policyTestUID)

	rec = doAction(t, srv2, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 1, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	srv2.mu.Lock()
	e := srv2.policies[subj]
	srv2.mu.Unlock()
	require.True(t, e.sealed, "binding replay restores the seal from the marker")
	// The recorded policy reloads from the file (relay GET is 409 while
	// sealed, so the file is the observable).
	pf, body := decodePolicyFile(t, h.fileFor(policyTestUID))
	assert.Equal(t, int64(1), pf.Revision)
	assert.Contains(t, string(body), "api.example.com")

	// Relay ops stay 409 while sealed; REMOVE_BINDING purges everything.
	rec = doPolicy(t, srv2, http.MethodPut, policyTestUID, policyPutDoc(policyAllowAlt(), 1, false, true), "")
	assert.Equal(t, http.StatusConflict, rec.Code)

	rec = doAction(t, srv2, removeBody(t, policyTestUID, testFenceR, testFenceA))
	require.Equal(t, http.StatusOK, rec.Code)
	marker, err := pp2.markerFor(subj)
	require.NoError(t, err)
	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr), "the purge removes the seal marker")

	rec = doAction(t, srv2, bindBody(t, policyTestUID, vaultTestIP, testFenceR, testFenceA, 2, strPtr(policyAllow())))
	require.Equal(t, http.StatusOK, rec.Code)
	srv2.mu.Lock()
	e = srv2.policies[subj]
	srv2.mu.Unlock()
	assert.False(t, e.sealed)
}
