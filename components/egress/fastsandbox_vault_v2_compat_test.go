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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

type vaultV2Fixture struct {
	Version          int                       `json:"v"`
	Incarnation      string                    `json:"incarnation"`
	AttachmentID     string                    `json:"attachmentId"`
	Revision         int64                     `json:"revision"`
	State            credentialvault.FileState `json:"state"`
	StateLineage     string                    `json:"stateLineage"`
	StateRevision    uint64                    `json:"stateRevision"`
	InitializationID string                    `json:"initializationId,omitempty"`
	Vault            json.RawMessage           `json:"vault"`
}

func encodeVaultV2Fixture(t *testing.T, state credentialvault.FileState, revision int64, stateRevision uint64, initID string, body credentialvault.VaultFileBody) []byte {
	t.Helper()
	rawBody, err := json.Marshal(body)
	require.NoError(t, err)
	raw, err := json.Marshal(vaultV2Fixture{
		Version: credentialvault.VaultFileVersion2, Incarnation: testFenceR, AttachmentID: testFenceA,
		Revision: revision, State: state, StateLineage: "lineage-test", StateRevision: stateRevision,
		InitializationID: initID, Vault: rawBody,
	})
	require.NoError(t, err)
	return append(raw, '\n')
}

func emptyVaultFileBody() credentialvault.VaultFileBody {
	return credentialvault.VaultFileBody{Credentials: []credentialvault.Credential{}, Bindings: []credentialvault.Binding{}}
}

func seedVaultV2Fixture(h *vaultPersistHarness, uid string, raw []byte) *fakeVaultFile {
	f := h.fileFor(uid)
	f.data = append([]byte(nil), raw...)
	f.exists = true
	return f
}

func TestVaultV2ReloadServesActiveButRejectsRelayWrites(t *testing.T) {
	h := newVaultPersistHarness(t)
	raw := encodeVaultV2Fixture(t, credentialvault.FileStateActive, 7, 12, "", bundleVaultBody(t))
	f := seedVaultV2Fixture(h, vaultTestUID, raw)
	h.readySubject(t, vaultTestUID, vaultTestIP)
	subj := subject.FromSandboxUID(vaultTestUID)
	h.srv.mu.Lock()
	meta := h.srv.vaults[subj].fileMeta
	h.srv.mu.Unlock()
	assert.Equal(t, credentialvault.VaultFileVersion2, meta.version)
	assert.Equal(t, int64(7), meta.revision)
	assert.Equal(t, credentialvault.FileStateActive, meta.state)
	assert.Equal(t, "lineage-test", meta.lineage)
	assert.Equal(t, uint64(12), meta.stateRevision)

	get := doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, get.Code)
	assert.Contains(t, get.Body.String(), `"revision":7`)
	assert.Contains(t, get.Body.String(), "sekret")

	for _, tc := range []struct{ method, body string }{
		{http.MethodPost, vaultCredential()}, {http.MethodPut, vaultPut("7")}, {http.MethodDelete, ""},
	} {
		rec := doVault(t, h.srv, tc.method, vaultTestUID, tc.body, "")
		assert.Equal(t, http.StatusConflict, rec.Code, tc.method)
		assert.Equal(t, raw, f.data, "v2 file bytes must not be rewritten")
		assert.Zero(t, f.saveCalls, "v2 relay writes are rejected before persistence")
	}
	assert.Equal(t, int64(7), mustRevision(t, doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")), "live last-good remains installed")
}

func TestVaultV2DeletedReloadIsKnown404ButCannotBeRecreatedYet(t *testing.T) {
	h := newVaultPersistHarness(t)
	raw := encodeVaultV2Fixture(t, credentialvault.FileStateDeleted, 0, 13, "", emptyVaultFileBody())
	f := seedVaultV2Fixture(h, vaultTestUID, raw)
	h.readySubject(t, vaultTestUID, vaultTestIP)

	assert.Equal(t, http.StatusNotFound, doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "").Code)
	rec := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	assert.Equal(t, http.StatusConflict, rec.Code, "a known v2 tombstone is not an uninitialized v1 file")
	assert.Equal(t, raw, f.data)
	assert.Zero(t, f.saveCalls)
	assert.Equal(t, http.StatusNotFound, doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "").Code)
}

func TestVaultV2WrongFenceIsPreservedAndUnavailable(t *testing.T) {
	h := newVaultPersistHarness(t)
	raw := encodeVaultV2Fixture(t, credentialvault.FileStateActive, 7, 12, "", bundleVaultBody(t))
	raw = []byte(strings.Replace(string(raw), testFenceR, "runtime-foreign", 1))
	f := seedVaultV2Fixture(h, vaultTestUID, raw)
	h.readySubject(t, vaultTestUID, vaultTestIP)

	assert.Equal(t, http.StatusServiceUnavailable, doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "").Code)
	assert.Equal(t, http.StatusServiceUnavailable, doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "").Code)
	assert.Equal(t, raw, f.data, "a file from an unverified fence is not purged or rewritten")
	assert.True(t, f.exists)
	assert.Zero(t, f.saveCalls)
}

func TestVaultV2RemoveAndRebindAreRejectedBeforeEnforcement(t *testing.T) {
	h := newVaultPersistHarness(t)
	raw := encodeVaultV2Fixture(t, credentialvault.FileStateActive, 7, 12, "", bundleVaultBody(t))
	f := seedVaultV2Fixture(h, vaultTestUID, raw)
	subj := h.readySubject(t, vaultTestUID, vaultTestIP)
	h.nft.mu.Lock()
	removedBefore := len(h.nft.removed)
	h.nft.mu.Unlock()

	remove := doAction(t, h.srv, removeBody(t, vaultTestUID, testFenceR, testFenceA))
	assert.Equal(t, http.StatusConflict, remove.Code)
	assert.Equal(t, raw, f.data)
	_, registered := h.reg.Fence(subj)
	assert.True(t, registered, "v2 removal is rejected before unregister")

	rebind := doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, "runtime-2", "attachment-2", 2, vaultAllowPolicy()))
	assert.Equal(t, http.StatusConflict, rebind.Code)
	assert.Equal(t, raw, f.data, "v2 rebind is rejected before purge")
	fence, ok := h.reg.Fence(subj)
	require.True(t, ok)
	assert.Equal(t, testFenceR, fence.RuntimeInstanceID)
	assert.Equal(t, testFenceA, fence.AttachmentID)
	h.nft.mu.Lock()
	assert.Len(t, h.nft.removed, removedBefore, "rejected lifecycle transitions do not unload enforcement")
	h.nft.mu.Unlock()
}

func TestVaultV2UnregisteredRemovePreservesPersistedState(t *testing.T) {
	h := newVaultPersistHarness(t)
	raw := encodeVaultV2Fixture(t, credentialvault.FileStateDeleted, 0, 13, "", emptyVaultFileBody())
	f := seedVaultV2Fixture(h, vaultTestUID, raw)

	rec := doAction(t, h.srv, removeBody(t, vaultTestUID, testFenceR, testFenceA))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.True(t, f.exists)
	assert.Equal(t, raw, f.data)
	assert.Zero(t, f.deleteCalls, "unregistered cleanup must check disk before purging")
}

func TestVaultV2StaleIPTakeoverIsRejectedBeforeEviction(t *testing.T) {
	h := newVaultPersistHarness(t)
	raw := encodeVaultV2Fixture(t, credentialvault.FileStateActive, 7, 12, "", bundleVaultBody(t))
	f := seedVaultV2Fixture(h, vaultTestUID, raw)
	victim := h.readySubject(t, vaultTestUID, vaultTestIP)
	h.nft.mu.Lock()
	removedBefore := len(h.nft.removed)
	h.nft.mu.Unlock()

	rec := doAction(t, h.srv, bindBody(t, "successor", vaultTestIP, "runtime-2", "attachment-2", 1, vaultAllowPolicy()))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, raw, f.data, "stale-IP takeover must not purge the victim's v2 state")
	_, victimRegistered := h.reg.Fence(victim)
	assert.True(t, victimRegistered)
	_, successorRegistered := h.reg.Fence(subject.FromSandboxUID("successor"))
	assert.False(t, successorRegistered)
	h.nft.mu.Lock()
	assert.Len(t, h.nft.removed, removedBefore, "stale-IP takeover must be rejected before enforcement unload")
	h.nft.mu.Unlock()
}

func TestVaultV2EmptyAndUnknownStateDoNotBecomeAbsent(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{name: "explicit empty without mode protocol", raw: encodeVaultV2Fixture(t, credentialvault.FileStateEmpty, 0, 1, "init-1", emptyVaultFileBody())},
		{name: "unknown future schema", raw: []byte(`{"v":9,"incarnation":"rt","attachmentId":"att","revision":0,"vault":{}}` + "\n")},
		{name: "truncated v2 envelope", raw: []byte(`{"v":2,"incarnation":"rt"`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVaultPersistHarness(t)
			f := seedVaultV2Fixture(h, vaultTestUID, tc.raw)
			h.readySubject(t, vaultTestUID, vaultTestIP)
			if tc.name == "explicit empty without mode protocol" {
				subj := subject.FromSandboxUID(vaultTestUID)
				h.srv.mu.Lock()
				meta := h.srv.vaults[subj].fileMeta
				h.srv.mu.Unlock()
				assert.Equal(t, credentialvault.FileStateEmpty, meta.state)
				assert.Equal(t, "lineage-test", meta.lineage)
				assert.Equal(t, uint64(1), meta.stateRevision)
				assert.Equal(t, "init-1", meta.initializationID)
			}

			get := doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
			assert.Equal(t, http.StatusServiceUnavailable, get.Code, "unverifiable state must not look like an ordinary 404")
			post := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
			assert.Equal(t, http.StatusServiceUnavailable, post.Code)
			assert.Equal(t, tc.raw, f.data)
			assert.True(t, f.exists, "unknown/explicit bytes must be preserved")
			assert.Zero(t, f.saveCalls)
		})
	}
}

func TestNullBindingLazyVaultAccessPreservesProtectedDiskState(t *testing.T) {
	for _, file := range []struct {
		name string
		raw  []byte
	}{
		{name: "active v2", raw: encodeVaultV2Fixture(t, credentialvault.FileStateActive, 7, 12, "", bundleVaultBody(t))},
		{name: "deleted v2", raw: encodeVaultV2Fixture(t, credentialvault.FileStateDeleted, 0, 13, "", emptyVaultFileBody())},
		{name: "empty v2", raw: encodeVaultV2Fixture(t, credentialvault.FileStateEmpty, 0, 14, "init-1", emptyVaultFileBody())},
		{name: "foreign fence v2", raw: []byte(strings.Replace(string(encodeVaultV2Fixture(t, credentialvault.FileStateActive, 7, 12, "", bundleVaultBody(t))), testFenceR, "runtime-foreign", 1))},
		{name: "truncated v2", raw: []byte(`{"v":2,"incarnation":"runtime-1"`)},
		{name: "unknown schema", raw: []byte(`{"v":9,"incarnation":"runtime-1","attachmentId":"attachment-1","revision":7,"vault":{}}` + "\n")},
		{name: "unreadable bytes", raw: []byte("not-json\n")},
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
			t.Run(file.name+"/"+method, func(t *testing.T) {
				h := newVaultPersistHarness(t)
				f := seedVaultV2Fixture(h, vaultTestUID, file.raw)
				set := doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 1, nil))
				require.Equal(t, http.StatusOK, set.Code)
				before := append([]byte(nil), f.data...)
				body := ""
				if method == http.MethodPost {
					body = `{"credentials":[],"bindings":[]}`
				} else if method == http.MethodPut {
					body = `{"credentials":[],"bindings":[]}`
				}
				rec := doVault(t, h.srv, method, vaultTestUID, body, "")
				assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "lazy materialization must not expose or overwrite protected state")
				assert.True(t, f.exists)
				assert.Equal(t, before, f.data)
				assert.Zero(t, f.saveCalls)
				assert.Zero(t, f.deleteCalls, "lazy inspection must never quarantine")
			})
		}
	}
}

func TestNullBindingLazyVaultProtectionIsStickyAndRetriesTransientIO(t *testing.T) {
	for _, replacement := range []string{"missing", "legacy"} {
		t.Run("protected-state-"+replacement, func(t *testing.T) {
			h := newVaultPersistHarness(t)
			f := seedVaultV2Fixture(h, vaultTestUID, encodeVaultV2Fixture(t, credentialvault.FileStateEmpty, 0, 14, "init-1", emptyVaultFileBody()))
			require.Equal(t, http.StatusOK, doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 1, nil)).Code)
			assert.Equal(t, http.StatusServiceUnavailable, doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "").Code)
			if replacement == "missing" {
				f.data, f.exists = nil, false
			} else {
				h.seedVaultFile(vaultTestUID, testFenceR, testFenceA, 7, `{"credentials":[],"bindings":[]}`)
			}
			before := append([]byte(nil), f.data...)
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				body := ""
				if method == http.MethodPost {
					body = `{"credentials":[],"bindings":[]}`
				}
				assert.Equal(t, http.StatusServiceUnavailable, doVault(t, h.srv, method, vaultTestUID, body, "").Code)
			}
			assert.Equal(t, before, f.data)
			assert.Zero(t, f.saveCalls)
			assert.Zero(t, f.deleteCalls)
		})
	}

	for _, source := range []string{"snapshot", "storeFor"} {
		t.Run("transient-"+source, func(t *testing.T) {
			h := newVaultPersistHarness(t)
			f := h.fileFor(vaultTestUID)
			h.seedVaultFile(vaultTestUID, testFenceR, testFenceA, 7, `{"credentials":[],"bindings":[]}`)
			if source == "snapshot" {
				f.snapErr = errors.New("temporary snapshot error")
			} else {
				h.newFileErr = errors.New("temporary store error")
			}
			require.Equal(t, http.StatusOK, doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 1, nil)).Code)
			assert.Equal(t, http.StatusServiceUnavailable, doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "").Code)
			f.snapErr = nil
			assert.Equal(t, http.StatusNotFound, doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "").Code, "a successful v1/absence probe restores legacy empty-vault behavior")
			assert.True(t, f.exists)
			assert.Zero(t, f.saveCalls)
			assert.Zero(t, f.deleteCalls)
		})
	}
}

func TestNullBindingLazyVaultMaterializationPreservesLegacyAndSealSemantics(t *testing.T) {
	for _, withFile := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "legacy-v1"}[withFile], func(t *testing.T) {
			h := newVaultPersistHarness(t)
			f := h.fileFor(vaultTestUID)
			if withFile {
				h.seedVaultFile(vaultTestUID, testFenceR, testFenceA, 7, `{"credentials":[],"bindings":[]}`)
			}
			before := append([]byte(nil), f.data...)
			require.Equal(t, http.StatusOK, doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 1, nil)).Code)
			assert.Equal(t, http.StatusNotFound, doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "").Code)
			assert.Equal(t, before, f.data)
			assert.Zero(t, f.deleteCalls)
			post := doVault(t, h.srv, http.MethodPost, vaultTestUID, `{"credentials":[],"bindings":[]}`, "")
			assert.Equal(t, http.StatusCreated, post.Code, "legacy v1/no-file subjects retain the v1 writer behavior")
		})
	}

	h := newVaultPersistHarness(t)
	subj := subject.FromSandboxUID(vaultTestUID)
	require.NoError(t, h.vp.seal(subj))
	require.Equal(t, http.StatusOK, doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 1, nil)).Code)
	assert.Equal(t, http.StatusConflict, doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "").Code)
	assert.Equal(t, http.StatusConflict, doVault(t, h.srv, http.MethodPost, vaultTestUID, `{"credentials":[],"bindings":[]}`, "").Code)
	active := httptest.NewRecorder()
	h.srv.handleCredentialVaultActive(active, httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp="+vaultTestIP, nil))
	assert.Equal(t, http.StatusConflict, active.Code, "the restored seal also covers the addon active-snapshot path")
	f := h.fileFor(vaultTestUID)
	assert.False(t, f.exists)
	assert.Zero(t, f.saveCalls)
}

func TestNullBindingLazyVaultMaterializationProtectsActiveAddonAndMemoryOnlyLegacy(t *testing.T) {
	h := newVaultPersistHarness(t)
	seedVaultV2Fixture(h, vaultTestUID, encodeVaultV2Fixture(t, credentialvault.FileStateActive, 7, 12, "", bundleVaultBody(t)))
	require.Equal(t, http.StatusOK, doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 1, nil)).Code)
	active := httptest.NewRecorder()
	h.srv.handleCredentialVaultActive(active, httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp="+vaultTestIP, nil))
	assert.Equal(t, http.StatusServiceUnavailable, active.Code, "the addon path must not expose a lazy v2 file as an empty snapshot")

	srv, _, _ := fastSandboxTestServer(t)
	require.Equal(t, http.StatusOK, doAction(t, srv, bindBody(t, "memory-only", vaultTestIP, testFenceR, testFenceA, 1, nil)).Code)
	assert.Equal(t, http.StatusNotFound, doVault(t, srv, http.MethodGet, "memory-only", "", "").Code)
	assert.Equal(t, http.StatusCreated, doVault(t, srv, http.MethodPost, "memory-only", `{"credentials":[],"bindings":[]}`, "").Code)
}

func TestNullBindingVaultExportRejectsProtectedStateBeforeSeal(t *testing.T) {
	h := newVaultPersistHarness(t)
	raw := encodeVaultV2Fixture(t, credentialvault.FileStateDeleted, 0, 13, "", emptyVaultFileBody())
	f := seedVaultV2Fixture(h, snapshotTestUID, raw)
	require.Equal(t, http.StatusOK, doAction(t, h.srv, bindBody(t, snapshotTestUID, snapshotTestIP, testFenceR, testFenceA, 1, nil)).Code)
	target := filepath.Join(t.TempDir(), "bundle", "v2.json")
	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", target))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, raw, f.data)
	assert.False(t, h.fakeForPath(target).exists, "rejected export must not create a bundle copy")
	assert.False(t, h.vp.isSealed(subject.FromSandboxUID(snapshotTestUID)), "rejected export must not seal")
	assert.Zero(t, f.saveCalls)
	assert.Zero(t, f.deleteCalls)
}

func TestVaultV2ExportAndImportFailBeforeSideEffects(t *testing.T) {
	h := newVaultPersistHarness(t)
	active := encodeVaultV2Fixture(t, credentialvault.FileStateActive, 7, 12, "", bundleVaultBody(t))
	f := seedVaultV2Fixture(h, snapshotTestUID, active)
	h.readySubject(t, snapshotTestUID, snapshotTestIP)

	legacyBundle := filepath.Join(t.TempDir(), "bundle", "v1.json")
	h.seedBundle(t, legacyBundle)
	before := append([]byte(nil), f.data...)
	importRec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_IMPORT", legacyBundle))
	assert.Equal(t, http.StatusConflict, importRec.Code, "v1 bundle must not restamp over a v2 file")
	assert.Equal(t, before, f.data)
	assert.Zero(t, f.saveCalls)
	assert.Equal(t, int64(7), mustRevision(t, doVault(t, h.srv, http.MethodGet, snapshotTestUID, "", "")))

	exportPath := filepath.Join(t.TempDir(), "bundle", "v2.json")
	exportRec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", exportPath))
	assert.Equal(t, http.StatusConflict, exportRec.Code, "v2 cannot be exported when the receiving side cannot restore its lineage")
	assert.False(t, h.fakeForPath(exportPath).exists, "v2 export fails before writing a bundle")
	assert.False(t, h.srv.vaultSealedLocked(subject.FromSandboxUID(snapshotTestUID)), "v2 export fails before sealing the subject")
	assert.Equal(t, before, f.data)
}

func TestVaultV2BundleImportIsRejectedBeforeRestamp(t *testing.T) {
	h := newVaultPersistHarness(t)
	path := filepath.Join(t.TempDir(), "bundle", "v2.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	raw := encodeVaultV2Fixture(t, credentialvault.FileStateActive, 4, 8, "", bundleVaultBody(t))
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	rec := doAction(t, h.srv, snapshotBody(t, "fresh-destination", snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", path))
	assert.Equal(t, http.StatusConflict, rec.Code)
	f := h.fileFor("fresh-destination")
	assert.False(t, f.exists, "unsupported v2 import must not create a destination file")
	assert.Zero(t, f.saveCalls)
}

func TestLegacyV1BundleReplayRemainsV1(t *testing.T) {
	h := newVaultPersistHarness(t)
	path := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
	h.seedBundle(t, path)
	transfer := snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", path)

	first := doAction(t, h.srv, transfer)
	require.Equal(t, http.StatusOK, first.Code)
	f := h.fileFor(snapshotTestUID)
	require.True(t, f.exists)
	vf, _, err := credentialvault.DecodeVaultFile(f.data)
	require.NoError(t, err)
	require.Equal(t, credentialvault.VaultFileVersion, vf.Version)
	firstBytes := append([]byte(nil), f.data...)

	second := doAction(t, h.srv, transfer)
	assert.Equal(t, http.StatusOK, second.Code)
	assert.Equal(t, firstBytes, f.data, "retrying a v1 transfer does not silently upgrade or restamp it differently")
	assert.Equal(t, 2, f.saveCalls)
}

func TestLegacyV1ImportStillRepairsCorruptLocalFile(t *testing.T) {
	h := newVaultPersistHarness(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
	h.seedBundle(t, bundlePath)
	f := h.fileFor(snapshotTestUID)
	f.data = []byte("{corrupt legacy v1 file")
	f.exists = true

	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, f.saveCalls, "the existing v1 import recovery path remains available")
	vf, _, err := credentialvault.DecodeVaultFile(f.data)
	require.NoError(t, err)
	assert.Equal(t, credentialvault.VaultFileVersion, vf.Version)
}

func TestVaultV2DecoderRetainsOrderingMetadata(t *testing.T) {
	raw := encodeVaultV2Fixture(t, credentialvault.FileStateDeleted, 0, 13, "", emptyVaultFileBody())
	vf, body, err := credentialvault.DecodeVaultFile(raw)
	require.NoError(t, err)
	assert.Equal(t, credentialvault.VaultFileVersion2, vf.Version)
	assert.Equal(t, credentialvault.FileStateDeleted, vf.State)
	assert.Equal(t, "lineage-test", vf.StateLineage)
	assert.Equal(t, uint64(13), vf.StateRevision)
	assert.Empty(t, body.Credentials)
	assert.Empty(t, body.Bindings)
	emptyRaw := encodeVaultV2Fixture(t, credentialvault.FileStateEmpty, 0, 14, "init-authority-1", emptyVaultFileBody())
	emptyFile, _, err := credentialvault.DecodeVaultFile(emptyRaw)
	require.NoError(t, err)
	assert.Equal(t, "init-authority-1", emptyFile.InitializationID, "read-only decoding retains the initialization identity")
}

func TestVaultV2MalformedEnvelopeIsPreservedAsUnsupported(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"v":2,"incarnation":"rt"`),
		[]byte(`{"v":2,"v":1,"incarnation":"rt","attachmentId":"att","revision":1,"vault":{"credentials":[],"bindings":[]}}`),
	} {
		_, _, err := credentialvault.DecodeVaultFile(raw)
		require.Error(t, err)
		assert.ErrorIs(t, err, credentialvault.ErrUnsupportedFileVersion)
	}
}

func TestVaultV2SnapshotFailureBlocksWriteUntilSameFenceRetry(t *testing.T) {
	for _, failure := range []string{"storeFor", "snapshot"} {
		t.Run(failure, func(t *testing.T) {
			h := newVaultPersistHarness(t)
			raw := encodeVaultV2Fixture(t, credentialvault.FileStateActive, 7, 12, "", bundleVaultBody(t))
			f := seedVaultV2Fixture(h, vaultTestUID, raw)
			if failure == "storeFor" {
				h.newFileErr = errors.New("temporary store error")
			} else {
				f.snapErr = errors.New("temporary snapshot error")
			}
			h.readySubject(t, vaultTestUID, vaultTestIP)
			f.snapErr = nil

			get := doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
			assert.Equal(t, http.StatusServiceUnavailable, get.Code, "unreadable persisted state must not look absent")
			post := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
			assert.Equal(t, http.StatusServiceUnavailable, post.Code)
			assert.Equal(t, raw, f.data, "a transient reload error must not let POST downgrade v2 to v1")
			assert.Zero(t, f.saveCalls)

			// A same-fence SET_BINDING replay retries the read. Once storage
			// recovers, v2 active reloads and stays protected by the read-only guard.
			retry := doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 2, vaultAllowPolicy()))
			require.Equal(t, http.StatusOK, retry.Code)
			get = doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
			require.Equal(t, http.StatusOK, get.Code)
			assert.Contains(t, get.Body.String(), `"revision":7`)
			post = doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
			assert.Equal(t, http.StatusConflict, post.Code)
			assert.Equal(t, raw, f.data)
			assert.Zero(t, f.saveCalls)
		})
	}
}

func TestVaultKnownProtectedStateMissingRemainsUnknownOnSameFenceReload(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{name: "explicit empty v2", raw: encodeVaultV2Fixture(t, credentialvault.FileStateEmpty, 0, 14, "init-1", emptyVaultFileBody())},
		{name: "foreign fence v2", raw: []byte(strings.Replace(string(encodeVaultV2Fixture(t, credentialvault.FileStateActive, 7, 12, "", bundleVaultBody(t))), testFenceR, "runtime-foreign", 1))},
		{name: "unknown schema", raw: []byte(`{"v":9,"incarnation":"runtime-1","attachmentId":"attachment-1","revision":7,"vault":{}}` + "\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVaultPersistHarness(t)
			f := seedVaultV2Fixture(h, vaultTestUID, tc.raw)
			h.readySubject(t, vaultTestUID, vaultTestIP)
			assert.Equal(t, http.StatusServiceUnavailable, doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "").Code)

			// Simulate the protected file disappearing after it was observed.
			// A same-fence SET replay must not turn that history into v1 absence.
			f.data, f.exists = nil, false
			retry := doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 2, vaultAllowPolicy()))
			require.Equal(t, http.StatusOK, retry.Code)
			get := doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
			post := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
			assert.Equal(t, http.StatusServiceUnavailable, get.Code, "known protected history must remain unavailable after the file disappears")
			assert.Equal(t, http.StatusServiceUnavailable, post.Code, "missing protected state must not be treated as a fresh v1 Vault")
			assert.False(t, f.exists)
			assert.Zero(t, f.saveCalls)
		})
	}
}

func TestVaultTransientFirstReadFailureCanRecoverToV1(t *testing.T) {
	for _, failure := range []string{"storeFor", "snapshot"} {
		t.Run(failure, func(t *testing.T) {
			h := newVaultPersistHarness(t)
			h.seedVaultFile(vaultTestUID, testFenceR, testFenceA, 7, `{"credentials":[],"bindings":[]}`)
			f := h.fileFor(vaultTestUID)
			if failure == "storeFor" {
				h.newFileErr = errors.New("temporary store error")
			} else {
				f.snapErr = errors.New("temporary snapshot error")
			}
			h.readySubject(t, vaultTestUID, vaultTestIP)
			f.snapErr = nil
			assert.Equal(t, http.StatusServiceUnavailable, doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "").Code)

			retry := doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 2, vaultAllowPolicy()))
			require.Equal(t, http.StatusOK, retry.Code)
			get := doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
			require.Equal(t, http.StatusOK, get.Code, "a transient read failure must clear when the same fence reads a v1 file")
			assert.Contains(t, get.Body.String(), `"revision":7`)
			put := doVault(t, h.srv, http.MethodPut, vaultTestUID, vaultPut("7"), "")
			require.Equal(t, http.StatusOK, put.Code, "recovered v1 state accepts its normal conditional update")
			assert.Contains(t, put.Body.String(), `"revision":8`)
			assert.True(t, f.exists)
			assert.Equal(t, 1, f.saveCalls)
		})
	}
}

func TestVaultKnownProtectedStateCannotReloadLegacyOnSameFence(t *testing.T) {
	h := newVaultPersistHarness(t)
	seedVaultV2Fixture(h, vaultTestUID, encodeVaultV2Fixture(t, credentialvault.FileStateEmpty, 0, 14, "init-1", emptyVaultFileBody()))
	h.readySubject(t, vaultTestUID, vaultTestIP)

	// A successful v1 read after a known explicit state would imply an
	// out-of-band downgrade. It must not erase the already observed provenance.
	h.seedVaultFile(vaultTestUID, testFenceR, testFenceA, 7, `{"credentials":[],"bindings":[]}`)
	f := h.fileFor(vaultTestUID)
	v1 := append([]byte(nil), f.data...)
	retry := doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 2, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, retry.Code)
	assert.Equal(t, http.StatusServiceUnavailable, doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "").Code)
	assert.Equal(t, http.StatusServiceUnavailable, doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "").Code)
	assert.Equal(t, v1, f.data, "the legacy file is left untouched for diagnosis/recovery")
	assert.Zero(t, f.saveCalls)
}

func TestRemoveBindingV1PurgeFailureKeepsBestEffortAck(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, vaultTestUID, vaultTestIP)
	post := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, post.Code)
	f := h.fileFor(vaultTestUID)
	f.deleteErr = errors.New("simulated v1 unlink failure")

	var rec *httptest.ResponseRecorder
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		rec = doAction(t, h.srv, removeBody(t, vaultTestUID, testFenceR, testFenceA))
	}()
	assert.False(t, panicked, "an I/O failure must not turn status 0 into an invalid WriteHeader")
	if panicked {
		return
	}
	require.NotNil(t, rec)
	assert.Equal(t, http.StatusOK, rec.Code, "legacy v1 cleanup retains its best-effort acknowledgement")
	assert.True(t, f.exists, "the failed v1 purge leaves the file for retry/recovery")
	assert.Equal(t, 1, f.deleteCalls)
}

func TestRebindV1PurgeFailureKeepsBestEffortAck(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, vaultTestUID, vaultTestIP)
	post := doVault(t, h.srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, post.Code)
	f := h.fileFor(vaultTestUID)
	f.deleteErr = errors.New("simulated v1 unlink failure")

	var rec *httptest.ResponseRecorder
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		rec = doAction(t, h.srv, bindBody(t, vaultTestUID, vaultTestIP, "runtime-2", "attachment-2", 2, vaultAllowPolicy()))
	}()
	assert.False(t, panicked, "an I/O failure must not turn status 0 into an invalid WriteHeader")
	if panicked {
		return
	}
	require.NotNil(t, rec)
	assert.Equal(t, http.StatusOK, rec.Code, "legacy v1 rebind cleanup retains its best-effort acknowledgement")
	assert.True(t, f.exists, "the failed v1 purge leaves the file for retry/recovery")
	assert.GreaterOrEqual(t, f.deleteCalls, 1)
	newFence, ok := h.reg.Fence(subject.FromSandboxUID(vaultTestUID))
	require.True(t, ok)
	assert.Equal(t, "runtime-2", newFence.RuntimeInstanceID)
	assert.Equal(t, "attachment-2", newFence.AttachmentID)
}
