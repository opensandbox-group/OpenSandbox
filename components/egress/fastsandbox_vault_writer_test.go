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
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
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
	for _, tc := range []struct {
		name                      string
		vaultState, rollbackState policy.FileMutationState
		hadEntry                  bool
	}{
		{"rollback-restores-entry", policy.FileUnchanged, policy.FileCommitted, true},
		{"rollback-restores-absence", policy.FileUnchanged, policy.FileCommitted, false},
		{"rollback-unchanged", policy.FileUnchanged, policy.FileUnchanged, true},
		{"rollback-uncertain", policy.FileUnchanged, policy.FileUnknown, true},
		{"vault-uncertain-no-rollback", policy.FileUnknown, policy.FileCommitted, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, owner := writerTestServer(t)
			subj := subject.FromSandboxUID("a")
			if !tc.hadEntry {
				delete(srv.vaults, subj)
			}
			beforeEntry := srv.vaults[subj]
			beforeOwner, err := os.ReadFile(filepath.Join(srv.policyFiles.dir, "a.json.owner"))
			require.NoError(t, err)
			ownerFile := newFakeVaultFile("owner")
			ownerFile.exists, ownerFile.data = true, beforeOwner
			srv.policyFiles.newFile = func(string) (vaultFileStore, error) { return ownerFile, nil }
			f := newFakeVaultFile("vault")
			f.saveErr, f.saveState = errors.New("injected Vault failure"), tc.vaultState
			f.onSave = func() {
				if tc.rollbackState != policy.FileCommitted {
					ownerFile.saveErr, ownerFile.saveState = errors.New("injected rollback failure"), tc.rollbackState
				}
			}
			srv.vaultFiles.files[subj] = f
			require.Equal(t, 503, writerInit(t, srv, owner, true))
			if tc.vaultState == policy.FileUnknown {
				require.Equal(t, 1, ownerFile.saveCalls, "uncertain publication must not roll back authority")
			} else {
				require.Equal(t, 2, ownerFile.saveCalls)
			}
			require.Equal(t, tc.vaultState == policy.FileUnknown || tc.rollbackState == policy.FileUnknown, srv.vaultRecoveryActive())
			if tc.vaultState == policy.FileUnchanged && tc.rollbackState == policy.FileCommitted {
				require.Equal(t, beforeOwner, ownerFile.data)
				entry, exists := srv.vaults[subj]
				require.Equal(t, tc.hadEntry, exists)
				require.Same(t, beforeEntry.store, entry.store)
				f.saveErr, f.saveState, f.onSave = nil, policy.FileCommitted, nil
				require.Equal(t, 200, writerInit(t, srv, owner, true))
			} else if tc.rollbackState == policy.FileUnchanged {
				require.Contains(t, srv.vaults[subj].degraded, "rollback not published")
				require.Equal(t, 409, writerInit(t, srv, owner, true))
				require.Equal(t, 503, writerInit(t, srv, owner, false))
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

func TestVaultWriterExchangeResolverMatchesSubject(t *testing.T) {
	for _, timeout := range []string{"", "5", "invalid"} {
		t.Run("timeout="+timeout, func(t *testing.T) {
			t.Setenv(constants.EnvCredentialResolverTimeout, timeout)
			srv, owner := writerTestServer(t)
			require.Equal(t, 200, writerInit(t, srv, owner, true))
			body := strings.ReplaceAll(exchangeVaultJSON(), "api.internal.example.com", "api.example.com")
			var create credentialvault.CreateRequest
			require.NoError(t, json.Unmarshal([]byte(body), &create))
			legacy := credentialvault.NewSubjectStore()
			_, legacyErr := legacy.Create(create, srv.vaultValidationPolicy(subject.FromSandboxUID("a")))
			status := writerMutation(t, srv, owner, "POST", writerState(t, srv, owner), body)
			if timeout == "invalid" {
				require.Error(t, legacyErr)
				require.Equal(t, 400, status)
				return
			}
			require.NoError(t, legacyErr)
			require.Equal(t, 201, status)
			require.Equal(t, 200, writerInit(t, srv, owner, false))
			require.NotNil(t, writerState(t, srv, owner).Vault)
			// Reload must enforce the same gate as admission, including on a
			// file previously admitted under a valid configuration.
			t.Setenv(constants.EnvCredentialResolverTimeout, "invalid")
			require.Equal(t, 503, writerInit(t, srv, owner, false))
			require.Error(t, credentialvault.NewSubjectStore().Reload(1, create, srv.vaultValidationPolicy(subject.FromSandboxUID("a"))))
		})
	}
}

type writerBlockedResponse struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
	once             sync.Once
}

func (w *writerBlockedResponse) WriteHeader(status int) {
	w.once.Do(func() { close(w.entered); <-w.release })
	w.ResponseRecorder.WriteHeader(status)
}
func (w *writerBlockedResponse) Write(body []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.ResponseRecorder.Write(body)
}
func TestVaultWriterBlockedResponseReleasesGlobalLocks(t *testing.T) {
	for _, operation := range []string{"fresh", "verify", "fresh-conflict", "state", "mutation", "state-precondition"} {
		t.Run(operation, func(t *testing.T) {
			srv, owner := writerTestServer(t)
			other := actionsV2Owner("b", "runtime-b", "attachment-b", 1)
			require.Equal(t, 200, doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationSetBinding, other, vaultAllowPolicy())).Code)
			require.Equal(t, 200, doActionsV2(t, srv, actionsV2Body(t, actionhandler.OperationLifecycleHook, other, nil)).Code)
			require.Equal(t, 200, writerInit(t, srv, other, true))
			method, path, body := "POST", vaultInitializationPrefix+"a", ""
			if operation != "fresh" {
				require.Equal(t, 200, writerInit(t, srv, owner, true))
			}
			if strings.HasPrefix(operation, "state") || operation == "mutation" {
				method, path = "GET", vaultStatePrefix+"a"
				if operation != "state" {
					token := writerState(t, srv, owner).Token
					if operation == "state-precondition" {
						token.Sequence++
					}
					data, err := json.Marshal(map[string]any{"expectedState": token, "vault": json.RawMessage(vaultCredential())})
					require.NoError(t, err)
					method, body = "POST", string(data)
				}
			} else {
				data, err := json.Marshal(map[string]any{"contract": strictVaultContract{vaultWriterVersion, "init-a", "lineage-a"}, "mode": "legacy", "fresh": operation != "verify", "handlerInstanceId": srv.instanceID})
				require.NoError(t, err)
				body = string(data)
			}
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			encoded, err := json.Marshal(owner)
			require.NoError(t, err)
			req.Header.Set(constants.EgressActionsOwnerHeader, base64.RawURLEncoding.EncodeToString(encoded))
			req.Header.Set(constants.EgressActionsSandboxUIDHeader, owner.SandboxUID)
			w := &writerBlockedResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
			done := make(chan struct{})
			go func() { defer close(done); srv.Handler().ServeHTTP(w, req) }()
			defer func() { close(w.release); <-done }()
			select {
			case <-w.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("response did not reach writer")
			}
			probe := make(chan [2]int, 1)
			go func() {
				state := doActionsV2Relay(t, srv, "GET", vaultStatePrefix+"b", other, "").Code
				probe <- [2]int{state, writerInit(t, srv, other, false)}
			}()
			select {
			case codes := <-probe:
				require.Equal(t, [2]int{200, 200}, codes)
			case <-time.After(2 * time.Second):
				t.Fatal("blocked response held a global lock")
			}
		})
	}
}
