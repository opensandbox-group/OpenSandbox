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
	"slices"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var blockedPackets, blockedSamples metric.Int64Counter
var dropSamplerMu sync.RWMutex
var dropSamplerValue int64
var dropSamplerCause = "disabled"

// RegisterDropMetrics enables drop instruments after the meter provider is initialized.
func RegisterDropMetrics() error {
	meter := otel.Meter("opensandbox/egress")
	var err error
	blockedPackets, err = meter.Int64Counter("egress.blocked.packets", metric.WithDescription("Kernel-enforced dropped packets"))
	if err != nil {
		return err
	}
	blockedSamples, err = meter.Int64Counter("egress.blocked.samples", metric.WithDescription("Sampled blocked IP packets"))
	if err != nil {
		return err
	}
	_, err = meter.Int64ObservableGauge("egress.drop_telemetry.sampler_up", metric.WithDescription("Whether kernel drop sampling is available"), metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
		dropSamplerMu.RLock()
		defer dropSamplerMu.RUnlock()
		o.Observe(dropSamplerValue, egressMetricOptWith(attribute.String("cause", dropSamplerCause)))
		return nil
	}))
	return err
}
func RecordBlockedPackets(reason string, n uint64) {
	if blockedPackets == nil {
		return
	}
	opts := egressMetricOptWith(attribute.String("reason", normalizeDropReason(reason)))
	for n > math.MaxInt64 {
		blockedPackets.Add(context.Background(), math.MaxInt64, opts)
		n -= math.MaxInt64
	}
	blockedPackets.Add(context.Background(), int64(n), opts)
}
func RecordBlockedSample(reason string) {
	if blockedSamples != nil {
		blockedSamples.Add(context.Background(), 1, egressMetricOptWith(attribute.String("reason", normalizeDropReason(reason))))
	}
}
func SetDropSampler(up bool, cause string) {
	dropSamplerMu.Lock()
	defer dropSamplerMu.Unlock()
	dropSamplerValue = 0
	if up {
		dropSamplerValue = 1
	}
	dropSamplerCause = normalizeDropCause(cause)
}

var dropReasons = []string{
	"deny_set", "port_deny", "dot", "doh", "default_deny", "established_revoke",
	"subject_input_deny", "unmarked_failclosed", "gateway_mitm_direct", "upstream_proxy_deny",
}

// DropReasons returns a copy of the accepted packet-drop metric labels.
func DropReasons() []string {
	return slices.Clone(dropReasons)
}

func normalizeDropReason(reason string) string {
	if slices.Contains(dropReasons, reason) {
		return reason
	}
	return "unknown"
}

func normalizeDropCause(cause string) string {
	switch cause {
	case "bind", "ruleset", "receive", "disabled", "":
		return cause
	default:
		return "unknown"
	}
}
