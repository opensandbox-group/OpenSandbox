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
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The policy relay and snapshot surfaces arrived on main after the original
// transition fix. They must obey the same unfinished-registration guard as
// Vault, including a failed registration that has not published subjFence.
func TestTransitionPolicySurfacesRejectPendingRebind(t *testing.T) {
	for _, phase := range []string{"old-network-removal", "new-registration"} {
		t.Run(phase, func(t *testing.T) {
			h := newPolicyPersistHarness(t)
			subj := h.readySubject(t, policyTestUID, vaultTestIP)
			if phase == "old-network-removal" {
				h.nft.removeErr = errors.New("remove failed")
			} else {
				h.nft.denyFirstErr = errors.New("deny failed")
			}
			rec := doAction(t, h.srv, bindBody(t, policyTestUID, "10.0.0.6", "new-r", "new-a", 2, strPtr(policyAllow())))
			require.Equal(t, http.StatusInternalServerError, rec.Code)
			require.NotNil(t, transitionCurrentToken(h.srv, subj))
			before := append([]byte(nil), h.fileFor(policyTestUID).data...)
			for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
				rec := doPolicy(t, h.srv, method, policyTestUID, policyPutDoc(policyAllowAlt(), 0, false, false), "")
				require.Equal(t, http.StatusServiceUnavailable, rec.Code, method)
			}
			export := filepath.Join(t.TempDir(), "export.json")
			rec = doAction(t, h.srv, policySnapshotBody(t, policyTestUID, "new-r", "new-a", "POLICY_EXPORT", export, nil))
			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			require.False(t, h.fakeForPath(export).exists)
			bundle := filepath.Join(t.TempDir(), "bundle.json")
			h.seedPolicyBundle(t, bundle, 7, policyAllowAlt())
			rec = doAction(t, h.srv, policySnapshotBody(t, policyTestUID, "new-r", "new-a", "POLICY_IMPORT", bundle, nil))
			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			require.Equal(t, before, h.fileFor(policyTestUID).data)
			require.False(t, h.fileFor(policyTestUID).exists, "rebind purges the predecessor policy file")
			h.nft.removeErr, h.nft.denyFirstErr = nil, nil
			require.Equal(t, http.StatusOK, doAction(t, h.srv, bindBody(t, policyTestUID, "10.0.0.6", "new-r", "new-a", 2, strPtr(policyAllow()))).Code)
			require.Nil(t, transitionCurrentToken(h.srv, subj))
			require.Equal(t, http.StatusOK, doPolicy(t, h.srv, http.MethodGet, policyTestUID, "", "new-r").Code)
		})
	}
}
