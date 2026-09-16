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
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type emptyCredentialSource struct{}

func (emptyCredentialSource) Type() string                            { return "empty" }
func (emptyCredentialSource) Resolve(context.Context) (string, error) { return "", nil }

func renderedBinding(name string, schemes, hosts []string, secret string) ActiveBinding {
	headers := []InjectionHeader{}
	if secret != "" {
		headers = append(headers, InjectionHeader{Name: "Private-Token", Value: secret})
	}
	return ActiveBinding{
		Name: name,
		Match: Match{
			Schemes: schemes,
			Hosts:   hosts,
			Methods: []string{"GET"},
			Paths:   []string{"/api/*"},
		},
		Headers:       headers,
		Substitutions: []InjectionSubstitution{},
	}
}

func TestMarshalDecisionSnapshotDerivesCanonicalTLSSelectors(t *testing.T) {
	snapshot := ActiveSnapshot{
		Revision: 7,
		Bindings: []ActiveBinding{
			renderedBinding("z-http", []string{"http"}, []string{"plain.example.com"}, ""),
			renderedBinding(
				"a-https",
				[]string{"https", "http"},
				[]string{"*.example.com", "api.example.com"},
				"never-log-me",
			),
		},
		Redactions: []string{"never-log-me"},
	}
	originalOrder := []string{snapshot.Bindings[0].Name, snapshot.Bindings[1].Name}
	payload, err := MarshalDecisionSnapshot(snapshot, 11)
	require.NoError(t, err)
	require.NotContains(t, string(payload), "controlGeneration")
	require.NotContains(t, string(payload), "decisionEpoch")
	require.NotContains(t, string(payload), "digest")
	require.NotContains(t, string(payload), ":null")

	var decoded decisionSnapshot
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Equal(t, 1, decoded.Version)
	require.Equal(t, int64(7), decoded.VaultRevision)
	require.Equal(t, int64(11), decoded.EffectivePolicyEpoch)
	require.Equal(t, "credential-bound", decoded.InterceptionMode)
	require.Equal(t, "active", decoded.State)
	require.Equal(t, []string{"*.example.com", "api.example.com"}, decoded.TLSBindingHostSelectors)
	require.Equal(t, []string{"a-https", "z-http"}, []string{
		decoded.FullRenderedBindings[0].Name,
		decoded.FullRenderedBindings[1].Name,
	})
	require.Equal(t, []string{"never-log-me"}, decoded.Redactions)
	require.Equal(t, originalOrder, []string{snapshot.Bindings[0].Name, snapshot.Bindings[1].Name})

	reversed := snapshot
	reversed.Bindings = slices.Clone(snapshot.Bindings)
	slices.Reverse(reversed.Bindings)
	again, err := MarshalDecisionSnapshot(reversed, 11)
	require.NoError(t, err)
	require.Equal(t, payload, again)
}

func TestMarshalDecisionSnapshotEmitsAuthoritativeEmptyArrays(t *testing.T) {
	payload, err := MarshalDecisionSnapshot(ActiveSnapshot{Revision: 0}, 0)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"version": 1,
		"vaultRevision": 0,
		"effectivePolicyEpoch": 0,
		"interceptionMode": "credential-bound",
		"state": "active-empty",
		"tlsBindingHostSelectors": [],
		"fullRenderedBindings": [],
		"redactions": []
	}`, string(payload))
}

func TestMarshalDecisionSnapshotAcceptsRenderedStoreOutput(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	policy := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	_, err := store.Create(testCredentialVaultRequest(), policy)
	require.NoError(t, err)
	snapshot, err := store.ActiveSnapshot()
	require.NoError(t, err)
	payload, err := MarshalDecisionSnapshot(snapshot, 3)
	require.NoError(t, err)
	var decoded decisionSnapshot
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Equal(t, []string{"code.example.com"}, decoded.TLSBindingHostSelectors)
	require.Len(t, decoded.FullRenderedBindings, 1)
	require.Equal(t, snapshot.Bindings[0].Name, decoded.FullRenderedBindings[0].Name)
	require.Equal(t, snapshot.Bindings[0].Match, decoded.FullRenderedBindings[0].Match)
	require.Equal(t, snapshot.Bindings[0].Headers, decoded.FullRenderedBindings[0].Headers)
	require.Empty(t, decoded.FullRenderedBindings[0].Substitutions)
	require.Equal(t, snapshot.Redactions, decoded.Redactions)
}

func TestMarshalDecisionSnapshotAcceptsEmptyRenderedSubstitutionValue(t *testing.T) {
	registry := NewSourceRegistry()
	registry.Register("empty", func(json.RawMessage) (CredentialSource, error) {
		return emptyCredentialSource{}, nil
	})
	store := NewStoreWithRegistry(nil, func() bool { return true }, registry)
	policy := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	request := testCredentialVaultRequest()
	request.Credentials[0].Source = mustMarshal(map[string]string{"type": "empty"})
	request.Bindings[0].Auth = Auth{
		Type: "passthrough",
		Substitutions: []Substitution{{
			Credential: "gitlab-token", Placeholder: "__token__", In: []string{"header"},
		}},
	}
	_, err := store.Create(request, policy)
	require.NoError(t, err)
	snapshot, err := store.ActiveSnapshot()
	require.NoError(t, err)
	require.Contains(t, snapshot.Redactions, "__token__")
	require.NotContains(t, snapshot.Redactions, "")
	_, err = MarshalDecisionSnapshot(snapshot, 3)
	require.NoError(t, err)
}

func TestMarshalDecisionSnapshotRejectsNonCanonicalOrUnsafeInput(t *testing.T) {
	valid := ActiveSnapshot{
		Revision:   1,
		Bindings:   []ActiveBinding{renderedBinding("binding", []string{"https"}, []string{"api.example.com"}, "never-log-me")},
		Redactions: []string{"never-log-me"},
	}
	tests := []struct {
		name   string
		mutate func(*ActiveSnapshot) int64
	}{
		{"negative policy epoch", func(*ActiveSnapshot) int64 { return -1 }},
		{"negative vault revision", func(s *ActiveSnapshot) int64 { s.Revision = -1; return 1 }},
		{"bindings at revision zero", func(s *ActiveSnapshot) int64 { s.Revision = 0; return 1 }},
		{"duplicate binding", func(s *ActiveSnapshot) int64 { s.Bindings = append(s.Bindings, s.Bindings[0]); return 1 }},
		{"invalid selector", func(s *ActiveSnapshot) int64 { s.Bindings[0].Match.Hosts = []string{"EXAMPLE.com"}; return 1 }},
		{"noncanonical port", func(s *ActiveSnapshot) int64 { s.Bindings[0].Match.Ports = []int{443}; return 1 }},
		{"reserved rendered header", func(s *ActiveSnapshot) int64 { s.Bindings[0].Headers[0].Name = "Content-Length"; return 1 }},
		{"missing redaction", func(s *ActiveSnapshot) int64 { s.Redactions = nil; return 1 }},
		{"redaction without binding", func(s *ActiveSnapshot) int64 { s.Bindings = nil; return 1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := valid
			candidate.Bindings = slices.Clone(valid.Bindings)
			candidate.Bindings[0].Match = valid.Bindings[0].Match
			candidate.Bindings[0].Match.Hosts = slices.Clone(valid.Bindings[0].Match.Hosts)
			candidate.Bindings[0].Headers = slices.Clone(valid.Bindings[0].Headers)
			candidate.Redactions = slices.Clone(valid.Redactions)
			policyEpoch := tc.mutate(&candidate)
			_, err := MarshalDecisionSnapshot(candidate, policyEpoch)
			require.ErrorIs(t, err, ErrInvalidDecisionSnapshot)
			require.NotContains(t, err.Error(), "never-log-me")
		})
	}
}

func TestMarshalDecisionSnapshotRequiresCanonicalRedactionOrder(t *testing.T) {
	snapshot := ActiveSnapshot{Revision: 1, Redactions: []string{"short", "much-longer", "short"}}
	_, err := MarshalDecisionSnapshot(snapshot, 1)
	require.True(t, errors.Is(err, ErrInvalidDecisionSnapshot))
}

func TestMarshalDecisionSnapshotOmitsAcceptedEmptyWildcardLanguage(t *testing.T) {
	base := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." +
		strings.Repeat("c", 63) + "." + strings.Repeat("d", 60)
	require.Len(t, base, 252)
	snapshot := ActiveSnapshot{
		Revision: 1,
		Bindings: []ActiveBinding{renderedBinding(
			"legacy", []string{"https"}, []string{"*." + base}, "",
		)},
	}
	payload, err := MarshalDecisionSnapshot(snapshot, 1)
	require.NoError(t, err)
	var decoded decisionSnapshot
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Empty(t, decoded.TLSBindingHostSelectors)
	require.Equal(t, "*."+base, decoded.FullRenderedBindings[0].Match.Hosts[0])
}
