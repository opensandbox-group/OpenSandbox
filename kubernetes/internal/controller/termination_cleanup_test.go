// Copyright 2025 The OpenSandbox Authors
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
	"fmt"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	taskscheduler "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/scheduler"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/fieldindex"
)

func TestReconcileTasksSkipsDeletingObjectAfterTaskCleanup(t *testing.T) {
	now := metav1.Now()
	sandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "terminating-sandbox",
			Namespace:         "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerPoolAllocation},
		},
	}
	key := types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Name}.String()
	_ = durationStore.Pop(key)

	r := &BatchSandboxReconciler{}
	result, err := r.reconcileTasks(context.Background(), sandbox, nil)
	if err != nil {
		t.Fatalf("reconcileTasks() error = %v", err)
	}
	if result != nil {
		t.Fatalf("reconcileTasks() result = %#v, want nil", result)
	}
	if requeueAfter := durationStore.Pop(key); requeueAfter != 0 {
		t.Fatalf("reconcileTasks() requeueAfter = %v, want 0", requeueAfter)
	}
	if _, exists := r.taskSchedulers.Load(key); exists {
		t.Fatal("task scheduler was recreated after task cleanup finalizer was removed")
	}
}

func TestDeletingTaskSandboxOnlyCleansUpTasks(t *testing.T) {
	tests := []struct {
		name  string
		phase sandboxv1alpha1.BatchSandboxPhase
		pause *bool
	}{
		{name: "running", phase: sandboxv1alpha1.BatchSandboxPhaseSucceed},
		{name: "pending pause", phase: sandboxv1alpha1.BatchSandboxPhaseSucceed, pause: ptr.To(true)},
		{name: "pending resume", phase: sandboxv1alpha1.BatchSandboxPhasePaused, pause: ptr.To(false)},
		{name: "pausing", phase: sandboxv1alpha1.BatchSandboxPhasePausing, pause: ptr.To(true)},
		{name: "resuming", phase: sandboxv1alpha1.BatchSandboxPhaseResuming, pause: ptr.To(false)},
		{name: "paused", phase: sandboxv1alpha1.BatchSandboxPhasePaused, pause: ptr.To(true)},
	}
	for _, tt := range tests {
		for _, expired := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/expired=%v", tt.name, expired), func(t *testing.T) {
				now := metav1.Now()
				sandbox := &sandboxv1alpha1.BatchSandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "deleting-task-sandbox",
						Namespace:         "default",
						Generation:        2,
						DeletionTimestamp: &now,
						Finalizers:        []string{finalizerTaskCleanup, "test.opensandbox.io/keep"},
					},
					Spec: sandboxv1alpha1.BatchSandboxSpec{
						Pause:    tt.pause,
						Replicas: ptr.To(int32(1)),
						Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "main", Image: "example.com"}},
						}},
						TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{Spec: sandboxv1alpha1.TaskSpec{
							Process: &sandboxv1alpha1.ProcessTask{Command: []string{"sleep", "3600"}},
						}},
					},
					Status: sandboxv1alpha1.BatchSandboxStatus{
						Phase:                   tt.phase,
						PauseObservedGeneration: 1,
					},
				}
				if expired {
					sandbox.Spec.ExpireTime = &metav1.Time{Time: time.Now().Add(-time.Minute)}
				}
				r := newTestReconciler(sandbox)
				scheduler := &recordingTaskScheduler{tasks: []taskscheduler.Task{
					fakeSchedulerTask{name: sandbox.Name + "-0", state: taskscheduler.RunningTaskState},
				}}
				key := client.ObjectKeyFromObject(sandbox)
				r.taskSchedulers.Store(key.String(), scheduler)
				t.Cleanup(func() { _ = durationStore.Pop(key.String()) })

				for i := 0; i < 2; i++ {
					if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
						t.Fatal(err)
					}
				}
				var pods corev1.PodList
				if err := r.List(context.Background(), &pods); err != nil {
					t.Fatal(err)
				}
				if len(pods.Items) != 0 {
					t.Fatalf("deleting task sandbox recreated %d pod(s)", len(pods.Items))
				}
				if scheduler.stopCalls != 2 || scheduler.scheduleCalls != 2 {
					t.Fatal("deleting sandbox did not continue task cleanup on each reconcile")
				}
				updated := &sandboxv1alpha1.BatchSandbox{}
				if err := r.Get(context.Background(), key, updated); err != nil {
					t.Fatal(err)
				}
				if !controllerutil.ContainsFinalizer(updated, finalizerTaskCleanup) {
					t.Fatal("task cleanup finalizer was removed before tasks finished")
				}

				scheduler.tasks = nil
				if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
					t.Fatal(err)
				}
				var snapshots sandboxv1alpha1.SandboxSnapshotList
				if err := r.List(context.Background(), &snapshots); err != nil {
					t.Fatal(err)
				}
				if len(snapshots.Items) != 0 {
					t.Fatalf("deleting task sandbox created %d snapshot(s)", len(snapshots.Items))
				}
				if err := r.Get(context.Background(), key, updated); err != nil {
					t.Fatal(err)
				}
				if controllerutil.ContainsFinalizer(updated, finalizerTaskCleanup) {
					t.Fatal("task cleanup finalizer remained after tasks finished")
				}
				if updated.Status.Phase != tt.phase {
					t.Fatalf("deletion changed lifecycle phase to %s, want %s", updated.Status.Phase, tt.phase)
				}
			})
		}
	}
}

func TestUnavailablePoolWaitsForAllocatedPodDeletion(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name := "terminating pool"
		if missing {
			name = "missing pool"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := metav1.Now()
			pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{
				Name: "pool-1", Namespace: "default", UID: "pool-uid",
				DeletionTimestamp: &now, Finalizers: []string{"test.opensandbox.io/keep"},
			}}
			sandbox := terminatingPoolSandbox("deleting-sandbox", pool.Name, &now)
			setSandboxAllocation(sandbox, sandboxAllocation{Pods: []string{"allocated-pod"}})
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "allocated-pod", Namespace: pool.Namespace,
					Finalizers: []string{"test.opensandbox.io/keep"},
					OwnerReferences: []metav1.OwnerReference{
						*metav1.NewControllerRef(pool, sandboxv1alpha1.GroupVersion.WithKind("Pool")),
					},
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			}
			objects := []client.Object{sandbox, pod}
			if !missing {
				objects = append(objects, pool)
			}
			c := fake.NewClientBuilder().WithScheme(testscheme).
				WithIndex(&sandboxv1alpha1.BatchSandbox{}, fieldindex.IndexNameForPoolRef, fieldindex.PoolRefIndexFunc).
				WithObjects(objects...).Build()
			readErr := apierrors.NewInternalError(fmt.Errorf("pod query failed"))
			apiReader := interceptor.NewClient(c, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.Pod); ok && readErr != nil {
						return readErr
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})
			r := &PoolReconciler{Client: c, APIReader: apiReader, Allocator: NewDefaultAllocator(c)}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}
			if _, err := r.Reconcile(ctx, req); err == nil {
				t.Fatal("pod query failure was treated as completed cleanup")
			}
			assertPoolFinalizerPresent(t, c, sandbox)
			readErr = nil

			for _, terminating := range []bool{false, true} {
				if terminating {
					if err := c.Delete(ctx, pod); err != nil {
						t.Fatal(err)
					}
				}
				result, err := r.Reconcile(ctx, req)
				if err != nil {
					t.Fatal(err)
				}
				if result.RequeueAfter != defaultRetryTime {
					t.Fatalf("requeueAfter = %v, want %v while allocated pod exists", result.RequeueAfter, defaultRetryTime)
				}
				assertPoolFinalizerPresent(t, c, sandbox)
			}

			if err := c.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
				t.Fatal(err)
			}
			pod.Finalizers = nil
			if err := c.Update(ctx, pod); err != nil {
				t.Fatal(err)
			}
			result, err := r.Reconcile(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if result.RequeueAfter != 0 {
				t.Fatalf("requeueAfter = %v after cleanup completed", result.RequeueAfter)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(sandbox), &sandboxv1alpha1.BatchSandbox{}); !apierrors.IsNotFound(err) {
				t.Fatalf("sandbox removal error = %v, want NotFound after allocated pod disappears", err)
			}
		})
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
					Finalizers:        []string{finalizerPoolAllocation, "test.opensandbox.io/keep"},
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
			if got := controllerutil.ContainsFinalizer(updated, finalizerPoolAllocation); got != tt.wantFinalizer {
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
			Finalizers:        []string{finalizerPoolAllocation, "test.opensandbox.io/keep"},
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
	if controllerutil.ContainsFinalizer(updated, finalizerPoolAllocation) {
		t.Fatal("pool finalizer was not removed")
	}
}

func TestCleanupTerminatingSandboxesForUnavailablePool(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	stranded := terminatingPoolSandbox("stranded", "missing-pool", &now)
	released := terminatingPoolSandbox("released", "missing-pool", &now)
	setSandboxAllocation(released, sandboxAllocation{Pods: []string{"historical-pod"}})
	released.Annotations[annoAllocReleasedKey] = `{"pods":["historical-pod"]}`
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "historical-pod", Namespace: "default"}}
	active := terminatingPoolSandbox("active", "missing-pool", nil)
	otherPool := terminatingPoolSandbox("other-pool", "existing-pool", &now)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&sandboxv1alpha1.BatchSandbox{}, fieldindex.IndexNameForPoolRef, fieldindex.PoolRefIndexFunc).
		WithObjects(stranded, released, pod, active, otherPool).
		Build()
	r := &PoolReconciler{Client: fakeClient, APIReader: fakeClient, Allocator: NewDefaultAllocator(fakeClient)}

	cleanupComplete, err := r.cleanupTerminatingSandboxesForUnavailablePool(context.Background(), "default", "missing-pool", "")
	if err != nil {
		t.Fatalf("cleanupTerminatingSandboxesForUnavailablePool() error = %v", err)
	}
	if !cleanupComplete {
		t.Fatal("cleanup remained pending after all allocations were released or absent")
	}

	updated := &sandboxv1alpha1.BatchSandbox{}
	err = fakeClient.Get(context.Background(), client.ObjectKeyFromObject(stranded), updated)
	if err == nil {
		if controllerutil.ContainsFinalizer(updated, finalizerPoolAllocation) {
			t.Fatal("stale pool finalizer was not removed")
		}
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("get stranded sandbox: %v", err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(released), &sandboxv1alpha1.BatchSandbox{}); !apierrors.IsNotFound(err) {
		t.Fatalf("sandbox with only historical released pods was not finalized: %v", err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Fatalf("historical released pod was removed: %v", err)
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
			Finalizers:        []string{finalizerPoolAllocation},
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
	if !controllerutil.ContainsFinalizer(updated, finalizerPoolAllocation) {
		t.Fatalf("sandbox %s unexpectedly lost pool finalizer", sandbox.Name)
	}
}
