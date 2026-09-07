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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/algorithm"
)

type failingScheduleAllocator struct {
	stubAllocator
	scheduleErr error
}

func (a *failingScheduleAllocator) Schedule(_ context.Context, _ *AllocSpec) (*algorithm.AllocAction, error) {
	return nil, a.scheduleErr
}

func newScheduleFailureTestReconciler(alloc Allocator, objs ...runtime.Object) *PoolReconciler {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = sandboxv1alpha1.AddToScheme(scheme)
	return &PoolReconciler{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build(),
		Scheme:    scheme,
		Recorder:  record.NewFakeRecorder(10),
		Allocator: alloc,
	}
}

func TestScheduleSandboxReturnsBestEffortResultOnFailure(t *testing.T) {
	ctx := context.Background()
	pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"}}
	pods := []*corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-allocated", Namespace: "default"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-idle", Namespace: "default"}},
	}
	alloc := &failingScheduleAllocator{
		stubAllocator: stubAllocator{podAllocation: map[string]string{"pod-allocated": "sandbox-a"}},
		scheduleErr:   errors.New("schedule exploded"),
	}
	r := newScheduleFailureTestReconciler(alloc)

	result, err := r.scheduleSandbox(ctx, pool, nil, pods)
	if err == nil {
		t.Fatal("expected schedule error, got nil")
	}
	if result == nil {
		t.Fatal("expected best-effort result, got nil")
	}
	if got := result.LatestAllocation["pod-allocated"]; got != "sandbox-a" {
		t.Fatalf("LatestAllocation[pod-allocated] = %q, want %q", got, "sandbox-a")
	}
	if len(result.IdlePods) != 1 || result.IdlePods[0] != "pod-idle" {
		t.Fatalf("IdlePods = %v, want [pod-idle]", result.IdlePods)
	}
	if result.SupplyCnt != 0 || len(result.ToDelete) != 0 {
		t.Fatalf("expected zero SupplyCnt/ToDelete on failure, got %+v", result)
	}
}

func TestBestEffortScheduleResultFallsBackToEmptyAllocation(t *testing.T) {
	ctx := context.Background()
	pool := &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"}}
	pods := []*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "pod-idle", Namespace: "default"}}}
	r := newScheduleFailureTestReconciler(&failingReadAllocator{})

	result := r.bestEffortScheduleResult(ctx, pool, pods)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.LatestAllocation) != 0 {
		t.Fatalf("LatestAllocation = %v, want empty", result.LatestAllocation)
	}
	if len(result.IdlePods) != 1 || result.IdlePods[0] != "pod-idle" {
		t.Fatalf("IdlePods = %v, want [pod-idle]", result.IdlePods)
	}
}

type failingReadAllocator struct {
	stubAllocator
}

func (a *failingReadAllocator) GetPoolAllocation(_ context.Context, _ *sandboxv1alpha1.Pool) (map[string]string, error) {
	return nil, errors.New("store unavailable")
}
