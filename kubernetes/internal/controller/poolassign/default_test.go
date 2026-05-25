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
}
