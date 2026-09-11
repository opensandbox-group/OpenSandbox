// Copyright 2025 Alibaba Group Holding Ltd.
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

package flag

import "time"

// Core server options.
var (
	// LogLevel controls the router log verbosity.
	LogLevel string

	// Port controls the HTTP listener port.
	Port int

	// ProviderType specifies the sandbox provider type (batchsandbox, agent-sandbox, fast-sandbox).
	ProviderType string

	// Mode specifies the sandbox service discovery mode (header or uri).
	Mode string
)

// Renew-intent publishing: ingress records observed traffic so the lifecycle
// server can extend soon-to-expire sandboxes.
var (
	RenewIntentEnabled        bool
	RenewIntentRedisDSN       string
	RenewIntentQueueKey       string
	RenewIntentQueueMaxLen    int
	RenewIntentMinIntervalSec int
)

// Secure access (signed routes) and FastPath (Fast Sandbox) routing.
var (
	// SecureAccessKeys holds the shared verification keys for signed ingress
	// routes and Fast Sandbox route scopes: "a=base64,b=base64".
	SecureAccessKeys string

	// FastPathEndpoint is the FastPath v2 gRPC endpoint; non-empty enables Fast Sandbox routing.
	FastPathEndpoint string

	// FastPathAccessMode selects the Fast Sandbox data-plane mode (central-proxy or direct-fastlet-proxy).
	FastPathAccessMode string

	// FastPathWaitTimeoutMillis bounds one FastPath ResolveEndpoint RPC per ingress request.
	FastPathWaitTimeoutMillis int
)

// Network-readiness shadow assessment: connectivity observations are aggregated
// per fixed window and only reported (never used to remove traffic).
var (
	NetworkReadinessShadowWindow               time.Duration
	NetworkReadinessShadowMaxTargets           int
	NetworkReadinessShadowMinAttempts          uint64
	NetworkReadinessShadowMinTargets           int
	NetworkReadinessShadowMinSignalTargets     int
	NetworkReadinessShadowDegradedFailureRatio float64
)
