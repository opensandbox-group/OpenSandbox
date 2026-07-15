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
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	slogger "github.com/alibaba/opensandbox/internal/logger"
	inttelemetry "github.com/alibaba/opensandbox/internal/telemetry"
)

var (
	meter metric.Meter

	dnsQueryDur  metric.Float64Histogram
	policyDenied metric.Int64Counter
	nftUpdates   metric.Int64Counter

	lastNftRuleCount atomic.Int64
)

var egressSharedAttrs = sync.OnceValue(func() []attribute.KeyValue {
	return inttelemetry.SharedAttrsFromEnv(inttelemetry.SharedAttrsEnvConfig{
		SandboxIDEnv:  constants.EnvSandboxID,
		ExtraAttrsEnv: constants.EnvEgressMetricsExtraAttrs,
		SandboxAttr:   "sandbox_id",
	})
})

var egressMetricOpt = sync.OnceValue(func() metric.MeasurementOption {
	return metric.WithAttributes(egressSharedAttrs()...)
})

func EgressLogFields() []slogger.Field {
	kvs := egressSharedAttrs()
	out := make([]slogger.Field, 0, len(kvs))
	for _, kv := range kvs {
		var v string
		if kv.Value.Type() == attribute.STRING {
			v = kv.Value.AsString()
		} else {
			v = kv.Value.Emit()
		}
		out = append(out, slogger.Field{Key: string(kv.Key), Value: v})
	}
	return out
}

func registerEgressMetrics() error {
	meter = otel.Meter("opensandbox/egress")

	var err error
	dnsQueryDur, err = meter.Float64Histogram(
		"egress.dns.query.duration",
		metric.WithDescription("DNS forward latency"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return err
	}
	policyDenied, err = meter.Int64Counter(
		"egress.policy.denied_total",
		metric.WithDescription("DNS policy denials"),
	)
	if err != nil {
		return err
	}
	nftUpdates, err = meter.Int64Counter(
		"egress.nftables.updates.count",
		metric.WithDescription("nft static apply and dynamic IP adds"),
	)
	if err != nil {
		return err
	}

	_, err = meter.Int64ObservableGauge(
		"egress.nftables.rules.count",
		metric.WithDescription("Approximate policy size after last static apply"),
		metric.WithUnit("{element}"),
		metric.WithInt64Callback(func(ctx context.Context, obs metric.Int64Observer) error {
			obs.Observe(lastNftRuleCount.Load(), egressMetricOpt())
			return nil
		}),
	)
	if err != nil {
		return err
	}

	_, err = meter.Int64ObservableGauge(
		"egress.system.memory.usage_bytes",
		metric.WithDescription("System RAM used bytes from gopsutil on Linux (non-Linux build: 0)."),
		metric.WithUnit("By"),
		metric.WithInt64Callback(func(ctx context.Context, obs metric.Int64Observer) error {
			obs.Observe(systemMemoryUsedBytes(), egressMetricOpt())
			return nil
		}),
	)
	if err != nil {
		return err
	}

	_, err = meter.Float64ObservableGauge(
		"egress.system.cpu.utilization",
		metric.WithDescription("CPU busy ratio 0-1 from gopsutil on Linux (non-Linux build: 0)."),
		metric.WithUnit("1"),
		metric.WithFloat64Callback(func(ctx context.Context, obs metric.Float64Observer) error {
			obs.Observe(cpuUtilizationRatio(), egressMetricOpt())
			return nil
		}),
	)
	return err
}

func NftRuleCountFromPolicy(p *policy.NetworkPolicy) int64 {
	if p == nil {
		p = policy.DefaultDenyPolicy()
	}
	a4, a6, d4, d6 := p.StaticIPSets()
	return int64(len(p.Egress) + len(a4) + len(a6) + len(d4) + len(d6))
}

func RecordDNSForward(seconds float64) {
	if dnsQueryDur == nil {
		return
	}
	opt := egressMetricOpt()
	dnsQueryDur.Record(context.Background(), seconds, opt)
}

func RecordDNSDenied() {
	if policyDenied == nil {
		return
	}
	policyDenied.Add(context.Background(), 1, egressMetricOpt())
}

func SetNftablesRuleCount(n int64) {
	lastNftRuleCount.Store(n)
}

func RecordNftablesUpdate() {
	if nftUpdates == nil {
		return
	}
	nftUpdates.Add(context.Background(), 1, egressMetricOpt())
}
