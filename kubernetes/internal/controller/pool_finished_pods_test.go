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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/expectations"
)

func finishedTestPool(t *testing.T, maxUnavailable intstr.IntOrString) *sandboxv1alpha1.Pool {
	t.Helper()
	pool := stabilityTestPool(maxUnavailable)
	pool.Spec.Template = &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "nginx"}}},
	}
	return pool
}

func finishedTestPod(name string, phase corev1.PodPhase, created time.Time) *corev1.Pod {
	pod := stabilityTestPod(name, false, created)
	pod.Status.Phase = phase
	pod.Status.Reason = "Evicted"
	pod.Status.Conditions = nil
	return pod
}

func TestIsTerminalPod(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{name: "running", pod: stabilityTestPod("running", true, now), want: false},
		{name: "pending", pod: stabilityTestPod("pending", false, now), want: false},
		{name: "evicted", pod: finishedTestPod("evicted", corev1.PodFailed, now), want: true},
		{name: "completed", pod: finishedTestPod("completed", corev1.PodSucceeded, now), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTerminalPod(tt.pod); got != tt.want {
				t.Fatalf("isTerminalPod(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// TestScalePoolEvictedPodsDoNotConsumeScaleUpBudget covers #2650: a pool whose
// whole buffer was evicted must refill instead of stalling forever, because
// finished pods no longer count against the scale-up budget.
func TestScalePoolEvictedPodsDoNotConsumeScaleUpBudget(t *testing.T) {
	poolScaleExpectations = expectations.NewScaleExpectations()
	t.Cleanup(func() { poolScaleExpectations = expectations.NewScaleExpectations() })

	now := time.Now()
	pods := []*corev1.Pod{
		finishedTestPod("evicted-1", corev1.PodFailed, now.Add(-3*time.Minute)),
		finishedTestPod("evicted-2", corev1.PodFailed, now.Add(-2*time.Minute)),
		finishedTestPod("completed-1", corev1.PodSucceeded, now.Add(-time.Minute)),
	}
	objects := make([]client.Object, 0, len(pods))
	idleNames := make([]string, 0, len(pods))
	for _, pod := range pods {
		objects = append(objects, pod)
		idleNames = append(idleNames, pod.Name)
	}
	r := stabilityTestReconciler(t, objects...)
	pool := finishedTestPool(t, intstr.FromString("25%"))
	pool.Spec.CapacitySpec.BufferMin = 3
	pool.Spec.CapacitySpec.BufferMax = 3

	if _, err := r.scalePool(context.Background(), pool, &scaleArgs{
		pods: pods, totalPodCnt: int32(len(pods)), idlePods: idleNames,
	}); err != nil {
		t.Fatal(err)
	}

	created := 0
	var podList corev1.PodList
	if err := r.List(context.Background(), &podList); err != nil {
		t.Fatal(err)
	}
	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded {
			created++
		}
	}
	if created == 0 {
		t.Fatal("pool created no replacement pod; finished pods still spend the scale-up budget")
	}
}

// TestScalePoolReplacesFinishedBufferPods checks the whole buffer is rebuilt
// once the finished pods are gone, and that their slots count against PoolMax
// instead of shrinking the room for replacements.
func TestScalePoolReplacesFinishedBufferPods(t *testing.T) {
	poolScaleExpectations = expectations.NewScaleExpectations()
	t.Cleanup(func() { poolScaleExpectations = expectations.NewScaleExpectations() })

	now := time.Now()
	pods := []*corev1.Pod{
		finishedTestPod("evicted-1", corev1.PodFailed, now.Add(-3*time.Minute)),
		finishedTestPod("evicted-2", corev1.PodFailed, now.Add(-2*time.Minute)),
		finishedTestPod("completed-1", corev1.PodSucceeded, now.Add(-time.Minute)),
	}
	objects := make([]client.Object, 0, len(pods))
	idleNames := make([]string, 0, len(pods))
	for _, pod := range pods {
		objects = append(objects, pod)
		idleNames = append(idleNames, pod.Name)
	}
	r := stabilityTestReconciler(t, objects...)
	pool := finishedTestPool(t, intstr.FromString("100%"))
	pool.Spec.CapacitySpec.PoolMax = 3
	pool.Spec.CapacitySpec.BufferMin = 3
	pool.Spec.CapacitySpec.BufferMax = 3

	if _, err := r.scalePool(context.Background(), pool, &scaleArgs{
		pods: pods, totalPodCnt: int32(len(pods)), idlePods: idleNames,
	}); err != nil {
		t.Fatal(err)
	}

	var podList corev1.PodList
	if err := r.List(context.Background(), &podList); err != nil {
		t.Fatal(err)
	}
	if len(podList.Items) != int(pool.Spec.CapacitySpec.BufferMin) {
		t.Fatalf("pool pod count = %d, want %d", len(podList.Items), pool.Spec.CapacitySpec.BufferMin)
	}
}

// TestScalePoolRecyclesFinishedPodsOnly keeps healthy idle pods in place: a
// finished pod is deleted, but a satisfied buffer is not rebuilt.
func TestScalePoolRecyclesFinishedPodsOnly(t *testing.T) {
	poolScaleExpectations = expectations.NewScaleExpectations()
	t.Cleanup(func() { poolScaleExpectations = expectations.NewScaleExpectations() })

	now := time.Now()
	live := []*corev1.Pod{
		stabilityTestPod("ready-1", true, now.Add(-2*time.Minute)),
		stabilityTestPod("ready-2", true, now.Add(-time.Minute)),
	}
	evicted := finishedTestPod("evicted-1", corev1.PodFailed, now)
	pods := []*corev1.Pod{live[0], live[1], evicted}
	r := stabilityTestReconciler(t, live[0], live[1], evicted)
	pool := finishedTestPool(t, intstr.FromString("100%"))
	pool.Spec.CapacitySpec.BufferMin = 2
	pool.Spec.CapacitySpec.BufferMax = 2

	if _, err := r.scalePool(context.Background(), pool, &scaleArgs{
		pods: pods, totalPodCnt: int32(len(pods)), idlePods: []string{"ready-1", "ready-2", "evicted-1"},
	}); err != nil {
		t.Fatal(err)
	}

	var podList corev1.PodList
	if err := r.List(context.Background(), &podList); err != nil {
		t.Fatal(err)
	}
	if len(podList.Items) != len(live) {
		t.Fatalf("pool pod count = %d, want %d; a satisfied buffer must not be rebuilt", len(podList.Items), len(live))
	}
	for _, pod := range podList.Items {
		if pod.Name == evicted.Name {
			t.Fatalf("finished pod %q was kept", pod.Name)
		}
	}
}

// TestScalePoolLeavesAllocatedFinishedPodsAlone keeps the reaping to the idle
// buffer: a finished pod that is still allocated to a sandbox keeps its slot
// until the sandbox hands it back, so deleting it here cannot strand an
// allocation the sandbox still points at.
func TestScalePoolLeavesAllocatedFinishedPodsAlone(t *testing.T) {
	poolScaleExpectations = expectations.NewScaleExpectations()
	t.Cleanup(func() { poolScaleExpectations = expectations.NewScaleExpectations() })

	now := time.Now()
	allocated := finishedTestPod("allocated-1", corev1.PodFailed, now.Add(-2*time.Minute))
	ready := stabilityTestPod("ready-1", true, now.Add(-time.Minute))
	pods := []*corev1.Pod{allocated, ready}
	r := stabilityTestReconciler(t, allocated, ready)
	pool := finishedTestPool(t, intstr.FromString("100%"))
	pool.Spec.CapacitySpec.BufferMin = 1
	pool.Spec.CapacitySpec.BufferMax = 1

	if _, err := r.scalePool(context.Background(), pool, &scaleArgs{
		pods: pods, totalPodCnt: int32(len(pods)), allocatedCnt: 1, idlePods: []string{"ready-1"},
	}); err != nil {
		t.Fatal(err)
	}

	var podList corev1.PodList
	if err := r.List(context.Background(), &podList); err != nil {
		t.Fatal(err)
	}
	if len(podList.Items) != len(pods) {
		t.Fatalf("pool pod count = %d, want %d; an allocated pod belongs to its sandbox", len(podList.Items), len(pods))
	}
}

// TestScalePoolRetiresFailedPodOnce checks a finished pod the rolling update
// already replaced through its own supply is not replaced a second time.
func TestScalePoolRetiresFailedPodOnce(t *testing.T) {
	poolScaleExpectations = expectations.NewScaleExpectations()
	t.Cleanup(func() { poolScaleExpectations = expectations.NewScaleExpectations() })

	now := time.Now()
	failed := finishedTestPod("failed-1", corev1.PodFailed, now.Add(-3*time.Minute))
	first := stabilityTestPod("ready-1", true, now.Add(-2*time.Minute))
	second := stabilityTestPod("ready-2", true, now.Add(-time.Minute))
	pods := []*corev1.Pod{failed, first, second}
	r := stabilityTestReconciler(t, failed, first, second)
	pool := finishedTestPool(t, intstr.FromString("100%"))
	pool.Spec.CapacitySpec.BufferMin = 2
	pool.Spec.CapacitySpec.BufferMax = 2

	if _, err := r.scalePool(context.Background(), pool, &scaleArgs{
		pods: pods, totalPodCnt: int32(len(pods)), supplyCnt: 1,
		idlePods: []string{"ready-1", "ready-2"}, toDeletePods: []string{"failed-1"},
	}); err != nil {
		t.Fatal(err)
	}

	var podList corev1.PodList
	if err := r.List(context.Background(), &podList); err != nil {
		t.Fatal(err)
	}
	if len(podList.Items) != 2 {
		t.Fatalf("pool pod count = %d, want 2; the update already supplies the replacement", len(podList.Items))
	}
}

// TestScalePoolDeletesFinishedPodsOutsideTheUnavailableBudget pins the delete
// budget: maxUnavailable throttles the pods that still serve traffic, while a
// finished pod is deleted in the pass that reaps it.
func TestScalePoolDeletesFinishedPodsOutsideTheUnavailableBudget(t *testing.T) {
	poolScaleExpectations = expectations.NewScaleExpectations()
	t.Cleanup(func() { poolScaleExpectations = expectations.NewScaleExpectations() })

	now := time.Now()
	live := []*corev1.Pod{
		stabilityTestPod("ready-1", true, now.Add(-4*time.Minute)),
		stabilityTestPod("ready-2", true, now.Add(-3*time.Minute)),
		stabilityTestPod("ready-3", true, now.Add(-2*time.Minute)),
	}
	evicted := finishedTestPod("evicted-1", corev1.PodFailed, now.Add(-time.Minute))
	pods := []*corev1.Pod{live[0], live[1], live[2], evicted}
	r := stabilityTestReconciler(t, live[0], live[1], live[2], evicted)
	// 25% of a 3-pod buffer rounds up to 1, so a budget shared with the finished
	// pod would leave it in place on this pass.
	pool := finishedTestPool(t, intstr.FromString("25%"))
	pool.Spec.CapacitySpec.BufferMin = 3
	pool.Spec.CapacitySpec.BufferMax = 3

	if _, err := r.scalePool(context.Background(), pool, &scaleArgs{
		pods: pods, totalPodCnt: int32(len(pods)), idlePods: []string{"ready-1", "ready-2", "ready-3", "evicted-1"},
	}); err != nil {
		t.Fatal(err)
	}

	var podList corev1.PodList
	if err := r.List(context.Background(), &podList); err != nil {
		t.Fatal(err)
	}
	for _, pod := range podList.Items {
		if pod.Name == evicted.Name {
			t.Fatalf("finished pod %q survived a pass that reaped it", pod.Name)
		}
	}
	if len(podList.Items) != len(live) {
		t.Fatalf("pool pod count = %d, want %d; the buffer is already satisfied", len(podList.Items), len(live))
	}
}

// TestScalePoolPoolMaxCountsNonTerminalPods covers the PoolMax semantics: a
// finished pod holds no slot, so the replacements for a dead buffer fit under
// PoolMax even though the dead pods are still listed in this pass.
func TestScalePoolPoolMaxCountsNonTerminalPods(t *testing.T) {
	poolScaleExpectations = expectations.NewScaleExpectations()
	t.Cleanup(func() { poolScaleExpectations = expectations.NewScaleExpectations() })

	now := time.Now()
	pods := []*corev1.Pod{
		finishedTestPod("evicted-1", corev1.PodFailed, now.Add(-3*time.Minute)),
		finishedTestPod("evicted-2", corev1.PodFailed, now.Add(-2*time.Minute)),
		finishedTestPod("completed-1", corev1.PodSucceeded, now.Add(-time.Minute)),
	}
	objects := make([]client.Object, 0, len(pods))
	idleNames := make([]string, 0, len(pods))
	for _, pod := range pods {
		objects = append(objects, pod)
		idleNames = append(idleNames, pod.Name)
	}
	r := stabilityTestReconciler(t, objects...)
	pool := finishedTestPool(t, intstr.FromString("100%"))
	pool.Spec.CapacitySpec.PoolMax = 3
	pool.Spec.CapacitySpec.BufferMin = 3
	pool.Spec.CapacitySpec.BufferMax = 3

	if _, err := r.scalePool(context.Background(), pool, &scaleArgs{
		pods: pods, totalPodCnt: int32(len(pods)), idlePods: idleNames,
	}); err != nil {
		t.Fatal(err)
	}

	var podList corev1.PodList
	if err := r.List(context.Background(), &podList); err != nil {
		t.Fatal(err)
	}
	if len(podList.Items) != 3 {
		t.Fatalf("pool pod count = %d, want 3", len(podList.Items))
	}
	for _, pod := range podList.Items {
		if isTerminalPod(&pod) {
			t.Fatalf("pod %q finished but was kept", pod.Name)
		}
	}
}
