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
)

var (
	deprecatedNamespace string
)

func InitFlags() {
	flag.StringVar(&LogLevel, "log-level", "info", "Server log level")
	flag.IntVar(&Port, "port", 28888, "Server listening port (default: 28888)")
	flag.StringVar(&deprecatedNamespace, "namespace", "opensandbox", "Deprecated compatibility flag (ingress now watches sandbox resources across all namespaces)")
	flag.StringVar(&ProviderType, "provider-type", "batchsandbox", "The sandbox provider type (default: batchsandbox)")
	flag.StringVar(&Mode, "mode", "header", "The sandbox service discovery mode (default: header)")

	flag.BoolVar(&RenewIntentEnabled, "renew-intent-enabled", false, "Enable publishing renew-intent events to Redis (OSEP-0009)")
	flag.StringVar(&RenewIntentRedisDSN, "renew-intent-redis-dsn", "redis://127.0.0.1:6379/0", "Redis DSN for renew-intent queue")
	flag.StringVar(&RenewIntentQueueKey, "renew-intent-queue-key", "opensandbox:renew:intent", "Redis List key for renew-intent payloads")
	flag.IntVar(&RenewIntentQueueMaxLen, "renew-intent-queue-max-len", 0, "Max renew-intent queue length (0 = no cap)")
	flag.IntVar(&RenewIntentMinIntervalSec, "renew-intent-min-interval", 60, "Min seconds between publishing intents for the same sandbox (client-side throttle)")

	flag.StringVar(&SecureAccessKeys, "secure-access-keys", "", "OSEP-0011 verification keys: a=base64,b=base64 (comma-separated; key_id is 1 char [0-9a-z])")

	flag.Parse()
}
