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

func TestImagePredicate(t *testing.T) {
	p, err := newImagePredicate(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx := context.Background()

	tests := []struct {
		name   string
		sbx    *sandboxv1alpha1.BatchSandbox
		pool   *sandboxv1alpha1.Pool
		expect bool
	}{
		{
			name: "matching images",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Image: "nginx:1.25"},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Image: "nginx:1.25"},
							},
						},
					},
				},
			},
			expect: true,
		},
		{
			name: "no matching images",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Image: "nginx:1.25"},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Image: "redis:7"},
							},
						},
					},
				},
			},
			expect: false,
		},
		{
			name: "nil sandbox template - passes (no image constraint)",
			sbx: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sbx-1"},
				Spec:       sandboxv1alpha1.BatchSandboxSpec{Template: nil},
			},
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Image: "nginx:1.25"},
							},
						},
					},
				},
			},
			expect: true,
		},
		{
			name: "nil pool template",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Image: "nginx:1.25"},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{Template: nil},
			},
			expect: false,
		},
		{
			name: "empty containers in sandbox - passes (no image constraint)",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{}},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Image: "nginx:1.25"}},
						},
					},
				},
			},
			expect: true,
		},
		{
			name: "empty containers in pool",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Image: "nginx:1.25"}},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{}},
					},
				},
			},
			expect: false,
		},
		{
			name: "partial match across multiple containers",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Image: "sidecar:latest"},
								{Image: "nginx:1.25"},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Image: "nginx:1.25"},
								{Image: "helper:latest"},
							},
						},
					},
				},
			},
			expect: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.Predicate(ctx, tt.sbx, tt.pool)
			if got != tt.expect {
				t.Errorf("imagePredicate.Predicate() = %v, want %v", got, tt.expect)
			}
		})
	}
}
