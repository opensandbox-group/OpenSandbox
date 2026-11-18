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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

// A Pool whose buffer died reports the finished pods as capacity: `total`
// stays at the old count and `available` cannot rise, which is exactly the
// status shape an operator sees when the pool stops refilling. Finished pods
// hold no capacity and must not count toward total/available/updated.
func TestUpdatePoolStatusCountsOnlyServiceablePods(t *testing.T) {
	now := time.Now()

	idleReady := stabilityTestPod("idle-ready", true, now)
	idleReady.Labels = map[string]string{labelPoolRevision: "rev-1"}

	allocatedReady := stabilityTestPod("allocated-ready", true, now)
	allocatedReady.Labels = map[string]string{labelPoolRevision: "rev-1"}

	evicted := stabilityTestPod("evicted", false, now)
	evicted.Status.Phase = corev1.PodFailed
	evicted.Status.Reason = "Evicted"
	evicted.Labels = map[string]string{labelPoolRevision: "rev-1"}

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

	pods := []*corev1.Pod{idleReady, allocatedReady, evicted, succeeded}
	allocation := map[string]string{allocatedReady.Name: "sandbox-a"}
	if err := r.updatePoolStatus(context.Background(), "rev-1", latest, pods, pods, allocation); err != nil {
		t.Fatal(err)
	}

	updated := &sandboxv1alpha1.Pool{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}, updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Total != 2 {
		t.Fatalf("Status.Total = %d, want 2 (finished pods hold no capacity)", updated.Status.Total)
	}
	if updated.Status.Updated != 2 {
		t.Fatalf("Status.Updated = %d, want 2 (only live pods on the current revision)", updated.Status.Updated)
	}
	if updated.Status.Available != 1 {
		t.Fatalf("Status.Available = %d, want 1 (a stale Ready condition on a finished pod is not availability)", updated.Status.Available)
	}
	if updated.Status.Allocated != 1 {
		t.Fatalf("Status.Allocated = %d, want 1", updated.Status.Allocated)
	}
}
