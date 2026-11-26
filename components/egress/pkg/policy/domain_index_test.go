// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDomainPermissionsScopedLookupOrder(t *testing.T) {
	rules := []string{
		`{"action":"allow","target":"*.EXample.Test.","ports":[1443,443]}`,
		`{"action":"allow","target":"api.example.test.","ports":[8443,443]}`,
		`{"action":"allow","target":"*.api.example.test","ports":[10443,443]}`,
		`{"action":"allow","target":"*.example.test","ports":[2443,1443]}`,
		`{"action":"allow","target":"leaf.api.example.test","ports":[30443,2443,31443,32443,33443]}`,
	}
	for _, tc := range []struct {
		name              string
		order             []int
		apex, leaf, child []int
	}{
		{"forward", []int{0, 1, 2, 3, 4}, []int{1443, 443, 8443, 2443},
			[]int{1443, 443, 10443, 2443, 30443, 31443, 32443, 33443}, []int{1443, 443, 10443, 2443}},
		{"reverse", []int{4, 3, 2, 1, 0}, []int{2443, 1443, 8443, 443},
			[]int{30443, 2443, 31443, 32443, 33443, 1443, 10443, 443}, []int{2443, 1443, 10443, 443}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ordered := make([]string, 0, len(rules))
			for _, i := range tc.order {
				ordered = append(ordered, rules[i])
			}
			p, err := ParseClassicPolicy(`{"egress":[` + strings.Join(ordered, ",") + `]}`)
			require.NoError(t, err)
			require.NoError(t, p.ValidateClassicDomainPorts())
			linear := *p
			linear.domainIndex = nil
			for _, query := range []struct {
				name  string
				ports []int
			}{
				{"API.Example.Test.", tc.apex},
				{"LEAF.api.Example.test.", tc.leaf},
				{"nested.leaf.api.example.test", tc.child},
				{"example.test.", nil},
				{"example.test.invalid", nil},
				{"notexample.test", nil},
			} {
				want := DomainPermissions{Allowed: len(query.ports) != 0, AllowPorts: query.ports}
				got := p.DomainPermissions(query.name)
				require.Equal(t, want, got, query.name)
				require.Equal(t, want, linear.DomainPermissions(query.name), query.name)
				if len(got.AllowPorts) != 0 {
					got.AllowPorts[0] = 1
					require.Equal(t, want, p.DomainPermissions(query.name), "returned ports must not alias policy state")
				}
			}
		})
	}
}

func TestDomainPermissionsScopedLookupPreservesPortlessOrder(t *testing.T) {
	p, err := ParseClassicPolicy(`{"egress":[
		{"action":"allow","target":"api.blocked.test","ports":[443]},
		{"action":"deny","target":"*.blocked.test"},
		{"action":"allow","target":"api.blocked.test"},
		{"action":"allow","target":"*.open.test"},
		{"action":"deny","target":"api.open.test"},
		{"action":"allow","target":"api.open.test","ports":[443]}]}`)
	require.NoError(t, err)
	linear := *p
	linear.domainIndex = nil
	for _, candidate := range []*NetworkPolicy{p, &linear} {
		require.Empty(t, candidate.DomainPermissions("API.BLOCKED.TEST."))
		require.Equal(t, DomainPermissions{Allowed: true, Unrestricted: true}, candidate.DomainPermissions("api.open.test"))
	}
}

func TestDomainPermissionsNestedGroupsPreserveFirstPortOrder(t *testing.T) {
	const query = "a.b.c.d.e.example.test"
	targets := []string{"*.d.e.example.test", query, "*.example.test", "*.b.c.d.e.example.test", "*.e.example.test", "*.c.d.e.example.test"}
	declared := []int{8443, 22, 9443, 3000, 80, 8000, 443, 9000, 5432, 81, 8080, 1443}
	var rules []string
	for i, port := range declared {
		rules = append(rules, fmt.Sprintf(`{"action":"allow","target":%q,"ports":[%d,53]}`, targets[i%len(targets)], port))
	}
	p, err := ParseClassicPolicy(`{"egress":[` + strings.Join(rules, ",") + `]}`)
	require.NoError(t, err)
	require.Equal(t, []int{8443, 53, 22, 9443, 3000, 80, 8000, 443, 9000, 5432, 81, 8080, 1443}, p.DomainPermissions(query).AllowPorts)
}

func TestDomainPermissionsScopedIndexRetainsRejectedDeclarations(t *testing.T) {
	for _, tc := range []struct {
		action, target, reason string
	}{
		{"allow", ".", "must be ASCII FQDNs or leftmost wildcards"},
		{"deny", "api.example.test", "only allow rules may combine domain targets with TCP ports"},
	} {
		p, err := ParseClassicPolicy(fmt.Sprintf(`{"egress":[{"action":%q,"target":%q,"ports":[443]}]}`, tc.action, tc.target))
		require.NoError(t, err)
		require.True(t, p.HasDomainPorts(), "unsupported declarations must still be visible to validation")
		require.ErrorContains(t, p.ValidateClassicDomainPorts(), tc.reason)
		require.Empty(t, p.DomainPermissions(tc.target))
	}
}

var scopedLookupBenchmarkResult DomainPermissions

func BenchmarkDomainPermissionsScoped4096(b *testing.B) {
	var raw strings.Builder
	raw.WriteString(`{"egress":[`)
	for i := range 4096 {
		if i != 0 {
			raw.WriteByte(',')
		}
		if i%2 == 0 {
			fmt.Fprintf(&raw, `{"action":"allow","target":"rule-%d.example.test","ports":[443,8443]}`, i)
		} else {
			fmt.Fprintf(&raw, `{"action":"allow","target":"*.zone-%d.example.test","ports":[443,8443]}`, i)
		}
	}
	raw.WriteString(`]}`)
	p, err := ParseClassicPolicy(raw.String())
	require.NoError(b, err)
	for _, tc := range []struct {
		name, query string
		ports       []int
	}{
		{"ExactHit", "rule-4094.example.test", []int{443, 8443}},
		{"ExactMiss", "missing.example.test", nil},
		{"Wildcard", "api.zone-4095.example.test", []int{443, 8443}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			require.Equal(b, DomainPermissions{Allowed: len(tc.ports) != 0, AllowPorts: tc.ports}, p.DomainPermissions(tc.query))
			b.ReportAllocs()
			for b.Loop() {
				scopedLookupBenchmarkResult = p.DomainPermissions(tc.query)
			}
		})
	}
}

func BenchmarkDomainPermissionsOverlapping4096(b *testing.B) {
	const pair = `{"action":"allow","target":"api.example.test","ports":[443,8443]},{"action":"allow","target":"*.example.test","ports":[443,8443]},`
	p, err := ParseClassicPolicy(`{"egress":[` + strings.TrimSuffix(strings.Repeat(pair, 2048), ",") + `]}`)
	require.NoError(b, err)
	require.Equal(b, []int{443, 8443}, p.DomainPermissions("api.example.test").AllowPorts)
	b.ReportAllocs()
	for b.Loop() {
		scopedLookupBenchmarkResult = p.DomainPermissions("api.example.test")
	}
}
