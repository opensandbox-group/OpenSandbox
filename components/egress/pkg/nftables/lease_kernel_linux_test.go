// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package nftables

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os/exec"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func TestNftDomainAnswerDoesNotShortenSharedLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	m := NewManagerWithOptions(Options{})
	p, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"*.example.com"}]}`)
	require.NoError(t, err)
	require.NoError(t, m.ApplyStatic(ctx, p))
	addr := netip.MustParseAddr("192.0.2.1")
	require.NoError(t, m.AddResolvedDomain(ctx, "long.example.com", []ResolvedIP{{Addr: addr, TTL: 300 * time.Second}}))
	require.NoError(t, m.AddResolvedDomain(ctx, "short.example.com", []ResolvedIP{{Addr: addr, TTL: time.Second}}))
	leases := readKernelLeaseExpiry(t, ctx)
	require.Contains(t, leases, addr.String())
	require.Greater(t, leases[addr.String()], 240, "the shorter answer must not replace a live long lease")
}

func TestNftPolicyReplacementKeepsOnlySurvivingDomainLeases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	m := NewManagerWithOptions(Options{})
	now := time.Unix(1000, 0)
	m.tracker.now = func() time.Time { return now }
	wide, err := policy.ParsePolicy(`{"egress":[{"action":"allow","target":"*.example.com"}]}`)
	require.NoError(t, err)
	tight, err := policy.ParsePolicy(`{"egress":[{"action":"allow","target":"keep.example.com"}]}`)
	require.NoError(t, err)
	require.NoError(t, m.ApplyStatic(ctx, wide))
	shared := netip.MustParseAddr("192.0.2.1")
	removed := netip.MustParseAddr("192.0.2.2")
	unattributed := netip.MustParseAddr("192.0.2.3")
	require.NoError(t, m.AddResolvedDomain(ctx, "keep.example.com", []ResolvedIP{{Addr: shared, TTL: 120 * time.Second}}))
	require.NoError(t, m.AddResolvedDomain(ctx, "remove.example.com", []ResolvedIP{{Addr: shared, TTL: 300 * time.Second}, {Addr: removed, TTL: 300 * time.Second}}))
	require.NoError(t, m.AddResolvedIPs(ctx, []ResolvedIP{{Addr: unattributed, TTL: 300 * time.Second}}))
	before := readKernelLeaseExpiry(t, ctx)
	require.Len(t, before, 3)
	require.Greater(t, before[shared.String()], 240)

	now = now.Add(70 * time.Second)
	require.NoError(t, m.ApplyStatic(ctx, tight))
	after := readKernelLeaseExpiry(t, ctx)
	require.Len(t, after, 1, "the revoked and unattributed leases must not survive")
	require.Contains(t, after, shared.String())
	require.Greater(t, after[shared.String()], 90, "the still-permitted source keeps its remaining lease")
	require.LessOrEqual(t, after[shared.String()], 110, "replacement cannot renew or inherit the revoked source's longer lease")
}

func TestNftUnknownPolicyApplyFencesDNSUntilVerifiedReplacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	m := NewManagerWithOptions(Options{})
	allow, err := policy.ParsePolicy(`{"egress":[{"action":"allow","target":"example.com"}]}`)
	require.NoError(t, err)
	require.NoError(t, m.ApplyStatic(ctx, allow))
	m.run = func(ctx context.Context, script string) ([]byte, error) {
		m.run = defaultRunner
		out, err := defaultRunner(ctx, script)
		if err != nil {
			return out, err
		}
		return out, &ApplyError{Effect: ApplyUnknown, Err: errors.New("lost nft apply acknowledgement")}
	}
	require.Error(t, m.ApplyStatic(ctx, policy.DefaultDenyPolicy()))
	ips := []ResolvedIP{{Addr: netip.MustParseAddr("192.0.2.4"), TTL: time.Minute}}
	require.Error(t, m.AddResolvedDomain(ctx, "example.com", ips), "an old-policy answer cannot be granted after an unacknowledged replacement")
	require.Empty(t, readKernelLeaseExpiry(t, ctx))
	require.NoError(t, m.ApplyStatic(ctx, allow))
	require.NoError(t, m.AddResolvedDomain(ctx, "example.com", ips))
	require.Contains(t, readKernelLeaseExpiry(t, ctx), ips[0].Addr.String())
}

func readKernelLeaseExpiry(t *testing.T, ctx context.Context) map[string]int {
	t.Helper()

	data, err := exec.CommandContext(ctx, "nft", "-j", "list", "set", "inet", tableName, dynAllowV4Set).CombinedOutput()
	require.NoError(t, err, "%s", data)
	var result struct {
		Nftables []struct {
			Set struct {
				Elem []struct {
					Elem struct {
						Val     string
						Expires int
					}
				}
			}
		}
	}
	require.NoError(t, json.Unmarshal(data, &result))
	leases := make(map[string]int)
	for _, entry := range result.Nftables {
		for _, wrapper := range entry.Set.Elem {
			leases[wrapper.Elem.Val] = wrapper.Elem.Expires
		}
	}
	return leases
}
