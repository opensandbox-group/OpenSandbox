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

package policy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAllowsProxyDestination(t *testing.T) {
	for _, tc := range []struct {
		name, raw, host string
		port            int
		want            bool
	}{
		{"default deny", `{}`, "code.invalid", 443, false},
		{"default allow", `{"defaultAction":"allow"}`, "code.invalid", 443, true},
		{"domain allow", `{"egress":[{"action":"allow","target":"*.invalid"}]}`, "code.invalid", 443, true},
		{"domain deny", `{"defaultAction":"allow","egress":[{"action":"deny","target":"code.invalid"}]}`, "code.invalid", 443, false},
		{"domain first match", `{"egress":[{"action":"allow","target":"code.invalid"},{"action":"deny","target":"*.invalid"}]}`, "code.invalid", 443, true},
		{"port deny beats domain allow", `{"egress":[{"action":"allow","target":"code.invalid"},{"action":"deny","ports":[443]}]}`, "code.invalid", 443, false},
		{"port allow cannot bypass domain deny", `{"egress":[{"action":"allow","ports":[443]}]}`, "code.invalid", 443, false},
		{"ip allow", `{"egress":[{"action":"allow","target":"192.0.2.1"}]}`, "192.0.2.1", 443, true},
		{"cidr deny beats ip allow", `{"egress":[{"action":"allow","target":"192.0.2.1"},{"action":"deny","target":"192.0.2.0/24"}]}`, "192.0.2.1", 443, false},
		{"ipv6 deny", `{"defaultAction":"allow","egress":[{"action":"deny","target":"2001:db8::/32"}]}`, "2001:db8::1", 443, false},
		{"mapped ipv4 deny", `{"defaultAction":"allow","egress":[{"action":"deny","target":"192.0.2.0/24"}]}`, "::ffff:192.0.2.1", 443, false},
		{"mapped prefix deny", `{"defaultAction":"allow","egress":[{"action":"deny","target":"::ffff:192.0.2.0/120"}]}`, "192.0.2.1", 443, false},
		{"ip port scope", `{"egress":[{"action":"allow","target":"192.0.2.0/24","ports":[443]}]}`, "192.0.2.1", 80, false},
		{"unknown resolved ip fails closed", `{"defaultAction":"allow","egress":[{"action":"deny","target":"10.0.0.0/8"}]}`, "code.invalid", 443, false},
		{"irrelevant ip port deny", `{"defaultAction":"allow","egress":[{"action":"deny","target":"10.0.0.0/8","ports":[22]}]}`, "code.invalid", 443, true},
		{"ip allow cannot authorize domain", `{"egress":[{"action":"allow","target":"192.0.2.0/24"}]}`, "code.invalid", 443, false},
		{"invalid port", `{"defaultAction":"allow"}`, "code.invalid", 65536, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ParsePolicy(tc.raw)
			require.NoError(t, err)
			require.Equal(t, tc.want, p.AllowsProxyDestination(tc.host, tc.port))
		})
	}
}

func TestProxyDestinationAlwaysOverlayAndPortFloor(t *testing.T) {
	p, err := ParsePolicy(`{"defaultAction":"allow"}`)
	require.NoError(t, err)
	deny, err := ParseValidatedEgressRule(ActionDeny, "blocked.invalid")
	require.NoError(t, err)
	p = MergeAlwaysOverlay(p, []EgressRule{deny, NewPortFloorDenyRule([]PortRange{{Lo: 444, Hi: 65535}})}, nil)
	require.False(t, p.AllowsProxyDestination("blocked.invalid", 443))
	require.False(t, p.AllowsProxyDestination("code.invalid", 8443))
	require.True(t, p.AllowsProxyDestination("code.invalid", 443))
}
