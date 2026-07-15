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

package strategy

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func TestDefaultPoolStrategy_IsPooledMode(t *testing.T) {
	tests := []struct {
		name     string
		batchSbx *sandboxv1alpha1.BatchSandbox
		want     bool
	}{
		{
			name: "with template - not pooled",
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test",
									Image: "nginx",
								},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "without template - pooled",
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: nil,
					PoolRef:  "test-pool",
				},
			},
			want: true,
		},
		{
			name: "with PoolRef star - pooled mode",
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					PoolRef: "*",
				},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			strategy := NewDefaultPoolStrategy(tt.batchSbx)
			if got := strategy.IsPooledMode(); got != tt.want {
				t.Errorf("DefaultPoolStrategy.IsPooledMode() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewPoolStrategy(t *testing.T) {
	tests := []struct {
		name         string
		batchSbx     *sandboxv1alpha1.BatchSandbox
		wantStrategy string
	}{
		{
			name: "without resource-speedup label - returns DefaultPoolStrategy",
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{},
				},
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					Template: nil,
				},
			},
			wantStrategy: "*strategy.DefaultPoolStrategy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewPoolStrategy(tt.batchSbx)
			gotType := getTypeName(got)
			if gotType != tt.wantStrategy {
				t.Errorf("NewPoolStrategy() = %v, want %v", gotType, tt.wantStrategy)
			}
		})
	}
}

func TestDefaultPoolStrategy_AssignProfile(t *testing.T) {
	tests := []struct {
		name     string
		batchSbx *sandboxv1alpha1.BatchSandbox
		want     string
	}{
		{
			name: "PoolRef star - returns default profile",
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					PoolRef: "*",
				},
			},
			want: "default",
		},
		{
			name: "PoolRef concrete name - empty profile",
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{
					PoolRef: "test-pool",
				},
			},
			want: "",
		},
		{
			name: "PoolRef empty - empty profile",
			batchSbx: &sandboxv1alpha1.BatchSandbox{
				Spec: sandboxv1alpha1.BatchSandboxSpec{},
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewDefaultPoolStrategy(tt.batchSbx)
			if got := s.AssignProfile(); got != tt.want {
				t.Errorf("DefaultPoolStrategy.AssignProfile() = %v, want %v", got, tt.want)
			}
		})
	}
}

func getTypeName(i interface{}) string {
	return fmt.Sprintf("%T", i)
}
