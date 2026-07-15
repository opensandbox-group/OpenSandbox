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

package dnsproxy

import (
	"net/netip"
	"sync"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/stretchr/testify/require"
)

func resetNameserverExemptCache(t *testing.T) {
	t.Helper()
	exemptAddrs = nil
	exemptSet = nil
	exemptListOnce = sync.Once{}
}

func TestParseNameserverExemptList_IPOnly(t *testing.T) {
	t.Setenv(constants.EnvNameserverExempt, "1.1.1.1, 2001:db8::1 ,invalid, 10.0.0.0/8, ,")
	resetNameserverExemptCache(t)

	got := ParseNameserverExemptList()
	want := []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("2001:db8::1")}
	require.Equal(t, want, got, "ParseNameserverExemptList() mismatch")

	// Cached result should stay the same on subsequent calls.
	require.Equal(t, want, ParseNameserverExemptList(), "cached ParseNameserverExemptList() mismatch")
}

func TestUpstreamInExemptList_IPOnly(t *testing.T) {
	t.Setenv(constants.EnvNameserverExempt, "1.1.1.1,2001:db8::1")
	resetNameserverExemptCache(t)

	require.True(t, UpstreamInExemptList("1.1.1.1"), "expected IPv4 upstream to be exempt")
	require.True(t, UpstreamInExemptList("2001:db8::1"), "expected IPv6 upstream to be exempt")
	require.False(t, UpstreamInExemptList("10.0.0.2"), "unexpected exempt match for non-listed IP")
	require.False(t, UpstreamInExemptList("not-an-ip"), "invalid IP string should not match")
}

func TestUpstreamInExemptList_CIDRIgnored(t *testing.T) {
	t.Setenv(constants.EnvNameserverExempt, "10.0.0.0/24")
	resetNameserverExemptCache(t)

	require.Empty(t, ParseNameserverExemptList(), "CIDR should be ignored in exempt list")
	require.False(t, UpstreamInExemptList("10.0.0.5"), "CIDR should not make upstream exempt")
}
