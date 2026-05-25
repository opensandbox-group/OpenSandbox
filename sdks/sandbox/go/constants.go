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

package opensandbox

import "time"

const (
	// DefaultExecdPort is the standard port for the execd service inside a sandbox.
	DefaultExecdPort = 44772

	// DefaultEgressPort is the standard port for the egress sidecar inside a sandbox.
	DefaultEgressPort = 18080

	// DefaultTimeoutSeconds is the default sandbox TTL in seconds.
	DefaultTimeoutSeconds = 600

	// DefaultReadyTimeoutSeconds is the default timeout for WaitUntilReady.
	DefaultReadyTimeoutSeconds = 30

	// DefaultHealthCheckPollingInterval is the default polling interval for WaitUntilReady.
	DefaultHealthCheckPollingInterval = 200 * time.Millisecond

	// DefaultRequestTimeout is the default HTTP request timeout.
	DefaultRequestTimeout = 30 * time.Second

	// DefaultCodeInterpreterTimeoutSeconds is the default TTL for code interpreter sandboxes.
	DefaultCodeInterpreterTimeoutSeconds = 900

	// Version is the SDK version reported in the User-Agent header.
	Version = "1.0.1"

	// APIVersion is the lifecycle API version prefix.
	APIVersion = "v1"

	// DefaultDomain is the default OpenSandbox server address.
	DefaultDomain = "localhost:8080"

	// DefaultProtocol is the default protocol for connecting to the server.
	DefaultProtocol = "http"
)

// DefaultEntrypoint keeps the sandbox alive for interactive use.
var DefaultEntrypoint = []string{"tail", "-f", "/dev/null"}

// DefaultResourceLimits provides sensible defaults for sandbox resource limits.
var DefaultResourceLimits = ResourceLimits{
	"cpu":    "1",
	"memory": "2Gi",
}
