// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNftRemovedSingletonPortAtOptIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}
	startWithdrawalEchoPeer(t, ctx, "443", "8443")
	const target = "198.18.0.2"
	m := NewManagerWithOptions(Options{})
	allowAll := mustPolicy(t, `{"defaultAction":"allow"}`)
	require.NoError(t, m.ApplyStatic(ctx, allowAll))
	cut := openWithdrawalStream(t, "198.18.0.2", "443")
	sibling := openWithdrawalStream(t, "198.18.0.2", "8443")
	oldHost := `{"action":"allow","target":"` + target + `","ports":[443,8443]}`
	const deny = `{"action":"deny","target":"198.18.0.0/24","ports":[443]}`
	old := mustPolicy(t, `{"defaultAction":"allow","egress":[`+oldHost+`,`+deny+`]}`)
	require.NoError(t, m.ApplyStatic(ctx, old))
	require.NoError(t, withdrawalEcho(cut, 2), "plain tighten must not cut this established stream")
	_, err := m.ApplyStaticComputingEstablishedDrops(ctx, old, mustPolicy(t, `{"defaultAction":"allow","egress":[`+deny+`]}`))
	require.NoError(t, err)
	requireWithdrawalDrop(t, ctx, cut, 3)
	require.NoError(t, withdrawalEcho(sibling, 3), "default allow still admits 8443")

	whole := mustPolicy(t, `{"egress":[{"action":"allow","target":"`+target+`"}]}`)
	require.NoError(t, m.ApplyStatic(ctx, whole))
	atOptIn := openWithdrawalStream(t, "198.18.0.2", "443")
	atReload := openWithdrawalStream(t, "198.18.0.2", "8443")
	_, err = m.ApplyStaticComputingEstablishedDrops(ctx, whole, mustPolicy(t, `{"defaultAction":"allow","egress":[{"action":"deny","target":"198.18.0.2"}]}`))
	require.NoError(t, err)
	requireWithdrawalDrop(t, ctx, atOptIn, 4)
	require.NoError(t, m.ApplyStatic(ctx, mustPolicy(t, `{"defaultAction":"deny"}`)))
	requireWithdrawalDrop(t, ctx, atReload, 5)
}
