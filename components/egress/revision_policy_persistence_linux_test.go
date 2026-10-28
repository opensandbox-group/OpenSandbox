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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRevisionAtomicPolicyRealRestore(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing"}[exists], func(t *testing.T) {
			s := recoveryPolicyFixture(t)
			s.policyFile = filepath.Join(t.TempDir(), "policy.json")
			original := []byte("  { \"defaultAction\": \"deny\" } \n")
			if exists {
				require.NoError(t, os.WriteFile(s.policyFile, original, 0640))
			}
			s.nft = &stubNft{err: errors.New("uncertain nft apply")}
			w := httptest.NewRecorder()
			s.handlePost(w, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(`{"defaultAction":"allow"}`)))
			require.Equal(t, http.StatusInternalServerError, w.Code)
			require.True(t, s.revisionRecoveryRequired.Load())
			data, err := os.ReadFile(s.policyFile)
			if exists {
				require.NoError(t, err)
				require.Equal(t, original, data)
				info, err := os.Stat(s.policyFile)
				require.NoError(t, err)
				require.Equal(t, os.FileMode(0640), info.Mode().Perm())
			} else {
				require.True(t, os.IsNotExist(err))
			}
			entries, err := os.ReadDir(filepath.Dir(s.policyFile))
			require.NoError(t, err)
			if exists {
				require.Len(t, entries, 1)
			} else {
				require.Empty(t, entries)
			}
		})
	}
}
