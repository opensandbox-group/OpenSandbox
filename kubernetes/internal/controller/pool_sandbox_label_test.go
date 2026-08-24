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
	"errors"
	"fmt"
	goruntime "runtime"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func TestDoAllocateLabelsPodBeforeAllocationSync(t *testing.T) {
	ctx := context.Background()
	pod := testPoolPod("pool-pod", map[string]string{LabelPoolName: "pool"})
	sandbox := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: pod.Namespace}}
	reconciler, countingClient := newPodLabelTestReconciler(t, pod)
	order := &labelSyncOrder{}
	countingClient.onPodPatch = func() { order.record("label-patch") }

	mockController := gomock.NewController(t)
	allocator := NewMockAllocator(mockController)
	allocator.EXPECT().GetSandboxAllocation(gomock.Any(), sandbox).Return([]string{}, nil)
	allocator.EXPECT().SyncSandboxAllocation(gomock.Any(), sandbox, []string{pod.Name}).DoAndReturn(
		func(ctx context.Context, _ *sandboxv1alpha1.BatchSandbox, _ []string) error {
			updated := &corev1.Pod{}
			if err := reconciler.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, updated); err != nil {
				return err
			}
			if got := updated.Labels[LabelBatchSandboxNameKey]; got != sandbox.Name {
				return fmt.Errorf("sandbox label = %q, want %q", got, sandbox.Name)
			}
			order.record("allocation-sync")
			return nil
		},
	)
	reconciler.Allocator = allocator

	require.NoError(t, reconciler.doAllocate(
		ctx,
		&sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: pod.Namespace}},
		[]*sandboxv1alpha1.BatchSandbox{sandbox},
		[]*corev1.Pod{pod.DeepCopy()},
		map[string][]string{sandbox.Name: {pod.Name}},
	))
	require.Equal(t, []string{"label-patch", "allocation-sync"}, order.events())
}

func TestDoAllocateDoesNotPublishMissingPod(t *testing.T) {
	ctx := context.Background()
	pod := testPoolPod("missing-pod", map[string]string{LabelPoolName: "pool"})
	sandbox := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: pod.Namespace}}
	reconciler, countingClient := newPodLabelTestReconciler(t, pod)
	countingClient.podPatchErr = apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, pod.Name)

	mockController := gomock.NewController(t)
	allocator := NewMockAllocator(mockController)
	allocator.EXPECT().GetSandboxAllocation(gomock.Any(), sandbox).Return([]string{}, nil)
	reconciler.Allocator = allocator

	err := reconciler.doAllocate(
		ctx,
		&sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: pod.Namespace}},
		[]*sandboxv1alpha1.BatchSandbox{sandbox},
		[]*corev1.Pod{pod.DeepCopy()},
		map[string][]string{sandbox.Name: {pod.Name}},
	)
	require.ErrorIs(t, err, countingClient.podPatchErr)
}

func TestDoAllocateRollsBackOnlyFailedPublicationLabels(t *testing.T) {
	ctx := context.Background()
	podA := testPoolPod("pool-pod-a", map[string]string{LabelPoolName: "pool"})
	podB := testPoolPod("pool-pod-b", map[string]string{LabelPoolName: "pool"})
	sandboxA := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: podA.Namespace}}
	sandboxB := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-b", Namespace: podB.Namespace}}
	pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: podA.Namespace}}
	reconciler, _ := newPodLabelTestReconciler(t, podA, podB)
	publicationErr := errors.New("allocation publication failed")

	mockController := gomock.NewController(t)
	allocator := NewMockAllocator(mockController)
	allocator.EXPECT().GetSandboxAllocation(gomock.Any(), sandboxA).Return([]string{}, nil)
	allocator.EXPECT().GetSandboxAllocation(gomock.Any(), sandboxB).Return([]string{}, nil)
	allocator.EXPECT().SyncSandboxAllocation(gomock.Any(), sandboxA, []string{podA.Name}).Return(nil)
	allocator.EXPECT().SyncSandboxAllocation(gomock.Any(), sandboxB, []string{podB.Name}).Return(publicationErr)
	allocator.EXPECT().GetPoolAllocation(gomock.Any(), pool).Return(map[string]string{podA.Name: sandboxA.Name}, nil)
	reconciler.Allocator = allocator

	err := reconciler.doAllocate(
		ctx,
		pool,
		[]*sandboxv1alpha1.BatchSandbox{sandboxA, sandboxB},
		[]*corev1.Pod{podA.DeepCopy(), podB.DeepCopy()},
		map[string][]string{sandboxA.Name: {podA.Name}, sandboxB.Name: {podB.Name}},
	)
	require.ErrorIs(t, err, publicationErr)

	updatedA := getPodLabelTestPod(t, ctx, reconciler.Client, podA)
	updatedB := getPodLabelTestPod(t, ctx, reconciler.Client, podB)
	require.Equal(t, sandboxA.Name, updatedA.Labels[LabelBatchSandboxNameKey])
	_, hasFailedSandboxLabel := updatedB.Labels[LabelBatchSandboxNameKey]
	require.False(t, hasFailedSandboxLabel)
}

func TestSyncPodSandboxLabelsAllocation(t *testing.T) {
	ctx := context.Background()
	pod := testPoolPod("pool-pod", map[string]string{LabelPoolName: "pool"})
	reconciler, countingClient := newPodLabelTestReconciler(t, pod)
	listedPod := pod.DeepCopy()

	require.NoError(t, reconciler.syncPodSandboxLabels(ctx, []*corev1.Pod{listedPod}, map[string]string{pod.Name: "sandbox-a"}))
	require.NoError(t, reconciler.syncPodSandboxLabels(ctx, []*corev1.Pod{listedPod}, map[string]string{pod.Name: "sandbox-a"}))

	updated := getPodLabelTestPod(t, ctx, reconciler.Client, pod)
	require.Equal(t, "sandbox-a", updated.Labels[LabelBatchSandboxNameKey])
	require.Equal(t, "pool", updated.Labels[LabelPoolName])
	require.Equal(t, 1, countingClient.podPatchCalls())
}

func TestSyncPodSandboxLabelsRelease(t *testing.T) {
	ctx := context.Background()
	pod := testPoolPod("pool-pod", map[string]string{
		LabelPoolName:            "pool",
		LabelBatchSandboxNameKey: "sandbox-a",
	})
	reconciler, countingClient := newPodLabelTestReconciler(t, pod)

	require.NoError(t, reconciler.syncPodSandboxLabels(ctx, []*corev1.Pod{pod.DeepCopy()}, map[string]string{}))

	updated := getPodLabelTestPod(t, ctx, reconciler.Client, pod)
	_, hasSandboxLabel := updated.Labels[LabelBatchSandboxNameKey]
	require.False(t, hasSandboxLabel)
	require.Equal(t, "pool", updated.Labels[LabelPoolName])
	require.Equal(t, 1, countingClient.podPatchCalls())
}

func TestSyncPodSandboxLabelsReassignment(t *testing.T) {
	ctx := context.Background()
	pod := testPoolPod("pool-pod", map[string]string{
		LabelPoolName:            "pool",
		LabelBatchSandboxNameKey: "sandbox-a",
	})
	reconciler, countingClient := newPodLabelTestReconciler(t, pod)

	require.NoError(t, reconciler.syncPodSandboxLabels(ctx, []*corev1.Pod{pod.DeepCopy()}, map[string]string{pod.Name: "sandbox-b"}))

	updated := getPodLabelTestPod(t, ctx, reconciler.Client, pod)
	require.Equal(t, "sandbox-b", updated.Labels[LabelBatchSandboxNameKey])
	require.Equal(t, 1, countingClient.podPatchCalls())
}

func TestSyncPodSandboxLabelsNoOp(t *testing.T) {
	ctx := context.Background()
	allocatedPod := testPoolPod("allocated-pod", map[string]string{
		LabelPoolName:            "pool",
		LabelBatchSandboxNameKey: "sandbox-a",
	})
	idlePod := testPoolPod("idle-pod", map[string]string{LabelPoolName: "pool"})
	reconciler, countingClient := newPodLabelTestReconciler(t, allocatedPod, idlePod)

	require.NoError(t, reconciler.syncPodSandboxLabels(
		ctx,
		[]*corev1.Pod{allocatedPod.DeepCopy(), idlePod.DeepCopy()},
		map[string]string{allocatedPod.Name: "sandbox-a"},
	))

	require.Equal(t, 0, countingClient.podPatchCalls())
}

func TestSyncPodSandboxLabelsBoundsWaitingGoroutines(t *testing.T) {
	const podCount = 200
	originalConcurrency := syncSandboxAllocConcurrency
	syncSandboxAllocConcurrency = 8
	t.Cleanup(func() { syncSandboxAllocConcurrency = originalConcurrency })

	ctx := context.Background()
	pods := make([]*corev1.Pod, 0, podCount)
	objects := make([]client.Object, 0, podCount)
	allocation := make(map[string]string, podCount)
	for i := 0; i < podCount; i++ {
		pod := testPoolPod(fmt.Sprintf("pool-pod-%03d", i), map[string]string{LabelPoolName: "pool"})
		pods = append(pods, pod.DeepCopy())
		objects = append(objects, pod)
		allocation[pod.Name] = fmt.Sprintf("sandbox-%03d", i)
	}

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	blockingClient := &blockingPodPatchClient{
		Client:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		started: make(chan struct{}, podCount),
		release: make(chan struct{}),
	}
	reconciler := &PoolReconciler{Client: blockingClient}
	done := make(chan error, 1)
	baseline := goruntime.NumGoroutine()
	go func() {
		done <- reconciler.syncPodSandboxLabels(ctx, pods, allocation)
	}()

	for i := 0; i < syncSandboxAllocConcurrency; i++ {
		select {
		case <-blockingClient.started:
		case <-time.After(time.Second):
			close(blockingClient.release)
			require.FailNow(t, "timed out waiting for bounded patch workers")
		}
	}
	waitingGoroutines := goruntime.NumGoroutine() - baseline
	close(blockingClient.release)
	require.NoError(t, <-done)
	require.Less(t, waitingGoroutines, 50, "label sync should not create one waiting goroutine per pod")
}

func testPoolPod(name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels}}
}

func newPodLabelTestReconciler(t *testing.T, pods ...*corev1.Pod) (*PoolReconciler, *podPatchCountingClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	objects := make([]client.Object, 0, len(pods))
	for _, pod := range pods {
		objects = append(objects, pod.DeepCopy())
	}
	countingClient := &podPatchCountingClient{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
	}
	return &PoolReconciler{Client: countingClient}, countingClient
}

func getPodLabelTestPod(t *testing.T, ctx context.Context, c client.Client, pod *corev1.Pod) *corev1.Pod {
	t.Helper()
	updated := &corev1.Pod{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, updated))
	return updated
}

type podPatchCountingClient struct {
	client.Client
	mu          sync.Mutex
	patchCalls  int
	onPodPatch  func()
	podPatchErr error
}

func (c *podPatchCountingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	err := c.podPatchErr
	if err == nil {
		err = c.Client.Patch(ctx, obj, patch, opts...)
	}
	if _, ok := obj.(*corev1.Pod); ok {
		c.mu.Lock()
		c.patchCalls++
		onPodPatch := c.onPodPatch
		c.mu.Unlock()
		if err == nil && onPodPatch != nil {
			onPodPatch()
		}
	}
	return err
}

type blockingPodPatchClient struct {
	client.Client
	started chan struct{}
	release chan struct{}
}

func (c *blockingPodPatchClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		c.started <- struct{}{}
		<-c.release
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *podPatchCountingClient) podPatchCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.patchCalls
}

type labelSyncOrder struct {
	mu    sync.Mutex
	items []string
}

func (o *labelSyncOrder) record(item string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.items = append(o.items, item)
}

func (o *labelSyncOrder) events() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.items...)
}
