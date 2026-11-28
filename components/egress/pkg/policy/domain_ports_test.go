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

package policy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDomainPortsCombineWithoutWholeAddressGrant(t *testing.T) {
	p, err := ParseClassicPolicy(`{"egress":[
		{"action":"allow","target":"*.example.com","ports":[443]},
		{"action":"allow","target":"api.example.com","ports":[8443,443]}]}`)
	require.NoError(t, err)
	permission := p.DomainPermissions("API.EXAMPLE.COM.")
	require.True(t, permission.Allowed)
	require.False(t, permission.Unrestricted)
	require.Equal(t, []int{443, 8443}, permission.AllowPorts)
	require.Equal(t, ActionDeny, p.Evaluate("example.com"), "wildcard does not include its apex")
}

func TestDomainPortsWildcardSuffixBoundary(t *testing.T) {
	legacy, err := ParsePolicy(`{"egress":[{"action":"allow","target":"*.internal"}]}`)
	require.NoError(t, err)
	require.Equal(t, ActionAllow, legacy.Evaluate("registry.internal"))
	require.Equal(t, ActionDeny, legacy.Evaluate("internal"))

	scoped, err := ParseClassicPolicy(`{"egress":[{"action":"allow","target":"*.internal","ports":[443]}]}`)
	require.NoError(t, err)
	require.Error(t, scoped.ValidateClassicDomainPorts())

	scoped, err = ParseClassicPolicy(`{"egress":[{"action":"allow","target":"*.svc.internal","ports":[443]}]}`)
	require.NoError(t, err)
	require.NoError(t, scoped.ValidateClassicDomainPorts())
	require.Equal(t, DomainPermissions{Allowed: true, AllowPorts: []int{443}}, scoped.DomainPermissions("registry.svc.internal"))
	require.Equal(t, ActionDeny, scoped.Evaluate("svc.internal"))
}

func TestClassicScopedPolicyRejectsUnsupportedActions(t *testing.T) {
	for _, raw := range []string{
		`{"defaultAction":"allow","egress":[{"action":"allow","target":"api.example.com","ports":[443]}]}`,
		`{"egress":[{"action":"deny","target":"api.example.com","ports":[443]}]}`,
		`{"egress":[{"action":"allow","target":"10.0.0.0/255.0.0.0","ports":[443]}]}`,
	} {
		p, err := ParseClassicPolicy(raw)
		require.NoError(t, err)
		require.Error(t, p.ValidateClassicDomainPorts())
	}
}

func TestDomainPortsPreserveExplicitNameDeny(t *testing.T) {
	p, err := ParseClassicPolicy(`{"egress":[
		{"action":"allow","target":"api.example.com","ports":[443]},
		{"action":"deny","target":"api.example.com"}]}`)
	require.NoError(t, err)
	require.Equal(t, ActionDeny, p.Evaluate("api.example.com"))
	require.Empty(t, p.DomainPermissions("api.example.com"))
}
