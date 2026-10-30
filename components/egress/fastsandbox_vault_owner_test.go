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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/stretchr/testify/require"
)

func writerActive(srv *fastSandboxPolicyServer, tag string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp=10.0.0.5", nil)
	req.Header.Set("If-None-Match", tag)
	rec := httptest.NewRecorder()
	srv.handleCredentialVaultActive(rec, req)
	return rec
}

func activeWriterTestServer(t *testing.T) (*fastSandboxPolicyServer, actionhandler.Owner, string) {
	t.Helper()
	srv, owner := writerTestServer(t)
	require.Equal(t, 200, writerInit(t, srv, owner, true))
	require.Equal(t, 201, writerMutation(t, srv, owner, "POST", writerState(t, srv, owner), vaultCredential()))
	active := writerActive(srv, "")
	require.Equal(t, 200, active.Code)
	tag := active.Header().Get("ETag")
	require.NotEmpty(t, tag)
	require.Equal(t, 304, writerActive(srv, tag).Code)
	return srv, owner, tag
}

func TestVaultWriterOwnerAuthorityLoss(t *testing.T) {
	for _, operation := range []string{"verify", "state", "mutation"} {
		for _, fault := range []string{"missing", "malformed", "snapshot", "open"} {
			t.Run(operation+"/"+fault, func(t *testing.T) {
				srv, owner, tag := activeWriterTestServer(t)
				prior := writerState(t, srv, owner)
				ownerPath := filepath.Join(srv.policyFiles.dir, "a.json.owner")
				vaultPath := filepath.Join(srv.vaultFiles.dir, "a.json")
				ownerBytes, err := os.ReadFile(ownerPath)
				require.NoError(t, err)
				vaultBytes, err := os.ReadFile(vaultPath)
				require.NoError(t, err)
				factory := srv.policyFiles.newFile
				switch fault {
				case "missing":
					require.NoError(t, os.Remove(ownerPath))
				case "malformed":
					require.NoError(t, os.WriteFile(ownerPath, []byte("{broken"), 0600))
				case "snapshot", "open":
					srv.policyFiles.newFile = func(path string) (vaultFileStore, error) {
						require.Equal(t, ownerPath, path)
						if fault == "open" {
							return nil, errors.New("injected owner open failure")
						}
						return &fakeVaultFile{snapErr: errors.New("injected owner Snapshot failure")}, nil
					}
				}
				stale := owner
				stale.RuntimeInstanceID = "obsolete-runtime"
				require.Equal(t, 412, doActionsV2Relay(t, srv, "GET", vaultStatePrefix+"a", stale, "").Code)
				require.Equal(t, 304, writerActive(srv, tag).Code)
				switch operation {
				case "verify":
					require.Equal(t, 412, writerInit(t, srv, owner, false))
				case "state":
					require.Equal(t, 412, doActionsV2Relay(t, srv, "GET", vaultStatePrefix+"a", owner, "").Code)
				case "mutation":
					require.Equal(t, 412, writerMutation(t, srv, owner, "DELETE", prior, ""))
				}
				entry := srv.vaults[subject.FromSandboxUID("a")]
				require.False(t, entry.v2Verified)
				require.True(t, entry.vaultStateUnknown)
				_, err = entry.store.Full()
				require.ErrorIs(t, err, credentialvault.ErrNotFound)
				require.Equal(t, 503, writerActive(srv, "").Code)
				require.Equal(t, 503, writerActive(srv, tag).Code)
				after, err := os.ReadFile(vaultPath)
				require.NoError(t, err)
				require.Equal(t, vaultBytes, after)
				// Repairing the authority is insufficient: only verify can republish.
				srv.policyFiles.newFile = factory
				require.NoError(t, os.WriteFile(ownerPath, ownerBytes, 0600))
				require.Equal(t, 503, doActionsV2Relay(t, srv, "GET", vaultStatePrefix+"a", owner, "").Code)
				require.Equal(t, 409, writerInit(t, srv, owner, true))
				require.Equal(t, 503, writerActive(srv, tag).Code)
				require.Equal(t, 200, writerInit(t, srv, owner, false))
				require.Equal(t, prior.Token, writerState(t, srv, owner).Token)
				require.Equal(t, 200, writerActive(srv, "").Code)
				after, err = os.ReadFile(vaultPath)
				require.NoError(t, err)
				require.Equal(t, vaultBytes, after)
			})
		}
	}
}

func TestVaultWriterInvalidRequestPreservesLiveStore(t *testing.T) {
	for _, missing := range []bool{false, true} {
		for _, invalid := range []string{"owner", "path", "header", "missing-header", "contract", "mode", "instance"} {
			t.Run(invalid+"/missing="+map[bool]string{false: "false", true: "true"}[missing], func(t *testing.T) {
				srv, owner, tag := activeWriterTestServer(t)
				vaultPath := filepath.Join(srv.vaultFiles.dir, "a.json")
				before, err := os.ReadFile(vaultPath)
				require.NoError(t, err)
				if missing {
					require.NoError(t, os.Remove(filepath.Join(srv.policyFiles.dir, "a.json.owner")))
				}
				contract := strictVaultContract{vaultWriterVersion, "init-a", "lineage-a"}
				mode, instance, uid := "legacy", srv.instanceID, "a"
				switch invalid {
				case "owner":
					owner.RuntimeInstanceID = "obsolete-runtime"
				case "path":
					uid = "b"
				case "contract":
					contract.Lineage = "wrong-lineage"
				case "mode":
					mode = "explicit"
				case "instance":
					instance = "old-process"
				}
				payload, err := json.Marshal(map[string]any{"contract": contract, "mode": mode, "fresh": false, "handlerInstanceId": instance})
				require.NoError(t, err)
				req := httptest.NewRequest("POST", vaultInitializationPrefix+uid, strings.NewReader(string(payload)))
				encoded, err := json.Marshal(owner)
				require.NoError(t, err)
				req.Header.Set(constants.EgressActionsOwnerHeader, base64.RawURLEncoding.EncodeToString(encoded))
				req.Header.Set(constants.EgressActionsSandboxUIDHeader, owner.SandboxUID)
				if invalid == "header" {
					req.Header.Set(constants.EgressActionsSandboxUIDHeader, "b")
				}
				if invalid == "missing-header" {
					req.Header.Del(constants.EgressActionsOwnerHeader)
				}
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, req)
				require.GreaterOrEqual(t, rec.Code, 400)
				entry := srv.vaults[subject.FromSandboxUID("a")]
				require.True(t, entry.v2Verified)
				require.False(t, entry.vaultStateUnknown)
				require.Equal(t, 200, writerActive(srv, "").Code)
				require.Equal(t, 304, writerActive(srv, tag).Code)
				after, err := os.ReadFile(vaultPath)
				require.NoError(t, err)
				require.Equal(t, before, after)
			})
		}
	}
}

func TestVaultWriterReadsOwnerOnce(t *testing.T) {
	for _, operation := range []string{"verify", "state"} {
		t.Run(operation, func(t *testing.T) {
			srv, owner, _ := activeWriterTestServer(t)
			factory := srv.policyFiles.newFile
			reads := 0
			srv.policyFiles.newFile = func(path string) (vaultFileStore, error) {
				reads++
				if reads > 1 {
					return nil, errors.New("second owner read must not occur")
				}
				return factory(path)
			}
			if operation == "verify" {
				require.Equal(t, 200, writerInit(t, srv, owner, false))
			} else {
				writerState(t, srv, owner)
			}
			require.Equal(t, 1, reads)
		})
	}
}

// A pending REMOVE has its own lifecycle; do not turn a valid retiring record
// into authority loss just because a late state request is rejected.
func TestVaultWriterRetiringOwnerPreservesPendingDelete(t *testing.T) {
	srv, owner, tag := activeWriterTestServer(t)
	require.NoError(t, srv.writeOwnerRecord(subject.FromSandboxUID("a"), owner, "retiring"))
	require.Equal(t, 412, writerInit(t, srv, owner, false))
	require.Equal(t, 412, doActionsV2Relay(t, srv, "GET", vaultStatePrefix+"a", owner, "").Code)
	require.Equal(t, 304, writerActive(srv, tag).Code)
}
