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

package opensandbox

import "time"

const (
	DefaultExecdPort = 44772

	DefaultEgressPort = 18080

	DefaultTimeoutSeconds = 600

	DefaultReadyTimeoutSeconds = 30

	DefaultHealthCheckPollingInterval = 200 * time.Millisecond

	DefaultRequestTimeout = 30 * time.Second

	DefaultCodeInterpreterTimeoutSeconds = 900

	// Version is the SDK version reported in the User-Agent header.
	Version = "1.2.1"

	// APIVersion is the lifecycle API version prefix.
	APIVersion = "v1"

	DefaultDomain = "localhost:8080"

	DefaultProtocol = "http"
)

// DefaultEntrypoint keeps the sandbox alive for interactive use.
var DefaultEntrypoint = []string{"tail", "-f", "/dev/null"}

var DefaultResourceLimits = ResourceLimits{
	"cpu":    "1",
	"memory": "2Gi",
}
