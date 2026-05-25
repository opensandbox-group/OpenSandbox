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

func TestNodeSelectorPredicate(t *testing.T) {
	p, err := newNodeSelectorPredicate(nil)
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
			name: "matching nodeSelector",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{"zone": "us-west-1"},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{"zone": "us-west-1", "rack": "r1"},
						},
					},
				},
			},
			expect: true,
		},
		{
			name: "non-matching nodeSelector",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{"zone": "us-west-1"},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{"zone": "us-east-1"},
						},
					},
				},
			},
			expect: false,
		},
		{
			name: "sandbox has no nodeSelector - passes",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{"zone": "us-west-1"},
						},
					},
				},
			},
			expect: true,
		},
		{
			name: "nil sandbox template - passes (no nodeSelector constraint)",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{Template: nil},
			},
			pool: &sandboxv1alpha1.Pool{
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{"zone": "us-west-1"},
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
							NodeSelector: map[string]string{"zone": "us-west-1"},
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
			name: "matching nodeAffinity In operator",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "zone",
														Operator: corev1.NodeSelectorOpIn,
														Values:   []string{"us-west-1"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"zone": "us-west-1"},
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{},
				},
			},
			expect: true,
		},
		{
			name: "non-matching nodeAffinity In operator",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "zone",
														Operator: corev1.NodeSelectorOpIn,
														Values:   []string{"us-west-1"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"zone": "us-east-1"},
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{},
				},
			},
			expect: false,
		},
		{
			name: "nodeAffinity with pool label missing",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "zone",
														Operator: corev1.NodeSelectorOpIn,
														Values:   []string{"us-west-1"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"rack": "r1"},
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{},
				},
			},
			expect: false,
		},
		{
			name: "both nodeSelector and nodeAffinity match",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{"disk": "ssd"},
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "zone",
														Operator: corev1.NodeSelectorOpIn,
														Values:   []string{"us-west-1"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"zone": "us-west-1"},
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{"disk": "ssd"},
						},
					},
				},
			},
			expect: true,
		},
		{
			name: "nodeSelector matches but nodeAffinity does not",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{"disk": "ssd"},
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "zone",
														Operator: corev1.NodeSelectorOpIn,
														Values:   []string{"us-west-1"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"zone": "us-east-1"},
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{"disk": "ssd"},
						},
					},
				},
			},
			expect: false,
		},
		{
			name: "sbx nodeSelector matches pool labels (not pool nodeSelector)",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{"accelerator": "gpu"},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"accelerator": "gpu"},
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{},
					},
				},
			},
			expect: true,
		},
		{
			name: "sbx nodeSelector does not match pool labels",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{"accelerator": "gpu"},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"accelerator": "cpu"},
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{},
					},
				},
			},
			expect: false,
		},
		{
			name: "sbx affinity matches pool nodeSelector (not pool labels)",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "disk",
														Operator: corev1.NodeSelectorOpIn,
														Values:   []string{"ssd"},
													},
												},
											},
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
							NodeSelector: map[string]string{"disk": "ssd"},
						},
					},
				},
			},
			expect: true,
		},
		{
			name: "sbx nodeSelector matches merged pool labels + nodeSelector",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{"accelerator": "gpu", "disk": "ssd"},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"accelerator": "gpu"},
				},
				Spec: sandboxv1alpha1.PoolSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{"disk": "ssd"},
						},
					},
				},
			},
			expect: true,
		},
		{
			name: "nil pool template with pool labels matching sbx nodeSelector",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							NodeSelector: map[string]string{"zone": "us-west-1"},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"zone": "us-west-1"},
				},
				Spec: sandboxv1alpha1.PoolSpec{Template: nil},
			},
			expect: true,
		},
		{
			name: "nodeAffinity NotIn operator - label not in list passes",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "zone",
														Operator: corev1.NodeSelectorOpNotIn,
														Values:   []string{"us-east-1", "us-east-2"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"zone": "us-west-1"},
				},
				Spec: sandboxv1alpha1.PoolSpec{Template: &corev1.PodTemplateSpec{}},
			},
			expect: true,
		},
		{
			name: "nodeAffinity NotIn operator - label in list fails",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "zone",
														Operator: corev1.NodeSelectorOpNotIn,
														Values:   []string{"us-east-1", "us-west-1"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"zone": "us-west-1"},
				},
				Spec: sandboxv1alpha1.PoolSpec{Template: &corev1.PodTemplateSpec{}},
			},
			expect: false,
		},
		{
			name: "nodeAffinity Exists operator - key present passes",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "zone",
														Operator: corev1.NodeSelectorOpExists,
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"zone": "us-west-1"},
				},
				Spec: sandboxv1alpha1.PoolSpec{Template: &corev1.PodTemplateSpec{}},
			},
			expect: true,
		},
		{
			name: "nodeAffinity Exists operator - key absent fails",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "zone",
														Operator: corev1.NodeSelectorOpExists,
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"rack": "r1"},
				},
				Spec: sandboxv1alpha1.PoolSpec{Template: &corev1.PodTemplateSpec{}},
			},
			expect: false,
		},
		{
			name: "nodeAffinity DoesNotExist operator - key absent passes",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "zone",
														Operator: corev1.NodeSelectorOpDoesNotExist,
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"rack": "r1"},
				},
				Spec: sandboxv1alpha1.PoolSpec{Template: &corev1.PodTemplateSpec{}},
			},
			expect: true,
		},
		{
			name: "nodeAffinity DoesNotExist operator - key present fails",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "zone",
														Operator: corev1.NodeSelectorOpDoesNotExist,
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"zone": "us-west-1"},
				},
				Spec: sandboxv1alpha1.PoolSpec{Template: &corev1.PodTemplateSpec{}},
			},
			expect: false,
		},
		{
			name: "nodeAffinity Gt operator - label greater passes",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "priority",
														Operator: corev1.NodeSelectorOpGt,
														Values:   []string{"5"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"priority": "10"},
				},
				Spec: sandboxv1alpha1.PoolSpec{Template: &corev1.PodTemplateSpec{}},
			},
			expect: true,
		},
		{
			name: "nodeAffinity Gt operator - label not greater fails",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "priority",
														Operator: corev1.NodeSelectorOpGt,
														Values:   []string{"5"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"priority": "3"},
				},
				Spec: sandboxv1alpha1.PoolSpec{Template: &corev1.PodTemplateSpec{}},
			},
			expect: false,
		},
		{
			name: "nodeAffinity Lt operator - label less passes",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "priority",
														Operator: corev1.NodeSelectorOpLt,
														Values:   []string{"5"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"priority": "3"},
				},
				Spec: sandboxv1alpha1.PoolSpec{Template: &corev1.PodTemplateSpec{}},
			},
			expect: true,
		},
		{
			name: "nodeAffinity Lt operator - label not less fails",
			sbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Affinity: &corev1.Affinity{
								NodeAffinity: &corev1.NodeAffinity{
									RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
										NodeSelectorTerms: []corev1.NodeSelectorTerm{
											{
												MatchExpressions: []corev1.NodeSelectorRequirement{
													{
														Key:      "priority",
														Operator: corev1.NodeSelectorOpLt,
														Values:   []string{"5"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"priority": "10"},
				},
				Spec: sandboxv1alpha1.PoolSpec{Template: &corev1.PodTemplateSpec{}},
			},
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.Predicate(ctx, tt.sbx, tt.pool)
			if got != tt.expect {
				t.Errorf("nodeSelectorPredicate.Predicate() = %v, want %v", got, tt.expect)
			}
		})
	}
}
