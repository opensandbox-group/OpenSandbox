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

var (
	// Standard forwarding headers.
	XRealIP         = http.CanonicalHeaderKey("X-Real-IP")
	XForwardedFor   = http.CanonicalHeaderKey("X-Forwarded-For")
	XForwardedProto = http.CanonicalHeaderKey("X-Forwarded-Proto")

	// OpenSandbox routing headers.
	SandboxIngress = http.CanonicalHeaderKey("OpenSandbox-Ingress-To")

	// DeprecatedSandboxIngress is the pre-rename routing header.
	//
	// Deprecated: use SandboxIngress instead.
	DeprecatedSandboxIngress = http.CanonicalHeaderKey("OPEN-SANDBOX-INGRESS")

	// AccessRenew is the per-request opt-out header for OSEP-0009
	// auto-renew-on-access. The exact sentinel value AccessRenewSkipValue
	// ("skip") suppresses publishing a renew intent for that one request;
	// unknown values are ignored for forward compatibility. Like
	// SandboxIngress, it is stripped before forwarding upstream so backend
	// applications never observe it.
	AccessRenew          = http.CanonicalHeaderKey("OpenSandbox-Access-Renew")
	AccessRenewSkipValue = "skip"

	ReverseProxyServerPowerBy = http.CanonicalHeaderKey("Reverse-Proxy-Server-PowerBy")

	// WebSocket handshake headers passed through to the upgrader.
	SecWebSocketProtocol   = http.CanonicalHeaderKey("Sec-WebSocket-Protocol")
	SecWebSocketKey        = http.CanonicalHeaderKey("Sec-WebSocket-Key")
	SecWebSocketVersion    = http.CanonicalHeaderKey("Sec-WebSocket-Version")
	SecWebSocketExtensions = http.CanonicalHeaderKey("Sec-WebSocket-Extensions")
	SetCookie              = http.CanonicalHeaderKey("Set-Cookie")
	Host                   = http.CanonicalHeaderKey("Host")

	// Hop-by-hop headers per RFC 7230 §6.1 — must not be forwarded by proxies.
	HopByHopConnection       = http.CanonicalHeaderKey("Connection")
	HopByHopKeepAlive        = http.CanonicalHeaderKey("Keep-Alive")
	HopByHopProxyAuth        = http.CanonicalHeaderKey("Proxy-Authenticate")
	HopByHopProxyAuthz       = http.CanonicalHeaderKey("Proxy-Authorization")
	HopByHopTE               = http.CanonicalHeaderKey("TE")
	HopByHopTrailer          = http.CanonicalHeaderKey("Trailer")
	HopByHopTransferEncoding = http.CanonicalHeaderKey("Transfer-Encoding")
	HopByHopUpgrade          = http.CanonicalHeaderKey("Upgrade")
	HopByHopProxyConnection  = http.CanonicalHeaderKey("Proxy-Connection")
)

// IsAccessRenewSkip reports whether the request opts out of access renew
// intents for this single request (OSEP-0009 per-request opt-out).
func IsAccessRenewSkip(header http.Header) bool {
	return header.Get(AccessRenew) == AccessRenewSkipValue
}
