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
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func revisionPolicyMountFixture(t *testing.T, singleFile bool) (string, string) {
	t.Helper()
	switch os.Getenv("OPENSANDBOX_ATOMIC_POLICY_MOUNT_TEST") {
	case "":
		t.Skip("set OPENSANDBOX_ATOMIC_POLICY_MOUNT_TEST=1 in a private mount namespace")
	case "1":
		require.Equal(t, 0, os.Geteuid())
	default:
		t.Fatal("OPENSANDBOX_ATOMIC_POLICY_MOUNT_TEST must be unset or 1")
	}
	root := t.TempDir()
	host := filepath.Join(root, "host")
	mounted := filepath.Join(root, "mounted")
	require.NoError(t, os.Mkdir(host, 0700))
	require.NoError(t, os.Mkdir(mounted, 0700))
	hostPath := filepath.Join(host, "policy.json")
	mountedPath := filepath.Join(mounted, "policy.json")
	require.NoError(t, os.WriteFile(hostPath, []byte(`{"defaultAction":"deny"}`), 0640))
	source, target := host, mounted
	if singleFile {
		require.NoError(t, os.WriteFile(mountedPath, nil, 0600))
		source, target = hostPath, mountedPath
	}
	require.NoError(t, unix.Mount(source, target, "", unix.MS_BIND, ""))
	t.Cleanup(func() { require.NoError(t, unix.Unmount(target, 0)) })
	return hostPath, mountedPath
}

func TestRevisionAtomicPolicyDirectoryBindMount(t *testing.T) {
	host, path := revisionPolicyMountFixture(t, false)
	s := recoveryPolicyFixture(t)
	s.policyFile = path
	nft := &stubNft{}
	s.nft = nft
	before, err := os.Stat(host)
	require.NoError(t, err)
	_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
	require.NoError(t, err)
	w := httptest.NewRecorder()
	s.handlePost(w, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(`{"defaultAction":"allow"}`)))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, 1, nft.calls)
	require.Zero(t, nft.quiesces)
	after, err := os.Stat(host)
	require.NoError(t, err)
	require.False(t, os.SameFile(before, after))
	require.Equal(t, before.Mode(), after.Mode())
	raw, err := os.ReadFile(host)
	require.NoError(t, err)
	loaded, err := policy.ParsePolicy(string(raw))
	require.NoError(t, err)
	require.Equal(t, policy.ActionAllow, loaded.DefaultAction)
	require.Equal(t, policy.ActionAllow, s.proxy.CurrentPolicy().DefaultAction)
	require.Equal(t, policy.ActionAllow, s.revisionRecovery.current.inputs.user.DefaultAction)
	require.ErrorIs(t, validateRecoveryTicket(s, ticket), errStaleRevisionBootstrap)
	require.False(t, s.revisionRecoveryRequired.Load())
}

func TestRevisionAtomicPolicySingleFileBindMountRejected(t *testing.T) {
	host, path := revisionPolicyMountFixture(t, true)
	original, err := os.ReadFile(host)
	require.NoError(t, err)
	before, err := os.Stat(host)
	require.NoError(t, err)
	inputs := policyCandidateInputs(t)
	s := &policyServer{policyFile: path, credentialVault: credentialvault.NewStore(nil, nil)}
	require.Error(t, s.initRevisionRecoveryLocked(inputs))
	require.Nil(t, s.revisionRecovery)
	require.Nil(t, s.atomicPolicyFile)
	s = recoveryPolicyFixture(t)
	s.policyFile = path
	nft := &stubNft{}
	s.nft = nft
	_, _, ticket, err := s.captureRevisionBootstrap(context.Background())
	require.NoError(t, err)
	w := httptest.NewRecorder()
	s.handlePost(w, httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(`{"defaultAction":"allow"}`)))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Zero(t, nft.calls)
	require.Zero(t, nft.quiesces)
	require.NoError(t, validateRecoveryTicket(s, ticket))
	current, err := os.ReadFile(host)
	require.NoError(t, err)
	require.Equal(t, original, current)
	after, err := os.Stat(host)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	require.False(t, s.revisionRecoveryRequired.Load())
}
