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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func TestResBalanceScorer(t *testing.T) {
	ctx := context.Background()
	sbx := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1"},
	}

	tests := []struct {
		name     string
		strategy string
		pool     *sandboxv1alpha1.Pool
		want     float64
	}{
		{
			name:     "MostAllocated - half allocated",
			strategy: "MostAllocated",
			pool: &sandboxv1alpha1.Pool{
				Status: sandboxv1alpha1.PoolStatus{Total: 10, Allocated: 5},
			},
			want: 0.5,
		},
		{
			name:     "MostAllocated - fully allocated",
			strategy: "MostAllocated",
			pool: &sandboxv1alpha1.Pool{
				Status: sandboxv1alpha1.PoolStatus{Total: 10, Allocated: 10},
			},
			want: 1.0,
		},
		{
			name:     "MostAllocated - zero allocated",
			strategy: "MostAllocated",
			pool: &sandboxv1alpha1.Pool{
				Status: sandboxv1alpha1.PoolStatus{Total: 10, Allocated: 0},
			},
			want: 0.0,
		},
		{
			name:     "MostAllocated - zero total",
			strategy: "MostAllocated",
			pool: &sandboxv1alpha1.Pool{
				Status: sandboxv1alpha1.PoolStatus{Total: 0, Allocated: 0},
			},
			want: 0.0,
		},
		{
			name:     "LeastAllocated - half allocated",
			strategy: "LeastAllocated",
			pool: &sandboxv1alpha1.Pool{
				Status: sandboxv1alpha1.PoolStatus{Total: 10, Allocated: 5},
			},
			want: 0.5,
		},
		{
			name:     "LeastAllocated - fully allocated",
			strategy: "LeastAllocated",
			pool: &sandboxv1alpha1.Pool{
				Status: sandboxv1alpha1.PoolStatus{Total: 10, Allocated: 10},
			},
			want: 0.0,
		},
		{
			name:     "LeastAllocated - zero allocated",
			strategy: "LeastAllocated",
			pool: &sandboxv1alpha1.Pool{
				Status: sandboxv1alpha1.PoolStatus{Total: 10, Allocated: 0},
			},
			want: 1.0,
		},
		{
			name:     "LeastAllocated - zero total",
			strategy: "LeastAllocated",
			pool: &sandboxv1alpha1.Pool{
				Status: sandboxv1alpha1.PoolStatus{Total: 0, Allocated: 0},
			},
			want: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := map[string]interface{}{"strategy": tt.strategy}
			s, err := newResBalanceScorer(args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := s.Score(ctx, sbx, tt.pool)
			if got != tt.want {
				t.Errorf("resBalanceScorer.Score() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResBalanceScorer_InvalidStrategy(t *testing.T) {
	args := map[string]interface{}{"strategy": "InvalidStrategy"}
	_, err := newResBalanceScorer(args)
	if err == nil {
		t.Fatal("expected error for invalid strategy, got nil")
	}
}
