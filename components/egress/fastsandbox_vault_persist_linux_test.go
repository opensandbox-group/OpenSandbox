//go:build linux

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
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// newRealVaultTestServer assembles the standard test server wired to real
// AtomicPolicyFile storage under dir.
func newRealVaultTestServer(t *testing.T, dir string) (*fastSandboxPolicyServer, *subject.MemoryRegistry) {
	t.Helper()
	srv, reg, _ := fastSandboxTestServer(t)
	vp, err := newVaultPersistence(dir)
	require.NoError(t, err)
	srv.SetVaultFiles(vp)
	return srv, reg
}

// TestVaultPersistenceRealFileEndToEnd runs the persistence story on real
// AtomicPolicyFile storage: write lands as a file, a new server generation
// reloads it at SET_BINDING, a corrupt file degrades until a write recovers.
func TestVaultPersistenceRealFileEndToEnd(t *testing.T) {
	dir := t.TempDir()
	srv, _ := newRealVaultTestServer(t, dir)

	s := subject.FromSandboxUID(vaultTestUID)
	rec := doAction(t, srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 1, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, vaultTestUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)

	// Relay create → the committed file exists on disk, stamped with the
	// owning runtime instance.
	rec = doVault(t, srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)
	path := filepath.Join(dir, vaultTestUID+".json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	vf, _, err := credentialvault.DecodeVaultFile(raw)
	require.NoError(t, err)
	assert.Equal(t, testFenceR, vf.Incarnation)
	assert.Equal(t, int64(1), vf.Revision)

	// A new server generation (egress restart): the replayed SET_BINDING
	// reloads the committed file — same incarnation, revision continuity.
	srv2, _ := newRealVaultTestServer(t, dir)
	rec = doAction(t, srv2, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 1, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv2, hookBody(t, vaultTestUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doVault(t, srv2, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":1`)
	assert.Equal(t, http.StatusOK, doRequest(t, srv2, http.MethodGet, "/healthz", "").Code)

	// A corrupt file degrades the subject at the NEXT registration: policy
	// enforcement continues, the actions status payload lists it, and a
	// successful relay write clears the degradation. healthz stays green —
	// a per-subject degradation is never a process-health failure.
	require.NoError(t, os.WriteFile(path, []byte("{corrupt"), 0o600))
	srv3, reg3 := newRealVaultTestServer(t, dir)
	rec = doAction(t, srv3, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 1, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv3, hookBody(t, vaultTestUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	state, ok := reg3.Get(s)
	require.True(t, ok)
	assert.Equal(t, subject.StateActive, state, "policy enforcement is unaffected by a degraded vault")
	assert.Equal(t, http.StatusOK, doRequest(t, srv3, http.MethodGet, "/healthz", "").Code)
	status := mustStatus(t, srv3)
	require.Equal(t, []string{vaultTestUID}, status.DegradedVaults)

	rec = doVault(t, srv3, http.MethodPut, vaultTestUID, vaultFullBody(), "")
	require.Equal(t, http.StatusCreated, rec.Code, "the successful write clears the degradation")
	assert.Equal(t, http.StatusOK, doRequest(t, srv3, http.MethodGet, "/healthz", "").Code)
	status = mustStatus(t, srv3)
	assert.Empty(t, status.DegradedVaults)
	rec = doVault(t, srv3, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":1`)

	// REMOVE_BINDING purges memory + file (terminal).
	rec = doAction(t, srv3, removeBody(t, vaultTestUID, testFenceR, testFenceA))
	require.Equal(t, http.StatusOK, rec.Code)
	_, err = os.Lstat(path)
	assert.True(t, os.IsNotExist(err))
}

func TestVaultV2ReloadUsesRealFileAndNeverDowngrades(t *testing.T) {
	for _, state := range []credentialvault.FileState{credentialvault.FileStateActive, credentialvault.FileStateDeleted} {
		t.Run(string(state), func(t *testing.T) {
			dir := t.TempDir()
			uid := vaultTestUID
			revision := int64(7)
			body := bundleVaultBody(t)
			if state == credentialvault.FileStateDeleted {
				revision = 0
				body = emptyVaultFileBody()
			}
			raw := encodeVaultV2Fixture(t, state, revision, 12, "", body)
			path := filepath.Join(dir, uid+".json")
			require.NoError(t, os.WriteFile(path, raw, 0o600))

			srv, _ := newRealVaultTestServer(t, dir)
			require.Equal(t, http.StatusOK, doAction(t, srv, bindBody(t, uid, vaultTestIP, testFenceR, testFenceA, 1, vaultAllowPolicy())).Code)
			require.Equal(t, http.StatusOK, doAction(t, srv, hookBody(t, uid, testFenceR, testFenceA, constants.HookRuntimeReady)).Code)
			require.Equal(t, http.StatusOK, doAction(t, srv, hookBody(t, uid, testFenceR, testFenceA, constants.HookDataPlaneReady)).Code)

			get := doVault(t, srv, http.MethodGet, uid, "", "")
			if state == credentialvault.FileStateActive {
				require.Equal(t, http.StatusOK, get.Code)
				assert.Contains(t, get.Body.String(), `"revision":7`)
			} else {
				require.Equal(t, http.StatusNotFound, get.Code)
			}
			assert.Equal(t, http.StatusConflict, doVault(t, srv, http.MethodPost, uid, vaultCredential(), "").Code)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, raw, got, "real persisted v2 bytes survive reload and rejected mutation")
		})
	}
}
