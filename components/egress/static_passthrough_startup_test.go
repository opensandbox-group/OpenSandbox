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
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/mitmpassthrough"
	"github.com/stretchr/testify/require"
)

func TestStaticPassthroughStartupRejectsBeforeRecovery(t *testing.T) {
	for _, item := range []struct{ name, live, raw, upstream string }{
		{"nonlive-empty", "", "[]", ""},
		{"invalid-live", "true", "null", ""},
		{"chained-live", "true", `["private.example.com"]`, "http://proxy.example:3128"},
	} {
		t.Run(item.name, func(t *testing.T) {
			t.Setenv(constants.EnvExperimentalRevisionRuntime, item.live)
			t.Setenv(constants.EnvMitmproxyPassthroughHosts, item.raw)
			t.Setenv(constants.EnvUpstreamProxy, item.upstream)
			// Nil collaborators would panic if startup reached policy recovery or serving.
			srv, handler, err := startPolicyServer(nil, nil, "dns+nft", "127.0.0.1:0", "", nil, "", nil, nil, nil)
			require.ErrorIs(t, err, mitmpassthrough.ErrInvalid)
			if item.name == "nonlive-empty" {
				require.Contains(t, err.Error(), constants.EnvExperimentalRevisionRuntime)
			}
			if item.name == "chained-live" {
				require.Contains(t, err.Error(), constants.EnvUpstreamProxy)
			}
			require.NotContains(t, err.Error(), "proxy.example")
			require.Nil(t, srv)
			require.Nil(t, handler)
			require.Contains(t, err.Error(), constants.EnvMitmproxyPassthroughHosts)
			require.NotContains(t, err.Error(), "private.example.com")
		})
	}
}
