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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// Vault persistence (OSEP-0022 stage 2). Outcomes are substituted at the
// vaultFileStore seam so every classification is reachable on any platform.

// fakeVaultFile is a controllable vaultFileStore. Its data/exists pair IS the
// simulated disk state; the save/delete/restore fields inject outcomes.
// newFakeVaultFile defaults every outcome to FileCommitted (healthy store).
type fakeVaultFile struct {
	path   string
	data   []byte
	exists bool

	snapErr error

	saveState policy.FileMutationState
	saveErr   error
	saveCalls int
	lastSaved []byte
	// onSave fires when SaveRaw starts — simulate a concurrent mutation of
	// another file mid-write.
	onSave      func()
	deleteState policy.FileMutationState
	deleteErr   error
	// failFirstDelete injects exactly one failed (uncertain) Delete — the
	// purge outcome a crash can leave behind — and then behaves healthy.
	failFirstDelete error
	deleteCalls     int

	restoreState policy.FileMutationState
	restoreErr   error
	restoreCalls int
	restored     []*policy.PolicyFileSnapshot
}

func newFakeVaultFile(path string) *fakeVaultFile {
	return &fakeVaultFile{
		path:         path,
		saveState:    policy.FileCommitted,
		deleteState:  policy.FileCommitted,
		restoreState: policy.FileCommitted,
	}
}

func (f *fakeVaultFile) Snapshot() (*policy.PolicyFileSnapshot, error) {
	f.saveCallsSnapshotGuard()
	if f.snapErr != nil {
		return nil, f.snapErr
	}
	return policy.NewFileSnapshot(f.path, f.data, f.exists), nil
}

func (f *fakeVaultFile) saveCallsSnapshotGuard() {}

func (f *fakeVaultFile) SaveRaw(data []byte) (policy.FileMutationState, error) {
	f.saveCalls++
	f.lastSaved = append([]byte(nil), data...)
	if f.onSave != nil {
		f.onSave()
	}
	if f.saveErr != nil {
		return f.saveState, f.saveErr
	}
	f.data = append([]byte(nil), data...)
	f.exists = true
	return f.saveState, nil
}

func (f *fakeVaultFile) SaveRawNonDurable(data []byte) (policy.FileMutationState, error) {
	// Same observable effect as SaveRaw in the fake: the durability
	// difference lives in the real atomic store's fsync behavior.
	return f.SaveRaw(data)
}

func (f *fakeVaultFile) Restore(s *policy.PolicyFileSnapshot) (policy.FileMutationState, error) {
	f.restoreCalls++
	f.restored = append(f.restored, s)
	if f.restoreErr != nil {
		return f.restoreState, f.restoreErr
	}
	if s.Exists() {
		f.data = append([]byte(nil), s.Data()...)
		f.exists = true
	} else {
		f.data, f.exists = nil, false
	}
	return f.restoreState, nil
}

func (f *fakeVaultFile) Delete() (policy.FileMutationState, error) {
	f.deleteCalls++
	if f.failFirstDelete != nil {
		err := f.failFirstDelete
		f.failFirstDelete = nil
		return policy.FileUnknown, err
	}
	if f.deleteErr != nil {
		return f.deleteState, f.deleteErr
	}
	f.data, f.exists = nil, false
	return f.deleteState, nil
}

// vaultPersistHarness wires a fast-sandbox test server to a fake-backed
// vaultPersistence over a real (swept) temp directory.
type vaultPersistHarness struct {
	srv        *fastSandboxPolicyServer
	reg        *subject.MemoryRegistry
	nft        *fakeNft
	vp         *vaultPersistence
	dir        string
	files      map[string]*fakeVaultFile
	mapMu      sync.Mutex
	newFileErr error
}

func newVaultPersistHarness(t *testing.T) *vaultPersistHarness {
	t.Helper()
	dir := t.TempDir()
	h := &vaultPersistHarness{
		files: make(map[string]*fakeVaultFile),
		dir:   dir,
	}
	h.srv, h.reg, h.nft = fastSandboxTestServer(t)
	h.vp = newTestVaultPersistence(t, h)
	h.srv.SetVaultFiles(h.vp)
	return h
}

func newTestVaultPersistence(t *testing.T, h *vaultPersistHarness) *vaultPersistence {
	t.Helper()
	vp, err := newVaultPersistence(h.dir)
	require.NoError(t, err)
	vp.newFile = func(path string) (vaultFileStore, error) {
		if h.newFileErr != nil {
			err := h.newFileErr
			h.newFileErr = nil
			return nil, err
		}
		h.mapMu.Lock()
		defer h.mapMu.Unlock()
		f, ok := h.files[path]
		if !ok {
			f = newFakeVaultFile(path)
			h.files[path] = f
		}
		return f, nil
	}
	return vp
}

func (h *vaultPersistHarness) fileFor(uid string) *fakeVaultFile {
	h.mapMu.Lock()
	defer h.mapMu.Unlock()
	path := filepath.Join(h.dir, uid+".json")
	f, ok := h.files[path]
	if !ok {
		f = newFakeVaultFile(path)
		h.files[path] = f
	}
	return f
}

// seedVaultFile writes simulated committed file bytes before registration.
func (h *vaultPersistHarness) seedVaultFile(uid, incarnation, attachment string, revision int64, body string) {
	h.fileFor(uid).data = []byte(fmt.Sprintf(
		`{"v":1,"incarnation":%q,"attachmentId":%q,"revision":%d,"vault":%s}`+"\n", incarnation, attachment, revision, body))
	h.fileFor(uid).exists = true
}

// readySubject drives SET_BINDING + both hooks under the default test fence.
func (h *vaultPersistHarness) readySubject(t *testing.T, uid, ip string) subject.Subject {
	t.Helper()
	s := subject.FromSandboxUID(uid)
	rec := doAction(t, h.srv, bindBody(t, uid, ip, testFenceR, testFenceA, 1, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, uid, testFenceR, testFenceA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, uid, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	return s
}

// decodeFile decodes the simulated file bytes of a subject.
func decodeFile(t *testing.T, f *fakeVaultFile) (credentialvault.VaultFile, credentialvault.VaultFileBody) {
	t.Helper()
	require.True(t, f.exists, "the vault file must exist")
	vf, body, err := credentialvault.DecodeVaultFile(f.data)
	require.NoError(t, err)
	return vf, body
}

// ---------------------------------------------------------------------------
// Commit protocol: persist → apply → ack (three-state classification)
// ---------------------------------------------------------------------------

func TestVaultPersistWriteStampsFileAndApplies(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, vaultTestUID, vaultTestIP)

	rec := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)

	// The file is stamped with the owning runtime fence and the exact
	// applied revision.
	vf, body := decodeFile(t, h.fileFor(vaultTestUID))
	assert.Equal(t, 1, vf.Version)
	assert.Equal(t, testFenceR, vf.Incarnation)
	assert.Equal(t, testFenceA, vf.AttachmentID)
	assert.Equal(t, int64(1), vf.Revision)
	require.Len(t, body.Credentials, 1)
	require.Len(t, body.Bindings, 1)

	// Applied: the live store serves the revision.
	rec = doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":1`)

	// A replace persists the successor revision.
	rec = doVault(t, h.srv, http.MethodPut, vaultTestUID, vaultPut("1"), "")
	require.Equal(t, http.StatusOK, rec.Code)
	vf, _ = decodeFile(t, h.fileFor(vaultTestUID))
	assert.Equal(t, int64(2), vf.Revision)

	// DELETE removes the file (acked ⇒ durable).
	rec = doVault(t, h.srv, http.MethodDelete, vaultTestUID, "", "")
	require.Equal(t, http.StatusNoContent, rec.Code)
	f := h.fileFor(vaultTestUID)
	assert.False(t, f.exists, "a deleted vault leaves no file behind")
}

func TestVaultPersistUnchangedIsSafeNackWithoutLatch(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, vaultTestUID, vaultTestIP)

	// Last-good state on disk and in memory.
	rec := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)

	// A pre-rename failure (temp create/write fails): FileUnchanged.
	f := h.fileFor(vaultTestUID)
	f.saveState, f.saveErr = policy.FileUnchanged, errors.New("no space on temp create")
	rec = doVault(t, h.srv, http.MethodPut, vaultTestUID, vaultPut("1"), "")
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	// Last-good kept on both sides; no restore was attempted.
	assert.Equal(t, int64(1), mustRevision(t, doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")))
	assert.Equal(t, 0, f.restoreCalls)
	assert.Equal(t, 2, f.saveCalls, "the initial create plus exactly one failed attempt")

	// No latch: the next write with a working store succeeds.
	f.saveErr = nil
	rec = doVault(t, h.srv, http.MethodPut, vaultTestUID, vaultPut("1"), "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, http.StatusOK, doRequest(t, h.srv, http.MethodGet, "/healthz", "").Code)
}

func TestVaultPersistUnknownLatchesAndRestores(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, vaultTestUID, vaultTestIP)

	rec := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)
	goodFile := append([]byte(nil), h.fileFor(vaultTestUID).data...)

	// Rename attempted, durability unconfirmed: FileUnknown.
	f := h.fileFor(vaultTestUID)
	f.saveState, f.saveErr = policy.FileUnknown, errors.New("rename returned EIO")
	rec = doVault(t, h.srv, http.MethodPut, vaultTestUID, vaultPut("1"), "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	// The previous file image was restored (best-effort recovery), and the
	// live state was never mutated.
	assert.Equal(t, 1, f.restoreCalls)
	require.NotEmpty(t, f.restored)
	assert.Equal(t, string(goodFile), string(f.restored[0].Data()), "restore receives the exact previous image")
	assert.Equal(t, string(goodFile), string(f.data))
	assert.Equal(t, int64(1), mustRevision(t, doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")))

	// The recovery latch freezes every vault write until reconciliation.
	rec = doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	rec = doVault(t, h.srv, http.MethodPut, vaultTestUID, vaultPut("1"), "")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	rec = doVault(t, h.srv, http.MethodDelete, vaultTestUID, "", "")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	// Surfaced: healthz 503 with the reason, status payload flags recovery.
	rec = doRequest(t, h.srv, http.MethodGet, "/healthz", "")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "vault persistence recovery required")
	status := mustStatus(t, h.srv)
	assert.True(t, status.Recovery)
	assert.False(t, status.Ready)
}

func TestVaultPersistContradictorySuccessIsUnknown(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, vaultTestUID, vaultTestIP)

	// A nil error paired with a non-committed state is contradictory and
	// must not be trusted as a committed write.
	f := h.fileFor(vaultTestUID)
	f.saveState = policy.FileUnknown
	rec := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.True(t, h.srv.vaultRecoveryActive())
	assert.Equal(t, 1, f.restoreCalls)
}

func TestVaultPersistApplyFailureRollsBackFile(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, vaultTestUID, vaultTestIP)

	rec := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)
	goodFile := append([]byte(nil), h.fileFor(vaultTestUID).data...)

	// Crash between persist and apply: the durable write lands, the live
	// apply fails → the previous file is restored (last-good preserved on
	// both sides), no latch (the restore is confirmed).
	f := h.fileFor(vaultTestUID)
	srv := h.srv
	srv.mu.Lock()
	err := srv.persistVaultCandidateLocked(
		subject.FromSandboxUID(vaultTestUID),
		func(fs vaultFileStore) (policy.FileMutationState, error) {
			return fs.SaveRaw([]byte(`{"v":1,"incarnation":"x","revision":9,"vault":{}}` + "\n"))
		},
		func() error { return errors.New("apply boom") },
	)
	srv.mu.Unlock()
	require.Error(t, err)

	assert.Equal(t, 1, f.restoreCalls)
	assert.Equal(t, string(goodFile), string(f.data), "the previous file image is restored")
	assert.False(t, srv.vaultRecoveryActive(), "a confirmed restore needs no latch")
	assert.Equal(t, int64(1), mustRevision(t, doVault(t, srv, http.MethodGet, vaultTestUID, "", "")))

	// An inconclusive restore after a failed apply latches recovery.
	f.restoreErr = errors.New("restore EIO")
	srv.mu.Lock()
	err = srv.persistVaultCandidateLocked(
		subject.FromSandboxUID(vaultTestUID),
		func(fs vaultFileStore) (policy.FileMutationState, error) {
			return fs.SaveRaw([]byte(`{"v":1,"incarnation":"x","revision":9,"vault":{}}` + "\n"))
		},
		func() error { return errors.New("apply boom") },
	)
	srv.mu.Unlock()
	require.Error(t, err)
	assert.True(t, srv.vaultRecoveryActive())
}

// ---------------------------------------------------------------------------
// Reload at registration (egress-restart recovery) + incarnation fencing
// ---------------------------------------------------------------------------

const reloadVaultBody = `{"credentials":[{"name":"tok","source":{"type":"inline","value":"sekret"}}],` +
	`"bindings":[{"name":"api","match":{"hosts":["api.example.com"]},"auth":{"type":"bearer","credential":"tok"}}]}`

// Construction restricts a dedicated 0777 emptyDir volume root (an
// untrusted ancestor for the atomic machinery), leaves one holding foreign
// entries alone, and keeps already-restrictive roots as they are.
func TestVaultPersistRestrictsDedicatedVolumeRoot(t *testing.T) {
	t.Run("dedicated root is restricted", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "vault")
		require.NoError(t, os.Mkdir(dir, 0o700))
		require.NoError(t, os.Chmod(root, 0o777))
		_, err := newVaultPersistence(dir)
		require.NoError(t, err)
		info, err := os.Stat(root)
		require.NoError(t, err)
		assert.Zero(t, info.Mode().Perm()&0o022, "group/world write cleared on the dedicated volume root")
	})
	t.Run("root with foreign entries is untouched", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "vault")
		require.NoError(t, os.Mkdir(dir, 0o700))
		require.NoError(t, os.Chmod(root, 0o777))
		require.NoError(t, os.WriteFile(filepath.Join(root, "confdir"), []byte("x"), 0o600))
		_, err := newVaultPersistence(dir)
		require.NoError(t, err)
		info, err := os.Stat(root)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o777), info.Mode().Perm(), "shared volume root left for the manifest to fix")
	})
	t.Run("restrictive root is a no-op", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "vault")
		require.NoError(t, os.Mkdir(dir, 0o700))
		require.NoError(t, os.Chmod(root, 0o755))
		_, err := newVaultPersistence(dir)
		require.NoError(t, err)
		info, err := os.Stat(root)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	})
}

func TestVaultPersistReloadAtSetBinding(t *testing.T) {
	h := newVaultPersistHarness(t)

	// Committed state from the previous egress generation: revision 5 under
	// the same runtime instance.
	h.seedVaultFile(vaultTestUID, testFenceR, testFenceA, 5, reloadVaultBody)

	h.readySubject(t, vaultTestUID, vaultTestIP)

	// The reloaded vault serves the exact file revision — the continuity the
	// relay preconditions rely on.
	rec := doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":5`)
	assert.Contains(t, rec.Body.String(), "sekret")

	// Live for interception: the addon dispatch injects from the reloaded
	// slice.
	req := httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp="+vaultTestIP, nil)
	rec2 := httptest.NewRecorder()
	h.srv.handleCredentialVaultActive(rec2, req)
	require.Equal(t, http.StatusOK, rec2.Code)
	assert.Contains(t, rec2.Body.String(), "Bearer sekret")

	// Revision numbering continues from the reloaded revision.
	rec = doVault(t, h.srv, http.MethodPut, vaultTestUID, vaultPut("5"), "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":6`)
	vf, _ := decodeFile(t, h.fileFor(vaultTestUID))
	assert.Equal(t, int64(6), vf.Revision)
}

func TestVaultPersistStaleIncarnationDiscarded(t *testing.T) {
	h := newVaultPersistHarness(t)

	// A reset: the file belongs to a previous runtime instance.
	h.seedVaultFile(vaultTestUID, "runtime-0", "attachment-0", 5, reloadVaultBody)

	h.readySubject(t, vaultTestUID, vaultTestIP)

	// Stale, discarded: the successor starts empty and the file is purged —
	// old credentials never carry into the new sandbox.
	rec := doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.False(t, h.fileFor(vaultTestUID).exists, "the stale file is purged at registration")

	// The fresh vault installs cleanly.
	rec = doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)
	vf, _ := decodeFile(t, h.fileFor(vaultTestUID))
	assert.Equal(t, testFenceR, vf.Incarnation)

	// The stale incarnation cannot write into the successor.
	rec = doVault(t, h.srv, http.MethodPut, vaultTestUID, vaultPut("1"), "runtime-0")
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)
}

func TestVaultPersistCorruptFileDegradesSubject(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.fileFor(vaultTestUID).data = []byte("{corrupt json")
	h.fileFor(vaultTestUID).exists = true

	// Registration succeeds; policy enforcement is unaffected.
	s := h.readySubject(t, vaultTestUID, vaultTestIP)
	state, ok := h.reg.Get(s)
	require.True(t, ok)
	assert.Equal(t, subject.StateActive, state)
	assert.Positive(t, h.nft.appliedCount())

	// The vault slice is degraded: interception fails closed (empty slice).
	// A per-subject degradation is NOT a process-health failure — healthz
	// stays green (only the recovery latch flips it) and the actions status
	// lists the subject; flipping readiness for it would risk restart loops
	// with no in-process healing path.
	rec := doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	req := httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp="+vaultTestIP, nil)
	rec2 := httptest.NewRecorder()
	h.srv.handleCredentialVaultActive(rec2, req)
	assert.Equal(t, http.StatusNotFound, rec2.Code)

	assert.Equal(t, http.StatusOK, doRequest(t, h.srv, http.MethodGet, "/healthz", "").Code)
	status := mustStatus(t, h.srv)
	assert.Equal(t, []string{vaultTestUID}, status.DegradedVaults)
	assert.True(t, status.Ready, "a per-subject degradation never flips process readiness")

	// The unusable file is quarantined so a restart cannot re-degrade on it.
	assert.False(t, h.fileFor(vaultTestUID).exists, "a structurally corrupt file is quarantined")

	// A successful relay write clears the degradation — the subject recovers
	// without recreation.
	rec = doVault(t, h.srv, http.MethodPut, vaultTestUID, vaultFullBody(), "")
	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, http.StatusOK, doRequest(t, h.srv, http.MethodGet, "/healthz", "").Code)
	status = mustStatus(t, h.srv)
	assert.Empty(t, status.DegradedVaults)
	rec = doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":1`)
}

func TestVaultPersistUnknownVersionDegradesButPreservesFile(t *testing.T) {
	h := newVaultPersistHarness(t)
	// v1 gating: a file written by a newer consumer (v2 envelope with
	// unknown fields) must not be misread as corruption — and must NOT be
	// destroyed by this reader.
	future := `{"v":2,"kid":"k1","alg":"A256GCM","ct":"cGxhaW50ZXh0"}` + "\n"
	h.fileFor(vaultTestUID).data = []byte(future)
	h.fileFor(vaultTestUID).exists = true

	h.readySubject(t, vaultTestUID, vaultTestIP)

	status := mustStatus(t, h.srv)
	assert.Equal(t, []string{vaultTestUID}, status.DegradedVaults)
	assert.True(t, status.Ready)
	assert.Equal(t, http.StatusOK, doRequest(t, h.srv, http.MethodGet, "/healthz", "").Code)
	assert.Equal(t, future, string(h.fileFor(vaultTestUID).data), "an unknown-version file is preserved for the newer writer")
}

func TestVaultPersistReloadValidationFailureDegrades(t *testing.T) {
	h := newVaultPersistHarness(t)
	// The file's binding host is not covered by the replayed binding policy:
	// the content cannot be revalidated, so the slice DEGRADES (fail closed)
	// instead of injecting uncovered credentials — and the file is KEPT: the
	// policy/vault divergence is a normal state (runtime relay commits never
	// re-validate the vault), so quarantining would silently destroy
	// persisted credentials. A policy heal + restart, or a vault relay
	// write, revives the slice.
	uncovered := `{"credentials":[{"name":"tok","source":{"type":"inline","value":"sekret"}}],` +
		`"bindings":[{"name":"api","match":{"hosts":["other.example.com"]},"auth":{"type":"bearer","credential":"tok"}}]}`
	h.seedVaultFile(vaultTestUID, testFenceR, testFenceA, 5, uncovered)

	h.readySubject(t, vaultTestUID, vaultTestIP)

	status := mustStatus(t, h.srv)
	assert.Equal(t, []string{vaultTestUID}, status.DegradedVaults)
	assert.True(t, status.Ready)
	assert.Equal(t, http.StatusOK, doRequest(t, h.srv, http.MethodGet, "/healthz", "").Code)
	// The vault slice stays empty — interception fails closed.
	rec := doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.True(t, h.fileFor(vaultTestUID).exists, "the uncovered-but-healthy vault file is kept (no quarantine)")

	// Revival: a policy heal (the record now covers the vault's binding
	// host) plus a restart reloads the kept vault at its preserved revision.
	healed := `{"defaultAction":"deny","egress":[{"action":"allow","target":"other.example.com"}]}`
	rec = doPolicy(t, h.srv, http.MethodPut, vaultTestUID, policyPutDoc(healed, 0, false, false), "")
	require.Equal(t, http.StatusOK, rec.Code)

	srv2, _, _ := fastSandboxTestServer(t)
	vp2, err := newVaultPersistence(h.dir)
	require.NoError(t, err)
	vp2.newFile = h.vp.newFile
	srv2.SetVaultFiles(vp2)

	// The replayed binding covers the vault's binding host (other.example.com).
	rec = doAction(t, srv2, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 2,
		strPtr(`{"defaultAction":"deny","egress":[{"action":"allow","target":"other.example.com"}]}`)))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv2, hookBody(t, vaultTestUID, testFenceR, testFenceA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv2, hookBody(t, vaultTestUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = doVault(t, srv2, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":5`)
	status = mustStatus(t, srv2)
	assert.Empty(t, status.DegradedVaults)
}

func TestVaultPersistDuplicateSetBindingKeepsLiveSlice(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, vaultTestUID, vaultTestIP)

	rec := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)
	// Advance the live store past the file (rev 2 applied and persisted).
	rec = doVault(t, h.srv, http.MethodPut, vaultTestUID, vaultPut("1"), "")
	require.Equal(t, http.StatusOK, rec.Code)

	// Duplicate SET_BINDING (at-least-once delivery, same fence): the live
	// slice wins, no reload clobbers it.
	rec = doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 2, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":2`)
}

// TestVaultPersistForeignFenceEntryNeverSurvivesReset: an entry left by a
// failed registration of a previous generation must not be inherited by the
// reset's successor — the fence check at rehydration drops it together with
// the stale file.
func TestVaultPersistForeignFenceEntryNeverSurvivesReset(t *testing.T) {
	h := newVaultPersistHarness(t)

	// Failed registration under runtime-0 leaves its vault entry behind.
	rec := doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, "runtime-0", testFenceA, 1, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	h.nft.denyFirstErr = errors.New("nft down")
	rec = doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, "runtime-0", testFenceA, 2, vaultAllowPolicy()))
	require.Equal(t, http.StatusInternalServerError, rec.Code, "registration fails after a fence-less entry exists")
	h.nft.denyFirstErr = nil

	// The slice survives the failed attempt (same fence retry works).
	rec = doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "runtime-0")
	require.Equal(t, http.StatusCreated, rec.Code)
	vf, _ := decodeFile(t, h.fileFor(vaultTestUID))
	require.Equal(t, "runtime-0", vf.Incarnation)

	// Reset: a new runtime instance registers the same UID. The foreign
	// slice and its stale file are discarded — no credential carry-over.
	rec = doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 3, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, vaultTestUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
	assert.Equal(t, http.StatusNotFound, rec.Code, "the previous generation's slice never reaches the successor")
	assert.False(t, h.fileFor(vaultTestUID).exists, "the stale file is purged")
}

// ---------------------------------------------------------------------------
// Purge: REMOVE_BINDING (terminal) and rebind
// ---------------------------------------------------------------------------

func TestVaultPersistRemoveBindingPurgesFile(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, vaultTestUID, vaultTestIP)

	rec := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)
	require.True(t, h.fileFor(vaultTestUID).exists)

	// A failed best-effort cleanup is still acknowledged; an explicit later
	// REMOVE can retry it, but no automatic retry is promised.
	h.fileFor(vaultTestUID).failFirstDelete = errors.New("unlink EIO")

	rec = doAction(t, h.srv, removeBody(t, vaultTestUID, testFenceR, testFenceA))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, h.fileFor(vaultTestUID).exists, "failed cleanup leaves the old fenced file")
	assert.Equal(t, 1, h.fileFor(vaultTestUID).deleteCalls)
	rec = doAction(t, h.srv, removeBody(t, vaultTestUID, testFenceR, testFenceA))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, h.fileFor(vaultTestUID).exists)

	// REMOVE_BINDING for unknown Handler state also drops any cached file.
	h.seedVaultFile(vaultTestUID, "runtime-x", "attachment-x", 1, reloadVaultBody)
	rec = doAction(t, h.srv, removeBody(t, vaultTestUID, "runtime-x", testFenceA))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, h.fileFor(vaultTestUID).exists)
}

func TestVaultPersistRebindPurgesFile(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, vaultTestUID, vaultTestIP)

	rec := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)
	require.True(t, h.fileFor(vaultTestUID).exists)

	// Rebind: same UID, new runtime instance, no REMOVE_BINDING.
	const newRuntime = "runtime-2"
	rec = doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, newRuntime, "attachment-2", 2, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, vaultTestUID, newRuntime, "attachment-2", constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.False(t, h.fileFor(vaultTestUID).exists, "the rebind purges the previous instance's file")
}

// ---------------------------------------------------------------------------
// Startup: staged leftovers discarded, directory contract
// ---------------------------------------------------------------------------

func TestVaultPersistStartupDiscardsStagedLeftovers(t *testing.T) {
	dir := t.TempDir()
	// Staging leftovers of an interrupted atomic write, plus an unrelated
	// file and the committed file itself.
	require.NoError(t, os.WriteFile(filepath.Join(dir, policy.AtomicTempPrefix+"deadbeef"+policy.AtomicTempSuffix), []byte("candidate"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep"), 0o600))
	committed := []byte(`{"v":1,"incarnation":"rt","revision":1,"vault":{"credentials":[],"bindings":[]}}` + "\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s-1.json"), committed, 0o600))

	vp, err := newVaultPersistence(dir)
	require.NoError(t, err)
	assert.Equal(t, dir, vp.dir)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{"notes.txt", "s-1.json"}, names, "startup loads the committed file only and discards staged leftovers")

	// The committed file survives byte-exact.
	got, err := os.ReadFile(filepath.Join(dir, "s-1.json"))
	require.NoError(t, err)
	assert.Equal(t, string(committed), string(got))
}

func TestVaultPersistenceRejectsBadDirectories(t *testing.T) {
	for _, dir := range []string{"", "relative/vault", "/tmp/..", "/tmp/escape/../vault"} {
		_, err := newVaultPersistence(dir)
		assert.Error(t, err, dir)
	}

	// A symlinked vault directory is rejected before chmod/sweep act
	// through it.
	link := filepath.Join(t.TempDir(), "vault-link")
	target := t.TempDir()
	require.NoError(t, os.Symlink(target, link))
	_, err := newVaultPersistence(link)
	assert.ErrorContains(t, err, "must not be a symlink")

	// A non-directory is rejected too (MkdirAll refuses a file component).
	file := filepath.Join(t.TempDir(), "vault-file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	_, err = newVaultPersistence(file)
	assert.ErrorContains(t, err, "not a directory")
}

// ---------------------------------------------------------------------------
// Status payload + healthy posture
// ---------------------------------------------------------------------------

func TestVaultPersistHealthyStatusOmitsFields(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, vaultTestUID, vaultTestIP)

	rec := doRequest(t, h.srv, http.MethodGet, constants.ActionsStatusPath, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var status actionhandler.StatusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &status))
	assert.True(t, status.Ready)
	assert.False(t, status.Recovery)
	assert.Empty(t, status.DegradedVaults)
	// Vault persistence configured: the capability the Fastlet's
	// pause/resume vault transfer probes for.
	assert.True(t, status.VaultTransfer)
	assert.NotContains(t, rec.Body.String(), "degradedVaults", "healthy vault fields stay omitted")
	assert.Equal(t, http.StatusOK, doRequest(t, h.srv, http.MethodGet, "/healthz", "").Code)
}

// Review-fix regressions.

// TestVaultRelayStalledBodyDoesNotHoldServerLock pins the parse-before-lock
// ordering: with the parse under the lock, the probe below deadlocks.
func TestVaultRelayStalledBodyDoesNotHoldServerLock(t *testing.T) {
	srv, _, _ := fastSandboxTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	bodyPipe, bodyWriter := io.Pipe()
	stalled := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPut, ts.URL+constants.VaultRelayPathPrefix+vaultTestUID, bodyPipe)
		if err != nil {
			stalled <- err
			return
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			stalled <- err
			return
		}
		stalled <- resp.Body.Close()
	}()

	// Give the handler time to park on the body read (under the lock in the
	// buggy ordering).
	time.Sleep(150 * time.Millisecond)

	// The probe must answer while the body is stalled.
	probeClient := &http.Client{Timeout: 2 * time.Second}
	resp, err := probeClient.Get(ts.URL + constants.VaultRelayPathPrefix + vaultTestUID)
	require.NoError(t, err, "a stalled request body must never hold the server lock")
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// Release the stalled handler; the failed parse is a 400.
	require.NoError(t, bodyWriter.CloseWithError(errors.New("client stalled")))
	select {
	case <-stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled request never completed")
	}
}

// TestVaultPersistRemovalReplayDoesNotDegrade: a removal replay skips the
// reload (nothing to revalidate against); the file stays fenced on disk.
func TestVaultPersistRemovalReplayDoesNotDegrade(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.seedVaultFile(vaultTestUID, testFenceR, testFenceA, 5, reloadVaultBody)

	// Binding removal replayed after an egress restart: null input, no
	// in-memory slice.
	rec := doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 1, nil))
	require.Equal(t, http.StatusOK, rec.Code)

	status := mustStatus(t, h.srv)
	assert.Empty(t, status.DegradedVaults, "a removal replay must not degrade the subject")
	assert.True(t, status.Ready)
	assert.Equal(t, http.StatusOK, doRequest(t, h.srv, http.MethodGet, "/healthz", "").Code)
	assert.True(t, h.fileFor(vaultTestUID).exists, "the committed file stays fenced on disk")

	// The next registration with a real binding reloads the file normally.
	rec = doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 2, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":5`)
}

// TestVaultPersistAttachmentOnlyRebindDiscardsFile: an attachment-only rebind
// with an uncertain purge must find the predecessor's file stale —
// incarnation-only fencing would silently resurrect the old credentials.
func TestVaultPersistAttachmentOnlyRebindDiscardsFile(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, vaultTestUID, vaultTestIP)

	// Generation one writes its vault under (runtime-1, attachment-1).
	rec := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)
	vf, _ := decodeFile(t, h.fileFor(vaultTestUID))
	require.Equal(t, testFenceA, vf.AttachmentID)

	// Rebind: same incarnation, NEW attachment only. The eager purge outcome
	// is uncertain (the unlink may have taken effect before the error).
	h.fileFor(vaultTestUID).failFirstDelete = errors.New("unlink EIO")

	const newAttachment = "attachment-2"
	rec = doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, newAttachment, 2, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, vaultTestUID, testFenceR, newAttachment, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)

	// The predecessor's file is stale under the new attachment: discarded,
	// never loaded, no degraded mark (nothing was wrong with the file — it
	// simply belongs to a previous identity).
	rec = doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
	assert.Equal(t, http.StatusNotFound, rec.Code, "the predecessor's vault must not resurrect into the rebind")
	assert.False(t, h.fileFor(vaultTestUID).exists, "the stale file is purged at registration")
	status := mustStatus(t, h.srv)
	assert.Empty(t, status.DegradedVaults)
	assert.Equal(t, http.StatusOK, doRequest(t, h.srv, http.MethodGet, "/healthz", "").Code)

	// The successor's own vault installs cleanly under the new attachment.
	rec = doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)
	vf, _ = decodeFile(t, h.fileFor(vaultTestUID))
	assert.Equal(t, newAttachment, vf.AttachmentID)
}

// TestVaultPersistPurgeEvictsCachedFileStore: the per-subject file-store
// cache must not leak an entry per subject on a long-lived pool pod.
func TestVaultPersistPurgeEvictsCachedFileStore(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, vaultTestUID, vaultTestIP)

	rec := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)

	h.vp.mu.Lock()
	_, cached := h.vp.files[subject.FromSandboxUID(vaultTestUID)]
	h.vp.mu.Unlock()
	require.True(t, cached, "the file store is cached while the subject lives")

	rec = doAction(t, h.srv, removeBody(t, vaultTestUID, testFenceR, testFenceA))
	require.Equal(t, http.StatusOK, rec.Code)

	h.vp.mu.Lock()
	_, cached = h.vp.files[subject.FromSandboxUID(vaultTestUID)]
	h.vp.mu.Unlock()
	assert.False(t, cached, "a purged subject's file store must be evicted")
}

// TestVaultPersistFailureResponseIsGeneric: persistence failures are a
// retryable classification on the relay contract; host paths and I/O phases
// stay in the server log.
func TestVaultPersistFailureResponseIsGeneric(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, vaultTestUID, vaultTestIP)

	rec := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)

	f := h.fileFor(vaultTestUID)
	f.saveState, f.saveErr = policy.FileUnchanged, errors.New("open /var/lib/opensandbox-egress/vault/secret-host-path.json: no space left on device")
	rec = doVault(t, h.srv, http.MethodPut, vaultTestUID, vaultPut("1"), "")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "vault persistence failure\n", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "/var/lib")
	assert.NotContains(t, rec.Body.String(), "no space")
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func mustRevision(t *testing.T, rec *httptest.ResponseRecorder) int64 {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code)
	var full credentialvault.FullRevision
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &full))
	return full.Revision
}

func mustStatus(t *testing.T, srv *fastSandboxPolicyServer) actionhandler.StatusResponse {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, constants.ActionsStatusPath, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var status actionhandler.StatusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &status))
	return status
}

var _ = strings.TrimSpace
