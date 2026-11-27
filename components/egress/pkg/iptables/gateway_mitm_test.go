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

	require.Contains(t, script, "delete table inet "+gatewayMitmNftTable)
	require.Contains(t, script, "add table inet "+gatewayMitmNftTable)
	require.Contains(t, script, "add set inet "+gatewayMitmNftTable+" bypass_v4 { type ipv4_addr . ipv4_addr; }")
	require.Contains(t, script, "flush set inet "+gatewayMitmNftTable+" bypass_v4")
	require.Contains(t, script, "flush set inet "+gatewayMitmNftTable+" bypass_v6")
	require.Contains(t, script, "add map inet "+gatewayMitmNftTable+" dnat_v4 { type ipv4_addr : ipv4_addr . inet_service; }")
	require.Contains(t, script, "flush map inet "+gatewayMitmNftTable+" dnat_v4")
	require.Contains(t, script, "flush map inet "+gatewayMitmNftTable+" dnat_v6")
	require.Contains(t, script, "add chain inet "+gatewayMitmNftTable+" gw { type nat hook prerouting priority dstnat; }")
	require.Contains(t, script, "add rule inet "+gatewayMitmNftTable+" gw tcp dport {80,443} ip saddr . ip daddr @bypass_v4 return")
	require.Contains(t, script, "add rule inet "+gatewayMitmNftTable+" gw tcp dport {80,443} dnat ip to ip saddr map @dnat_v4")
	require.Contains(t, script, "add element inet "+gatewayMitmNftTable+" bypass_v4 { 10.0.0.5 . 10.0.0.1 }")
	require.Contains(t, script, "add element inet "+gatewayMitmNftTable+" dnat_v4 { 10.0.0.5 : 10.0.0.1 . 18081 }")
	assert.Less(t, strings.Index(script, "flush set inet "+gatewayMitmNftTable+" bypass_v4"), strings.Index(script, "add element inet "+gatewayMitmNftTable+" bypass_v4"), "flush must precede add element")
	assert.Less(t, strings.Index(script, "flush map inet "+gatewayMitmNftTable+" dnat_v4"), strings.Index(script, "add element inet "+gatewayMitmNftTable+" dnat_v4"), "flush must precede add element")
}

func TestMitmRedirectScriptV6(t *testing.T) {
	script := mitmRedirectScript([]MitmRedirectEntry{
		{SandboxIP: netip.MustParseAddr("fd00::5"), Gateway: netip.MustParseAddr("fd00::1")},
	}, 18081, []int{80, 443})

	require.Contains(t, script, "add rule inet "+gatewayMitmNftTable+" gw tcp dport {80,443} ip6 saddr . ip6 daddr @bypass_v6 return")
	require.Contains(t, script, "add rule inet "+gatewayMitmNftTable+" gw tcp dport {80,443} dnat ip6 to ip6 saddr map @dnat_v6")
	require.Contains(t, script, "add element inet "+gatewayMitmNftTable+" bypass_v6 { fd00::5 . fd00::1 }")
	require.Contains(t, script, "add element inet "+gatewayMitmNftTable+" dnat_v6 { fd00::5 : fd00::1 . 18081 }")
}

func TestMitmRedirectScriptExtraPortsAndEmpty(t *testing.T) {
	script := mitmRedirectScript([]MitmRedirectEntry{
		{SandboxIP: netip.MustParseAddr("10.0.0.5"), Gateway: netip.MustParseAddr("10.0.0.1")},
	}, 18081, []int{80, 443, 8080})
	require.Contains(t, script, `tcp dport {80,443,8080} dnat ip to ip saddr map @dnat_v4`)
	require.Contains(t, script, `add element inet `+gatewayMitmNftTable+` dnat_v4 { 10.0.0.5 : 10.0.0.1 . 18081 }`)

	empty := mitmRedirectScript(nil, 18081, []int{80, 443})
	require.Contains(t, empty, "add chain inet "+gatewayMitmNftTable+" gw")
	require.NotContains(t, empty, "add element")
	assert.True(t, strings.HasSuffix(empty, "\n"))
}

func TestMitmIncrementalFragments(t *testing.T) {
	entry := MitmRedirectEntry{
		SandboxIP: netip.MustParseAddr("10.0.0.5"),
		Gateway:   netip.MustParseAddr("10.0.0.1"),
	}
	addFrag, err := MitmAddElementFragment(entry, 18081)
	require.NoError(t, err)
	assert.Contains(t, addFrag, "add element inet "+gatewayMitmNftTable+" bypass_v4 { 10.0.0.5 . 10.0.0.1 }")
	assert.Contains(t, addFrag, "add element inet "+gatewayMitmNftTable+" dnat_v4 { 10.0.0.5 : 10.0.0.1 . 18081 }")

	delFrag, err := MitmDeleteElementFragment(entry)
	require.NoError(t, err)
	assert.Contains(t, delFrag, "delete element inet "+gatewayMitmNftTable+" bypass_v4 { 10.0.0.5 . 10.0.0.1 }")
	assert.Contains(t, delFrag, "delete element inet "+gatewayMitmNftTable+" dnat_v4 { 10.0.0.5 }")
}

func TestMitmDportList(t *testing.T) {
	assert.Equal(t, "{80,443}", mitmDportList([]int{80, 443}))
	assert.Equal(t, "{80,443,8080}", mitmDportList([]int{80, 443, 8080}))
	assert.Equal(t, "80,443", mitmDportList(nil), "empty list falls back to the base intercept set")
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
