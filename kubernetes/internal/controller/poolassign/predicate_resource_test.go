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
	"k8s.io/apimachinery/pkg/api/resource"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func TestResourcePredicate(t *testing.T) {
	p, err := newResourcePredicate(nil)
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
			name: "pool has equal resources",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "main",
									Image: "nginx",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("1"),
											corev1.ResourceMemory: resource.MustParse("1Gi"),
										},
									},
								},
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
								{
									Name:  "main",
									Image: "nginx",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("1"),
											corev1.ResourceMemory: resource.MustParse("1Gi"),
										},
									},
								},
							},
						},
					},
				},
			},
			expect: true,
		},
		{
			name: "pool has greater resources",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("1"),
											corev1.ResourceMemory: resource.MustParse("1Gi"),
										},
									},
								},
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
								{
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("2"),
											corev1.ResourceMemory: resource.MustParse("4Gi"),
										},
									},
								},
							},
						},
					},
				},
			},
			expect: true,
		},
		{
			name: "pool has insufficient CPU",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU: resource.MustParse("2"),
										},
									},
								},
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
								{
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU: resource.MustParse("1"),
										},
									},
								},
							},
						},
					},
				},
			},
			expect: false,
		},
		{
			name: "pool has insufficient memory",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceMemory: resource.MustParse("4Gi"),
										},
									},
								},
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
								{
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceMemory: resource.MustParse("2Gi"),
										},
									},
								},
							},
						},
					},
				},
			},
			expect: false,
		},
		{
			name: "sandbox has no resource requests - passes",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Image: "nginx"},
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
								{Image: "nginx"},
							},
						},
					},
				},
			},
			expect: true,
		},
		{
			name: "sandbox requests resources but pool has none - fails",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU: resource.MustParse("1"),
										},
									},
								},
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
								{Image: "nginx"},
							},
						},
					},
				},
			},
			expect: false,
		},
		{
			name: "nil sandbox template - passes (no resource constraint)",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{Template: nil},
			},
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Image: "nginx"}},
						},
					},
				},
			},
			expect: true,
		},
		{
			name: "nil pool template but sandbox has no resource requests - passes",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Image: "nginx"}},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{Template: nil},
			},
			expect: true,
		},
		{
			name: "nil pool template and sandbox has resource requests - fails",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU: resource.MustParse("1"),
										},
									},
								},
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
			name: "multiple containers - all satisfied",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU: resource.MustParse("500m"),
										},
									},
								},
								{
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceMemory: resource.MustParse("512Mi"),
										},
									},
								},
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
								{
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("1"),
											corev1.ResourceMemory: resource.MustParse("1Gi"),
										},
									},
								},
								{
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceMemory: resource.MustParse("2Gi"),
										},
									},
								},
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
				t.Errorf("resourcePredicate.Predicate() = %v, want %v", got, tt.expect)
			}
		})
	}
}
