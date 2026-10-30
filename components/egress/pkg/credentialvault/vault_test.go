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

package credentialvault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/mitmpassthrough"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

type countingCredentialSource struct {
	resolves *atomic.Int32
}

func (s *countingCredentialSource) Type() string { return "counting" }

func (s *countingCredentialSource) Resolve(_ context.Context) (string, error) {
	s.resolves.Add(1)
	return "resolved-secret", nil
}

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

func TestCredentialVaultPatchRevisionConflict(t *testing.T) {
	for _, prepare := range []bool{false, true} {
		t.Run(fmt.Sprintf("prepare=%t", prepare), func(t *testing.T) {
			store := NewStore(nil, func() bool { return true })
			pol := testCredentialPolicy(t, `{"defaultAction":"deny"}`)
			initial, err := store.Create(CreateRequest{}, pol)
			require.NoError(t, err)
			patch := func(req MutationRequest) error {
				if prepare {
					candidate, err := store.PreparePatch(req, pol)
					if candidate != nil {
						candidate.Discard()
					}
					return err
				}
				_, err := store.Patch(req, pol)
				return err
			}
			stale := initial.Revision + 1
			err = patch(MutationRequest{ExpectedRevision: &stale})
			require.ErrorIs(t, err, ErrRevisionConflict)
			response := httptest.NewRecorder()
			WriteError(response, fmt.Errorf("wrapped: %w", err))
			require.Equal(t, http.StatusConflict, response.Code)

			err = patch(MutationRequest{ExpectedRevision: &initial.Revision,
				Credentials: &CredentialMutationSet{Delete: []string{"expectedRevision"}}})
			require.Error(t, err)
			require.Contains(t, err.Error(), "expectedRevision")
			require.NotErrorIs(t, err, ErrRevisionConflict)
			response = httptest.NewRecorder()
			WriteError(response, err)
			require.Equal(t, http.StatusBadRequest, response.Code)
			state, err := store.Sanitized()
			require.NoError(t, err)
			require.Equal(t, initial, state)
		})
	}
}

func TestCredentialVaultWriteErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{"conflict-without-expectedRevision-text", fmt.Errorf("wrapped: %w", ErrRevisionConflict), http.StatusConflict},
		{"validation-with-expectedRevision-text", errors.New("invalid name expectedRevision"), http.StatusBadRequest},
		{"invalid", errors.New("invalid credential"), http.StatusBadRequest},
		{"not-found", fmt.Errorf("wrapped: %w", ErrNotFound), http.StatusNotFound},
		{"exists", fmt.Errorf("wrapped: %w", ErrExists), http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			WriteError(response, tc.err)
			require.Equal(t, tc.code, response.Code)
		})
	}
}

func TestCredentialVaultActiveSnapshotIfChanged(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	_, err := store.Create(testCredentialVaultRequest(), pol)
	require.NoError(t, err)

	snapshot, tag, changed, err := store.ActiveSnapshotIfChanged(context.Background(), "")
	require.NoError(t, err)
	require.True(t, changed)
	require.NotEmpty(t, tag)
	require.Equal(t, int64(1), snapshot.Revision)
	require.Contains(t, snapshot.Redactions, "secret-token")

	snapshot, unchangedTag, changed, err := store.ActiveSnapshotIfChanged(context.Background(), tag)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, tag, unchangedTag)
	require.Equal(t, int64(1), snapshot.Revision)
	require.Empty(t, snapshot.Bindings)
	require.Empty(t, snapshot.Redactions)

	_, err = store.Patch(MutationRequest{
		Credentials: &CredentialMutationSet{Replace: []Credential{{
			Name:   "gitlab-token",
			Source: mustMarshal(map[string]string{"type": "inline", "value": "new-secret-token"}),
		}}},
	}, pol)
	require.NoError(t, err)

	snapshot, updatedTag, changed, err := store.ActiveSnapshotIfChanged(context.Background(), tag)
	require.NoError(t, err)
	require.True(t, changed)
	require.NotEqual(t, tag, updatedTag)
	require.Equal(t, int64(2), snapshot.Revision)
	require.Contains(t, snapshot.Redactions, "new-secret-token")
}

func TestCredentialVaultUnchangedRevisionDoesNotResolveCredentialSources(t *testing.T) {
	var resolves atomic.Int32
	registry := NewSourceRegistry()
	registry.Register("counting", func(_ json.RawMessage) (CredentialSource, error) {
		return &countingCredentialSource{resolves: &resolves}, nil
	})
	store := NewStoreWithRegistry(nil, func() bool { return true }, registry)
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	req := testCredentialVaultRequest()
	req.Credentials[0].Source = json.RawMessage(`{"type":"counting"}`)
	_, err := store.Create(req, pol)
	require.NoError(t, err)

	_, tag, changed, err := store.ActiveSnapshotIfChanged(context.Background(), "")
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, int32(1), resolves.Load())

	_, unchangedTag, changed, err := store.ActiveSnapshotIfChanged(context.Background(), tag)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, tag, unchangedTag)
	require.Equal(t, int32(1), resolves.Load(), "304 path must not render credential material")
}

func TestCredentialVaultActiveTagChangesWhenRevisionResetsAfterRecreate(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	_, err := store.Create(testCredentialVaultRequest(), pol)
	require.NoError(t, err)
	first, firstTag, changed, err := store.ActiveSnapshotIfChanged(context.Background(), "")
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, int64(1), first.Revision)

	require.NoError(t, store.Delete())
	recreated := testCredentialVaultRequest()
	recreated.Credentials[0].Source = mustMarshal(map[string]string{
		"type": "inline", "value": "recreated-secret",
	})
	_, err = store.Create(recreated, pol)
	require.NoError(t, err)

	second, secondTag, changed, err := store.ActiveSnapshotIfChanged(context.Background(), firstTag)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, int64(1), second.Revision)
	require.NotEqual(t, firstTag, secondTag)
	require.Contains(t, second.Redactions, "recreated-secret")
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
	require.Equal(t, &[]InjectionSubstitution{
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

func TestCredentialVaultScopedMatchEnforcementRejectsImplicitMethodOrPath(t *testing.T) {
	t.Setenv(constants.EnvCredentialVaultRequireScopedMatch, "true")
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)

	req := testCredentialVaultRequest()
	req.Bindings[0].Match.Methods = nil
	_, err := store.Create(req, pol)
	require.ErrorContains(t, err, "match.methods must be explicit")

	req = testCredentialVaultRequest()
	req.Bindings[0].Match.Paths = nil
	_, err = store.Create(req, pol)
	require.ErrorContains(t, err, "match.paths must be explicit")

	req = testCredentialVaultRequest()
	req.Bindings[0].Match.Paths = []string{"/*"}
	_, err = store.Create(req, pol)
	require.ErrorContains(t, err, "match.paths must not contain /*")

	state, err := store.Create(testCredentialVaultRequest(), pol)
	require.NoError(t, err)
	require.Equal(t, []string{"GET"}, state.Bindings[0].Match.Methods)
	require.Equal(t, []string{"/api/v8/*"}, state.Bindings[0].Match.Paths)
}

func TestCredentialVaultScopedMatchEnforcementAppliesToPatch(t *testing.T) {
	t.Setenv(constants.EnvCredentialVaultRequireScopedMatch, "true")
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	_, err := store.Create(testCredentialVaultRequest(), pol)
	require.NoError(t, err)

	wideBinding := testCredentialVaultRequest().Bindings[0]
	wideBinding.Name = "wide-binding"
	wideBinding.Match.Methods = nil
	wideBinding.Match.Paths = nil
	_, err = store.Patch(MutationRequest{
		Bindings: &BindingMutationSet{Add: []Binding{wideBinding}},
	}, pol)
	require.ErrorContains(t, err, "match.methods must be explicit")

	state, err := store.Sanitized()
	require.NoError(t, err)
	require.Equal(t, int64(1), state.Revision)
	require.Len(t, state.Bindings, 1)
}

func TestCredentialVaultKeepsCompatibilityDefaultsWhenScopedMatchEnforcementIsOff(t *testing.T) {
	t.Setenv(constants.EnvCredentialVaultRequireScopedMatch, "")
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	req := testCredentialVaultRequest()
	req.Bindings[0].Match.Methods = nil
	req.Bindings[0].Match.Paths = nil

	state, err := store.Create(req, pol)
	require.NoError(t, err)
	require.Equal(t, []string{"GET", "POST", "PUT", "PATCH", "DELETE"}, state.Bindings[0].Match.Methods)
	require.Equal(t, []string{"/*"}, state.Bindings[0].Match.Paths)
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

func testExchangeCredential(name string) Credential {
	return Credential{
		Name:   name,
		Source: mustMarshal(map[string]string{"type": "exchange", "endpoint": "unix:///run/spi/" + name + ".sock"}),
	}
}

func testExchangeVaultRequest() CreateRequest {
	return CreateRequest{
		Credentials: []Credential{testExchangeCredential("spi-signer")},
		Bindings: []Binding{
			{
				Name: "internal-api",
				Match: Match{
					Hosts:   []string{"api.internal.example.com"},
					Methods: []string{"GET"},
					Paths:   []string{"/*"},
				},
				Auth: Auth{Credential: "spi-signer"},
			},
		},
	}
}

func testExchangePolicy(t *testing.T) *policy.NetworkPolicy {
	return testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"api.internal.example.com"}]}`)
}

func testResolverReadyStore() *Store {
	store := NewStore(nil, func() bool { return true })
	store.SetResolverConfig(ResolverConfig{TimeoutConfigured: true, AuthTableParsed: true})
	return store
}

func TestCredentialVaultTypelessExchangeAuthSanitizesAndRendersMarker(t *testing.T) {
	store := testResolverReadyStore()
	state, err := store.Create(testExchangeVaultRequest(), testExchangePolicy(t))
	require.NoError(t, err)
	require.Equal(t, []Metadata{{Name: "spi-signer", SourceType: "exchange", Revision: 1}}, state.Credentials)
	require.Equal(t, AuthMetadata{Credential: "spi-signer"}, state.Bindings[0].Auth)
	require.NotContains(t, string(mustMarshal(state)), "unix:///run/spi/spi-signer.sock")

	snapshot, err := store.ActiveSnapshot()
	require.NoError(t, err)
	require.Len(t, snapshot.Bindings, 1)
	require.Empty(t, snapshot.Bindings[0].Headers)
	require.Empty(t, *snapshot.Bindings[0].Substitutions)
	require.Equal(t, &ExchangeRef{Credential: "spi-signer"}, snapshot.Bindings[0].Exchange)
	require.Empty(t, snapshot.Redactions)
	require.JSONEq(t, `{
		"revision": 1,
		"bindings": [{
			"name": "internal-api",
			"match": {"schemes": ["https"], "hosts": ["api.internal.example.com"], "methods": ["GET"], "paths": ["/*"]},
			"headers": [],
			"substitutions": [],
			"exchange": {"credential": "spi-signer"}
		}]
	}`, string(mustMarshal(snapshot)))
}

func TestCredentialVaultTypelessAuthRejectsExtraFields(t *testing.T) {
	base := testExchangeVaultRequest()
	tests := []struct {
		name string
		auth Auth
	}{
		{"missing credential", Auth{}},
		{"blank credential", Auth{Credential: "   "}},
		{"with name", Auth{Credential: "spi-signer", Name: "x-signature"}},
		{"with headers", Auth{Credential: "spi-signer", Headers: []CustomHeaderEntry{{Name: "x-a", Credential: "spi-signer"}}}},
		{"with substitutions", Auth{Credential: "spi-signer", Substitutions: []Substitution{{Credential: "spi-signer", Placeholder: "__t__", In: []string{"header"}}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := testResolverReadyStore()
			request := base
			request.Bindings[0].Auth = tc.auth
			_, err := store.Create(request, testExchangePolicy(t))
			require.Error(t, err)
			response := httptest.NewRecorder()
			WriteError(response, err)
			require.Equal(t, http.StatusBadRequest, response.Code)
			require.NotContains(t, response.Body.String(), "unsupported auth type")
		})
	}
}

func TestCredentialVaultCrossLockRejectsMixedSourceReferences(t *testing.T) {
	tests := []struct {
		name   string
		auth   Auth
		unwant string
	}{
		{
			name:   "bearer references exchange",
			auth:   Auth{Type: "bearer", Credential: "spi-signer"},
			unwant: "only the typeless auth form may use exchange credentials",
		},
		{
			name:   "apiKey references exchange",
			auth:   Auth{Type: "apiKey", Name: "x-token", Credential: "spi-signer"},
			unwant: "only the typeless auth form may use exchange credentials",
		},
		{
			name:   "customHeaders references exchange",
			auth:   Auth{Type: "customHeaders", Headers: []CustomHeaderEntry{{Name: "x-token", Credential: "spi-signer"}}},
			unwant: "only the typeless auth form may use exchange credentials",
		},
		{
			name:   "typeless references inline",
			auth:   Auth{Credential: "gitlab-token"},
			unwant: "requires an exchange credential",
		},
		{
			name:   "substitution references exchange",
			auth:   Auth{Type: "passthrough", Substitutions: []Substitution{{Credential: "spi-signer", Placeholder: "__t__", In: []string{"header"}}}},
			unwant: "substitution cannot reference the exchange credential",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := testResolverReadyStore()
			request := CreateRequest{
				Credentials: []Credential{
					{Name: "gitlab-token", Source: mustMarshal(map[string]string{"type": "inline", "value": "secret-token"})},
					testExchangeCredential("spi-signer"),
				},
				Bindings: []Binding{{
					Name:  "mixed",
					Match: Match{Hosts: []string{"api.internal.example.com"}},
					Auth:  tc.auth,
				}},
			}
			_, err := store.Create(request, testExchangePolicy(t))
			require.ErrorContains(t, err, tc.unwant)
			_, err = store.PrepareCreate(request, testExchangePolicy(t))
			require.Error(t, err)
		})
	}
}

func TestCredentialVaultMixedInlineAndExchangeBindingsAreValid(t *testing.T) {
	store := testResolverReadyStore()
	request := testExchangeVaultRequest()
	request.Credentials = append(request.Credentials,
		Credential{Name: "gitlab-token", Source: mustMarshal(map[string]string{"type": "inline", "value": "secret-token"})},
	)
	request.Bindings = append(request.Bindings, Binding{
		Name:  "gitlab-api",
		Match: Match{Hosts: []string{"code.example.com"}, Methods: []string{"GET"}, Paths: []string{"/api/*"}},
		Auth:  Auth{Type: "apiKey", Name: "private-token", Credential: "gitlab-token"},
	})
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[
		{"action":"allow","target":"api.internal.example.com"},
		{"action":"allow","target":"code.example.com"}
	]}`)
	state, err := store.Create(request, pol)
	require.NoError(t, err)
	require.Len(t, state.Credentials, 2)
	require.Len(t, state.Bindings, 2)

	snapshot, err := store.ActiveSnapshot()
	require.NoError(t, err)
	byName := map[string]ActiveBinding{}
	for _, binding := range snapshot.Bindings {
		byName[binding.Name] = binding
	}
	require.Nil(t, byName["gitlab-api"].Exchange)
	require.Equal(t, &ExchangeRef{Credential: "spi-signer"}, byName["internal-api"].Exchange)
	require.Equal(t, "secret-token", byName["gitlab-api"].Headers[0].Value)
	require.Contains(t, snapshot.Redactions, "secret-token")
}

func TestCredentialVaultExchangeRequiresResolverConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		config ResolverConfig
	}{
		{"unset", ResolverConfig{}},
		{"timeout only", ResolverConfig{TimeoutConfigured: true}},
		{"auth only", ResolverConfig{AuthTableParsed: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore(nil, func() bool { return true })
			store.SetResolverConfig(tc.config)
			_, err := store.Create(testExchangeVaultRequest(), testExchangePolicy(t))
			require.ErrorContains(t, err, "credential resolver operator configuration")
			_, err = store.PrepareReplace(ReplaceRequest{CreateRequest: testExchangeVaultRequest()}, testExchangePolicy(t))
			require.Error(t, err)
		})
	}

	store := NewStore(nil, func() bool { return true })
	store.SetResolverConfig(ResolverConfig{TimeoutConfigured: true, AuthTableParsed: true})
	_, err := store.Create(testExchangeVaultRequest(), testExchangePolicy(t))
	require.NoError(t, err)
}

func TestCredentialVaultExchangeGateCoversUnreferencedCredentials(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	request := CreateRequest{Credentials: []Credential{testExchangeCredential("unused-exchange")}}
	_, err := store.Create(request, testExchangePolicy(t))
	require.ErrorContains(t, err, "credential resolver operator configuration")

	store.SetResolverConfig(ResolverConfig{TimeoutConfigured: true, AuthTableParsed: true})
	_, err = store.Create(request, testExchangePolicy(t))
	require.NoError(t, err)
}

func TestCredentialVaultLiveRevisionRuntimeRejectsExchange(t *testing.T) {
	store := NewLiveStore(nil, func() bool { return true }, mitmpassthrough.Config{})
	store.SetResolverConfig(ResolverConfig{TimeoutConfigured: true, AuthTableParsed: true})
	_, err := store.Create(testExchangeVaultRequest(), testExchangePolicy(t))
	require.ErrorContains(t, err, "exchange credentials are not supported in the live revision runtime")
	_, err = store.PrepareCreate(testExchangeVaultRequest(), testExchangePolicy(t))
	require.ErrorContains(t, err, "exchange credentials are not supported in the live revision runtime")

	// The live runtime still admits inline-only vaults unchanged.
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	_, err = store.Create(testCredentialVaultRequest(), pol)
	require.NoError(t, err)
}

func TestCredentialVaultExchangeReloadRequiresNoResolverListener(t *testing.T) {
	// Reload runs factory validation and the capability gate only — it must
	// never dial the resolver, so it succeeds without anything listening on
	// the endpoint socket.
	store := testResolverReadyStore()
	require.NoError(t, store.Reload(4, testExchangeVaultRequest(), testExchangePolicy(t)))
	snapshot, err := store.ActiveSnapshot()
	require.NoError(t, err)
	require.Equal(t, int64(4), snapshot.Revision)
	require.Equal(t, &ExchangeRef{Credential: "spi-signer"}, snapshot.Bindings[0].Exchange)

	closed := NewStore(nil, func() bool { return true })
	err = closed.Reload(4, testExchangeVaultRequest(), testExchangePolicy(t))
	require.ErrorContains(t, err, "credential resolver operator configuration")
}

func TestCredentialVaultExchangeVaultFileV1RoundTrip(t *testing.T) {
	body := VaultFileBody{
		Credentials: []Credential{testExchangeCredential("spi-signer")},
		Bindings:    testExchangeVaultRequest().Bindings,
	}
	encoded, err := EncodeVaultFile("incarnation-1", "attachment-1", 2, body)
	require.NoError(t, err)
	_, decoded, err := DecodeVaultFile(encoded)
	require.NoError(t, err)
	require.JSONEq(t, string(mustMarshal(body.Credentials[0].Source)), string(decoded.Credentials[0].Source))
	require.Equal(t, Auth{Credential: "spi-signer"}, decoded.Bindings[0].Auth)

	store := testResolverReadyStore()
	require.NoError(t, store.Reload(2, CreateRequest(decoded), testExchangePolicy(t)))
	snapshot, err := store.ActiveSnapshot()
	require.NoError(t, err)
	require.Equal(t, &ExchangeRef{Credential: "spi-signer"}, snapshot.Bindings[0].Exchange)
}

func TestRenderInjectionHeadersRejectsTypelessDefense(t *testing.T) {
	credentials := map[string]record{
		"spi-signer": {Name: "spi-signer", SourceType: "exchange", Source: &exchangeSource{endpoint: "unix:///run/spi.sock"}},
	}
	headers, redactions, err := renderInjectionHeaders(context.Background(), Auth{Credential: "spi-signer"}, credentials)
	require.ErrorContains(t, err, "cannot be rendered as static injection headers")
	require.Nil(t, headers)
	require.Nil(t, redactions)
}
