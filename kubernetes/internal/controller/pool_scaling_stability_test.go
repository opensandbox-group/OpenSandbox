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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/algorithm"
	controllerutils "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/controller"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/expectations"
)

func stabilityTestPod(name string, ready bool, created time.Time) *corev1.Pod {
	status := corev1.ConditionFalse
	phase := corev1.PodPending
	if ready {
		status = corev1.ConditionTrue
		phase = corev1.PodRunning
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: corev1.PodStatus{Phase: phase, Conditions: []corev1.PodCondition{{
			Type:   corev1.PodReady,
			Status: status,
		}}},
	}
}

func stabilityTestPool(maxUnavailable intstr.IntOrString) *sandboxv1alpha1.Pool {
	return &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "pool-uid"},
		Spec: sandboxv1alpha1.PoolSpec{
			CapacitySpec: sandboxv1alpha1.CapacitySpec{
				PoolMin: 0, PoolMax: 100, BufferMin: 0, BufferMax: 0,
			},
			ScaleStrategy: &sandboxv1alpha1.ScaleStrategy{MaxUnavailable: &maxUnavailable},
		},
	}
}

func stabilityTestReconciler(t *testing.T, objects ...client.Object) *PoolReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &PoolReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(objects...).
			WithStatusSubresource(&sandboxv1alpha1.Pool{}).
			Build(),
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(20),
	}
}

func TestCountReadyIdlePods(t *testing.T) {
	now := time.Now()
	pods := []*corev1.Pod{
		stabilityTestPod("ready-idle", true, now),
		stabilityTestPod("pending-idle", false, now),
		stabilityTestPod("ready-allocated", true, now),
	}
	r := &PoolReconciler{}
	if got := r.countReadyIdlePods(pods, []string{"ready-idle", "pending-idle"}); got != 1 {
		t.Fatalf("countReadyIdlePods() = %d, want 1", got)
	}
}

func TestDesiredBufferCountUsesBandEdges(t *testing.T) {
	tests := []struct {
		name  string
		ready int32
		min   int32
		max   int32
		want  int32
	}{
		{name: "below minimum", ready: 0, min: 20, max: 100, want: 20},
		{name: "at minimum", ready: 20, min: 20, max: 100, want: 20},
		{name: "inside band", ready: 89, min: 20, max: 100, want: 89},
		{name: "at maximum", ready: 100, min: 20, max: 100, want: 100},
		{name: "above maximum", ready: 120, min: 20, max: 100, want: 100},
		{name: "zero capacity", ready: 0, min: 0, max: 0, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := desiredBufferCount(tt.ready, tt.min, tt.max); got != tt.want {
				t.Fatalf("desiredBufferCount(%d, %d, %d) = %d, want %d", tt.ready, tt.min, tt.max, got, tt.want)
			}
		})
	}
}

func TestPickPodsToDeleteLeastUsefulFirst(t *testing.T) {
	now := time.Now()
	pendingOld := stabilityTestPod("pending-old", false, now.Add(-4*time.Minute))
	pendingNew := stabilityTestPod("pending-new", false, now.Add(-time.Minute))
	readyOld := stabilityTestPod("ready-old", true, now.Add(-5*time.Minute))
	readyNew := stabilityTestPod("ready-new", true, now.Add(-2*time.Minute))
	terminating := stabilityTestPod("terminating", false, now)
	deletionTime := metav1.NewTime(now)
	terminating.DeletionTimestamp = &deletionTime

	r := &PoolReconciler{}
	got := r.pickPodsToDelete(
		[]*corev1.Pod{pendingOld, pendingNew, readyOld, readyNew, terminating},
		[]string{pendingOld.Name, pendingNew.Name, readyOld.Name, readyNew.Name, terminating.Name},
		[]string{readyNew.Name, readyNew.Name, terminating.Name},
		3,
	)

	want := []string{"ready-new", "pending-new", "pending-old", "ready-old"}
	if len(got) != len(want) {
		t.Fatalf("pickPodsToDelete() returned %d pods, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Name != want[i] {
			t.Errorf("pod[%d] = %q, want %q", i, got[i].Name, want[i])
		}
	}
}

func TestScalePoolPendingPodsCoverDesiredCapacity(t *testing.T) {
	PoolScaleExpectations = expectations.NewScaleExpectations()
	t.Cleanup(func() { PoolScaleExpectations = expectations.NewScaleExpectations() })

	now := time.Now()
	pods := []*corev1.Pod{
		stabilityTestPod("pending-1", false, now),
		stabilityTestPod("pending-2", false, now),
		stabilityTestPod("pending-3", false, now),
		stabilityTestPod("pending-4", false, now),
	}
	objects := make([]client.Object, 0, len(pods))
	idleNames := make([]string, 0, len(pods))
	for _, pod := range pods {
		objects = append(objects, pod)
		idleNames = append(idleNames, pod.Name)
	}
	r := stabilityTestReconciler(t, objects...)
	pool := stabilityTestPool(intstr.FromString("100%"))
	pool.Spec.CapacitySpec.BufferMin = 2
	pool.Spec.CapacitySpec.BufferMax = 4

	pending, err := r.scalePool(context.Background(), pool, &scaleArgs{
		pods: pods, totalPodCnt: 4, idlePods: idleNames, supplyCnt: 2,
	})
	if err != nil || pending {
		t.Fatalf("scalePool() = pending %v, error %v", pending, err)
	}

	var podList corev1.PodList
	if err := r.List(context.Background(), &podList); err != nil {
		t.Fatal(err)
	}
	if len(podList.Items) != 4 {
		t.Fatalf("pod count = %d, want 4; pending capacity should prevent repeated creation", len(podList.Items))
	}
}

func TestScalePoolCountsTerminatingPodsAgainstPoolMax(t *testing.T) {
	PoolScaleExpectations = expectations.NewScaleExpectations()
	t.Cleanup(func() { PoolScaleExpectations = expectations.NewScaleExpectations() })

	now := time.Now()
	active := []*corev1.Pod{
		stabilityTestPod("pending-1", false, now),
		stabilityTestPod("pending-2", false, now),
	}
	r := stabilityTestReconciler(t, active[0], active[1])
	pool := stabilityTestPool(intstr.FromString("100%"))
	pool.Spec.CapacitySpec.PoolMax = 3

	_, err := r.scalePool(context.Background(), pool, &scaleArgs{
		pods: active, totalPodCnt: 3, idlePods: []string{"pending-1", "pending-2"}, supplyCnt: 5,
	})
	if err != nil {
		t.Fatal(err)
	}

	var podList corev1.PodList
	if err := r.List(context.Background(), &podList); err != nil {
		t.Fatal(err)
	}
	if len(podList.Items) != 2 {
		t.Fatalf("active pod count = %d, want 2; terminating occupancy must block creation", len(podList.Items))
	}
}

func TestScalePoolLimitsDeleteBatch(t *testing.T) {
	tests := []struct {
		name           string
		maxUnavailable intstr.IntOrString
		poolMin        int32
		wantDeleted    int
	}{
		{name: "integer", maxUnavailable: intstr.FromInt(2), poolMin: 0, wantDeleted: 2},
		{name: "percentage", maxUnavailable: intstr.FromString("40%"), poolMin: 5, wantDeleted: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			PoolScaleExpectations = expectations.NewScaleExpectations()
			t.Cleanup(func() { PoolScaleExpectations = expectations.NewScaleExpectations() })

			now := time.Now()
			pods := make([]*corev1.Pod, 0, 10)
			objects := make([]client.Object, 0, 10)
			idleNames := make([]string, 0, 10)
			for i := 0; i < 10; i++ {
				pod := stabilityTestPod("pod-"+string(rune('a'+i)), false, now.Add(time.Duration(i)*time.Second))
				pods = append(pods, pod)
				objects = append(objects, pod)
				idleNames = append(idleNames, pod.Name)
			}
			r := stabilityTestReconciler(t, objects...)
			pool := stabilityTestPool(tt.maxUnavailable)
			pool.Spec.CapacitySpec.PoolMin = tt.poolMin

			_, err := r.scalePool(context.Background(), pool, &scaleArgs{
				pods: pods, totalPodCnt: 10, idlePods: idleNames,
			})
			if err != nil {
				t.Fatal(err)
			}
			deleteExpectations := PoolScaleExpectations.GetExpectations(controllerutils.GetControllerKey(pool))[expectations.Delete]
			if deleteExpectations.Len() != tt.wantDeleted {
				t.Fatalf("delete expectations = %d, want %d", deleteExpectations.Len(), tt.wantDeleted)
			}
		})
	}
}

func TestObserveDeletedPodsCompletesDeleteBatch(t *testing.T) {
	PoolScaleExpectations = expectations.NewScaleExpectations()
	t.Cleanup(func() { PoolScaleExpectations = expectations.NewScaleExpectations() })
	controllerKey := "default/pool"
	PoolScaleExpectations.ExpectScale(controllerKey, expectations.Delete, "gone")
	PoolScaleExpectations.ExpectScale(controllerKey, expectations.Delete, "terminating")

	observeDeletedPods(controllerKey, map[string]struct{}{"terminating": {}})
	dirty := PoolScaleExpectations.GetExpectations(controllerKey)[expectations.Delete]
	if dirty.Has("gone") || !dirty.Has("terminating") {
		t.Fatalf("unexpected remaining delete expectations: %v", dirty.List())
	}

	observeDeletedPods(controllerKey, map[string]struct{}{})
	if satisfied, _, _ := PoolScaleExpectations.SatisfiedExpectations(controllerKey); !satisfied {
		t.Fatal("expected delete batch to be satisfied after all pods disappear")
	}
}

type deleteFailingClient struct {
	client.Client
}

func (c *deleteFailingClient) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	return errors.New("delete failed")
}

func TestScalePoolRollsBackExpectationOnDeleteFailure(t *testing.T) {
	PoolScaleExpectations = expectations.NewScaleExpectations()
	t.Cleanup(func() { PoolScaleExpectations = expectations.NewScaleExpectations() })

	pod := stabilityTestPod("pod", false, time.Now())
	r := stabilityTestReconciler(t, pod)
	r.Client = &deleteFailingClient{Client: r.Client}
	pool := stabilityTestPool(intstr.FromInt(1))

	_, err := r.scalePool(context.Background(), pool, &scaleArgs{
		pods: []*corev1.Pod{pod}, totalPodCnt: 1, idlePods: []string{pod.Name},
	})
	if err == nil {
		t.Fatal("expected delete error")
	}
	if satisfied, _, dirty := PoolScaleExpectations.SatisfiedExpectations(controllerutils.GetControllerKey(pool)); !satisfied {
		t.Fatalf("delete failure left stale expectation: %v", dirty)
	}
}

func TestScalePoolUnsatisfiedExpectationIsNotAnError(t *testing.T) {
	PoolScaleExpectations = expectations.NewScaleExpectations()
	t.Cleanup(func() { PoolScaleExpectations = expectations.NewScaleExpectations() })
	pool := stabilityTestPool(intstr.FromInt(1))
	PoolScaleExpectations.ExpectScale(controllerutils.GetControllerKey(pool), expectations.Delete, "terminating")

	r := stabilityTestReconciler(t)
	pending, err := r.scalePool(context.Background(), pool, &scaleArgs{})
	if err != nil || !pending {
		t.Fatalf("scalePool() = pending %v, error %v; want a non-error retry", pending, err)
	}
}

type stabilityAllocator struct {
	*stubAllocator
}

func (a *stabilityAllocator) Schedule(context.Context, *AllocSpec) (*algorithm.AllocAction, error) {
	return &algorithm.AllocAction{}, nil
}

func TestReconcilePoolUpdatesStatusWhileScaleIsPending(t *testing.T) {
	PoolScaleExpectations = expectations.NewScaleExpectations()
	t.Cleanup(func() { PoolScaleExpectations = expectations.NewScaleExpectations() })

	pool := stabilityTestPool(intstr.FromInt(1))
	pool.Generation = 7
	r := stabilityTestReconciler(t, pool)
	r.Allocator = &stabilityAllocator{stubAllocator: &stubAllocator{podAllocation: map[string]string{}}}
	PoolScaleExpectations.ExpectScale(controllerutils.GetControllerKey(pool), expectations.Delete, "terminating")

	result, err := r.reconcilePool(context.Background(), pool, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != defaultRetryTime {
		t.Fatalf("RequeueAfter = %v, want %v", result.RequeueAfter, defaultRetryTime)
	}

	updated := &sandboxv1alpha1.Pool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.ObservedGeneration != pool.Generation {
		t.Fatalf("ObservedGeneration = %d, want %d", updated.Status.ObservedGeneration, pool.Generation)
	}
}
