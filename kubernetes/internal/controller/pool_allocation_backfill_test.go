// Copyright 2025 Alibaba Group Holding Ltd.
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

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/algorithm"
)

func TestBackfillLegacyPoolAllocation(t *testing.T) {
	ctx := context.Background()
	pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "pool-pod",
		Namespace: "default",
		Labels:    map[string]string{LabelPoolName: pool.Name},
	}}

	t.Run("success stamps exact record and is idempotent", func(t *testing.T) {
		sandbox := newLegacyAllocationSandbox("sandbox", pool.Name)
		r := newBackfillTestReconciler(t, sandbox)

		if err := r.backfillLegacyPoolAllocation(ctx, pool, sandbox, []*corev1.Pod{pod}, map[string]string{"pool-pod": sandbox.Name}); err != nil {
			t.Fatalf("backfillLegacyPoolAllocation() error = %v", err)
		}
		updated := getBackfillSandbox(t, ctx, r, sandbox)
		allocation := parseBackfillAllocation(t, updated)
		want := SandboxAllocation{Pods: []string{"pool-pod"}, PoolRef: pool.Name, Generation: sandbox.Generation}
		if !reflect.DeepEqual(allocation, want) {
			t.Fatalf("allocation = %#v, want %#v", allocation, want)
		}
		firstAnnotations := updated.GetAnnotations()
		firstResourceVersion := updated.ResourceVersion

		if err := r.backfillLegacyPoolAllocation(ctx, pool, updated, []*corev1.Pod{pod}, map[string]string{"pool-pod": sandbox.Name}); err != nil {
			t.Fatalf("second backfillLegacyPoolAllocation() error = %v", err)
		}
		again := getBackfillSandbox(t, ctx, r, sandbox)
		if !reflect.DeepEqual(again.GetAnnotations(), firstAnnotations) {
			t.Fatalf("second backfill changed annotations: got %#v, want %#v", again.GetAnnotations(), firstAnnotations)
		}
		if again.ResourceVersion != firstResourceVersion {
			t.Fatalf("second backfill changed resource version: got %q, want %q", again.ResourceVersion, firstResourceVersion)
		}
	})

	tests := []struct {
		name             string
		mutate           func(*sandboxv1alpha1.BatchSandbox)
		pods             []*corev1.Pod
		latestAllocation map[string]string
	}{
		{
			name: "missing pool pod",
			pods: nil,
		},
		{
			name: "release intersects allocation",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocReleaseKey] = `{"pods":["pool-pod"]}`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "malformed release",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocReleaseKey] = `{`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "release missing pods",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocReleaseKey] = `{}`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "release null pods",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocReleaseKey] = `{"pods":null}`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "release duplicate pod",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocReleaseKey] = `{"pods":["released-pod","released-pod"]}`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "malformed released",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocReleasedKey] = `{`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "released missing pods",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocReleasedKey] = `{}`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "released invalid pod",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocReleasedKey] = `{"pods":["INVALID_POD"]}`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "deleting sandbox",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				now := metav1.NewTime(time.Now())
				sandbox.DeletionTimestamp = &now
				sandbox.Finalizers = append(sandbox.Finalizers, "keep-deleting-object")
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "missing allocation finalizer",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Finalizers = nil
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "nonempty mismatched pool ref",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocStatusKey] = `{"pods":["pool-pod"],"poolRef":"other-pool","generation":1}`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "explicit empty pool ref is not legacy",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocStatusKey] = `{"pods":["pool-pod"],"poolRef":""}`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "explicit generation is not legacy",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocStatusKey] = `{"pods":["pool-pod"],"generation":0}`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "extra allocation field is not legacy",
			mutate: func(sandbox *sandboxv1alpha1.BatchSandbox) {
				sandbox.Annotations[AnnoAllocStatusKey] = `{"pods":["pool-pod"],"unexpected":"value"}`
			},
			pods: []*corev1.Pod{pod},
		},
		{
			name: "idle pool pod is not backfilled",
			pods: []*corev1.Pod{pod},
		},
		{
			name: "pod owned by another sandbox is not backfilled",
			pods: []*corev1.Pod{pod},
			latestAllocation: map[string]string{
				"pool-pod": "other-sandbox",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := newLegacyAllocationSandbox("sandbox", pool.Name)
			if tt.mutate != nil {
				tt.mutate(sandbox)
			}
			original := sandbox.Annotations[AnnoAllocStatusKey]
			r := newBackfillTestReconciler(t, sandbox)

			latestAllocation := tt.latestAllocation
			if latestAllocation == nil {
				latestAllocation = map[string]string{}
			}
			if err := r.backfillLegacyPoolAllocation(ctx, pool, sandbox, tt.pods, latestAllocation); err != nil {
				t.Fatalf("backfillLegacyPoolAllocation() error = %v", err)
			}
			updated := getBackfillSandbox(t, ctx, r, sandbox)
			if got := updated.Annotations[AnnoAllocStatusKey]; got != original {
				t.Fatalf("alloc-status = %q, want unchanged %q", got, original)
			}
		})
	}
}

func TestReconcilePoolRequeuesAfterBackfillPatchFailure(t *testing.T) {
	ctx := context.Background()
	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", Generation: 1},
		Spec: sandboxv1alpha1.PoolSpec{
			CapacitySpec: sandboxv1alpha1.CapacitySpec{PoolMax: 2},
		},
	}
	sandbox := newLegacyAllocationSandbox("sandbox", pool.Name)
	allocatedPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "pool-pod",
		Namespace: "default",
		Labels:    map[string]string{LabelPoolName: pool.Name},
	}}
	idlePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "idle-pod",
		Namespace: "default",
		Labels:    map[string]string{LabelPoolName: pool.Name},
	}}

	r := newBackfillTestReconciler(t, pool, sandbox, allocatedPod, idlePod)
	failingClient := &backfillPatchFailingClient{
		Client:   r.Client,
		patchErr: errors.New("backfill patch failed"),
	}
	r.Client = failingClient
	r.Allocator = &backfillReconcileAllocator{
		latestAllocation: map[string]string{allocatedPod.Name: sandbox.Name},
	}

	result, err := r.reconcilePool(ctx, pool, []*sandboxv1alpha1.BatchSandbox{sandbox}, []*corev1.Pod{allocatedPod, idlePod})
	if err != nil {
		t.Fatalf("reconcilePool() error = %v, want nil", err)
	}
	if result.RequeueAfter != defaultRetryTime {
		t.Fatalf("RequeueAfter = %v, want %v", result.RequeueAfter, defaultRetryTime)
	}
	if failingClient.patchCalls != 1 {
		t.Fatalf("backfill patch calls = %d, want 1", failingClient.patchCalls)
	}

	if err := r.Get(ctx, types.NamespacedName{Name: idlePod.Name, Namespace: idlePod.Namespace}, &corev1.Pod{}); err == nil {
		t.Fatal("idle pod still exists; pool scaling did not run after backfill failure")
	}
	updatedPool := &sandboxv1alpha1.Pool{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(pool), updatedPool); err != nil {
		t.Fatalf("get updated pool: %v", err)
	}
	if updatedPool.Status.Allocated != 1 {
		t.Fatalf("pool status allocated = %d, want 1", updatedPool.Status.Allocated)
	}
}

func newLegacyAllocationSandbox(name, poolRef string) *sandboxv1alpha1.BatchSandbox {
	return &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Generation:  7,
			Finalizers:  []string{FinalizerPoolAllocation},
			Annotations: map[string]string{AnnoAllocStatusKey: `{"pods":["pool-pod"]}`},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{PoolRef: poolRef},
	}
}

func newBackfillTestReconciler(t *testing.T, objects ...runtime.Object) *PoolReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &PoolReconciler{
		Client:   fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&sandboxv1alpha1.Pool{}).WithRuntimeObjects(objects...).Build(),
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
	}
}

func getBackfillSandbox(t *testing.T, ctx context.Context, r *PoolReconciler, sandbox *sandboxv1alpha1.BatchSandbox) *sandboxv1alpha1.BatchSandbox {
	t.Helper()
	updated := &sandboxv1alpha1.BatchSandbox{}
	if err := r.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, updated); err != nil {
		t.Fatal(err)
	}
	return updated
}

func parseBackfillAllocation(t *testing.T, sandbox *sandboxv1alpha1.BatchSandbox) SandboxAllocation {
	t.Helper()
	allocation := SandboxAllocation{}
	if err := json.Unmarshal([]byte(sandbox.Annotations[AnnoAllocStatusKey]), &allocation); err != nil {
		t.Fatal(err)
	}
	return allocation
}

type backfillPatchFailingClient struct {
	client.Client
	patchErr   error
	patchCalls int
}

func (c *backfillPatchFailingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if _, ok := obj.(*sandboxv1alpha1.BatchSandbox); ok {
		c.patchCalls++
		return c.patchErr
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

type backfillReconcileAllocator struct {
	latestAllocation map[string]string
}

func (a *backfillReconcileAllocator) Schedule(context.Context, *AllocSpec) (*algorithm.AllocAction, error) {
	return &algorithm.AllocAction{}, nil
}

func (a *backfillReconcileAllocator) GetPoolAllocation(context.Context, *sandboxv1alpha1.Pool) (map[string]string, error) {
	return a.latestAllocation, nil
}

func (a *backfillReconcileAllocator) ClearPoolAllocation(context.Context, string, string) error {
	return nil
}

func (a *backfillReconcileAllocator) ReleasePodsAllocation(context.Context, string, string, []string) {
}

func (a *backfillReconcileAllocator) SyncSandboxAllocation(context.Context, *sandboxv1alpha1.BatchSandbox, []string) error {
	return nil
}

func (a *backfillReconcileAllocator) SyncSandboxReleased(context.Context, *sandboxv1alpha1.BatchSandbox, []string) error {
	return nil
}

func (a *backfillReconcileAllocator) GetSandboxAllocation(context.Context, *sandboxv1alpha1.BatchSandbox) ([]string, error) {
	return nil, nil
}

func (a *backfillReconcileAllocator) GetSandboxReleased(context.Context, *sandboxv1alpha1.BatchSandbox) ([]string, error) {
	return nil, nil
}
