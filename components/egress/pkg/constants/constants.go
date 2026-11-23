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

package constants

const (
	MarkValue = 0x1
	MarkHex   = "0x1"
)

// ForwarderMarkValue is the SO_MARK the forward-mode forwarder sets on its
// gateway sockets (OSEP-0030). It is distinct from the DNS proxy's 0x1 bypass
// and the fast-sandbox allow mark 0x2: only sockets carrying it may reach the
// gateway endpoint, and setting it requires CAP_NET_ADMIN or CAP_NET_RAW.
const (
	ForwarderMarkValue = 0x4
	ForwarderMarkHex   = "0x4"
)

// TelemetryMarkValue is the SO_MARK set on the egress sidecar's own OTLP
// collector sockets in forward mode (OSEP-0030) so trusted metrics bypass
// forward-mode capture and reach only the configured collector endpoint.
const (
	TelemetryMarkValue = 0x8
	TelemetryMarkHex   = "0x8"
)

const (
	EgressAuthTokenHeader = "OPENSANDBOX-EGRESS-AUTH"
)
