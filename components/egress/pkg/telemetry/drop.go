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

package telemetry

import (
	"context"
	"math"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var blockedPackets, blockedSamples metric.Int64Counter
var dropSamplerMu sync.RWMutex
var dropSamplerValue int64
var dropSamplerCause = "disabled"

func registerDropMetrics() error {
	var err error
	blockedPackets, err = meter.Int64Counter("egress.blocked.packets")
	if err != nil {
		return err
	}
	blockedSamples, err = meter.Int64Counter("egress.blocked.samples")
	if err != nil {
		return err
	}
	_, err = meter.Int64ObservableGauge("egress.drop_telemetry.sampler_up", metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
		dropSamplerMu.RLock()
		defer dropSamplerMu.RUnlock()
		o.Observe(dropSamplerValue, metric.WithAttributes(attribute.String("cause", dropSamplerCause)))
		return nil
	}))
	return err
}
func RecordBlockedPackets(reason string, n uint64) {
	if blockedPackets == nil {
		return
	}
	opts := metric.WithAttributes(attribute.String("reason", reason))
	for n > math.MaxInt64 {
		blockedPackets.Add(context.Background(), math.MaxInt64, opts)
		n -= math.MaxInt64
	}
	blockedPackets.Add(context.Background(), int64(n), opts)
}
func RecordBlockedSample(reason string) {
	if blockedSamples != nil {
		blockedSamples.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", reason)))
	}
}
func SetDropSampler(up bool, cause string) {
	dropSamplerMu.Lock()
	defer dropSamplerMu.Unlock()
	dropSamplerValue = 0
	if up {
		dropSamplerValue = 1
	}
	dropSamplerCause = cause
}
