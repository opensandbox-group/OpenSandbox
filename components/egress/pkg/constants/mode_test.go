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

package constants

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseEgressMode(t *testing.T) {
	s, err := ParseEgressMode("")
	require.NoError(t, err)
	require.Equal(t, PolicyDnsOnly, s)

	s, err = ParseEgressMode("dns")
	require.NoError(t, err)
	require.Equal(t, PolicyDnsOnly, s)

	s, err = ParseEgressMode("dns+nft")
	require.NoError(t, err)
	require.Equal(t, PolicyDnsNft, s)

	s, err = ParseEgressMode("nft+dns")
	require.NoError(t, err)
	require.Equal(t, PolicyDnsNft, s)

	_, err = ParseEgressMode("nft")
	require.Error(t, err)

	_, err = ParseEgressMode("dns+unknown")
	require.Error(t, err)

	_, err = ParseEgressMode("dns+mitm")
	require.Error(t, err)
}

func TestModeUsesNft(t *testing.T) {
	require.False(t, ModeUsesNft(""))
	require.False(t, ModeUsesNft(PolicyDnsOnly))
	require.True(t, ModeUsesNft(PolicyDnsNft))
	require.True(t, ModeUsesNft("  DNS+nft  "))
}

func TestIsForwardMode(t *testing.T) {
	require.True(t, IsForwardMode("forward"))
	require.True(t, IsForwardMode("  FORWARD "))
	require.False(t, IsForwardMode(""))
	require.False(t, IsForwardMode(PolicyDnsNft))
	require.False(t, IsForwardMode("dns+forward"))

	// The legacy parser never accepts forward, so it cannot be normalized
	// into a dns-only fallback by callers that use ParseEgressMode.
	_, err := ParseEgressMode("forward")
	require.Error(t, err)
}

func TestContainsForwardToken(t *testing.T) {
	require.True(t, ContainsForwardToken("forward"))
	require.True(t, ContainsForwardToken("forward+nft"))
	require.True(t, ContainsForwardToken("dns+forward"))
	require.True(t, ContainsForwardToken("  DNS + FORWARD  "))
	require.True(t, ContainsForwardToken("forward+"))
	require.True(t, ContainsForwardToken("+forward"))
	require.False(t, ContainsForwardToken(""))
	require.False(t, ContainsForwardToken(PolicyDnsOnly))
	require.False(t, ContainsForwardToken(PolicyDnsNft))
	require.False(t, ContainsForwardToken("forwarded"))
}
