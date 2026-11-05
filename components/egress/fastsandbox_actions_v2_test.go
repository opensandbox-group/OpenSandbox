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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/stretchr/testify/require"
)

func actionsV2Owner(uid, runtimeID, attachmentID string, generation int64) actionhandler.Owner {
	return actionhandler.Owner{
		SandboxUID: uid, FastletPodUID: "fastlet-pod", RuntimeInstanceID: runtimeID,
		InstanceGeneration: generation, AssignmentAttempt: 1, RouteGeneration: 1, AttachmentID: attachmentID,
	}
}

func actionsV2Body(t *testing.T, operation actionhandler.Operation, owner actionhandler.Owner, input *string) string {
	t.Helper()
	env := actionhandler.Envelope{
		Mode:       "legacy",
		APIVersion: actionhandler.APIVersionV2, Operation: operation, InvocationID: "v2-test",
		Sandbox: actionhandler.SandboxRef{UID: owner.SandboxUID}, Owner: &owner,
		Revision: actionhandler.Revision{SpecGeneration: 1, RuntimeInstanceID: owner.RuntimeInstanceID, AttachmentID: owner.AttachmentID, RouteGeneration: uint64(owner.RouteGeneration)},
	}
	if operation == actionhandler.OperationSetBinding {
		env.Attachment = testAttachment()
		if owner.SandboxUID == "b" {
			env.Attachment.Network.IP = netip.MustParseAddr("10.0.0.6")
		}
		if input == nil {
			null := []byte("null")
			env.Binding = &actionhandler.Binding{Input: null}
		} else {
			encoded, err := json.Marshal(*input)
			require.NoError(t, err)
			env.Binding = &actionhandler.Binding{Input: encoded}
		}
	}
	if operation == actionhandler.OperationLifecycleHook {
		env.Hook = &actionhandler.Hook{Name: actionhandler.HookDataPlaneReady, Sequence: 2}
	}
	encoded, err := json.Marshal(env)
	require.NoError(t, err)
	return string(encoded)
}

func doActionsV2(t *testing.T, srv *fastSandboxPolicyServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, srv, http.MethodPost, constants.ActionsV2DispatchPath, body)
}

func doActionsV2Relay(t *testing.T, srv *fastSandboxPolicyServer, method, path string, owner actionhandler.Owner, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	encoded, err := json.Marshal(owner)
	require.NoError(t, err)
	req.Header.Set(constants.EgressActionsOwnerHeader, base64.RawURLEncoding.EncodeToString(encoded))
	req.Header.Set(constants.EgressActionsSandboxUIDHeader, owner.SandboxUID)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestActionsV2IsOptInAndDoesNotMountLegacyMutationRoutes(t *testing.T) {
	srv, _, _ := fastSandboxTestServer(t)
	legacy := srv.Handler()
	for _, path := range []string{constants.ActionsV2StatusPath, constants.ActionsV2DispatchPath, constants.ActionsV2VaultRelayPathPrefix + "a"} {
		rec := httptest.NewRecorder()
		legacy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusNotFound, rec.Code, path)
	}

	srv.SetActionsAPIVersion(actionhandler.APIVersionV2)
	strict := srv.Handler()
	for _, path := range []string{constants.ActionsStatusPath, constants.ActionsDispatchPath, constants.VaultRelayPathPrefix + "a"} {
		rec := httptest.NewRecorder()
		strict.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusNotFound, rec.Code, path)
	}
	status := httptest.NewRecorder()
	strict.ServeHTTP(status, httptest.NewRequest(http.MethodGet, constants.ActionsV2StatusPath, nil))
	require.Equal(t, http.StatusOK, status.Code)
	var response actionhandler.StatusResponse
	require.NoError(t, json.Unmarshal(status.Body.Bytes(), &response))
	require.Equal(t, actionhandler.APIVersionV2, response.APIVersion)
	require.True(t, response.OwnerStatus)
	require.False(t, response.VaultTransfer)
	require.False(t, response.PolicyTransfer)
	require.False(t, response.TemplateStateExport)
}

func TestActionsV2SeparatesSubjectsAndFencesOwnerChanges(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	srv.SetActionsAPIVersion(actionhandler.APIVersionV2)
	policyInput := vaultAllowPolicy()
	a := actionsV2Owner("a", "runtime-a", "attachment-a", 1)
	b := actionsV2Owner("b", "runtime-b", "attachment-b", 1)
	require.Equal(t, http.StatusOK, doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationSetBinding, a, policyInput)).Code)
	require.Equal(t, http.StatusOK, doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationSetBinding, b, policyInput)).Code)
	require.Equal(t, http.StatusOK, doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationLifecycleHook, a, nil)).Code)

	subjA, subjB := subject.FromSandboxUID("a"), subject.FromSandboxUID("b")
	beforeDeny, beforePolicy := len(nft.denyFirst), len(nft.policyApplied)
	newOwnerA := actionsV2Owner("a", "runtime-a2", "attachment-a2", 2)
	require.Equal(t, http.StatusConflict, doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationSetBinding, newOwnerA, policyInput)).Code,
		"a replacement owner must first retire the old exact owner")
	require.Len(t, nft.denyFirst, beforeDeny)
	require.Len(t, nft.policyApplied, beforePolicy)

	require.Equal(t, http.StatusOK, doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationRemoveBinding, a, nil)).Code)
	_, foundA := reg.Fence(subjA)
	_, foundB := reg.Fence(subjB)
	require.False(t, foundA)
	require.True(t, foundB, "retiring subject A must not affect B")
	srv.mu.Lock()
	gotB, foundB := srv.actionOwners[subjB]
	srv.mu.Unlock()
	require.True(t, foundB)
	require.True(t, gotB.Matches(b))

	newOwnerABody := actionsV2Body(t, actionhandler.OperationSetBinding, newOwnerA, policyInput)
	require.Equal(t, http.StatusConflict, doActionsV2(t, srv, newOwnerABody).Code, "strict rebind requires a new Sandbox UID")
}

func TestActionsV2RejectsSnapshotAndRelayPathOwnerMismatchBeforeEffects(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	srv.SetActionsAPIVersion(actionhandler.APIVersionV2)
	owner := actionsV2Owner("a", "runtime-a", "attachment-a", 1)
	require.Equal(t, http.StatusOK, doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationSetBinding, owner, vaultAllowPolicy())).Code)
	fenceBefore, registeredBefore := reg.Fence(subject.FromSandboxUID("a"))
	require.True(t, registeredBefore)
	beforeDeny := len(nft.denyFirst)
	snapshot, err := json.Marshal(actionhandler.Envelope{
		Mode:       "legacy",
		APIVersion: actionhandler.APIVersionV2, Operation: actionhandler.OperationVaultExport, InvocationID: "snap",
		Sandbox: actionhandler.SandboxRef{UID: "a"}, Revision: actionhandler.Revision{SpecGeneration: 1, RuntimeInstanceID: owner.RuntimeInstanceID, AttachmentID: owner.AttachmentID, RouteGeneration: 1},
		Owner: &owner, Snapshot: &actionhandler.SnapshotTransfer{Path: "/tmp/state"},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusNotImplemented, doActionsV2(t, srv, string(snapshot)).Code)
	fenceAfter, registered := reg.Fence(subject.FromSandboxUID("a"))
	require.True(t, registered)
	require.True(t, fenceBefore.Matches(fenceAfter))
	require.Len(t, nft.denyFirst, beforeDeny)

	other := actionsV2Owner("b", "runtime-b", "attachment-b", 1)
	rec := doActionsV2Relay(t, srv, http.MethodGet, constants.ActionsV2VaultRelayPathPrefix+"b", owner, "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	_, registered = reg.Fence(subject.FromSandboxUID("b"))
	require.False(t, registered)
	require.NotEqual(t, owner.SandboxUID, other.SandboxUID)
}

func TestActionsV2CarriesOwnerThroughVaultAndPolicyRelayCRUD(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	srv.SetActionsAPIVersion(actionhandler.APIVersionV2)
	input := vaultAllowPolicy()
	ownerA := actionsV2Owner("a", "runtime-a", "attachment-a", 1)
	ownerB := actionsV2Owner("b", "runtime-b", "attachment-b", 1)
	require.Equal(t, http.StatusOK, doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationSetBinding, ownerA, input)).Code)
	require.Equal(t, http.StatusOK, doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationLifecycleHook, ownerA, nil)).Code)

	created := doActionsV2Relay(t, srv, http.MethodPost, constants.ActionsV2VaultRelayPathPrefix+"a", ownerA, vaultCredential())
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	readVault := doActionsV2Relay(t, srv, http.MethodGet, constants.ActionsV2VaultRelayPathPrefix+"a", ownerA, "")
	require.Equal(t, http.StatusOK, readVault.Code, readVault.Body.String())
	require.Contains(t, readVault.Body.String(), `"name":"tok"`)

	readPolicy := doActionsV2Relay(t, srv, http.MethodGet, constants.ActionsV2PolicyRelayPathPrefix+"a", ownerA, "")
	require.Equal(t, http.StatusOK, readPolicy.Code, readPolicy.Body.String())
	revision, current := mustPolicyRevision(t, readPolicy)
	require.EqualValues(t, 1, revision)
	updated := doActionsV2Relay(t, srv, http.MethodPut, constants.ActionsV2PolicyRelayPathPrefix+"a", ownerA,
		policyPutDoc(policyAllowAlt(), revision, true, false))
	require.Equal(t, http.StatusOK, updated.Code, updated.Body.String())
	require.Contains(t, updated.Body.String(), "other.example.com")
	_ = current

	deleted := doActionsV2Relay(t, srv, http.MethodDelete, constants.ActionsV2VaultRelayPathPrefix+"a", ownerA, "")
	require.Equal(t, http.StatusNoContent, deleted.Code, deleted.Body.String())

	// A different, unbound Sandbox cannot use A's shared management routes.
	for _, request := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, constants.ActionsV2VaultRelayPathPrefix + "b", ""},
		{http.MethodPost, constants.ActionsV2VaultRelayPathPrefix + "b", vaultCredential()},
		{http.MethodGet, constants.ActionsV2PolicyRelayPathPrefix + "b", ""},
	} {
		rec := doActionsV2Relay(t, srv, request.method, request.path, ownerB, request.body)
		require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
	}
	_, found := reg.Fence(subject.FromSandboxUID("b"))
	require.False(t, found, "relay must not materialize an unbound subject")
}

func TestActionsV2RequiresModeBeforeSideEffects(t *testing.T) {
	for _, mode := range []string{"", "explicit", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			srv, reg, _ := fastSandboxTestServer(t)
			srv.SetActionsAPIVersion(actionhandler.APIVersionV2)
			owner := actionsV2Owner("a", "runtime-a", "attachment-a", 1)
			var body map[string]any
			require.NoError(t, json.Unmarshal([]byte(actionsV2Body(t, actionhandler.OperationSetBinding, owner, vaultAllowPolicy())), &body))
			body["mode"] = mode
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			require.Equal(t, http.StatusBadRequest, doActionsV2(t, srv, string(raw)).Code)
			_, registered := reg.Fence(subject.FromSandboxUID("a"))
			require.False(t, registered)
			record, err := srv.readOwnerRecord(subject.FromSandboxUID("a"))
			require.NoError(t, err)
			require.Nil(t, record)
		})
	}
}

func TestActionsV2RetirementSurvivesRestartAndRejectsOldSet(t *testing.T) {
	srv, _, _ := fastSandboxTestServer(t)
	// Exercise actual atomic files, not just the in-memory file seam.
	srv.policyFiles.newFile = func(path string) (vaultFileStore, error) { return policy.NewAtomicPolicyFile(path) }
	srv.SetActionsAPIVersion(actionhandler.APIVersionV2)
	owner := actionsV2Owner("a", "runtime-a", "attachment-a", 1)
	set := actionsV2Body(t, actionhandler.OperationSetBinding, owner, vaultAllowPolicy())
	remove := actionsV2Body(t, actionhandler.OperationRemoveBinding, owner, nil)
	require.Equal(t, http.StatusOK, doActionsV2(t, srv, set).Code)
	require.Equal(t, http.StatusOK, doActionsV2(t, srv, remove).Code)
	require.FileExists(t, filepath.Join(srv.policyFiles.dir, "a.json.owner"))
	require.Error(t, validateOwnerDomain(srv.policyFiles.dir, constants.ActionsAPIVersion))
	require.NoError(t, validateOwnerDomain(srv.policyFiles.dir, constants.ActionsAPIVersionV2))
	require.Equal(t, http.StatusConflict, doActionsV2(t, srv, set).Code)
	restarted, reg, _ := fastSandboxTestServer(t)
	restarted.SetPolicyFiles(srv.policyFiles)
	restarted.SetActionsAPIVersion(actionhandler.APIVersionV2)
	require.Equal(t, http.StatusConflict, doActionsV2(t, restarted, set).Code)
	require.Equal(t, http.StatusOK, doActionsV2(t, restarted, remove).Code)
	observation := doActionsV2Relay(t, restarted, http.MethodGet, constants.ActionsV2OwnerPathPrefix+"a", owner, "")
	require.Equal(t, http.StatusOK, observation.Code)
	require.Contains(t, observation.Body.String(), `"state":"retired"`)
	_, registered := reg.Fence(subject.FromSandboxUID("a"))
	require.False(t, registered)
}

func TestActionsV2RetiresAnOwnerBeforeItsFirstSet(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	srv.SetActionsAPIVersion(actionhandler.APIVersionV2)
	owner := actionsV2Owner("a", "runtime-a", "attachment-a", 1)
	observation := doActionsV2Relay(t, srv, http.MethodGet, constants.ActionsV2OwnerPathPrefix+"a", owner, "")
	require.Equal(t, http.StatusOK, observation.Code)
	require.Contains(t, observation.Body.String(), `"state":"absent"`)
	require.Equal(t, http.StatusOK, doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationRemoveBinding, owner, nil)).Code)
	require.Equal(t, http.StatusConflict, doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationSetBinding, owner, vaultAllowPolicy())).Code)
	_, registered := reg.Fence(subject.FromSandboxUID("a"))
	require.False(t, registered)
}

func TestActionsV2RestoresSameOwnerButRejectsUnknownPersistentState(t *testing.T) {
	srv, _, _ := fastSandboxTestServer(t)
	srv.SetActionsAPIVersion(actionhandler.APIVersionV2)
	owner := actionsV2Owner("a", "runtime-a", "attachment-a", 1)
	set := actionsV2Body(t, actionhandler.OperationSetBinding, owner, vaultAllowPolicy())
	require.Equal(t, http.StatusOK, doActionsV2(t, srv, set).Code)
	restarted, _, _ := fastSandboxTestServer(t)
	restarted.SetPolicyFiles(srv.policyFiles)
	restarted.SetActionsAPIVersion(actionhandler.APIVersionV2)
	require.Equal(t, http.StatusOK, doActionsV2(t, restarted, set).Code)
	f, err := restarted.ownerFile(subject.FromSandboxUID("a"))
	require.NoError(t, err)
	_, err = f.SaveRaw([]byte(`{"version":"unknown"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, doActionsV2(t, restarted, set).Code)
	require.Equal(t, http.StatusPreconditionFailed, doActionsV2Relay(t, restarted, http.MethodPost, constants.ActionsV2VaultRelayPathPrefix+"a", owner, vaultCredential()).Code)
}

func TestActionsV2OwnerPersistenceAndRetirementFailuresRemainClosed(t *testing.T) {
	srv, reg, nft := fastSandboxTestServer(t)
	srv.SetActionsAPIVersion(actionhandler.APIVersionV2)
	owner := actionsV2Owner("a", "runtime-a", "attachment-a", 1)
	subj := subject.FromSandboxUID("a")
	file, err := srv.ownerFile(subj)
	require.NoError(t, err)
	fault := file.(*fakeVaultFile)
	fault.saveState, fault.saveErr = policy.FileUnknown, fmt.Errorf("injected uncertain owner persistence")
	set := actionsV2Body(t, actionhandler.OperationSetBinding, owner, vaultAllowPolicy())
	require.Equal(t, http.StatusServiceUnavailable, doActionsV2(t, srv, set).Code)
	_, registered := reg.Fence(subj)
	require.False(t, registered, "no SET effects before owner persistence")
	fault.saveState, fault.saveErr = policy.FileCommitted, nil
	require.Equal(t, http.StatusOK, doActionsV2(t, srv, set).Code)
	nft.removeErr = fmt.Errorf("injected cleanup failure")
	remove := actionsV2Body(t, actionhandler.OperationRemoveBinding, owner, nil)
	require.NotEqual(t, http.StatusOK, doActionsV2(t, srv, remove).Code)
	record, err := srv.readOwnerRecord(subj)
	require.NoError(t, err)
	require.Equal(t, "retiring", record.Phase)
	require.Equal(t, http.StatusConflict, doActionsV2(t, srv, set).Code)
	nft.removeErr = nil
	require.Equal(t, http.StatusOK, doActionsV2(t, srv, remove).Code)
	record, err = srv.readOwnerRecord(subj)
	require.NoError(t, err)
	require.Equal(t, "retired", record.Phase)
}
