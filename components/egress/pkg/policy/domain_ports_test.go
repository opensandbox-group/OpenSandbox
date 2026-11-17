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
	p, err := ParsePolicy(`{"egress":[
		{"action":"allow","target":"*.example.com","ports":[443]},
		{"action":"allow","target":"api.example.com","ports":[8443,443]},
		{"action":"deny","target":"api.example.com","ports":[443]}]}`)
	require.NoError(t, err)
	permission := p.DomainPermissions("API.EXAMPLE.COM.")
	require.True(t, permission.Allowed)
	require.False(t, permission.Unrestricted)
	require.Equal(t, []int{443, 8443}, permission.AllowPorts)
	require.Equal(t, []int{443}, permission.DenyPorts)
	require.False(t, p.AllowsEntireHostSelectorPort("api.example.com", 443))
	require.True(t, p.AllowsEntireHostSelectorPort("api.example.com", 8443))
	require.False(t, p.AllowsEntireHostSelectorPort("api.example.com", 80))
	require.Equal(t, ActionDeny, p.Evaluate("example.com"), "wildcard does not include its apex")
}

func TestDomainPortDenyDoesNotDenyEntireName(t *testing.T) {
	p, err := ParsePolicy(`{"defaultAction":"allow","egress":[
		{"action":"deny","target":"api.example.com","ports":[25]}]}`)
	require.NoError(t, err)
	permission := p.DomainPermissions("api.example.com")
	require.True(t, permission.Allowed)
	require.True(t, permission.Unrestricted)
	require.Equal(t, []int{25}, permission.DenyPorts)
	require.False(t, p.AllowsEntireHostSelectorPort("api.example.com", 25))
	require.True(t, p.AllowsEntireHostSelectorPort("api.example.com", 443))
}

func TestDomainPortsPreserveNameGateAndKnownAddressDenies(t *testing.T) {
	p, err := ParsePolicy(`{"egress":[
		{"action":"allow","target":"api.example.com","ports":[443]},
		{"action":"deny","target":"api.example.com"},
		{"action":"deny","target":"api.example.com","ports":[25]}]}`)
	require.NoError(t, err)
	require.Equal(t, ActionDeny, p.Evaluate("api.example.com"))
	require.Equal(t, DomainPermissions{DenyPorts: []int{25}}, p.DomainPermissions("api.example.com"))
	require.False(t, p.AllowsEntireHostSelectorPort("api.example.com", 443))
}

func TestDomainPortDenyDoesNotRevokeScopedDNSAdmission(t *testing.T) {
	p, err := ParsePolicy(`{"egress":[
		{"action":"allow","target":"api.example.com","ports":[443]},
		{"action":"deny","target":"*.example.com","ports":[443]}]}`)
	require.NoError(t, err)
	require.Equal(t, ActionAllow, p.Evaluate("api.example.com"))
	require.False(t, p.AllowsEntireHostSelectorPort("api.example.com", 443))
	require.Equal(t, []int{443}, p.DomainPermissions("api.example.com").DenyPorts)
}

func TestDomainPortsEntireSelectorCoverageUsesActualPort(t *testing.T) {
	cases := []struct {
		name    string
		policy  string
		host    string
		port    int
		allowed bool
	}{
		{"https wildcard", `{"egress":[{"action":"allow","target":"*.example.com","ports":[443]}]}`, "*.example.com", 443, true},
		{"http not covered", `{"egress":[{"action":"allow","target":"*.example.com","ports":[443]}]}`, "*.example.com", 80, false},
		{"one host cannot cover wildcard", `{"egress":[{"action":"allow","target":"api.example.com","ports":[443]}]}`, "*.example.com", 443, false},
		{"overlapping port deny", `{"egress":[{"action":"allow","target":"*.example.com","ports":[443]},{"action":"deny","target":"private.example.com","ports":[443]}]}`, "*.example.com", 443, false},
		{"different denied port", `{"egress":[{"action":"allow","target":"*.example.com","ports":[443]},{"action":"deny","target":"private.example.com","ports":[80]}]}`, "*.example.com", 443, true},
		{"legacy name deny", `{"egress":[{"action":"deny","target":"private.example.com"},{"action":"allow","target":"*.example.com","ports":[443]}]}`, "*.example.com", 443, false},
		{"earlier legacy allow shadows name deny", `{"egress":[{"action":"allow","target":"*.example.com"},{"action":"deny","target":"private.example.com"},{"action":"deny","target":"*.example.com","ports":[25]}]}`, "*.example.com", 443, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ParsePolicy(tc.policy)
			require.NoError(t, err)
			require.Equal(t, tc.allowed, p.AllowsEntireHostSelectorPort(tc.host, tc.port))
		})
	}
}
