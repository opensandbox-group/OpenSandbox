// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package nftables

import (
	"context"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNftFirstPortlessDNSAfterWidening(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 85*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	startWithdrawalEchoPeer(t, ctx, "28443", "32443")
	m := NewManagerWithOptions(Options{})
	const host = "198.18.0.2"
	const other = "198.18.0.3"
	wide := mustPolicy(t, `{"defaultAction":"allow"}`)
	tight := mustPolicy(t, `{"defaultAction":"deny"}`)
	whole := mustPolicy(t, `{"egress":[{"action":"allow","target":"a.example"},{"action":"deny","ports":[32443]},{"action":"deny","target":"::ffff:198.18.0.2","ports":[28443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, wide))
	denied := openWithdrawalStream(t, host, "32443")
	unconfirmed := openWithdrawalStream(t, other, "28443")
	_, err := m.ApplyStaticComputingEstablishedDrops(ctx, wide, tight)
	require.NoError(t, err)
	require.NoError(t, m.ApplyStatic(ctx, whole)) // Ordinary replacement, with no DNS answer yet.
	grant := func(ip string) error {
		return m.AddResolvedDomain(ctx, "a.example", []ResolvedIP{{Addr: netip.MustParseAddr(ip), TTL: time.Second}})
	}
	m.run = func(ctx context.Context, script string) ([]byte, error) {
		out, err := defaultRunner(ctx, script)
		require.NoError(t, err, "%s", out)
		return out, &ApplyError{Effect: ApplyUnknown, Err: context.DeadlineExceeded}
	}
	require.ErrorIs(t, grant(other), context.DeadlineExceeded)
	m.run = defaultRunner
	require.ErrorIs(t, grant(host), errStaticStateUncertain, "unknown persistent history requires a confirmed rebuild")
	require.NoError(t, m.ApplyStatic(ctx, whole))
	requireWithdrawalDrop(t, ctx, unconfirmed, 51)

	require.NoError(t, grant(host))
	admitted := openWithdrawalStream(t, host, "28443")
	requireWithdrawalDrop(t, ctx, denied, 52)
	require.Eventually(t, func() bool {
		out, err := exec.CommandContext(ctx, "nft", "get", "element", "inet", tableName, dynAllowV4Set, "{", host, "}").CombinedOutput()
		return err != nil && strings.Contains(string(out), "No such file or directory")
	}, 68*time.Second, 250*time.Millisecond)
	require.NoError(t, withdrawalEcho(admitted, 53), "the first portless answer must protect its admitted flow beyond DNS expiry")
	require.NoError(t, m.ApplyStatic(ctx, whole))
	require.NoError(t, withdrawalEcho(admitted, 54), "an ordinary reload must preserve acknowledged established history")
	fresh, err := net.DialTimeout("tcp4", net.JoinHostPort(host, "28443"), 300*time.Millisecond)
	if fresh != nil {
		_ = fresh.Close()
	}
	var timeout net.Error
	require.ErrorAs(t, err, &timeout)
	require.True(t, timeout.Timeout(), "established history must not admit a new connection")
}

func TestNftPortlessDNSKeepsEffectiveAdmissionsFromBothPolicies(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	startWithdrawalEchoPeer(t, ctx, "2443", "28443", "32443")
	const host = "198.18.0.2"
	m := NewManagerWithOptions(Options{})
	wide := mustPolicy(t, `{"defaultAction":"allow"}`)
	old := mustPolicy(t, `{"egress":[{"action":"allow","target":"a.example"},{"action":"allow","target":"b.example"},{"action":"deny","ports":[28443,32443]}]}`)
	next := mustPolicy(t, `{"egress":[{"action":"allow","target":"b.example"},{"action":"deny","ports":[2443,32443]}]}`)
	require.NoError(t, m.ApplyStatic(ctx, wide))
	never := openWithdrawalStream(t, host, "32443")
	_, err := m.ApplyStaticComputingEstablishedDrops(ctx, wide, old)
	require.NoError(t, err)
	for _, name := range []string{"a.example", "b.example"} {
		require.NoError(t, m.AddResolvedDomain(ctx, name, []ResolvedIP{{Addr: netip.MustParseAddr(host)}}))
	}
	oldOnly := openWithdrawalStream(t, host, "2443")
	require.NoError(t, m.ApplyStatic(ctx, next))
	newOnly := openWithdrawalStream(t, host, "28443")
	require.NoError(t, withdrawalEcho(oldOnly, 61), "an ordinary deny does not cut an already admitted stream")

	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"defaultAction":"deny"}`)))
	require.NotContains(t, readKernelLeaseExpiry(t, ctx), host)
	require.NoError(t, withdrawalEcho(oldOnly, 62))
	require.NoError(t, withdrawalEcho(newOnly, 63))
	requireWithdrawalDrop(t, ctx, never, 64)
}
