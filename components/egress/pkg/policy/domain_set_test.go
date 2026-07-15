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

package policy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDomainSet_ExactMatch(t *testing.T) {
	d := NewDomainSet([]string{"metadata.internal"})
	require.True(t, d.Match("metadata.internal"), "exact pattern must match identical host")
	require.False(t, d.Match("other.internal"), "different host must not match")
	require.False(t, d.Match("sub.metadata.internal"), "exact pattern must not match subdomain")
}

func TestDomainSet_WildcardMatchesSubdomainOnly(t *testing.T) {
	d := NewDomainSet([]string{"*.cluster.local"})
	require.True(t, d.Match("svc.cluster.local"), "wildcard must match one-level subdomain")
	require.True(t, d.Match("a.b.cluster.local"), "wildcard must match deeper subdomain")
	require.False(t, d.Match("cluster.local"), "wildcard *.foo must not match bare foo (matches compiledDomainIndex semantics)")
	require.False(t, d.Match("notcluster.local"), "wildcard suffix must align on dot boundary")
}

func TestDomainSet_CombinedExactAndWildcard(t *testing.T) {
	d := NewDomainSet([]string{"cluster.local", "*.cluster.local"})
	require.True(t, d.Match("cluster.local"), "exact entry covers bare host")
	require.True(t, d.Match("svc.cluster.local"), "wildcard entry covers subdomain")
}

func TestDomainSet_CaseInsensitive(t *testing.T) {
	d := NewDomainSet([]string{"Metadata.Internal"})
	require.True(t, d.Match("METADATA.INTERNAL"), "match must be case-insensitive")
}

func TestDomainSet_TrailingDotStripped(t *testing.T) {
	d := NewDomainSet([]string{"metadata.internal"})
	require.True(t, d.Match("metadata.internal."), "trailing dot in queried host must be stripped before match")
}

func TestDomainSet_EmptyOrNilNeverMatches(t *testing.T) {
	require.False(t, NewDomainSet(nil).Match("foo.com"))
	require.False(t, NewDomainSet([]string{}).Match("foo.com"))
	require.False(t, NewDomainSet([]string{"", "  "}).Match("foo.com"), "whitespace-only entries are ignored")
	var d *DomainSet
	require.False(t, d.Match("foo.com"), "nil receiver must not panic and must return false")
}

func TestDomainSet_Empty(t *testing.T) {
	require.True(t, NewDomainSet(nil).Empty())
	require.True(t, NewDomainSet([]string{"  "}).Empty(), "whitespace-only patterns produce empty set")
	require.False(t, NewDomainSet([]string{"foo.com"}).Empty())
	var d *DomainSet
	require.True(t, d.Empty(), "nil receiver reports empty")
}

func TestDomainSet_EmptyHostNeverMatches(t *testing.T) {
	d := NewDomainSet([]string{"foo.com"})
	require.False(t, d.Match(""), "empty host must not match anything")
}
