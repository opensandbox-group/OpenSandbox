// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"

	"go.opentelemetry.io/otel"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type capacityMetricsRunner struct {
	reader      client.Reader
	allocations poolAllocationReader
}

func SetupCapacityMetricsWithManager(mgr manager.Manager, allocations Allocator) error {
	return mgr.Add(&capacityMetricsRunner{reader: mgr.GetCache(), allocations: allocations})
}

func (r *capacityMetricsRunner) NeedLeaderElection() bool {
	return true
}

// Start registers the capacity gauges on the global meter provider and
// unregisters them when the manager stops. With no OTLP endpoint configured
// the no-op provider keeps them inert.
func (r *capacityMetricsRunner) Start(ctx context.Context) error {
	registration, err := registerCapacityMetrics(otel.GetMeterProvider().Meter(capacityMeterName), r.reader, r.allocations)
	if err != nil {
		logf.FromContext(ctx).Error(err, "Capacity metrics disabled after registration failure")
		<-ctx.Done()
		return nil
	}
	<-ctx.Done()
	if err := registration.Unregister(); err != nil {
		logf.FromContext(ctx).Error(err, "Unable to unregister capacity metrics")
	}
	return nil
}
