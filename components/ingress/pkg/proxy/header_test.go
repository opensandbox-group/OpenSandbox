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

package proxy

import (
	"net/http"
	"testing"
)

func TestHeaderNamesAreCanonical(t *testing.T) {
	for _, header := range []string{
		SandboxIngress, DeprecatedSandboxIngress, AccessRenew, SkipActivity,
		xRealIP, xForwardedFor, xForwardedProto, reverseProxyServerPowerBy,
		secWebSocketProtocol, secWebSocketKey, secWebSocketVersion, secWebSocketExtensions,
		setCookie, hopByHopConnection, hopByHopKeepAlive, hopByHopProxyAuth, hopByHopProxyAuthz,
		hopByHopTE, hopByHopTrailer, hopByHopTransferEncoding, hopByHopUpgrade, hopByHopProxyConnection,
	} {
		if canonical := http.CanonicalHeaderKey(header); header != canonical {
			t.Errorf("header %q is not canonical: want %q", header, canonical)
		}
	}
}
