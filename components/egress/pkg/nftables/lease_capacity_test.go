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

func TestAddResolvedDomain_SharedAddressDoesNotHoldExpiredDomainSlot(t *testing.T) {
	ctx := context.Background()
	p, err := policy.ParsePolicy(`{"egress":[{"action":"allow","target":"*.example.com"}]}`)
	require.NoError(t, err)
	m := NewManagerWithRunner(func(context.Context, string) ([]byte, error) { return nil, nil })
	require.NoError(t, m.ApplyStatic(ctx, p))
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	shared := netip.MustParseAddr("192.0.2.1")
	require.NoError(t, m.AddResolvedDomain(ctx, "short.example.com", []ResolvedIP{{Addr: shared, TTL: time.Second}}))
	for i := range maxResolvedDomains - 1 {
		long := []ResolvedIP{{Addr: shared, TTL: 300 * time.Second}}
		require.NoError(t, m.AddResolvedDomain(ctx, fmt.Sprintf("long%d.example.com", i), long))
	}
	fresh := []ResolvedIP{{Addr: netip.MustParseAddr("198.51.100.1"), TTL: 300 * time.Second}}
	require.Error(t, m.AddResolvedDomain(ctx, "new.example.com", fresh), "all domain slots are live")
	now = now.Add(62 * time.Second)
	require.NoError(t, m.AddResolvedDomain(ctx, "new.example.com", fresh), "the short-lived name no longer owns the shared IP")
	require.Error(t, m.AddResolvedDomain(ctx, "short.example.com", []ResolvedIP{{Addr: shared}}), "the long-lived names still hold their slots")
}
