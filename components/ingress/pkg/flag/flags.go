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

// Activity tracking and wake-on-access (OSEP-0024 auto-pause): the ingress
// records per-sandbox last-traffic observations in Redis, and requests that
// hit a paused fast sandbox park while a resume flight restores it.
var (
	// ActivityEnabled turns on per-request activity recording.
	ActivityEnabled bool
	// ActivityRedisDSN is the Redis DSN for activity keys.
	ActivityRedisDSN string
	// ActivityMinInterval coalesces writes per sandbox.
	ActivityMinInterval time.Duration
	// ActivityTTLSeconds bounds how long an observation stays fresh; the
	// server's idle threshold must never exceed it, and both sides must
	// agree on this value.
	ActivityTTLSeconds int

	// WakeEnabled turns on pause detection, resume flights, and parking.
	WakeEnabled bool
	// WakeParkBudget is the per-flight park budget Y.
	WakeParkBudget time.Duration
	// WakeParkMax is the parking lot capacity per replica.
	WakeParkMax int
	// WakeRetryInterval is the first readiness-poll interval.
	WakeRetryInterval time.Duration
	// WakeRetryFactor grows the poll interval exponentially.
	WakeRetryFactor float64
	// WakeRetryJitter randomizes the poll interval relatively.
	WakeRetryJitter float64
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
