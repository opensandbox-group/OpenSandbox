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

package main

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/droptelemetry"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
	slogger "github.com/alibaba/opensandbox/internal/logger"
)

type dropAuditCapture struct {
	slogger.Logger
	fields map[string]any
	events int
}

func (l *dropAuditCapture) With(fields ...slogger.Field) slogger.Logger {
	for _, f := range fields {
		l.fields[f.Key] = f.Value
	}
	return l
}
func (l *dropAuditCapture) Infof(string, ...any) { l.events++ }
func TestDropAuditSidecar(t *testing.T) {
	prior := log.Logger
	capture := &dropAuditCapture{Logger: prior, fields: map[string]any{}}
	log.Logger = capture
	t.Cleanup(func() { log.Logger = prior })
	t.Setenv(constants.EnvSandboxID, "process-id")
	p := droptelemetry.Packet{SrcIP: netip.MustParseAddr("10.0.0.5"), DstIP: netip.MustParseAddr("192.0.2.1"), DstPort: 443, Proto: "tcp", Reason: droptelemetry.DenySet}
	auditBlockedPacket(p)
	require.Equal(t, 1, capture.events)
	require.Equal(t, "egress.blocked_ip", capture.fields["opensandbox.event"])
	require.Equal(t, "192.0.2.1", capture.fields["dst_ip"])
	require.Equal(t, "process-id", capture.fields["sandbox_id"])
	require.NotContains(t, capture.fields, "subject_id")
}

func TestDropReceiveErrorGauge(t *testing.T) {
	export := captureTransitions(t)
	t.Cleanup(func() { telemetry.SetDropSampler(false, "disabled") })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := droptelemetry.DefaultConfig()
	cfg.Mode = droptelemetry.Sample
	cfg.Poll = time.Hour
	rt := droptelemetry.New(cfg, "unused")
	var receiveError func(error)
	stop := rt.Start(ctx, func(context.Context, string) ([]byte, error) { return nil, nil }, func(_ context.Context, _ droptelemetry.Config, _ func(droptelemetry.Packet), failure func(error)) (func(), error) {
		receiveError = failure
		return func() {}, nil
	}, func(droptelemetry.Packet) {})
	defer stop()
	require.NotNil(t, receiveError)
	receiveError(errors.New("injected receive failure"))
	flushCtx, flushCancel := context.WithTimeout(ctx, 5*time.Second)
	defer flushCancel()
	require.NoError(t, telemetry.ForceFlush(flushCtx))
	var body []byte
	select {
	case body = <-export.received:
	case <-flushCtx.Done():
		t.Fatal("sampler gauge was not exported")
	}
	var request collectormetrics.ExportMetricsServiceRequest
	require.NoError(t, proto.Unmarshal(body, &request))
	found := false
	for _, resource := range request.ResourceMetrics {
		for _, scope := range resource.ScopeMetrics {
			for _, m := range scope.Metrics {
				if m.Name != "egress.drop_telemetry.sampler_up" {
					continue
				}
				points := m.GetGauge().DataPoints
				require.Len(t, points, 1)
				require.EqualValues(t, 0, points[0].GetAsInt())
				require.Len(t, points[0].Attributes, 1)
				require.Equal(t, "cause", points[0].Attributes[0].Key)
				require.Equal(t, "receive", points[0].Attributes[0].Value.GetStringValue())
				found = true
			}
		}
	}
	require.True(t, found)
}

func TestStartDropTelemetry(t *testing.T) {
	t.Setenv(droptelemetry.EnvMode, "off")
	t.Setenv(droptelemetry.EnvRate, "invalid")
	rt, stop, err := startDropTelemetry(context.Background(), "unused", nil)
	require.NoError(t, err)
	require.Nil(t, rt)
	stop()
	t.Setenv(droptelemetry.EnvMode, "invalid")
	_, _, err = startDropTelemetry(context.Background(), "unused", nil)
	require.Error(t, err)
	t.Setenv(droptelemetry.EnvMode, "sample")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bound, audited, closed := false, false, false
	packet := droptelemetry.Packet{Reason: droptelemetry.DenySet}
	rt, stop, err = startDropTelemetryWith(ctx, "unused", func(context.Context, string) ([]byte, error) { return nil, nil }, func(_ context.Context, _ droptelemetry.Config, emit func(droptelemetry.Packet), _ func(error)) (func(), error) {
		bound = true
		emit(packet)
		return func() { closed = true }, nil
	}, func(got droptelemetry.Packet) { audited = true; require.Equal(t, packet, got) })
	require.NoError(t, err)
	require.NotNil(t, rt)
	require.True(t, bound)
	require.True(t, audited)
	stop()
	require.True(t, closed)
}
