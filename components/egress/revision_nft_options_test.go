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

package main

import (
	"context"
	"net/netip"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func TestNftOptionsRevisionQuiescence(t *testing.T) {
	for _, value := range []string{"true", "false", ""} {
		t.Run("enabled="+value, func(t *testing.T) {
			t.Setenv(constants.EnvExperimentalRevisionRuntime, value)
			t.Setenv(constants.EnvDoHBlocklist, "invalid-prefix")
			opts, err := parseNftOptions(nil)
			require.NoError(t, err)
			require.Equal(t, value == "true", opts.QuiesceOnApplyFailure)
			mgr, err := createNftManager("dns+nft", nil)
			require.NoError(t, err)
			require.NotNil(t, mgr)
			// Test the exact production constructor result before setupNft or
			// the recovery owner can initialize. A canceled context cannot start
			// an nft command, so this needs neither nft nor kernel privileges.
			m := mgr.(*nftables.Manager)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			require.Equal(t, nftables.ApplyUnchanged, nftables.ApplyEffectOf(m.ApplyStatic(ctx, policy.DefaultDenyPolicy())))
			// Pre-start cancellation never freezes a healthy Manager.
			require.NoError(t, m.AddResolvedIPs(context.Background(), nil))
			m.Quiesce()
			require.ErrorIs(t, m.AddResolvedIPs(context.Background(), []nftables.ResolvedIP{
				{Addr: netip.MustParseAddr("192.0.2.1")},
			}), nftables.ErrQuiesced)
			require.ErrorIs(t, m.ApplyStatic(context.Background(), policy.DefaultDenyPolicy()), nftables.ErrQuiesced)
			dnsOnly, err := createNftManager("dns", nil)
			require.NoError(t, err)
			require.Nil(t, dnsOnly)
		})
	}
}

// The exemption port must follow the transparent-MITM env, and only when enabled.
func TestNftOptionsMitmRedirectPortFollowsTransparentMode(t *testing.T) {
	opts, err := parseNftOptions(nil)
	require.NoError(t, err)
	require.Zero(t, opts.MitmRedirectPort, "no exemption when transparent MITM is disabled")

	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	opts, err = parseNftOptions(nil)
	require.NoError(t, err)
	require.Equal(t, constants.DefaultMitmproxyPort, opts.MitmRedirectPort)

	t.Setenv(constants.EnvMitmproxyPort, "18443")
	opts, err = parseNftOptions(nil)
	require.NoError(t, err)
	require.Equal(t, 18443, opts.MitmRedirectPort)
}
