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
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetupRedirect_BuildsNativeNftScript(t *testing.T) {
	var scripts []string
	run := func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	}

	err := setupRedirect(context.Background(), run, 15353, []netip.Addr{
		netip.MustParseAddr("10.179.156.2"),
		netip.MustParseAddr("fd00::53"),
	})
	require.NoError(t, err)

	// Cleanup deletes every family first, then a single add-only apply.
	require.Len(t, scripts, len(dnsRedirectFamilies)+1)
	for i, family := range dnsRedirectFamilies {
		require.Equal(t, "delete table "+family+" opensandbox_dns_redirect\n", scripts[i])
	}

	add := scripts[len(scripts)-1]
	require.Contains(t, add, "add table ip opensandbox_dns_redirect")
	require.Contains(t, add, "add table ip6 opensandbox_dns_redirect")
	require.NotContains(t, add, "add table inet opensandbox_dns_redirect")
	require.Contains(t, add, "type nat hook output priority -100; policy accept;")
	require.Contains(t, add, "udp dport 53 ip daddr 10.179.156.2 return")
	require.Contains(t, add, "tcp dport 53 ip6 daddr fd00::53 return")
	require.Contains(t, add, "meta mark 0x1 udp dport 53 return")
	require.Contains(t, add, "udp dport 53 redirect to :15353")
	require.Contains(t, add, "tcp dport 53 redirect to :15353")
	require.NotContains(t, add, "hook prerouting")
	// No iptables anywhere: the whole path is native nft.
	require.NotContains(t, add, "iptables")
}

func TestSetupRedirect_ToleratesMissingTableOnCleanup(t *testing.T) {
	run := func(_ context.Context, script string) ([]byte, error) {
		if strings.HasPrefix(script, "delete table ") {
			return []byte("Error: Could not process rule: No such file or directory"), errors.New("exit status 1")
		}
		return nil, nil
	}

	require.NoError(t, setupRedirect(context.Background(), run, 15353, nil))
}

func TestSetupRedirect_ReturnsApplyError(t *testing.T) {
	run := func(_ context.Context, script string) ([]byte, error) {
		if strings.HasPrefix(script, "add table ") {
			return []byte("Error: Could not process rule: Operation not supported"), errors.New("exit status 1")
		}
		return nil, nil
	}

	err := setupRedirect(context.Background(), run, 15353, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nft DNS redirect apply failed")
	require.Contains(t, err.Error(), "Operation not supported")
}

func TestSetupRedirect_ReturnsCleanupError(t *testing.T) {
	run := func(_ context.Context, _ string) ([]byte, error) {
		return []byte("Operation not permitted"), errors.New("exit status 1")
	}

	err := setupRedirect(context.Background(), run, 15353, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nft delete table")
}
