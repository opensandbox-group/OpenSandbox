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
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

type capacityAllocationReaderStub struct {
	allocations map[string]map[string]string
}

func (s capacityAllocationReaderStub) GetPoolAllocation(_ context.Context, pool *sandboxv1alpha1.Pool) (map[string]string, error) {
	return s.allocations[pool.Namespace+"/"+pool.Name], nil
}

func TestCapacityMetricsExposePoolAndBatchSandboxState(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, sandboxv1alpha1.AddToScheme(scheme))

	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "warm", Namespace: "tenant-a", UID: "warm-a"},
		Status: sandboxv1alpha1.PoolStatus{
			Total: 2, Allocated: 1, Available: 1, Updated: 2,
		},
	}
	allocated := poolPod("allocated", "warm", true, "500m", "512Mi")
	allocated.Spec.InitContainers = []corev1.Container{{
		Name: "init",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"),
		}},
	}}
	allocated.Spec.Overhead = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi"),
	}
	available := poolPod("available", "warm", true, "250m", "256Mi")
	setPoolOwner(pool, allocated, available)
	pooledSandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "pooled", Namespace: "tenant-a"},
		Spec:       sandboxv1alpha1.BatchSandboxSpec{Replicas: int32Ptr(1), PoolRef: "warm"},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			Phase: sandboxv1alpha1.BatchSandboxPhaseSucceed, Replicas: 1, Allocated: 1, Ready: 1,
		},
	}
	directSandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "direct", Namespace: "tenant-a"},
		Spec:       sandboxv1alpha1.BatchSandboxSpec{Replicas: int32Ptr(2)},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			Phase: sandboxv1alpha1.BatchSandboxPhasePending, Replicas: 1, Allocated: 1,
		},
	}
	client := capacityMetricsClient(scheme, pool, allocated, available, pooledSandbox, directSandbox)
	allocations := capacityAllocationReaderStub{allocations: map[string]map[string]string{
		"tenant-a/warm": {"allocated": "pooled"},
	}}

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	registration, err := registerCapacityMetrics(provider.Meter(capacityMeterName), client, allocations)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, registration.Unregister()) })

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	require.Equal(t, map[string]int64{
		"namespace=tenant-a,pool_name=warm,state=allocated": 1,
		"namespace=tenant-a,pool_name=warm,state=available": 1,
		"namespace=tenant-a,pool_name=warm,state=total":     2,
		"namespace=tenant-a,pool_name=warm,state=updated":   2,
	}, intGaugePoints(t, &rm, poolPodsMetricName))
	require.Equal(t, map[string]int64{
		"allocation_mode=direct,namespace=tenant-a,phase=Pending": 1,
		"allocation_mode=pool,namespace=tenant-a,phase=Succeed":   1,
	}, intGaugePoints(t, &rm, batchSandboxCountMetricName))
	require.Equal(t, map[string]int64{
		"allocation_mode=direct,namespace=tenant-a,state=allocated": 1,
		"allocation_mode=direct,namespace=tenant-a,state=current":   1,
		"allocation_mode=direct,namespace=tenant-a,state=desired":   2,
		"allocation_mode=direct,namespace=tenant-a,state=ready":     0,
		"allocation_mode=pool,namespace=tenant-a,state=allocated":   1,
		"allocation_mode=pool,namespace=tenant-a,state=current":     1,
		"allocation_mode=pool,namespace=tenant-a,state=desired":     1,
		"allocation_mode=pool,namespace=tenant-a,state=ready":       1,
	}, intGaugePoints(t, &rm, batchSandboxPodsMetricName))

	for _, name := range []string{poolPodsMetricName, batchSandboxCountMetricName, batchSandboxPodsMetricName} {
		for key := range intGaugePoints(t, &rm, name) {
			require.NotContains(t, key, "sandbox_id")
			require.NotContains(t, key, "pod_name")
			require.NotContains(t, key, "batchsandbox_name")
		}
	}
}

func TestCapacityMetricsUseSchedulerEquivalentPodRequests(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, sandboxv1alpha1.AddToScheme(scheme))

	pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "warm", Namespace: "tenant-a", UID: "warm-a"}}
	allocated := poolPod("allocated", "warm", true, "500m", "512Mi")
	allocated.Spec.InitContainers = []corev1.Container{{
		Name: "init",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"),
		}},
	}}
	allocated.Spec.Overhead = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi"),
	}
	available := poolPod("available", "warm", true, "250m", "256Mi")
	setPoolOwner(pool, allocated, available)
	client := capacityMetricsClient(scheme, pool, allocated, available)
	allocations := capacityAllocationReaderStub{allocations: map[string]map[string]string{
		"tenant-a/warm": {"allocated": "pooled"},
	}}

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	registration, err := registerCapacityMetrics(provider.Meter(capacityMeterName), client, allocations)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, registration.Unregister()) })

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	require.Equal(t, map[string]float64{
		"namespace=tenant-a,pool_name=warm,state=allocated": 1.1,
		"namespace=tenant-a,pool_name=warm,state=available": 0.25,
		"namespace=tenant-a,pool_name=warm,state=total":     1.35,
	}, floatGaugePoints(t, &rm, poolCPURequestsMetricName))
	require.Equal(t, map[string]int64{
		"namespace=tenant-a,pool_name=warm,state=allocated": 1207959552,
		"namespace=tenant-a,pool_name=warm,state=available": 268435456,
		"namespace=tenant-a,pool_name=warm,state=total":     1476395008,
	}, intGaugePoints(t, &rm, poolMemoryRequestsMetricName))
}

func TestCapacityMetricsRunnerRequiresLeaderElection(t *testing.T) {
	require.True(t, (&capacityMetricsRunner{}).NeedLeaderElection())
}

func poolPod(name, pool string, ready bool, cpu, memory string) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-a", Labels: map[string]string{LabelPoolName: pool}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "sandbox",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory),
			}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: status,
		}}},
	}
}

func int32Ptr(value int32) *int32 {
	return &value
}

func intGaugePoints(t *testing.T, rm *metricdata.ResourceMetrics, name string) map[string]int64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			gauge, ok := m.Data.(metricdata.Gauge[int64])
			require.True(t, ok, "unexpected aggregation %T for %s", m.Data, name)
			points := map[string]int64{}
			for _, point := range gauge.DataPoints {
				points[attributeKey(point.Attributes)] = point.Value
			}
			return points
		}
	}
	t.Fatalf("metric %s not collected", name)
	return nil
}

func floatGaugePoints(t *testing.T, rm *metricdata.ResourceMetrics, name string) map[string]float64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			gauge, ok := m.Data.(metricdata.Gauge[float64])
			require.True(t, ok, "unexpected aggregation %T for %s", m.Data, name)
			points := map[string]float64{}
			for _, point := range gauge.DataPoints {
				points[attributeKey(point.Attributes)] = point.Value
			}
			return points
		}
	}
	t.Fatalf("metric %s not collected", name)
	return nil
}

func attributeKey(set attribute.Set) string {
	iter := set.Iter()
	key := ""
	for iter.Next() {
		if key != "" {
			key += ","
		}
		item := iter.Attribute()
		key += string(item.Key) + "=" + item.Value.AsString()
	}
	return key
}
