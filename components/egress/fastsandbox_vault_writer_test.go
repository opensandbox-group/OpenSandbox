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
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/stretchr/testify/require"
)

func writerTestServer(t *testing.T) (*fastSandboxPolicyServer, actionhandler.Owner) {
	t.Helper()
	srv, _, _ := fastSandboxTestServer(t)
	srv.policyFiles.newFile = func(path string) (vaultFileStore, error) { return policy.NewAtomicPolicyFile(path) }
	vp, err := newVaultPersistence(filepath.Join(t.TempDir(), "vault"))
	require.NoError(t, err)
	srv.SetVaultFiles(vp)
	srv.SetActionsAPIVersion(actionhandler.APIVersionV2)
	owner := actionsV2Owner("a", "runtime-a", "attachment-a", 1)
	set := doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationSetBinding, owner, vaultAllowPolicy()))
	require.Equal(t, 200, set.Code, set.Body.String())
	hook := doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationLifecycleHook, owner, nil))
	require.Equal(t, 200, hook.Code, hook.Body.String())
	return srv, owner
}
func writerInit(t *testing.T, srv *fastSandboxPolicyServer, owner actionhandler.Owner, fresh bool) int {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"contract": strictVaultContract{vaultWriterVersion, "init-a", "lineage-a"}, "mode": "legacy", "fresh": fresh, "handlerInstanceId": srv.instanceID})
	require.NoError(t, err)
	return doActionsV2Relay(t, srv, "POST", vaultInitializationPrefix+owner.SandboxUID, owner, string(payload)).Code
}
func writerState(t *testing.T, srv *fastSandboxPolicyServer, owner actionhandler.Owner) vaultStateReply {
	t.Helper()
	rec := doActionsV2Relay(t, srv, "GET", vaultStatePrefix+owner.SandboxUID, owner, "")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var state vaultStateReply
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &state))
	return state
}
func writerMutation(t *testing.T, srv *fastSandboxPolicyServer, owner actionhandler.Owner, method string, state vaultStateReply, body string) int {
	t.Helper()
	if body == "" {
		body = "{}"
	}
	payload, err := json.Marshal(map[string]any{"expectedState": state.Token, "vault": json.RawMessage(body)})
	require.NoError(t, err)
	return doActionsV2Relay(t, srv, method, vaultStatePrefix+owner.SandboxUID, owner, string(payload)).Code
}
func TestVaultWriterCRUDAndABA(t *testing.T) {
	srv, owner := writerTestServer(t)
	require.Equal(t, 412, writerInit(t, srv, owner, false))
	require.Equal(t, 200, writerInit(t, srv, owner, true))
	require.Equal(t, 409, writerInit(t, srv, owner, true))
	empty := writerState(t, srv, owner)
	require.EqualValues(t, "empty", empty.State)
	require.EqualValues(t, 1, empty.Token.Sequence)
	require.Equal(t, 404, doActionsV2Relay(t, srv, "GET", constants.ActionsV2VaultRelayPathPrefix+"a", owner, "").Code)
	require.Equal(t, 201, writerMutation(t, srv, owner, "POST", empty, vaultCredential()))
	active := writerState(t, srv, owner)
	require.EqualValues(t, 1, active.Vault.Revision)
	require.Equal(t, 204, writerMutation(t, srv, owner, "DELETE", active, ""))
	deleted := writerState(t, srv, owner)
	require.EqualValues(t, "deleted", deleted.State)
	require.Nil(t, deleted.Vault)
	require.Equal(t, 201, writerMutation(t, srv, owner, "POST", deleted, vaultCredential()))
	recreated := writerState(t, srv, owner)
	require.EqualValues(t, 1, recreated.Vault.Revision)
	require.EqualValues(t, 4, recreated.Token.Sequence)
	file := filepath.Join(srv.vaultFiles.dir, "a.json")
	before, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, 412, writerMutation(t, srv, owner, "PUT", active, vaultCredential()))
	after, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, 428, doActionsV2Relay(t, srv, "PUT", constants.ActionsV2VaultRelayPathPrefix+"a", owner, vaultCredential()).Code)
	require.Equal(t, 200, writerMutation(t, srv, owner, "PUT", recreated, vaultCredential()))
	require.EqualValues(t, 2, writerState(t, srv, owner).Vault.Revision)
	// Terminal retirement deletes credential bytes but preserves the owner fence.
	remove := actionsV2Body(t, actionhandler.OperationRemoveBinding, owner, nil)
	rec := doActionsV2(t, srv, remove)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NoFileExists(t, file)
	require.FileExists(t, filepath.Join(srv.policyFiles.dir, "a.json.owner"))
	require.Equal(t, 409, doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationSetBinding, owner, vaultAllowPolicy())).Code)
	require.Equal(t, 200, doActionsV2(t, srv, remove).Code)
}
func TestVaultWriterRestartAndMissingFile(t *testing.T) {
	srv, owner := writerTestServer(t)
	require.Equal(t, 200, writerInit(t, srv, owner, true))
	require.Equal(t, 201, writerMutation(t, srv, owner, "POST", writerState(t, srv, owner), vaultCredential()))
	restarted, _, _ := fastSandboxTestServer(t)
	restarted.SetActionsAPIVersion(actionhandler.APIVersionV2)
	restarted.SetPolicyFiles(srv.policyFiles)
	restarted.SetVaultFiles(srv.vaultFiles)
	rec := doActionsV2(t, restarted, actionsV2Body(t, actionhandler.OperationSetBinding, owner, vaultAllowPolicy()))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, 503, doActionsV2Relay(t, restarted, "GET", vaultStatePrefix+"a", owner, "").Code)
	require.Equal(t, 409, writerInit(t, restarted, owner, true))
	require.Equal(t, 200, writerInit(t, restarted, owner, false))
	require.EqualValues(t, 2, writerState(t, restarted, owner).Token.Sequence)
	require.NoError(t, os.Remove(filepath.Join(srv.vaultFiles.dir, "a.json")))
	require.Equal(t, 503, writerInit(t, restarted, owner, false))
	require.Equal(t, 409, writerInit(t, restarted, owner, true))
	require.Equal(t, 503, doActionsV2Relay(t, restarted, "GET", vaultStatePrefix+"a", owner, "").Code)
}
func TestVaultWriterPartialInitializationAndMutationFailure(t *testing.T) {
	for _, state := range []policy.FileMutationState{policy.FileUnchanged, policy.FileUnknown} {
		t.Run(string(rune('a'+int(state))), func(t *testing.T) {
			srv, owner := writerTestServer(t)
			f := newFakeVaultFile("vault")
			f.saveErr = errors.New("injected")
			f.saveState = state
			srv.vaultFiles.files[subject.FromSandboxUID("a")] = f
			require.Equal(t, 503, writerInit(t, srv, owner, true))
			f.saveErr = nil
			f.saveState = policy.FileCommitted
			// The owner contract exists, but no committed initial file: never fresh.
			if state == policy.FileUnchanged {
				require.Equal(t, 409, writerInit(t, srv, owner, true))
				require.Equal(t, 503, writerInit(t, srv, owner, false))
			} else {
				require.True(t, srv.vaultRecoveryActive())
			}
		})
	}
	srv, owner := writerTestServer(t)
	f := newFakeVaultFile("vault")
	srv.vaultFiles.files[subject.FromSandboxUID("a")] = f
	require.Equal(t, 200, writerInit(t, srv, owner, true))
	empty := writerState(t, srv, owner)
	before := append([]byte(nil), f.data...)
	f.saveErr = errors.New("pre rename")
	f.saveState = policy.FileUnchanged
	require.Equal(t, 503, writerMutation(t, srv, owner, "POST", empty, vaultCredential()))
	require.Equal(t, before, f.data)
	require.False(t, srv.vaultRecoveryActive())
	f.saveErr = nil
	f.saveState = policy.FileCommitted
	require.Equal(t, 201, writerMutation(t, srv, owner, "POST", empty, vaultCredential()))
	active := writerState(t, srv, owner)
	f.saveErr = errors.New("fsync")
	f.saveState = policy.FileUnknown
	require.Equal(t, 503, writerMutation(t, srv, owner, "PUT", active, vaultCredential()))
	require.True(t, srv.vaultRecoveryActive())
}
func TestVaultWriterRejectsWrongOwnerAndUnselectedDomain(t *testing.T) {
	srv, owner := writerTestServer(t)
	require.Equal(t, 404, doActionsV2Relay(t, srv, "GET", vaultStatePrefix+"a", owner, "").Code)
	wrong := owner
	wrong.FastletPodUID = "successor"
	require.Equal(t, 412, writerInit(t, srv, wrong, true))
	require.Equal(t, 400, doActionsV2Relay(t, srv, "POST", vaultInitializationPrefix+"b", owner, "{}").Code)
	require.Equal(t, 200, writerInit(t, srv, owner, true))
	require.Equal(t, 412, writerMutation(t, srv, wrong, "POST", writerState(t, srv, owner), vaultCredential()))
	require.Equal(t, 412, writerMutation(t, srv, owner, "POST", vaultStateReply{}, vaultCredential()))
	require.Equal(t, http.StatusConflict, doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationRemoveBinding, wrong, nil)).Code)
}

func TestVaultWriterInitializationRejectsUnclassifiedBytes(t *testing.T) {
	for _, content := range []string{`{"v":1}`, `{"v":3}`, `broken`} {
		srv, owner := writerTestServer(t)
		file := filepath.Join(srv.vaultFiles.dir, "a.json")
		require.NoError(t, os.WriteFile(file, []byte(content), 0600))
		require.Equal(t, 409, writerInit(t, srv, owner, true))
		got, err := os.ReadFile(file)
		require.NoError(t, err)
		require.Equal(t, content, string(got))
	}
}
func TestVaultWriterContractMismatchAndMissingOwner(t *testing.T) {
	srv, owner := writerTestServer(t)
	require.Equal(t, 200, writerInit(t, srv, owner, true))
	state := writerState(t, srv, owner)
	wrong := state
	wrong.Token.Lineage = "other"
	require.Equal(t, 412, writerMutation(t, srv, owner, "POST", wrong, vaultCredential()))
	require.Equal(t, 201, writerMutation(t, srv, owner, "POST", state, vaultCredential()))
	file := filepath.Join(srv.vaultFiles.dir, "a.json")
	before, err := os.ReadFile(file)
	require.NoError(t, err)
	// Missing authority is not inferred from the surviving v2 file.
	require.NoError(t, os.Remove(filepath.Join(srv.policyFiles.dir, "a.json.owner")))
	require.Equal(t, 412, writerInit(t, srv, owner, true))
	require.Equal(t, 412, doActionsV2Relay(t, srv, "GET", vaultStatePrefix+"a", owner, "").Code)
	after, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
func TestVaultWriterConcurrentFreshAndVerifyAckRecovery(t *testing.T) {
	srv, owner := writerTestServer(t)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- writerInit(t, srv, owner, true) }()
	}
	wg.Wait()
	close(codes)
	var success, conflict int
	for code := range codes {
		if code == 200 {
			success++
		}
		if code == 409 {
			conflict++
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, conflict)
	// Ignore the original ACK and verify; no fresh bit is replayed.
	require.Equal(t, 200, writerInit(t, srv, owner, false))
	require.EqualValues(t, 1, writerState(t, srv, owner).Token.Sequence)
}
func TestVaultWriterRetirementUncertaintyRetainsFence(t *testing.T) {
	srv, owner := writerTestServer(t)
	f := newFakeVaultFile("vault")
	srv.vaultFiles.files[subject.FromSandboxUID("a")] = f
	require.Equal(t, 200, writerInit(t, srv, owner, true))
	f.deleteErr = errors.New("pre unlink")
	f.deleteState = policy.FileUnchanged
	remove := actionsV2Body(t, actionhandler.OperationRemoveBinding, owner, nil)
	require.Equal(t, 503, doActionsV2(t, srv, remove).Code)
	record, err := srv.readOwnerRecord(subject.FromSandboxUID("a"))
	require.NoError(t, err)
	require.Equal(t, "retiring", record.Phase)
	require.Equal(t, 409, doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationSetBinding, owner, vaultAllowPolicy())).Code)
	f.deleteErr = nil
	f.deleteState = policy.FileCommitted
	require.Equal(t, 200, doActionsV2(t, srv, remove).Code)
	record, err = srv.readOwnerRecord(subject.FromSandboxUID("a"))
	require.NoError(t, err)
	require.Equal(t, "retired", record.Phase)
}
func TestVaultWriterPublicRevisionLimitStillAllowsDelete(t *testing.T) {
	srv, owner := writerTestServer(t)
	require.Equal(t, 200, writerInit(t, srv, owner, true))
	require.Equal(t, 201, writerMutation(t, srv, owner, "POST", writerState(t, srv, owner), vaultCredential()))
	subj := subject.FromSandboxUID("a")
	record, err := srv.readOwnerRecord(subj)
	require.NoError(t, err)
	vf, body, err := srv.writerFile(subj, record)
	require.NoError(t, err)
	vf.Revision = math.MaxInt64
	data, err := credentialvault.EncodeVaultFileV2(vf, body)
	require.NoError(t, err)
	f, err := srv.vaultFiles.storeFor(subj)
	require.NoError(t, err)
	_, err = f.SaveRaw(data)
	require.NoError(t, err)
	require.Equal(t, 200, writerInit(t, srv, owner, false))
	state := writerState(t, srv, owner)
	require.Equal(t, 409, writerMutation(t, srv, owner, "PUT", state, vaultCredential()))
	require.Equal(t, 204, writerMutation(t, srv, owner, "DELETE", state, ""))
}

// Simulate unlink publication followed by an uncertain durability barrier.
type writerUncertainUnlink struct{ *fakeVaultFile }

func (f *writerUncertainUnlink) Delete() (policy.FileMutationState, error) {
	f.exists = false
	f.data = nil
	return policy.FileUnknown, errors.New("directory fsync failed")
}
func TestVaultWriterUncertainUnlinkCannotAckRetry(t *testing.T) {
	srv, owner := writerTestServer(t)
	f := &writerUncertainUnlink{newFakeVaultFile("vault")}
	srv.vaultFiles.files[subject.FromSandboxUID("a")] = f
	require.Equal(t, 200, writerInit(t, srv, owner, true))
	remove := actionsV2Body(t, actionhandler.OperationRemoveBinding, owner, nil)
	require.Equal(t, 503, doActionsV2(t, srv, remove).Code)
	require.False(t, f.exists)
	require.True(t, srv.vaultRecoveryActive())
	require.Equal(t, 503, doActionsV2(t, srv, remove).Code)
	record, err := srv.readOwnerRecord(subject.FromSandboxUID("a"))
	require.NoError(t, err)
	require.Equal(t, "retiring", record.Phase)
}
func TestVaultWriterInitializationPinsHandlerIncarnation(t *testing.T) {
	srv, owner := writerTestServer(t)
	payload, _ := json.Marshal(map[string]any{"contract": strictVaultContract{vaultWriterVersion, "init-a", "lineage-a"}, "mode": "legacy", "fresh": true, "handlerInstanceId": "old-process"})
	require.Equal(t, 412, doActionsV2Relay(t, srv, "POST", vaultInitializationPrefix+"a", owner, string(payload)).Code)
	record, err := srv.readOwnerRecord(subject.FromSandboxUID("a"))
	require.NoError(t, err)
	require.Nil(t, record.VaultContract)
	require.Equal(t, 200, writerInit(t, srv, owner, true))
	require.Equal(t, srv.instanceID, writerState(t, srv, owner).HandlerInstanceID)
}
