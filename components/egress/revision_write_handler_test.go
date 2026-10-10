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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/stretchr/testify/require"
)

// liveWriteFixture builds a policyServer whose Vault writes run the live
// revision transaction against a fake mitmdump session.
func liveWriteFixture(t *testing.T) (*policyServer, *mitmTransparent, *mutationTestSession) {
	t.Helper()
	t.Setenv(constants.EnvExperimentalRevisionRuntime, "true")
	t.Setenv(constants.EnvEgressMode, constants.PolicyDnsNft)
	s, m, session := mutationFixture(t)
	s.mitm = m
	return s, m, session
}

func liveWritePolicy(t *testing.T, allowHost bool) *policy.NetworkPolicy {
	t.Helper()
	if !allowHost {
		return testCredentialVaultPolicy(t, `{"defaultAction":"deny"}`)
	}
	return testCredentialVaultPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
}

func liveWriteCreateBody() string {
	body, err := json.Marshal(testCredentialVaultRequest())
	if err != nil {
		panic(err)
	}
	return string(body)
}

func liveWritePatchBody(expected int64) string {
	body, err := json.Marshal(credentialvault.MutationRequest{
		ExpectedRevision: &expected,
		Credentials: &credentialvault.CredentialMutationSet{
			Replace: []credentialvault.Credential{
				{
					Name:   "gitlab-token",
					Source: json.RawMessage(`{"type":"inline","value":"rotated-token"}`),
				},
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func liveWriteDo(t *testing.T, s *policyServer, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	var request *http.Request
	if body == "" {
		request = httptest.NewRequest(method, "/credential-vault", nil)
	} else {
		request = httptest.NewRequest(method, "/credential-vault", strings.NewReader(body))
	}
	response := httptest.NewRecorder()
	s.handleCredentialVault(response, request)
	return response
}

func liveWriteCreate(t *testing.T, s *policyServer) {
	t.Helper()
	s.proxy = &stubProxy{updated: liveWritePolicy(t, true)}
	response := liveWriteDo(t, s, http.MethodPost, liveWriteCreateBody())
	require.Equal(t, http.StatusCreated, response.Code)
}

func TestRevisionWriteHandlerCreateRunsTransaction(t *testing.T) {
	s, _, session := liveWriteFixture(t)
	liveWriteCreate(t, s)

	require.Equal(t, 1, session.updateCalls)
	state, err := s.credentialVault.Sanitized()
	require.NoError(t, err)
	require.Equal(t, int64(1), state.Revision)
}

func TestRevisionWriteHandlerPatchRequiresExpectedRevision(t *testing.T) {
	s, _, _ := liveWriteFixture(t)
	liveWriteCreate(t, s)

	patched := liveWriteDo(t, s, http.MethodPatch, `{"credentials":{"replace":[{"name":"gitlab-token","source":{"type":"inline","value":"rotated-token"}}]}}`)
	require.Equal(t, http.StatusBadRequest, patched.Code)
	require.Contains(t, patched.Body.String(), "expectedRevision is required")
}

func TestRevisionWriteHandlerPatchRejectsStaleRevision(t *testing.T) {
	s, _, _ := liveWriteFixture(t)
	liveWriteCreate(t, s)

	stale := liveWriteDo(t, s, http.MethodPatch, liveWritePatchBody(99))
	require.Equal(t, http.StatusConflict, stale.Code)
	state, err := s.credentialVault.Sanitized()
	require.NoError(t, err)
	require.Equal(t, int64(1), state.Revision)
}

func TestRevisionWriteHandlerPatchRotatesCredential(t *testing.T) {
	s, _, session := liveWriteFixture(t)
	liveWriteCreate(t, s)

	rotated := liveWriteDo(t, s, http.MethodPatch, liveWritePatchBody(1))
	require.Equal(t, http.StatusOK, rotated.Code)
	require.Equal(t, 2, session.updateCalls)
	var state credentialvault.State
	require.NoError(t, json.Unmarshal(rotated.Body.Bytes(), &state))
	require.Equal(t, int64(2), state.Revision)
}

func TestRevisionWriteHandlerDeleteAndRecreate(t *testing.T) {
	s, _, _ := liveWriteFixture(t)
	liveWriteCreate(t, s)

	deleted := liveWriteDo(t, s, http.MethodDelete, "")
	require.Equal(t, http.StatusNoContent, deleted.Code)
	_, err := s.credentialVault.Sanitized()
	require.ErrorIs(t, err, credentialvault.ErrNotFound)

	recreated := liveWriteDo(t, s, http.MethodPost, liveWriteCreateBody())
	require.Equal(t, http.StatusCreated, recreated.Code)
	state, err := s.credentialVault.Sanitized()
	require.NoError(t, err)
	require.Equal(t, int64(1), state.Revision)
}

func TestRevisionWriteHandlerValidationFailureKeepsFixedVocabulary(t *testing.T) {
	s, _, session := liveWriteFixture(t)
	s.proxy = &stubProxy{updated: liveWritePolicy(t, false)}

	// The binding host is not allowed by the policy: the prepare failure is
	// sanitized and no session work happens.
	response := liveWriteDo(t, s, http.MethodPost, liveWriteCreateBody())
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.NotContains(t, response.Body.String(), "secret-token")
	require.Zero(t, session.updateCalls)
	_, err := s.credentialVault.Sanitized()
	require.ErrorIs(t, err, credentialvault.ErrNotFound)
}

func TestRevisionWriteHandlerIndeterminateOutcomeFailsClosed(t *testing.T) {
	s, m, session := liveWriteFixture(t)
	liveWriteCreate(t, s)

	stops := 0
	m.revisionOwner.stop = func(*mitmproxy.Running) { stops++ }
	session.update = func(context.Context, credentialvault.ActiveSnapshot, int64) (revision.Identity, error) {
		return revision.Identity{}, errors.New("private-session-secret")
	}
	patched := liveWriteDo(t, s, http.MethodPatch, liveWritePatchBody(1))
	require.Equal(t, http.StatusServiceUnavailable, patched.Code)
	require.NotContains(t, patched.Body.String(), "private")
	state, err := s.credentialVault.Sanitized()
	require.NoError(t, err)
	require.Equal(t, int64(1), state.Revision)
}
