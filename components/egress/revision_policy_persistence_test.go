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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

type faultPolicyStore struct {
	state                            policy.FileMutationState
	saveErr, snapshotErr, restoreErr error
	restoreState                     *policy.FileMutationState
	saves, restores                  int
	onSave, onRestore                func()
}

func (f *faultPolicyStore) Snapshot() (*policy.PolicyFileSnapshot, error) { return nil, f.snapshotErr }
func (f *faultPolicyStore) Save(*policy.NetworkPolicy) (policy.FileMutationState, error) {
	f.saves++
	if f.onSave != nil {
		f.onSave()
	}
	return f.state, f.saveErr
}
func (f *faultPolicyStore) Restore(*policy.PolicyFileSnapshot) (policy.FileMutationState, error) {
	f.restores++
	if f.onRestore != nil {
		f.onRestore()
	}
	if f.restoreState != nil {
		return *f.restoreState, f.restoreErr
	}
	return policy.FileCommitted, f.restoreErr
}
func injectUnknownPolicySave(s *policyServer) {
	s.policyFile = "injected-policy.json"
	s.atomicPolicyFile = &faultPolicyStore{state: policy.FileUnknown, saveErr: errors.New("injected uncertain rename")}
}
func TestRevisionAtomicPolicyOutcomeWiring(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		state                                        policy.FileMutationState
		snapshotFail, saveFail, restoreFail, nftFail bool
		wantStatus, restores                         int
		recovery                                     bool
	}{
		{"snapshot", policy.FileUnchanged, true, false, false, false, 500, 0, false},
		{"snapshot-directory-sync", policy.FileUnchanged, true, false, false, false, 500, 0, false},
		{"pre-rename", policy.FileUnchanged, false, true, false, false, 500, 0, false},
		{"rename-unknown", policy.FileUnknown, false, true, false, false, 500, 1, true},
		{"restore-failure", policy.FileUnknown, false, true, true, false, 500, 1, true},
		{"invalid-success-state", policy.FileUnknown, false, false, false, false, 500, 1, true},
		{"nft-failure", policy.FileCommitted, false, false, false, true, 500, 1, true},
		{"nft-restore-failure", policy.FileCommitted, false, false, true, true, 500, 1, true},
		{"committed", policy.FileCommitted, false, false, false, false, 200, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := recoveryPolicyFixture(t)
			nft := &stubNft{}
			s.nft = nft
			s.policyFile = "policy.json"
			f := &faultPolicyStore{state: tc.state}
			s.atomicPolicyFile = f
			injected := errors.New("injected persistence stage")
			if tc.snapshotFail {
				f.snapshotErr = injected
				if tc.name == "snapshot-directory-sync" {
					f.snapshotErr = &policy.FileMutationError{Phase: "directory-sync-preflight", Err: injected}
				}
			}
			if tc.saveFail {
				f.saveErr = injected
			}
			if tc.restoreFail {
				f.restoreErr = injected
			}
			if tc.nftFail {
				nft.err = injected
			}
			original := s.revisionRecovery.current
			_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
			require.NoError(t, err)
			f.onRestore = func() {
				require.True(t, s.revisionRecoveryRequired.Load(), "latch must precede restoration")
				require.Equal(t, 1, nft.quiesces)
			}
			w := httptest.NewRecorder()
			s.handlePost(w, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(`{"defaultAction":"allow"}`)))
			require.Equal(t, tc.wantStatus, w.Code)
			require.Equal(t, tc.restores, f.restores)
			if tc.snapshotFail {
				require.Zero(t, f.saves)
			}
			require.Equal(t, tc.recovery, s.revisionRecoveryRequired.Load())
			if tc.recovery {
				require.ErrorIs(t, validateRecoveryTicket(s, ticket), errRevisionRecoveryRequired)
				require.Same(t, original, s.revisionRecovery.current)
				require.Equal(t, 1, nft.quiesces)
				w = httptest.NewRecorder()
				s.handlePost(w, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(`{"defaultAction":"allow"}`)))
				require.Equal(t, 503, w.Code)
				require.Equal(t, 1, f.saves)
			} else if tc.wantStatus != 200 {
				require.NoError(t, validateRecoveryTicket(s, ticket))
				require.Same(t, original, s.revisionRecovery.current)
				require.Zero(t, nft.calls)
				require.Zero(t, nft.quiesces)
			} else {
				require.NotSame(t, original, s.revisionRecovery.current)
				require.ErrorIs(t, validateRecoveryTicket(s, ticket), errStaleRevisionBootstrap)
			}
			if tc.saveFail || tc.snapshotFail || tc.state == policy.FileUnknown {
				require.Zero(t, nft.calls)
			}
		})
	}
}

func TestRevisionAtomicPolicyUnsupportedStartup(t *testing.T) {
	for _, kind := range []string{"missing-parent", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "policy.json")
			switch kind {
			case "missing-parent":
				path = filepath.Join(root, "missing", "policy.json")
			case "directory":
				require.NoError(t, os.Mkdir(path, 0700))
			case "symlink":
				target := filepath.Join(root, "target")
				require.NoError(t, os.WriteFile(target, []byte("original"), 0600))
				require.NoError(t, os.Symlink(target, path))
			}
			s := &policyServer{policyFile: path}
			err := s.initRevisionRecoveryLocked(policyCandidateInputs(t))
			require.Error(t, err)
			require.Nil(t, s.revisionRecovery)
			require.Nil(t, s.atomicPolicyFile)
		})
	}
}

func TestRevisionAtomicPolicyLegacySymlinkCompatibility(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	path := filepath.Join(root, "policy.json")
	require.NoError(t, os.WriteFile(target, []byte(`{"defaultAction":"deny"}`), 0600))
	require.NoError(t, os.Symlink(target, path))
	s := &policyServer{policyFile: path}
	restore, err := s.persistPolicyChangeLocked(policy.DefaultDenyPolicy())
	require.NoError(t, err)
	require.NoError(t, restore())
	require.Nil(t, s.atomicPolicyFile)
	info, err := os.Lstat(path)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink)
}

func TestRevisionAtomicPolicyDisabledPersistence(t *testing.T) {
	for _, experimental := range []bool{false, true} {
		s := &policyServer{}
		if experimental {
			s = recoveryPolicyFixture(t)
		}
		unused := &faultPolicyStore{snapshotErr: errors.New("must not touch disabled storage")}
		s.atomicPolicyFile = unused
		restore, err := s.persistPolicyChangeLocked(policy.DefaultDenyPolicy())
		require.NoError(t, err)
		require.NoError(t, restore())
		require.Zero(t, unused.saves)
		require.Zero(t, unused.restores)
		require.False(t, s.revisionRecoveryRequired.Load())
	}
}
