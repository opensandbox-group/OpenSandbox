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

package telemetry

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/attribute"

	inttelemetry "github.com/alibaba/opensandbox/internal/telemetry"
)

func TestNormalizeRoute(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "unknown", normalizeRoute(""))
	assert.Equal(t, "/code/contexts/:contextId", normalizeRoute("/code/contexts/:contextId"))
}

func TestRecordHTTPRequestWithoutInit(t *testing.T) {
	t.Parallel()

	RecordHTTPRequest(context.Background(), "GET", "/ping", 200, 0.01)
}

func TestSystemMetricsReaders(t *testing.T) {
	t.Parallel()

	assert.GreaterOrEqual(t, systemProcessCount(), int64(0))
	assert.GreaterOrEqual(t, systemCPUUsagePercent(), 0.0)
	assert.GreaterOrEqual(t, systemMemoryUsageBytes(), int64(0))
	inBytes, outBytes := systemNetworkIOBytes()
	assert.GreaterOrEqual(t, inBytes, int64(0))
	assert.GreaterOrEqual(t, outBytes, int64(0))
	tcpCount, udpCount := systemNetworkConnectionCounts()
	assert.GreaterOrEqual(t, tcpCount, int64(0))
	assert.GreaterOrEqual(t, udpCount, int64(0))
}

func TestExecdSharedAttrs(t *testing.T) {
	t.Setenv(envSandboxID, "sb-123")
	t.Setenv(envMetricsExtraAttr, "tenant=t1,env=dev")

	orig := execdSharedAttrs
	execdSharedAttrs = sync.OnceValue(func() []attribute.KeyValue {
		return inttelemetry.SharedAttrsFromEnv(inttelemetry.SharedAttrsEnvConfig{
			SandboxIDEnv:  envSandboxID,
			ExtraAttrsEnv: envMetricsExtraAttr,
			SandboxAttr:   "sandbox_id",
		})
	})
	t.Cleanup(func() { execdSharedAttrs = orig })

	attrs := execdSharedAttrs()
	assert.Len(t, attrs, 3)
}
