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
	"fmt"
	"os"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
)

// upstreamProxySpecForProfile validates the chained upstream proxy env at
// startup, before any profile dispatch. A configured proxy without
// transparent mitmproxy is a hard error rather than silently ignored. The
// sidecar profile additionally requires the dns+nft enforcement mode; the
// fast-sandbox profile always enforces through its source-IP-keyed nft
// table and never reads the mode env, so only transparent mitmproxy is
// required there.
func upstreamProxySpecForProfile(profile string) (*mitmproxy.UpstreamProxySpec, error) {
	spec, err := mitmproxy.UpstreamProxyFromEnv()
	if err != nil {
		return nil, err
	}
	if spec == nil {
		return nil, nil
	}
	if !constants.IsTruthy(os.Getenv(constants.EnvMitmproxyTransparent)) {
		return nil, fmt.Errorf("%s requires %s=true", constants.EnvUpstreamProxy, constants.EnvMitmproxyTransparent)
	}
	if profile == constants.ProfileFastSandbox {
		return spec, nil
	}
	mode, err := constants.ParseEgressMode(os.Getenv(constants.EnvEgressMode))
	if err != nil || mode != constants.PolicyDnsNft {
		return nil, fmt.Errorf("%s requires %s=%s", constants.EnvUpstreamProxy, constants.EnvEgressMode, constants.PolicyDnsNft)
	}
	return spec, nil
}
