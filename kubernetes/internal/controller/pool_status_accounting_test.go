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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

// A Pool whose buffer died reports the finished pods as capacity: `total`
// stays at the old count and `available` cannot rise, which is exactly the
// status shape an operator sees when the pool stops refilling. Finished idle
// pods hold no capacity and must not count toward total/available/updated. A
// finished pod that is still allocated stays counted — it is owed to its
// sandbox until handback — so `allocated / total` remains a utilization
// fraction.
func TestUpdatePoolStatusExcludesFinishedIdlePodsButKeepsAllocatedOnes(t *testing.T) {
	now := time.Now()

	idleReady := stabilityTestPod("idle-ready", true, now)
	idleReady.Labels = map[string]string{labelPoolRevision: "rev-1"}

	allocatedReady := stabilityTestPod("allocated-ready", true, now)
	allocatedReady.Labels = map[string]string{labelPoolRevision: "rev-1"}

	evictedAllocated := stabilityTestPod("evicted-allocated", false, now)
	evictedAllocated.Status.Phase = corev1.PodFailed
	evictedAllocated.Status.Reason = "Evicted"
	evictedAllocated.Labels = map[string]string{labelPoolRevision: "rev-1"}

	evictedIdle := stabilityTestPod("evicted-idle", false, now)
	evictedIdle.Status.Phase = corev1.PodFailed
	evictedIdle.Status.Reason = "Evicted"
	evictedIdle.Labels = map[string]string{labelPoolRevision: "rev-1"}

	// A completed pod can keep a stale Ready condition; its phase still says
	// it can never serve another sandbox.
	succeeded := stabilityTestPod("succeeded", true, now)
	succeeded.Status.Phase = corev1.PodSucceeded

	pool := stabilityTestPool(intstr.FromInt(1))
	pool.Generation = 7
	pool.Spec.CapacitySpec.BufferMin = 1
	pool.Spec.CapacitySpec.BufferMax = 1

	r := stabilityTestReconciler(t, pool.DeepCopy())
	latest := &sandboxv1alpha1.Pool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), latest); err != nil {
		t.Fatal(err)
	}

	pods := []*corev1.Pod{idleReady, allocatedReady, evictedAllocated, evictedIdle, succeeded}
	allocation := map[string]string{
		allocatedReady.Name:   "sandbox-a",
		evictedAllocated.Name: "sandbox-b",
	}
	if err := r.updatePoolStatus(context.Background(), "rev-1", latest, pods, pods, allocation); err != nil {
		t.Fatal(err)
	}

	updated := &sandboxv1alpha1.Pool{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}, updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Total != 3 {
		t.Fatalf("Status.Total = %d, want 3 (live pods plus the finished pod still allocated)", updated.Status.Total)
	}
	if updated.Status.Updated != 3 {
		t.Fatalf("Status.Updated = %d, want 3 (same pod population as Total)", updated.Status.Updated)
	}
	if updated.Status.Available != 1 {
		t.Fatalf("Status.Available = %d, want 1 (finished and allocated pods are not availability)", updated.Status.Available)
	}
	if updated.Status.Allocated != 2 {
		t.Fatalf("Status.Allocated = %d, want 2", updated.Status.Allocated)
	}
	if updated.Status.Total < updated.Status.Allocated {
		t.Fatalf("Status.Total (%d) < Status.Allocated (%d); allocated/total would exceed 1",
			updated.Status.Total, updated.Status.Allocated)
	}
}

// Graceful termination leaves a pod with a DeletionTimestamp while its phase
// is still Running, and the reconcile loop pre-filters such pods out of the
// list handed to updatePoolStatus. The allocator keeps the reservation until
// handback, so the pod must stay counted in Total (but not Updated) until it
// is released — otherwise Allocated could exceed Total and push the rebalance
// scorer's Allocated/Total ratio above 1 through the termination window.
func TestUpdatePoolStatusKeepsTerminatingAllocatedPodsInTotal(t *testing.T) {
	now := time.Now()

	live := stabilityTestPod("live-ready", true, now)
	live.Labels = map[string]string{labelPoolRevision: "rev-1"}

	terminatingAllocated := stabilityTestPod("terminating-allocated", true, now)
	terminatingAllocated.Labels = map[string]string{labelPoolRevision: "rev-1"}
	deletionTimestamp := metav1.Now()
	terminatingAllocated.DeletionTimestamp = &deletionTimestamp

	pool := stabilityTestPool(intstr.FromInt(1))
	pool.Generation = 7
	pool.Spec.CapacitySpec.BufferMin = 1
	pool.Spec.CapacitySpec.BufferMax = 1

	r := stabilityTestReconciler(t, pool.DeepCopy())
	latest := &sandboxv1alpha1.Pool{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pool), latest); err != nil {
		t.Fatal(err)
	}

	// The reconcile loop drops the terminating pod before calling
	// updatePoolStatus, so it is absent from the slice here.
	pods := []*corev1.Pod{live}
	allocation := map[string]string{
		live.Name:                 "sandbox-a",
		terminatingAllocated.Name: "sandbox-b",
	}
	if err := r.updatePoolStatus(context.Background(), "rev-1", latest, pods, pods, allocation); err != nil {
		t.Fatal(err)
	}

	updated := &sandboxv1alpha1.Pool{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}, updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Total != 2 {
		t.Fatalf("Status.Total = %d, want 2 (live pod plus the terminating pod still allocated)", updated.Status.Total)
	}
	if updated.Status.Updated != 1 {
		t.Fatalf("Status.Updated = %d, want 1 (the terminating pod no longer counts as up-to-date capacity)", updated.Status.Updated)
	}
	if updated.Status.Allocated != 2 {
		t.Fatalf("Status.Allocated = %d, want 2", updated.Status.Allocated)
	}
	if updated.Status.Total < updated.Status.Allocated {
		t.Fatalf("Status.Total (%d) < Status.Allocated (%d); allocated/total would exceed 1",
			updated.Status.Total, updated.Status.Allocated)
	}
}
