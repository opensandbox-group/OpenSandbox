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
