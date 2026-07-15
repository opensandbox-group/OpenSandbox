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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/algorithm"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/eviction"
)

type stubAllocator struct {
	podAllocation map[string]string
}

func (a *stubAllocator) Schedule(_ context.Context, _ *AllocSpec) (*algorithm.AllocAction, error) {
	return nil, nil
}
func (a *stubAllocator) GetPoolAllocation(_ context.Context, _ *sandboxv1alpha1.Pool) (map[string]string, error) {
	return a.podAllocation, nil
}
func (a *stubAllocator) ClearPoolAllocation(_ context.Context, _ string, _ string) error {
	return nil
}
func (a *stubAllocator) SyncSandboxAllocation(_ context.Context, _ *sandboxv1alpha1.BatchSandbox, _ []string) error {
	return nil
}
func (a *stubAllocator) SyncSandboxReleased(_ context.Context, _ *sandboxv1alpha1.BatchSandbox, _ []string) error {
	return nil
}
func (a *stubAllocator) GetSandboxAllocation(_ context.Context, _ *sandboxv1alpha1.BatchSandbox) ([]string, error) {
	return nil, nil
}
func (a *stubAllocator) GetSandboxReleased(_ context.Context, _ *sandboxv1alpha1.BatchSandbox) ([]string, error) {
	return nil, nil
}
func (a *stubAllocator) ReleasePodsAllocation(_ context.Context, _ string, _ string, _ []string) {}

func newEvictionTestPod(name string, labels map[string]string, deleting bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    labels,
		},
	}
	if deleting {
		now := metav1.NewTime(time.Now())
		pod.DeletionTimestamp = &now
		pod.Finalizers = []string{"test-finalizer"}
	}
	return pod
}

func newEvictionTestReconciler(podAllocation map[string]string, objs ...runtime.Object) *PoolReconciler {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = sandboxv1alpha1.AddToScheme(scheme)

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		if pod, ok := o.(*corev1.Pod); ok {
			p := *pod
			builder = builder.WithObjects(&p)
		}
	}
	c := builder.Build()

	return &PoolReconciler{
		Client:    c,
		Scheme:    scheme,
		Recorder:  record.NewFakeRecorder(10),
		Allocator: &stubAllocator{podAllocation: podAllocation},
	}
}

func TestHandleEviction(t *testing.T) {
	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default"},
	}
	ctx := context.Background()

	evictLabel := map[string]string{eviction.LabelEvict: ""}
	normalLabel := map[string]string{"app": "test"}

	t.Run("no eviction labels keeps all pods", func(t *testing.T) {
		pods := []*corev1.Pod{
			newEvictionTestPod("pod-1", normalLabel, false),
			newEvictionTestPod("pod-2", normalLabel, false),
		}
		r := newEvictionTestReconciler(map[string]string{}, pods[0], pods[1])

		got, err := r.handleEviction(ctx, pool, pods)
		if err != nil {
			t.Fatalf("handleEviction() returned error: %v", err)
		}
		expectNames := []string{"pod-1", "pod-2"}
		if len(got) != len(expectNames) {
			t.Fatalf("got %d pods, want %d", len(got), len(expectNames))
		}
		for i, pod := range got {
			if pod.Name != expectNames[i] {
				t.Errorf("pod[%d] = %s, want %s", i, pod.Name, expectNames[i])
			}
		}
	})

	t.Run("unallocated eviction-labeled pods are evicted and excluded", func(t *testing.T) {
		pod1 := newEvictionTestPod("pod-1", evictLabel, false)
		pod2 := newEvictionTestPod("pod-2", normalLabel, false)
		r := newEvictionTestReconciler(map[string]string{}, pod1, pod2)

		got, err := r.handleEviction(ctx, pool, []*corev1.Pod{pod1, pod2})
		if err != nil {
			t.Fatalf("handleEviction() returned error: %v", err)
		}
		if len(got) != 1 || got[0].Name != "pod-2" {
			t.Fatalf("got %v, want [pod-2]", got)
		}
		// pod-1 should be deleted
		check := &corev1.Pod{}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: "pod-1", Namespace: "default"}, check); err == nil {
			t.Error("expected pod-1 to be deleted")
		}
	})

	t.Run("allocated eviction-labeled pods are kept", func(t *testing.T) {
		pod1 := newEvictionTestPod("pod-1", evictLabel, false)
		pod2 := newEvictionTestPod("pod-2", normalLabel, false)
		alloc := map[string]string{"pod-1": "sandbox-1"}
		r := newEvictionTestReconciler(alloc, pod1, pod2)

		got, err := r.handleEviction(ctx, pool, []*corev1.Pod{pod1, pod2})
		if err != nil {
			t.Fatalf("handleEviction() returned error: %v", err)
		}
		expectNames := []string{"pod-1", "pod-2"}
		if len(got) != len(expectNames) {
			t.Fatalf("got %d pods, want %d", len(got), len(expectNames))
		}
		for i, pod := range got {
			if pod.Name != expectNames[i] {
				t.Errorf("pod[%d] = %s, want %s", i, pod.Name, expectNames[i])
			}
		}
	})

	t.Run("mix of allocated and unallocated eviction-labeled pods", func(t *testing.T) {
		pod1 := newEvictionTestPod("pod-1", evictLabel, false)
		pod2 := newEvictionTestPod("pod-2", evictLabel, false)
		pod3 := newEvictionTestPod("pod-3", normalLabel, false)
		alloc := map[string]string{"pod-1": "sandbox-1"}
		r := newEvictionTestReconciler(alloc, pod1, pod2, pod3)

		got, err := r.handleEviction(ctx, pool, []*corev1.Pod{pod1, pod2, pod3})
		if err != nil {
			t.Fatalf("handleEviction() returned error: %v", err)
		}
		expectNames := []string{"pod-1", "pod-3"}
		if len(got) != len(expectNames) {
			t.Fatalf("got %d pods, want %d", len(got), len(expectNames))
		}
		for i, pod := range got {
			if pod.Name != expectNames[i] {
				t.Errorf("pod[%d] = %s, want %s", i, pod.Name, expectNames[i])
			}
		}
	})

	t.Run("deleting pods with eviction label are not evicted", func(t *testing.T) {
		pod1 := newEvictionTestPod("pod-1", evictLabel, true)
		r := newEvictionTestReconciler(map[string]string{}, pod1)

		got, err := r.handleEviction(ctx, pool, []*corev1.Pod{pod1})
		if err != nil {
			t.Fatalf("handleEviction() returned error: %v", err)
		}
		if len(got) != 1 || got[0].Name != "pod-1" {
			t.Fatalf("expected deleting pod to be kept, got %v", got)
		}
	})

	t.Run("empty pod list", func(t *testing.T) {
		r := newEvictionTestReconciler(map[string]string{})

		got, err := r.handleEviction(ctx, pool, []*corev1.Pod{})
		if err != nil {
			t.Fatalf("handleEviction() returned error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expected empty result, got %d pods", len(got))
		}
	})

	t.Run("evicts multiple idle pods and keeps allocated ones", func(t *testing.T) {
		pod1 := newEvictionTestPod("pod-1", evictLabel, false)
		pod2 := newEvictionTestPod("pod-2", evictLabel, false)
		pod3 := newEvictionTestPod("pod-3", evictLabel, false)
		alloc := map[string]string{"pod-2": "sandbox-1"}
		r := newEvictionTestReconciler(alloc, pod1, pod2, pod3)

		got, err := r.handleEviction(ctx, pool, []*corev1.Pod{pod1, pod2, pod3})
		if err != nil {
			t.Fatalf("handleEviction() returned error: %v", err)
		}
		if len(got) != 1 || got[0].Name != "pod-2" {
			t.Fatalf("expected only pod-2, got %v", got)
		}

		check := &corev1.Pod{}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: "pod-1", Namespace: "default"}, check); err == nil {
			t.Error("expected pod-1 to be deleted")
		}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: "pod-2", Namespace: "default"}, check); err != nil {
			t.Error("expected allocated pod-2 to still exist")
		}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: "pod-3", Namespace: "default"}, check); err == nil {
			t.Error("expected pod-3 to be deleted")
		}
	})
}
