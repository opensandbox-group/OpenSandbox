// Copyright 2025 The OpenSandbox Authors
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

import "net/http"

const (
	SandboxIngress = "Opensandbox-Ingress-To"
	// Deprecated: use SandboxIngress instead.
	DeprecatedSandboxIngress = "Open-Sandbox-Ingress"

	// Only the exact sentinel suppresses renewal; control headers are stripped upstream.
	AccessRenew          = "Opensandbox-Access-Renew"
	AccessRenewSkipValue = "skip"

	// Idle probes skip activity so they do not reset the clock they measure.
	SkipActivity          = "Opensandbox-Skip-Activity"
	SkipActivitySkipValue = "skip"
)

const (
	xRealIP                   = "X-Real-Ip"
	xForwardedFor             = "X-Forwarded-For"
	xForwardedProto           = "X-Forwarded-Proto"
	reverseProxyServerPowerBy = "Reverse-Proxy-Server-Powerby"
	secWebSocketProtocol      = "Sec-Websocket-Protocol"
	secWebSocketKey           = "Sec-Websocket-Key"
	secWebSocketVersion       = "Sec-Websocket-Version"
	secWebSocketExtensions    = "Sec-Websocket-Extensions"
	setCookie                 = "Set-Cookie"
	hopByHopConnection        = "Connection"
	hopByHopKeepAlive         = "Keep-Alive"
	hopByHopProxyAuth         = "Proxy-Authenticate"
	hopByHopProxyAuthz        = "Proxy-Authorization"
	hopByHopTE                = "Te"
	hopByHopTrailer           = "Trailer"
	hopByHopTransferEncoding  = "Transfer-Encoding"
	hopByHopUpgrade           = "Upgrade"
	hopByHopProxyConnection   = "Proxy-Connection"
)

func IsAccessRenewSkip(header http.Header) bool {
	return header.Get(AccessRenew) == AccessRenewSkipValue
}

func IsSkipActivity(header http.Header) bool {
	return header.Get(SkipActivity) == SkipActivitySkipValue
}
