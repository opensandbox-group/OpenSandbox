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

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/strategy"
	taskapi "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

func TestPoolAllocationExclusivity(t *testing.T) {
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	config, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	apiClient, err := client.NewWithWatch(config, client.Options{Scheme: testscheme})
	require.NoError(t, err)
	ctx := context.Background()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "allocation-exclusive-"}}
	require.NoError(t, apiClient.Create(ctx, namespace))

	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: namespace.Name},
		Spec: sandboxv1alpha1.PoolSpec{
			CapacitySpec: sandboxv1alpha1.CapacitySpec{PoolMax: 2, BufferMax: 2},
		},
	}
	require.NoError(t, apiClient.Create(ctx, pool))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "shared-pod", Namespace: namespace.Name,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(pool, sandboxv1alpha1.GroupVersion.WithKind("Pool"))},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "example.com/sandbox"}}},
	}
	require.NoError(t, apiClient.Create(ctx, pod))
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, apiClient.Status().Update(ctx, pod))

	sandboxes := make([]*sandboxv1alpha1.BatchSandbox, 0, 2)
	for _, name := range []string{"sandbox-a", "sandbox-b"} {
		sandbox := &sandboxv1alpha1.BatchSandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: namespace.Name,
				Finalizers: []string{finalizerPoolAllocation},
			},
			Spec: sandboxv1alpha1.BatchSandboxSpec{PoolRef: pool.Name, Replicas: ptr.To(int32(1))},
		}
		setSandboxAllocation(sandbox, sandboxAllocation{Pods: []string{pod.Name}})
		require.NoError(t, apiClient.Create(ctx, sandbox))
		sandboxes = append(sandboxes, sandbox)
	}
	allocator := NewDefaultAllocator(apiClient, apiClient)
	before, err := allocator.GetPoolAllocation(ctx, pool)
	require.NoError(t, err)
	owner := before[pod.Name]
	require.NotEmpty(t, owner)
	for _, sandbox := range sandboxes {
		if sandbox.Name != owner {
			sandbox.Annotations[annoAllocReleaseKey] = `{"pods":["shared-pod"]}`
			require.NoError(t, apiClient.Update(ctx, sandbox))
		}
	}
	r := &PoolReconciler{
		Client: apiClient, APIReader: apiClient, Allocator: allocator,
		Recorder: record.NewFakeRecorder(20),
	}
	// A concurrent metadata update must reject the stale repair without changing ownership.
	allocator.client = interceptor.NewClient(apiClient, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			latest := &sandboxv1alpha1.BatchSandbox{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), latest))
			latest.Annotations["test.opensandbox.io/concurrent"] = "preserved"
			require.NoError(t, c.Update(ctx, latest))
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	_, err = r.scheduleSandbox(ctx, pool, sandboxes, []*corev1.Pod{pod})
	require.True(t, apierrors.IsConflict(err), "repair must use the observed resource version: %v", err)
	afterConflict, err := allocator.GetPoolAllocation(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, before, afterConflict)
	allocator.client = apiClient
	for _, sandbox := range sandboxes {
		require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox))
	}
	// I-2: recovering a single map entry must also repair the conflicting API records.
	result, err := r.scheduleSandbox(ctx, pool, sandboxes, []*corev1.Pod{pod})
	require.NoError(t, err)
	require.Empty(t, result.ToDelete, "a duplicate reference must not recycle the owner's Pod")
	require.Equal(t, map[string]string{pod.Name: owner}, result.LatestAllocation)
	require.EqualValues(t, 1, result.SupplyCnt, "the other sandbox still needs a Pod")
	reader := &BatchSandboxReconciler{Client: apiClient}
	for _, sandbox := range sandboxes {
		require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox))
		visible, err := reader.listPods(ctx, strategy.NewPoolStrategy(sandbox), sandbox)
		require.NoError(t, err)
		if sandbox.Name == owner {
			require.Len(t, visible, 1)
		} else {
			require.Empty(t, visible)
			require.Equal(t, "preserved", sandbox.Annotations["test.opensandbox.io/concurrent"])
		}
	}

	versions := []string{sandboxes[0].ResourceVersion, sandboxes[1].ResourceVersion}
	_, err = r.scheduleSandbox(ctx, pool, sandboxes, []*corev1.Pod{pod})
	require.NoError(t, err)
	for i, sandbox := range sandboxes {
		require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox))
		require.Equal(t, versions[i], sandbox.ResourceVersion, "a settled reconcile must not rewrite allocations")
	}
	recovered, err := NewDefaultAllocator(apiClient, apiClient).GetPoolAllocation(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, map[string]string{pod.Name: owner}, recovered)

	spare := pod.DeepCopy()
	spare.ObjectMeta = metav1.ObjectMeta{Name: "spare-pod", Namespace: namespace.Name, OwnerReferences: pod.OwnerReferences}
	require.NoError(t, apiClient.Create(ctx, spare))
	spare.Status = pod.Status
	require.NoError(t, apiClient.Status().Update(ctx, spare))
	result, err = r.scheduleSandbox(ctx, pool, sandboxes, []*corev1.Pod{pod, spare})
	require.NoError(t, err)
	require.Empty(t, result.ToDelete)
	require.Zero(t, result.SupplyCnt)
	require.Len(t, result.LatestAllocation, 2)
	require.Equal(t, owner, result.LatestAllocation[pod.Name])
	require.NotEqual(t, owner, result.LatestAllocation[spare.Name])

	// A later conflicting annotation must not displace the established owner.
	for _, sandbox := range sandboxes {
		if sandbox.Name != owner {
			require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox))
			setSandboxAllocation(sandbox, sandboxAllocation{Pods: []string{pod.Name, spare.Name}})
			require.NoError(t, apiClient.Update(ctx, sandbox))
		}
	}
	stableAllocation := result.LatestAllocation
	result, err = r.scheduleSandbox(ctx, pool, sandboxes, []*corev1.Pod{pod, spare})
	require.NoError(t, err)
	require.Empty(t, result.ToDelete)
	require.Equal(t, stableAllocation, result.LatestAllocation)

}

func TestSetReleasedPreservesConcurrentCleanup(t *testing.T) {
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	config, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	apiClient, err := client.NewWithWatch(config, client.Options{Scheme: testscheme})
	require.NoError(t, err)
	ctx := context.Background()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "release-conflict-"}}
	require.NoError(t, apiClient.Create(ctx, namespace))
	t.Cleanup(func() { require.NoError(t, apiClient.Delete(ctx, namespace)) })

	for _, name := range []string{"stale-input", "write-conflict", "stale-cache"} {
		beforeRead := name == "stale-input"
		t.Run(name, func(t *testing.T) {
			allocation, err := marshalJSON(&sandboxAllocation{Pods: []string{"pod-1"}})
			require.NoError(t, err)
			sandbox := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace.Name,
					Finalizers:  []string{finalizerPoolAllocation, finalizerTaskCleanup, "example.com/retained"},
					Annotations: map[string]string{annoAllocStatusKey: allocation}},
				Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: ptr.To(int32(1))},
			}
			require.NoError(t, apiClient.Create(ctx, sandbox))
			require.NoError(t, apiClient.Delete(ctx, sandbox))
			require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox))
			cached := sandbox.DeepCopy()
			removeTaskFinalizer := func() {
				latest := &sandboxv1alpha1.BatchSandbox{}
				require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(sandbox), latest))
				latest.Finalizers = []string{finalizerPoolAllocation, "example.com/retained"}
				latest.Annotations["example.com/concurrent"] = "preserved"
				require.NoError(t, apiClient.Update(ctx, latest))
			}
			if beforeRead {
				removeTaskFinalizer()
			}
			patches, cachedReads := 0, 0
			conflictClient := interceptor.NewClient(apiClient, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if name == "stale-cache" {
						cachedReads++
						cached.DeepCopyInto(obj.(*sandboxv1alpha1.BatchSandbox))
						return nil
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					if !beforeRead && patches == 1 {
						removeTaskFinalizer()
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			syncer := NewDefaultAllocator(conflictClient, apiClient).syncer
			applied, err := syncer.SetReleased(ctx, cached, &allocationReleased{Pods: []string{"pod-1"}})
			require.NoError(t, err)
			require.True(t, applied)
			stored := &sandboxv1alpha1.BatchSandbox{}
			require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(sandbox), stored))
			require.Equal(t, []string{"example.com/retained"}, stored.Finalizers)
			require.Equal(t, "preserved", stored.Annotations["example.com/concurrent"])
			released, err := syncer.GetReleased(ctx, stored)
			require.NoError(t, err)
			require.Equal(t, []string{"pod-1"}, released.Pods)
			if !beforeRead {
				require.GreaterOrEqual(t, patches, 2, "a conflicting patch must reread and retry")
			}
			require.Zero(t, cachedReads, "retry reads must bypass the informer cache")
			stored.Finalizers = nil
			require.NoError(t, apiClient.Update(ctx, stored))
		})
	}
	t.Run("allocation-changes-on-retry", func(t *testing.T) {
		allocation, err := marshalJSON(&sandboxAllocation{Pods: []string{"pod-1"}})
		require.NoError(t, err)
		sandbox := &sandboxv1alpha1.BatchSandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "allocation-changes", Namespace: namespace.Name,
				Finalizers:  []string{finalizerPoolAllocation, "example.com/retained"},
				Annotations: map[string]string{annoAllocStatusKey: allocation}},
			Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: ptr.To(int32(1))},
		}
		require.NoError(t, apiClient.Create(ctx, sandbox))
		require.NoError(t, apiClient.Delete(ctx, sandbox))
		patches := 0
		conflictClient := interceptor.NewClient(apiClient, interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				if patches == 1 {
					latest := &sandboxv1alpha1.BatchSandbox{}
					require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(sandbox), latest))
					latest.Annotations[annoAllocStatusKey], err = marshalJSON(&sandboxAllocation{Pods: []string{"pod-1", "pod-2"}})
					require.NoError(t, err)
					require.NoError(t, apiClient.Update(ctx, latest))
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		})
		syncer := &annoAllocationSyncer{client: conflictClient, apiReader: apiClient}
		applied, err := syncer.SetReleased(ctx, sandbox, &allocationReleased{Pods: []string{"pod-1"}})
		require.NoError(t, err)
		require.True(t, applied)
		require.GreaterOrEqual(t, patches, 2)
		stored := &sandboxv1alpha1.BatchSandbox{}
		require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(sandbox), stored))
		require.Contains(t, stored.Finalizers, finalizerPoolAllocation, "newly allocated pods must be released before dropping the guard")
		stored.Finalizers = nil
		require.NoError(t, apiClient.Update(ctx, stored))
	})

	t.Run("recreated-name", func(t *testing.T) {
		replacement := &sandboxv1alpha1.BatchSandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "recreated", Namespace: namespace.Name},
			Spec:       sandboxv1alpha1.BatchSandboxSpec{Replicas: ptr.To(int32(1))},
		}
		require.NoError(t, apiClient.Create(ctx, replacement))
		stale := replacement.DeepCopy()
		stale.UID = "previous-sandbox"
		syncer := &annoAllocationSyncer{client: apiClient, apiReader: apiClient}
		applied, err := syncer.SetReleased(ctx, stale, &allocationReleased{Pods: []string{"old-pod"}})
		require.NoError(t, err)
		require.False(t, applied)
		stored := &sandboxv1alpha1.BatchSandbox{}
		require.NoError(t, apiClient.Get(ctx, client.ObjectKeyFromObject(replacement), stored))
		require.Empty(t, stored.Annotations[annoAllocReleasedKey], "release state from a deleted sandbox must not touch its replacement")
		require.NoError(t, apiClient.Delete(ctx, stored))
	})

	t.Run("already-deleted", func(t *testing.T) {
		syncer := &annoAllocationSyncer{client: apiClient, apiReader: apiClient}
		gone := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "gone", Namespace: namespace.Name}}
		require.True(t, apierrors.IsNotFound(apiClient.Get(ctx, client.ObjectKeyFromObject(gone), &sandboxv1alpha1.BatchSandbox{})))
		applied, err := syncer.SetReleased(ctx, gone, &allocationReleased{})
		require.NoError(t, err)
		require.False(t, applied)
	})
}

func TestTaskAllocationExclusivity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tasks     []taskapi.Task
		status    int
		wantOwner string
	}{
		{name: "retain running task", tasks: []taskapi.Task{{Name: "sandbox-a-0", ProcessStatus: &taskapi.ProcessStatus{Running: &taskapi.Running{}}}}, wantOwner: "sandbox-a"},
		{name: "empty executor", wantOwner: "sandbox-b"},
		{name: "unknown task", tasks: []taskapi.Task{{Name: "unrelated-0"}}},
		{name: "ambiguous tasks", tasks: []taskapi.Task{{Name: "sandbox-a-0"}, {Name: "sandbox-b-0"}}},
		{name: "unavailable executor", status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/getTasks", r.URL.Path)
				require.Equal(t, http.MethodGet, r.Method, "conflict recovery must not stop or dispatch tasks")
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					return
				}
				require.NoError(t, json.NewEncoder(w).Encode(tc.tasks))
			}))
			defer server.Close()
			pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "test"}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "test"}, Status: corev1.PodStatus{PodIP: "127.0.0.1"}}
			sandboxes := []*sandboxv1alpha1.BatchSandbox{}
			objects := []client.Object{pod, pool}
			for _, name := range []string{"sandbox-a", "sandbox-b"} {
				sandbox := &sandboxv1alpha1.BatchSandbox{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test", Finalizers: []string{finalizerTaskCleanup}},
					Spec:       sandboxv1alpha1.BatchSandboxSpec{PoolRef: "pool", Replicas: ptr.To(int32(1)), TaskTemplate: &sandboxv1alpha1.TaskTemplateSpec{Spec: sandboxv1alpha1.TaskSpec{Process: &sandboxv1alpha1.ProcessTask{Command: []string{"sleep", "3600"}}}}},
				}
				setSandboxAllocation(sandbox, sandboxAllocation{Pods: []string{"shared"}})
				sandboxes = append(sandboxes, sandbox)
				objects = append(objects, sandbox)
			}
			c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(objects...).Build()
			allocator := NewDefaultAllocator(c, c)
			allocator.taskClient = func(_ *corev1.Pod) *taskapi.Client { return taskapi.NewClient(server.URL) }
			// A cache miss must not hide the live executor from ownership resolution.
			allocator.client = interceptor.NewClient(c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					return apierrors.NewNotFound(corev1.Resource("pods"), key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			}})
			batch := &BatchSandboxReconciler{Client: c, APIReader: c, AllocationReader: allocator}
			// A cold allocator must wait for both claims, even if the informer has only seen one.
			_, err := allocator.Schedule(context.Background(), &allocSpec{Pool: pool, Pods: []*corev1.Pod{pod}, Sandboxes: sandboxes[:1]})
			require.Error(t, err)
			for _, sandbox := range sandboxes {
				_, err := batch.listPods(context.Background(), strategy.NewPoolStrategy(sandbox), sandbox)
				require.Error(t, err, "neither claimant may dispatch until the conflict is repaired")
			}
			action, err := allocator.Schedule(context.Background(), &allocSpec{Pool: pool, Pods: []*corev1.Pod{pod}, Sandboxes: sandboxes})
			if tc.wantOwner == "" {
				require.Error(t, err)
				for _, sandbox := range sandboxes {
					latest := &sandboxv1alpha1.BatchSandbox{}
					require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(sandbox), latest))
					require.Equal(t, sandbox.Annotations[annoAllocStatusKey], latest.Annotations[annoAllocStatusKey])
				}
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, action.PodSupplement)
			require.Empty(t, action.ToRelease)
			owners, err := allocator.GetPoolAllocation(context.Background(), pool)
			require.NoError(t, err)
			require.Equal(t, tc.wantOwner, owners["shared"])
			for _, sandbox := range sandboxes {
				require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(sandbox), sandbox))
				visible, err := batch.listPods(context.Background(), strategy.NewPoolStrategy(sandbox), sandbox)
				require.NoError(t, err)
				if sandbox.Name == tc.wantOwner {
					require.Len(t, visible, 1)
				} else {
					require.Empty(t, visible)
				}
			}
		})
	}
}
