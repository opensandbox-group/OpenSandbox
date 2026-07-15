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

package eviction

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func newTestPod(name string, labels map[string]string, deleting bool) *corev1.Pod {
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

func TestDefaultEvictionHandler_NeedsEviction(t *testing.T) {
	handler := newDefaultEvictionHandler(nil)

	tests := []struct {
		name   string
		pod    *corev1.Pod
		expect bool
	}{
		{
			name:   "pod with eviction label",
			pod:    newTestPod("pod-1", map[string]string{LabelEvict: ""}, false),
			expect: true,
		},
		{
			name:   "pod without eviction label",
			pod:    newTestPod("pod-2", map[string]string{"other": "label"}, false),
			expect: false,
		},
		{
			name:   "pod with no labels",
			pod:    newTestPod("pod-3", nil, false),
			expect: false,
		},
		{
			name:   "pod with eviction label but already deleting",
			pod:    newTestPod("pod-4", map[string]string{LabelEvict: ""}, true),
			expect: false,
		},
		{
			name:   "pod deleting without eviction label",
			pod:    newTestPod("pod-5", nil, true),
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := handler.NeedsEviction(tt.pod)
			if got != tt.expect {
				t.Errorf("NeedsEviction() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestDefaultEvictionHandler_Evict(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	t.Run("deletes a pod", func(t *testing.T) {
		pod := newTestPod("pod-1", map[string]string{LabelEvict: ""}, false)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
		handler := newDefaultEvictionHandler(c)

		err := handler.Evict(context.Background(), pod)
		if err != nil {
			t.Fatalf("Evict() returned error: %v", err)
		}

		got := &corev1.Pod{}
		err = c.Get(context.Background(), keyFor(pod), got)
		if err == nil {
			t.Error("expected pod to be deleted, but it still exists")
		}
	})

	t.Run("skips pod already deleting", func(t *testing.T) {
		pod := newTestPod("pod-2", map[string]string{LabelEvict: ""}, true)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
		handler := newDefaultEvictionHandler(c)

		err := handler.Evict(context.Background(), pod)
		if err != nil {
			t.Fatalf("Evict() returned error: %v", err)
		}

		got := &corev1.Pod{}
		err = c.Get(context.Background(), keyFor(pod), got)
		if err != nil {
			t.Error("expected pod to still exist since it was already deleting")
		}
	})
}

func TestNewEvictionHandler(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = sandboxv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	t.Run("returns default handler when no label", func(t *testing.T) {
		pool := &sandboxv1alpha1.Pool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-1"},
		}
		h := NewEvictionHandler(ctx, c, pool)
		if h == nil {
			t.Fatal("expected non-nil handler")
		}
		if _, ok := h.(*defaultEvictionHandler); !ok {
			t.Errorf("expected *defaultEvictionHandler, got %T", h)
		}
	})

	t.Run("returns default handler for unknown label value", func(t *testing.T) {
		pool := &sandboxv1alpha1.Pool{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "pool-2",
				Labels: map[string]string{LabelEvictionHandler: "unknown-handler"},
			},
		}
		h := NewEvictionHandler(ctx, c, pool)
		if _, ok := h.(*defaultEvictionHandler); !ok {
			t.Errorf("expected *defaultEvictionHandler, got %T", h)
		}
	})
}

func keyFor(obj metav1.Object) types.NamespacedName {
	return types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
}
