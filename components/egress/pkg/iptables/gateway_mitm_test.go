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

package iptables

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMitmRedirectScriptV4(t *testing.T) {
	script := mitmRedirectScript([]MitmRedirectEntry{
		{SandboxIP: netip.MustParseAddr("10.0.0.5"), Gateway: netip.MustParseAddr("10.0.0.1")},
	}, 18081, []int{80, 443})

	require.Contains(t, script, "delete table inet "+GatewayMitmNftTable)
	require.Contains(t, script, "add table inet "+GatewayMitmNftTable)
	require.Contains(t, script, "add chain inet "+GatewayMitmNftTable+" gw { type nat hook prerouting priority dstnat; }")

	// management-plane exception: sandbox -> gateway must NOT be intercepted
	require.Contains(t, script, `ip saddr 10.0.0.5 ip daddr 10.0.0.1 tcp dport {80,443} return`)
	// DNAT targets the gateway address, not loopback.
	require.Contains(t, script, `ip saddr 10.0.0.5 tcp dport {80,443} dnat to 10.0.0.1:18081`)
}

func TestMitmRedirectScriptV6(t *testing.T) {
	script := mitmRedirectScript([]MitmRedirectEntry{
		{SandboxIP: netip.MustParseAddr("fd00::5"), Gateway: netip.MustParseAddr("fd00::1")},
	}, 18081, []int{80, 443})

	require.Contains(t, script, `ip6 saddr fd00::5 ip6 daddr fd00::1 tcp dport {80,443} return`)
	require.Contains(t, script, `ip6 saddr fd00::5 tcp dport {80,443} dnat to fd00::1:18081`)
}

// TestMitmRedirectScriptDeterministicEntryOrder: multiple subjects arrive
// through a Go map, and the rebuild's exact textual state is what the anchor
// ledger verifies. The rendered rules must be independent of iteration order,
// or a semantically identical rebuild would be reported as drift.
func TestMitmRedirectScriptDeterministicEntryOrder(t *testing.T) {
	entries := []MitmRedirectEntry{
		{SandboxIP: netip.MustParseAddr("fd00::5"), Gateway: netip.MustParseAddr("fd00::1")},
		{SandboxIP: netip.MustParseAddr("10.0.0.10"), Gateway: netip.MustParseAddr("10.0.0.1")},
		{SandboxIP: netip.MustParseAddr("10.0.0.5"), Gateway: netip.MustParseAddr("10.0.0.1")},
		{SandboxIP: netip.MustParseAddr("10.0.0.9"), Gateway: netip.MustParseAddr("10.0.0.1")},
	}
	want := mitmRedirectScript(entries, 18081, []int{80, 443})
	for shift := 1; shift < len(entries); shift++ {
		rotated := append(append([]MitmRedirectEntry(nil), entries[shift:]...), entries[:shift]...)
		require.Equal(t, want, mitmRedirectScript(rotated, 18081, []int{80, 443}))
	}

	// Rules are ordered by unmapped sandbox address: numeric IPv4 first, then
	// IPv6 (netip.Addr.Compare orders IPv4 before IPv6).
	ruleIndex := func(addr string) int { return strings.Index(want, "saddr "+addr+" tcp") }
	require.NotEqual(t, -1, ruleIndex("10.0.0.5"))
	require.Less(t, ruleIndex("10.0.0.5"), ruleIndex("10.0.0.9"))
	require.Less(t, ruleIndex("10.0.0.9"), ruleIndex("10.0.0.10"))
	require.Less(t, ruleIndex("10.0.0.10"), ruleIndex("fd00::5"))
}

func TestMitmRedirectScriptExtraPortsAndEmpty(t *testing.T) {
	script := mitmRedirectScript([]MitmRedirectEntry{
		{SandboxIP: netip.MustParseAddr("10.0.0.5"), Gateway: netip.MustParseAddr("10.0.0.1")},
	}, 18081, []int{80, 443, 8080})
	require.Contains(t, script, `tcp dport {80,443,8080} dnat to 10.0.0.1:18081`)

	empty := mitmRedirectScript(nil, 18081, []int{80, 443})
	require.Contains(t, empty, "add chain inet "+GatewayMitmNftTable+" gw")
	require.NotContains(t, empty, "dnat to")
	assert.True(t, strings.HasSuffix(empty, "\n"))
}

func TestMitmDportList(t *testing.T) {
	assert.Equal(t, "{80,443}", mitmDportList([]int{80, 443}))
	assert.Equal(t, "{80,443,8080}", mitmDportList([]int{80, 443, 8080}))
	assert.Equal(t, "80,443", mitmDportList(nil), "empty list falls back to the base intercept set")
}

// TestInstallMitmRedirectsPropagatesContext: the reconcile restore path runs
// the installer under a step deadline, so the caller's context must reach the
// nft invocation. Dropping it would let one hung nft wedge the whole serial
// reconcile pass (and the shutdown that waits on it).
func TestInstallMitmRedirectsPropagatesContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var seen []context.Context
	r := redirectRunner{
		runNft: func(runCtx context.Context, _ string) ([]byte, error) {
			seen = append(seen, runCtx)
			return nil, runCtx.Err()
		},
	}
	err := installMitmRedirects(ctx, r, []MitmRedirectEntry{
		{SandboxIP: netip.MustParseAddr("10.0.0.5"), Gateway: netip.MustParseAddr("10.0.0.1")},
	}, 18081, []int{80, 443})
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, seen, 1)
	require.ErrorIs(t, seen[0].Err(), context.Canceled, "the installer must pass the caller's context to nft")
}

func TestMitmMissingTableDetection(t *testing.T) {
	// the real nft failure shape: exec.ExitError says only "exit status 1";
	// the message lives in the command output
	parse := func(msg string) error { return parseMitmError("exit status 1", msg) }
	assert.True(t, isMitmMissingTableError([]byte("Error: No such file or directory\n\tdelete table inet opensandbox_gateway_mitm"), parse("exit status 1")))
	assert.True(t, isMitmMissingTableError([]byte("Error: No such file or directory; did you mean table 'inet opensandbox_gateway_mitm' in family inet?"), parse("exit status 1")))
	assert.True(t, isMitmMissingTableError([]byte("table 'inet opensandbox_gateway_mitm' does not exist"), parse("exit status 1")))
	assert.False(t, isMitmMissingTableError([]byte("Error: syntax error"), parse("exit status 1")))
	assert.False(t, isMitmMissingTableError([]byte("Error: No such file or directory\n\tdelete table inet other_table"), parse("exit status 1")))
	assert.False(t, isMitmMissingTableError(nil, nil))
}

func TestRemoveMitmDeleteTableLine(t *testing.T) {
	script := "delete table inet opensandbox_gateway_mitm\nadd table inet opensandbox_gateway_mitm\nadd chain inet opensandbox_gateway_mitm gw { type nat hook prerouting priority dstnat; }\n"
	out := removeMitmDeleteTableLine(script)
	assert.NotContains(t, out, "delete table")
	assert.Contains(t, out, "add table")
	assert.Contains(t, out, "add chain")
}

func parseMitmError(prefix, msg string) error {
	return &mitmScriptError{prefix: prefix, msg: msg}
}

type mitmScriptError struct{ prefix, msg string }

func (e *mitmScriptError) Error() string { return e.prefix + ": " + e.msg }
