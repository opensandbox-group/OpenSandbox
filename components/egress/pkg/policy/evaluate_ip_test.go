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
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEvaluateIP_DenyBeatsAllow(t *testing.T) {
	p, err := ParsePolicy(`{"defaultAction":"allow","egress":[{"action":"allow","target":"203.0.113.0/24"},{"action":"deny","target":"203.0.113.10"}]}`)
	require.NoError(t, err)
	require.Equal(t, ActionDeny, p.EvaluateIP(netip.MustParseAddr("203.0.113.10")))
	require.Equal(t, ActionAllow, p.EvaluateIP(netip.MustParseAddr("203.0.113.11")))
	require.Equal(t, ActionAllow, p.EvaluateIP(netip.MustParseAddr("198.51.100.1")))
}

func TestEvaluateIP_DefaultDenyNeedsAllow(t *testing.T) {
	p, err := ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"1.1.1.1"}]}`)
	require.NoError(t, err)
	require.Equal(t, ActionAllow, p.EvaluateIP(netip.MustParseAddr("1.1.1.1")))
	require.Equal(t, ActionDeny, p.EvaluateIP(netip.MustParseAddr("8.8.8.8")))
}

func TestEvaluateIP_IgnoresPortScopedRules(t *testing.T) {
	p, err := ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"1.1.1.1","ports":[443]}]}`)
	require.NoError(t, err)
	require.Equal(t, ActionDeny, p.EvaluateIP(netip.MustParseAddr("1.1.1.1")))
}
