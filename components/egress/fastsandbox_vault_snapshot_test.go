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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// Vault snapshot transfer across pause/resume (OSEP-0022). The driver side
// is platform work — these tests drive the egress hooks via the actions.

const (
	snapshotTestUID  = "snapshot-u"
	snapshotTestIP   = "10.9.1.5"
	snapshotResumeR  = "runtime-2"
	snapshotResumeA  = "attachment-9"
	bundleTestOldRev = 3
)

// snapshotBody builds a VAULT_EXPORT / VAULT_IMPORT envelope.
func snapshotBody(t *testing.T, uid, runtimeID, attID, op, path string, seal ...*bool) string {
	t.Helper()
	env := actionhandler.Envelope{
		APIVersion:   constants.ActionsAPIVersion,
		Operation:    actionhandler.Operation(op),
		InvocationID: fmt.Sprintf("inv-%s-snapshot", uid),
		Sandbox:      actionhandler.SandboxRef{UID: uid, Name: "sb", Namespace: "ns"},
		Revision: actionhandler.Revision{
			SpecGeneration:    1,
			RuntimeInstanceID: runtimeID,
			AttachmentID:      attID,
			RouteGeneration:   1,
		},
		Snapshot: &actionhandler.SnapshotTransfer{Path: path},
	}
	if len(seal) > 0 {
		env.Snapshot.Seal = seal[0]
	}
	raw, err := json.Marshal(env)
	require.NoError(t, err)
	return string(raw)
}

// fakeForPath returns (creating if needed) the fake file store the harness
// hands out for an arbitrary path — vault files and bundle staging files
// share the newFile seam.
func (h *vaultPersistHarness) fakeForPath(path string) *fakeVaultFile {
	h.mapMu.Lock()
	defer h.mapMu.Unlock()
	f, ok := h.files[path]
	if !ok {
		f = newFakeVaultFile(path)
		h.files[path] = f
	}
	return f
}

// bundleVaultBody renders the canonical bundle body for the shared test
// credential vault (the exact shape a relay write would persist).
func bundleVaultBody(t *testing.T) credentialvault.VaultFileBody {
	t.Helper()
	var req credentialvault.CreateRequest
	require.NoError(t, json.Unmarshal([]byte(vaultCredential()), &req))
	return credentialvault.VaultFileBody{Credentials: req.Credentials, Bindings: req.Bindings}
}

// seedBundle writes a real bundle file carrying the OLD runtime fence
// (import reads the source with plain I/O — no injected seam).
func (h *vaultPersistHarness) seedBundle(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := credentialvault.EncodeVaultFile(testFenceR, testFenceA, bundleTestOldRev, bundleVaultBody(t))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return raw
}

// snapshotSubject drives the shared test subject to active with a vault
// revision 1 persisted.
func (h *vaultPersistHarness) snapshotSubject(t *testing.T) subject.Subject {
	t.Helper()
	s := subject.FromSandboxUID(snapshotTestUID)
	rec := doAction(t, h.srv, bindBody(t, snapshotTestUID, snapshotTestIP, testFenceR, testFenceA, 1, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, snapshotTestUID, testFenceR, testFenceA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, snapshotTestUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doVault(t, h.srv, http.MethodPost, snapshotTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)
	return s
}

// ---------------------------------------------------------------------------
// Export: copy + seal
// ---------------------------------------------------------------------------

func TestVaultSnapshotExportCopiesAndSeals(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.snapshotSubject(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")

	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)

	// The bundle copy is byte-identical to the subject's vault file.
	bundle := h.fakeForPath(bundlePath)
	vaultFile := h.fileFor(snapshotTestUID)
	require.True(t, bundle.exists, "export must publish the bundle file")
	require.True(t, vaultFile.exists)
	assert.Equal(t, vaultFile.data, bundle.data)

	// Sealed: every vault op is a 409 — the bundle copy is already taken.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := doVault(t, h.srv, method, snapshotTestUID, vaultCredential(), "")
		assert.Equal(t, http.StatusConflict, rec.Code, method)
	}
	// Policy enforcement is unchanged while the sandbox is paused.
	state, ok := h.reg.Get(subject.FromSandboxUID(snapshotTestUID))
	require.True(t, ok)
	assert.Equal(t, subject.StateActive, state)

	// A retried export (lost ack) re-copies and re-seals idempotently.
	rec = doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, vaultFile.data, h.fakeForPath(bundlePath).data)
}

func TestVaultSnapshotExportWithoutVaultFileStillSeals(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, snapshotTestUID, snapshotTestIP)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")

	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, h.fakeForPath(bundlePath).exists, "nothing to carry: no vault file, no bundle write")
	assert.Equal(t, http.StatusConflict, doVault(t, h.srv, http.MethodPost, snapshotTestUID, vaultCredential(), "").Code)
}

// An unregistered subject (no egress binding, or after terminal cleanup)
// has no vault to carry: the export is a 200 no-op. A registered subject
// under a stale fence still fails (409).
func TestVaultSnapshotExportUnregisteredSubjectIsNoOp(t *testing.T) {
	h := newVaultPersistHarness(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", "unregistered.json")

	rec := doAction(t, h.srv, snapshotBody(t, "unregistered-sandbox", testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, h.fakeForPath(bundlePath).exists, "nothing to carry: no subject, no bundle write")
}

func TestVaultSnapshotExportFailureStaysActive(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.snapshotSubject(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")

	// The bundle target rejects the copy (pre-rename failure).
	bundle := h.fakeForPath(bundlePath)
	bundle.saveErr = fmt.Errorf("disk on fire")
	bundle.saveState = policy.FileUnchanged

	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	// The subject stays active and unsealed; the caller decides what the
	// pause flow does (the fast-sandbox driver fails the checkpoint), and
	// live vault ops keep working.
	assert.False(t, h.srv.vaultSealedLocked(subject.FromSandboxUID(snapshotTestUID)))
	rec = doVault(t, h.srv, http.MethodPost, snapshotTestUID, vaultCredential(), "")
	assert.Equal(t, http.StatusConflict, rec.Code, "POST on an existing vault is 409 (create-only), proving the surface still serves")
	rec = doVault(t, h.srv, http.MethodGet, snapshotTestUID, "", "")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestVaultSnapshotExportGuards(t *testing.T) {
	t.Run("persistence disabled", func(t *testing.T) {
		srv, reg, _ := fastSandboxTestServer(t)
		setBindingAndReady(t, srv, reg, snapshotTestUID, snapshotTestIP, vaultAllowPolicy())
		rec := doAction(t, srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", "/tmp/bundle.json"))
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		rec = doAction(t, srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_IMPORT", "/tmp/bundle.json"))
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})
	t.Run("unknown subject", func(t *testing.T) {
		h := newVaultPersistHarness(t)
		rec := doAction(t, h.srv, snapshotBody(t, "ghost", testFenceR, testFenceA, "VAULT_EXPORT", "/tmp/bundle.json"))
		// 200 no-op: nothing to carry. See the dedicated no-op test.
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.False(t, h.fakeForPath("/tmp/bundle.json").exists)
	})
	t.Run("stale fence", func(t *testing.T) {
		h := newVaultPersistHarness(t)
		h.snapshotSubject(t)
		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, "old-runtime", testFenceA, "VAULT_EXPORT", "/tmp/bundle.json"))
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.False(t, h.srv.vaultSealedLocked(subject.FromSandboxUID(snapshotTestUID)))
	})
	t.Run("recovery latch rejects the export", func(t *testing.T) {
		h := newVaultPersistHarness(t)
		h.snapshotSubject(t)
		h.srv.requireVaultRecoveryLocked("simulated latch")
		bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")

		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.False(t, h.srv.vaultSealedLocked(subject.FromSandboxUID(snapshotTestUID)))
		assert.False(t, h.fakeForPath(bundlePath).exists, "no bundle copy while the latch holds")
	})
	t.Run("target under the vault persistence directory", func(t *testing.T) {
		h := newVaultPersistHarness(t)
		h.snapshotSubject(t)
		inside := filepath.Join(h.dir, "other.json") // another subject's file

		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", inside))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.False(t, h.srv.vaultSealedLocked(subject.FromSandboxUID(snapshotTestUID)))
		// fakeForPath would create one, so scan the map directly.
		h.mapMu.Lock()
		_, materialized := h.files[inside]
		h.mapMu.Unlock()
		assert.False(t, materialized, "no store materialized for the rejected target")
	})
	t.Run("concurrent vault write abandons the seal", func(t *testing.T) {
		h := newVaultPersistHarness(t)
		h.snapshotSubject(t)
		bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
		copied := h.fileFor(snapshotTestUID).data

		// A relay write lands mid-copy: the copy is stale, phase 3 refuses
		// to seal.
		newer, err := credentialvault.EncodeVaultFile(testFenceR, testFenceA, 2, credentialvault.VaultFileBody{})
		require.NoError(t, err)
		bundle := h.fakeForPath(bundlePath)
		bundle.onSave = func() {
			f := h.fileFor(snapshotTestUID)
			f.data = newer
			f.exists = true
		}

		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.False(t, h.srv.vaultSealedLocked(subject.FromSandboxUID(snapshotTestUID)))
		assert.Equal(t, copied, bundle.data, "the stale copy exists but is not sealed — the driver retries against the newer state")
	})
}

// TestVaultSnapshotExportSealFree (issue #2312, Seal=false): the template
// carry copies the vault file verbatim and the source keeps full relay CRUD.
func TestVaultSnapshotExportSealFree(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.snapshotSubject(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
	sealFree := false

	before := h.fileFor(snapshotTestUID).data
	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath, &sealFree))
	require.Equal(t, http.StatusOK, rec.Code)

	bundle := h.fakeForPath(bundlePath)
	require.True(t, bundle.exists)
	assert.Equal(t, before, bundle.data, "byte-identical copy")

	// The source keeps full relay CRUD (no seal): GET works, a relay write
	// lands, and the write does not leak into the taken bundle.
	rec = doVault(t, h.srv, http.MethodGet, snapshotTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doVault(t, h.srv, http.MethodPut, snapshotTestUID, vaultPut("1"), "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotEqual(t, before, h.fileFor(snapshotTestUID).data, "the source moved on")
	assert.Equal(t, before, bundle.data, "the taken bundle is unchanged")
}

// ---------------------------------------------------------------------------
// Import: re-stamp + commit protocol
// ---------------------------------------------------------------------------

func TestVaultSnapshotImportLandsFileBeforeRegistration(t *testing.T) {
	h := newVaultPersistHarness(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
	oldBytes := h.seedBundle(t, bundlePath)

	// Resume on a fresh Pod: no subject registered yet; the driver imports
	// before SET_BINDING.
	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)

	// Landed via the commit protocol, re-stamped with the restored fence —
	// the only authorized incarnation transfer.
	vf, body := decodeFile(t, h.fileFor(snapshotTestUID))
	assert.Equal(t, 1, vf.Version)
	assert.Equal(t, snapshotResumeR, vf.Incarnation)
	assert.Equal(t, snapshotResumeA, vf.AttachmentID)
	assert.Equal(t, int64(bundleTestOldRev), vf.Revision)
	require.Len(t, body.Credentials, 1)
	require.Len(t, body.Bindings, 1)
	carried, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	assert.Equal(t, oldBytes, carried, "the bundle source is read, never consumed")

	// No memory slice before registration (the reload happens at
	// SET_BINDING, fenced by the same identity).
	srv := h.srv
	srv.mu.Lock()
	_, live := srv.vaults[subject.FromSandboxUID(snapshotTestUID)]
	srv.mu.Unlock()
	assert.False(t, live)

	// At registration the file reloads under the restored fence; the vault
	// is held (served to ops) from then on and activates with policy at the
	// data-plane-ready barrier.
	rec = doAction(t, h.srv, bindBody(t, snapshotTestUID, snapshotTestIP, snapshotResumeR, snapshotResumeA, 1, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doVault(t, h.srv, http.MethodGet, snapshotTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), fmt.Sprintf(`"revision":%d`, bundleTestOldRev))
	rec = doAction(t, h.srv, hookBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	state, ok := h.reg.Get(subject.FromSandboxUID(snapshotTestUID))
	require.True(t, ok)
	assert.Equal(t, subject.StateActive, state)
	rec = doVault(t, h.srv, http.MethodGet, snapshotTestUID, "", "")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestVaultSnapshotImportAfterSetBindingInstallsMemory(t *testing.T) {
	h := newVaultPersistHarness(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
	h.seedBundle(t, bundlePath)

	// Import while denying (after SET_BINDING, before data-plane-ready).
	rec := doAction(t, h.srv, bindBody(t, snapshotTestUID, snapshotTestIP, snapshotResumeR, snapshotResumeA, 1, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)

	// The memory slice is installed immediately (validated against the
	// pending policy) and serves ops while the subject is held at the
	// barrier.
	rec = doVault(t, h.srv, http.MethodGet, snapshotTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), fmt.Sprintf(`"revision":%d`, bundleTestOldRev))

	// A relay write on top lands the successor revision (not sealed).
	rec = doVault(t, h.srv, http.MethodPut, snapshotTestUID, vaultPut(fmt.Sprint(bundleTestOldRev)), "")
	require.Equal(t, http.StatusOK, rec.Code)
	vf, _ := decodeFile(t, h.fileFor(snapshotTestUID))
	assert.Equal(t, snapshotResumeR, vf.Incarnation)
	assert.Equal(t, int64(bundleTestOldRev+1), vf.Revision)
}

func TestVaultSnapshotImportRejects(t *testing.T) {
	newHarness := func(t *testing.T) (*vaultPersistHarness, string) {
		h := newVaultPersistHarness(t)
		return h, filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
	}
	t.Run("missing bundle file", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
	t.Run("corrupt bundle", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(bundlePath), 0o700))
		require.NoError(t, os.WriteFile(bundlePath, []byte(`{"v":1,`), 0o600))
		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("newer file version", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(bundlePath), 0o700))
		require.NoError(t, os.WriteFile(bundlePath,
			[]byte(`{"v":99,"incarnation":"old","attachmentId":"old","revision":1,"vault":{"credentials":[],"bindings":[]}}`+"\n"), 0o600))
		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("oversized bundle", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(bundlePath), 0o700))
		require.NoError(t, os.WriteFile(bundlePath, make([]byte, maxVaultSnapshotBytes+1), 0o600))
		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("source under the vault persistence directory", func(t *testing.T) {
		h, _ := newHarness(t)
		inside := filepath.Join(h.dir, "staged.json")
		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", inside))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("stale fence while registered", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		h.seedBundle(t, bundlePath)
		rec := doAction(t, h.srv, bindBody(t, snapshotTestUID, snapshotTestIP, testFenceR, testFenceA, 1, vaultAllowPolicy()))
		require.Equal(t, http.StatusOK, rec.Code)
		rec = doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusConflict, rec.Code)
	})
	t.Run("bundle rejected by policy keeps last-good", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		// A live vault (revision 1) under the current policy.
		h.snapshotSubject(t)
		// A bundle whose binding host the policy does not allow.
		denied := credentialvault.VaultFileBody{
			Credentials: bundleVaultBody(t).Credentials,
			Bindings: []credentialvault.Binding{{
				Name:  "other",
				Match: credentialvault.Match{Hosts: []string{"denied.example.com"}},
				Auth:  credentialvault.Auth{Type: "bearer", Credential: "tok"},
			}},
		}
		raw, err := credentialvault.EncodeVaultFile(testFenceR, testFenceA, bundleTestOldRev, denied)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(bundlePath), 0o700))
		require.NoError(t, os.WriteFile(bundlePath, raw, 0o600))

		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusBadRequest, rec.Code)

		// Last-good on both sides: the live vault and the file keep revision 1.
		vf, _ := decodeFile(t, h.fileFor(snapshotTestUID))
		assert.Equal(t, int64(1), vf.Revision)
		rec = doVault(t, h.srv, http.MethodGet, snapshotTestUID, "", "")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), `"revision":1`)
		assert.False(t, h.srv.vaultRecoveryActive(), "a pre-rename NACK never latches recovery")
	})
}

func TestVaultSnapshotDuplicateImportDoesNotRollBack(t *testing.T) {
	h := newVaultPersistHarness(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
	h.seedBundle(t, bundlePath)

	// First import lands revision 3 (registered, denying).
	rec := doAction(t, h.srv, bindBody(t, snapshotTestUID, snapshotTestIP, snapshotResumeR, snapshotResumeA, 1, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)

	// Relay writes resume: revision 4 lands.
	rec = doVault(t, h.srv, http.MethodPut, snapshotTestUID, vaultPut(fmt.Sprint(bundleTestOldRev)), "")
	require.Equal(t, http.StatusOK, rec.Code)

	// A lost-ack retry must not roll revision 4 back to the bundle's 3.
	rec = doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)

	vf, _ := decodeFile(t, h.fileFor(snapshotTestUID))
	assert.Equal(t, int64(bundleTestOldRev+1), vf.Revision, "the newer relay write survives the retried import")
	rec = doVault(t, h.srv, http.MethodGet, snapshotTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), fmt.Sprintf(`"revision":%d`, bundleTestOldRev+1))
}

func TestVaultSnapshotImportCommitProtocolOutcomes(t *testing.T) {
	t.Run("uncertain persist latches recovery", func(t *testing.T) {
		h := newVaultPersistHarness(t)
		bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
		h.seedBundle(t, bundlePath)
		h.fileFor(snapshotTestUID).saveState = policy.FileUnknown
		h.fileFor(snapshotTestUID).saveErr = fmt.Errorf("rename lied")

		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.True(t, h.srv.vaultRecoveryActive(), "an uncertain outcome freezes vault writes until restart")
	})
	t.Run("pre-rename failure nacks without latch", func(t *testing.T) {
		h := newVaultPersistHarness(t)
		bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
		h.seedBundle(t, bundlePath)
		h.fileFor(snapshotTestUID).saveState = policy.FileUnchanged
		h.fileFor(snapshotTestUID).saveErr = fmt.Errorf("temp create failed")

		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.False(t, h.srv.vaultRecoveryActive())
		assert.False(t, h.fileFor(snapshotTestUID).exists, "a rejected import lands nothing")
	})
	t.Run("latch freezes unregistered imports too", func(t *testing.T) {
		h := newVaultPersistHarness(t)
		bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
		h.seedBundle(t, bundlePath)
		// An earlier subject's uncertain outcome latched recovery.
		h.srv.requireVaultRecoveryLocked("simulated latch")

		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.False(t, h.fileFor(snapshotTestUID).exists, "no vault file mutation while latched")
	})
}

// ---------------------------------------------------------------------------
// REMOVE_BINDING on a sealed subject
// ---------------------------------------------------------------------------

func TestVaultSnapshotRemoveBindingPurgesSealed(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.snapshotSubject(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")

	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, h.fakeForPath(bundlePath).exists, "precondition: the bundle copy is intact")

	rec = doAction(t, h.srv, removeBody(t, snapshotTestUID, testFenceR, testFenceA))
	require.Equal(t, http.StatusOK, rec.Code)

	// Terminal purge: memory + file gone; every vault op 404s. The bundle
	// copy survives (export strictly precedes the teardown).
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := doVault(t, h.srv, method, snapshotTestUID, vaultCredential(), "")
		assert.Equal(t, http.StatusNotFound, rec.Code, method)
	}
	assert.False(t, h.fileFor(snapshotTestUID).exists, "REMOVE_BINDING purges the vault file")
	assert.True(t, h.fakeForPath(bundlePath).exists, "the bundle copy is untouched by the teardown")
}

// ---------------------------------------------------------------------------
// Codex review round 2 regressions
// ---------------------------------------------------------------------------

func TestVaultSnapshotExportCleansStaleTargetForAbsentVault(t *testing.T) {
	t.Run("stale target from a previous attempt", func(t *testing.T) {
		h := newVaultPersistHarness(t)
		h.readySubject(t, snapshotTestUID, snapshotTestIP)
		bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
		// A previous attempt left bytes at the target; the vault has since
		// been deleted (relay DELETE).
		stale := h.fakeForPath(bundlePath)
		stale.data = []byte(`{"v":1,"incarnation":"runtime-1","attachmentId":"attachment-1","revision":1,"vault":{"credentials":[],"bindings":[]}}`)
		stale.exists = true

		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.False(t, stale.exists, "sealing an absent vault must remove stale bundle bytes")
		assert.True(t, h.srv.vaultSealedLocked(subject.FromSandboxUID(snapshotTestUID)))
	})
	t.Run("relay DELETE mid-copy is rejected, retry cleans up", func(t *testing.T) {
		h := newVaultPersistHarness(t)
		h.snapshotSubject(t)
		bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")

		// The vault is deleted while the (unlocked) copy is in flight.
		bundle := h.fakeForPath(bundlePath)
		bundle.onSave = func() {
			f := h.fileFor(snapshotTestUID)
			f.data, f.exists = nil, false
		}
		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
		require.Equal(t, http.StatusConflict, rec.Code)
		require.True(t, bundle.exists, "the stale copy is published but unsealed")
		bundle.onSave = nil

		// The retry sees an absent source and must clean the stale copy.
		rec = doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.False(t, bundle.exists, "the retry removes the stale copy before sealing")
		assert.True(t, h.srv.vaultSealedLocked(subject.FromSandboxUID(snapshotTestUID)))
	})
}

func TestVaultSnapshotExportSkipsUnattributableFile(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, snapshotTestUID, snapshotTestIP)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")

	// A leftover from a previous incarnation survived on disk (its purge
	// failed): fenced out by its recorded incarnation, never carried.
	stale, err := credentialvault.EncodeVaultFile("old-runtime", "old-attachment", 7, bundleVaultBody(t))
	require.NoError(t, err)
	f := h.fileFor(snapshotTestUID)
	f.data = stale
	f.exists = true

	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, h.srv.vaultSealedLocked(subject.FromSandboxUID(snapshotTestUID)))
	assert.False(t, h.fakeForPath(bundlePath).exists, "unattributable bytes are never exported")
	assert.Equal(t, stale, f.data, "the source file is left for its fence to discard")
}

func TestVaultSnapshotImportSupersededGuardRequiresFence(t *testing.T) {
	h := newVaultPersistHarness(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
	h.seedBundle(t, bundlePath) // revision 3 under the OLD fence

	// A newer local file under the OLD incarnation (its purge failed): the
	// import for the restored incarnation must supersede it, not ack.
	stale, err := credentialvault.EncodeVaultFile(testFenceR, testFenceA, 9, bundleVaultBody(t))
	require.NoError(t, err)
	sf := h.fileFor(snapshotTestUID)
	sf.data = stale
	sf.exists = true

	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)

	vf, _ := decodeFile(t, sf)
	assert.Equal(t, int64(bundleTestOldRev), vf.Revision, "the stale higher revision was superseded by the bundle")
	assert.Equal(t, snapshotResumeR, vf.Incarnation)
}

func TestVaultPersistenceReservesPath(t *testing.T) {
	dir := t.TempDir()
	vp, err := newVaultPersistence(dir)
	require.NoError(t, err)
	cases := map[string]bool{
		dir:                                 true,  // the directory itself
		filepath.Join(dir, "u.json"):        true,  // a subject's vault file
		filepath.Join(dir, "..victim.json"): true,  // a child whose name begins with dots
		filepath.Join(dir, "sub", "x"):      true,  // any depth inside
		filepath.Join(dir, "..", "out"):     false, // parent traversal
	}
	for path, want := range cases {
		assert.Equal(t, want, vp.reservesPath(path), path)
	}
	outside, err := filepath.Rel(dir, filepath.Join(filepath.Dir(dir), "sibling.json"))
	require.NoError(t, err)
	assert.False(t, vp.reservesPath(filepath.Join(filepath.Dir(dir), "sibling.json")), "sibling path %q must be outside", outside)
	// The snapshot cap must cover the encoded envelope, not just the relay
	// body — otherwise valid vaults cannot survive pause/resume.
	assert.Greater(t, maxVaultSnapshotBytes, 1<<20, "cap must exceed the 1 MiB relay body limit")
}

func TestVaultSnapshotSealSurvivesRestart(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.snapshotSubject(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")

	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)

	// Simulate a handler restart before REMOVE_BINDING: a fresh server over
	// the same persistence state (the fake-backed files simulate the disk
	// and survive), then the Fastlet's binding replay.
	srv2, _, _ := fastSandboxTestServer(t)
	vp2, err := newVaultPersistence(h.dir)
	require.NoError(t, err)
	vp2.newFile = h.vp.newFile
	srv2.SetVaultFiles(vp2)
	subj := subject.FromSandboxUID(snapshotTestUID)

	rec = doAction(t, srv2, bindBody(t, snapshotTestUID, snapshotTestIP, testFenceR, testFenceA, 1, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, srv2.vaultSealedLocked(subj), "binding replay restores the seal from the marker")

	// REMOVE_BINDING purges memory + file + marker; a later re-registration
	// comes back unsealed.
	rec = doAction(t, srv2, removeBody(t, snapshotTestUID, testFenceR, testFenceA))
	require.Equal(t, http.StatusOK, rec.Code)
	marker, err := vp2.markerFor(subj)
	require.NoError(t, err)
	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr), "the purge removes the seal marker")

	rec = doAction(t, srv2, bindBody(t, snapshotTestUID, snapshotTestIP, testFenceR, testFenceA, 1, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, srv2.vaultSealedLocked(subj))
}

func TestVaultSnapshotImportUnseals(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.snapshotSubject(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
	h.seedBundle(t, bundlePath) // real file: import reads the source with plain I/O

	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)
	subj := subject.FromSandboxUID(snapshotTestUID)
	marker, err := h.vp.markerFor(subj)
	require.NoError(t, err)
	_, statErr := os.Stat(marker)
	require.NoError(t, statErr, "precondition: the seal marker exists")

	// Resume on the same fence: the import lands and unseals — vault ops
	// work again.
	rec = doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_IMPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, h.srv.vaultSealedLocked(subj))
	_, statErr = os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr), "the import removes the seal marker")
	rec = doVault(t, h.srv, http.MethodGet, snapshotTestUID, "", "")
	assert.Equal(t, http.StatusOK, rec.Code)
}
