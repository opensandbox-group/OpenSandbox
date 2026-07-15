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

package assign

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func makeSBX(name string, image string) *sandboxv1alpha1.BatchSandbox {
	return &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Image: image}},
				},
			},
		},
	}
}

func makePool(name string, image string, total, allocated int32) *sandboxv1alpha1.Pool {
	return &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: sandboxv1alpha1.PoolSpec{
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Image: image}},
				},
			},
			CapacitySpec: sandboxv1alpha1.CapacitySpec{
				PoolMax: total,
			},
		},
		Status: sandboxv1alpha1.PoolStatus{
			Total:     total,
			Allocated: allocated,
		},
	}
}

func TestDefaultAssigner_AssignPool(t *testing.T) {
	ctx := context.Background()

	t.Run("single pool passes all predicates", func(t *testing.T) {
		assigner := NewDefaultAssigner(DefaultProfile())
		sbx := makeSBX("sbx-1", "nginx")
		pools := []*sandboxv1alpha1.Pool{makePool("pool-1", "nginx", 10, 5)}

		name, err := assigner.AssignPool(ctx, sbx, pools)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "pool-1" {
			t.Errorf("AssignPool() = %q, want %q", name, "pool-1")
		}
	})

	t.Run("multiple pools - higher score wins", func(t *testing.T) {
		profile := &Profile{
			Name: "most-allocated",
			Plugins: PluginsSpec{
				Predicate: []string{"image"},
				Score:     []ScoreSpec{{Name: "resbalance", Weight: 100}},
			},
			PluginConf: []PluginConf{
				{Name: "resbalance", Args: map[string]interface{}{"strategy": "MostAllocated"}},
			},
		}
		assigner := NewDefaultAssigner(profile)
		sbx := makeSBX("sbx-1", "nginx")
		pools := []*sandboxv1alpha1.Pool{
			makePool("pool-a", "nginx", 10, 2),
			makePool("pool-b", "nginx", 10, 8),
		}

		name, err := assigner.AssignPool(ctx, sbx, pools)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "pool-b" {
			t.Errorf("AssignPool() = %q, want %q", name, "pool-b")
		}
	})

	t.Run("all pools filtered out - error", func(t *testing.T) {
		assigner := NewDefaultAssigner(DefaultProfile())
		sbx := makeSBX("sbx-1", "nginx")
		pools := []*sandboxv1alpha1.Pool{makePool("pool-1", "redis", 10, 5)}

		_, err := assigner.AssignPool(ctx, sbx, pools)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("tie-breaking by name", func(t *testing.T) {
		profile := &Profile{
			Name: "tie-break",
			Plugins: PluginsSpec{
				Predicate: []string{"image"},
				Score:     []ScoreSpec{{Name: "resbalance", Weight: 100}},
			},
			PluginConf: []PluginConf{
				{Name: "resbalance", Args: map[string]interface{}{"strategy": "MostAllocated"}},
			},
		}
		assigner := NewDefaultAssigner(profile)
		sbx := makeSBX("sbx-1", "nginx")
		pools := []*sandboxv1alpha1.Pool{
			makePool("pool-b", "nginx", 10, 5),
			makePool("pool-a", "nginx", 10, 5),
		}

		name, err := assigner.AssignPool(ctx, sbx, pools)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "pool-a" {
			t.Errorf("AssignPool() = %q, want %q (lexicographic first)", name, "pool-a")
		}
	})

	t.Run("no scorers - pick by name", func(t *testing.T) {
		profile := &Profile{
			Name:    "no-scorers",
			Plugins: PluginsSpec{Predicate: []string{"image"}},
		}
		assigner := NewDefaultAssigner(profile)
		sbx := makeSBX("sbx-1", "nginx")
		pools := []*sandboxv1alpha1.Pool{
			makePool("pool-b", "nginx", 10, 5),
			makePool("pool-a", "nginx", 10, 5),
		}

		name, err := assigner.AssignPool(ctx, sbx, pools)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "pool-a" {
			t.Errorf("AssignPool() = %q, want %q", name, "pool-a")
		}
	})

	t.Run("empty pool list - error", func(t *testing.T) {
		assigner := NewDefaultAssigner(DefaultProfile())
		sbx := makeSBX("sbx-1", "nginx")

		_, err := assigner.AssignPool(ctx, sbx, nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("fully allocated pool filtered by capacity predicate", func(t *testing.T) {
		assigner := NewDefaultAssigner(DefaultProfile())
		sbx := makeSBX("sbx-1", "nginx")
		pools := []*sandboxv1alpha1.Pool{
			makePool("pool-full", "nginx", 10, 10),
			makePool("pool-avail", "nginx", 10, 5),
		}

		name, err := assigner.AssignPool(ctx, sbx, pools)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "pool-avail" {
			t.Errorf("AssignPool() = %q, want %q (full pool should be filtered)", name, "pool-avail")
		}
	})

	t.Run("MostAllocated skips fully allocated pool", func(t *testing.T) {
		profile := &Profile{
			Name: "most-allocated-cap",
			Plugins: PluginsSpec{
				Predicate: []string{"capacity", "image"},
				Score:     []ScoreSpec{{Name: "resbalance", Weight: 100}},
			},
			PluginConf: []PluginConf{
				{Name: "resbalance", Args: map[string]interface{}{"strategy": "MostAllocated"}},
			},
		}
		assigner := NewDefaultAssigner(profile)
		sbx := makeSBX("sbx-1", "nginx")
		pools := []*sandboxv1alpha1.Pool{
			makePool("pool-full", "nginx", 10, 10),
			makePool("pool-partial", "nginx", 10, 8),
		}

		name, err := assigner.AssignPool(ctx, sbx, pools)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "pool-partial" {
			t.Errorf("AssignPool() = %q, want %q (full pool should be filtered even with MostAllocated)", name, "pool-partial")
		}
	})

	t.Run("all pools fully allocated - error", func(t *testing.T) {
		assigner := NewDefaultAssigner(DefaultProfile())
		sbx := makeSBX("sbx-1", "nginx")
		pools := []*sandboxv1alpha1.Pool{
			makePool("pool-a", "nginx", 10, 10),
			makePool("pool-b", "nginx", 5, 5),
		}

		_, err := assigner.AssignPool(ctx, sbx, pools)
		if err == nil {
			t.Fatal("expected error when all pools are full, got nil")
		}
	})

	t.Run("scale-from-zero pool is eligible", func(t *testing.T) {
		assigner := NewDefaultAssigner(DefaultProfile())
		sbx := makeSBX("sbx-1", "nginx")
		zeroPool := &sandboxv1alpha1.Pool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-zero"},
			Spec: sandboxv1alpha1.PoolSpec{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Image: "nginx"}},
					},
				},
				CapacitySpec: sandboxv1alpha1.CapacitySpec{PoolMax: 5},
			},
			Status: sandboxv1alpha1.PoolStatus{Total: 0, Allocated: 0},
		}
		pools := []*sandboxv1alpha1.Pool{makePool("pool-full", "nginx", 3, 3), zeroPool}

		name, err := assigner.AssignPool(ctx, sbx, pools)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "pool-zero" {
			t.Errorf("AssignPool() = %q, want %q (scale-from-zero pool should be selected)", name, "pool-zero")
		}
	})

	t.Run("capacity requires room for all desired replicas", func(t *testing.T) {
		assigner := NewDefaultAssigner(DefaultProfile())
		sbx := makeSBX("sbx-1", "nginx")
		sbx.Spec.Replicas = int32Ptr(2)
		smallPool := &sandboxv1alpha1.Pool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-small"},
			Spec: sandboxv1alpha1.PoolSpec{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Image: "nginx"}},
					},
				},
				CapacitySpec: sandboxv1alpha1.CapacitySpec{PoolMax: 1},
			},
			Status: sandboxv1alpha1.PoolStatus{Total: 1, Allocated: 0},
		}
		bigPool := &sandboxv1alpha1.Pool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-big"},
			Spec: sandboxv1alpha1.PoolSpec{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Image: "nginx"}},
					},
				},
				CapacitySpec: sandboxv1alpha1.CapacitySpec{PoolMax: 10},
			},
			Status: sandboxv1alpha1.PoolStatus{Total: 10, Allocated: 5},
		}

		name, err := assigner.AssignPool(ctx, sbx, []*sandboxv1alpha1.Pool{smallPool, bigPool})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "pool-big" {
			t.Errorf("AssignPool() = %q, want %q (pool with PoolMax<replicas must be filtered)", name, "pool-big")
		}
	})
}
