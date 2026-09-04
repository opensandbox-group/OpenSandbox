// Copyright 2026 Alibaba Group Holding Ltd.
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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	resourcehelper "k8s.io/component-helpers/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/fieldindex"
)

const (
	capacityMeterName            = "opensandbox/controller"
	poolPodsMetricName           = "controller.pool.pods"
	poolCPURequestsMetricName    = "controller.pool.cpu.requested"
	poolMemoryRequestsMetricName = "controller.pool.memory.requested"
	batchSandboxCountMetricName  = "controller.batchsandbox.count"
	batchSandboxPodsMetricName   = "controller.batchsandbox.pods"
	unknownBatchSandboxPhase     = "Unknown"
	allocationModePool           = "pool"
	allocationModeDirect         = "direct"
)

type poolAllocationReader interface {
	GetPoolAllocation(context.Context, *sandboxv1alpha1.Pool) (map[string]string, error)
}

type poolCapacity struct {
	namespace string
	name      string
	pods      map[string]int64
	cpu       map[string]float64
	memory    map[string]int64
}

type batchSandboxKey struct {
	namespace      string
	phase          string
	allocationMode string
}

type batchSandboxPodsKey struct {
	namespace      string
	state          string
	allocationMode string
}

type capacitySnapshot struct {
	pools              []poolCapacity
	batchSandboxCounts map[batchSandboxKey]int64
	batchSandboxPods   map[batchSandboxPodsKey]int64
}

func registerCapacityMetrics(meter metric.Meter, reader client.Reader, allocations poolAllocationReader) (metric.Registration, error) {
	poolPods, err := meter.Int64ObservableGauge(
		poolPodsMetricName,
		metric.WithDescription("Current Pool Pod count by allocation state"),
		metric.WithUnit("{pod}"),
	)
	if err != nil {
		return nil, err
	}
	poolCPU, err := meter.Float64ObservableGauge(
		poolCPURequestsMetricName,
		metric.WithDescription("Current CPU requests represented by Pool Pods"),
		metric.WithUnit("{cpu}"),
	)
	if err != nil {
		return nil, err
	}
	poolMemory, err := meter.Int64ObservableGauge(
		poolMemoryRequestsMetricName,
		metric.WithDescription("Current memory requests represented by Pool Pods"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}
	batchSandboxCount, err := meter.Int64ObservableGauge(
		batchSandboxCountMetricName,
		metric.WithDescription("Current BatchSandbox count by phase and allocation mode"),
		metric.WithUnit("{batchsandbox}"),
	)
	if err != nil {
		return nil, err
	}
	batchSandboxPods, err := meter.Int64ObservableGauge(
		batchSandboxPodsMetricName,
		metric.WithDescription("Current BatchSandbox Pod count by state and allocation mode"),
		metric.WithUnit("{pod}"),
	)
	if err != nil {
		return nil, err
	}

	return meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		snapshot, err := collectCapacitySnapshot(ctx, reader, allocations)
		if err != nil {
			return err
		}
		for _, pool := range snapshot.pools {
			for state, value := range pool.pods {
				attrs := metric.WithAttributes(poolAttributes(pool.namespace, pool.name, state)...)
				observer.ObserveInt64(poolPods, value, attrs)
			}
			for state, value := range pool.cpu {
				observer.ObserveFloat64(poolCPU, value, metric.WithAttributes(poolAttributes(pool.namespace, pool.name, state)...))
			}
			for state, value := range pool.memory {
				observer.ObserveInt64(poolMemory, value, metric.WithAttributes(poolAttributes(pool.namespace, pool.name, state)...))
			}
		}
		for key, value := range snapshot.batchSandboxCounts {
			observer.ObserveInt64(batchSandboxCount, value, metric.WithAttributes(
				attribute.String("namespace", key.namespace),
				attribute.String("phase", key.phase),
				attribute.String("allocation_mode", key.allocationMode),
			))
		}
		for key, value := range snapshot.batchSandboxPods {
			observer.ObserveInt64(batchSandboxPods, value, metric.WithAttributes(
				attribute.String("namespace", key.namespace),
				attribute.String("state", key.state),
				attribute.String("allocation_mode", key.allocationMode),
			))
		}
		return nil
	}, poolPods, poolCPU, poolMemory, batchSandboxCount, batchSandboxPods)
}

func collectCapacitySnapshot(ctx context.Context, reader client.Reader, allocations poolAllocationReader) (capacitySnapshot, error) {
	pools := &sandboxv1alpha1.PoolList{}
	if err := reader.List(ctx, pools); err != nil {
		return capacitySnapshot{}, err
	}
	batchSandboxes := &sandboxv1alpha1.BatchSandboxList{}
	if err := reader.List(ctx, batchSandboxes); err != nil {
		return capacitySnapshot{}, err
	}

	snapshot := capacitySnapshot{
		pools:              make([]poolCapacity, 0, len(pools.Items)),
		batchSandboxCounts: make(map[batchSandboxKey]int64),
		batchSandboxPods:   make(map[batchSandboxPodsKey]int64),
	}
	for i := range pools.Items {
		pool := &pools.Items[i]
		if !pool.DeletionTimestamp.IsZero() {
			continue
		}
		capacity, err := collectPoolCapacity(ctx, reader, allocations, pool)
		if err != nil {
			return capacitySnapshot{}, err
		}
		snapshot.pools = append(snapshot.pools, capacity)
	}
	for i := range batchSandboxes.Items {
		addBatchSandboxCapacity(&snapshot, &batchSandboxes.Items[i])
	}
	return snapshot, nil
}

func collectPoolCapacity(ctx context.Context, reader client.Reader, allocations poolAllocationReader, pool *sandboxv1alpha1.Pool) (poolCapacity, error) {
	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, &client.ListOptions{
		Namespace:     pool.Namespace,
		FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(pool.UID)}),
	}); err != nil {
		return poolCapacity{}, err
	}
	allocatedPods, err := allocations.GetPoolAllocation(ctx, pool)
	if err != nil {
		return poolCapacity{}, err
	}

	capacity := poolCapacity{
		namespace: pool.Namespace,
		name:      pool.Name,
		pods: map[string]int64{
			"total": int64(pool.Status.Total), "allocated": int64(pool.Status.Allocated),
			"available": int64(pool.Status.Available), "updated": int64(pool.Status.Updated),
		},
		cpu:    map[string]float64{"total": 0, "allocated": 0, "available": 0},
		memory: map[string]int64{"total": 0, "allocated": 0, "available": 0},
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		requests := resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{})
		cpu := float64(requests.Cpu().MilliValue()) / 1000
		memory := requests.Memory().Value()
		capacity.cpu["total"] += cpu
		capacity.memory["total"] += memory
		if _, allocated := allocatedPods[pod.Name]; allocated {
			capacity.cpu["allocated"] += cpu
			capacity.memory["allocated"] += memory
			continue
		}
		if utils.IsPodReady(pod) {
			capacity.cpu["available"] += cpu
			capacity.memory["available"] += memory
		}
	}
	return capacity, nil
}

func addBatchSandboxCapacity(snapshot *capacitySnapshot, sandbox *sandboxv1alpha1.BatchSandbox) {
	phase := string(sandbox.Status.Phase)
	if phase == "" {
		phase = unknownBatchSandboxPhase
	}
	allocationMode := allocationModeDirect
	if sandbox.Spec.PoolRef != "" {
		allocationMode = allocationModePool
	}
	snapshot.batchSandboxCounts[batchSandboxKey{
		namespace: sandbox.Namespace, phase: phase, allocationMode: allocationMode,
	}]++

	desired := int64(1)
	if sandbox.Spec.Replicas != nil {
		desired = int64(*sandbox.Spec.Replicas)
	}
	values := map[string]int64{
		"desired": desired, "current": int64(sandbox.Status.Replicas),
		"allocated": int64(sandbox.Status.Allocated), "ready": int64(sandbox.Status.Ready),
	}
	for state, value := range values {
		snapshot.batchSandboxPods[batchSandboxPodsKey{
			namespace: sandbox.Namespace, state: state, allocationMode: allocationMode,
		}] += value
	}
}

func poolAttributes(namespace, poolName, state string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("namespace", namespace),
		attribute.String("pool_name", poolName),
		attribute.String("state", state),
	}
}
