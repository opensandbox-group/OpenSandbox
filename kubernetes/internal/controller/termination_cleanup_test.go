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
	"testing"

	"github.com/golang/mock/gomock"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/fieldindex"
)

func TestReconcileTasksSkipsDeletingObjectAfterTaskCleanup(t *testing.T) {
	now := metav1.Now()
	sandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "terminating-sandbox",
			Namespace:         "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{FinalizerPoolAllocation},
		},
	}
	key := types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Name}.String()
	_ = DurationStore.Pop(key)

	r := &BatchSandboxReconciler{}
	result, err := r.reconcileTasks(context.Background(), sandbox, nil)
	if err != nil {
		t.Fatalf("reconcileTasks() error = %v", err)
	}
	if result != nil {
		t.Fatalf("reconcileTasks() result = %#v, want nil", result)
	}
	if requeueAfter := DurationStore.Pop(key); requeueAfter != 0 {
		t.Fatalf("reconcileTasks() requeueAfter = %v, want 0", requeueAfter)
	}
	if _, exists := r.taskSchedulers.Load(key); exists {
		t.Fatal("task scheduler was recreated after task cleanup finalizer was removed")
	}
}

func TestFinalizeTerminatingSandboxesWithoutPendingAllocations(t *testing.T) {
	tests := []struct {
		name          string
		allocated     []string
		released      []string
		wantFinalizer bool
	}{
		{
			name:          "empty legacy allocation",
			wantFinalizer: false,
		},
		{
			name:          "all allocations already released",
			allocated:     []string{"pod-1"},
			released:      []string{"pod-1"},
			wantFinalizer: false,
		},
		{
			name:          "unreleased allocation remains",
			allocated:     []string{"pod-1"},
			wantFinalizer: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockController := gomock.NewController(t)
			allocator := NewMockAllocator(mockController)
			scheme := runtime.NewScheme()
			if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			now := metav1.Now()
			sandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "terminating-sandbox",
					Namespace:         "default",
					DeletionTimestamp: &now,
					Finalizers:        []string{FinalizerPoolAllocation, "test.opensandbox.io/keep"},
				},
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()

			allocator.EXPECT().GetSandboxAllocation(gomock.Any(), sandbox).Return(tt.allocated, nil)
			allocator.EXPECT().GetSandboxReleased(gomock.Any(), sandbox).Return(tt.released, nil)

			r := &PoolReconciler{Client: fakeClient, Allocator: allocator}
			if err := r.finalizeTerminatingSandboxes(context.Background(), []*sandboxv1alpha1.BatchSandbox{sandbox}); err != nil {
				t.Fatalf("finalizeTerminatingSandboxes() error = %v", err)
			}

			updated := &sandboxv1alpha1.BatchSandbox{}
			if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(sandbox), updated); err != nil {
				t.Fatalf("get sandbox: %v", err)
			}
			if got := controllerutil.ContainsFinalizer(updated, FinalizerPoolAllocation); got != tt.wantFinalizer {
				t.Fatalf("pool finalizer present = %v, want %v", got, tt.wantFinalizer)
			}
			if !controllerutil.ContainsFinalizer(updated, "test.opensandbox.io/keep") {
				t.Fatal("unrelated finalizer was removed")
			}
		})
	}
}

func TestDoReleaseFinalizesWithoutResyncingHistoricalReleasedPods(t *testing.T) {
	mockController := gomock.NewController(t)
	allocator := NewMockAllocator(mockController)
	scheme := runtime.NewScheme()
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	sandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sandbox-a",
			Namespace:         "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{FinalizerPoolAllocation, "test.opensandbox.io/keep"},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{PoolRef: "pool-1"},
	}
	pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool-1", Namespace: "default"}}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()

	// pod-1 may already have been reassigned after its earlier release. This
	// cleanup must inspect the annotations but must not call SyncSandboxReleased,
	// which would delete pod-1's current in-memory owner by name.
	allocator.EXPECT().GetSandboxAllocation(gomock.Any(), sandbox).Return([]string{"pod-1"}, nil)
	allocator.EXPECT().GetSandboxReleased(gomock.Any(), sandbox).Return([]string{"pod-1"}, nil)

	r := &PoolReconciler{Client: fakeClient, Allocator: allocator}
	if _, err := r.doRelease(context.Background(), pool, []*sandboxv1alpha1.BatchSandbox{sandbox}, nil, nil); err != nil {
		t.Fatalf("doRelease() error = %v", err)
	}

	updated := &sandboxv1alpha1.BatchSandbox{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(sandbox), updated); err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if controllerutil.ContainsFinalizer(updated, FinalizerPoolAllocation) {
		t.Fatal("pool finalizer was not removed")
	}
}

func TestCleanupTerminatingSandboxesForUnavailablePool(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	stranded := terminatingPoolSandbox("stranded", "missing-pool", &now)
	active := terminatingPoolSandbox("active", "missing-pool", nil)
	otherPool := terminatingPoolSandbox("other-pool", "existing-pool", &now)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&sandboxv1alpha1.BatchSandbox{}, fieldindex.IndexNameForPoolRef, fieldindex.PoolRefIndexFunc).
		WithObjects(stranded, active, otherPool).
		Build()
	r := &PoolReconciler{Client: fakeClient, APIReader: fakeClient}

	poolUnavailable, err := r.cleanupTerminatingSandboxesForUnavailablePool(context.Background(), "default", "missing-pool", "")
	if err != nil {
		t.Fatalf("cleanupTerminatingSandboxesForUnavailablePool() error = %v", err)
	}
	if !poolUnavailable {
		t.Fatal("cleanupTerminatingSandboxesForUnavailablePool() reported the missing Pool as available")
	}

	updated := &sandboxv1alpha1.BatchSandbox{}
	err = fakeClient.Get(context.Background(), client.ObjectKeyFromObject(stranded), updated)
	if err == nil {
		if controllerutil.ContainsFinalizer(updated, FinalizerPoolAllocation) {
			t.Fatal("stale pool finalizer was not removed")
		}
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("get stranded sandbox: %v", err)
	}

	assertPoolFinalizerPresent(t, fakeClient, active)
	assertPoolFinalizerPresent(t, fakeClient, otherPool)
}

func TestPoolReconcileDoesNotCleanUpOnCachedNotFound(t *testing.T) {
	mockController := gomock.NewController(t)
	allocator := NewMockAllocator(mockController)
	scheme := runtime.NewScheme()
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	sandbox := terminatingPoolSandbox("terminating", "pool-1", &now)
	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-1", Namespace: "default", UID: types.UID("new-pool")},
	}
	cachedClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
	apiReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).Build()
	r := &PoolReconciler{Client: cachedClient, APIReader: apiReader, Allocator: allocator}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != defaultRetryTime {
		t.Fatalf("Reconcile() requeueAfter = %v, want %v", result.RequeueAfter, defaultRetryTime)
	}
	assertPoolFinalizerPresent(t, cachedClient, sandbox)
}

func TestPoolReconcileDoesNotCleanUpRecreatedPool(t *testing.T) {
	mockController := gomock.NewController(t)
	allocator := NewMockAllocator(mockController)
	scheme := runtime.NewScheme()
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	cachedPool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "pool-1",
			Namespace:         "default",
			UID:               types.UID("old-pool"),
			DeletionTimestamp: &now,
			Finalizers:        []string{"test.opensandbox.io/keep"},
		},
	}
	recreatedPool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-1", Namespace: "default", UID: types.UID("new-pool")},
	}
	cachedClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cachedPool).Build()
	apiReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(recreatedPool).Build()
	r := &PoolReconciler{Client: cachedClient, APIReader: apiReader, Allocator: allocator}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cachedPool)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != defaultRetryTime {
		t.Fatalf("Reconcile() requeueAfter = %v, want %v", result.RequeueAfter, defaultRetryTime)
	}
}

func TestPoolReconcileRechecksBeforeRemovingFinalizer(t *testing.T) {
	mockController := gomock.NewController(t)
	allocator := NewMockAllocator(mockController)
	scheme := runtime.NewScheme()
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	sandbox := terminatingPoolSandbox("terminating", "pool-1", &now)
	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-1", Namespace: "default", UID: types.UID("new-pool")},
	}
	cachedClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&sandboxv1alpha1.BatchSandbox{}, fieldindex.IndexNameForPoolRef, fieldindex.PoolRefIndexFunc).
		WithObjects(sandbox).
		Build()
	apiClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, sandbox.DeepCopy()).Build()
	apiReader := &poolAppearingReader{Reader: apiClient, poolKey: client.ObjectKeyFromObject(pool)}
	allocator.EXPECT().ClearPoolAllocation(gomock.Any(), "default", "pool-1").Return(nil)
	r := &PoolReconciler{Client: cachedClient, APIReader: apiReader, Allocator: allocator}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != defaultRetryTime {
		t.Fatalf("Reconcile() requeueAfter = %v, want %v", result.RequeueAfter, defaultRetryTime)
	}
	if apiReader.poolGets < 2 {
		t.Fatalf("uncached Pool reads = %d, want at least 2", apiReader.poolGets)
	}
	assertPoolFinalizerPresent(t, cachedClient, sandbox)
}

type poolAppearingReader struct {
	client.Reader
	poolKey  client.ObjectKey
	poolGets int
}

func (r *poolAppearingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*sandboxv1alpha1.Pool); ok && key == r.poolKey {
		r.poolGets++
		if r.poolGets == 1 {
			return apierrors.NewNotFound(schema.GroupResource{Group: sandboxv1alpha1.GroupVersion.Group, Resource: "pools"}, key.Name)
		}
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func terminatingPoolSandbox(name, poolRef string, deletionTimestamp *metav1.Time) *sandboxv1alpha1.BatchSandbox {
	return &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			DeletionTimestamp: deletionTimestamp,
			Finalizers:        []string{FinalizerPoolAllocation},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{PoolRef: poolRef},
	}
}

func assertPoolFinalizerPresent(t *testing.T, c client.Client, sandbox *sandboxv1alpha1.BatchSandbox) {
	t.Helper()
	updated := &sandboxv1alpha1.BatchSandbox{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sandbox), updated); err != nil {
		t.Fatalf("get sandbox %s: %v", sandbox.Name, err)
	}
	if !controllerutil.ContainsFinalizer(updated, FinalizerPoolAllocation) {
		t.Fatalf("sandbox %s unexpectedly lost pool finalizer", sandbox.Name)
	}
}
