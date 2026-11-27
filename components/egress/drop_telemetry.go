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
	"os"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/droptelemetry"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
	slogger "github.com/alibaba/opensandbox/internal/logger"
)

func startDropTelemetry(ctx context.Context, table string, run droptelemetry.Runner) (*droptelemetry.Runtime, func(), error) {
	return startDropTelemetryWith(ctx, table, run, droptelemetry.Bind, auditBlockedPacket)
}

func startDropTelemetryWith(ctx context.Context, table string, run droptelemetry.Runner, bind droptelemetry.Binder, audit func(droptelemetry.Packet)) (*droptelemetry.Runtime, func(), error) {
	cfg, err := droptelemetry.Parse(os.Getenv)
	if err != nil {
		return nil, func() {}, err
	}
	if cfg.Mode == droptelemetry.Off {
		return nil, func() {}, nil
	}
	rt := droptelemetry.New(cfg, table)
	return rt, rt.Start(ctx, run, bind, audit), nil
}
func auditBlockedPacket(p droptelemetry.Packet) {
	sandboxID := os.Getenv(constants.EnvSandboxID)
	fields := []slogger.Field{
		{Key: "opensandbox.event", Value: "egress.blocked_ip"},
		{Key: "src_ip", Value: p.SrcIP.String()},
		{Key: "dst_ip", Value: p.DstIP.String()},
		{Key: "dst_port", Value: p.DstPort},
		{Key: "proto", Value: p.Proto},
		{Key: "reason", Value: string(p.Reason)},
	}
	fields = append(fields, slogger.Field{Key: "sandbox_id", Value: sandboxID})
	telemetry.RecordBlockedSample(string(p.Reason))
	log.Logger.With(fields...).Infof("egress blocked IP")
}
