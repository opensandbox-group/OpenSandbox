// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func TestDomainRefresh_KeepsCapacityChargedWhileGrantsRemainLive(t *testing.T) {
	for _, limit := range []string{"domains", "addresses"} {
		t.Run(limit, func(t *testing.T) {
			var writes int
			manager := NewManagerWithRunner(func(context.Context, string) ([]byte, error) {
				writes++
				return nil, nil
			})
			ctx := context.Background()
			p, err := policy.ParsePolicy(`{"egress":[{"action":"allow","target":"*.example.com"}]}`)
			require.NoError(t, err)
			require.NoError(t, manager.ApplyStatic(ctx, p))
			now := time.Unix(1_000, 0)
			manager.tracker.now = func() time.Time { return now }

			const name = "all.example.com"
			limitSize, overflowName := maxDomainAddresses, name
			if limit == "domains" {
				limitSize, overflowName = maxResolvedDomains, "new.example.com"
			}
			previous := make([]ResolvedIP, limitSize)
			for i := range previous {
				previous[i] = ResolvedIP{Addr: netip.AddrFrom4([4]byte{192, 0, 2, byte(i + 1)}), TTL: time.Minute}
			}
			if limit == "domains" {
				for i := range previous {
					require.NoError(t, manager.AddResolvedDomain(ctx, fmt.Sprintf("%d.example.com", i), previous[i:i+1]))
				}
			} else {
				require.NoError(t, manager.AddResolvedDomain(ctx, name, previous))
			}
			now = now.Add(domainRefreshLead)
			manager.refreshDomains(ctx, func(context.Context, string) ([]ResolvedIP, error) {
				if limit == "addresses" {
					return previous[:1], nil
				}
				return nil, nil
			})

			before := writes
			fresh := []ResolvedIP{{Addr: netip.MustParseAddr("198.51.100.1"), TTL: time.Minute}}
			require.Error(t, manager.AddResolvedDomain(ctx, overflowName, fresh),
				"no longer confirmed does not mean no longer installed")
			require.Equal(t, before, writes, "running out of tracking space must not install a grant")
			now = now.Add(domainRefreshLead + time.Second)
			require.NoError(t, manager.AddResolvedDomain(ctx, overflowName, fresh), "expired observations no longer hold capacity")
			require.Equal(t, before+1, writes)
		})
	}
}
