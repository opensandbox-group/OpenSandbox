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

import (
	"flag"
	"time"
)

// deprecatedNamespace is kept so old command lines with --namespace still parse.
// Ingress watches sandbox resources across all namespaces.
var deprecatedNamespace string

func InitFlags() {
	// Core server options.
	flag.StringVar(&LogLevel, "log-level", "info", "Server log level")
	flag.IntVar(&Port, "port", 28888, "Server listening port")
	flag.StringVar(&deprecatedNamespace, "namespace", "opensandbox", "Deprecated compatibility flag (ingress now watches sandbox resources across all namespaces)")
	flag.StringVar(&ProviderType, "provider-type", "batchsandbox", "The sandbox provider type (batchsandbox, agent-sandbox, fast-sandbox)")
	flag.StringVar(&Mode, "mode", "header", "The sandbox service discovery mode")

	// Renew-intent publishing.
	flag.BoolVar(&RenewIntentEnabled, "renew-intent-enabled", false, "Enable publishing renew-intent events to Redis")
	flag.StringVar(&RenewIntentRedisDSN, "renew-intent-redis-dsn", "redis://127.0.0.1:6379/0", "Redis DSN for renew-intent queue")
	flag.StringVar(&RenewIntentQueueKey, "renew-intent-queue-key", "opensandbox:renew:intent", "Redis List key for renew-intent payloads")
	flag.IntVar(&RenewIntentQueueMaxLen, "renew-intent-queue-max-len", 0, "Max renew-intent queue length (0 = no cap)")
	flag.IntVar(&RenewIntentMinIntervalSec, "renew-intent-min-interval", 60, "Min seconds between publishing intents for the same sandbox (client-side throttle)")

	// Activity tracking and wake-on-access (OSEP-0024).
	flag.BoolVar(&ActivityEnabled, "activity-enabled", false, "Enable recording per-sandbox activity observations in Redis")
	flag.StringVar(&ActivityRedisDSN, "activity-redis-dsn", "redis://127.0.0.1:6379/0", "Redis DSN for activity keys")
	flag.DurationVar(&ActivityMinInterval, "activity-min-interval", 5*time.Second, "Min interval between activity writes for the same sandbox (per-replica write coalescing; N replicas produce at most N writes per sandbox per interval)")
	flag.IntVar(&ActivityTTLSeconds, "activity-ttl-seconds", 1800, "TTL for activity keys; must match the server's activity_ttl_seconds and stay >= the largest accepted idle threshold")
	flag.BoolVar(&WakeEnabled, "wake-enabled", false, "Enable wake-on-access: park requests routed to paused fast sandboxes while they resume")
	flag.DurationVar(&WakeParkBudget, "wake-park-budget", 5*time.Second, "Park budget Y per wake flight; must exceed the FastPath wait timeout plus one GetSandbox RPC")
	flag.IntVar(&WakeParkMax, "wake-park-max", 1024, "Parking lot capacity: max concurrently parked requests (and resume flights) per replica")
	flag.DurationVar(&WakeRetryInterval, "wake-retry-interval", 50*time.Millisecond, "First readiness-poll interval of a wake flight")
	flag.Float64Var(&WakeRetryFactor, "wake-retry-factor", 1.3, "Exponential growth factor of the wake poll interval")
	flag.Float64Var(&WakeRetryJitter, "wake-retry-jitter", 0.1, "Relative jitter applied to the wake poll interval")

	// Secure access (signed routes) and FastPath (Fast Sandbox) routing.
	flag.StringVar(&SecureAccessKeys, "secure-access-keys", "", "Verification keys for signed ingress routes and Fast Sandbox route scopes: a=base64,b=base64 (comma-separated; key_id is 1 char [0-9a-z])")
	flag.StringVar(&FastPathEndpoint, "fastpath-endpoint", "", "FastPath v2 gRPC endpoint; a non-empty value enables Fast Sandbox routing")
	flag.StringVar(&FastPathAccessMode, "fastpath-access-mode", "direct-fastlet-proxy", "FastPath Fast Sandbox data-plane mode: central-proxy or direct-fastlet-proxy")
	flag.IntVar(&FastPathWaitTimeoutMillis, "fastpath-wait-timeout-millis", 2000, "FastPath ResolveEndpoint RPC timeout for one ingress request")

	// Network-readiness shadow assessment.
	flag.DurationVar(&NetworkReadinessShadowWindow, "network-readiness-shadow-window", time.Minute, "Shadow connectivity assessment window")
	flag.IntVar(&NetworkReadinessShadowMaxTargets, "network-readiness-shadow-max-targets", 1024, "Maximum distinct upstream targets retained per shadow window")
	flag.Uint64Var(&NetworkReadinessShadowMinAttempts, "network-readiness-shadow-min-attempts", 20, "Minimum connection attempts required for a shadow assessment")
	flag.IntVar(&NetworkReadinessShadowMinTargets, "network-readiness-shadow-min-targets", 5, "Minimum distinct upstream targets required for a shadow assessment")
	flag.IntVar(&NetworkReadinessShadowMinSignalTargets, "network-readiness-shadow-min-signal-targets", 2, "Minimum distinct upstream targets with timeout or unreachable results required for a degraded shadow assessment")
	flag.Float64Var(&NetworkReadinessShadowDegradedFailureRatio, "network-readiness-shadow-failure-ratio", 0.2, "Timeout or unreachable ratio reported as degraded in shadow mode")

	flag.Parse()
}
