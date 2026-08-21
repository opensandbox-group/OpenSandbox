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

	dnsQueryDur     metric.Float64Histogram
	dnsQueryFailed  metric.Int64Counter
	policyDenied    metric.Int64Counter
	nftUpdates      metric.Int64Counter
	nftUpdateFailed metric.Int64Counter

	lastNftRuleCount atomic.Int64
)

// Bounded reason values for RecordDNSQueryFailed. A closed set keeps the counter's
// cardinality fixed: error strings and queried names must never reach an attribute.
const (
	DNSFailureNoUpstreams   = "no_upstreams"
	DNSFailureUpstreamError = "upstream_error"
	DNSFailureEmptyResponse = "empty_response"
	DNSFailureRcode         = "rcode"
)

// Bounded operation values for RecordNftablesUpdateFailed.
const (
	NftOpStaticApply = "static_apply"
	NftOpDynamicAdd  = "dynamic_add"
	NftOpRemove      = "remove"
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

// egressMetricOptWith adds one attribute to the shared set. It copies rather than
// appending to the slice returned by egressSharedAttrs: that slice is shared by every
// caller and may have spare capacity, so append would write into the backing array and
// let one call's attribute leak into another's.
func egressMetricOptWith(kv attribute.KeyValue) metric.MeasurementOption {
	shared := egressSharedAttrs()
	attrs := make([]attribute.KeyValue, 0, len(shared)+1)
	attrs = append(attrs, shared...)
	attrs = append(attrs, kv)
	return metric.WithAttributes(attrs...)
}

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
		// Explicit boundaries: the instrument records seconds, but the SDK default
		// boundaries are the spec's millisecond ladder, which would collapse every
		// realistic latency into one bucket. The head covers a cache hit up to one
		// upstream timeout (5s); the tail must reach past a serial retry chain of
		// timeout x len(upstreams) — including late successes — hence 600s. See
		// docs/opentelemetry.md for the full rationale.
		metric.WithExplicitBucketBoundaries(
			0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
			15, 30, 60, 120, 300, 600,
		),
	)
	if err != nil {
		return err
	}
	dnsQueryFailed, err = meter.Int64Counter(
		"egress.dns.query.failed_total",
		metric.WithDescription("DNS queries the proxy could not resolve, by reason. "+
			"Distinct from egress.policy.denied_total, which counts deliberate policy denials."),
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
	nftUpdateFailed, err = meter.Int64Counter(
		"egress.nftables.updates.failed_total",
		metric.WithDescription("nft updates that failed, by operation. A failed dynamic_add "+
			"means an allowed destination is unreachable while the policy says otherwise."),
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

// ForceFlush exports pending metrics immediately. Callers that are about to terminate the
// process must use it: metrics leave through a periodic reader, and the deferred shutdown in
// main does not run past os.Exit.
func ForceFlush(ctx context.Context) error {
	return inttelemetry.ForceFlush(ctx)
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

// RecordDNSQueryFailed counts a lookup the proxy could not answer. reason must be one of
// the DNSFailure* constants.
func RecordDNSQueryFailed(reason string) {
	if dnsQueryFailed == nil {
		return
	}
	dnsQueryFailed.Add(context.Background(), 1, egressMetricOptWith(attribute.String("reason", reason)))
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

// RecordNftablesUpdateFailed counts an update that did not reach the kernel. operation
// must be one of the NftOp* constants.
func RecordNftablesUpdateFailed(operation string) {
	if nftUpdateFailed == nil {
		return
	}
	nftUpdateFailed.Add(context.Background(), 1, egressMetricOptWith(attribute.String("operation", operation)))
}
