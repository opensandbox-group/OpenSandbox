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
