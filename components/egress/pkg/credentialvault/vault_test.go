// Copyright 2026 Alibaba Group Holding Ltd.
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

package credentialvault

import (
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func testCredentialPolicy(t *testing.T, raw string) *policy.NetworkPolicy {
	t.Helper()
	pol, err := policy.ParsePolicy(raw)
	require.NoError(t, err)
	return pol
}

func testCredentialVaultRequest() CreateRequest {
	return CreateRequest{
		Credentials: []Credential{
			{
				Name: "gitlab-token",
				Source: InlineCredentialSource{
					Type:  "inline",
					Value: "secret-token",
				},
			},
		},
		Bindings: []Binding{
			{
				Name: "gitlab-api",
				Match: Match{
					Hosts:   []string{"code.example.com"},
					Methods: []string{"GET"},
					Paths:   []string{"/api/v8/*"},
				},
				Auth: Auth{
					Type:       "apiKey",
					Name:       "PRIVATE-TOKEN",
					Credential: "gitlab-token",
				},
			},
		},
	}
}

func TestCredentialVaultCreateSanitizesAndRendersActiveSnapshot(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)

	state, err := store.Create(testCredentialVaultRequest(), pol)
	require.NoError(t, err)
	require.Equal(t, int64(1), state.Revision)
	require.Equal(t, []Metadata{{Name: "gitlab-token", SourceType: "inline", Revision: 1}}, state.Credentials)
	require.Equal(t, "apiKey", state.Bindings[0].Auth.Type)
	require.Equal(t, "Private-Token", state.Bindings[0].Auth.Name)

	payload, err := store.ActiveSnapshot()
	require.NoError(t, err)
	require.Equal(t, int64(1), payload.Revision)
	require.Equal(t, []InjectionHeader{{Name: "Private-Token", Value: "secret-token"}}, payload.Bindings[0].Headers)
	require.Contains(t, payload.Redactions, "secret-token")
}

func TestCredentialVaultAllowsDefaultAllowWithoutExplicitRules(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"allow","egress":[]}`)

	_, err := store.Create(testCredentialVaultRequest(), pol)
	require.NoError(t, err, "defaultAction allow should not require explicit egress rules")
}

func TestCredentialVaultDefaultAllowRespectsExplicitDenyRule(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"allow","egress":[{"action":"deny","target":"code.example.com"}]}`)

	_, err := store.Create(testCredentialVaultRequest(), pol)
	require.ErrorContains(t, err, "not allowed by egress policy")
}

func TestCredentialVaultRejectsReservedAndDuplicateHeaderNamesCaseInsensitively(t *testing.T) {
	_, err := normalizeBinding(Binding{
		Name:  "bad",
		Match: Match{Hosts: []string{"code.example.com"}},
		Auth: Auth{
			Type:       "apiKey",
			Name:       "Content-Length",
			Credential: "token",
		},
	})
	require.ErrorContains(t, err, "reserved credential header name")

	_, err = normalizeBinding(Binding{
		Name:  "dupe",
		Match: Match{Hosts: []string{"code.example.com"}},
		Auth: Auth{
			Type: "customHeaders",
			Headers: []CustomHeaderEntry{
				{Name: "X-Access-Token", Credential: "a"},
				{Name: "x-access-token", Credential: "b"},
			},
		},
	})
	require.ErrorContains(t, err, "duplicate custom header name")
}

func TestCredentialVaultRejectsNonFQDNBindingHosts(t *testing.T) {
	for _, host := range []string{
		"api.example.com:443",
		"api_example.com",
		"localhost",
		"*.localhost",
		"*.example.com:443",
	} {
		_, err := normalizeBinding(Binding{
			Name:  "bad-host",
			Match: Match{Hosts: []string{host}},
			Auth: Auth{
				Type:       "bearer",
				Credential: "token",
			},
		})
		require.Error(t, err, host)
	}
}

func TestCredentialVaultPatchRejectsDeletingReferencedCredential(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	_, err := store.Create(testCredentialVaultRequest(), pol)
	require.NoError(t, err)

	_, err = store.Patch(MutationRequest{
		Credentials: &CredentialMutationSet{Delete: []string{"gitlab-token"}},
	}, pol)
	require.ErrorContains(t, err, "references unknown credential")

	state, err := store.Patch(MutationRequest{
		Bindings:    &BindingMutationSet{Delete: []string{"gitlab-api"}},
		Credentials: &CredentialMutationSet{Delete: []string{"gitlab-token"}},
	}, pol)
	require.NoError(t, err)
	require.Empty(t, state.Credentials)
	require.Empty(t, state.Bindings)
}

func TestParseMitmproxyIgnoreHosts(t *testing.T) {
	require.Equal(t, []string{`^example\.com$`, `.*\.internal$`}, parseMitmproxyIgnoreHosts(`
mode:
  - transparent
ignore_hosts:
  - '^example\.com$'
  - ".*\.internal$"
listen_host: 127.0.0.1
`))

	require.Equal(t, []string{`^example\.com$`, `.*\.internal$`}, parseMitmproxyIgnoreHosts(`
ignore_hosts: ['^example\.com$', ".*\.internal$"]
`))

	require.Nil(t, parseMitmproxyIgnoreHosts("ignore_hosts: []"))
}
