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

package nftables

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeNFTIntervalSet_cgnatContainsNameservers(t *testing.T) {
	got, err := normalizeNFTIntervalSet([]string{
		"100.64.0.0/10",
		"127.0.0.1",
		"100.100.2.136",
		"100.100.2.138",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"100.64.0.0/10", "127.0.0.1"}, got)
}

func TestNormalizeNFTIntervalSet_noChange(t *testing.T) {
	got, err := normalizeNFTIntervalSet([]string{"1.1.1.1", "2.2.0.0/16"})
	require.NoError(t, err)
	require.Equal(t, []string{"1.1.1.1", "2.2.0.0/16"}, got)
}

func TestNormalizeNFTIntervalSet_dedupe(t *testing.T) {
	got, err := normalizeNFTIntervalSet([]string{"8.8.8.8", "8.8.8.8"})
	require.NoError(t, err)
	require.Equal(t, []string{"8.8.8.8"}, got)
}

func TestNormalizeNFTIntervalSet_ipv6(t *testing.T) {
	got, err := normalizeNFTIntervalSet([]string{
		"2001:db8::/32",
		"2001:db8::1",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"2001:db8::/32"}, got)
}

func TestNormalizeNFTIntervalSet_invalid(t *testing.T) {
	_, err := normalizeNFTIntervalSet([]string{"not-an-ip"})
	require.Error(t, err)
}
