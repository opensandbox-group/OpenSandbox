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
	"encoding/json"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func mustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

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
				Name:   "gitlab-token",
				Source: mustMarshal(map[string]string{"type": "inline", "value": "secret-token"}),
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

func TestCredentialVaultRendersScopedSubstitutions(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)

	state, err := store.Create(CreateRequest{
		Credentials: []Credential{
			{
				Name:   "client-secret",
				Source: mustMarshal(map[string]string{"type": "inline", "value": `real "clé"&value😀`}),
			},
		},
		Bindings: []Binding{
			{
				Name: "token-request",
				Match: Match{
					Hosts:   []string{"code.example.com"},
					Methods: []string{"POST"},
					Paths:   []string{"/oauth/token"},
				},
				Auth: Auth{
					Type: "passthrough",
					Substitutions: []Substitution{
						{
							Credential:  "client-secret",
							Placeholder: "__client_secret__",
							In:          []string{"body", "query", "path", "body"},
						},
					},
				},
			},
		},
	}, pol)
	require.NoError(t, err)
	require.Equal(t, "passthrough", state.Bindings[0].Auth.Type)
	require.NotContains(t, string(mustMarshal(state)), "__client_secret__")
	require.NotContains(t, string(mustMarshal(state)), `real "secret"+value`)

	payload, err := store.ActiveSnapshot()
	require.NoError(t, err)
	require.Equal(t, int64(1), payload.Revision)
	require.Empty(t, payload.Bindings[0].Headers)
	require.Equal(t, []InjectionSubstitution{
		{
			Placeholder: "__client_secret__",
			Value:       `real "clé"&value😀`,
			In:          []string{"body", "query", "path"},
		},
	}, payload.Bindings[0].Substitutions)
	require.Contains(t, payload.Redactions, "__client_secret__")
	require.Contains(t, payload.Redactions, `real "clé"&value😀`)
	require.Contains(t, payload.Redactions, "real%20%22cl%C3%A9%22%26value%F0%9F%98%80")
	require.Contains(t, payload.Redactions, "real%20%22cl%c3%a9%22%26value%f0%9f%98%80")
	require.Contains(t, payload.Redactions, "real+%22cl%C3%A9%22%26value%F0%9F%98%80")
	require.Contains(t, payload.Redactions, "real+%22cl%c3%a9%22%26value%f0%9f%98%80")
	require.Contains(t, payload.Redactions, `real \"clé\"\u0026value😀`)
	require.Contains(t, payload.Redactions, `real \"cl\u00e9\"&value\ud83d\ude00`)
}

func TestCredentialVaultAllowsDefaultAllowPolicyForCompatibility(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"allow","egress":[]}`)

	state, err := store.Create(testCredentialVaultRequest(), pol)
	require.NoError(t, err)
	require.Len(t, state.Bindings, 1)
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

func TestCredentialVaultRejectsInvalidSubstitution(t *testing.T) {
	_, err := normalizeBinding(Binding{
		Name:  "bad-substitution-surface",
		Match: Match{Hosts: []string{"code.example.com"}},
		Auth: Auth{
			Type: "passthrough",
			Substitutions: []Substitution{
				{Credential: "token", Placeholder: "__token__", In: []string{"cookie"}},
			},
		},
	})
	require.ErrorContains(t, err, "unsupported target surface")

	_, err = normalizeBinding(Binding{
		Name:  "bad-substitution-placeholder",
		Match: Match{Hosts: []string{"code.example.com"}},
		Auth: Auth{
			Type: "passthrough",
			Substitutions: []Substitution{
				{Credential: "token", Placeholder: " ", In: []string{"body"}},
			},
		},
	})
	require.ErrorContains(t, err, "requires placeholder")
}

func TestCredentialVaultPreservesSubstitutionPlaceholderWhitespace(t *testing.T) {
	binding, err := normalizeBinding(Binding{
		Name:  "literal-placeholder",
		Match: Match{Hosts: []string{"code.example.com"}},
		Auth: Auth{
			Type: "passthrough",
			Substitutions: []Substitution{
				{Credential: "token", Placeholder: " __token__ ", In: []string{" Body ", "body"}},
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, " __token__ ", binding.Auth.Substitutions[0].Placeholder)
	require.Equal(t, []string{"body"}, binding.Auth.Substitutions[0].In)
}

func TestCredentialVaultRejectsDuplicateSubstitutionPlaceholderSurface(t *testing.T) {
	_, err := normalizeBinding(Binding{
		Name:  "duplicate-placeholder-surface",
		Match: Match{Hosts: []string{"code.example.com"}},
		Auth: Auth{
			Type: "passthrough",
			Substitutions: []Substitution{
				{Credential: "primary", Placeholder: "__token__", In: []string{"body"}},
				{Credential: "secondary", Placeholder: "__token__", In: []string{"query", "body"}},
			},
		},
	})
	require.ErrorContains(t, err, `duplicates placeholder "__token__" on body surface`)
}

func TestCredentialVaultRejectsPassthroughIgnoredFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth Auth
		want string
	}{
		{
			name: "credential",
			auth: Auth{Type: "passthrough", Credential: "api-token"},
			want: "does not accept credential",
		},
		{
			name: "name",
			auth: Auth{Type: "passthrough", Name: "X-Token"},
			want: "does not accept name",
		},
		{
			name: "headers",
			auth: Auth{
				Type:    "passthrough",
				Headers: []CustomHeaderEntry{{Name: "X-Token", Credential: "api-token"}},
			},
			want: "does not accept headers",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeBinding(Binding{
				Name:  "bad-passthrough",
				Match: Match{Hosts: []string{"code.example.com"}},
				Auth:  tc.auth,
			})
			require.ErrorContains(t, err, tc.want)
		})
	}
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

func TestCredentialVaultRejectsNonStandardPorts(t *testing.T) {
	for _, tc := range []struct {
		ports   []int
		wantErr bool
	}{
		{[]int{8080}, true},
		{[]int{443, 8080}, true},
		{[]int{80, 443}, false},
		{[]int{443}, false},
		{[]int{80}, false},
		{nil, false},
	} {
		_, err := normalizeBinding(Binding{
			Name:  "test",
			Match: Match{Hosts: []string{"api.example.com"}, Ports: tc.ports},
			Auth:  Auth{Type: "bearer", Credential: "token"},
		})
		if tc.wantErr {
			require.ErrorContains(t, err, "unsupported port", "ports=%v", tc.ports)
		} else {
			require.NoError(t, err, "ports=%v", tc.ports)
		}
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

func TestCredentialVaultRejectsUnknownSubstitutionCredential(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)

	_, err := store.Create(CreateRequest{
		Credentials: nil,
		Bindings: []Binding{
			{
				Name:  "missing-substitution-credential",
				Match: Match{Hosts: []string{"code.example.com"}},
				Auth: Auth{
					Type: "passthrough",
					Substitutions: []Substitution{
						{Credential: "missing", Placeholder: "__missing__", In: []string{"body"}},
					},
				},
			},
		},
	}, pol)
	require.ErrorContains(t, err, "references unknown credential")
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
